//go:build linux

// Per-workload cgroup v2 partition (issue #463 / ADR-069 /
// PR-B AC #4).
//
// Why in-guest, not just host-side: the host-side
// per-instance / per-workload cgroup scopes (vmmd's
// writeWorkloadCgroup at pkg/fcvm/cgroup.go:168) scope the
// guest *from the outside*. Inside the guest, processes are
// in the guest kernel's memcg tree, and the host's cgroup
// hierarchy is not visible (the guest cgroup namespace is
// isolated by guest-init's CLONE_NEWCGROUP step in main_linux.go).
// A workload that exceeds its plan RAM triggers the host's
// per-VM scope, but inside the guest we want a per-workload
// OOM kill — the sidecar OOM must not OOM-kill the main
// workload (and vice versa).
//
// The contract:
//
//   1. cgroup2 is mounted at /sys/fs/cgroup inside the
//      guest (see mountCgroup2 in main_linux.go — the mount
//      happens AFTER pivotInto so the mount lives on the
//      new root).
//
//   2. Before each workload's exec.Command.Start, mkdir the
//      per-workload leaf at /sys/fs/cgroup/<safe-name>,
//      write memory.max = spec.RamMB << 20, and after Start
//      write the child PID into the leaf's cgroup.procs.
//
//   3. Sidecar OOM stays scoped to that leaf (cgroup v2
//      memory controller kills only the offending leaf's
//      processes). The main workload keeps running.
//
// The cgroup name derivation is intentionally narrow: type
// + name joined with a single dash, with a guard against
// path separators that would let a workload escape the leaf.
// Mirrors writeWorkloadCgroup at pkg/fcvm/cgroup.go:172.

package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// cgroupRoot (issue #463 / ADR-069 / PR-B AC #4) is the
// canonical mountpoint for the in-guest cgroup v2
// hierarchy. guest-init's mountCgroup2 (main_linux.go) mounts
// cgroup2 here after pivotInto so the leaf writes below land
// on the new root. Hardcoded to /sys/fs/cgroup because every
// Linux userland agrees on the path and the alternative
// (env-driven) would let a deployment redirect the partition
// into a workload-controlled path.
const cgroupRoot = "/sys/fs/cgroup"

// cgroupSafeName (issue #463 / ADR-069 / PR-B AC #4) is
// the leaf-name helper. Joins type + name with a single
// dash, with a guard against path separators (a workload
// that smuggles ".." into its name must NOT escape the
// cgroup hierarchy). Empty type or name returns "" so the
// caller can skip the leaf write — empty leaves are
// indistinguishable from the root scope, and writing into
// the root would defeat the per-workload partition.
//
// Mirrors the host-side writeWorkloadCgroup path at
// pkg/fcvm/cgroup.go:172-174 (which clamps `..` and `/`),
// with the additional constraint that the in-guest
// partition never uses the workload's StorageKey (the
// guest can't read StorageBackend keys — the leaf name
// stays short, type + name only).
func cgroupSafeName(typ, name string) string {
	if typ == "" || name == "" {
		return ""
	}
	for _, ch := range name {
		if ch == '/' || ch == '\\' || ch == 0 {
			return ""
		}
	}
	// Belt-and-suspenders against the ".." escape
	// (cgroup v2 rejects leaf names containing .. in
	// practice, but checking here keeps the failure
	// observable at workload boot, not at the first
	// write to memory.max).
	if strings.Contains(name, "..") {
		return ""
	}
	return typ + "-" + name
}

// leafDir returns the absolute cgroup v2 leaf path for a
// workload spec. The path is rooted at cgroupRoot so the
// caller can os.WriteFile into it directly. The leaf is
// NOT created here — partitionInto creates it before
// writing memory.max. Empty safe name returns "" so the
// caller can skip the partition without an error (the
// workload still runs, just at the root cgroup scope —
// the per-instance parent scope still has the cap from
// vmmd's writePlanCgroup).
func leafDir(typ, name string) string {
	safe := cgroupSafeName(typ, name)
	if safe == "" {
		return ""
	}
	return filepath.Join(cgroupRoot, safe)
}

