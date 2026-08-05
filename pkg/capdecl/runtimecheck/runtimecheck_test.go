// runtimecheck_test.go is the unit test surface for the
// runtimecheck package. It exercises Validate() with
// fixture-driven masks — the macOS-friendly half of the gate.
// The Check() function (live /proc/self/status) is exercised
// by the metal-build-tagged test in
// runtimecheck_linux_test.go so the same package compiles on
// darwin during `make test` and runs the real check on the
// EX44 / Lima VM in `make test-metal`.
package runtimecheck

import (
	"errors"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/capdecl"
)

// TestValidate_AllCapsPresent covers the green path: every
// Allow-listed cap is present in Bnd, no Deny-listed cap is
// in Bnd. Validate must return nil.
func TestValidate_AllCapsPresent(t *testing.T) {
	t.Parallel()

	const (
		bitKill  = uint64(1) << 5  // cap_kill
		bitAdmin = uint64(1) << 21 // cap_sys_admin
	)

	mask := capdecl.CapMasks{Bnd: bitKill | bitAdmin}
	decl := capdecl.Declaration{
		Allow: []string{"cap_kill", "cap_sys_admin"},
	}
	if err := Validate(decl, mask); err != nil {
		t.Fatalf("Validate(matching) = %v, want nil", err)
	}
}

// TestValidate_AllowMissing covers the failure path: an
// Allow-listed cap is missing from Bnd. Validate returns
// a *Violation with Kind=ViolationAllowMissing.
func TestValidate_AllowMissing(t *testing.T) {
	t.Parallel()

	bitAdmin := uint64(1) << 21 // cap_sys_admin

	mask := capdecl.CapMasks{Bnd: bitAdmin}
	decl := capdecl.Declaration{
		Allow: []string{"cap_sys_admin", "cap_kill"},
	}
	err := Validate(decl, mask)
	if err == nil {
		t.Fatalf("Validate(missing cap_kill) = nil, want violation")
	}
	var v *Violation
	if !errors.As(err, &v) {
		t.Fatalf("Validate: error is %T (%v), want *Violation", err, err)
	}
	if v.Kind != ViolationAllowMissing {
		t.Fatalf("Validate: Violation.Kind = %d, want ViolationAllowMissing (%d)", v.Kind, ViolationAllowMissing)
	}
	if !strings.Contains(v.Error(), "cap_kill") {
		t.Fatalf("Violation.Error() = %q, want to mention cap_kill", v.Error())
	}
}

// TestValidate_DenyPresent covers the second failure path:
// a Deny-listed cap IS present in Bnd. Validate returns a
// *Violation with Kind=ViolationDenyPresent.
func TestValidate_DenyPresent(t *testing.T) {
	t.Parallel()

	const (
		bitKill  = uint64(1) << 5  // cap_kill
		bitAdmin = uint64(1) << 21 // cap_sys_admin
	)

	// Bnd has BOTH cap_kill (allowed) AND cap_sys_admin
	// (denied). The check should pass the Allow step (kill
	// is in Bnd) and then fail the Deny step (admin is in
	// Bnd too).
	mask := capdecl.CapMasks{Bnd: bitKill | bitAdmin}
	decl := capdecl.Declaration{
		Allow: []string{"cap_kill"},
		Deny:  []string{"cap_sys_admin"},
	}
	err := Validate(decl, mask)
	if err == nil {
		t.Fatalf("Validate(deny-present) = nil, want violation")
	}
	var v *Violation
	if !errors.As(err, &v) {
		t.Fatalf("Validate: error is %T (%v), want *Violation", err, err)
	}
	if v.Kind != ViolationDenyPresent {
		t.Fatalf("Validate: Violation.Kind = %d, want ViolationDenyPresent (%d)", v.Kind, ViolationDenyPresent)
	}
	if !strings.Contains(v.Error(), "cap_sys_admin") {
		t.Fatalf("Violation.Error() = %q, want to mention cap_sys_admin", v.Error())
	}
}

// TestValidate_UnknownCap covers the third failure path: the
// declaration contains a cap name that's not in our Decode
// table. Validate surfaces the unknown name in the violation.
func TestValidate_UnknownCap(t *testing.T) {
	t.Parallel()

	mask := capdecl.CapMasks{Bnd: 0}
	decl := capdecl.Declaration{
		Allow: []string{"cap_no_such_thing"},
	}
	err := Validate(decl, mask)
	if err == nil {
		t.Fatalf("Validate(unknown) = nil, want violation")
	}
	var v *Violation
	if !errors.As(err, &v) {
		t.Fatalf("Validate: error is %T, want *Violation", err)
	}
	if !strings.Contains(v.Error(), "cap_no_such_thing") {
		t.Fatalf("Violation.Error() = %q, want unknown cap name", v.Error())
	}
}

