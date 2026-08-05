// Package capdecl is the load-bearing enforcement of the
// CLAUDE.md invariant "vmmd is the ONLY root component that touches
// firecracker/jailer / mounts filesystems". Before DEPLOY-1 the
// invariant was enforced by code review and conventions; PR-K
// (PR #644) silently violated it by adding AmbientCapabilities=
// cap_sys_admin to imaged's systemd unit and a unix.Mount(2) syscall
// in pkg/imaged/mount_overlay_linux.go, and the regression wasn't
// caught until five days of failed cd-controlplane deploys later.
//
// This package gives the invariant a code shape. Each daemon
// declares its expected capability set in Decode-capname form
// (cap_sys_admin, cap_net_bind_service, etc.). The runtimecheck
// subpackage reads /proc/self/status at startup and asserts the
// live capBnd/capEff/capPrm/capAmb sets are subsets of the
// declaration. The depguard rule in .golangci.yml rejects
// unix.Mount / unix.Unmount calls outside pkg/vmmd/, and rejects
// pkg/vmmdgrpc / pkg/vmmdmount imports outside cmd/vmmd/** and
// pkg/vmmd/**. Together: the next PR-K attempt fails at CI time
// (lint) and at runtime (capdecl check), not in production.
//
// The choice of capnames is rooted in /proc/self/status which
// the kernel emits as a hex bitmask of the indexed caps defined
// in linux/capability.h. cap_net_bind_service is bit 10,
// cap_sys_admin is bit 21, etc. Encode("cap_sys_admin") returns
// 1 << 21. The full 41-cap Linux list lives in
// internal/capbits/capbits.go so the test runs on macOS dev too
// (the kernel-conditional compile is isolated).
package capdecl

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Declaration is one daemon's declared capability set. Allow is
// the explicit allow-list — the daemon REQUIRES these caps at
// runtime. Deny is the explicit deny-list — the daemon must NEVER
// acquire these caps. A cap may not appear in both lists.
//
// The split is intentional: a vmmd declaration that lists every
// cap it inherits from systemd (the default bounding set is
// effectively the full set under systemd v252+) makes the
// declaration meaningless. The Allow list is what the daemon
// ACTIVELY uses; the Deny list is what the daemon promises never
// to acquire. The runtimecheck validates both.
//
// Empty Allow + empty Deny is the canonical "unprivileged daemon"
// declaration — the vmmd-only exception is the single fingerprint
// the lint rule protects.
type Declaration struct {
	Allow []string
	Deny  []string
}

// Validate checks the declaration for self-consistency. It does
// not check the cap names against the kernel capset — that lives
// in the runtimecheck and is bounded by the operating-system
// running the test. Validation here is structural: no overlap,
// no empty strings, no duplicate names.
func (d Declaration) Validate() error {
	allow := make(map[string]struct{}, len(d.Allow))
	for _, c := range d.Allow {
		if c == "" {
			return errors.New("capdecl: empty cap name in Allow")
		}
		if _, dup := allow[c]; dup {
			return fmt.Errorf("capdecl: duplicate cap %q in Allow", c)
		}
		allow[c] = struct{}{}
	}
	deny := make(map[string]struct{}, len(d.Deny))
	for _, c := range d.Deny {
		if c == "" {
			return errors.New("capdecl: empty cap name in Deny")
		}
		if _, dup := deny[c]; dup {
			return fmt.Errorf("capdecl: duplicate cap %q in Deny", c)
		}
		if _, ok := allow[c]; ok {
			return fmt.Errorf("capdecl: cap %q appears in both Allow and Deny", c)
		}
		deny[c] = struct{}{}
	}
	return nil
}

// Sorted returns Allow and Deny in canonical ordering (alphabetical)
// so two equivalent declarations compare equal — used by the
// runtimecheck violation message so log output is deterministic.
func (d Declaration) Sorted() Declaration {
	out := Declaration{
		Allow: append([]string(nil), d.Allow...),
		Deny:  append([]string(nil), d.Deny...),
	}
	sort.Strings(out.Allow)
	sort.Strings(out.Deny)
	return out
}

