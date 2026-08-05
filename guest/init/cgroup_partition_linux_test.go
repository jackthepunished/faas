//go:build linux

// Tests for the per-workload cgroup v2 partition helpers
// (issue #463 / ADR-069 / PR-B AC #4). The pure-function
// tests (cgroupSafeName, leafDir) don't require a cgroup
// mount — they exercise the name derivation that the
// caller relies on to keep workloads from escaping the
// cgroup hierarchy via path manipulation.
//
// The partitionInto / placeIntoLeaf / mountCgroup2 tests
// are omitted here because they require a real cgroup v2
// mount and root permissions to write into memory.max /
// cgroup.procs. Those tests run as part of the metal
// suite (TestMetalSidecarCgroupPartition in
// pkg/fcvm/manager_metal_test.go).

package main

import (
	"strings"
	"testing"
)

// TestCgroupSafeName_ValidCombinations pins the happy path:
// type + name joined with a single dash, used to derive the
// per-workload cgroup leaf path. The cgroup v2 kernel
// rejects slashes in leaf names; this test catches a
// regression that lets a workload's name leak through as a
// path separator.
func TestCgroupSafeName_ValidCombinations(t *testing.T) {
	cases := []struct {
		typ, name, want string
	}{
		{"init", "migrator", "init-migrator"},
		{"sidecar", "scraper", "sidecar-scraper"},
		{"main", "app", "main-app"},
		// Names with internal dashes are valid; the
		// function preserves them (kernel allows).
		{"sidecar", "log-shipper", "sidecar-log-shipper"},
		// Numbers in the name are valid.
		{"init", "init1", "init-init1"},
	}
	for _, tc := range cases {
		t.Run(tc.typ+"_"+tc.name, func(t *testing.T) {
			got := cgroupSafeName(tc.typ, tc.name)
			if got != tc.want {
				t.Errorf("cgroupSafeName(%q, %q) = %q, want %q", tc.typ, tc.name, got, tc.want)
			}
		})
	}
}

// TestCgroupSafeName_RejectsPathEscape pins the
// path-escape guard: a workload whose name contains / or
// \\ or a NUL byte MUST NOT produce a leaf name that could
// escape /sys/fs/cgroup. An empty result signals the
// caller to skip the partition (the workload runs under
// the parent scope, which still has the host-side cap).
func TestCgroupSafeName_RejectsPathEscape(t *testing.T) {
	cases := []struct {
		name, value string
	}{
		{"slash-injection", "evil/name"},
		{"backslash-injection", `evil\name`},
		{"nul-byte-injection", "evil\x00name"},
		{"dotdot-escape", "../etc"},
		{"dotdot-middle", "name/../../etc"},
		{"dotdot-suffix", "name.."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cgroupSafeName("init", tc.value)
			if got != "" {
				t.Errorf("cgroupSafeName must reject %q, got %q", tc.value, got)
			}
		})
	}
}

// TestCgroupSafeName_RejectsEmpty pins the empty-input
// guard. Both empty type and empty name return "" so the
// caller skips the partition — an empty leaf name would
// resolve to the cgroup root, defeating the per-workload
// partition (writing into root memory.max affects every
// cgroup in the tree).
func TestCgroupSafeName_RejectsEmpty(t *testing.T) {
	if got := cgroupSafeName("", "main"); got != "" {
		t.Errorf("cgroupSafeName(empty type) = %q, want \"\"", got)
	}
	if got := cgroupSafeName("main", ""); got != "" {
		t.Errorf("cgroupSafeName(empty name) = %q, want \"\"", got)
	}
	if got := cgroupSafeName("", ""); got != "" {
		t.Errorf("cgroupSafeName(both empty) = %q, want \"\"", got)
	}
}

// TestLeafDir_PinsMountpoint pins the leaf path derivation:
// leafDir joins cgroupRoot + safe name. A regression that
// drifts the mountpoint (e.g. to /sys/fs/cgroup/faas)
// would silently break the partition because the
// partitionInto / placeIntoLeaf writes would land in a
// non-mounted subtree.
func TestLeafDir_PinsMountpoint(t *testing.T) {
	got := leafDir("init", "migrator")
	want := cgroupRoot + "/init-migrator"
	if got != want {
		t.Errorf("leafDir = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, cgroupRoot) {
		t.Errorf("leafDir must root at %q, got %q", cgroupRoot, got)
	}
}

// TestLeafDir_EmptySafeNameReturnsEmpty pins the
// skipped-partition outcome: leafDir returns "" when the
// name fails cgroupSafeName (path escape, empty input).
// The caller checks for "" to skip the partition without
// an error — the workload still runs, just at the parent
// cgroup scope.
func TestLeafDir_EmptySafeNameReturnsEmpty(t *testing.T) {
	if got := leafDir("init", "../etc"); got != "" {
		t.Errorf("leafDir(escape attempt) = %q, want \"\"", got)
	}
	if got := leafDir("", "main"); got != "" {
		t.Errorf("leafDir(empty type) = %q, want \"\"", got)
	}
}
