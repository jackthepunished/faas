//go:build linux

// Tests for the sidecar events proxy (issue #463 / ADR-069 /
// ADR-071 / PR-C §3,§4). The proxy is the guest-side emit
// point for two DGRAM classes:
//
//   - sidecar_init_exit (type=0x02) — sent from
//     runWorkloads when an init sidecar's supervisor.Run
//     returns (clean: status=init_ok; failure: status=
//     init_failed). AC #1 chain: a failed init fails the
//     deploy with failure_class: user_error.
//
//   - sidecar_restart (type=0x03) — sent from
//     supervisor.OnCrash when an essential sidecar is being
//     restarted (PR-C §4).
//
// Spinning up an AF_VSOCK DGRAM socket requires a Linux
// kernel with vsock loaded (Linux 4.0+) and a CID; the unit
// test cannot construct one without root + a sibling
// AF_VSOCK listener on CID=VMADDR_CID_HOST. We therefore
// drive the wire-format (type-byte + JSON envelope) directly
// via the marshaling helper used inside SendInitExit /
// SendRestart — the same shape the host receiver parses
// (cmd/vmmd/framework_ready_recv.go::parseFrameworkReadyDatagram).
//
// The full AC #1 chain (guest-init orchestrator → vsock DGRAM
// → cmd/vmmd dispatch → deployments.audit row with
// failure_class: user_error) is covered by the metal suite;
// here we pin the wire shape so a future rename is caught
// loudly at the unit level.

package main

import (
	"encoding/json"
	"fmt"
	"testing"
)

// TestSidecarInitExitEnvelope_Shape pins the wire shape for
// type=0x02 (issue #463 / ADR-069 / ADR-071 / PR-C §3). The
// guest proxy sends the JSON envelope after the 1B type
// byte; the host receiver parses it back into the same
// fields. A rename here requires a parallel rename in
// cmd/vmmd/sidecar_events_emit.go::sidecarInitExitWire.
func TestSidecarInitExitEnvelope_Shape(t *testing.T) {
	env := sidecarInitExitEnvelope{
		Sidecar: "audit", Status: "init_failed",
		ExitCode: 137, DurationMs: 250,
	}
	blob, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Round-trip: marshal → unmarshal → equality.
	var back sidecarInitExitEnvelope
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back != env {
		t.Errorf("round-trip mismatch: %+v vs %+v", back, env)
	}
	// Wire framing: type=0x02 + JSON body. The host reads
	// 1B type then json.Unmarshal(rest). A drift here is
	// caught at the first vsock DGRAM the host logs at
	// Warn ("sidecar_init_exit: <err>").
	wire := make([]byte, 1+len(blob))
	wire[0] = VsockSidecarEventsTypeInitExit
	copy(wire[1:], blob)

	if wire[0] != 0x02 {
		t.Errorf("type byte = 0x%02x; want 0x02", wire[0])
	}
	if !contains(string(wire[1:]), `"status":"init_failed"`) {
		t.Errorf("wire lacks init_failed status: %s", string(wire[1:]))
	}
	if !contains(string(wire[1:]), `"exit_code":137`) {
		t.Errorf("wire lacks exit_code: %s", string(wire[1:]))
	}
}

// TestSidecarInitExitEnvelope_OkShape pins the happy-path
// (status=init_ok, exit_code=0). The host's
// dispatchSidecarInitExit recognises the closed enum
// ("init_ok" | "init_failed") and refuses anything else with
// a Warn — PR-C's audit MUST emit exactly one of those two
// statuses per init sidecar.
func TestSidecarInitExitEnvelope_OkShape(t *testing.T) {
	env := sidecarInitExitEnvelope{
		Sidecar: "audit", Status: "init_ok",
		ExitCode: 0, DurationMs: 12,
	}
	blob, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back sidecarInitExitEnvelope
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Status != "init_ok" || back.ExitCode != 0 {
		t.Errorf("ok envelope = %+v; want status=init_ok exit_code=0", back)
	}
}

// TestSidecarRestartEnvelope_Shape pins the wire shape for
// type=0x03 (issue #463 / ADR-069 / ADR-071 / PR-C §4).
// Attempt is the 1-indexed restart number (1 = first restart
// after the initial fork). The supervisor.OnCrash hook
// fires AFTER the supervisor has incremented its restart
// counter, so the first call here is attempt=1.
func TestSidecarRestartEnvelope_Shape(t *testing.T) {
	env := sidecarRestartEnvelope{Sidecar: "metrics", Attempt: 1}
	blob, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back sidecarRestartEnvelope
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back != env {
		t.Errorf("round-trip mismatch: %+v vs %+v", back, env)
	}
	wire := make([]byte, 1+len(blob))
	wire[0] = VsockSidecarEventsTypeRestart
	copy(wire[1:], blob)
	if wire[0] != 0x03 {
		t.Errorf("type byte = 0x%02x; want 0x03", wire[0])
	}
}

