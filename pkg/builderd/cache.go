package builderd

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// CacheEntry is one cached build: source-hash + framework → produced layer
// path + size. The entry is purely on-disk; pkg/state never sees it (ADR-005
// keeps state in the SQL tables; the cache is content-addressed storage that
// can be wiped without data loss).
type CacheEntry struct {
	Path  string
	Bytes int64
}

// Cache is a content-addressed cache of produced app layers. The key is
// sha256(source-bytes); the value is the produced ext4 layer + size. The
// filesystem layout is:
//
//	<CacheDir>/<sha256>.<framework>/layer.ext4
//
// Lookup is best-effort: a missing entry is not an error (the caller will
// build). A corrupted entry (size mismatch, missing file) IS an error so the
// caller can rebuild instead of using a broken layer.
type Cache struct {
	root string
}

// NewCache wires a Cache rooted at dir. The dir is created lazily.
func NewCache(dir string) *Cache { return &Cache{root: dir} }

// Lookup returns the cached layer for (sourceHash, fw) if one exists and
// looks intact. (false, nil) is a cache miss, not an error.
func (c *Cache) Lookup(sourceHash string, fw Framework) (CacheEntry, bool) {
	if c == nil || c.root == "" {
		return CacheEntry{}, false
	}
	p := c.entryPath(sourceHash, fw)
	st, err := os.Stat(p)
	if err != nil {
		return CacheEntry{}, false
	}
	if !st.Mode().IsRegular() {
		return CacheEntry{}, false
	}
	return CacheEntry{Path: p, Bytes: st.Size()}, true
}

// Store moves the produced layer into the cache under the source-hash key.
// The publish is atomic: write to a unique temp file in the destination
// directory, fsync, close, then os.Rename onto the canonical name. A crash
// mid-write leaves the temp file behind; the canonical name is never
// observable in a half-written state.
//
// CRITICAL invariants:
//
//  1. The source layerPath is NEVER renamed — pkg/builderd/builderd.go:432
//     uses out.OCIImage (the original path) immediately after Store
//     returns. Renaming the source would silently break the subsequent
//     SetDeploymentRootfs call. Always copy-then-rename-to-dst.
//
//  2. Concurrent Store calls for DIFFERENT (sourceHash, fw) keys MUST NOT
//     share a temp path. Two writers with a literal dst.tmp would race —
//     one writer's os.Rename would publish the other's data. os.CreateTemp
//     with a "cache-*.tmp" wildcard gives each call a unique suffix
//     (mirrors pkg/storage/local.go::Put's atomic-publish idiom).
//
//  3. Cross-device writes fail loud — the old copyFile fallback was the
//     bug that allowed partial writes to be observable on EXDEV. A
//     cross-filesystem cache root is a config error; refuse it.
//
//  4. First-writer wins: if the canonical entry already exists, return
//     nil without rewriting. Content-addressed storage means later
//     writers should produce identical bytes; the existing copy is fine.
func (c *Cache) Store(sourceHash string, fw Framework, layerPath string, bytes int64) error {
	if c == nil || c.root == "" {
		return errors.New("cache: not configured")
	}
	dst := c.entryPath(sourceHash, fw)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("cache: mkdir: %w", err)
	}
	// Idempotent: if the destination already exists, keep it (its bytes
	// should match — content-addressed — so the existing copy is fine).
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	// Step 1: open source for reading (never write to it).
	//
	//nolint:forbidigo // layerPath is the builderd-produced OCI image from
	// pkg/builderd/builderd.go::processClaimedBuild; the path is built by
	// builderd itself under its vetted cache/spool dir, never reaches a
	// customer-supplied path. The shape validator (validateTarballShape in
	// cmd/apid/deploy_inputs.go) ran on the source tarball before
	// builderd saw it.
	in, err := os.Open(layerPath)
	if err != nil {
		return fmt.Errorf("cache: store %s: open source: %w", dst, err)
	}
	// Step 2: create a UNIQUE temp file on the destination filesystem.
	// os.CreateTemp gives a random suffix and atomic create semantics
	// (O_EXCL). Two concurrent Store calls for distinct keys cannot
	// collide on this temp.
	tmp, err := os.CreateTemp(filepath.Dir(dst), "cache-*.tmp")
	if err != nil {
		_ = in.Close()
		return fmt.Errorf("cache: store %s: open tmp: %w", dst, err)
	}
	tmpPath := tmp.Name()
	closed := false
	defer func() {
		if !closed {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()
	// Step 3: copy source bytes into temp.
	if _, err := io.Copy(tmp, in); err != nil {
		_ = in.Close()
		return fmt.Errorf("cache: store %s: copy: %w", dst, err)
	}
	if err := in.Close(); err != nil {
		return fmt.Errorf("cache: store %s: close source: %w", dst, err)
	}
	// Step 4: fsync so the rename publishes durable bytes.
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("cache: store %s: fsync tmp: %w", dst, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cache: store %s: close tmp: %w", dst, err)
	}
	closed = true
	// Step 5: atomic rename. The temp file is in filepath.Dir(dst), so
	// the rename stays on the same filesystem (atomic).
	if err := os.Rename(tmpPath, dst); err != nil {
		// EXDEV: temp file is on a different filesystem than dst.
		// Old code silently fell back to copyFile — the bug B1.2
		// closes. New code refuses: a cross-device cache root is a
		// configuration error that the operator must fix (the cache
		// must be on the same filesystem as /srv/fc).
		_ = os.Remove(tmpPath)
		return fmt.Errorf("cache: store %s: rename tmp: %w", dst, err)
	}
	return nil
}

func (c *Cache) entryPath(sourceHash string, fw Framework) string {
	return filepath.Join(c.root, sourceHash+"."+string(fw), "layer.ext4")
}

// hashFile streams the file at path through sha256 and returns the hex digest.
// The whole file is read; builderd source tarballs are bounded by the plan's
// SourceTarballMaxMB (100 MB Hobby, 250 MB Pro+) so this is safe in memory.
//
//nolint:forbidigo // path is a vetted-id cache file under c.root joined from sourceHash + framework — no customer input reaches the open. Symlink-attack impossible because c.root is apid-owned and populated only by builderd.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("hash: open: %w", err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash: read: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
