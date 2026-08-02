package events

import (
	"testing"
	"time"
)

// TestWakeEvent_AllKindsImplementInterface — compile-time check
// that every payload struct in wake.go implements the WakeEvent
// interface. A new struct that forgets a method fails the
// compilation, which is the cheapest schema validator the package
// can ship.
func TestWakeEvent_AllKindsImplementInterface(t *testing.T) {
	now := time.Now()
	acct := "acct-1"
	_ = acct
	// Touch each struct so the linter + compiler can't drop the
	// interface check; the bind is through the WakeEvent type
	// assertion. If any payload struct drifts from the interface
	// (a missing method, a Kind() string drift), this fails to
	// compile.
	var _ WakeEvent = QueueAccepted{EmitAt: now, WakeID: "w", AppID: "a", RequestID: "r"}
	var _ WakeEvent = Admitted{EmitAt: now, WakeID: "w", AppID: "a", RequestID: "r", AccountID: "acct-1", Plan: "hobby"}
	var _ WakeEvent = BootStarted{EmitAt: now, WakeID: "w", AppID: "a", InstanceID: "i", NodeID: "n", Method: "cold_boot"}
	var _ WakeEvent = BootCompleted{EmitAt: now, WakeID: "w", AppID: "a", InstanceID: "i", NodeID: "n", Method: "cold_boot"}
	var _ WakeEvent = BootFailed{EmitAt: now, WakeID: "w", AppID: "a", InstanceID: "i", NodeID: "n", Method: "cold_boot", Reason: "stub"}
	var _ WakeEvent = Readiness200{EmitAt: now, WakeID: "w", AppID: "a", InstanceID: "i", NodeID: "n", HealthcheckPath: "/healthz", ProbeCount: 1, ElapsedMs: 50}
	var _ WakeEvent = ProxyFirstByte{EmitAt: now, WakeID: "w", AppID: "a", RequestID: "r", InstanceID: "i", NodeID: "n", LatencyMs: 12}
	var _ WakeEvent = ParkStarted{EmitAt: now, WakeID: "w", AppID: "a", InstanceID: "i", NodeID: "n"}
	var _ WakeEvent = ParkCompleted{EmitAt: now, WakeID: "w", AppID: "a", InstanceID: "i", NodeID: "n", SnapshotID: "s-1"}
	var _ WakeEvent = Stalled{EmitAt: now, WakeID: "w", AppID: "a", InstanceID: "i", NodeID: "n", Reason: "watchdog"}
	var _ WakeEvent = BuildSucceeded{EmitAt: now, AppID: "a", DeploymentID: "d", ImageDigest: "sha256:abc", DurationMs: 12000}
	var _ WakeEvent = BuildFailed{EmitAt: now, AppID: "a", DeploymentID: "d", ImageDigest: "sha256:abc", Reason: "compile"}
	var _ WakeEvent = DeployFailed{EmitAt: now, AppID: "a", DeploymentID: "d", Reason: "scan"}
}

// TestQueueAccepted_Shape — the payload keys are the wire
// contract; the customer-facing JSON surfaces {"wake_id": ...}.
// Catch a typo'd key here before it ships.
func TestQueueAccepted_Shape(t *testing.T) {
	ev := QueueAccepted{EmitAt: time.Unix(0, 0).UTC(), WakeID: "w-1", AppID: "a-1", RequestID: "r-1", QueueWaitMs: 42}
	if got := ev.Kind(); got != WakeQueueAccepted {
		t.Errorf("Kind = %q, want %q", got, WakeQueueAccepted)
	}
	if got := ev.Payload()["wake_id"]; got != "w-1" {
		t.Errorf("payload.wake_id = %v, want w-1", got)
	}
	if got := ev.Payload()["queue_wait_ms"]; got != int64(42) {
		t.Errorf("payload.queue_wait_ms = %v, want 42", got)
	}
	if got := ev.Subject(); got != nil {
		t.Errorf("Subject = %v, want nil", got)
	}
}

// TestAdmitted_ShapeOnAccountID — Subject() returns the account_id
// pointer so the audit row's subject column is populated; the
// Empty-string case collapses to nil (system event).
func TestAdmitted_ShapeOnAccountID(t *testing.T) {
	ev := Admitted{EmitAt: time.Unix(0, 0).UTC(), WakeID: "w-1", AppID: "a-1", AccountID: "acct-1", Plan: "hobby"}
	if got := ev.Subject(); got == nil || *got != "acct-1" {
		t.Errorf("Subject = %v, want pointer to acct-1", got)
	}
	empty := Admitted{EmitAt: time.Unix(0, 0).UTC()}
	if got := empty.Subject(); got != nil {
		t.Errorf("Empty AccountID Subject = %v, want nil", got)
	}
}

// TestStalled_WakeIDPreserved — the wake_id is the join key the
// customer-facing endpoint uses (GET
// /v1/apps/{slug}/wakes/{wake_id}/timeline). Catch a payload field
// rename that drops wake_id.
func TestStalled_WakeIDPreserved(t *testing.T) {
	ev := Stalled{WakeID: "w-123", AppID: "a-1", InstanceID: "i-1", NodeID: "n-1", Reason: "watchdog"}
	if got := ev.Payload()["wake_id"]; got != "w-123" {
		t.Errorf("payload.wake_id = %v, want w-123", got)
	}
	if got := ev.Payload()["reason"]; got != "watchdog" {
		t.Errorf("payload.reason = %v, want watchdog", got)
	}
}

// TestWakePhaseFromKind — the helper that strips the `wake.`
// prefix to feed the per-phase metric label. Catches typos.
func TestWakePhaseFromKind(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"wake.boot_started", "boot_started"},
		{"wake.readiness_200", "readiness_200"},
		{"wake.proxy_first_byte", "proxy_first_byte"},
		{"legacy", "legacy"}, // bare names pass through unchanged
	}
	for _, c := range cases {
		if got := wakePhaseFromKind(c.in); got != c.want {
			t.Errorf("wakePhaseFromKind(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