// TestSidecarEventsProxy_NilSendDoesNotBlock guards against
// the regression where the proxy fails to come up (bind
// error in boot()) and the orchestrator crashes because the
// receiver is nil. SendInitExit / SendRestart on a nil
// receiver are documented to be no-ops (the "no signal"
// contract) so the production orchestrator never has to nil-
// check.
func TestSidecarEventsProxy_NilSendDoesNotBlock(t *testing.T) {
	var p *sidecarEventsProxy
	if err := p.SendInitExit("audit", "init_ok", 0, 1); err != nil {
		t.Errorf("nil SendInitExit = %v; want nil", err)
	}
	if err := p.SendRestart("metrics", 1); err != nil {
		t.Errorf("nil SendRestart = %v; want nil", err)
	}
}

// TestSidecarEventsTypeConstants pins the closed enum on the
// type byte so a future drift is caught at compile time +
// here. The host receiver (cmd/vmmd/framework_ready_recv.go)
// rejects unknown types with a Warn, so the wire enum is
// load-bearing — adding 0x04 / 0x05 needs a parallel PR.
func TestSidecarEventsTypeConstants(t *testing.T) {
	if VsockSidecarEventsTypeInitExit != 0x02 {
		t.Errorf("TypeInitExit = 0x%02x; want 0x02", VsockSidecarEventsTypeInitExit)
	}
	if VsockSidecarEventsTypeRestart != 0x03 {
		t.Errorf("TypeRestart = 0x%02x; want 0x03", VsockSidecarEventsTypeRestart)
	}
	if VsockSidecarEventsPort != 1027 {
		t.Errorf("Port = %d; want 1027 (must match host)", VsockSidecarEventsPort)
	}
	// Diff vs. the framework_ready type byte. PR #470 typed
	// 0x01 for framework_ready; PR-C extends the closed set
	// by 0x02 and 0x03. None may collide.
	if VsockSidecarEventsTypeInitExit == 0x01 || VsockSidecarEventsTypeRestart == 0x01 {
		t.Errorf("sidecar events types collide with framework_ready type=0x01")
	}
}

// TestInitSidecarEmitsFailureClassUserError_Envelope pins
// the failure-class surface (issue #463 / ADR-069 / ADR-071
// / PR-C §3 acceptance). The full AC #1 chain
// (guest-init orchestrator → vsock DGRAM → cmd/vmmd dispatch
// → deployments.audit row with failure_class: user_error) is
// covered by the metal test TestMetalSidecarInitFailure
// (PR-B); here we pin the wire shape that test consumes so
// a future drift is caught at the unit level.
//
// We exercise the marshaling + framing path the supervisor
// takes through SendInitExit, but assert against the wire
// format itself rather than a full vsock send (the test runs
// on a host without CID=2). What matters: type=0x02 +
// status=init_failed + exit_code=137 (OOM-style) round-trip
// through the wire format.
func TestInitSidecarEmitsFailureClassUserError_Envelope(t *testing.T) {
	// Sidecar failed with status=init_failed + exit 137
	// (OOM, AC #1's user_error anchor). The supervisor's
	// `runErr` wraps an *exec.ExitError with ExitCode()=137.
	// The orchestrator (runWorkloads) catches that and
	// calls SendInitExit("audit", "init_failed", 137, …).
	// We exercise that exact call shape via the envelope
	// producer.
	env := sidecarInitExitEnvelope{
		Sidecar: "audit", Status: "init_failed",
		ExitCode: 137, DurationMs: 2_500,
	}
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Frame: [1B type=0x02][json body]
	wire := append([]byte{VsockSidecarEventsTypeInitExit}, body...)
	msg, perr := parseSidecarInitExitWire(wire)
	if perr != nil {
		t.Fatalf("host parse failed: %v", perr)
	}
	if msg.Sidecar != "audit" {
		t.Errorf("parsed Sidecar = %q; want audit", msg.Sidecar)
	}
	if msg.Status != "init_failed" {
		t.Errorf("parsed Status = %q; want init_failed (AC #1 user_error)", msg.Status)
	}
	if msg.ExitCode != 137 {
		t.Errorf("parsed ExitCode = %d; want 137 (AC #1 user_error)", msg.ExitCode)
	}
	if msg.DurationMs != 2_500 {
		t.Errorf("parsed DurationMs = %d; want 2500", msg.DurationMs)
	}
}

// parseSidecarInitExitWire is the test-local reverse of the
// wire format — mirrors cmd/vmmd/framework_ready_recv.go
// ::parseFWReadyKindInitExit branch exactly. We can't
// import the linux-only framework_ready_recv.go from a test
// that runs on darwin (the test binary won't link), so the
// host parser is duplicated here for the wire-shape
// assertion. The duplication is narrow by design — see
// issue #463 / ADR-069 / ADR-071 / PR-C §3 for the closed
// enum + wire spec.
func parseSidecarInitExitWire(b []byte) (sidecarInitExitEnvelope, error) {
	if len(b) == 0 {
		return sidecarInitExitEnvelope{}, fmt.Errorf("empty body")
	}
	if b[0] != VsockSidecarEventsTypeInitExit {
		return sidecarInitExitEnvelope{}, fmt.Errorf("unknown msg sub-type 0x%02x", b[0])
	}
	var env sidecarInitExitEnvelope
	if err := json.Unmarshal(b[1:], &env); err != nil {
		return sidecarInitExitEnvelope{}, fmt.Errorf("sidecar_init_exit: %w", err)
	}
	return env, nil
}

// contains is a tiny strings.Contains stand-in to avoid an
// extra import in this file. (Tiny perf hit on a 32-byte
// body is irrelevant.)
func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
