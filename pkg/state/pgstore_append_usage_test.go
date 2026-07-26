package state_test

import (
	"errors"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/state"
)

// TestPg_AppendUsage_IdempotentSameMinute pins the production Postgres
// contract for AppendUsage: a second write for the same (instance_id,
// minute) is a no-op. The first call's mb_seconds / requests are preserved;
// the second call returns nil with no error and no row mutation. This is the
// load-bearing fix for the M7 hardening double-bill risk — see the audit in
// the feat/m7-beta-hardening PR description.
func TestPg_AppendUsage_IdempotentSameMinute(t *testing.T) {
	s, ctx := pgStore(t)
	acctID, appID, depID := seedLiveDeploy(t, s, ctx)
	ins, err := s.CreateInstance(ctx, appID, depID, string(state.StateRunning), 512, resolveDefaultLocal(t, ctx, s), "")
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	minute := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

	// First write — wins.
	if err := s.AppendUsage(ctx, acctID, appID, ins.ID, minute, 30_720, 1, 0); err != nil {
		t.Fatalf("first append: %v", err)
	}
	// Redelivered minute — a no-op. Different mb_seconds / requests must
	// NOT overwrite the first write.
	if err := s.AppendUsage(ctx, acctID, appID, ins.ID, minute, 99_999, 99, 0); err != nil {
		t.Fatalf("redelivered append: %v", err)
	}

	// Read back via the per-app, per-hour aggregator — UsageByHour over a
	// window that covers the minute should show ONE row with the FIRST
	// write's values.
	rows, err := s.UsageByHour(ctx, acctID,
		time.Date(2026, 7, 17, 11, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 17, 13, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("UsageByHour: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].MBSeconds != 30_720 {
		t.Fatalf("MBSeconds = %d, want 30_720 (first write wins)", rows[0].MBSeconds)
	}
	if rows[0].Requests != 1 {
		t.Fatalf("Requests = %d, want 1 (first write wins)", rows[0].Requests)
	}
}

// TestPg_AppendUsage_AccumulatesAcrossMinutes confirms that two writes for
// adjacent minutes on the same instance both land — distinct rows whose
// MBSeconds / Requests aggregate. Guards against the idempotency fix being
// too aggressive (collapsing different minutes into one row).
func TestPg_AppendUsage_AccumulatesAcrossMinutes(t *testing.T) {
	s, ctx := pgStore(t)
	acctID, appID, depID := seedLiveDeploy(t, s, ctx)
	ins, err := s.CreateInstance(ctx, appID, depID, string(state.StateRunning), 256, resolveDefaultLocal(t, ctx, s), "")
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	m0 := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	m1 := m0.Add(time.Minute)

	if err := s.AppendUsage(ctx, acctID, appID, ins.ID, m0, 15_840, 1, 0); err != nil {
		t.Fatalf("append m0: %v", err)
	}
	if err := s.AppendUsage(ctx, acctID, appID, ins.ID, m1, 15_840, 2, 0); err != nil {
		t.Fatalf("append m1: %v", err)
	}

	rows, err := s.UsageByHour(ctx, acctID,
		time.Date(2026, 7, 17, 11, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 17, 13, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("UsageByHour: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].MBSeconds != 31_680 {
		t.Fatalf("MBSeconds = %d, want 31_680 (15_840 + 15_840 across two minutes)", rows[0].MBSeconds)
	}
	if rows[0].Requests != 3 {
		t.Fatalf("Requests = %d, want 3 (1 + 2 across two minutes)", rows[0].Requests)
	}
}

// TestPg_AppendUsage_AddsCpuUsecOnConflict pins the additive-merge
// contract for the cpu_usec column (issue #279 / PR-B / ADR-039): two
// writes for the same (instance_id, minute) ADD their cpu_usec values
// while still leaving mb_seconds and requests as first-write-wins.
// The sampler fires ~240 times per minute (250 ms cadence), so the
// same (instance_id, minute) row is rewritten many times within the
// minute — the merge must be additive for cpu_usec, not idempotent.
//
// This is the asymmetry called out in ADR-039 §3.2: mb_seconds is the
// billing-floor metric (stable once the minute is decided), cpu_usec
// is the sampled metric (Σ over the per-tick deltas).
func TestPg_AppendUsage_AddsCpuUsecOnConflict(t *testing.T) {
	s, ctx := pgStore(t)
	acctID, appID, depID := seedLiveDeploy(t, s, ctx)
	ins, err := s.CreateInstance(ctx, appID, depID, string(state.StateRunning), 512, resolveDefaultLocal(t, ctx, s), "")
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	minute := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

	// First write: billing-floor + 5_000_000 µs of CPU work.
	if err := s.AppendUsage(ctx, acctID, appID, ins.ID, minute, 30_720, 1, 5_000_000); err != nil {
		t.Fatalf("first append: %v", err)
	}
	// Second write: same minute, different billing-floor (must be
	// discarded), additive CPU delta (10_000_000 µs).
	if err := s.AppendUsage(ctx, acctID, appID, ins.ID, minute, 99_999, 99, 10_000_000); err != nil {
		t.Fatalf("second append: %v", err)
	}

	rows, err := s.UsageByHour(ctx, acctID,
		time.Date(2026, 7, 17, 11, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 17, 13, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("UsageByHour: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	// mb_seconds / requests: first-wins (the existing contract).
	if rows[0].MBSeconds != 30_720 {
		t.Errorf("MBSeconds = %d, want 30_720 (first write wins)", rows[0].MBSeconds)
	}
	if rows[0].Requests != 1 {
		t.Errorf("Requests = %d, want 1 (first write wins)", rows[0].Requests)
	}
	// cpu_usec: additive across the conflict.
	if rows[0].CPUUsec != 15_000_000 {
		t.Errorf("CPUUsec = %d, want 15_000_000 (5M + 10M additive merge)", rows[0].CPUUsec)
	}
}

// TestPg_AppendUsage_NoUniqueViolationReturned locks down the API contract:
// AppendUsage never surfaces a unique-violation error from the underlying
// ON CONFLICT DO NOTHING. Before the M7 hardening fix this would have leaked
// a `state.ErrConflict`-mappable error on every redelivered minute; today
// every call returns nil regardless of collision.
func TestPg_AppendUsage_NoUniqueViolationReturned(t *testing.T) {
	s, ctx := pgStore(t)
	acctID, appID, depID := seedLiveDeploy(t, s, ctx)
	ins, err := s.CreateInstance(ctx, appID, depID, string(state.StateRunning), 128, resolveDefaultLocal(t, ctx, s), "")
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	minute := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

	// 50 same-minute writes — every one must succeed and not surface ErrConflict.
	for i := 0; i < 50; i++ {
		if err := s.AppendUsage(ctx, acctID, appID, ins.ID, minute, 7_680, 1, 0); err != nil {
			if errors.Is(err, state.ErrConflict) {
				t.Fatalf("call %d returned ErrConflict: %v", i, err)
			}
			t.Fatalf("call %d returned error: %v", i, err)
		}
	}
}
