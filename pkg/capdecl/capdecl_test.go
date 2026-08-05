package capdecl

import (
	"strings"
	"testing"
)

// TestDeclaration_Validate covers the structural validation
// (empty / duplicate / overlap) in capdecl.go. The runtime
// kernel-cap-set validation lives in runtimecheck_test.go.
func TestDeclaration_Validate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		allow   []string
		deny    []string
		wantErr string // substring; "" means nil err
	}{
		{
			name:  "ok_empty",
			allow: nil,
			deny:  nil,
		},
		{
			name:  "ok_simple",
			allow: []string{"cap_sys_admin"},
			deny:  []string{"cap_sys_module"},
		},
		{
			name:  "ok_multiple",
			allow: []string{"cap_sys_admin", "cap_net_admin", "cap_kill"},
			deny:  []string{"cap_sys_module", "cap_sys_rawio"},
		},
		{
			name:    "empty_allow_entry",
			allow:   []string{""},
			wantErr: "empty cap name in Allow",
		},
		{
			name:    "empty_deny_entry",
			deny:    []string{""},
			wantErr: "empty cap name in Deny",
		},
		{
			name:    "duplicate_allow",
			allow:   []string{"cap_sys_admin", "cap_sys_admin"},
			wantErr: "duplicate cap",
		},
		{
			name:    "duplicate_deny",
			deny:    []string{"cap_sys_module", "cap_sys_module"},
			wantErr: "duplicate cap",
		},
		{
			name:    "overlap_allow_deny",
			allow:   []string{"cap_sys_admin"},
			deny:    []string{"cap_sys_admin"},
			wantErr: "appears in both Allow and Deny",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := Declaration{Allow: tc.allow, Deny: tc.deny}
			err := d.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("Validate(%v,%v) unexpected error: %v", tc.allow, tc.deny, err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("Validate(%v,%v) expected error matching %q, got nil", tc.allow, tc.deny, tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("Validate(%v,%v) = %v, want substring %q", tc.allow, tc.deny, err, tc.wantErr)
			}
		})
	}
}

// TestDeclaration_Sorted canonicalises two equivalent declarations
// (same cap names, different order) to the same Sorted() form.
// This is what Equal() relies on.
func TestDeclaration_Sorted(t *testing.T) {
	t.Parallel()

	d := Declaration{
		Allow: []string{"cap_kill", "cap_sys_admin", "cap_net_admin"},
		Deny:  []string{"cap_sys_module", "cap_sys_rawio"},
	}
	got := d.Sorted()
	want := Declaration{
		Allow: []string{"cap_kill", "cap_net_admin", "cap_sys_admin"},
		Deny:  []string{"cap_sys_module", "cap_sys_rawio"},
	}
	if !got.Equal(want) {
		t.Fatalf("Sorted() = %s, want %s", got.String(), want.String())
	}
}

// TestDeclaration_Equal is order-insensitive.
func TestDeclaration_Equal(t *testing.T) {
	t.Parallel()

	a := Declaration{
		Allow: []string{"cap_sys_admin", "cap_kill"},
		Deny:  []string{"cap_sys_module"},
	}
	b := Declaration{
		Allow: []string{"cap_kill", "cap_sys_admin"},
		Deny:  []string{"cap_sys_module"},
	}
	c := Declaration{
		Allow: []string{"cap_sys_admin"},
		Deny:  []string{"cap_sys_module"},
	}

	if !a.Equal(b) {
		t.Fatalf("Equal(a,b) = false, want true (same caps, different order)")
	}
	if a.Equal(c) {
		t.Fatalf("Equal(a,c) = true, want false (different Allow length)")
	}
}

// TestDeclaration_String is the log-message contract — a stable,
// deterministic form. The exact format is locked here so log
// parsers and the runtimecheck violation messages line up.
func TestDeclaration_String(t *testing.T) {
	t.Parallel()

	d := Declaration{
		Allow: []string{"cap_sys_admin"},
		Deny:  nil,
	}
	got := d.String()
	want := "capdecl{allow:[cap_sys_admin]}"
	if got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}

	empty := Declaration{}
	if got := empty.String(); got != "capdecl{}" {
		t.Fatalf("String(empty) = %q, want %q", got, "capdecl{}")
	}

	denyOnly := Declaration{Deny: []string{"cap_sys_module"}}
	if got := denyOnly.String(); got != "capdecl{deny:[cap_sys_module]}" {
		t.Fatalf("String(denyOnly) = %q, want %q", got, "capdecl{deny:[cap_sys_module]}")
	}
}

