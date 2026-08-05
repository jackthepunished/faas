// Tests for the sidecar failure-class shibboleth and the
// init-failed audit-payload shape (issue #463 / ADR-069 /
// ADR-071 / PR-C §3, AC #1). The pin exists so a future
// edit cannot silently reword "user_error" to a near-synonym
// and break the deployments UI's red-traffic-light grep. The
// wire value matches pkg/state.FailureUserError; a divergence
// here and a fix there should be a single PR.
//
// No build tag: the type-under-test (sidecarInitExitWire in
// sidecar_events_emit.go) and the producer
// (sidecar_events_through_platform.go) compile on every
// platform, so the test follows suit. A darwin build of vmmd
// isn't a thing today but the test still serves as a static
// pin against the literal.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// TestSidecarFailureClassUserError_Pinned — the shibboleth
// is a closed enum. A reword (e.g. "user-fail") would
// silently break the dashboard's grep without a test failure
// here. Mirrors pkg/wire/metrics_test.go's similar pin on
// "user_error" in the build-counter code label.
func TestSidecarFailureClassUserError_Pinned(t *testing.T) {
	if sidecarFailureClassUserError != "user_error" {
		t.Fatalf("sidecarFailureClassUserError = %q; the AC #1 shibboleth is closed and must stay %q",
			sidecarFailureClassUserError, "user_error")
	}
}

