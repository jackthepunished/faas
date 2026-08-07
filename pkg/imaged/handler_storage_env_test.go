// handler_storage_env_test.go — ADR-054 acceptance pin (imaged side).
//
// Pins that imaged's Handler accepts the production env-derived
// backend (storage.BackendFromEnv with FAAS_STORAGE_BACKEND=oci +
// FAAS_OCI_REGISTRY=<url>) without re-typing or falling through to
// the default LocalStorageBackend. The shape of the resulting
// backend (PrefixRouter with OCI fallback, default-on cache wrapper)
// is pinned in pkg/storage/env_test.go — this test is the imaged-side
// boundary that proves the env contract survives the WithStorage
// round-trip.
//
// Why narrow: the load-bearing test is the handoff (env → Handler),
// not the router's internal field shape. Internal field assertions
// would force this test into the *PrefixRouter fields which are
// unexported; the pins there are pkg/storage's responsibility. The
// test is named _Handoff rather than _Build because it does NOT
// drive the full build pipeline (no rootfs, no OCI manifest probe);
// it only asserts the env-derived backend survives the
// WithStorage / storageFor round-trip.

package imaged

import (
	"testing"

	"github.com/onebox-faas/faas/pkg/state"
	"github.com/onebox-faas/faas/pkg/storage"
)

// TestHandler_OCIBackendEnv_Handoff pins the env-var end-to-end
// handoff into imaged's Handler. The env vars resolve to a
// *PrefixRouter (cache explicitly disabled so the assertion is
// shape-stable); the test confirms:
//
//   - storage.BackendFromEnv succeeds with the OCI env vars.
//   - The BackendFromEnv result is a *PrefixRouter (the OCI fork
//     registers snap/, base/, kernel/, layers/ as local-prefix routes
//     and falls through to the OCI backend for apps/).
//   - imaged's Handler accepts the env-derived backend via
//     WithStorage and storageFor() returns it unchanged (no fall-
//     through to the legacy LocalStorageBackend).
//
// The cache wrapper assertions live in pkg/storage/env_test.go
// (TestBackendFromEnv_OCIDefaultsCacheDirHermetic and
// TestBackendFromEnv_OCIDoesNotWrapWhenExplicitlyDisabled). We
// disable the cache here to keep the type shape stable.
//
// Why this is named _Handoff rather than _Build: a true Build test
// would need a real rootfs (≥10 MB) and a live OCI registry to
// probe (the registry's HEAD /v2/ endpoint and the manifest schema
// are validated end-to-end). That's covered by the metal suite and
// the pkg/storage/oci_test.go integration tests, neither of which
// runs in `make test`. This test is the unit-side env→Handler
// wiring pin; renaming to _Handoff reflects that scope so a future
// reader doesn't grep for "Build" and assume coverage they don't
// have.
func TestHandler_OCIBackendEnv_Handoff(t *testing.T) {
	// Stable temp roots so the test never touches /srv/fc on the
	// dev machine. The cache is explicitly disabled — the
	// default-on path is pinned in pkg/storage/env_test.go.
	tmp := t.TempDir()
	t.Setenv("FAAS_STORAGE_BACKEND", "oci")
	t.Setenv("FAAS_OCI_REGISTRY", "http://127.0.0.1:0/fake")
	t.Setenv("FAAS_STORAGE_ROOT", tmp+"/fc")
	t.Setenv("FAAS_STORAGE_CACHE_DIR", "") // explicit disable for shape stability
	t.Setenv("FAAS_STORAGE_LOCAL_PREFIXES", "snap/,base/,kernel/,layers/")

	be, err := storage.BackendFromEnv()
	if err != nil {
		t.Fatalf("BackendFromEnv: %v", err)
	}

	// The OCI fork produces a *PrefixRouter (cache disabled). The
	// type-assert is the load-bearing env pin: the BackendFromEnv
	// contract must produce a router when in OCI mode.
	if _, ok := be.(*storage.PrefixRouter); !ok {
		t.Fatalf("BackendFromEnv returned %T; want *PrefixRouter (OCI mode, cache disabled)", be)
	}

	// Wire imaged's Handler with the env-derived backend. New()
	// takes a non-nil state store + Notifier; we use the existing
	// silent fakes from handler_storage_routing_test.go.
	h := New(state.NewMemStore(), &fakeNotifier{}, fakePuller{}, &fakeBuilder{},
		"./init", tmp+"/apps", silentLogger()).WithStorage(be)

	// The handler's storageFor() must return the wired backend
	// unchanged — no fall-through to the default LocalStorageBackend.
	fromHandler, err := h.storageFor()
	if err != nil {
		t.Fatalf("storageFor: %v", err)
	}
	if fromHandler != be {
		t.Errorf("storageFor() = %p, want %p (env-derived backend)", fromHandler, be)
	}
}
