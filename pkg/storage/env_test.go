package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseLocalPrefixes_Default: an empty env var returns the
// canonical ADR-054 default list. The default is the union of
// {snap, base, kernel, layers} — content-addressed, latency-
// sensitive, small enough to keep on every box.
func TestParseLocalPrefixes_Default(t *testing.T) {
	t.Setenv("FAAS_STORAGE_LOCAL_PREFIXES", "")
	got, err := parseLocalPrefixes("")
	if err != nil {
		t.Fatalf("parseLocalPrefixes: %v", err)
	}
	want := []string{"snap/", "base/", "kernel/", "layers/"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (got %v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestParseLocalPrefixes_CustomList: a non-empty comma-separated
// list is honoured verbatim (whitespace + trailing slash normalised).
func TestParseLocalPrefixes_CustomList(t *testing.T) {
	got, err := parseLocalPrefixes("snap/, base/ ,kernel/")
	if err != nil {
		t.Fatalf("parseLocalPrefixes: %v", err)
	}
	want := []string{"snap/", "base/", "kernel/"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (got %v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestParseLocalPrefixes_Dedup: a list with duplicates dedups
// while preserving order. The router requires unique prefixes.
func TestParseLocalPrefixes_Dedup(t *testing.T) {
	got, err := parseLocalPrefixes("snap/,base/,snap/")
	if err != nil {
		t.Fatalf("parseLocalPrefixes: %v", err)
	}
	want := []string{"snap/", "base/"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (got %v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestParseLocalPrefixes_TrailingSlashAuto: a list with bare
// prefixes (no trailing slash) is normalised to the router's
// contract.
func TestParseLocalPrefixes_TrailingSlashAuto(t *testing.T) {
	got, err := parseLocalPrefixes("snap,base")
	if err != nil {
		t.Fatalf("parseLocalPrefixes: %v", err)
	}
	want := []string{"snap/", "base/"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (got %v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestParseLocalPrefixes_AllEmpty: a list of only commas +
// whitespace is rejected (the router would lose its fallback).
func TestParseLocalPrefixes_AllEmpty(t *testing.T) {
	if _, err := parseLocalPrefixes(" , , "); err == nil {
		t.Fatal("parseLocalPrefixes accepted all-empty list; want error")
	}
}

// TestBackendFromEnv_LocalIgnoresLocalPrefixes: the local-only
// backend IGNORES FAAS_STORAGE_LOCAL_PREFIXES. The local-prefix
// list is an OCI-side knob (snap/, base/, kernel/, layers/ are the
// prefixes NOT shipped to the registry); in a local-only deployment
// there's nothing to keep apart, and honouring them as routes
// strips prefixes and crashes imaged under ProtectSystem=strict
// (CI run 30650464753, 2026-07-31). The local router must always
// register apps/ as the only route and let everything else fall
// through to FAAS_STORAGE_ROOT.
func TestBackendFromEnv_LocalIgnoresLocalPrefixes(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FAAS_STORAGE_BACKEND", "local")
	t.Setenv("FAAS_STORAGE_ROOT", filepath.Join(tmp, "fc"))
	t.Setenv("FAAS_APPS_ROOT", filepath.Join(tmp, "apps"))
	// Even with a custom prefix list, the local backend keeps the
	// router shape minimal — just apps/.
	t.Setenv("FAAS_STORAGE_LOCAL_PREFIXES", "snap/,base/,kernel/")
	be, err := BackendFromEnv()
	if err != nil {
		t.Fatalf("BackendFromEnv: %v", err)
	}
	router, ok := be.(*PrefixRouter)
	if !ok {
		t.Fatalf("backend type = %T, want *PrefixRouter", be)
	}
	// Only apps/ is a route. snap/, base/, kernel/ fall through to
	// fcBackend with their full prefix preserved (so a Put for
	// "base/runner-builder-amd64.ext4" lands at
	// /srv/fc/base/runner-builder-amd64.ext4, not
	// /srv/fc/runner-builder-amd64.ext4).
	if len(router.routes) != 1 {
		t.Errorf("routes = %d, want 1 (apps only); got %v",
			len(router.routes), router.routes)
	}
	if _, ok := router.routes["apps/"]; !ok {
		t.Errorf("apps/ route missing; routes = %v", router.routes)
	}
	if router.fallback == nil {
		t.Error("fallback is nil; want FAAS_STORAGE_ROOT backend")
	}
}

// TestBackendFromEnv_LocalEmptyPrefixesAccepted: the local backend
// accepts an empty/all-whitespace FAAS_STORAGE_LOCAL_PREFIXES value
// because it doesn't parse it. (The OCI backend still rejects empty
// lists — see TestBackendFromEnv_OCIRequiresRegistry for the
// equivalent there.) A custom-prefix override that would be invalid
// for OCI is silently OK for local; that's the desired asymmetry.
func TestBackendFromEnv_LocalEmptyPrefixesAccepted(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FAAS_STORAGE_BACKEND", "local")
	t.Setenv("FAAS_STORAGE_ROOT", filepath.Join(tmp, "fc"))
	t.Setenv("FAAS_APPS_ROOT", filepath.Join(tmp, "apps"))
	t.Setenv("FAAS_STORAGE_LOCAL_PREFIXES", " , , ")
	if _, err := BackendFromEnv(); err != nil {
		t.Fatalf("BackendFromEnv rejected empty prefixes list; want accept (local-only ignores it): %v", err)
	}
}

// TestBackendFromEnv_LocalDefaults exercises the local backend fork
// with production default roots — the FAAS_APPS_ROOT default
// (/var/lib/faas/apps) differs from FAAS_STORAGE_ROOT (/srv/fc), so
// the helper produces a PrefixRouter.
func TestBackendFromEnv_LocalDefaults(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FAAS_STORAGE_BACKEND", "")
	t.Setenv("FAAS_STORAGE_ROOT", filepath.Join(tmp, "fc"))
	t.Setenv("FAAS_APPS_ROOT", filepath.Join(tmp, "apps"))
	be, err := BackendFromEnv()
	if err != nil {
		t.Fatalf("BackendFromEnv: %v", err)
	}
	if _, ok := be.(*PrefixRouter); !ok {
		t.Errorf("backend type = %T, want *PrefixRouter (default split)", be)
	}
}

// TestBackendFromEnv_LocalSplit exercises the local fork with
// FAAS_APPS_ROOT pointing at a separate dir (production deploys the
// two as siblings).
func TestBackendFromEnv_LocalSplit(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FAAS_STORAGE_BACKEND", "local")
	t.Setenv("FAAS_STORAGE_ROOT", filepath.Join(tmp, "fc"))
	t.Setenv("FAAS_APPS_ROOT", filepath.Join(tmp, "apps"))
	be, err := BackendFromEnv()
	if err != nil {
		t.Fatalf("BackendFromEnv: %v", err)
	}
	if _, ok := be.(*PrefixRouter); !ok {
		t.Errorf("backend type = %T, want *PrefixRouter (split layout)", be)
	}
}

// TestBackendFromEnv_LocalCoalesced verifies the router collapses to a
// single LocalStorageBackend when the two roots coincide.
func TestBackendFromEnv_LocalCoalesced(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FAAS_STORAGE_BACKEND", "local")
	t.Setenv("FAAS_STORAGE_ROOT", tmp)
	t.Setenv("FAAS_APPS_ROOT", tmp)
	be, err := BackendFromEnv()
	if err != nil {
		t.Fatalf("BackendFromEnv: %v", err)
	}
	if _, ok := be.(*LocalStorageBackend); !ok {
		t.Errorf("backend type = %T, want *LocalStorageBackend (coalesced)", be)
	}
}

// TestBackendFromEnv_LocalPutLandsInSubdir is the end-to-end
// regression for the 2026-07-31 imaged crash-loop on the EX44 (CI
// run 30650464753). The local-only backend must Put a key like
// "base/runner-builder-amd64.ext4" into the FAAS_STORAGE_ROOT's
// base/ subdir — NOT into the root. Pre-fix the local backend
// registered "base/" as a PrefixRouter route → fcBackend, which
// stripped the prefix and made fcBackend.Put land the file at
// <root>/runner-builder-amd64.ext4 instead of
// <root>/base/runner-builder-amd64.ext4. The temp file followed the
// same wrong path: /srv/fc/.faas-tmp-<rand> instead of
// /srv/fc/base/.faas-tmp-<rand>, and EROFS'd under ProtectSystem=strict.
func TestBackendFromEnv_LocalPutLandsInSubdir(t *testing.T) {
	tmp := t.TempDir()
	fcRoot := filepath.Join(tmp, "fc")
	appsRoot := filepath.Join(tmp, "apps")
	t.Setenv("FAAS_STORAGE_BACKEND", "local")
	t.Setenv("FAAS_STORAGE_ROOT", fcRoot)
	t.Setenv("FAAS_APPS_ROOT", appsRoot)
	be, err := BackendFromEnv()
	if err != nil {
		t.Fatalf("BackendFromEnv: %v", err)
	}
	// Put for "base/runner-builder-amd64.ext4" must land at
	// <fcRoot>/base/runner-builder-amd64.ext4, not at
	// <fcRoot>/runner-builder-amd64.ext4.
	if err := be.Put(t.Context(), "base/runner-builder-amd64.ext4", strings.NewReader("payload")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	want := filepath.Join(fcRoot, "base", "runner-builder-amd64.ext4")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("file at %s: %v (this is the 2026-07-31 bug shape — file landed at the wrong path)", want, err)
	}
	// Belt-and-braces: no file at the root with the basename only.
	wrongPath := filepath.Join(fcRoot, "runner-builder-amd64.ext4")
	if _, err := os.Stat(wrongPath); err == nil {
		t.Errorf("file at %s — pre-fix bug shape; router stripped the base/ prefix", wrongPath)
	}
}

// TestBackendFromEnv_OCIRequiresRegistry verifies the OCI fork
// refuses to default without FAAS_OCI_REGISTRY.
func TestBackendFromEnv_OCIRequiresRegistry(t *testing.T) {
	t.Setenv("FAAS_STORAGE_BACKEND", "oci")
	os.Unsetenv("FAAS_OCI_REGISTRY") // ensure unset
	_, err := BackendFromEnv()
	if err == nil {
		t.Fatal("expected error for oci backend without registry")
	}
}

// TestBackendFromEnv_OCIRejectsUnknown verifies unknown backend kinds
// are rejected at startup.
func TestBackendFromEnv_OCIRejectsUnknown(t *testing.T) {
	t.Setenv("FAAS_STORAGE_BACKEND", "s3")
	_, err := BackendFromEnv()
	if err == nil {
		t.Fatal("expected error for unknown backend kind")
	}
	if got := err.Error(); !strings.Contains(got, "unknown") {
		t.Errorf("error %q lacks 'unknown'", got)
	}
}

// TestResolveCacheDir_OCIDefaultsCacheDir pins the multi-box default:
// FAAS_STORAGE_BACKEND=oci + FAAS_STORAGE_CACHE_DIR unset →
// DefaultOCICacheDir, wrap=true.
func TestResolveCacheDir_OCIDefaultsCacheDir(t *testing.T) {
	os.Unsetenv("FAAS_STORAGE_CACHE_DIR")
	dir, ok := resolveCacheDir("oci")
	if !ok {
		t.Fatal("resolveCacheDir(oci) ok=false; want true (default-on)")
	}
	if dir != DefaultOCICacheDir {
		t.Errorf("dir = %q, want %q", dir, DefaultOCICacheDir)
	}
}

// TestResolveCacheDir_OCICustomCacheDir: a custom dir is honoured
// verbatim (no rewriting).
func TestResolveCacheDir_OCICustomCacheDir(t *testing.T) {
	t.Setenv("FAAS_STORAGE_CACHE_DIR", "/var/lib/faas/cache-prod")
	dir, ok := resolveCacheDir("oci")
	if !ok {
		t.Fatal("ok=false; want true")
	}
	if dir != "/var/lib/faas/cache-prod" {
		t.Errorf("dir = %q, want %q", dir, "/var/lib/faas/cache-prod")
	}
}

// TestResolveCacheDir_OCIExplicitDisable: explicit empty disables
// regardless of kind. The os.LookupEnv distinction is load-bearing —
// operators use the empty form to opt out when the default would
// otherwise wrap.
func TestResolveCacheDir_OCIExplicitDisable(t *testing.T) {
	t.Setenv("FAAS_STORAGE_CACHE_DIR", "")
	dir, ok := resolveCacheDir("oci")
	if ok {
		t.Errorf("ok=true; want false (explicit disable). dir=%q", dir)
	}
}

// TestResolveCacheDir_LocalNoDefault: single-box stays opt-in.
func TestResolveCacheDir_LocalNoDefault(t *testing.T) {
	os.Unsetenv("FAAS_STORAGE_CACHE_DIR")
	if _, ok := resolveCacheDir("local"); ok {
		t.Error("ok=true; want false (single-box opt-in)")
	}
}

// TestBackendFromEnv_OCIDefaultsCacheDirHermetic: full BackendFromEnv
// path with oci mode + override cache dir to t.TempDir. Asserts the
// returned backend is *LocalCacheBackend rooted at the temp dir. Uses
// t.Setenv("FAAS_STORAGE_CACHE_DIR", tmp) instead of relying on the
// /var/lib/faas/cache default — that way CI never creates the
// production path on a developer machine.
func TestBackendFromEnv_OCIDefaultsCacheDirHermetic(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FAAS_STORAGE_BACKEND", "oci")
	t.Setenv("FAAS_OCI_REGISTRY", "http://127.0.0.1:0/fake")
	t.Setenv("FAAS_STORAGE_CACHE_DIR", filepath.Join(tmp, "cache"))
	t.Setenv("FAAS_STORAGE_ROOT", filepath.Join(tmp, "fc"))
	be, err := BackendFromEnv()
	if err != nil {
		t.Fatalf("BackendFromEnv: %v", err)
	}
	cache, ok := be.(*LocalCacheBackend)
	if !ok {
		t.Fatalf("backend = %T, want *LocalCacheBackend", be)
	}
	if cache.Root() != filepath.Join(tmp, "cache") {
		t.Errorf("Root() = %q, want %q", cache.Root(), filepath.Join(tmp, "cache"))
	}
}

// TestBackendFromEnv_OCIDoesNotWrapWhenExplicitlyDisabled: explicit
// empty cache dir + oci mode → backend is NOT *LocalCacheBackend.
func TestBackendFromEnv_OCIDoesNotWrapWhenExplicitlyDisabled(t *testing.T) {
	t.Setenv("FAAS_STORAGE_BACKEND", "oci")
	t.Setenv("FAAS_OCI_REGISTRY", "http://127.0.0.1:0/fake")
	t.Setenv("FAAS_STORAGE_CACHE_DIR", "")
	t.Setenv("FAAS_STORAGE_ROOT", t.TempDir())
	be, err := BackendFromEnv()
	if err != nil {
		t.Fatalf("BackendFromEnv: %v", err)
	}
	if _, ok := be.(*LocalCacheBackend); ok {
		t.Errorf("backend wrapped as *LocalCacheBackend; want no wrap (explicit disable)")
	}
}

// TestBackendFromEnv_LocalDoesNotDefaultCacheDir: single-box stays
// opt-in even when the cache dir env var is unset. (The /var/lib/faas/cache
// default applies to oci mode only.)
func TestBackendFromEnv_LocalDoesNotDefaultCacheDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FAAS_STORAGE_BACKEND", "local")
	t.Setenv("FAAS_STORAGE_ROOT", tmp)
	os.Unsetenv("FAAS_STORAGE_CACHE_DIR")
	be, err := BackendFromEnv()
	if err != nil {
		t.Fatalf("BackendFromEnv: %v", err)
	}
	if _, ok := be.(*LocalCacheBackend); ok {
		t.Errorf("backend wrapped as *LocalCacheBackend; local mode stays opt-in")
	}
}