// TestParseStatus_RoundTrip is the regex-shaped parser check.
// The format /proc/self/status uses is "CapBnd: <hex>"; we
// feed a fixture, parse it, and assert the four masks.
func TestParseStatus_RoundTrip(t *testing.T) {
	t.Parallel()

	const fixture = `Name:	fixture
Umask:	0022
State:	R (running)
CapInh:	0000000000000000
CapPrm:	00000000a80425fb
CapEff:	0000000000000000
CapBnd:	00000000a80425fb
CapAmb:	0000000000000000
Seccomp:	2
`

	got := ParseStatus([]byte(fixture))
	want := CapMasks{
		Inh: 0x0000000000000000,
		Prm: 0x00000000a80425fb,
		Eff: 0x0000000000000000,
		Bnd: 0x00000000a80425fb,
		Amb: 0x0000000000000000,
	}
	if got != want {
		t.Fatalf("ParseStatus = %+v, want %+v", got, want)
	}
}

// TestParseStatus_IgnoresUnrelatedLines: lines without "Cap<...>:"
// prefix (Uid, Groups, etc.) must not affect the mask.
func TestParseStatus_IgnoresUnrelatedLines(t *testing.T) {
	t.Parallel()

	const fixture = `Name:	fixture
CapBnd:	00000000deadbeef
Uid:	0	0	0	0
Groups:	0
CapPrm:	0000000000000001
CapEff:	0000000000000000
CapAmb:	0000000000000000
CapInh:	0000000000000000
`
	got := ParseStatus([]byte(fixture))
	if got.Bnd != 0x00000000deadbeef {
		t.Fatalf("ParseStatus: Bnd = 0x%x, want 0xdeadbeef", got.Bnd)
	}
	if got.Prm != 1 {
		t.Fatalf("ParseStatus: Prm = 0x%x, want 0x1", got.Prm)
	}
}

// TestParseStatus_Empty: nil or empty input must not panic.
func TestParseStatus_Empty(t *testing.T) {
	t.Parallel()

	got := ParseStatus(nil)
	if got != (CapMasks{}) {
		t.Fatalf("ParseStatus(nil) = %+v, want zero", got)
	}
	got = ParseStatus([]byte(""))
	if got != (CapMasks{}) {
		t.Fatalf("ParseStatus(\"\") = %+v, want zero", got)
	}
}

// TestCapMasks_Has covers the Allow-list check used by runtimecheck.
func TestCapMasks_Has(t *testing.T) {
	t.Parallel()

	const (
		bitKill   = uint64(1) << 5  // cap_kill
		bitAdmin  = uint64(1) << 21 // cap_sys_admin
		bitModule = uint64(1) << 16 // cap_sys_module
	)

	mask := CapMasks{Bnd: bitKill | bitAdmin | bitModule}
	if missing, unknown := mask.Has([]string{"cap_kill", "cap_sys_admin", "cap_sys_module"}, mask.Bnd); missing != "" || len(unknown) != 0 {
		t.Fatalf("Has(all_present) = (%q, %v), want (\"\", nil)", missing, unknown)
	}
	if missing, _ := mask.Has([]string{"cap_kill", "cap_audit_write"}, mask.Bnd); missing != "cap_audit_write" {
		t.Fatalf("Has(missing) = %q, want %q", missing, "cap_audit_write")
	}
	// unknown cap returns unknown[] only, no "missing" hit.
	if missing, unknown := mask.Has([]string{"cap_unknown_thing"}, mask.Bnd); missing != "" || len(unknown) != 1 || unknown[0] != "cap_unknown_thing" {
		t.Fatalf("Has(unknown) = (%q, %v), want (\"\", [cap_unknown_thing])", missing, unknown)
	}
}

// TestCapMasks_NotIn covers the Deny-list check used by runtimecheck.
func TestCapMasks_NotIn(t *testing.T) {
	t.Parallel()

	const (
		bitAdmin  = uint64(1) << 21 // cap_sys_admin
		bitModule = uint64(1) << 16 // cap_sys_module
	)

	mask := CapMasks{Bnd: bitAdmin}
	// cap_sys_module is NOT in Bnd → NotIn returns empty (we deny module, it's absent — success).
	if unexpected, unknown := mask.NotIn([]string{"cap_sys_module"}, mask.Bnd); len(unexpected) != 0 || len(unknown) != 0 {
		t.Fatalf("NotIn(cap_sys_module) = (%v, %v), want both empty", unexpected, unknown)
	}
	// cap_sys_admin IS in Bnd → NotIn returns it (we deny admin, but it's there — violation).
	if unexpected, _ := mask.NotIn([]string{"cap_sys_admin"}, mask.Bnd); len(unexpected) != 1 || unexpected[0] != "cap_sys_admin" {
		t.Fatalf("NotIn(cap_sys_admin) = %v, want [cap_sys_admin]", unexpected)
	}
}
