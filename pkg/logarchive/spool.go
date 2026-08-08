// spool.go — per-Firecracker-instance append-only JSONL writer
// (issue #562). Every evicted logbuf.Line flows through here
// before being dropped from the ring's byte budget.
//
// Layout:
//
//	{root}/{instanceID}/{YYYY}/{MM}/log-{YYYY-MM-DD}.jsonl.partial
//
// Each line is one JSON object with `{ts, stream, seq, msg}`
// fields. The file is created lazily on first write for the
// (instance, day) tuple; subsequent evictions on the same day
// append to the same file. The .partial rename target is
// .jsonl.gz (gzipping happens at flush time in the shipper,
// not per line). The day is encoded in the file name so a
// directory walk can reconstruct the (instance, day) tuple
// after CloseAll drains the in-memory map.
//
// The writer buffers 64 KiB via bufio so a chatty app doesn't
// burn a syscall per eviction. The buffer is flushed on every
// Close and on every rotateBoundary crossing (the shipper's
// flushOnce renames the file, after which the next eviction
// opens a fresh .partial in the new day's directory).
//
// Threading: the ring's OnEvict callback runs under r.mu.
// The spool holds its own per-instance mu so concurrent
// ring/manager goroutines can write to different (instance,
// day) files without contending. A single (instance, day)
// pair is serialised by the spool's per-key map.

package logarchive

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultBufferBytes is the bufio.Writer buffer size. 64 KiB
// matches the ring's MaxPartialLineBytes floor and amortises
// syscalls across many evictions on a chatty app.
const DefaultBufferBytes = 64 * 1024

// MaxInstanceIDLen guards the path-traversal surface. A
// Firecracker instance id is normally a UUID (~36 chars) but the
// schedd/manager surface accepts caller-supplied strings; we cap
// to 128 to keep the path layout bounded and reject anything
// that contains "/" or "\0".
const MaxInstanceIDLen = 128

// spoolFileName is the .partial suffix the shipper looks for at
// flush time. Renaming to .jsonl.gz signals "shipped" once the
// gzipped object lands in the bucket; the purger sweeps
// .jsonl.gz files older than the retention boundary.
//
// The prefix is the day (YYYY-MM-DD) so FilesSnapshot can recover
// the day after CloseAll drains the in-memory map — the spool
// directory layout has year/month above, but the day is also
// encoded in the file name to keep the on-disk parse unambiguous.
const spoolFilePrefix = "log-"

// spoolLine is the on-disk JSON shape. Fields are kept stable
// across versions; `seq` is the monotonic ring seq the line was
// evicted with so the archive replays the same order the ring
// held.
type spoolLine struct {
	Seq       int64     `json:"seq"`
	Stream    string    `json:"stream"`
	WrittenAt time.Time `json:"ts"`
	Line      string    `json:"msg"`
}

// spoolKey is the (instance, day) tuple the spool keys on. The
// day boundary is the line's WrittenAt truncated to UTC date —
// a line evicted at 23:59:59.999 UTC opens a new file at 00:00
// the next day.
type spoolKey struct {
	instance string
	day      string // YYYY-MM-DD (UTC)
}

// fileHandle is the per-(instance, day) writer + its underlying
// fd. Closed by CloseAll (called from Shipper.Shutdown).
type fileHandle struct {
	bw   *bufio.Writer
	file *os.File
	path string
	size int64 // accumulated bytes written to this file
}

// Spool is the on-disk sink. Construct with NewSpool, drive with
// Write from the ring's OnEvict callback, drain with CloseAll
// from the Shipper shutdown path. One Spool per process; the
// Shipper owns the lifetime.
type Spool struct {
	root     string
	maxBytes int64 // FAAS_LOG_ARCHIVE_LOCAL_BYTES_MAX (DefaultLocalBytesMax)
	bufBytes int   // bufio buffer size (DefaultBufferBytes)

	mu      sync.Mutex
	files   map[spoolKey]*fileHandle
	written int64 // current local-spool size
}

// NewSpool constructs a Spool rooted at root. maxBytes is the
// upper bound on the accumulated file size across all
// (instance, day) keys (DefaultLocalBytesMax when 0). The
// directory tree is created lazily on first write to avoid
// touching the filesystem when no lines have flowed yet.
func NewSpool(root string, maxBytes int64) *Spool {
	if maxBytes <= 0 {
		maxBytes = DefaultLocalBytesMax
	}
	return &Spool{
		root:     root,
		maxBytes: maxBytes,
		bufBytes: DefaultBufferBytes,
		files:    make(map[spoolKey]*fileHandle),
	}
}

