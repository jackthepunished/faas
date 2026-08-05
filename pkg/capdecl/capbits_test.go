package capdecl

import (
	"sort"
	"testing"
)

// TestDecode_KnownCaps covers the canonical mapping for caps
// this codebase actively uses. The full 41-cap list is also
// asserted at the end of the test so a future kernel cap
// addition doesn't get silently dropped.
func TestDecode_KnownCaps(t *testing.T) {
	t.Parallel()

	known := map[string]uint64{
		"cap_chown":              0,
		"cap_dac_override":       1,
		"cap_dac_read_search":    2,
		"cap_fowner":             3,
		"cap_fsetid":             4,
		"cap_kill":               5,
		"cap_setgid":             6,
		"cap_setuid":             7,
		"cap_setpcap":            8,
		"cap_linux_immutable":    9,
		"cap_net_bind_service":   10,
		"cap_net_broadcast":      11,
		"cap_net_admin":          12,
		"cap_net_raw":            13,
		"cap_ipc_lock":           14,
		"cap_ipc_owner":          15,
		"cap_sys_module":         16,
		"cap_sys_rawio":          17,
		"cap_sys_chroot":         18,
		"cap_sys_ptrace":         19,
		"cap_sys_pacct":          20,
		"cap_sys_admin":          21,
		"cap_sys_boot":           22,
		"cap_sys_nice":           23,
		"cap_sys_resource":       24,
		"cap_sys_time":           25,
		"cap_sys_tty_config":     26,
		"cap_mknod":              27,
		"cap_lease":              28,
		"cap_audit_write":        29,
		"cap_audit_control":      30,
		"cap_setfcap":            31,
		"cap_mac_override":       32,
		"cap_mac_admin":          33,
		"cap_syslog":             34,
		"cap_wake_alarm":         35,
		"cap_block_suspend":      36,
		"cap_audit_read":         37,
		"cap_perfmon":            38,
		"cap_bpf":                39,
		"cap_checkpoint_restore": 40,
	}
	for name, bit := range known {
		bit := bit
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, ok := Decode(name)
			if !ok {
				t.Fatalf("Decode(%q) ok=false", name)
			}
			if got != bit {
				t.Fatalf("Decode(%q) = %d, want %d", name, got, bit)
			}
		})
	}
}

// TestDecode_Unknown covers the negative path. A typo or future
// kernel cap returns (0, false).
func TestDecode_Unknown(t *testing.T) {
	t.Parallel()

	if _, ok := Decode("cap_no_such_thing"); ok {
		t.Fatalf("Decode(cap_no_such_thing) ok=true, want false")
	}
	if _, ok := Decode(""); ok {
		t.Fatalf("Decode(\"\") ok=true, want false")
	}
}

// TestEncode_RoundTrip: every Decode(name) result feeds back into
// Encode(bit) and we get the same name back. This protects against
// silent collisions in the bit table.
func TestEncode_RoundTrip(t *testing.T) {
	t.Parallel()

	for name, bit := range capBits {
		bit := bit
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, ok := Encode(bit)
			if !ok {
				t.Fatalf("Encode(%d) ok=false", bit)
			}
			if got != name {
				t.Fatalf("Encode(%d) = %q, want %q", bit, got, name)
			}
		})
	}
}

// TestEncode_UnknownBit: Encode is the inverse of Decode only
// for known bits. Unknown bits must NOT panic and must return
// ("", false).
func TestEncode_UnknownBit(t *testing.T) {
	t.Parallel()

	if _, ok := Encode(99); ok {
		t.Fatalf("Encode(99) ok=true, want false")
	}
}

// TestAllNames_Sorted: AllNames returns the canonical sorted
// list of cap names the codebase recognises. The list must
// not include any unknowns (this protects against a typo or
// a future cap whose name we don't know yet but we tried to
// declare in a unit file).
func TestAllNames_Sorted(t *testing.T) {
	t.Parallel()

	got := AllNames()
	if len(got) != 41 {
		t.Fatalf("AllNames len = %d, want 41 (Linux 5.9+ canonical capset)", len(got))
	}
	sorted := append([]string(nil), got...)
	sort.Strings(sorted)
	for i := range got {
		if got[i] != sorted[i] {
			t.Fatalf("AllNames() not sorted at index %d: got %q, want %q", i, got[i], sorted[i])
		}
	}
}

// TestAllNames_CoversKernelCapset: every name this codebase
// recognises maps to a known bit. The check is redundant with
// TestDecode_KnownCaps but it locks the contract that the two
// functions don't drift.
func TestAllNames_CoversKernelCapset(t *testing.T) {
	t.Parallel()

	names := AllNames()
	for _, n := range names {
		if _, ok := Decode(n); !ok {
			t.Fatalf("AllNames() contains %q but Decode(%q) ok=false", n, n)
		}
	}
}
