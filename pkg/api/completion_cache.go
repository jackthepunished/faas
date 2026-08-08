// completion_cache.go — Tier A8 / ADR-083.
//
// Slug/org cache that powers `gregale completion <shell>` for the
// per-account positional completion paths (e.g. <slug> in
// `gregale app <slug> ...`). The cache lives at
// ${UserConfigDir}/gregale/completion-cache.json and is rewritten
// on every 2xx response from a qualifying list endpoint (/v1/apps,
// /v1/orgs) by the c.do middleware (client.go::doReq). Completion
// scripts read it on TAB and emit the values; the user never has
// to refresh by hand (auto-refresh model, per ADR-083 §Decision 2).
//
// Security posture (CLAUDE.md §11, gap G2 lean):
//
//   - File path: ${UserConfigDir}/gregale/, mode 0700 for the dir
//     and 0600 for the file. World-readable is a leak of the
//     account's app+org list — equivalent to an unauthenticated
//     `ls` against the API. The cache MUST NOT expose secrets; it
//     only stores slugs, IDs, and names.
//   - Atomic writes: tmp file in the same directory, then os.Rename.
//     Mirrors LocalStorageBackend.Put (storage-tmp-sibling-of-final,
//     cmd/e2e log). A crash mid-write leaves either the previous
//     good file or a fresh tmp that the next refresh overwrites.
//   - TTL: 24h via file mtime. Operators who want a forced refresh
//     just `rm ~/.config/gregale/completion-cache.json` and the
//     next list call repopulates.
//   - Errors: every write is swallowed at the call site (c.do
//     middleware) with slog.Warn. A broken cache must NEVER fail
//     a request — that would be a much worse user experience
//     than a stale completion list.
//
// File format:
//
//	{
//	  "version": 1,
//	  "apps":   [{"slug":"demo","id":"...","name":"demo"}],
//	  "orgs":   [{"slug":"acme","id":"...","name":"Acme"}],
//	  "saved_at": "2026-08-08T12:34:56Z"
//	}
//
// The version field is the schema rev — bumping it invalidates
// every existing cache file. Keep it at 1 until the file shape
// changes; future revs add the version-aware reader path.

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CompletionCache is the on-disk cache of completion-eligible
// values (currently app slugs + org slugs). One cache file per
// machine, per account token (the path doesn't include the account
// id; switching tokens on the same box means the next list call
// overwrites). Safe for concurrent use.
type CompletionCache struct {
	mu   sync.Mutex
	path string // override via env; computed lazily
	now  func() time.Time
	ttl  time.Duration // 24h by default
}

// CompletionCacheEntry is the persisted shape. The version tag
// lets the reader reject caches from incompatible schema revs
// without a per-field feature flag.
type CompletionCacheEntry struct {
	Version int                     `json:"version"`
	Apps    []CompletionCacheRecord `json:"apps"`
	Orgs    []CompletionCacheRecord `json:"orgs"`
	SavedAt time.Time               `json:"saved_at"`
}

// CompletionCacheRecord is one slug-bearing record. ID + Slug +
// Name are all surfaced in completion (some shells prefer slug,
// others prefer name; the field set covers both).
type CompletionCacheRecord struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

const (
	// completionCacheVersion is the schema rev. Bump on breaking
	// changes to CompletionCacheEntry.
	completionCacheVersion = 1

	// completionCacheTTL is how long a cache file is considered
	// fresh. The cache is overwritten on every 2xx list response,
	// so this is mostly a safety net for offline boxes (and a
	// guard against zombie caches persisting across account
	// changes).
	completionCacheTTL = 24 * time.Hour

	// completionCacheEnvPath lets tests (and operators who want
	// a different location) override the computed path. Empty
	// means "use UserConfigDir".
	completionCacheEnvPath = "FAAS_COMPLETION_CACHE_PATH"
)

// NewCompletionCache returns a cache rooted at the default path
// (UserConfigDir/gregale/completion-cache.json). Tests override
// via SetPath().
func NewCompletionCache() *CompletionCache {
	return &CompletionCache{
		now: time.Now,
		ttl: completionCacheTTL,
	}
}

// SetPath overrides the on-disk path. Tests call this with a
// t.TempDir() result to keep the test hermetic.
func (c *CompletionCache) SetPath(path string) {
	c.mu.Lock()
	c.path = path
	c.mu.Unlock()
}

// Path returns the on-disk path the cache reads/writes. The path
// is computed lazily on first call (and cached) — UserConfigDir
// requires the home directory which is stable but accessing it
// before flag parsing is a tad eager.
func (c *CompletionCache) Path() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pathLocked()
}

// pathLocked is Path's body without the lock. Callers MUST hold
// c.mu — used by readLocked and writeEntryLocked when they are
// already inside the MaybeRefresh outer critical section, to avoid
// re-entrant Lock on the same mutex.
func (c *CompletionCache) pathLocked() string {
	if c.path != "" {
		return c.path
	}
	if env := os.Getenv(completionCacheEnvPath); env != "" {
		c.path = env
		return c.path
	}
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	c.path = filepath.Join(base, "gregale", "completion-cache.json")
	return c.path
}

// Read returns the cached entry plus the file's mtime. A missing
// or stale file returns (zero, time.Time{}, nil) — callers can
// always render a no-completion fallback. A corrupt file is
// treated as missing (the next refresh overwrites it) so a
// half-flushed tmp from a previous crash never bricks the CLI.
func (c *CompletionCache) Read() (CompletionCacheEntry, time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readLocked()
}