// Write persists one evicted logbuf.Line to disk. Safe for
// concurrent calls from many ring goroutines. Returns the byte
// count written (so the caller can update a metric) and any
// error. Errors are surfaced to the ring via the apid
// apid_log_archive_failures_total{reason="spool_write"} counter
// (cmd/apid/main.go).
//
// When the spool is at maxBytes (FAAS_LOG_ARCHIVE_LOCAL_BYTES_MAX)
// the write is refused and ErrSpoolFull is returned — the ring
// has already lost the line so dropping on the floor here is
// safer than filling the disk (issue #562 risk #6).
func (s *Spool) Write(instanceID string, seq int64, stream string, ts time.Time, line string) (int, error) {
	if instanceID == "" || len(instanceID) > MaxInstanceIDLen {
		return 0, fmt.Errorf("logarchive: invalid instance id (len %d)", len(instanceID))
	}
	if strings.ContainsAny(instanceID, "/\\\x00") {
		return 0, fmt.Errorf("logarchive: instance id contains path separator or NUL")
	}
	key := spoolKey{instance: instanceID, day: ts.UTC().Format("2006-01-02")}
	payload, err := json.Marshal(spoolLine{
		Seq:       seq,
		Stream:    stream,
		WrittenAt: ts.UTC(),
		Line:      line,
	})
	if err != nil {
		return 0, fmt.Errorf("logarchive: marshal line: %w", err)
	}
	payload = append(payload, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.written+int64(len(payload)) > s.maxBytes {
		return 0, ErrSpoolFull
	}
	fh, err := s.openLocked(key)
	if err != nil {
		return 0, err
	}
	n, err := fh.bw.Write(payload)
	if err != nil {
		return n, fmt.Errorf("logarchive: spool write: %w", err)
	}
	fh.size += int64(n)
	s.written += int64(n)
	return n, nil
}

// openLocked returns the fileHandle for key, creating it on
// first call. Caller holds s.mu.
func (s *Spool) openLocked(key spoolKey) (*fileHandle, error) {
	if fh, ok := s.files[key]; ok {
		return fh, nil
	}
	dir := filepath.Join(s.root, key.instance, key.day[:4], key.day[5:7])
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("logarchive: mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, spoolFilePrefix+key.day+".jsonl.partial")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, fmt.Errorf("logarchive: open %s: %w", path, err)
	}
	stat, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("logarchive: stat %s: %w", path, err)
	}
	bw := bufio.NewWriterSize(f, s.bufBytes)
	fh := &fileHandle{bw: bw, file: f, path: path, size: stat.Size()}
	s.files[key] = fh
	s.written += stat.Size()
	return fh, nil
}

// CloseAll flushes + closes every open file. Called from the
// Shipper shutdown path. Safe to call multiple times (the map
// is reset so a re-call is a no-op).
func (s *Spool) CloseAll() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstErr error
	for k, fh := range s.files {
		if err := fh.bw.Flush(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("flush %s: %w", fh.path, err)
		}
		if err := fh.file.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("close %s: %w", fh.path, err)
		}
		delete(s.files, k)
	}
	return firstErr
}

// LocalBytes returns the current local-spool size. Read under
// s.mu; safe for concurrent reads from the apid background
// sampler (used to populate the apid_log_archive_local_bytes
// gauge).
func (s *Spool) LocalBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.written
}

// FilesSnapshot returns the (instance, day, size) tuples the
// shipper needs at flush time. Walks the spool root on disk
// rather than the in-memory map, so a CloseAll'd spool still
// surfaces .partial files for the next tick.
func (s *Spool) FilesSnapshot() []FileInfo {
	s.mu.Lock()
	root := s.root
	s.mu.Unlock()
	out := []FileInfo{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasPrefix(name, spoolFilePrefix) || !strings.HasSuffix(name, ".jsonl.partial") {
			return nil
		}
		// {prefix}{YYYY-MM-DD}{suffix}. The day sits between
		// the fixed prefix and suffix.
		day := strings.TrimSuffix(strings.TrimPrefix(name, spoolFilePrefix), ".jsonl.partial")
		if len(day) != 10 { // YYYY-MM-DD
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) != 4 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		out = append(out, FileInfo{
			Instance: parts[0],
			Day:      day,
			Path:     path,
			Size:     info.Size(),
		})
		return nil
	})
	if err != nil {
		return out
	}
	return out
}

// FileInfo is the (instance, day, path, size) tuple the shipper
// reads at flush time. Mirrors the FilesSnapshot return type so
// the shipper can iterate without holding s.mu.
type FileInfo struct {
	Instance string
	Day      string
	Path     string
	Size     int64
}

// ErrSpoolFull is the typed sentinel the spool returns when
// FAAS_LOG_ARCHIVE_LOCAL_BYTES_MAX would be exceeded. The
// shipper increments apid_log_archive_failures_total{reason=
// "spool_full"} on this branch so the operator sees the
// degraded state in /metrics.
var ErrSpoolFull = errors.New("logarchive: spool full")

// FlushKey flushes the bufio buffer for the (instance, day) key
// without closing the file. The shipper calls this AFTER
// successfully uploading the gzipped object so any subsequent
// eviction on the same key starts from a clean buffer state.
// Returns nil when the key has no open file (e.g. the file was
// already CloseAll'd by a concurrent shutdown path).
func (s *Spool) FlushKey(instance, day string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fh, ok := s.files[spoolKey{instance: instance, day: day}]
	if !ok {
		return nil
	}
	return fh.bw.Flush()
}