// String returns a stable, human-readable form of the declaration
// for log messages. The format is:
//
//	capdecl{allow:[cap_a cap_b] deny:[cap_c]}
//
// with cap names sorted. Empty sets are omitted. The two halves
// are space-separated only when both halves are non-empty.
func (d Declaration) String() string {
	var sb strings.Builder
	sb.WriteString("capdecl{")
	sorted := d.Sorted()
	allowNonEmpty := len(sorted.Allow) > 0
	denyNonEmpty := len(sorted.Deny) > 0
	if allowNonEmpty {
		sb.WriteString("allow:[")
		sb.WriteString(strings.Join(sorted.Allow, " "))
		sb.WriteString("]")
	}
	if allowNonEmpty && denyNonEmpty {
		sb.WriteString(" ")
	}
	if denyNonEmpty {
		sb.WriteString("deny:[")
		sb.WriteString(strings.Join(sorted.Deny, " "))
		sb.WriteString("]")
	}
	sb.WriteString("}")
	return sb.String()
}

// Equal reports whether two declarations are equivalent. Order
// insensitive (the Allow/Deny slices are sorted before compare).
func (d Declaration) Equal(other Declaration) bool {
	a := d.Sorted()
	o := other.Sorted()
	if len(a.Allow) != len(o.Allow) || len(a.Deny) != len(o.Deny) {
		return false
	}
	for i := range a.Allow {
		if a.Allow[i] != o.Allow[i] {
			return false
		}
	}
	for i := range a.Deny {
		if a.Deny[i] != o.Deny[i] {
			return false
		}
	}
	return true
}

// ParseStatus parses a /proc/self/status-style byte slice and
// returns the capBnd/capEff/capPrm/capAmb bitmasks it contains.
// The format is line-oriented: each line has a single "Cap<Kind>:"
// prefix followed by a hex bitmask; lines that don't start with
// "Cap" are ignored. Empty input returns all-zero masks, no
// error.
//
// The function is pure parsing — it does not validate that the
// resulting mask is consistent with the kernel capset (a cap
// name that Encode doesn't recognise produces a bit the kernel
// would never set). The runtimecheck relies on this purity so it
// can be tested on darwin where /proc/self/status doesn't exist.
func ParseStatus(b []byte) CapMasks {
	var m CapMasks
	for _, line := range strings.Split(string(b), "\n") {
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		key := line[:colon]
		var target *uint64
		switch key {
		case "CapInh":
			target = &m.Inh
		case "CapPrm":
			target = &m.Prm
		case "CapEff":
			target = &m.Eff
		case "CapBnd":
			target = &m.Bnd
		case "CapAmb":
			target = &m.Amb
		default:
			continue
		}
		var v uint64
		_, err := fmt.Sscanf(line[colon+1:], " %x", &v)
		if err != nil {
			continue
		}
		*target = v
	}
	return m
}

// CapMasks is the four live cap sets /proc/self/status emits.
// Inh = inheritable, Prm = permitted, Eff = effective, Bnd =
// bounding, Amb = ambient. The kernel emits the masks as hex
// bitmasks; the runtimecheck validates that each cap the
// declaration Allow-lists has a corresponding bit in the
// relevant mask (Bnd for the cap's reachability, Eff for what
// the process actually uses).
type CapMasks struct {
	Inh uint64
	Prm uint64
	Eff uint64
	Bnd uint64
	Amb uint64
}

// Has reports whether each cap in names has its bit set in mask.
// Returns the first cap that is missing, or "" if all are set.
// Names that aren't recognised by Decode produce a parse error
// path — see Decode for the bit-resolution contract.
//
// Decode returns the raw bit number (e.g. cap_kill → 5). The
// runtime mask from /proc/self/status is the shifted bit
// (1 << 5 = 0x20). We apply the shift here so callers can
// pass masks directly without pre-shifting.
func (m CapMasks) Has(names []string, mask uint64) (missing string, unknown []string) {
	for _, name := range names {
		bitNum, ok := Decode(name)
		if !ok {
			unknown = append(unknown, name)
			continue
		}
		bit := uint64(1) << bitNum
		if mask&bit == 0 {
			return name, unknown
		}
	}
	return "", unknown
}

// NotIn reports which of names have a bit set in mask. The
// complement of Has — used by the deny-list check. Same shift
// semantics as Has: the runtime mask is shifted; Decode's
// raw bit number is shifted here.
func (m CapMasks) NotIn(names []string, mask uint64) (unexpected []string, unknown []string) {
	for _, name := range names {
		bitNum, ok := Decode(name)
		if !ok {
			unknown = append(unknown, name)
			continue
		}
		bit := uint64(1) << bitNum
		if mask&bit != 0 {
			unexpected = append(unexpected, name)
		}
	}
	return unexpected, unknown
}
