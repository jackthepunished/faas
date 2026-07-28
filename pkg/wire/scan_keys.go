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
// base ext4 storage key (issue #299). Replaces the "base/" prefix
// with "scans/" and appends ".scan.json" — e.g.
// "base/runner-node22-amd64.ext4" maps to
// "scans/runner-node22-amd64.ext4.scan.json". This is the
// inverse-of-mapping shape vmmd uses at boot time to look up the
// scan sidecar given a wake request's BaseKey; imaged calls the
// same function at stage time to write the sidecar in lock-step
// with the digest sidecar. Uses strings.Replace with n=1 so only
// the first "base/" prefix is rewritten (defensive — a baseKey
// string with two "base/" prefixes is malformed input that we
// silently pass through unchanged).
func ScanKeyForBaseKey(baseKey string) string {
	return strings.Replace(baseKey, "base/", "scans/", 1) + ".scan.json"
}
