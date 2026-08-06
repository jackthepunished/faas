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
	if err := s.AppendUsage(ctx, acctID, appID, ins.ID, minute, 30_720, 1, 0, 0, 0, 0, 0, 0); err != nil {
		t.Fatalf("first append: %v", err)
	}
	// Redelivered minute — a no-op. Different mb_seconds / requests must
	// NOT overwrite the first write.
	if err := s.AppendUsage(ctx, acctID, appID, ins.ID, minute, 99_999, 99, 0, 0, 0, 0, 0, 0); err != nil {
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

	if err := s.AppendUsage(ctx, acctID, appID, ins.ID, m0, 15_840, 1, 0, 0, 0, 0, 0, 0); err != nil {
		t.Fatalf("append m0: %v", err)
	}
	if err := s.AppendUsage(ctx, acctID, appID, ins.ID, m1, 15_840, 2, 0, 0, 0, 0, 0, 0); err != nil {
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
	if err := s.AppendUsage(ctx, acctID, appID, ins.ID, minute, 30_720, 1, 5_000_000, 0, 0, 0, 0, 0); err != nil {
		t.Fatalf("first append: %v", err)
	}
	// Second write: same minute, different billing-floor (must be
	// discarded), additive CPU delta (10_000_000 µs).
	if err := s.AppendUsage(ctx, acctID, appID, ins.ID, minute, 99_999, 99, 10_000_000, 0, 0, 0, 0, 0); err != nil {
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

// TestPg_AppendUsage_AddsTxBytesAndNetTxBytesOnConflict (ADR-046,
// step 11) pins the additive-merge contract for the egress
// columns. Mirrors TestPg_AppendUsage_AddsCpuUsecOnConflict: two
// writes for the same (instance_id, minute) ADD their tx_bytes
// and net_tx_bytes values. The sampler's net_tx_bytes comes
// from vmmd netstats.Cache (250 ms cadence, ~240 ticks per
// minute) and tx_bytes from the gateway ring buffer; both
// sources can fire within the same minute, and the merge must
// be additive. The asymmetry with mb_seconds / requests
// (first-write-wins) is the same ADR-039 §3.2 shape —
// billing-floor metrics stay stable once the minute is
// decided; sampled metrics accumulate across per-tick deltas.
//
// The read-back path uses UsageByMonth (not UsageByHour) to
// pin the usage_monthly view sums the new columns correctly,
// closing the weak assertion the cpu_usec test had at lines
// 70-79 (which silently discarded the read).
func TestPg_AppendUsage_AddsTxBytesAndNetTxBytesOnConflict(t *testing.T) {
	s, ctx := pgStore(t)
	acctID, appID, depID := seedLiveDeploy(t, s, ctx)
	ins, err := s.CreateInstance(ctx, appID, depID, string(state.StateRunning), 512, resolveDefaultLocal(t, ctx, s), "")
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	minute := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

	// First write: tx_bytes=1_000_000 (gateway HTTP response),
	// net_tx_bytes=4_000_000 (vmmd netstats). Both are interface
	// bytes; the units are identical on the wire so a straight
	// additive merge is correct.
	if err := s.AppendUsage(ctx, acctID, appID, ins.ID, minute, 30_720, 1, 0, 1_000_000, 4_000_000, 0, 0, 0); err != nil {
		t.Fatalf("first append: %v", err)
	}
	// Second write: same minute, additional tx_bytes + net_tx_bytes.
	if err := s.AppendUsage(ctx, acctID, appID, ins.ID, minute, 99_999, 99, 0, 2_500_000, 8_000_000, 0, 0, 0); err != nil {
		t.Fatalf("second append: %v", err)
	}

	rows, err := s.UsageByHour(ctx, acctID,
		time.Date(2026, 8, 17, 11, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 17, 13, 0, 0, 0, time.UTC),
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
	// tx_bytes: additive across the conflict.
	if rows[0].TXBytes != 3_500_000 {
		t.Errorf("TXBytes = %d, want 3_500_000 (1M + 2.5M additive merge)", rows[0].TXBytes)
	}
	// net_tx_bytes: additive across the conflict.
	if rows[0].NetTxBytes != 12_000_000 {
		t.Errorf("NetTxBytes = %d, want 12_000_000 (4M + 8M additive merge)", rows[0].NetTxBytes)
	}

	// usage_monthly view (close the weak read-back from the cpu_usec
	// test): sum both columns across all instances for the month.
	monthRows, err := s.UsageByMonth(ctx, acctID,
		time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("UsageByMonth: %v", err)
	}
	if len(monthRows) != 1 {
		t.Fatalf("month rows = %d, want 1", len(monthRows))
	}
	if monthRows[0].TXBytes != 3_500_000 {
		t.Errorf("month TXBytes = %d, want 3_500_000 (view sum)", monthRows[0].TXBytes)
	}
	if monthRows[0].NetTxBytes != 12_000_000 {
		t.Errorf("month NetTxBytes = %d, want 12_000_000 (view sum)", monthRows[0].NetTxBytes)
	}
}

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
		if err := s.AppendUsage(ctx, acctID, appID, ins.ID, minute, 7_680, 1, 0, 0, 0, 0, 0, 0); err != nil {
			if errors.Is(err, state.ErrConflict) {
				t.Fatalf("call %d returned ErrConflict: %v", i, err)
			}
			t.Fatalf("call %d returned error: %v", i, err)
		}
	}
}

// TestPg_AppendUsage_AddsTailSecondsOnConflict pins the additive-merge
// contract for the tail_seconds column (issue #667 / ADR-078). Mirrors
// TestPg_AppendUsage_AddsCpuUsecOnConflict: two writes for the same
// (instance_id, minute) ADD their tail_seconds values while still
// leaving mb_seconds and requests as first-write-wins. The Sampler
// fires ~240 times per minute (250 ms cadence), and the per-tick
// tail_seconds delta is small (a few seconds per tail that drains
// during the tick), so the same (instance_id, minute) row is rewritten
// many times within the minute — the merge must be additive for
// tail_seconds, not idempotent.
//
// The read-back path queries usage_minutes directly because the
// Store.UsageByHour aggregate intentionally does not project
// tail_seconds (the Usage struct carries only the billing-relevant
// columns; tail_seconds is per-day-grain via UsageDaily/DailyUsage,
// not per-hour). tail_seconds is pinned informationally only —
// billing has no dependency on it, so the additive merge is purely
// a metric-fidelity concern; a regression to first-write-wins would
// drop >99% of the cumulative number (240 ticks coalesced to the
// first) and the §12 tail-watchdog panel would mis-report.
func TestPg_AppendUsage_AddsTailSecondsOnConflict(t *testing.T) {
	s, pool, ctx := pgStoreWithPool(t)
	acctID, appID, depID := seedLiveDeploy(t, s, ctx)
	ins, err := s.CreateInstance(ctx, appID, depID, string(state.StateRunning), 512, resolveDefaultLocal(t, ctx, s), "")
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	minute := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

	// First write: 30 tail-seconds (one tail at 30s, or three tails
	// at 10s each — the column is the sum of per-tick deltas).
	if err := s.AppendUsage(ctx, acctID, appID, ins.ID, minute, 30_720, 1, 0, 0, 0, 0, 0, 30); err != nil {
		t.Fatalf("first append: %v", err)
	}
	// Second write: same minute, different billing-floor (must be
	// discarded), additive tail_seconds delta (45 tail-seconds).
	if err := s.AppendUsage(ctx, acctID, appID, ins.ID, minute, 99_999, 99, 0, 0, 0, 0, 0, 45); err != nil {
		t.Fatalf("second append: %v", err)
	}

	// Read tail_seconds directly from the usage_minutes row — the
	// Store.UsageByHour aggregate intentionally does not project
	// tail_seconds (it's per-day-grain via DailyUsage, not per-hour).
	var tailSeconds int64
	if err := pool.QueryRow(ctx,
		`select tail_seconds from usage_minutes where instance_id = $1 and minute = $2`,
		ins.ID, minute,
	).Scan(&tailSeconds); err != nil {
		t.Fatalf("read tail_seconds: %v", err)
	}
	if tailSeconds != 75 {
		t.Errorf("tail_seconds = %d, want 75 (30 + 45 additive merge)", tailSeconds)
	}
}
