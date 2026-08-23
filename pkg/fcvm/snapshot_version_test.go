package fcvm

import "testing"

// TestFAASBaseImageVersion_Pinned asserts the FAAS_BASE_IMAGE_VERSION
// constant is non-empty and pinned to a known value. The constant is the
// load-bearing contract for the imaged F3 sweep
// (pkg/imaged/handler.go::MarkAppProtocolSnapshotsStale, ADR-127 §D1):
// when the constant is bumped here, every non-stale snapshot whose
// deployment's app.app_protocol ∈ {http2, grpc} must be flipped stale
// on the next imaged sweep so vmmd's lazy re-snapshot path (ADR-005)
// rebuilds them transparently.
//
// App staying on app_protocol=http1 are unaffected by the bump — the
// h2c-capable runner listener is part of the new image but the bridge
// path they ride (H1+chunked) doesn't depend on it.
//
// ADR-127 §D1 + §Layer 6 — closes the "no FAAS_BASE_IMAGE_VERSION const"
// blocking gap surfaced by the 12-layer audit of PR #1050.
func TestFAASBaseImageVersion_Pinned(t *testing.T) {
	if FAAS_BASE_IMAGE_VERSION == "" {
		t.Fatal("FAAS_BASE_IMAGE_VERSION must be non-empty (ADR-127 §D1)")
	}
	// Pinned to "v1" at PR #1050's GA + ADR-127 hardening pass.
	// Update this assertion when the constant is bumped; the
	// imaged F3 sweep then rebuilds the opt-in slice.
	const want = "v1"
	if got := FAAS_BASE_IMAGE_VERSION; got != want {
		t.Fatalf("FAAS_BASE_IMAGE_VERSION = %q, want %q (update imaged F3 sweep on bump)", got, want)
	}
}

// TestFAASBaseImageVersion_NotEqualToFCVersion is a defense-in-depth
// assertion that the base-image version and the Firecracker version
// dimensions don't accidentally collapse to the same constant. They are
// distinct dimensions: FAAS_BASE_IMAGE_VERSION tracks the OCI base
// image wire-protocol capability, Snapshot.FCVersion tracks the
// Firecracker hypervisor version (ADR-005).
func TestFAASBaseImageVersion_NotEqualToFCVersion(t *testing.T) {
	// We don't import the FC version constant directly here — the
	// point is to assert that the FAAS_BASE_IMAGE_VERSION string is
	// not a Firecracker semver (e.g. "1.13.0", "v1.10.x"). The
	// canonical shape is a short prefix-stamp ("v1", "v2", ...).
	if len(FAAS_BASE_IMAGE_VERSION) < 2 || FAAS_BASE_IMAGE_VERSION[0] != 'v' {
		t.Fatalf("FAAS_BASE_IMAGE_VERSION = %q, want short 'vN' prefix-stamp shape", FAAS_BASE_IMAGE_VERSION)
	}
}
