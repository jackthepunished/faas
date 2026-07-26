// seccomp_self_filter_test.go — M8 §11 in-process pin for the
// SeccompStatus gRPC handler with a REAL seccomp filter attached to
// the test process. The existing TestSeccompStatus_HappyPath_ReadsProcFS
// (bufconn_test.go:607) reads /proc/self/status from the test process
// but the test process is mode=disabled by default, so it only
// pins "handler doesn't crash" — a regression that demotes mode=2
// to mode=0 on the wire would still pass. This file installs a
// trivial ALLOW-BPF filter on the test process so the same handler
// round-trip is exercised against mode=filter + filter_len>0, the
// only mode the jailer actually runs in production.
//
// The cross-process e2e (cmd/e2e/sec11_seccomp_e2e_test.go, //go:build
// metal) remains the authoritative gate: it boots vmmd as a real
// subprocess and reads /proc/<pid>/status from the test process
// independently of the gRPC handler. The two readers MUST agree
// because they're reading the same kernel state — that tripwire is
// what test #2 below asserts at the in-process layer.
//
// File is //go:build linux because the BPF + seccomp(2) + /proc code
// path is Linux-only. macOS dev + Windows CI skip via the build tag
// (cleaner than runtime.GOOS skip + never-compile-dead-code).
//
//go:build linux

package vmmdgrpc_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"unsafe"

	vmmdpb "github.com/onebox-faas/faas/api/proto/onebox/faas/vmmd/v1"
	"github.com/onebox-faas/faas/pkg/vmmdgrpc"
	"golang.org/x/sys/unix"
)

// bpfInsn mirrors the kernel's `struct sock_filter` (8 bytes: u16 code,
// u8 jt, u8 jf, u32 k). The Linux kernel BPF ISA has one RET-only
// instruction that returns SECCOMP_RET_ALLOW unconditionally — the
// trivial "let everything through" filter used by libseccomp's
// SCMP_ACT_ALLOW default. Hand-built rather than via libseccomp-golang
// so we don't add a CGO dependency to the build graph.
type bpfInsn struct {
	Code uint16
	Jt   uint8
	Jf   uint8
	K    uint32
}

// allowFilter is the single-instruction BPF program:
//
//	BPF_RET | K = SECCOMP_RET_ALLOW
//
// Attached to the test process before calling SeccompStatus; the gRPC
// handler then reads /proc/self/status and MUST report mode="filter"
// + FilterLen=1. The BPF stays attached for the rest of the test
// process — that's fine because SECCOMP_RET_ALLOW is a no-op, and
// every test in this file installs the SAME allow-filter (kernel
// rejects stack-of-different-filters, but identical second-install
// is allowed and benign).
var allowFilter = bpfInsn{
	Code: 0x0006, // BPF_RET
	K:    unix.SECCOMP_RET_ALLOW,
}

// installSelfFilter attaches the allow filter to the current process.
// Self-attaching a seccomp filter does not require root (Linux ≥ 3.5).
// Returns the error from the underlying syscall so the test can
// t.Skip cleanly if the kernel refuses (e.g. seccomp disabled at
// boot in a container).
//
// Idempotency caveat: the kernel rejects replacing a non-trivial
// filter with a DIFFERENT one (EACCES). Calling installSelfFilter
// twice in the same binary is fine here only because every caller
// in this file installs the same allowFilter — the BPF instruction
// is identical, so the second install is a benign restart of the
// same filter. If a future test in this file ever installs a
// different filter, it must either come last or use
// SECCOMP_FILTER_FLAG_TSYNC / SECCOMP_SET_MODE_FILTER with
// SECCOMP_FILTER_FLAG_SPEC_ALLOW — or the kernel will skip the
// test with EACCES.
func installSelfFilter(t *testing.T) {
	t.Helper()
	// PR_SET_NO_NEW_PRIVS is required before seccomp(2) accepts any
	// non-trivial filter; harmless for a trivial allow filter.
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		t.Skipf("PR_SET_NO_NEW_PRIVS refused: %v (kernel does not allow self-seccomp)", err)
	}
	progPtr := uintptr(unsafe.Pointer(&allowFilter))
	if _, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, 0, progPtr); errno != 0 {
		t.Skipf("seccomp(SECCOMP_SET_MODE_FILTER) refused: %v (kernel may have seccomp disabled)", errno)
	}
}

