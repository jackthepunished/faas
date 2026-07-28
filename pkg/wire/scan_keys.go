package wire

import (
	goruntime "runtime"
	"strings"
)

// scan_keys.go — Grype scan sidecar storage key helpers (issue #299).
//
// These helpers live in pkg/wire (not pkg/sched where the digest
// sidecar helpers live) because pkg/fcvm imports pkg/wire but cannot
// import pkg/sched — pkg/sched already imports pkg/fcvm via
// pkg/sched/vmmclient.go, and Go forbids the import cycle. pkg/wire
// is the lowest layer that both pkg/fcvm and pkg/sched can import,
// making it the right home for a string-rewrite helper that both
// sides need at the boundary (imaged writes the scan sidecar at
// stage time, vmmd reads it at boot time).

// BaseScanKey returns the storage key for a runtime's Grype scan
// sidecar (issue #299). The sidecar records per-severity Grype
// finding counts for the staged base ext4 and is what vmmd reads at
// boot to decide whether to admit the instance. The key is per-arch
// (same precedent as sched.BaseDigestKey). Thin wrapper over
// BaseScanKeyForArch that pins the arch to runtime.GOARCH for the
// single-box case (this daemon's host arch).
func BaseScanKey(runtime string) string {
	return BaseScanKeyForArch(runtime, goruntime.GOARCH)
}

// BaseScanKeyForArch mirrors sched.BaseDigestKeyForArch for the
// Grype scan sidecar. Same per-arch partition so an amd64/arm64
// install rotation does not nuke the partner arch's sidecar. The
// shape mirrors the digest sidecar line-for-line so the two
// sidecars can be reasoned about symmetrically.
func BaseScanKeyForArch(runtime, arch string) string {
	if runtime == "" {
		return "scans/base-" + arch + ".ext4.scan.json"
	}
	return "scans/runner-" + runtime + "-" + arch + ".ext4.scan.json"
}

// ScanKeyForBaseKey derives the scan sidecar storage key from a
// base ext4 storage key (issue #299). The first path segment is
// rewritten to "scans/" via TrimPrefix+prepend, not strings.Replace —
// Replace with n=1 only catches the literal "base/" prefix and
// silently passes through "bases/" / "BASE/" / mixed-case through
// unchanged, so the scan sidecar ends up at a key vmmd never
// reads. The TrimPrefix+prepend shape is case-sensitive against the
// canonical "base/" prefix and handles non-root "bases/legacy/..."
// keys (a future imaged refactor that moves the base prefix into a
// sub-directory) by stripping the leading "base" segment and
// rewriting the first component. Appends ".scan.json" so the
// mapping is unambiguous.
//
// Inverse-of-mapping shape: vmmd uses this at boot time to look up
// the scan sidecar given a wake request's BaseKey; imaged uses the
// same function at stage time to write the sidecar in lock-step
// with the digest sidecar. Pinned by TestScanKeyForBaseKeyFormat in
// pkg/fcvm/manager_scan_test.go.
func ScanKeyForBaseKey(baseKey string) string {
	const (
		basePrefix = "base/"
		scanPrefix = "scans/"
		scanSuffix = ".scan.json"
	)
	if baseKey == "" {
		return scanPrefix + scanSuffix
	}
	if strings.HasPrefix(baseKey, basePrefix) {
		return scanPrefix + strings.TrimPrefix(baseKey, basePrefix) + scanSuffix
	}
	// Fallback for keys that don't carry the canonical "base/"
	// prefix (legacy / operator-overridden paths). The gate's
	// get-miss path surfaces this as *api.Problem{Code: scan_critical}
	// anyway, so a malformed key is fail-closed rather than silently
	// admitted under a different prefix. The trim-and-prepend keeps
	// the function pure (no panic on bad input).
	return scanPrefix + strings.TrimPrefix(baseKey, "/") + scanSuffix
}
