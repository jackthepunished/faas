package builderd

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
//
// B1.3 (issue #195): a cache hit requires BOTH the layer file AND a
// matching sidecar. The sidecar is a sibling file
// `<root>/<sha256>.<fw>/layer.sha256` whose contents are the sourceHash
// the layer was built from. A missing sidecar, an empty sidecar, or a
// mismatched sidecar all return a cache miss — the next Store
// re-creates the sidecar (idempotent), so legacy caches written before
// B1.3 self-heal on first Store of the same key.
//
// The sidecar is a tamper-detector for crash-recovery (the layer
// publish is atomic, but a process kill between the layer rename and
// the sidecar write could leave a half-keyed entry). It is NOT a
// content-addressed proof of layer bytes — that requires hashing the
// layer, not the source, and is out of scope for #195 (Tier 3 cosign
// signing territory).
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
	// Sidecar check: must exist, must be regular, must contain the
	// sourceHash. Whitespace-tolerant because Store writes
	// "<sourceHash>\n" and operators may inspect the file with `cat`.
	cs := c.checksumPath(sourceHash, fw)
	sc, err := os.Stat(cs)
	if err != nil {
		return CacheEntry{}, false
	}
	if !sc.Mode().IsRegular() || sc.Size() == 0 {
		return CacheEntry{}, false
	}
	content, err := os.ReadFile(cs)
	if err != nil {
		return CacheEntry{}, false
	}
	if strings.TrimSpace(string(content)) != sourceHash {
		return CacheEntry{}, false
	}
	return CacheEntry{Path: p, Bytes: st.Size()}, true
}

// checksumPath returns the canonical sidecar path for (sourceHash, fw).
// The sidecar is a SIBLING of the layer.ext4 file inside the layer's
// own directory:
//
//	<root>/<sha256>.<fw>/layer.ext4   ← the layer
//	<root>/<sha256>.<fw>/layer.sha256 ← the sidecar
//
// The two filenames are siblings inside the same directory, so neither
// can collide with the other and a future "manifest.json" or "meta.json"
// added to the same dir won't rename-conflict.
func (c *Cache) checksumPath(sourceHash string, fw Framework) string {
	return filepath.Join(c.root, sourceHash+"."+string(fw), "layer.sha256")
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
	// We still write the sidecar if it's missing (legacy cache self-heal
	// after the B1.3 upgrade — a pre-B1.3 entry is a cache miss under
	// the new Lookup, and the sidecar is the small bit of metadata that
	// makes it a hit again).
	if _, err := os.Stat(dst); err == nil {
		return c.writeSidecar(sourceHash, fw)
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
	// Layer published. Now write the sidecar (B1.3). Publish order
	// is layer-first-then-sidecar: a concurrent Lookup arriving
	// after the layer rename but before the sidecar rename sees a
	// layer-without-sidecar and returns a cache miss — the
	// conservative choice. Every successful Lookup after both
	// publishes have landed sees both files.
	return c.writeSidecar(sourceHash, fw)
}

// writeSidecar atomically publishes the sidecar file at
// checksumPath(sourceHash, fw) with content "<sourceHash>\n". Uses
// the same temp+rename idiom as the layer publish: unique temp,
// copy, sync, close, rename. Idempotent on existing sidecar.
func (c *Cache) writeSidecar(sourceHash string, fw Framework) error {
	if c == nil || c.root == "" {
		return errors.New("cache: not configured")
	}
	cs := c.checksumPath(sourceHash, fw)
	if _, err := os.Stat(cs); err == nil {
		return nil // already published
	}
	if err := os.MkdirAll(filepath.Dir(cs), 0o755); err != nil {
		return fmt.Errorf("cache: sidecar mkdir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(cs), "sidecar-*.tmp")
	if err != nil {
		return fmt.Errorf("cache: sidecar open tmp: %w", err)
	}
	tmpPath := tmp.Name()
	closed := false
	defer func() {
		if !closed {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.WriteString(sourceHash + "\n"); err != nil {
		return fmt.Errorf("cache: sidecar write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("cache: sidecar fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cache: sidecar close: %w", err)
	}
	closed = true
	if err := os.Rename(tmpPath, cs); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("cache: sidecar rename: %w", err)
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
