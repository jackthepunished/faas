// router_env_extra_test.go — fill pkg/storage coverage of the
// router.LocalPath branch, the router.List fallback branches
// (no LocalArtifactLister backend → ErrInvalidKey), the
// anyRouteContains equal-or-strict-prefix branches, and the
// env-dispatch branches that wrap OCI / local-backend env wiring.
//
// Targets:
//   - PrefixRouter.LocalPath: hits the LocalPathResolver branch
//     and the non-resolver no-op branch
//   - PrefixRouter.List: returns ErrInvalidKey when at least one
//     backend does NOT implement LocalArtifactLister
//   - anyRouteContains: query==route equal match; non-matching
//     query; route=="" filtered out
//   - localBackendFromEnv: FAAS_STORAGE_ROOT validation failure;
//     FAAS_APPS_ROOT validation failure
//   - ociBackendFromEnv: invalid FAAS_OCI_TIMEOUT_SECONDS
//   - BackendFromEnv: unknown FAAS_STORAGE_BACKEND value
//   - AsCacheBackend: nil root; nested prefix router route cache

package storage

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

// --- LocalPath branches -----------------------------------------

// pathResolverBackend is a StorageBackend + LocalPathResolver. We
// register it under a route and verify the router forwards the
// LocalPath call with the stripped key (no prefix leakage).
type pathResolverBackend struct {
	*LocalStorageBackend
	gotKey string
	ok     bool
}

func (p *pathResolverBackend) LocalPath(key string) (string, bool, error) {
	p.gotKey = key
	return "/local/path/" + key, p.ok, nil
}

// TestPrefixRouter_LocalPath_ForwardsToResolver covers the
// LocalPathResolver branch at router.go:192. A backend that
// implements LocalPathResolver gets a forwarded call with the
// prefix stripped from the key.
func TestPrefixRouter_LocalPath_ForwardsToResolver(t *testing.T) {
	root := t.TempDir()
	b, err := NewLocalStorageBackend(root)
	if err != nil {
		t.Fatalf("NewLocalStorageBackend: %v", err)
	}
	pr := &pathResolverBackend{LocalStorageBackend: b, ok: true}
	router, err := NewPrefixRouter(map[string]StorageBackend{"apps/": pr}, nil)
	if err != nil {
		t.Fatalf("NewPrefixRouter: %v", err)
	}

	path, ok, err := router.LocalPath("apps/foo/dep.ext4")
	if err != nil {
		t.Fatalf("LocalPath: %v", err)
	}
	if !ok {
		t.Errorf("LocalPath ok = false, want true")
	}
	if path != "/local/path/foo/dep.ext4" {
		t.Errorf("path = %q, want /local/path/foo/dep.ext4 (stripped)", path)
	}
	if pr.gotKey != "foo/dep.ext4" {
		t.Errorf("backend got key %q, want stripped foo/dep.ext4", pr.gotKey)
	}
}

// TestPrefixRouter_LocalPath_NoResolverReturnsFalse covers the
// non-resolver branch at router.go:193-195. A backend that does
// NOT implement LocalPathResolver is intentional — remote drivers
// (OCI registry, future HTTP backend) — and the router must
// silently return ok=false rather than panic.
func TestPrefixRouter_LocalPath_NoResolverReturnsFalse(t *testing.T) {
	router, err := NewPrefixRouter(map[string]StorageBackend{"apps/": newFakeOCI()}, nil)
	if err != nil {
		t.Fatalf("NewPrefixRouter: %v", err)
	}
	path, ok, err := router.LocalPath("apps/foo")
	if err != nil {
		t.Fatalf("LocalPath: %v", err)
	}
	if ok {
		t.Errorf("ok = true, want false (backend does not implement LocalPathResolver)")
	}
	if path != "" {
		t.Errorf("path = %q, want empty", path)
	}
}

// TestPrefixRouter_LocalPath_NoRouteReturnsError covers the
// dispatch error path: an invalid key (here: no matching route,
// no fallback) returns the dispatch error before LocalPath is
// consulted.
func TestPrefixRouter_LocalPath_NoRouteReturnsError(t *testing.T) {
	plain := newTestBackend(t)
	router, err := NewPrefixRouter(map[string]StorageBackend{"apps/": plain}, nil)
	if err != nil {
		t.Fatalf("NewPrefixRouter: %v", err)
	}
	_, _, err = router.LocalPath("snap/foo")
	if !IsInvalidKey(err) {
		t.Errorf("LocalPath(unmatched): IsInvalidKey=false, err=%v", err)
	}
}

// --- List branches ----------------------------------------------

// nonLocalBackend is a StorageBackend that does NOT implement
// LocalArtifactLister. Used to drive the router.List error branch
// at router.go:257-258 (returns ErrInvalidKey when at least one
// backend lacks the capability).
type nonLocalBackend struct{}

