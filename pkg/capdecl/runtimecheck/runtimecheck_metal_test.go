//go:build metal

// runtimecheck_metal_test.go is the build-tagged live test that
// exercises Check() against /proc/self/status on the EX44
// (production) or Lima nested-VM (local M3+ Mac) runners.
//
// Review finding M4: this file used to be `runtimecheck_linux_test.go`
// with a `//go:build linux` tag — but `make test` (which runs on
// every CI machine including macOS dev boxes) does not pass
// `-tags=linux`. The old shape compiled on macOS, then tried to
// open /proc/self/status at runtime, then silently returned a
// zero mask (M3) so the test "passed" with no real introspection.
// Pinning `//go:build metal` aligns the file with the metal-only
// contract (memory: pkg/capdecl/runtimecheck_metal_test.md
// invariant) and the `//go:build metal` block in pkg/fcvm/leakcheck
// that has run-time-similar semantics. The non-metal validation
// logic lives in runtimecheck_test.go (fixture-driven, no
// /proc/self/status touch).
//
// The tests are intentionally narrow: each one constructs a
// minimal Declaration that the test process (running under
// `go test -tags=metal`) can or cannot satisfy, and asserts
// the matching pass / fail. On macOS dev the build tag is
// absent and the file does not compile — the macOS-friendly
// runtimecheck_test.go covers the validation logic.
package runtimecheck

import (
	"testing"

	"github.com/onebox-faas/faas/pkg/capdecl"
)

// TestCheck_LiveProc is the metal-build-time smoke: the test
// process runs under `go test -tags=metal` and Check(opts)
// reads the real /proc/self/status. With an empty declaration
// (no Allow, no Deny) every process should pass — the empty
// declaration is the canonical "unprivileged daemon".
func TestCheck_LiveProc(t *testing.T) {
	opts := Options{PID: 0} // 0 == /proc/self/status
	decl := capdecl.Declaration{}
	if err := Check(decl, opts); err != nil {
		t.Fatalf("Check(empty decl) on live proc = %v, want nil", err)
	}
}

// TestCheck_LiveProc_MatchesAllow constructs an Allow declaration
// from a cap the runner's Bnd already contains. The test passes
// for any runner whose Bnd is non-empty. If the runner's capBnd
// is empty (containers without caps) the test is skipped — the
// cap set varies wildly across CI runners.
func TestCheck_LiveProc_MatchesAllow(t *testing.T) {
	opts := Options{PID: 0}
	mask, err := readSelfStatus()
	if err != nil {
		t.Skipf("readSelfStatus: %v (M3 fail-closed; treat as runner cap absence)", err)
	}
	if mask.Bnd == 0 {
		t.Skip("live /proc/self/status returned empty capBnd; runner does not expose caps")
	}
	decl := allowOnlyFromBnd(mask.Bnd)
	if err := Check(decl, opts); err != nil {
		t.Fatalf("Check(allow-matching-Bnd) on live proc = %v, want nil", err)
	}
}

// TestCheck_LiveProc_DenyPresent declares cap_sys_admin as Deny
// and asserts the check returns a violation IF cap_sys_admin is
// actually in capBnd. If the runner's test process doesn't have
// cap_sys_admin in Bnd, the test is skipped (the contract is
// "deny cap not present" — which is the success case).
func TestCheck_LiveProc_DenyPresent(t *testing.T) {
	opts := Options{PID: 0}
	mask, err := readSelfStatus()
	if err != nil {
		t.Skipf("readSelfStatus: %v (M3 fail-closed; treat as runner cap absence)", err)
	}
	const bitSysAdmin = uint64(1) << 21
	if mask.Bnd&bitSysAdmin == 0 {
		t.Skip("live /proc/self/status does not include cap_sys_admin in capBnd; can't smoke-deny")
	}
	decl := capdecl.Declaration{Deny: []string{"cap_sys_admin"}}
	if err := Check(decl, opts); err == nil {
		t.Fatalf("Check(deny cap_sys_admin present in Bnd) = nil, want violation")
	}
}

// allowOnlyFromBnd returns a Declaration whose Allow list is
// the lowest set cap in bnd, decoded back to its canonical
// name. If no bit decodes to a known cap (e.g. an unknown
// bit set by a future kernel cap) the function returns the
// empty declaration — every process trivially passes the
// empty declaration.
func allowOnlyFromBnd(bnd uint64) capdecl.Declaration {
	for i := uint64(0); i < 64; i++ {
		if bnd&(uint64(1)<<i) == 0 {
			continue
		}
		if name, ok := capdecl.Encode(i); ok {
			return capdecl.Declaration{Allow: []string{name}}
		}
	}
	return capdecl.Declaration{}
}
