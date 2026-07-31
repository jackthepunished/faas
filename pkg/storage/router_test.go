package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"testing"
)

// TestPrefixRouterPutGetDelete is the round-trip suite: a router
// with two backends (apps/ → A, snap/ → S) and a fallback (F)
// routes each prefix correctly and the fallback catches the rest.
// The keys are written via the router and read back via the router
// to assert the wrappers don't lose data.
func TestPrefixRouterPutGetDelete(t *testing.T) {
	a := newTestBackend(t)
	s := newTestBackend(t)
	f := newTestBackend(t)
	router, err := NewPrefixRouter(map[string]StorageBackend{
		"apps/": a,
		"snap/": s,
	}, f)
	if err != nil {
		t.Fatalf("NewPrefixRouter: %v", err)
	}
	ctx := context.Background()

	if err := router.Put(ctx, "apps/slug/dep.ext4", strings.NewReader("app-data")); err != nil {
		t.Fatalf("put apps: %v", err)
	}
	if err := router.Put(ctx, "snap/dep/mem", strings.NewReader("snap-data")); err != nil {
		t.Fatalf("put snap: %v", err)
	}
	if err := router.Put(ctx, "base/runtime.ext4", strings.NewReader("base-data")); err != nil {
		t.Fatalf("put base: %v", err)
	}

	// Each key must read back via the router; the contents must
	// match what we Put, and the underlying backend must have the
	// file at the stripped path (no prefix leakage).
	mustReadRouter := func(key, want string) {
		t.Helper()
		got := mustReadAll(t, router, key)
		if got != want {
			t.Fatalf("get %s: got %q, want %q", key, got, want)
		}
	}
	mustReadRouter("apps/slug/dep.ext4", "app-data")
	mustReadRouter("snap/dep/mem", "snap-data")
	mustReadRouter("base/runtime.ext4", "base-data")

	// Underlying backends hold the stripped keys.
	if got := mustReadAll(t, a, "slug/dep.ext4"); got != "app-data" {
		t.Fatalf("apps backend: got %q, want %q", got, "app-data")
	}
	if got := mustReadAll(t, s, "dep/mem"); got != "snap-data" {
		t.Fatalf("snap backend: got %q, want %q", got, "snap-data")
	}

	// Delete through the router removes the file from the right
	// backend.
	if err := router.Delete(ctx, "apps/slug/dep.ext4"); err != nil {
		t.Fatalf("delete apps: %v", err)
	}
	if _, err := a.Get(ctx, "slug/dep.ext4"); !IsNotFound(err) {
		t.Fatalf("apps backend after delete: got %v, want IsNotFound", err)
	}
	if err := router.Delete(ctx, "snap/dep/mem"); err != nil {
		t.Fatalf("delete snap: %v", err)
	}
	if _, err := s.Get(ctx, "dep/mem"); !IsNotFound(err) {
		t.Fatalf("snap backend after delete: got %v, want IsNotFound", err)
	}
}