// readLocked is Read's lock-held body. Callers MUST hold c.mu.
// Used by MaybeRefresh to fold the read-modify-write into one
// critical section.
func (c *CompletionCache) readLocked() (CompletionCacheEntry, time.Time, error) {
	path := c.pathLocked()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return CompletionCacheEntry{}, time.Time{}, nil
		}
		return CompletionCacheEntry{}, time.Time{}, fmt.Errorf("read completion cache: %w", err)
	}
	var e CompletionCacheEntry
	if err := json.Unmarshal(data, &e); err != nil {
		// Corrupt cache — treat as missing. Don't surface the error
		// to the user; the next refresh overwrites.
		return CompletionCacheEntry{}, time.Time{}, nil //nolint:nilerr
	}
	if e.Version != completionCacheVersion {
		return CompletionCacheEntry{}, time.Time{}, nil
	}
	st, err := os.Stat(path)
	if err != nil {
		return CompletionCacheEntry{}, time.Time{}, nil //nolint:nilerr
	}
	return e, st.ModTime(), nil
}

// IsFresh returns true if the cache file is younger than the TTL.
// A zero mtime (missing cache) is not fresh. Stale caches are
// still readable; the freshness check is advisory — completion
// scripts may choose to render stale values rather than nothing.
func (c *CompletionCache) IsFresh(mtime time.Time) bool {
	if mtime.IsZero() {
		return false
	}
	return c.now().Sub(mtime) < c.ttl
}

// WriteEntry persists entry. Atomic: tmp file in the same dir,
// then os.Rename. The dir is created with mode 0700 (the account
// owner's eyes only); the file is 0600. Errors are returned to
// the caller — the c.do middleware swallows them with slog.Warn.
func (c *CompletionCache) WriteEntry(entry CompletionCacheEntry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writeEntryLocked(entry)
}

// writeEntryLocked is WriteEntry's lock-held body. Callers MUST
// hold c.mu. Used by MaybeRefresh to fold the read-modify-write
// into one critical section.
func (c *CompletionCache) writeEntryLocked(entry CompletionCacheEntry) error {
	entry.Version = completionCacheVersion
	if entry.SavedAt.IsZero() {
		entry.SavedAt = c.now().UTC()
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal completion cache: %w", err)
	}
	path := c.pathLocked()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir completion cache dir: %w", err)
	}
	// MkdirAll is umask-honoring; explicit chmod guarantees the dir
	// is 0700 even when the process umask would otherwise strip
	// the execute bit. The file mode is enforced via tmp.Chmod
	// below for the same reason.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("chmod completion cache dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "completion-cache.*.tmp")
	if err != nil {
		return fmt.Errorf("create completion cache tmp: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) } //nolint:errcheck
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close() //nolint:errcheck
		cleanup()
		return fmt.Errorf("write completion cache tmp: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close() //nolint:errcheck
		cleanup()
		return fmt.Errorf("chmod completion cache tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close completion cache tmp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("rename completion cache tmp: %w", err)
	}
	return nil
}

// MaybeRefresh inspects a 2xx response body and, if path matches
// a known list endpoint, decodes the relevant slug field and
// rewrites the cache. Errors are swallowed at the call site
// (c.do logs via slog.Warn) — a broken cache must never fail a
// request.
//
// Recognised paths today:
//
//   - GET /v1/apps   → bare JSON array of AppResponse (slug field).
//   - GET /v1/orgs   → {"orgs":[OrgResponse, ...]} envelope.
//
// Add new endpoints by extending the path switch. The cache file
// is whole-record-replaced on every qualifying response (small N
// — accounts have at most a few hundred apps).
//
// Concurrency: the read-modify-write is serialised under c.mu so
// concurrent refreshes of DIFFERENT fields (one goroutine hitting
// /v1/apps while another hits /v1/orgs) don't clobber each other.
// Read + WriteEntry both take the lock individually; without the
// outer lock here, the inner locks allow a baseline read, a peer
// rewrite, and a stale baseline write that loses the peer's field.
// The outer lock collapses the RMW into one critical section.
func (c *CompletionCache) MaybeRefresh(path string, body []byte) {
	if len(body) == 0 {
		return
	}
	switch path {
	case "/v1/apps":
		var apps []struct {
			ID   string `json:"id"`
			Slug string `json:"slug"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(body, &apps); err != nil {
			return
		}
		recs := make([]CompletionCacheRecord, 0, len(apps))
		for _, a := range apps {
			if a.Slug == "" {
				continue
			}
			recs = append(recs, CompletionCacheRecord{ID: a.ID, Slug: a.Slug, Name: a.Name})
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		existing, _, _ := c.readLocked()
		existing.Apps = recs
		_ = c.writeEntryLocked(existing)
	case "/v1/orgs":
		var env struct {
			Orgs []struct {
				ID   string `json:"id"`
				Slug string `json:"slug"`
				Name string `json:"name"`
			} `json:"orgs"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			return
		}
		recs := make([]CompletionCacheRecord, 0, len(env.Orgs))
		for _, o := range env.Orgs {
			if o.Slug == "" {
				continue
			}
			recs = append(recs, CompletionCacheRecord{ID: o.ID, Slug: o.Slug, Name: o.Name})
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		existing, _, _ := c.readLocked()
		existing.Orgs = recs
		_ = c.writeEntryLocked(existing)
	}
}
