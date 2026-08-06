//go:build linux

// Tests for the type=0x04 (tail_event) parse path of
// parseFrameworkReadyDatagram (issue #667 / ADR-078). The parser
// is the host-side DGRAM boundary between the guest-init proxy
// and the Manager's MarkInstanceTailTerminal; the wire shape is
// mirrored in guest/init/sidecar_events_proxy_linux.go (see
// VsockTailEventType + sendTail).
//
// The build tag mirrors cmd/vmmd/framework_ready_recv.go — the
// parser and the DGRAM constants are linux-only because the wire
// is AF_VSOCK, which is a Linux kernel feature.
//
// Why a separate test file: the tail-event wire is a different
// envelope shape (16-byte fixed-size body after the type byte,
// no NUL-bounded runtime, no JSON envelope, no optional warmup
// prefix) and a different dispatch target. Splitting the test
// files keeps each one narrative-tight and makes the per-PR
// diff reviewable in isolation.
package main

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/fcvm"
)

// makeTailEvent builds a wire body in the same shape the guest
// emits: [1B type=0x04][1B outcome][6B reserved][8B BE uint64
// elapsed_ms]. Centralised so each test case reads as a small
// picture of the wire rather than a placeholder soup. The
// reserved 6 bytes are always 0x00 in PR 3 — see
// guest/init/sidecar_events_proxy_linux.go::sendTail.
func makeTailEvent(outcome byte, elapsedMs int64) []byte {
	out := []byte{VsockFrameworkReadyHostTypeTail, outcome}
	out = append(out, 0, 0, 0, 0, 0, 0) // 6 reserved bytes
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(elapsedMs))
	out = append(out, buf[:]...)
	return out
}