// TestPrefixRouterLongestMatch covers the longest-prefix-wins
// rule: with routes "apps/" and "apps/acme/", a key under
// "apps/acme/" must land on the second backend, not the first.
func TestPrefixRouterLongestMatch(t *testing.T) {
	a := newTestBackend(t)
	ac := newTestBackend(t)
	router, err := NewPrefixRouter(map[string]StorageBackend{
		"apps/":      a,
		"apps/acme/": ac,
	}, nil)
	if err != nil {
		t.Fatalf("NewPrefixRouter: %v", err)
	}
	ctx := context.Background()
	if err := router.Put(ctx, "apps/acme/dep.ext4", strings.NewReader("acme")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if got := mustReadAll(t, ac, "dep.ext4"); got != "acme" {
		t.Fatalf("acme backend: got %q, want %q", got, "acme")
	}
	// The apps backend must not have the file at the top-level
	// (a previous broken dispatch would have stored the file
	// verbatim there).
	if _, err := a.Get(ctx, "acme/dep.ext4"); !IsNotFound(err) {
		t.Fatalf("apps backend unexpectedly has acme/dep.ext4: %v", err)
	}
}

// TestPrefixRouterFallback covers the no-matching-route path: when
// a key matches no route, it lands in the fallback. The fallback is
// the production pattern for /srv/fc holding most prefixes and
// /var/lib/faas/apps holding only "apps/".
func TestPrefixRouterFallback(t *testing.T) {
	a := newTestBackend(t)
	f := newTestBackend(t)
	router, err := NewPrefixRouter(map[string]StorageBackend{
		"apps/": a,
	}, f)
	if err != nil {
		t.Fatalf("NewPrefixRouter: %v", err)
	}
	ctx := context.Background()
	if err := router.Put(ctx, "snap/dep/mem", strings.NewReader("snap")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if got := mustReadAll(t, f, "snap/dep/mem"); got != "snap" {
		t.Fatalf("fallback: got %q, want %q", got, "snap")
	}
}

// TestPrefixRouterNoRouteNoFallback covers the case where a key
// matches no route and there is no fallback — every Put/Get/Delete
// must fail with ErrInvalidKey so callers see a clear "no route"
// error rather than a confusing 404.
func TestPrefixRouterNoRouteNoFallback(t *testing.T) {
	a := newTestBackend(t)
	router, err := NewPrefixRouter(map[string]StorageBackend{
		"apps/": a,
	}, nil)
	if err != nil {
		t.Fatalf("NewPrefixRouter: %v", err)
	}
	ctx := context.Background()
	if err := router.Put(ctx, "snap/dep/mem", strings.NewReader("x")); !IsInvalidKey(err) {
		t.Fatalf("put unmatched: IsInvalidKey=false, err=%v", err)
	}
	if _, err := router.Get(ctx, "snap/dep/mem"); !IsInvalidKey(err) {
		t.Fatalf("get unmatched: IsInvalidKey=false, err=%v", err)
	}
	if err := router.Delete(ctx, "snap/dep/mem"); !IsInvalidKey(err) {
		t.Fatalf("delete unmatched: IsInvalidKey=false, err=%v", err)
	}
}

// TestPrefixRouterListAggregates covers the LocalArtifactLister
// aggregation: keys from every backend come back with their route
// prefix re-applied, in sorted order, with no duplicates.
func TestPrefixRouterListAggregates(t *testing.T) {
	a := newTestBackend(t)
	s := newTestBackend(t)
	f := newTestBackend(t)
	router, err := NewPrefixRouter(map[string]StorageBackend{
		"apps/": a,
		"snap/": s,
	}, f)
	if err != nil {
		t.Fatalf("NewPrefixRouter: %v", err)
	}
	ctx := context.Background()
	keys := []string{
		"apps/slug/dep.ext4",
		"snap/a/mem",
		"snap/a/vmstate",
		"base/runtime.ext4",
	}
	for _, k := range keys {
		if err := router.Put(ctx, k, strings.NewReader("x")); err != nil {
			t.Fatalf("put %s: %v", k, err)
		}
	}
	got, err := router.List(ctx, "")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	want := append([]string{}, keys...)
	sort.Strings(want)
	sort.Strings(got)
	if !equalStrings(got, want) {
		t.Fatalf("list all: got %v, want %v", got, want)
	}
	// Per-prefix list narrows to the matching route only.
	gotApps, err := router.List(ctx, "apps/")
	if err != nil {
		t.Fatalf("list apps/: %v", err)
	}
	if !equalStrings(gotApps, []string{"apps/slug/dep.ext4"}) {
		t.Fatalf("list apps/: got %v", gotApps)
	}
	gotSnap, err := router.List(ctx, "snap/")
	if err != nil {
		t.Fatalf("list snap/: %v", err)
	}
	if !equalStrings(gotSnap, []string{"snap/a/mem", "snap/a/vmstate"}) {
		t.Fatalf("list snap/: got %v", gotSnap)
	}
}

// TestPrefixRouterRejectsBadRoute covers the constructor's prefix
// validation: empty prefix, ".." prefix, and missing trailing slash
// are all rejected before the router is usable. A bad route is a
// misconfiguration and must fail loud at startup.
func TestPrefixRouterRejectsBadRoute(t *testing.T) {
	be := newTestBackend(t)
	// Empty prefix is rejected by the trailing-slash gate before
	// validateKey sees it — the message must mention "end in '/'".
	if _, err := NewPrefixRouter(map[string]StorageBackend{
		"": be,
	}, nil); err == nil {
		t.Fatalf("empty route: nil err")
	} else if !strings.Contains(err.Error(), "end in '/'") {
		t.Fatalf("empty route: unexpected err: %v", err)
	}
	// "apps" without a trailing slash would let "appsfoo/x" slip
	// through dispatch — the trailing-slash gate must reject it.
	if _, err := NewPrefixRouter(map[string]StorageBackend{
		"apps": be,
	}, nil); err == nil {
		t.Fatalf("missing trailing slash: nil err")
	} else if !strings.Contains(err.Error(), "end in '/'") {
		t.Fatalf("missing trailing slash: unexpected err: %v", err)
	}
	// Traversal is rejected by the validateKey path (ErrInvalidKey).
	if _, err := NewPrefixRouter(map[string]StorageBackend{
		"../escape/": be,
	}, nil); !IsInvalidKey(err) {
		t.Fatalf("traversal route: IsInvalidKey=false, err=%v", err)
	}
	if _, err := NewPrefixRouter(map[string]StorageBackend{
		"apps/": nil,
	}, nil); err == nil {
		t.Fatalf("nil backend: nil err")
	}
}

// TestPrefixRouterRoundtripBytes is the byte-equality round-trip:
// write a multi-KiB payload through the router, read it back
// through the router, byte-for-byte equality. This is the test that
// proves the dispatch wrappers do not corrupt io.Copy semantics.
func TestPrefixRouterRoundtripBytes(t *testing.T) {
	a := newTestBackend(t)
	f := newTestBackend(t)
	router, err := NewPrefixRouter(map[string]StorageBackend{
		"apps/": a,
	}, f)
	if err != nil {
		t.Fatalf("NewPrefixRouter: %v", err)
	}
	ctx := context.Background()
	want := bytes.Repeat([]byte{0xfa, 0xce}, 4096)
	if err := router.Put(ctx, "apps/slug/dep.ext4", bytes.NewReader(want)); err != nil {
		t.Fatalf("put: %v", err)
	}
	rc, err := router.Get(ctx, "apps/slug/dep.ext4")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read all: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("byte mismatch: got %d bytes, want %d bytes", len(got), len(want))
	}
}

// TestPrefixRouterGetMissing covers the cold-boot path: a missing
// key routed through a router must surface as IsNotFound so
// downstream callers can branch to fallback (ADR-005).
func TestPrefixRouterGetMissing(t *testing.T) {
	a := newTestBackend(t)
	router, err := NewPrefixRouter(map[string]StorageBackend{
		"apps/": a,
	}, newTestBackend(t))
	if err != nil {
		t.Fatalf("NewPrefixRouter: %v", err)
	}
	_, err = router.Get(context.Background(), "apps/missing.ext4")
	if !IsNotFound(err) {
		t.Fatalf("get missing: IsNotFound=false, err=%v", err)
	}
	// Legacy single-box idiom must keep working.
	if !errors.Is(err, error(nil)) && !strings.Contains(err.Error(), "storage:") {
		t.Fatalf("get missing: missing storage tag in %v", err)
	}
}

// TestPrefixRouterMidSegmentEscape is the regression for the prefix-
// boundary escape the PR review flagged: a route lacking a trailing
// "/" must NOT match "appsfoo/x" and route its remainder ("foo/x")
// into the apps backend. NewPrefixRouter already rejects routes
// without a trailing "/" at construction time, so the test
// constructs the PrefixRouter directly to exercise the dispatcher's
// defense-in-depth check.
func TestPrefixRouterMidSegmentEscape(t *testing.T) {
	a := newTestBackend(t)
	fb := newTestBackend(t)
	// Construct directly to bypass the constructor's trailing-slash
	// gate — we're testing the dispatcher's defense-in-depth, not the
	// constructor (which has its own test in TestPrefixRouterRejectsBadRoute).
	router := &PrefixRouter{
		routes:   map[string]StorageBackend{"apps": a},
		fallback: fb,
	}
	// "appsfoo/x" must not route into the apps backend: dispatch must
	// fall through to the fallback. A successful Put to the fallback
	// (which has no appsfoo subdir) is fine — the key assertion is
	// that the apps backend did NOT receive a mid-segment write with
	// "foo/x" as the remainder.
	err := router.Put(context.Background(), "appsfoo/x", strings.NewReader("nope"))
	if err != nil {
		t.Fatalf("put to fallback: %v", err)
	}
	if _, err := a.Get(context.Background(), "foo/x"); err == nil {
		t.Fatalf("apps backend received a mid-segment write — escape!")
	}
	// dispatch-level check: confirm the apps backend is NOT selected.
	b, rem, err := router.dispatch("appsfoo/x")
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if b == a {
		t.Fatalf("dispatch selected apps backend for appsfoo/x; remainder=%q", rem)
	}
}

// equalStrings is a small helper used by the list tests to compare
// two slices without importing reflect or cmp. Order-insensitive
// comparison would hide bugs; we want deterministic ordering.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// fakeOCIBackend is the stub OCI backend for the multi-prefix
// router test. It's a full StorageBackend so it satisfies the
// router's signature, and LocalArtifactLister so the router can
// aggregate its keys into the router-level List. The tracker
// counters let the test assert which backend received which key.
type fakeOCIBackend struct {
	blobs   map[string][]byte
	puts    int
	gets    int
	deletes int
}

func newFakeOCI() *fakeOCIBackend {
	return &fakeOCIBackend{blobs: map[string][]byte{}}
}

func (f *fakeOCIBackend) Put(_ context.Context, key string, r io.Reader) error {
	f.puts++
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.blobs[key] = data
	return nil
}

func (f *fakeOCIBackend) Get(_ context.Context, key string) (io.ReadCloser, error) {
	f.gets++
	data, ok := f.blobs[key]
	if !ok {
		return nil, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (f *fakeOCIBackend) Delete(_ context.Context, key string) error {
	f.deletes++
	delete(f.blobs, key)
	return nil
}

func (f *fakeOCIBackend) List(_ context.Context, prefix string) ([]string, error) {
	out := make([]string, 0, len(f.blobs))
	for k := range f.blobs {
		if prefix == "" || strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}

// TestPrefixRouterLocalPlusOCI pins the ADR-054 routing semantics
// end-to-end: a single PrefixRouter carries the canonical local
// routes (snap/, base/, kernel/, layers/) AND a sibling OCI route
// (apps/). Every Put/Get/Delete routes by prefix; List aggregates
// keys from both backends. The fake OCI backend tracks per-op
// counters so the test asserts which backend actually received
// each request.
func TestPrefixRouterLocalPlusOCI(t *testing.T) {
	snap := newTestBackend(t)
	base := newTestBackend(t)
	kernel := newTestBackend(t)
	layers := newTestBackend(t)
	oci := newFakeOCI()

	router, err := NewPrefixRouter(map[string]StorageBackend{
		"snap/":   snap,
		"base/":   base,
		"kernel/": kernel,
		"layers/": layers,
		"apps/":   oci,
	}, nil)
	if err != nil {
		t.Fatalf("NewPrefixRouter: %v", err)
	}
	ctx := context.Background()

	// --- Puts: each prefix lands in its declared backend ---
	puts := []struct {
		key, blob string
	}{
		{"snap/d-1/mem", "mem-data"},
		{"snap/d-1/vmstate", "vmstate-data"},
		{"base/base.ext4", "base-blob"},
		{"kernel/v1.0", "kernel-blob"},
		{"layers/l-1.ext4", "layer-blob"},
		{"apps/acme/d-1.ext4", "oci-blob"},
	}
	for _, p := range puts {
		if err := router.Put(ctx, p.key, strings.NewReader(p.blob)); err != nil {
			t.Fatalf("Put %q: %v", p.key, err)
		}
	}

	// OCI must have received exactly the apps/ key, nothing else.
	if oci.puts != 1 {
		t.Errorf("oci.puts = %d, want 1", oci.puts)
	}
	if got, err := readTestBackend(ctx, oci, "acme/d-1.ext4"); err != nil {
		t.Errorf("read OCI apps/acme/d-1.ext4: %v", err)
	} else if got != "oci-blob" {
		t.Errorf("OCI apps/acme/d-1.ext4 = %q, want %q", got, "oci-blob")
	}

	// Local backends each received exactly their own key. The
	// router strips the route prefix before forwarding to the
	// LocalStorageBackend, so we read using the stripped key
	// directly from the backend (which mirrors what the
	// underlying fs layout looks like).
	for _, want := range []struct{ prefix, strippedKey, blob string }{
		{"snap", "d-1/mem", "mem-data"},
		{"snap", "d-1/vmstate", "vmstate-data"},
		{"base", "base.ext4", "base-blob"},
		{"kernel", "v1.0", "kernel-blob"},
		{"layers", "l-1.ext4", "layer-blob"},
	} {
		got, err := readTestBackend(ctx, mustLister(t, want.prefix, snap, base, kernel, layers), want.strippedKey)
		if err != nil {
			t.Errorf("read %s %s: %v", want.prefix, want.strippedKey, err)
			continue
		}
		if got != want.blob {
			t.Errorf("%s %s = %q, want %q", want.prefix, want.strippedKey, got, want.blob)
		}
	}

	// --- Get: same routing for reads ---
	// Snapshot the counter after the List call above so we can
	// assert the delta from a single Get. List walks every backend
	// to aggregate keys, so it's expected to have bumped the
	// per-backend counters; we only care about the Get path here.
	beforeGets := oci.gets
	got, err := router.Get(ctx, "apps/acme/d-1.ext4")
	if err != nil {
		t.Fatalf("Get apps/acme/d-1.ext4: %v", err)
	}
	if delta := oci.gets - beforeGets; delta != 1 {
		t.Errorf("after Get: oci.gets delta = %d, want 1", delta)
	}
	data, err := io.ReadAll(got)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(data) != "oci-blob" {
		t.Errorf("apps/acme/d-1.ext4 = %q, want %q", string(data), "oci-blob")
	}
	_ = got.Close()

	// --- List: aggregates local + OCI keys ---
	keys, err := router.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	sort.Strings(keys)
	want := []string{
		"apps/acme/d-1.ext4",
		"base/base.ext4",
		"kernel/v1.0",
		"layers/l-1.ext4",
		"snap/d-1/mem",
		"snap/d-1/vmstate",
	}
	if !equalStrings(keys, want) {
		t.Errorf("List = %v, want %v", keys, want)
	}

	// --- Delete: propagates to the right backend ---
	if err := router.Delete(ctx, "apps/acme/d-1.ext4"); err != nil {
		t.Fatalf("Delete apps/...: %v", err)
	}
	if oci.deletes != 1 {
		t.Errorf("oci.deletes = %d, want 1", oci.deletes)
	}
	if _, err := router.Get(ctx, "apps/acme/d-1.ext4"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after Delete, Get = %v, want ErrNotFound", err)
	}
}

// readTestBackend pulls a blob out of the named test backend.
// Used by TestPrefixRouterLocalPlusOCI to verify per-backend
// round-trips without exposing internal counters.
func readTestBackend(ctx context.Context, be StorageBackend, key string) (string, error) {
	rc, err := be.Get(ctx, key)
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// mustLister picks the right LocalStorageBackend for a prefix
// assertion. Keeps TestPrefixRouterLocalPlusOCI's per-prefix
// round-trip loop readable.
func mustLister(t *testing.T, prefix string, snap, base, kernel, layers *LocalStorageBackend) StorageBackend {
	t.Helper()
	switch prefix {
	case "snap":
		return snap
	case "base":
		return base
	case "kernel":
		return kernel
	case "layers":
		return layers
	default:
		t.Fatalf("unknown prefix %q", prefix)
		return nil
	}
}
