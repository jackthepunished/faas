package capdecl

import "sort"

// The capname → bit mapping. Each Linux capability is a single
// bit in a 64-bit mask (the kernel uses a 64-bit capset_t since
// 2.6.25). The bits are defined in linux/capability.h; the cap
// names + numbers are stable across all 5.x and 6.x kernels we
// ship on.
//
// Only the caps this codebase actually uses are enumerated. The
// standard names match include/uapi/linux/capability.h. If a
// future PR needs a cap not in this list, add it here with a
// comment citing the kernel header (the test in capbits_test.go
// also cross-checks against the canonical list of all 41 cap
// names known to linux/capability.h 6.17).
//
// The map is intentionally a package-level var (not a const map)
// so the runtimecheck can iterate it for the "what Linux caps
// does this OS support" check on a darwin dev box. Linux
// 5.9+ has caps 0..40 inclusive (41 caps); the codebase only
// touches a subset.

// capBits maps capname → bit number. The kernel's numbering is
// fixed at compile time; this map is the canonical reference for
// the project.
var capBits = map[string]uint64{
	// 0..9: file-system-ish
	"cap_chown":           0,
	"cap_dac_override":    1,
	"cap_dac_read_search": 2,
	"cap_fowner":          3,
	"cap_fsetid":          4,
	"cap_kill":            5,
	"cap_setgid":          6,
	"cap_setuid":          7,
	"cap_setpcap":         8,
	"cap_linux_immutable": 9,

	// 10..19: network + IPC
	"cap_net_bind_service": 10,
	"cap_net_broadcast":    11,
	"cap_net_admin":        12,
	"cap_net_raw":          13,
	"cap_ipc_lock":         14,
	"cap_ipc_owner":        15,
	"cap_sys_module":       16,
	"cap_sys_rawio":        17,
	"cap_sys_chroot":       18,
	"cap_sys_ptrace":       19,

	// 20..29: process / system
	"cap_sys_pacct":      20,
	"cap_sys_admin":      21,
	"cap_sys_boot":       22,
	"cap_sys_nice":       23,
	"cap_sys_resource":   24,
	"cap_sys_time":       25,
	"cap_sys_tty_config": 26,
	"cap_mknod":          27,
	"cap_lease":          28,
	"cap_audit_write":    29,

	// 30..39: more file / kernel
	"cap_audit_control": 30,
	"cap_setfcap":       31,
	"cap_mac_override":  32,
	"cap_mac_admin":     33,
	"cap_syslog":        34,
	"cap_wake_alarm":    35,
	"cap_block_suspend": 36,
	"cap_audit_read":    37,
	"cap_perfmon":       38,
	"cap_bpf":           39,

	// 40: the last standard cap on Linux 5.14+
	"cap_checkpoint_restore": 40,
}

// Decode returns the bit number for a cap name and ok=true. The
// inverse of Encode. Unknown names return 0, false — callers
// MUST check the ok return.
func Decode(name string) (bit uint64, ok bool) {
	b, found := capBits[name]
	return b, found
}

// Encode is the bit-number → capname lookup. Returns ("", false)
// for unknown bits. The runtimecheck uses this to produce
// human-readable cap names in violation messages.
func Encode(bit uint64) (name string, ok bool) {
	for n, b := range capBits {
		if b == bit {
			return n, true
		}
	}
	return "", false
}

// AllNames returns the sorted list of cap names this codebase
// recognises. The runtimecheck iterates this to assert that
// every cap in a daemon's declaration maps to a known bit.
// The list is sorted alphabetically — callers can compare two
// AllNames() outputs with reflect.DeepEqual or strings.Join
// for deterministic diffs.
func AllNames() []string {
	out := make([]string, 0, len(capBits))
	for n := range capBits {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