// TestParseFrameworkReadyDatagram_TailEvent pins the type=0x04
// parse path. The wire layout after the type byte is
// fixed-size: [1B outcome][6B reserved][8B BE uint64 elapsed_ms].
// Outcome is the closed enum byte
// {0x01 completed, 0x02 failed, 0x03 timeout}; elapsed_ms is
// the wall-clock duration from waitUntil registration to
// terminal in milliseconds. The reserved 6 bytes are ignored
// in PR 3 (a future wire-level instance_id upgrade uses them).
func TestParseFrameworkReadyDatagram_TailEvent(t *testing.T) {
	type want struct {
		outcome   byte
		elapsedMs int64
		err       bool
		errSub    string
	}
	cases := []struct {
		name string
		body []byte
		want want
	}{
		{
			name: "tail completed (outcome=1, elapsed=0)",
			body: makeTailEvent(tailEventOutcomeCompleted, 0),
			want: want{outcome: tailEventOutcomeCompleted, elapsedMs: 0},
		},
		{
			name: "tail completed (outcome=1, elapsed=3500ms)",
			body: makeTailEvent(tailEventOutcomeCompleted, 3500),
			want: want{outcome: tailEventOutcomeCompleted, elapsedMs: 3500},
		},
		{
			name: "tail failed (outcome=2, elapsed=42ms)",
			body: makeTailEvent(tailEventOutcomeFailed, 42),
			want: want{outcome: tailEventOutcomeFailed, elapsedMs: 42},
		},
		{
			name: "tail timeout (outcome=3, elapsed=30000ms = Pro plan cap)",
			body: makeTailEvent(tailEventOutcomeTimeout, 30000),
			want: want{outcome: tailEventOutcomeTimeout, elapsedMs: 30000},
		},
		{
			name: "tail timeout (outcome=3, elapsed=60000ms = Scale plan cap)",
			body: makeTailEvent(tailEventOutcomeTimeout, 60000),
			want: want{outcome: tailEventOutcomeTimeout, elapsedMs: 60000},
		},
		{
			name: "short read — type only, missing outcome byte",
			// The parser must NOT silently accept a
			// missing outcome byte — that would
			// advance Kind to Tail without populating
			// Outcome, and the dispatch path's closed-set
			// guard would log Warn and drop, but the
			// receipt itself is structurally malformed.
			// Returning an error here surfaces the
			// receipt as a DGRAM-level warn in the recv
			// loop's existing per-DGRAM error handling.
			body: []byte{VsockFrameworkReadyHostTypeTail},
			want: want{err: true, errSub: "missing outcome"},
		},
		{
			name: "short read — type+outcome, missing elapsed_ms",
			// Short-read tolerance: the outcome byte
			// is preserved but elapsed_ms defaults to
			// 0. This matches the design comment in
			// parseFrameworkReadyDatagram's tail arm
			// — a short read means the kernel truncated
			// the DGRAM (which is logged at Debug and
			// dropped via the loop's existing per-DGRAM
			// error handling).
			body: append([]byte{VsockFrameworkReadyHostTypeTail, tailEventOutcomeCompleted},
				0, 0, 0, 0, 0, 0), // 6 reserved, missing elapsed_ms
			want: want{outcome: tailEventOutcomeCompleted, elapsedMs: 0},
		},
		{
			name: "type byte + outcome + reserved (no elapsed_ms)",
			// Same shape as above but with reserved bytes
			// — confirms the parser doesn't accidentally
			// read from the reserved region.
			body: []byte{
				VsockFrameworkReadyHostTypeTail,
				tailEventOutcomeFailed,
				0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF,
			},
			want: want{outcome: tailEventOutcomeFailed, elapsedMs: 0},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseFrameworkReadyDatagram(tc.body)
			if tc.want.err {
				if err == nil {
					t.Fatalf("err = nil, want error containing %q", tc.want.errSub)
				}
				if tc.want.errSub != "" && !strings.Contains(err.Error(), tc.want.errSub) {
					t.Fatalf("err = %q, want contains %q", err.Error(), tc.want.errSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got.Kind != parseFWReadyKindTail {
				t.Errorf("Kind = %d, want parseFWReadyKindTail (%d)", got.Kind, parseFWReadyKindTail)
			}
			if got.Tail.Outcome != tc.want.outcome {
				t.Errorf("Tail.Outcome = 0x%02x, want 0x%02x", got.Tail.Outcome, tc.want.outcome)
			}
			if got.Tail.ElapsedMs != tc.want.elapsedMs {
				t.Errorf("Tail.ElapsedMs = %d, want %d", got.Tail.ElapsedMs, tc.want.elapsedMs)
			}
		})
	}
}

// TestParseFWReadyMsg_TypeLabel_Tail pins the human-readable
// label for the type=0x04 case. The label is used only for
// diagnostic Debug logs (the recv loop logs "framework_ready-scope
// DGRAM for unknown CID" with the type label); a typo here is a
// dashboard-noise bug, not a correctness bug.
func TestParseFWReadyMsg_TypeLabel_Tail(t *testing.T) {
	msg := parseFWReadyMsg{Kind: parseFWReadyKindTail}
	got := msg.TypeLabel()
	want := "tail_event(0x04)"
	if got != want {
		t.Errorf("TypeLabel = %q, want %q", got, want)
	}
}

// TestTailEventOutcome_ClosedSet pins the closed-set outcome
// byte values that guest-init / pkg/fcvm / cmd/vmmd agree on.
// A drift between these constants is a wire-incompatible bug
// — the closed-set guard in dispatchTailEvent surfaces
// unknown bytes at Warn, but the bytes themselves must agree
// across packages.
func TestTailEventOutcome_ClosedSet(t *testing.T) {
	// cmd/vmmd (this file) and pkg/fcvm (manager.go) and
	// guest/init (sidecar_events_proxy_linux.go) must agree
	// on the numeric values. Pin all three.
	type tuple struct {
		name      string
		completed byte
		failed    byte
		timeout   byte
	}
	cmd := tuple{
		name:      "cmd/vmmd",
		completed: tailEventOutcomeCompleted,
		failed:    tailEventOutcomeFailed,
		timeout:   tailEventOutcomeTimeout,
	}
	fcvm_ := tuple{
		name:      "pkg/fcvm",
		completed: byte(fcvm.TailOutcomeCompleted),
		failed:    byte(fcvm.TailOutcomeFailed),
		timeout:   byte(fcvm.TailOutcomeTimeout),
	}
	if cmd.completed != fcvm_.completed ||
		cmd.failed != fcvm_.failed ||
		cmd.timeout != fcvm_.timeout {
		t.Fatalf("cmd/vmmd / pkg/fcvm closed-set drift:\n  cmd = %+v\n  fcvm = %+v", cmd, fcvm_)
	}
	// The numeric values are part of the wire contract; pin
	// them explicitly. A drift from these values is a
	// wire-incompatible change.
	if cmd.completed != 0x01 {
		t.Errorf("completed = 0x%02x, want 0x01 (wire contract)", cmd.completed)
	}
	if cmd.failed != 0x02 {
		t.Errorf("failed = 0x%02x, want 0x02 (wire contract)", cmd.failed)
	}
	if cmd.timeout != 0x03 {
		t.Errorf("timeout = 0x%02x, want 0x03 (wire contract)", cmd.timeout)
	}
}