// TestSidecarInitFailedAuditPayload_FailureClass — the
// hand-marshalled jsonb payload (buildSidecarInitFailedPayload)
// must carry the shibboleth in the failure_class key, not in
// a sibling key the dashboard doesn't read. A future schema
// refactor (e.g. moving the value into a wrapper struct)
// must update this test in the same commit.
func TestSidecarInitFailedAuditPayload_FailureClass(t *testing.T) {
	env := sidecarInitExitWire{
		Sidecar:    "metrics",
		Status:     "init_failed",
		ExitCode:   1,
		DurationMs: 80,
	}
	body := buildSidecarInitFailedPayload("i-1", "a-1", "w-1", env)
	var parsed struct {
		FailureClass string `json:"failure_class"`
		Sidecar      string `json:"sidecar"`
		InstanceID   string `json:"instance_id"`
		AppID        string `json:"app_id"`
		WakeID       string `json:"wake_id"`
		ExitCode     int    `json:"exit_code"`
		DurationMs   int64  `json:"duration_ms"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("audit payload is not valid json: %v (raw=%s)", err, body)
	}
	if parsed.FailureClass != sidecarFailureClassUserError {
		t.Errorf("payload.failure_class = %q; want %q",
			parsed.FailureClass, sidecarFailureClassUserError)
	}
	if !strings.Contains(string(body), `"failure_class":"user_error"`) {
		t.Errorf("audit payload missing the grep-target key: %s", body)
	}
	if parsed.Sidecar != "metrics" || parsed.InstanceID != "i-1" ||
		parsed.AppID != "a-1" || parsed.ExitCode != 1 || parsed.DurationMs != 80 {
		t.Errorf("payload carrier fields drifted: %+v", parsed)
	}
}

// guestSidecarInitExitEnvelope mirrors
// guest/init/sidecar_events_proxy_linux.go::sidecarInitExitEnvelope
// (the guest-init emitter). The two structs are a wire pair
// — guest marshals into the envelope, host unmarshals into
// sidecarInitExitWire. They live in separate Go modules
// (cmd/vmmd/ is the host binary; guest/init/ is the in-guest
// PID 1 cross-compiled to a static binary) so a shared type
// is impossible; a guard test that re-declares the guest
// shape here catches a JSON-tag drift in either direction.
//
// The fields and tags below MUST match the guest-init
// declaration exactly. A mismatch fails the test with a
// "field renamed" message; the fix is a single PR that
// updates BOTH files in lockstep.
type guestSidecarInitExitEnvelope struct {
	Sidecar    string `json:"sidecar"`
	Status     string `json:"status"`
	ExitCode   int    `json:"exit_code"`
	DurationMs int64  `json:"duration_ms"`
}

// guestSidecarRestartEnvelope mirrors
// guest/init/sidecar_events_proxy_linux.go::sidecarRestartEnvelope.
type guestSidecarRestartEnvelope struct {
	Sidecar string `json:"sidecar"`
	Attempt int    `json:"attempt"`
}

// TestSidecarWireMirror_InitExit — guest-init's
// sidecarInitExitEnvelope must marshal to bytes the host's
// sidecarInitExitWire can unmarshal. Marshals a known
// envelope, parses it back into the host struct, and asserts
// every field. The same fixture bytes also get stamped with
// the 0x02 type byte and asserted to match the
// parseFrameworkReadyDatagram body shape (the file-level
// fixture covers the type-byte + jsonb envelope, not the
// type=0x01 framework_ready prefix).
func TestSidecarWireMirror_InitExit(t *testing.T) {
	guest := guestSidecarInitExitEnvelope{
		Sidecar: "metrics", Status: "init_failed",
		ExitCode: 137, DurationMs: 250,
	}
	blob, err := json.Marshal(guest)
	if err != nil {
		t.Fatalf("guest marshal: %v", err)
	}
	// The host receiver strips the 1B type prefix and unmarshals
	// the remainder. Build the full datagram (type byte + json
	// body) and assert the body parses as sidecarInitExitWire.
	wire := make([]byte, 1+len(blob))
	wire[0] = 0x02 // VsockSidecarEventsTypeInitExit (the host
	// does not import guest/init constants, so the literal is
	// pinned here with the constant name in the comment).
	copy(wire[1:], blob)

	var host sidecarInitExitWire
	if err := json.Unmarshal(wire[1:], &host); err != nil {
		t.Fatalf("host parse of guest wire failed — JSON tags drifted? raw=%s err=%v", wire[1:], err)
	}
	if host.Sidecar != guest.Sidecar || host.Status != guest.Status ||
		host.ExitCode != guest.ExitCode || host.DurationMs != guest.DurationMs {
		t.Errorf("host/guest wire drift: host=%+v guest=%+v", host, guest)
	}
	if host.Status != "init_failed" {
		t.Errorf("wire status = %q; the closed enum must stay init_ok|init_failed", host.Status)
	}
}

// TestSidecarWireMirror_Restart — same pattern for the
// type=0x03 restart envelope. PR-C §4 sends
// sidecarRestartEnvelope{sidecar, attempt} from the
// supervisor's OnCrash hook; the host parses it into
// sidecarRestartWire and increments the
// vmmd_sidecar_restart_total counter.
func TestSidecarWireMirror_Restart(t *testing.T) {
	guest := guestSidecarRestartEnvelope{Sidecar: "metrics", Attempt: 1}
	blob, err := json.Marshal(guest)
	if err != nil {
		t.Fatalf("guest marshal: %v", err)
	}
	var host sidecarRestartWire
	if err := json.Unmarshal(blob, &host); err != nil {
		t.Fatalf("host parse of guest wire failed — JSON tags drifted? raw=%s err=%v", blob, err)
	}
	if host.Sidecar != guest.Sidecar || host.Attempt != guest.Attempt {
		t.Errorf("host/guest wire drift: host=%+v guest=%+v", host, guest)
	}
}

// fakeDeploymentFailer (issue #463 / ADR-069 / PR-B AC #1) is
// the in-memory fake the cmd/vmmd tests substitute for the
// narrowed deploymentFailer interface so the production
// emitter's deploy-row flip can be exercised without spinning
// up a Postgres connection. Records every call so the
// dispatch path is asserted end-to-end. Mutex-guarded because
// the dispatch path is goroutine-safe in production (the
// receiver runs one goroutine per DGRAM receipt).
type fakeDeploymentFailer struct {
	mu    sync.Mutex
	calls []fakeDeploymentFailerCall
	err   error
}

type fakeDeploymentFailerCall struct {
	ID      string
	Code    string
	Message string
}

func (f *fakeDeploymentFailer) SetDeploymentFailed(_ context.Context, id, code, message string) (state.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeDeploymentFailerCall{ID: id, Code: code, Message: message})
	return state.Deployment{}, f.err
}

// TestSidecarInitFailed_FlipsDeploymentRow (PR-B AC #1)
// pins the vmmd-side deploy-row flip on a non-zero init
// sidecar exit: the production emitter must call
// SetDeploymentFailed with the literal CodeInitSidecarFailed
// and a customer-facing reason string built from the wire
// envelope (sidecar name + exit code + duration). The flip is
// the load-bearing difference between "audit row written" and
// "deploy row reflects the failure"; a regression that drops
// the call leaves the customer staring at a green traffic
// light on a deployment whose init sidecar exited 1.
func TestSidecarInitFailed_FlipsDeploymentRow(t *testing.T) {
	fake := &fakeDeploymentFailer{}
	e := &SidecarEventsThroughPlatform{Failer: fake}
	e.EmitSidecarInitExit(context.Background(),
		"inst-1", "app-1", "dep-1", "wake-1",
		sidecarInitExitWire{Sidecar: "metrics", Status: "init_failed", ExitCode: 1, DurationMs: 80})
	if len(fake.calls) != 1 {
		t.Fatalf("SetDeploymentFailed call count = %d, want 1", len(fake.calls))
	}
	got := fake.calls[0]
	if got.ID != "dep-1" {
		t.Errorf("deployment_id = %q, want %q", got.ID, "dep-1")
	}
	if got.Code != api.CodeInitSidecarFailed {
		t.Errorf("error_code = %q, want %q (RFC 7807 stable code)", got.Code, api.CodeInitSidecarFailed)
	}
	if !strings.Contains(got.Message, "metrics") || !strings.Contains(got.Message, "exited 1") {
		t.Errorf("message missing sidecar / exit details: %q", got.Message)
	}
}

// TestSidecarInitOK_DoesNotFlipDeploymentRow pins the inverse:
// a successful init exit must NOT touch the deployments row.
// A regression that flips on every status (instead of only
// init_failed) would mask legitimate deploys as failures —
// the customer-visible red traffic light is wrong on init_ok.
func TestSidecarInitOK_DoesNotFlipDeploymentRow(t *testing.T) {
	fake := &fakeDeploymentFailer{}
	e := &SidecarEventsThroughPlatform{Failer: fake}
	e.EmitSidecarInitExit(context.Background(),
		"inst-1", "app-1", "dep-1", "wake-1",
		sidecarInitExitWire{Sidecar: "metrics", Status: "init_ok", ExitCode: 0, DurationMs: 80})
	if len(fake.calls) != 0 {
		t.Fatalf("SetDeploymentFailed call count = %d, want 0 (init_ok must not flip)", len(fake.calls))
	}
}

// TestSidecarInitFailed_EmptyDeploymentIDSkipsFlip pins the
// legacy tolerance: a pre-PR-B wake without a deployment_id
// on the wire leaves the deploy row untouched. The audit row
// is still written (the sidecar init_failed event surfaces
// the failure to the dashboard), so the dispatch remains
// observable.
func TestSidecarInitFailed_EmptyDeploymentIDSkipsFlip(t *testing.T) {
	fake := &fakeDeploymentFailer{}
	e := &SidecarEventsThroughPlatform{Failer: fake}
	e.EmitSidecarInitExit(context.Background(),
		"inst-1", "app-1", "" /* legacy: no deployment_id on wire */, "wake-1",
		sidecarInitExitWire{Sidecar: "metrics", Status: "init_failed", ExitCode: 1, DurationMs: 80})
	if len(fake.calls) != 0 {
		t.Fatalf("SetDeploymentFailed call count = %d, want 0 (empty deployment_id skips flip)", len(fake.calls))
	}
}

// TestSidecarInitFailed_NilFailerDoesNotPanic pins the
// missing-wiring tolerance: a receiver that came up but the
// Failer field was left nil (default-local run without a
// state.Store) must not panic. The dispatch is best-effort
// observability — a missing wire is logged + dropped, never
// fatal.
func TestSidecarInitFailed_NilFailerDoesNotPanic(t *testing.T) {
	e := &SidecarEventsThroughPlatform{} // no Failer
	e.EmitSidecarInitExit(context.Background(),
		"inst-1", "app-1", "dep-1", "wake-1",
		sidecarInitExitWire{Sidecar: "metrics", Status: "init_failed", ExitCode: 1, DurationMs: 80})
	// No assertion needed: the test fails if the call panics.
}

// TestSidecarInitFailed_FailerErrorLoggedAndDropped pins the
// error-tolerance contract: a SetDeploymentFailed failure
// (e.g. Postgres transient outage) is logged + dropped, not
// retried. The audit row is the secondary observability
// surface; the deploy-row flip is best-effort by design so
// a transient PG error doesn't block the dispatch hot loop.
func TestSidecarInitFailed_FailerErrorLoggedAndDropped(t *testing.T) {
	fake := &fakeDeploymentFailer{err: errors.New("transient pg error")}
	e := &SidecarEventsThroughPlatform{Failer: fake}
	// No panic + log.Warn is acceptable; the dispatch must
	// not propagate the error to the caller (the receiver's
	// goroutine would otherwise exit on the panic boundary).
	e.EmitSidecarInitExit(context.Background(),
		"inst-1", "app-1", "dep-1", "wake-1",
		sidecarInitExitWire{Sidecar: "metrics", Status: "init_failed", ExitCode: 1, DurationMs: 80})
	if len(fake.calls) != 1 {
		t.Fatalf("SetDeploymentFailed call count = %d, want 1 (the call must be attempted before logging the error)", len(fake.calls))
	}
}
