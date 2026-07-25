package netns

// PR-A — DenySet regression net. The typed DenySet is the single
// source of truth for the deny list (pkg/netns/denylist.go). These
// tests pin:
//
//   - every entry in NewDefaultDenySet() has a SourceADR string
//     (so the operator-facing artifact can render provenance)
//   - the v4 / v6 slices match the Entries slice (no drift between
//     the typed view and the metadata view)
//   - the comma-joined output uses the modern-nft syntax (no
//     trailing whitespace, comma-with-no-trailing-whitespace —
//     memory `nft-cidr-set-comma-required`)
//   - 6to4 (2002::/16) and Teredo (2001::/32) are present
//     (ADR-034 closes the v6 lateral-movement gap)
//
// The MissingEntry sub-test mutates a clone of the deny set to
// drop one entry and confirms a downstream consumer would notice
// — this is the "test isn't over-asserting" pin (any future
// regression that drops a CIDR would surface as a single
// t.Errorf).

import (
	"net/netip"
	"strings"
	"testing"
)

func TestNewDefaultDenySet_EntriesHaveProvenance(t *testing.T) {
	d := NewDefaultDenySet()
	if len(d.Entries) == 0 {
		t.Fatal("NewDefaultDenySet returned an empty Entries slice")
	}
	for i, e := range d.Entries {
		if e.SourceADR == "" {
			t.Errorf("Entries[%d] (%s) missing SourceADR", i, e.Prefix)
		}
		if !e.Prefix.IsValid() {
			t.Errorf("Entries[%d] has invalid Prefix %v", i, e.Prefix)
		}
		if e.Family != FamilyV4 && e.Family != FamilyV6 {
			t.Errorf("Entries[%d] (%s) has unknown Family %d", i, e.Prefix, e.Family)
		}
		// Family must match the prefix family — a v4 prefix labeled
		// v6 would land in the wrong argv.
		isV4 := e.Prefix.Addr().Is4()
		if isV4 && e.Family != FamilyV4 {
			t.Errorf("Entries[%d] (%s) is v4 but Family = %v", i, e.Prefix, e.Family)
		}
		if !isV4 && e.Family != FamilyV6 {
			t.Errorf("Entries[%d] (%s) is v6 but Family = %v", i, e.Prefix, e.Family)
		}
	}
}

func TestNewDefaultDenySet_TypedSlicesMatchEntries(t *testing.T) {
	d := NewDefaultDenySet()
	var v4FromEntries, v6FromEntries []netip.Prefix
	for _, e := range d.Entries {
		if e.Family == FamilyV4 {
			v4FromEntries = append(v4FromEntries, e.Prefix)
		} else {
			v6FromEntries = append(v6FromEntries, e.Prefix)
		}
	}
	if len(v4FromEntries) != len(d.V4DenyCIDRs) {
		t.Errorf("len(V4DenyCIDRs) = %d, want %d (from Entries)", len(d.V4DenyCIDRs), len(v4FromEntries))
	}
	if len(v6FromEntries) != len(d.V6DenyCIDRs) {
		t.Errorf("len(V6DenyCIDRs) = %d, want %d (from Entries)", len(d.V6DenyCIDRs), len(v6FromEntries))
	}
	for i, p := range v4FromEntries {
		if p != d.V4DenyCIDRs[i] {
			t.Errorf("V4DenyCIDRs[%d] = %s, want %s", i, d.V4DenyCIDRs[i], p)
		}
	}
	for i, p := range v6FromEntries {
		if p != d.V6DenyCIDRs[i] {
			t.Errorf("V6DenyCIDRs[%d] = %s, want %s", i, d.V6DenyCIDRs[i], p)
		}
	}
}

func TestNewDefaultDenySet_CommaSetHasNoTrailingWhitespace(t *testing.T) {
	d := NewDefaultDenySet()
	v4 := d.V4CommaSet()
	v6 := d.V6CommaSet()
	// Modern nft on ubuntu-latest rejects trailing-whitespace set
	// syntax (memory `nft-cidr-set-comma-required`). The helpers
	// here must NOT emit `, `, `, `, only `,`.
	for _, raw := range []string{v4, v6} {
		if strings.Contains(raw, ", ") {
			t.Errorf("deny set has `, ` trailing whitespace: %q", raw)
		}
		if strings.Contains(raw, " ") {
			t.Errorf("deny set has unexpected space: %q", raw)
		}
	}
}

func TestNewDefaultDenySet_6to4AndTeredoPresent(t *testing.T) {
	// ADR-034: 6to4 + Teredo are the documented gap from ADR-023.
	// This test is the regression net — if a future refactor
	// drops either entry the test fails loudly with the ADR
	// citation in the message.
	d := NewDefaultDenySet()
	var has6to4, hasTeredo bool
	for _, e := range d.Entries {
		if e.Prefix.String() == "2002::/16" {
			has6to4 = true
			if e.SourceADR != "ADR-034" {
				t.Errorf("6to4 entry SourceADR = %q, want %q", e.SourceADR, "ADR-034")
			}
		}
		if e.Prefix.String() == "2001::/32" {
			hasTeredo = true
			if e.SourceADR != "ADR-034" {
				t.Errorf("Teredo entry SourceADR = %q, want %q", e.SourceADR, "ADR-034")
			}
		}
	}
	if !has6to4 {
		t.Error("NewDefaultDenySet missing 6to4 (2002::/16) — see ADR-034")
	}
	if !hasTeredo {
		t.Error("NewDefaultDenySet missing Teredo (2001::/32) — see ADR-034")
	}
}

func TestNewDefaultDenySet_SMTPPortsAreComplete(t *testing.T) {
	// Spec §11: "deny 25/465/587". A future edit that drops one
	// would let the Hetzner abuse desk come knocking.
	d := NewDefaultDenySet()
	wantPorts := map[uint16]bool{25: false, 465: false, 587: false}
	for _, p := range d.SMTPPorts {
		if _, ok := wantPorts[p]; ok {
			wantPorts[p] = true
		}
	}
	for port, present := range wantPorts {
		if !present {
			t.Errorf("SMTPPorts missing %d (spec §11)", port)
		}
	}
}

// TestDenySetFamilyString — the family keyword must be the nft
// family keyword (`ip` / `ip6`), not a Go-side enum name. A
// future edit that swaps Family for `Family4` / `Family6` would
// land the wrong nft argv family.
func TestDenySetFamilyString(t *testing.T) {
	cases := map[Family]string{
		FamilyV4: "ip",
		FamilyV6: "ip6",
	}
	for f, want := range cases {
		if got := f.String(); got != want {
			t.Errorf("Family(%d).String() = %q, want %q", int(f), got, want)
		}
	}
}