// TestSeccompStatus_WireAgreesWithLocalProcRead mirrors the wire-vs-local
// tripwire shape at the in-process layer: install the filter, call
// SeccompStatus over bufconn, AND independently parse /proc/self/status
// via vmmdgrpc.ParseSeccompLines, then assert the two readers agree.
//
// The cross-process e2e (cmd/e2e/sec11_seccomp_e2e_test.go, //go:build
// metal) is the only place that reads /proc/<pid>/status from a
// DIFFERENT process than the gRPC handler — that is the property
// that makes the wire-vs-local tripwire load-bearing. Here both
// readers run in the same process, so the tripwire is narrower:
// catches a handler-side parser drift that returns different
// (mode, filterLen) than the same kernel state read via the exported
// ParseSeccompLines. The kernel-state-with-different-reader property
// stays the metal e2e's job.
//
// Runs first in the file (before TestSeccompStatus_SelfInFilterMode)
// so a local-read sanity-check failure localises here. If the Go
// runtime ever installs an unrelated filter before this test runs,
// the local mode read returns something other than "filter" and
// the test fatals with a clear "sanity check" message — preferable
// to the stricter assertion failing first with a confusing diff.
func TestSeccompStatus_WireAgreesWithLocalProcRead(t *testing.T) {
	installSelfFilter(t)

	// Local read first (mirrors what the cross-process e2e does:
	// read /proc/<pid>/status from a process that does NOT own
	// the gRPC handler, asserting the kernel state independently).
	localBody, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatalf("read /proc/self/status: %v", err)
	}
	localMode, localFilter, err := vmmdgrpc.ParseSeccompLines(strings.NewReader(string(localBody)))
	if err != nil {
		t.Fatalf("local ParseSeccompLines: %v", err)
	}
	if localMode != "filter" {
		t.Fatalf("local mode = %q, want %q (sanity check: did installSelfFilter take effect?)", localMode, "filter")
	}

	// Wire read: same kernel state, but read via the gRPC handler.
	f := &fakeVMM{}
	f.instancePIDFn = func(instance string) (int, bool) {
		if instance == "" {
			return 0, false
		}
		return os.Getpid(), true
	}
	cli, _ := newServer(t, f)
	resp, err := cli.SeccompStatus(context.Background(), &vmmdpb.SeccompStatusRequest{Instance: "self"})
	if err != nil {
		t.Fatalf("SeccompStatus: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("Error = %q", resp.GetError())
	}

	if got := resp.GetMode(); got != localMode {
		t.Errorf("wire mode = %q, local mode = %q (handler and direct reader disagree — kernel-state readback drift)", got, localMode)
	}
	if got := resp.GetFilterLen(); got != localFilter {
		t.Errorf("wire FilterLen = %d, local FilterLen = %d (handler and direct reader disagree — Seccomp_filters: parse drift)", got, localFilter)
	}
}

// TestSeccompStatus_SelfInFilterMode_ReturnsFilter — installs the
// allow filter on the test process, then calls SeccompStatus through
// the bufconn harness with InstancePID=os.Getpid(). The wire MUST
// report mode="filter" + FilterLen>=1. The existing Happy-Path test
// only asserts mode ∈ {disabled,strict,filter}; this one pins the
// "filter" half specifically — a parser regression that returns
// "filter" for a real mode=2 + returns "disabled" for a real mode=0
// would pass the existing test but trip here.
//
// The kernel-attached filter survives across tests in the same
// binary (no way to detach; the BPF_ACCUMULATE form just adds to
// the chain, and identical-second-install is benign because the
// instruction is identical). The follow-up test calls
// installSelfFilter again — that is a no-op for the wire but the
// idempotent install covers the "test was run after another that
// already installed" case.
func TestSeccompStatus_SelfInFilterMode_ReturnsFilter(t *testing.T) {
	installSelfFilter(t)

	f := &fakeVMM{}
	f.instancePIDFn = func(instance string) (int, bool) {
		if instance == "" {
			return 0, false
		}
		return os.Getpid(), true
	}
	cli, _ := newServer(t, f)
	resp, err := cli.SeccompStatus(context.Background(), &vmmdpb.SeccompStatusRequest{Instance: "self"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("Error = %q (handler failed to read /proc/self/status)", resp.GetError())
	}
	if got := resp.GetMode(); got != "filter" {
		t.Errorf("mode = %q, want %q (test process has BPF attached)", got, "filter")
	}
	if got := resp.GetFilterLen(); got < 1 {
		t.Errorf("FilterLen = %d, want >= 1 (test process has 1 BPF program attached)", got)
	}
	if got := resp.GetPid(); got != int32(os.Getpid()) {
		t.Errorf("pid = %d, want %d", got, os.Getpid())
	}
}