// TestValidate_InvalidDeclaration covers the structural
// validation re-emitted by runtimecheck: an empty cap name
// or an overlap between Allow and Deny should fail before
// the mask check even runs.
func TestValidate_InvalidDeclaration(t *testing.T) {
	t.Parallel()

	mask := capdecl.CapMasks{Bnd: 0}
	tests := []struct {
		name string
		decl capdecl.Declaration
	}{
		{
			name: "empty_allow",
			decl: capdecl.Declaration{Allow: []string{""}},
		},
		{
			name: "overlap",
			decl: capdecl.Declaration{
				Allow: []string{"cap_sys_admin"},
				Deny:  []string{"cap_sys_admin"},
			},
		},
		{
			name: "duplicate_allow",
			decl: capdecl.Declaration{Allow: []string{"cap_kill", "cap_kill"}},
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := Validate(tc.decl, mask)
			if err == nil {
				t.Fatalf("Validate(invalid %s) = nil, want error", tc.name)
			}
			// The wrapping chain is "runtimecheck: declaration
			// invalid: capdecl: <reason>". Both prefixes should
			// appear so an ops engineer can grep either.
			if !strings.Contains(err.Error(), "declaration invalid") {
				t.Fatalf("Validate(invalid %s): err = %v, want substring %q", tc.name, err, "declaration invalid")
			}
		})
	}
}

// TestCheck_StatusReader covers the io.Reader code path of
// Check(): the test feeds a fixture /proc/<pid>/status body
// into opts.StatusReader. The check must match what ParseStatus
// yields.
func TestCheck_StatusReader(t *testing.T) {
	t.Parallel()

	bitKill := uint64(1) << 5   // cap_kill
	bitAdmin := uint64(1) << 21 // cap_sys_admin

	fixture := ComposeFixture(0, bitKill|bitAdmin, bitKill|bitAdmin, bitKill|bitAdmin, 0)
	decl := capdecl.Declaration{
		Allow: []string{"cap_kill", "cap_sys_admin"},
	}
	opts := Options{StatusReader: strings.NewReader(string(fixture))}
	if err := Check(decl, opts); err != nil {
		t.Fatalf("Check(fixture-ok) = %v, want nil", err)
	}

	// Now flip the fixture to a state where cap_kill is missing.
	fixtureMissing := ComposeFixture(0, bitAdmin, bitAdmin, bitAdmin, 0)
	declAllowKill := capdecl.Declaration{
		Allow: []string{"cap_kill", "cap_sys_admin"},
	}
	optsMissing := Options{StatusReader: strings.NewReader(string(fixtureMissing))}
	if err := Check(declAllowKill, optsMissing); err == nil {
		t.Fatalf("Check(fixture-missing) = nil, want violation")
	}
}

// TestComposeFixture_RoundTripsWithParseStatus: the fixture
// helper produces bytes that ParseStatus reads back. This
// pins the test scaffolding to the production parser.
func TestComposeFixture_RoundTripsWithParseStatus(t *testing.T) {
	t.Parallel()

	bitKill := uint64(1) << 5   // cap_kill
	bitAdmin := uint64(1) << 21 // cap_sys_admin
	bitRawio := uint64(1) << 17 // cap_sys_rawio

	fixture := ComposeFixture(0, bitKill|bitAdmin, bitKill|bitAdmin, bitKill|bitAdmin|bitRawio, 0)
	got := capdecl.ParseStatus(fixture)
	if got.Prm != bitKill|bitAdmin {
		t.Fatalf("ParseStatus: Prm = 0x%x, want 0x%x", got.Prm, bitKill|bitAdmin)
	}
	if got.Eff != bitKill|bitAdmin {
		t.Fatalf("ParseStatus: Eff = 0x%x, want 0x%x", got.Eff, bitKill|bitAdmin)
	}
	if got.Bnd != bitKill|bitAdmin|bitRawio {
		t.Fatalf("ParseStatus: Bnd = 0x%x, want 0x%x", got.Bnd, bitKill|bitAdmin|bitRawio)
	}
}