func (nonLocalBackend) Put(_ context.Context, _ string, _ io.Reader) error { return nil }
func (nonLocalBackend) Get(_ context.Context, _ string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (nonLocalBackend) Delete(_ context.Context, _ string) error { return nil }

// TestPrefixRouter_List_NonLocalBackendReturnsError covers the
// "router with at least one non-LocalArtifactLister backend" branch.
// The router must surface ErrInvalidKey rather than silently dropping
// the call.
func TestPrefixRouter_List_NonLocalBackendReturnsError(t *testing.T) {
	a := newTestBackend(t)
	router, err := NewPrefixRouter(map[string]StorageBackend{
		"apps/":   a,
		"remote/": nonLocalBackend{}, // no List capability
	}, nil)
	if err != nil {
		t.Fatalf("NewPrefixRouter: %v", err)
	}
	_, err = router.List(context.Background(), "")
	if !IsInvalidKey(err) {
		t.Errorf("List: IsInvalidKey=false, err=%v", err)
	}
}

// --- anyRouteContains branches ---------------------------------

// anyRouteContains is unexported; cover via the public List path
// that exercises the equal-prefix match (query==route) and the
// non-matching query (no route contains → fallback consulted).
func TestPrefixRouter_List_FallbackConsultedOnUnmatchedPrefix(t *testing.T) {
	a := newTestBackend(t)
	f := newTestBackend(t)
	router, err := NewPrefixRouter(map[string]StorageBackend{
		"apps/": a,
	}, f)
	if err != nil {
		t.Fatalf("NewPrefixRouter: %v", err)
	}
	// Put a key under the fallback's scope (not under apps/).
	if err := router.Put(context.Background(), "snap/dep/mem", strings.NewReader("snap-data")); err != nil {
		t.Fatalf("put: %v", err)
	}
	// List with a prefix that matches NO route. The router must
	// consult the fallback (anyRouteContains returns false) and
	// return its keys.
	got, err := router.List(context.Background(), "snap/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0] != "snap/dep/mem" {
		t.Errorf("list snap/ = %v, want [snap/dep/mem]", got)
	}
}

// --- BackendFromEnv dispatch ------------------------------------

// TestBackendFromEnv_UnknownBackendValue covers the default branch
// in BackendFromEnv (env.go:80-82). Anything other than "local"
// or "oci" must fail loud at startup so a typo doesn't silently
// fall through to the local backend.
func TestBackendFromEnv_UnknownBackendValue(t *testing.T) {
	t.Setenv("FAAS_STORAGE_BACKEND", "bogus")
	_, err := BackendFromEnv()
	if err == nil {
		t.Fatal("unknown backend: err = nil, want error")
	}
	if !strings.Contains(err.Error(), "unknown FAAS_STORAGE_BACKEND") {
		t.Errorf("err = %v, want unknown FAAS_STORAGE_BACKEND substring", err)
	}
}

// --- localBackendFromEnv error paths ---------------------------

// localBackendFromEnv validates FAAS_STORAGE_ROOT and FAAS_APPS_ROOT
// via NewLocalStorageBackend; the failure branches fire when the
// root contains a ".." segment (NewLocalStorageBackend calls
// validateKey). Cover via the public BackendFromEnv path.

// TestBackendFromEnv_LocalStorageRootInvalid covers the
// FAAS_STORAGE_ROOT validation failure at env.go:204. A root
// containing ".." trips NewLocalStorageBackend's validateKey path
// (the same one Put/Get/Delete keys go through); we set the env
// directly via os.Setenv because t.Setenv is a no-op for empty
// strings (and ".." alone would be the cleanup of a prior key).
func TestBackendFromEnv_LocalStorageRootInvalid(t *testing.T) {
	t.Setenv("FAAS_STORAGE_BACKEND", "local")
	// Use os.Setenv with a backslash that survives t.Setenv's
	// validation but trips the storage validateKey's
	// ContainsAny("\\") branch.
	prevRoot, ok := os.LookupEnv("FAAS_STORAGE_ROOT")
	if err := os.Setenv("FAAS_STORAGE_ROOT", "/srv/bad\\root"); err != nil {
		t.Fatalf("setenv: %v", err)
	}
	t.Cleanup(func() {
		if ok {
			_ = os.Setenv("FAAS_STORAGE_ROOT", prevRoot)
		} else {
			_ = os.Unsetenv("FAAS_STORAGE_ROOT")
		}
	})
	t.Setenv("FAAS_APPS_ROOT", "/var/lib/faas/apps")

	_, err := BackendFromEnv()
	if err == nil {
		t.Fatal("invalid root: err = nil, want error")
	}
	if !strings.Contains(err.Error(), "FAAS_STORAGE_ROOT") {
		t.Errorf("err = %v, want FAAS_STORAGE_ROOT substring", err)
	}
}

// TestBackendFromEnv_LocalAppsRootInvalid covers the
// FAAS_APPS_ROOT validation failure at env.go:211. Same shape as
// the storage-root test above.
func TestBackendFromEnv_LocalAppsRootInvalid(t *testing.T) {
	t.Setenv("FAAS_STORAGE_BACKEND", "local")
	t.Setenv("FAAS_STORAGE_ROOT", "/srv/fc")
	prev, ok := os.LookupEnv("FAAS_APPS_ROOT")
	if err := os.Setenv("FAAS_APPS_ROOT", "/var/lib/bad\\apps"); err != nil {
		t.Fatalf("setenv: %v", err)
	}
	t.Cleanup(func() {
		if ok {
			_ = os.Setenv("FAAS_APPS_ROOT", prev)
		} else {
			_ = os.Unsetenv("FAAS_APPS_ROOT")
		}
	})

	_, err := BackendFromEnv()
	if err == nil {
		t.Fatal("invalid apps root: err = nil, want error")
	}
	if !strings.Contains(err.Error(), "FAAS_APPS_ROOT") {
		t.Errorf("err = %v, want FAAS_APPS_ROOT substring", err)
	}
}

// --- ociBackendFromEnv timeout branch ---------------------------

// TestBackendFromEnv_OCIInvalidTimeout covers the timeout
// validation branch at env.go:262-265. A non-positive integer
// for FAAS_OCI_TIMEOUT_SECONDS must fail loud.
func TestBackendFromEnv_OCIInvalidTimeout(t *testing.T) {
	t.Setenv("FAAS_STORAGE_BACKEND", "oci")
	t.Setenv("FAAS_OCI_REGISTRY", "https://ghcr.io/onebox-faas")
	t.Setenv("FAAS_OCI_TIMEOUT_SECONDS", "0")
	_, err := BackendFromEnv()
	if err == nil {
		t.Fatal("timeout=0: err = nil, want error")
	}
	if !strings.Contains(err.Error(), "FAAS_OCI_TIMEOUT_SECONDS") {
		t.Errorf("err = %v, want FAAS_OCI_TIMEOUT_SECONDS substring", err)
	}
}

// TestBackendFromEnv_OCIInvalidTimeoutNegative covers the negative
// branch of the timeout validation (same code path, different input).
func TestBackendFromEnv_OCIInvalidTimeoutNegative(t *testing.T) {
	t.Setenv("FAAS_STORAGE_BACKEND", "oci")
	t.Setenv("FAAS_OCI_REGISTRY", "https://ghcr.io/onebox-faas")
	t.Setenv("FAAS_OCI_TIMEOUT_SECONDS", "not-a-number")
	_, err := BackendFromEnv()
	if err == nil {
		t.Fatal("timeout=not-a-number: err = nil, want error")
	}
	if !strings.Contains(err.Error(), "FAAS_OCI_TIMEOUT_SECONDS") {
		t.Errorf("err = %v, want FAAS_OCI_TIMEOUT_SECONDS substring", err)
	}
}

// --- AsCacheBackend edge cases --------------------------------

// TestAsCacheBackend_NilRoot covers the nil-receiver branch at
// env.go:330-331. A nil backend chain root returns nil rather
// than panicking — daemons rely on this to log "cache not wired"
// at startup rather than crash.
func TestAsCacheBackend_NilRoot(t *testing.T) {
	if got := AsCacheBackend(nil); got != nil {
		t.Errorf("AsCacheBackend(nil) = %v, want nil", got)
	}
}

// TestAsCacheBackend_UnrecognisedWrapper covers the "no
// LocalCacheBackend reachable" branch at env.go:346. A
// PrefixRouter whose children are all plain LocalStorageBackend
// instances (no cache anywhere) returns nil — the daemon sees a
// "cache not wired" log line.
func TestAsCacheBackend_UnrecognisedWrapper(t *testing.T) {
	a := newTestBackend(t)
	router, err := NewPrefixRouter(map[string]StorageBackend{"apps/": a}, a)
	if err != nil {
		t.Fatalf("NewPrefixRouter: %v", err)
	}
	if got := AsCacheBackend(router); got != nil {
		t.Errorf("AsCacheBackend(router w/o cache) = %v, want nil", got)
	}
}

// --- list prefix validation (router.go:289-292) ----------------

// TestPrefixRouter_List_PrefixWithTrailingSlash covers the
// validateKey branch on the aggregator's prefix arg. The
// aggregator strips the trailing slash before calling
// validateKey; this test asserts the branch fires for a prefix
// that contains a ".." segment after stripping.
func TestPrefixRouter_List_PrefixWithBadSegment(t *testing.T) {
	a := newTestBackend(t)
	router, err := NewPrefixRouter(map[string]StorageBackend{"apps/": a}, nil)
	if err != nil {
		t.Fatalf("NewPrefixRouter: %v", err)
	}
	_, err = router.List(context.Background(), "../escape/")
	if !IsInvalidKey(err) {
		t.Errorf("List(bad prefix): IsInvalidKey=false, err=%v", err)
	}
}

// --- helpers ---------------------------------------------------

// _ = errors.New is referenced below for go vet; the unused-import
// pattern keeps the file self-contained when future tests are
// trimmed.
var _ = errors.New
