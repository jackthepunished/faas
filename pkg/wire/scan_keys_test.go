// scan_keys_test.go — fill pkg/wire coverage of the Grype scan
// sidecar storage key helpers (issue #299). The three functions in
// scan_keys.go are all at 0% on the baseline because the only
// existing caller (pkg/fcvm/manager.go) gates the call behind a
// build tag we don't exercise in unit tests. The keys are part of
// the vmmd ↔ imaged cross-process contract (vmmd reads at boot,
// imaged writes at stage time) so the format is load-bearing —
// any future drift would surface as a silent scan-sidecar miss at
// wake, not as a test failure.
//
// Targets:
//   - BaseScanKey: passes the runtime + runtime.GOARCH through to
//     BaseScanKeyForArch
//   - BaseScanKeyForArch: empty runtime falls back to the base
//     scan shape; populated runtime prefixes runner- and appends
//     arch + suffix
//   - ScanKeyForBaseKey: empty input → canonical "scans/.scan.json"
//     (fail-closed); canonical "base/..." prefix → rewrite to
//     "scans/..."; non-canonical prefix → still emit under
//     scans/... via the fallback rewrite

package wire_test

import (
	"strings"
	"testing"

	goruntime "runtime"

	"github.com/onebox-faas/faas/pkg/wire"
)

// --- BaseScanKey ---------------------------------------------------

// BaseScanKey is a thin wrapper over BaseScanKeyForArch that pins
// the arch to runtime.GOARCH (the single-box host arch). Assert
// the wrapper passes the runtime through and embeds runtime.GOARCH
// in the resulting key.
func TestBaseScanKey_EmbedsHostArch(t *testing.T) {
	got := wire.BaseScanKey("node22")
	if !strings.Contains(got, goruntime.GOARCH) {
		t.Errorf("BaseScanKey(%q) = %q; missing host arch %q", "node22", got, goruntime.GOARCH)
	}
	if !strings.HasPrefix(got, "scans/runner-node22-") {
		t.Errorf("BaseScanKey(%q) = %q; want scans/runner-node22-…", "node22", got)
	}
	if !strings.HasSuffix(got, ".scan.json") {
		t.Errorf("BaseScanKey(%q) = %q; missing .scan.json suffix", "node22", got)
	}
}

// --- BaseScanKeyForArch --------------------------------------------

// Empty runtime routes to the BASE scan key shape (per-runtime
// suffix is dropped; the partition is by arch only). This is the
// path imaged takes when staging the bare base ext4 (no runtime
// layer applied).
func TestBaseScanKeyForArch_EmptyRuntimeIsBaseShape(t *testing.T) {
	got := wire.BaseScanKeyForArch("", "amd64")
	want := "scans/base-amd64.ext4.scan.json"
	if got != want {
		t.Errorf("BaseScanKeyForArch(\"\", amd64) = %q, want %q", got, want)
	}
}

// Populated runtime emits the per-runner scan key shape.
func TestBaseScanKeyForArch_PopulatedRuntime(t *testing.T) {
	got := wire.BaseScanKeyForArch("python312", "arm64")
	want := "scans/runner-python312-arm64.ext4.scan.json"
	if got != want {
		t.Errorf("BaseScanKeyForArch(python312, arm64) = %q, want %q", got, want)
	}
}

// Per-arch partition is preserved on the per-runner path: the
// same runtime on amd64 and arm64 produces different keys. A
// regression that dropped the arch would merge the two sidecars
// on an amd64/arm64 install rotation (same precedent as
// sched.BaseDigestKeyForArch).
func TestBaseScanKeyForArch_ArchPartition(t *testing.T) {
	a := wire.BaseScanKeyForArch("node22", "amd64")
	b := wire.BaseScanKeyForArch("node22", "arm64")
	if a == b {
		t.Errorf("arch partition broken: amd64 and arm64 produced identical keys %q", a)
	}
}

// --- ScanKeyForBaseKey ---------------------------------------------

// Empty baseKey is the fail-closed shape documented at scan_keys.go:66-67.
// The gate's get-miss path surfaces a *api.Problem{Code: scan_critical}
// if the read comes back empty, so the helper returns the
// canonical "scans/.scan.json" rather than an empty string.
func TestScanKeyForBaseKey_EmptyBaseKey(t *testing.T) {
	got := wire.ScanKeyForBaseKey("")
	want := "scans/.scan.json"
	if got != want {
		t.Errorf("ScanKeyForBaseKey(\"\") = %q, want %q", got, want)
	}
}

// Canonical "base/..." prefix rewrites to "scans/..." + suffix.
// This is the imaged→vmmd lock-step path documented at
// scan_keys.go:43-59.
func TestScanKeyForBaseKey_CanonicalBasePrefix(t *testing.T) {
	got := wire.ScanKeyForBaseKey("base/node22-amd64.ext4")
	want := "scans/node22-amd64.ext4.scan.json"
	if got != want {
		t.Errorf("ScanKeyForBaseKey(base/node22-amd64.ext4) = %q, want %q", got, want)
	}
}

// Non-canonical prefix (legacy / operator-overridden path) still
// emits under "scans/" via the fallback rewrite at scan_keys.go:78.
// The leading "/" is trimmed so "extra/base/foo" becomes
// "scans/extra/base/foo.scan.json" — the gate's get-miss path
// fails closed regardless.
func TestScanKeyForBaseKey_NonCanonicalPrefix(t *testing.T) {
	got := wire.ScanKeyForBaseKey("legacy/node22.ext4")
	want := "scans/legacy/node22.ext4.scan.json"
	if got != want {
		t.Errorf("ScanKeyForBaseKey(legacy/node22.ext4) = %q, want %q", got, want)
	}
}

// A key with a leading "/" is trimmed before the scans/ prepend —
// the helper is path-safe against operator-overridden keys that
// start with the storage root prefix.
func TestScanKeyForBaseKey_LeadingSlashTrimmed(t *testing.T) {
	got := wire.ScanKeyForBaseKey("/legacy/node22.ext4")
	want := "scans/legacy/node22.ext4.scan.json"
	if got != want {
		t.Errorf("ScanKeyForBaseKey(/legacy/...) = %q, want %q", got, want)
	}
}

// Round-trip sanity: BaseScanKeyForArch → ScanKeyForBaseKey is not
// 1:1 (the former uses "scans/runner-..." + arch, the latter
// rewrites "base/..." + suffix), but ScanKeyForBaseKey's output
// always starts with "scans/" and ends with ".scan.json".
func TestScanKeyForBaseKey_AlwaysHasScansPrefixAndScanSuffix(t *testing.T) {
	cases := []string{
		"",
		"base/node22-amd64.ext4",
		"legacy/node22.ext4",
		"/root/path/file.ext4",
	}
	for _, base := range cases {
		got := wire.ScanKeyForBaseKey(base)
		if !strings.HasPrefix(got, "scans/") {
			t.Errorf("ScanKeyForBaseKey(%q) = %q; missing scans/ prefix", base, got)
		}
		if !strings.HasSuffix(got, ".scan.json") {
			t.Errorf("ScanKeyForBaseKey(%q) = %q; missing .scan.json suffix", base, got)
		}
	}
}
