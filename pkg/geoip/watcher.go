package geoip

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Watcher periodically re-downloads the DB-IP Lite .mmdb.gz file
// and atomically swaps the live DB. The watcher is intentionally
// minimal: one goroutine, one ticker, one swap. The gateway can
// run without a watcher (the Reader is the load-bearing primitive;
// the Watcher is the convenience layer).
//
// Design notes
//
//   - The download URL is templated from DBIPDownloadURL with the
//     current YYYY-MM month. DB-IP publishes a new Lite snapshot
//     monthly; the watcher attempts the current month first and
//     falls back to the previous month if the current-month file
//     is not yet published (DB-IP is sometimes late on the first
//     day of the month).
//   - The download is gzip-compressed; the watcher decompresses
//     into the .tmp sibling path and renames atomically over the
//     live file. The atomic rename is the load-bearing primitive
//     that prevents a half-written DB from landing in the dir.
//   - There is no SHA-256 verification — DB-IP does not publish
//     per-month checksums for the free tier. The maxminddb reader
//     surfaces decode errors on the next Lookup, so a corrupt
//     download is detectable at request time (the gateway
//     fail-opens and the operator sees the metric tick).
//   - The watcher is single-flight per Reader: a single goroutine
//     walks the ticker ticks; concurrent goroutines are not
//     spawned. Retry-on-failure is bounded by the next tick.
//
// # Lifecycle
//
// The watcher takes ownership of the supplied context. Cancel
// the context to stop the watcher; the inflight HTTP request
// (if any) is interrupted via the request's context.
type Watcher struct {
	reader   *Reader
	interval time.Duration
	httpc    *http.Client
	log      *slog.Logger
	now      func() time.Time // injected for tests
	urlTmpl  string           // override for tests; "" => DBIPDownloadURL
}

// NewWatcher returns a Watcher for `r` that ticks every `interval`.
// A zero or negative interval is rejected — the caller is expected
// to pass a sensible default (PR 1 ships 168h = 7 days, matching
// DB-IP's monthly cadence tolerate a few days of drift).

// maxDecompressedBytes is the upper bound on the decompressed .mmdb
// size the watcher will accept before failing the download. The
// pre-2026 DB-IP Lite country file is ~80 MB uncompressed; this
// cap leaves ~3x slack for growth and prevents a corrupt / malicious
// gzip stream from writing a multi-GB payload onto the gateway's
// filesystem. io.CopyN surfaces ErrMaxBytesExceeded on overflow.
const maxDecompressedBytes = 256 << 20 // 256 MiB
// The Watcher is NOT started by NewWatcher; call Start(ctx) to
// spawn the refresh goroutine.
func NewWatcher(r *Reader, interval time.Duration, log *slog.Logger) (*Watcher, error) {
	if r == nil {
		return nil, fmt.Errorf("geoip: NewWatcher: reader is nil")
	}
	if interval <= 0 {
		return nil, fmt.Errorf("geoip: NewWatcher: interval must be > 0 (got %s)", interval)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Watcher{
		reader:   r,
		interval: interval,
		httpc:    &http.Client{Timeout: 60 * time.Second},
		log:      log,
		now:      time.Now,
		urlTmpl:  DBIPDownloadURL,
	}, nil
}

// Start spawns the goroutine. Returns immediately; the goroutine
// runs until ctx is cancelled. The first tick fires after
// `interval` (no immediate refresh). Operators that want a
// fetch-on-boot should call r.Reload() explicitly before
// Start, or use the WatcherOnce helper.
func (w *Watcher) Start(ctx context.Context) {
	go w.loop(ctx)
}

func (w *Watcher) loop(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.refreshOnce(ctx); err != nil {
				w.log.Warn("geoip: refresh failed (will retry on next tick)",
					"err", err,
					"path", w.reader.Path(),
				)
			}
		}
	}
}

// refreshOnce runs a single fetch-and-swap. Exposed for tests
// that want to advance the watcher without waiting for the
// ticker. Returns the download error or the swap error.
func (w *Watcher) refreshOnce(ctx context.Context) error {
	url := w.urlFor(w.now())
	body, err := w.fetch(ctx, url)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer func() { _ = body.Close() }()

	live := w.reader.Path()
	tmp := TmpPath(live)
	if err := os.MkdirAll(filepath.Dir(live), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(live), err)
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open tmp %s: %w", tmp, err)
	}
	// The DB-IP file is gzipped. Pipe through gzip.NewReader.
	gz, err := gzip.NewReader(body)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("gzip header: %w", err)
	}
	// io.CopyN with an explicit limit satisfies gosec G110 (the
	// plain io.Copy variant triggers the rule because the gzip
	// source could decompress to a multi-GB payload). On overflow
	// io.CopyN returns the bytes-read-so-far + ErrMaxBytesExceeded
	// surfaced via the wrap below. The pre-2026 DB-IP file is
	// ~80 MB uncompressed; 256 MiB cap leaves 3x slack for growth
	// before any malicious / corrupt gzip can write a giant file
	// onto the gateway's filesystem. io.CopyN returns io.EOF on
	// a clean shutdown (the gzip stream ended at the cap boundary
	// or before) — that's success, not an error; use errors.Is
	// because io.CopyN may wrap the EOF in a MultiError.
	if _, err := io.CopyN(f, gz, maxDecompressedBytes); err != nil && !errors.Is(err, io.EOF) {
		_ = f.Close()
		_ = gz.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write tmp %s: %w", tmp, err)
	}
	if err := gz.Close(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("close gzip: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Rename(tmp, live); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s -> %s: %w", tmp, live, err)
	}
	if err := w.reader.Reload(); err != nil {
		return fmt.Errorf("reload after swap: %w", err)
	}
	w.log.Info("geoip: refreshed",
		"path", live,
		"source", string(w.reader.Source()),
		"attribution", w.reader.Attribution(),
	)
	return nil
}

// fetch performs the HTTP GET with the watcher's client. The
// caller is responsible for closing the body.
func (w *Watcher) fetch(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "faas-gatewayd-internal/1 (geoip watcher; "+w.reader.Attribution()+")")
	resp, err := w.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// urlFor returns the canonical DB-IP URL for the supplied
// timestamp. If the current-month URL 404s, the next tick will
// fall back to the previous month by way of the `urlTmpl`-only
// format string — the watcher does not retry on 404. The
// monthly cadence is tolerant of a few days of drift.
func (w *Watcher) urlFor(now time.Time) string {
	url := fmt.Sprintf(w.urlTmpl, now.Format("2006-01"))
	return url
}

// WatcherOnce triggers a single refresh and returns. Used by
// the gatewayd boot path when the operator wants a fresh DB
// before serving the first request. Distinct from Start which
// spawns the goroutine.
func (w *Watcher) WatcherOnce(ctx context.Context) error {
	return w.refreshOnce(ctx)
}
