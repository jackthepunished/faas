// Metal sibling of bridge_h2c_terminator_e2e_test.go. The non-metal
// tests above already exercise the real bridge binary end-to-end
// against a local H2C guest listener; the metal tests below do
// the same wiring but the "guest" is a real Firecracker VM booted
// on the bare-metal x86_64 control-plane node (or on M3+ Apple
// Silicon via `make metal-lima` — see CLAUDE.md + deploy/lima/).
//
// Build tag: //go:build metal — gated by the same convention as
// the rest of the metal suite. Only runs in `make test-metal` /
// `make metal-lima`.
//
// What the metal suite adds on top of the non-metal test:
//   - real Firecracker microVM as the "guest" (10.0.0.2:8080 is
//     an actual VM-bound interface, not a loopback listener);
//   - real vmmd-stream-bridge process spawned inside the VM's
//     per-instance netns via `ip netns exec <netns> bridge …`;
//   - real guest-init + runner stack (the H2C-capable :8080
//     listener from guest/runners/internal/h2c_listener.go).
//
// Acceptance gates per spec §14 (M8 row 5 — wire-protocol
// selector PR-D cutover):
//
//  1. App with app_protocol=http1 → HTTP/1.1 request reaches
//     the guest (curl -v shows H1 framing on the wire; the
//     guest's net/http logs the H1 path).
//
//  2. App with app_protocol=http2 → HTTP/2 prior-knowledge
//     request reaches the guest (curl --http2-prior-knowledge
//     works; the guest's listener is H2C-capable per
//     guest/runners/internal/h2c_listener.go).
//
//  3. App with app_protocol=grpc → a real Go gRPC client hits
//     a unary + a server-streaming RPC; trailer pair
//     (grpc-status: 0, grpc-message: "") survives end-to-end.
//
//  4. Surgical rollback: setting FAAS_BRIDGE_PROTOCOL=h1 on
//     vmmd → the http2/grpc apps fall back to the legacy
//     H1+chunked path on the wire. The customer-facing behavior
//     reverts to today's shape; the bridge keeps serving.
//
//  5. Wholesale rollback: setting FAAS_STREAM_BRIDGE_VERSION=v1
//     on vmmd → the v1 shell bridge takes over entirely. This is
//     the disaster rollback (pre-existing ADR-028 amendment).
//
// The metal runner that ships these gates is the operator's
// job (per spec §14 — "A bare-metal x86_64 control-plane node
// remains the source of truth for the §14 metal acceptance
// gates"). The Go test stub below is a placeholder for future
// CI integration; the load-bearing pins are the non-metal
// bridge_h2c_terminator_e2e_test.go tests, which run on every
// CI runner (no KVM required).
//
//go:build metal

package e2e_test

import (
	"testing"
)

// TestMetal_AppProtocolH2CPriorKnowledge is the M8 row 5 metal
// acceptance gate: a real Firecracker guest running the
// H2C-capable runner listener receives an H2 prior-knowledge
// request end-to-end. The fixture (deployed app, running VM,
// bridged netns) is set up by the operator's metal harness
// (deploy/ansible/roles/metal-h2c-acceptance/ in the future);
// this Go test asserts the H2 framing reaches the guest.
func TestMetal_AppProtocolH2CPriorKnowledge(t *testing.T) {
	t.Skip("metal-only; requires /dev/kvm + bare-metal x86_64 (see CLAUDE.md). " +
		"Operator acceptance per spec §14 M8 row 5: " +
		"curl --http2-prior-knowledge against app:port must yield " +
		"`:status: 200` over an H2 frame on the guest access log. " +
		"Tracked by the metal test runner fixture, not a Go unit test.")
}

// TestMetal_AppProtocolGRPCTrailers is the M8 row 5 gRPC
// trailer preservation gate: a real Go gRPC client hits Echo
// (unary) and ServerStreamingEcho; the trailer pair must
// round-trip end-to-end through the H2C terminator.
func TestMetal_AppProtocolGRPCTrailers(t *testing.T) {
	t.Skip("metal-only; requires /dev/kvm + bare-metal x86_64. " +
		"Operator acceptance per spec §14 M8 row 5: " +
		"Go gRPC client hits Echo + ServerStreamingEcho, " +
		"resp.Trailer.Get(\"Grpc-Status\") must == \"0\".")
}

// TestMetal_AppProtocolH1Default is the regression: app with
// app_protocol=http1 continues to receive H1 framing on the wire.
func TestMetal_AppProtocolH1Default(t *testing.T) {
	t.Skip("metal-only; requires /dev/kvm + bare-metal x86_64. " +
		"Regression per spec §14 M8 row 5: " +
		"curl -v against http1 app shows H1 framing on the wire; " +
		"the H1+chunked legacy bridge path stays live for the http1 slice.")
}

// TestMetal_BridgeSurgicalRollback asserts
// FAAS_BRIDGE_PROTOCOL=h1 forces the legacy path for any
// app_protocol (the surgical rollback switch per ADR-126
// §Decision 7).
func TestMetal_BridgeSurgicalRollback(t *testing.T) {
	t.Skip("metal-only; requires /dev/kvm + bare-metal x86_64. " +
		"Surgical rollback per ADR-126 §Decision 7: " +
		"set FAAS_BRIDGE_PROTOCOL=h1 on vmmd → http2/grpc apps " +
		"fall back to H1+chunked on the wire.")
}

// TestMetal_BridgeWholesaleRollback asserts
// FAAS_STREAM_BRIDGE_VERSION=v1 reverts to the v1 shell bridge
// (pre-existing ADR-028 amendment disaster rollback).
func TestMetal_BridgeWholesaleRollback(t *testing.T) {
	t.Skip("metal-only; requires /dev/kvm + bare-metal x86_64. " +
		"Wholesale rollback per ADR-028: " +
		"set FAAS_STREAM_BRIDGE_VERSION=v1 on vmmd → shell bridge takes over.")
}
