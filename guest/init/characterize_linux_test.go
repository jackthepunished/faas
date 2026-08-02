//go:build linux

// Linux-only pure tests for characterize_linux.go. The wire / vsock /
// real-/proc paths are not exercised here — those need a live AF_VSOCK
// listener + a customer app and live in the //go:build metal suite.
//
// countOutboundLinux lives in characterize_linux.go (gated on linux
// because it references ownedSocketInodes which walks /proc/<pid>/fd).
// Its pure early-out contract is testable here without a real /proc;
// the integration path (real /proc/net/tcp with ESTABLISHED entries)
// belongs to metal.
package main

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

func TestCountOutboundLinux_NoChildEarlyOut(t *testing.T) {
	// countOutboundLinux short-circuits when there's no supervisor
	// child yet (pid <= 0). Mirrors TestProbeListening_NoChildEarlyOut's
	// contract on the bind side: a missing PID is a "no signal", not
	// an error. The classifier interprets it as 0 outbound, which is
	// the correct hint for a job that never connected out.
	for _, pid := range []int{-1, 0} {
		if got := countOutboundLinux(pid); got != 0 {
			t.Errorf("countOutboundLinux(%d) = %d, want 0", pid, got)
		}
	}
}

// TestOwnedSocketInodes_DepthBounded pins the recursive walk's depth
// cap (ADR-051 §"Consequences": customer process tree visibility is
// load-bearing for the worker signal, but a runaway walk on a
// pathological forker would lock up characterization). The constant
// is intentionally small (8) — covers realistic Node cluster-mode +
// setpgid shapes, refuses anything deeper. A bump here should be a
// deliberate choice with a regression test in the metal suite that
// pins the new depth end-to-end.
func TestOwnedSocketInodes_DepthBounded(t *testing.T) {
	if ownedSocketInodesRecursiveDepth > 16 {
		t.Errorf("ownedSocketInodesRecursiveDepth = %d, want <= 16 (any larger cap invites a runaway walk on a pathological forker)",
			ownedSocketInodesRecursiveDepth)
	}
	if ownedSocketInodesRecursiveDepth < 2 {
		t.Errorf("ownedSocketInodesRecursiveDepth = %d, want >= 2 (a depth-1 walk misses grandchildren — the common Node cluster-mode case)",
			ownedSocketInodesRecursiveDepth)
	}
}

// TestWireConstants_MatchHost pins the wire-format constants shared
// between guest-init and the host-side listener. ADR-051 §"Wire
// constants" + ADR-047's numbering line (resume=1024/1,
// stateless_advisory=1025/2, characterization=1026/3) require a
// 1:1 match — a drift on either side silently breaks the wire (the
// guest's STREAM+ack listener either doesn't accept (wrong port) or
// accepts from the wrong guest (wrong msgtype)).
//
// The text-extract approach mirrors the SQL-static guards in
// pkg/state/ that pin SQL shape across refactors — guest/init does
// not import pkg/fcvm (one-way layering, see
// listen_resume_linux.go:25), so a text comparison is the right
// level of defense. Failure modes caught:
//   - port bumped on one side (e.g. 1026 → 1027) without the other;
//   - msgtype changed (e.g. 3 → 4) without the other;
//   - body cap lowered below the guest's truncation threshold
//     (a 32 KiB guest body would be rejected by a 16 KiB host cap).
func TestWireConstants_MatchHost(t *testing.T) {
	data, err := os.ReadFile(repoRootVMM(t))
	if err != nil {
		t.Fatalf("read pkg/fcvm/vmm.go: %v", err)
	}
	src := string(data)

	port := extractIntConst(t, src, `VsockCharacterizationHostPort\s*(?:uint32)?\s*=\s*([0-9]+)`)
	if port != int(VsockCharacterizationPort) {
		t.Errorf("host port = %d, want guest port %d (drift would break wire accept)", port, VsockCharacterizationPort)
	}

	msgType := extractIntConst(t, src, `VsockCharacterizationMsgType\s*(?:uint32)?\s*=\s*([0-9]+)`)
	if msgType != int(VsockCharacterizationMsgType) {
		t.Errorf("host msgtype = %d, want guest msgtype %d (drift would route characterization frames to the wrong handler)",
			msgType, VsockCharacterizationMsgType)
	}

	// MaxBody is `32 * 1024` — the regex extracts the LHS literal so
	// we evaluate the expression the same way the host would at
	// runtime. A host-side drop to 16 KiB would reject every
	// characterization body the guest produces.
	maxBodyExpr := extractFirstMatch(t, src, `VsockCharacterizationMaxBody\s*(?:=\s*|int\s*=\s*)(.+)`)
	hostMax := evalIntExpr(t, maxBodyExpr)
	if hostMax != VsockCharacterizationMaxBody {
		t.Errorf("host MaxBody = %d, want guest MaxBody %d (drift would reject guest bodies above host cap)",
			hostMax, VsockCharacterizationMaxBody)
	}
}

// repoRootVMM resolves the path to pkg/fcvm/vmm.go from the
// guest/init package. Tests run with cwd=module root (Go's standard
// test runner), so we can reach the file via two `..` segments from
// guest/init/. If a future test runner changes cwd, this path is the
// single point of adjustment.
func repoRootVMM(t *testing.T) string {
	t.Helper()
	for _, candidate := range []string{
		"../pkg/fcvm/vmm.go",
		"../../pkg/fcvm/vmm.go",
	} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	t.Fatalf("cannot locate pkg/fcvm/vmm.go from cwd=%s; check repoRootVMM", mustGetwd())
	return ""
}

func mustGetwd() string {
	wd, _ := os.Getwd()
	return wd
}

// extractFirstMatch returns the first regex match's first capture
// group from src. Used for arbitrary RHS expressions (the
// MaxBody case where the host writes `32 * 1024`).
func extractFirstMatch(t *testing.T, src, pattern string) string {
	t.Helper()
	re := regexp.MustCompile(pattern)
	m := re.FindStringSubmatch(src)
	if len(m) < 2 {
		t.Fatalf("pattern %q produced no match in pkg/fcvm/vmm.go", pattern)
	}
	return m[1]
}

// extractIntConst matches a single-literal RHS (e.g. `= 1026`) and
// returns it parsed as int. For expressions like `32 * 1024`, use
// extractFirstMatch + evalIntExpr.
func extractIntConst(t *testing.T, src, pattern string) int {
	t.Helper()
	expr := extractFirstMatch(t, src, pattern)
	v, err := strconv.Atoi(expr)
	if err != nil {
		t.Fatalf("RHS %q of pattern %q is not a single integer literal: %v", expr, pattern, err)
	}
	return v
}

// evalIntExpr evaluates a tiny Go int expression with the supported
// shapes used in pkg/fcvm/vmm.go's vsock constants: a single literal,
// `N * M`, or `N << K`. Anything else fails the test — a new shape
// is a deliberate widening of the supported surface and should be
// added here explicitly.
func evalIntExpr(t *testing.T, expr string) int {
	t.Helper()
	for _, op := range []string{" << ", " * "} {
		if idx := indexOf(expr, op); idx > 0 {
			lhs := mustAtoi(t, expr[:idx])
			rhs := mustAtoi(t, expr[idx+len(op):])
			switch op {
			case " << ":
				return lhs << uint(rhs)
			case " * ":
				return lhs * rhs
			}
		}
	}
	return mustAtoi(t, expr)
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	v, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("strconv.Atoi(%q): %v", s, err)
	}
	return v
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