// partitionInto (issue #463 / ADR-069 / PR-B AC #4) sets up
// the per-workload cgroup v2 leaf BEFORE the workload is
// exec'd:
//
//  1. mkdir <leaf>
//  2. write memory.max = ramMB << 20 (bytes)
//
// Errors are logged + returned (the caller decides whether
// to fail the deploy). memory.max = 0 is forbidden — the
// kernel treats it as "unlimited", which defeats the
// partition. We floor at max(1, ramMB) << 20 (a 1 MB
// minimum leaf) so a workload that boots with a missing
// ram_mb (RamMB = 0 on the wire, legacy single-workload
// path) still gets a non-zero cap. Defence-in-depth: the
// customer-facing API gate already floors Sidecar.RamMB at
// 16 MB; this is a backstop.
//
// cgroup v2 must be mounted at cgroupRoot for the writes
// to land. mountCgroup2 (main_linux.go) is called between
// pivotInto and the supervisor's first workload, so by the
// time partitionInto runs the leaf path is reachable.
func partitionInto(leaf string, ramMB int) error {
	if leaf == "" {
		return errors.New("cgroup partition: empty leaf")
	}
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		return fmt.Errorf("cgroup partition: mkdir %s: %w", leaf, err)
	}
	bytes := int64(ramMB) << 20
	if bytes <= 0 {
		// Floor: a zero memory.max = "unlimited"
		// (kernel semantics), which defeats the
		// partition. Defensive floor at 1 MiB.
		bytes = 1 << 20
	}
	if err := os.WriteFile(
		filepath.Join(leaf, "memory.max"),
		[]byte(strconv.FormatInt(bytes, 10)+"\n"),
		0o644,
	); err != nil {
		return fmt.Errorf("cgroup partition: write memory.max for %s: %w", leaf, err)
	}
	return nil
}

// placeIntoLeaf writes the child PID into the leaf's
// cgroup.procs. Called AFTER exec.Command.Start so the PID
// is the forked child's PID (the kernel's exec semantics
// preserve the PID across execve). Empty pid means the
// caller did not capture the PID — skip without error so a
// workload that fails to fork does not produce a spurious
// "cgroup.procs write failed" log line.
//
// Race window: between Start and placeIntoLeaf, the
// forked child can fork+mmap before the cgroup.procs
// write lands. The window is benign because the leaf's
// parent scope (the cgroup root, which has no cap)
// doesn't enforce a limit either; worst case is a brief
// moment where the cap is unenforced, then enforced once
// the write completes. Same posture as Docker / runc.
func placeIntoLeaf(leaf string, pid int, log *slog.Logger) {
	if leaf == "" || pid <= 0 {
		return
	}
	if err := os.WriteFile(
		filepath.Join(leaf, "cgroup.procs"),
		[]byte(strconv.Itoa(pid)+"\n"),
		0o644,
	); err != nil && log != nil {
		log.Warn("cgroup.procs write failed",
			"leaf", leaf, "pid", pid, "err", err)
	}
}

// mountCgroup2 (issue #463 / ADR-069 / PR-B AC #4) mounts
// cgroup2 at /sys/fs/cgroup inside the guest. Called from
// main_linux.go::boot AFTER pivotInto so the mount lives on
// the new root (mounting before pivot would put the
// mountpoint on the OLD root, and pivot would hide it).
//
// Returns the mount error verbatim. The caller (boot)
// tolerates a non-nil return (the host-side per-instance
// scope from vmmd is still enforced even when the in-guest
// partition is unavailable) and logs the error so a
// missing CONFIG_CGROUP_V2=y (kernel ENOSYS) is visible in
// the journalctl logs as a soft warning, not the silent
// no-op the previous shape produced.
func mountCgroup2() error {
	if err := os.MkdirAll(cgroupRoot, 0o755); err != nil {
		return fmt.Errorf("cgroup2 mkdir: %w", err)
	}
	if err := syscall.Mount("cgroup2", cgroupRoot, "cgroup2", 0, ""); err != nil {
		return fmt.Errorf("cgroup2 mount %s: %w", cgroupRoot, err)
	}
	return nil
}
