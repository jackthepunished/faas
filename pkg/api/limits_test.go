package api

import (
	"testing"
	"time"
)

// TestPlanLimitsMatchSpec pins every value in the table to the financial-model /
// spec §1 numbers. If the spreadsheet moves, this test must be updated in the
// same PR — that is the point.
func TestPlanLimitsMatchSpec(t *testing.T) {
	want := map[Plan]Limits{
		// Move 1: Free gates async_invoke and queues (spec §4.4 paid-only).
		// EgressAllowlistAllowed/MaxSize default to false/0 (Go zero), so
		// Free/Hobby rows below omit them intentionally — mirrors the
		// MinInstancesAllowed row shape.
		PlanFree: {Plan: PlanFree, DeployedApps: 1, MaxConcurrency: 1, RAMMB: 128, AppLayerMaxMB: 256, SourceTarballMaxMB: 100, VCPU: 2, IdleTimeoutS: 30, IncludedGBHours: 5, PriceMillicents: 0, RateLimitRPS: 5, RateLimitBurst: 20, EgressMbit: 10, SecretCountMax: 3, SecretValueMaxBytes: 4096, MaxMinInstances: 0,
			// Issue #559: Free = 1 (single-concurrency plan — one VM
			// serves one request at a time; mirrors MaxConcurrency).
			ConcurrencyPerVMBound: 1,
			// Issue #395 / ADR-045: Free gets 8 keys / 4 KB per value.
			EnvVarsMax: 8, EnvValueMaxBytes: 4096,
			// ADR-044: per-plan CPUWeight/CPUQuotaUS/CPUPeriodUS — issue
			// #301 acceptance #1+#2. The 2/4/8/16 ratio is the literal
			// value from the issue; the quota is the spec's literal
			// "100ms/100ms, 200ms/100ms, 500ms/100ms, 1000ms/100ms".
			CPUWeight: 2, CPUQuotaUS: 100_000, CPUPeriodUS: 100_000,
			MaxQueueDepth: 0, MaxDelayedTasksPerApp: 0, MaxSourceBytesPerInvocation: 0, AsyncInvokeAllowed: false,
			// Issue #394: Free is gated out of queues entirely (spec §4.4
			// paid-only), so MaxQueueAttempts is moot — 0 matches the
			// "feature not offered" contract.
			MaxQueueAttempts: 0,
			// Cron (spec §4.4 paid-only): Free has no crons at all. Handler
			// returns 402 ErrPlanCronsNotAllowed before the store is touched.
			CronLimitPerApp: 0, CronLimitPerAccount: 0,
			// Issue #475: Free is gated off the reserved eviction tier.
			// Fail-closed at 0/0 mirrors the cron 0/0 posture above.
			EvictionPriorityReservedAllowed: false, ReservedConcurrencyPerAccount: 0,
			// IAM-6 / ADR-061: PR 1 placeholder. Fail-closed at 0/0
			// until the financial model authorizes values for PR 2.
			OrgMembersMax: 0, OrgPendingInvitationsMax: 0,
			// ADR-045 (#396): alert rules — Free gated to 402, so the limits
			// surface is 0/0 to fail-closed by default.
			AlertRuleLimitPerApp: 0, AlertRuleLimitPerAccount: 0,
			// ADR-040: Free gets 50/min — covers the 1-concurrency plan's
			// traffic envelope with a 50× burst ceiling.
			RateLimitPerAccountRPM: 50,
			// Issue #471 / ADR-047 (PR-A): Free is gated out of streaming
			// entirely. The 25 MiB / 300 s caps are the legacy pre-#471
			// defaults — kept here so a Free customer that PATCHes
			// streaming_enabled=false sees the same envelope they'd have
			// seen before the streaming patch landed. MaxResponseBodyBytes
			// (25 MiB) and ResponseWriteTimeoutSeconds (300 s) are the
			// pre-#471 spec §4.1 caps PR-A inherits.
			StreamingEnabled: false, MaxResponseBodyBytes: 26_214_400, ResponseWriteTimeoutSeconds: 300,
			// Issue #461 / ADR-062: Free has no private-registry
			// credential surface (handler returns 403
			// plan_registry_credentials_not_allowed).
			RegistryCredentialMax: 0,
			// Issue #470 / ADR-055: Free is gated off for warm-tier
			// snapshots — doubling the per-app parked footprint
			// doesn't fit the Free pricing tier. The 0/0 defaults
			// are defence-in-depth; the WarmSnapshotAllowed() gate
			// surfaces the 403 to a Free customer PATCHing true.
			WarmSnapshotEnabled: false, WarmSnapshotMinRequestsDefault: 0, WarmSnapshotMinMsDefault: 0,
			// Issue #560: Free is gated off for require_authn
			// — opt-in is a paid-tier feature (Cloud Run's
			// `--no-allow-unauthenticated` shape).
			RequireAuthn: false,
			// Issue #189 / IAM-5: Free = 3 keys (primary deploy + staging + break-glass).
			KeysMax: 3,
			// Issue #667 / ADR-078: tail primitive on with floor timeout.
			TailEnabled: true, TailTimeoutS: 5, TailCapMax: 16, ConcurrentTailsPerInstance: 4},
		PlanHobby: {Plan: PlanHobby, DeployedApps: 5, MaxConcurrency: 2, RAMMB: 256, AppLayerMaxMB: 512, SourceTarballMaxMB: 100, VCPU: 2, IdleTimeoutS: 60, IncludedGBHours: 50, PriceMillicents: 900_000, RateLimitRPS: 20, RateLimitBurst: 100, EgressMbit: 25, SecretCountMax: 25, SecretValueMaxBytes: 8192, MaxMinInstances: 1,
			// Issue #559: Hobby = 5 (smallest paid tier — one Node
			// event loop comfortably handles 5 concurrent requests).
			ConcurrencyPerVMBound: 5,
			// Issue #472 / ADR-058: Hobby gets 4 trusted publishers — covers the
			// typical CI rotation surface (GitHub Actions + GitLab + Jenkins +
			// in-house) without letting one app accumulate an unbounded allowlist.
			TrustedSignerCountMax: 4,
			// Issue #395 / ADR-045: Hobby gets 32 keys / 8 KB per value.
			EnvVarsMax: 32, EnvValueMaxBytes: 8192,
			// ADR-044: see PlanFree. Hobby's tight quota is the
			// load-bearing signal in the cpu-fairness e2e (cmd/e2e/cpu_fairness_test.go).
			CPUWeight: 4, CPUQuotaUS: 200_000, CPUPeriodUS: 200_000,
			MaxQueueDepth: 5, MaxDelayedTasksPerApp: 5, MaxSourceBytesPerInvocation: 64 * 1024, AsyncInvokeAllowed: true,
			// Issue #394: Hobby gets 3 retries before dead-letter — a
			// poisoned row exits within ~15s at the default 5s backoff
			// without thrashing the worker for long.
			MaxQueueAttempts: 3,
			// Issue #462 / ADR-058 / PR-A: Hobby unlocks the warm
			// floor (MinInstancesAllowed) and the max_instances
			// ceiling (MaxInstancesAllowed). Hobby is still
			// gated out of the autoscale_target_rps /
			// autoscale_target_cpu_pct knobs (the cost shape
			// rationale is unchanged). The bill auto-counts
			// (pkg/meter/sampler.go:238-239) so the warm floor
			// has a bounded cost.
			MinInstancesAllowed: true, MaxInstancesAllowed: true,
			// Issue #169 / #172: Hobby is gated on Pro+ for both RPS
			// and CPU (2026-07-28: ADR-037 amendment — Hobby→Pro re-tier
			// on ScaleUpTargetRPSAllowed). CPU-driven scaling is gated
			// on Pro+ for cost reasons.
			ScaleUpTargetRPSAllowed: false, ScaleUpTargetCPUAllowed: false,
			// Cron: Hobby gets 5 per-app and 10 per-account.
			CronLimitPerApp: 5, CronLimitPerAccount: 10,
			// Issue #475: Hobby gets 1 reserved-tier app.
			EvictionPriorityReservedAllowed: true, ReservedConcurrencyPerAccount: 1,
			// IAM-6 / ADR-061: PR 1 placeholder — 0/0 until the
			// financial model authorizes values.
			OrgMembersMax: 0, OrgPendingInvitationsMax: 0,
			// ADR-045 (#396): Hobby gets 3 per-app and 10 per-account.
			AlertRuleLimitPerApp: 3, AlertRuleLimitPerAccount: 10,
			// ADR-040: Hobby gets 200/min — ~10× the per-app rps (20),
			// so the per-app limit trips first on a single hot app and
			// the account limit catches the cross-app botnet signature.
			RateLimitPerAccountRPM: 200,
			// Issue #471 / ADR-047 (PR-A): Hobby unlocks streaming
			// (100 MiB / 900 s) — the first paid tier. PR-A wires
			// the flag + accessor; PR-B activates the Flusher path.
			StreamingEnabled: true, MaxResponseBodyBytes: 104_857_600, ResponseWriteTimeoutSeconds: 900,
			// Issue #517 / PR-B: Hobby unlocks the `?deployment=`
			// filter for the typical one-staging-deployment workload.
			LogDeploymentFilterMax: 1,
			// Issue #461 / ADR-064: Hobby = 2 — staging + production.
			RegistryCredentialMax: 2,
			// Issue #470 / ADR-055: Hobby is gated off for the
			// same cost-shape reason as Free — the doubled parked
			// footprint doesn't fit the €9/month Hobby tier.
			WarmSnapshotEnabled: false, WarmSnapshotMinRequestsDefault: 0, WarmSnapshotMinMsDefault: 0,
			// Issue #560: Hobby is gated off for the same
			// posture-change shape as Free.
			RequireAuthn: false,
			// Issue #189 / IAM-5: Hobby = 10 keys (2 per app across 5 apps).
			KeysMax: 10,
			// Issue #667 / ADR-078: tail primitive on at 15 s.
			TailEnabled: true, TailTimeoutS: 15, TailCapMax: 16, ConcurrentTailsPerInstance: 16},
		// ADR-031: Pro opt-in for per-app egress allowlist with a 16-CIDR cap.
		PlanPro: {Plan: PlanPro, DeployedApps: 25, MaxConcurrency: 5, RAMMB: 512, AppLayerMaxMB: 1024, SourceTarballMaxMB: 250, VCPU: 2, IdleTimeoutS: 300, IncludedGBHours: 250, PriceMillicents: 2_900_000, RateLimitRPS: 100, RateLimitBurst: 500, EgressMbit: 100, SecretCountMax: 50, SecretValueMaxBytes: 16384, MaxMinInstances: 3,
			// Issue #559: Pro = 25 (typical SaaS-tier workload
			// envelope — one Node/Python service handling fan-out).
			ConcurrencyPerVMBound: 25,
			// Issue #472 / ADR-058: Pro gets 8 trusted publishers — 2× Hobby for the
			// larger team rotation surface (multiple repos × multiple CI providers).
			TrustedSignerCountMax: 8,
			// Issue #395 / ADR-045: Pro gets 64 keys / 16 KB per value.
			EnvVarsMax: 64, EnvValueMaxBytes: 16384,
			// Issue #462 / ADR-058: Pro unlocks warm-floor + max-instances
			// ceiling (was min-instances only at the pre-#462 contract).
			MinInstancesAllowed: true, MaxInstancesAllowed: true,
			// ADR-044: see PlanFree.
			CPUWeight: 8, CPUQuotaUS: 500_000, CPUPeriodUS: 500_000,
			MaxQueueDepth: 25, MaxDelayedTasksPerApp: 50, MaxSourceBytesPerInvocation: 256 * 1024, AsyncInvokeAllowed: true,
			// Issue #394: Pro gets 10 retries — 5× Hobby's budget.
			// Tolerates a transient downstream flap while still bounding
			// the "permanently bad payload" worker cost.
			MaxQueueAttempts:       10,
			EgressAllowlistAllowed: true, EgressAllowlistMaxSize: 16,
			// Issue #169 / #172: Pro unlocks both RPS and CPU targets.
			ScaleUpTargetRPSAllowed: true, ScaleUpTargetCPUAllowed: true,
			// Cron: Pro gets 20 per-app and 50 per-account.
			CronLimitPerApp: 20, CronLimitPerAccount: 50,
			// Issue #475: Pro gets 2 reserved-tier apps.
			EvictionPriorityReservedAllowed: true, ReservedConcurrencyPerAccount: 2,
			// IAM-6 / ADR-061: PR 1 placeholder — 0/0 until the
			// financial model authorizes values.
			OrgMembersMax: 0, OrgPendingInvitationsMax: 0,
			// ADR-045 (#396): Pro gets 10 per-app and 30 per-account.
			AlertRuleLimitPerApp: 10, AlertRuleLimitPerAccount: 30,
			// ADR-040: Pro gets 1000/min — ~10× the per-app rps (100).
			RateLimitPerAccountRPM: 1000,
			// Issue #471 / ADR-047 (PR-A): Pro keeps the same streaming
			// envelope as Hobby. The cap is the same; the per-app
			// streaming path is gatewayd-edged, not per-tier.
			StreamingEnabled: true, MaxResponseBodyBytes: 104_857_600, ResponseWriteTimeoutSeconds: 900,
			// Issue #517 / PR-B: Pro gets 10 — covers the typical
			// multi-staging fan-out (prod + 3-5 staging + a few
			// preview slots) without monopolising the schedd's
			// per-instance goroutine fan-out.
			LogDeploymentFilterMax: 10,
			// Issue #461 / ADR-064: Pro = 5 — multi-region + CI shapes.
			RegistryCredentialMax: 5,
			// Issue #470 / ADR-055: Pro is the first tier where
			// warm-tier snapshots are on by default — 5 requests /
			// 2000 ms is the sweet spot for the issue's acceptance
			// (p50 halved vs init-tier).
			WarmSnapshotEnabled: true, WarmSnapshotMinRequestsDefault: 5, WarmSnapshotMinMsDefault: 2000,
			// Issue #560: Pro is the first tier where the
			// per-app require_authn opt-in unlocks.
			RequireAuthn: true,
			// Issue #189 / IAM-5: Pro = 50 keys (2 per app across 25 apps).
			KeysMax: 50,
			// Issue #667 / ADR-078: tail primitive on at 30 s.
			TailEnabled: true, TailTimeoutS: 30, TailCapMax: 16, ConcurrentTailsPerInstance: 64},
		// ADR-031: Scale double-up to 64 CIDR cap (2× Pro, tracks 2×
		// DeployedApps).
		PlanScale: {Plan: PlanScale, DeployedApps: 100, MaxConcurrency: 20, RAMMB: 1024, AppLayerMaxMB: 2048, SourceTarballMaxMB: 250, VCPU: 4, IdleTimeoutS: 600, IncludedGBHours: 1500, PriceMillicents: 9_900_000, RateLimitRPS: 500, RateLimitBurst: 2000, EgressMbit: 250, SecretCountMax: 100, SecretValueMaxBytes: 32768, MaxMinInstances: 10,
			// Issue #559: Scale = 80 (matches Cloud Run's
			// `80 × vCPU` default per the issue body).
			ConcurrencyPerVMBound: 80,
			// Issue #472 / ADR-058: Scale gets 16 trusted publishers — 2× Pro for the
			// enterprise rotation surface (multi-team, multi-cloud, multi-CI).
			TrustedSignerCountMax: 16,
			// Issue #395 / ADR-045: Scale gets 256 keys / 32 KB per value.
			EnvVarsMax: 256, EnvValueMaxBytes: 32768,
			// Issue #462 / ADR-058: Scale unlocks warm-floor +
			// max-instances ceiling (same as Pro).
			MinInstancesAllowed: true, MaxInstancesAllowed: true,
			// ADR-044: see PlanFree. Scale's 1000ms/100ms quota is the
			// upper bound — 10 vCPU worth of compute at the per-instance
			// level, gated by the §1 56 GB hard fence at the slice level.
			CPUWeight: 16, CPUQuotaUS: 1_000_000, CPUPeriodUS: 1_000_000,
			MaxQueueDepth: 100, MaxDelayedTasksPerApp: 1_000_000, MaxSourceBytesPerInvocation: 1024 * 1024, AsyncInvokeAllowed: true,
			// Issue #394: Scale gets 25 retries — 2.5× Pro's budget, but
			// capped so a genuinely-bad payload still terminates within
			// the worker's hourly budget window.
			MaxQueueAttempts:       25,
			EgressAllowlistAllowed: true, EgressAllowlistMaxSize: 64,
			// Issue #169 / #172: Scale unlocks both targets (same rationale as Pro).
			ScaleUpTargetRPSAllowed: true, ScaleUpTargetCPUAllowed: true,
			// Cron: Scale gets 100 per-app and 500 per-account.
			CronLimitPerApp: 100, CronLimitPerAccount: 500,
			// Issue #475: Scale gets 4 reserved-tier apps.
			EvictionPriorityReservedAllowed: true, ReservedConcurrencyPerAccount: 4,
			// IAM-6 / ADR-061: PR 1 placeholder — 0/0 until the
			// financial model authorizes values.
			OrgMembersMax: 0, OrgPendingInvitationsMax: 0,
			// ADR-045 (#396): Scale gets 25 per-app and 100 per-account.
			AlertRuleLimitPerApp: 25, AlertRuleLimitPerAccount: 100,
			// ADR-040: Scale gets 5000/min — ~10× the per-app rps (500).
			// The fleet-summed alert at 100/min/5m (FaasPerAccountRateLimitSpike)
			// triggers well before any single paid customer's bucket fills.
			RateLimitPerAccountRPM: 5000,
			// Issue #471 / ADR-047 (PR-A): Scale keeps the same envelope
			// as Hobby/Pro. The streaming cap is uniform across paid
			// tiers — the spec's paid-only unlock is the boolean, not
			// the byte/time ceiling.
			StreamingEnabled: true, MaxResponseBodyBytes: 104_857_600, ResponseWriteTimeoutSeconds: 900,
			// Issue #517 / PR-B: Scale gets 50 — 5× Pro, tracks the
			// larger app budget (100 vs 25) and multi-region staging
			// fan-out SaaS-scale customers typically run.
			LogDeploymentFilterMax: 50,
			// Issue #461 / ADR-064: Scale = 20 — broad fan-out.
			RegistryCredentialMax: 20,
			// Issue #470 / ADR-055: Scale stays on by default —
			// the per-app parked cost fits inside the 452 GB
			// budget and the wake-p50 win is the largest dollar
			// lever for SaaS workloads.
			WarmSnapshotEnabled: true, WarmSnapshotMinRequestsDefault: 5, WarmSnapshotMinMsDefault: 2000,
			// Issue #560: Scale mirrors Pro — opt-in
			// available, column default still false.
			RequireAuthn: true,
			// Issue #189 / IAM-5: Scale = 200 keys (2 per app across 100 apps).
			KeysMax: 200,
			// Issue #667 / ADR-078: tail primitive on at 60 s.
			TailEnabled: true, TailTimeoutS: 60, TailCapMax: 16, ConcurrentTailsPerInstance: 256},
	}
	for _, p := range Plans {
		got := MustLimitsFor(p)
		if got != want[p] {
			t.Errorf("limits for %s:\n got  %+v\n want %+v", p, got, want[p])
		}
	}
}

// TestOrgMembersLimits_ZeroUntilAuthorised pins the fail-closed
// contract for the IAM-6 / ADR-061 org caps (issue #190, PR 1).
// The handler gates membership creation on Plan.OrgMembersMax()
// and the store reads the same value as a defence-in-depth
// back-stop. Until the financial model authorizes per-plan values,
// every plan must read 0/0 — a missing row must NEVER silently
// inherit a permissive cap. PR 2 will populate actual values
// alongside the schema work; this test catches a regression where
// the field is left at Go's default zero (which happens to be 0)
// and the accessor is omitted (no reader = silent allow).
func TestOrgMembersLimits_ZeroUntilAuthorised(t *testing.T) {
	for _, p := range Plans {
		t.Run(string(p), func(t *testing.T) {
			if got := p.OrgMembersMax(); got != 0 {
				t.Errorf("Plan(%s).OrgMembersMax() = %d, want 0 (fail-closed until financial model authorises)", p, got)
			}
			if got := p.OrgPendingInvitationsMax(); got != 0 {
				t.Errorf("Plan(%s).OrgPendingInvitationsMax() = %d, want 0 (fail-closed until financial model authorises)", p, got)
			}
		})
	}

	// Unknown plan must also fail closed (return 0). Mirrors the
	// CronLimitPerApp / AlertRuleLimitPerApp contract.
	if got := Plan("enterprise").OrgMembersMax(); got != 0 {
		t.Errorf("Plan(unknown).OrgMembersMax() = %d, want 0 (fail-closed)", got)
	}
	if got := Plan("enterprise").OrgPendingInvitationsMax(); got != 0 {
		t.Errorf("Plan(unknown).OrgPendingInvitationsMax() = %d, want 0 (fail-closed)", got)
	}
}

// TestOrgAccessorsMatchTable pins that the accessor methods read the
// same value the Limits struct holds. Catches regressions where a
// future contributor edits the struct field but forgets the accessor
// (or vice versa). For PR 1 both fields are 0/0, but the
// relationship must be stable — when PR 2 adds real values, this
// test catches asymmetric drift.
func TestOrgAccessorsMatchTable(t *testing.T) {
	for _, p := range Plans {
		l := MustLimitsFor(p)
		if got, want := p.OrgMembersMax(), l.OrgMembersMax; got != want {
			t.Errorf("Plan(%s).OrgMembersMax() = %d, table = %d", p, got, want)
		}
		if got, want := p.OrgPendingInvitationsMax(), l.OrgPendingInvitationsMax; got != want {
			t.Errorf("Plan(%s).OrgPendingInvitationsMax() = %d, table = %d", p, got, want)
		}
	}
}

func TestPlansTableCoverage(t *testing.T) {
	if len(Plans) != len(planLimits) {
		t.Fatalf("Plans list (%d) and planLimits table (%d) out of sync", len(Plans), len(planLimits))
	}
	for _, p := range Plans {
		if _, ok := planLimits[p]; !ok {
			t.Errorf("plan %s in Plans but missing from planLimits", p)
		}
	}
}

// TestAdmissionCeilingIs85Percent guards the headroom invariant (§6.2-2): schedd
// admits to 85% of the 56 GB tenant budget.
func TestAdmissionCeilingIs85Percent(t *testing.T) {
	// 0.85 * 56000 = 47600 exactly. Do the check in integers to avoid floats.
	if got := TenantRAMBudgetMB * 85 / 100; got != RAMAdmissionCeilingMB {
		t.Errorf("RAMAdmissionCeilingMB = %d, want 85%% of %d = %d", RAMAdmissionCeilingMB, TenantRAMBudgetMB, got)
	}
	if RAMAdmissionCeilingMB >= TenantSliceMaxMB {
		t.Errorf("admission ceiling %d must sit below the hard slice fence %d", RAMAdmissionCeilingMB, TenantSliceMaxMB)
	}
}

// TestDefaultComputeNodeCeilingMB pins the helper that the synthetic
// default-local row (pkg/state/memstore.go) and the vmmd LoadConfig
// default (cmd/vmmd/config.go) both consume. Today it delegates to
// RAMAdmissionCeilingMB; the test catches any future drift between
// the helper and the constant.
//
// Two assertions cover two regressions:
//   - the value-pinning literal at 47_600 catches a future contributor
//     changing RAMAdmissionCeilingMB without updating the helper, OR
//     changing the helper's body to a non-delegating expression;
//   - the headroom check (helper == 85% of TenantRAMBudgetMB) is the
//     invariant underlying both values, so a regression in either
//     constant alone still surfaces with a targeted message instead
//     of the value-pin's hard-coded number.
//
// PR scale-out readiness #4 callers (memstore seed + vmmd default)
// are pinned in their own test sites so a regression localised to
// the helper surfaces here, not at the production call sites.
func TestDefaultComputeNodeCeilingMB(t *testing.T) {
	const want = 47_600
	if got := DefaultComputeNodeCeilingMB(); got != want {
		t.Errorf("DefaultComputeNodeCeilingMB() = %d, want %d (platform baseline pin)", got, want)
	}
	if got := TenantRAMBudgetMB * 85 / 100; got != DefaultComputeNodeCeilingMB() {
		t.Errorf("DefaultComputeNodeCeilingMB() = %d, want 85%% of %d = %d (headroom invariant)",
			DefaultComputeNodeCeilingMB(), TenantRAMBudgetMB, got)
	}
}

// TestPlansAreMonotonic asserts every quota grows (or holds) from Free→Scale, so
// an upgrade never reduces a customer's allowance.
func TestPlansAreMonotonic(t *testing.T) {
	for i := 1; i < len(Plans); i++ {
		lo := MustLimitsFor(Plans[i-1])
		hi := MustLimitsFor(Plans[i])
		checks := []struct {
			name   string
			lo, hi int
		}{
			{"DeployedApps", lo.DeployedApps, hi.DeployedApps},
			{"MaxConcurrency", lo.MaxConcurrency, hi.MaxConcurrency},
			{"RAMMB", lo.RAMMB, hi.RAMMB},
			{"AppLayerMaxMB", lo.AppLayerMaxMB, hi.AppLayerMaxMB},
			{"IncludedGBHours", lo.IncludedGBHours, hi.IncludedGBHours},
			{"IdleTimeoutS", lo.IdleTimeoutS, hi.IdleTimeoutS},
			{"RateLimitRPS", lo.RateLimitRPS, hi.RateLimitRPS},
			{"EgressMbit", lo.EgressMbit, hi.EgressMbit},
			{"CronLimitPerApp", lo.CronLimitPerApp, hi.CronLimitPerApp},
			{"CronLimitPerAccount", lo.CronLimitPerAccount, hi.CronLimitPerAccount},
			// Issue #475: per-account reserved-tier cap must be
			// monotonic across plans (Free 0 < Hobby 1 < Pro 2 < Scale 4).
			// apid's updateApp path reads this directly.
			{"ReservedConcurrencyPerAccount", lo.ReservedConcurrencyPerAccount, hi.ReservedConcurrencyPerAccount},
			// Issue #395 / ADR-045: env quota must be monotonic like every
			// other gate — Free's 8 < Hobby's 32 < Pro's 64 < Scale's 256,
			// and the per-value byte cap doubles each step.
			{"EnvVarsMax", lo.EnvVarsMax, hi.EnvVarsMax},
			{"EnvValueMaxBytes", lo.EnvValueMaxBytes, hi.EnvValueMaxBytes},
			// Issue #461 / ADR-062: per-app registry credential quota
			// (Free=0 → Hobby=2 → Pro=5 → Scale=20).
			{"RegistryCredentialMax", lo.RegistryCredentialMax, hi.RegistryCredentialMax},
			// Issue #189 / IAM-5: per-account API-key quota
			// (Free=3 → Hobby=10 → Pro=50 → Scale=200).
			{"KeysMax", lo.KeysMax, hi.KeysMax},
			// Issue #559: per-VM concurrency bound must grow with
			// plan (Free=1 → Hobby=5 → Pro=25 → Scale=80). Mirrors
			// MaxConcurrency's monotonicity because a customer's
			// concurrency ceiling should never shrink on upgrade.
			{"ConcurrencyPerVMBound", lo.ConcurrencyPerVMBound, hi.ConcurrencyPerVMBound},
		}
		for _, c := range checks {
			if c.hi < c.lo {
				t.Errorf("%s not monotonic: %s=%d < %s=%d", c.name, Plans[i], c.hi, Plans[i-1], c.lo)
			}
		}
		if hi.PriceMillicents < lo.PriceMillicents {
			t.Errorf("price not monotonic: %s=%d < %s=%d", Plans[i], hi.PriceMillicents, Plans[i-1], lo.PriceMillicents)
		}
	}
}

func TestAdmissionMB(t *testing.T) {
	for _, p := range Plans {
		l := MustLimitsFor(p)
		if got, want := l.AdmissionMB(), l.RAMMB+PerVMOverheadMB; got != want {
			t.Errorf("%s AdmissionMB()=%d want %d", p, got, want)
		}
	}
}

func TestIdleTimeoutBounds(t *testing.T) {
	l := MustLimitsFor(PlanPro) // default 300s
	floor, ceiling := l.IdleTimeoutBounds()
	if floor != IdleTimeoutFloorSeconds {
		t.Errorf("floor=%d want %d", floor, IdleTimeoutFloorSeconds)
	}
	if ceiling != 600 {
		t.Errorf("ceiling=%d want 600 (300 * %d)", ceiling, IdleTimeoutMaxMultiple)
	}
}

func TestPlanValidity(t *testing.T) {
	for _, p := range Plans {
		if !p.Valid() {
			t.Errorf("plan %s should be valid", p)
		}
	}
	if Plan("enterprise").Valid() {
		t.Error(`"enterprise" should not be a valid plan`)
	}
	if Plan("").Valid() {
		t.Error("empty plan should not be valid")
	}
	if _, ok := LimitsFor(Plan("nope")); ok {
		t.Error("LimitsFor unknown plan should return ok=false")
	}
}

func TestMustLimitsForPanicsOnUnknown(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("MustLimitsFor should panic on unknown plan")
		}
	}()
	MustLimitsFor(Plan("nope"))
}

// TestPlanMinInstancesAllowed pins the per-plan gate that apid's
// updateApp handler uses for ux_spec §6.5. Free → false (always
// scale to zero); Hobby/Pro/Scale → true (issue #462 / ADR-058
// PR-A tier-up). Unknown plans must default to false (fail-closed:
// a missing plan never silently unlocks a premium feature).
//
// PR-A history (2026-07-31): Hobby was previously false (the
// pre-#462 contract). The Hobby+ tier-up landed because the bill
// auto-counts via pkg/meter/sampler.go:238-239 and Hobby's
// MaxConcurrency is bounded (2) so the worst-case residency cost
// is 2 × RAMMB + 16 MB overhead.
func TestPlanMinInstancesAllowed(t *testing.T) {
	cases := []struct {
		plan Plan
		want bool
	}{
		{PlanFree, false},
		{PlanHobby, true},
		{PlanPro, true},
		{PlanScale, true},
		{Plan("unknown"), false},
	}
	for _, c := range cases {
		if got := c.plan.MinInstancesAllowed(); got != c.want {
			t.Errorf("%s.MinInstancesAllowed() = %v, want %v", c.plan, got, c.want)
		}
	}
}

// TestSidecarCapMax pins the global constant (issue #463 / ADR-066
// §Decision 1). The 2-sidecar hard cap is a GLOBAL const, not a
// per-plan matrix field — a future PR may grow this to a per-plan
// matrix if telemetry shows demand, but for PR-A every plan
// inherits the same 2-cap. The companion schema CHECK on
// `deployments.sidecars` (migration 00095) is the second-line
// defence — see migrations/00095_deployments_sidecars_test.go.
func TestSidecarCapMax(t *testing.T) {
	if SidecarCapMax != 2 {
		t.Errorf("SidecarCapMax = %d, want 2 (issue #463 / ADR-066 §Decision 1)", SidecarCapMax)
	}
}

// TestPlanMaxMinInstances pins the per-plan max-floor cap (issue #557
// / ADR-071 §Decision 5). Free 0, Hobby 1, Pro 3, Scale 10. The cap
// is tighter than MaxConcurrency (1/2/5/20) to protect the §6.2-2
// RAM ceiling from a single API call. Unknown plans fail closed
// (return 0) — same contract as TrustedSignerCountMax.
func TestPlanMaxMinInstances(t *testing.T) {
	cases := []struct {
		plan Plan
		want int
	}{
		{PlanFree, 0},
		{PlanHobby, 1},
		{PlanPro, 3},
		{PlanScale, 10},
		{Plan("unknown"), 0},
	}
	for _, c := range cases {
		if got := c.plan.MaxMinInstances(); got != c.want {
			t.Errorf("%s.MaxMinInstances() = %d, want %d", c.plan, got, c.want)
		}
	}
}

// TestPlanSidecarAllowed pins the per-plan accessor (issue #463 /
// ADR-066 §Decision 1). PR-A's accessor returns true for every
// plan — the load-bearing gate is the GLOBAL `SidecarCapMax`
// constant, not a per-plan matrix. The accessor exists so a future
// per-plan gate (Free = 0, paid = 2) can be wired in one place
// without the apid handler branching on Plan strings. Mirrors
// TestPlanMinInstancesAllowed above.
func TestPlanSidecarAllowed(t *testing.T) {
	for _, p := range []Plan{PlanFree, PlanHobby, PlanPro, PlanScale} {
		if !p.SidecarAllowed() {
			t.Errorf("%s.SidecarAllowed() = false; PR-A returns true for all plans (global cap is the load-bearing gate)", p)
		}
	}
}

// TestBillableRAMMBWithSidecars pins the sidecar-shape billing
// math (issue #463 / ADR-066 §Decision 6). The billable shutter
// is `plan.RAMMB + Σ(sidecar.ram_mb) + PerVMOverheadMB`: sidecars
// share the per-VM overhead (one netns, one cgroup scope per
// instance), but each sidecar contributes its own RAM. PR-A
// defines the math; PR-B wires the consumer (schedd's admission
// ledger + meterd's sampler).
func TestBillableRAMMBWithSidecars(t *testing.T) {
	cases := []struct {
		name       string
		planRAM    int
		sidecarMBs []int
		want       int
	}{
		// No sidecars: matches BillableRAMMB exactly.
		{"no-sidecars", 256, nil, 256 + PerVMOverheadMB},
		// One init: 256 + 64 + 8 = 328.
		{"one-init-64", 256, []int{64}, 256 + 64 + PerVMOverheadMB},
		// Two sidecars: 256 + 64 + 32 + 8 = 360.
		{"two-sidecars", 256, []int{64, 32}, 256 + 64 + 32 + PerVMOverheadMB},
		// Empty sidecarMBs slice is the no-sidecars shape.
		{"empty-slice", 256, []int{}, 256 + PerVMOverheadMB},
		// Zero in the slice is a "absent / inherit" sentinel — skipped
		// by the helper (the apid handler normalises ram_mb=0 → absent
		// at validation time, but the helper is defensive anyway).
		{"zero-skipped", 256, []int{0, 64}, 256 + 64 + PerVMOverheadMB},
		// Scale shape: 1024 + 64 + 64 + 8 = 1160 (matches ADR-066
		// §Financial-model addendum scenario column).
		{"scale-two-sidecars", 1024, []int{64, 64}, 1024 + 64 + 64 + PerVMOverheadMB},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := BillableRAMMBWithSidecars(c.planRAM, c.sidecarMBs)
			if got != c.want {
				t.Errorf("BillableRAMMBWithSidecars(%d, %v) = %d, want %d", c.planRAM, c.sidecarMBs, got, c.want)
			}
		})
	}
}

// TestPlanConcurrencyPerVMBound pins the platform-advertised per-VM
// concurrency bound (issue #559). Free 1, Hobby 5, Pro 25, Scale 80.
// Distinct from MaxConcurrency (the per-app instance cap, free=1 /
// hobby=2 / pro=5 / scale=20) — this is per-VM. Surfaced on GET
// /v1/apps/{slug} as concurrency_per_vm so dashboards + CLI can
// render the bound without reading limits.go. Unknown plans fail
// closed (return 0) — same contract as MaxMinInstances.
func TestPlanConcurrencyPerVMBound(t *testing.T) {
	cases := []struct {
		plan Plan
		want int
	}{
		{PlanFree, 1},
		{PlanHobby, 5},
		{PlanPro, 25},
		{PlanScale, 80},
		{Plan("unknown"), 0},
	}
	for _, c := range cases {
		if got := c.plan.ConcurrencyPerVMBound(); got != c.want {
			t.Errorf("%s.ConcurrencyPerVMBound() = %d, want %d", c.plan, got, c.want)
		}
	}
}

// TestConcurrencyPerVMBoundAccessorMatchesTable pins that the
// accessor reads the same value the Limits struct holds. Mirrors
// TestOrgAccessorsMatchTable above — catches regressions where a
// future contributor edits one side but forgets the other.
func TestConcurrencyPerVMBoundAccessorMatchesTable(t *testing.T) {
	for _, p := range Plans {
		l := MustLimitsFor(p)
		if got, want := p.ConcurrencyPerVMBound(), l.ConcurrencyPerVMBound; got != want {
			t.Errorf("Plan(%s).ConcurrencyPerVMBound() = %d, table = %d", p, got, want)
		}
	}
}

// TestPlanScaleUpTargetRPSAllowed pins the per-plan gate that apid's
// updateApp handler uses for the per-app autoscale_target_rps field
// (issue #172, ADR-037). Free/Hobby → false (Hobby lost the gate
// via the 2026-07-28 Hobby→Pro re-tier — ADR-037 amendment); Pro/Scale
// → true. Unknown plans must default to false (fail-closed: a missing
// plan never silently unlocks a premium feature). Mirrors
// TestPlanMinInstancesAllowed above.
func TestPlanScaleUpTargetRPSAllowed(t *testing.T) {
	cases := []struct {
		plan Plan
		want bool
	}{
		{PlanFree, false},
		{PlanHobby, false},
		{PlanPro, true},
		{PlanScale, true},
		{Plan("unknown"), false},
	}
	for _, c := range cases {
		if got := c.plan.ScaleUpTargetRPSAllowed(); got != c.want {
			t.Errorf("%s.ScaleUpTargetRPSAllowed() = %v, want %v", c.plan, got, c.want)
		}
	}
}

// TestPlanEgressAllowlistAllowed pins the per-plan gate that apid's
// updateApp handler uses for the per-app egress allowlist (ADR-031).
// Free/Hobby → false (no allowlist — abuse-desk hygiene is a Pro+
// concern; the default scale-to-zero tenant never sees this surface);
// Pro/Scale → true. Unknown plans must default to false (fail-closed
// — same contract as MinInstancesAllowed above).
func TestPlanEgressAllowlistAllowed(t *testing.T) {
	cases := []struct {
		plan Plan
		want bool
	}{
		{PlanFree, false},
		{PlanHobby, false},
		{PlanPro, true},
		{PlanScale, true},
		{Plan("unknown"), false},
	}
	for _, c := range cases {
		if got := c.plan.EgressAllowlistAllowed(); got != c.want {
			t.Errorf("%s.EgressAllowlistAllowed() = %v, want %v", c.plan, got, c.want)
		}
	}
}

// TestPlanEgressAllowlistMaxSize pins the per-plan CIDR cap (ADR-031).
// Free/Hobby → 0 (no allowlist slot, the gate above rejects the
// PATCH before this matters); Pro → 16; Scale → 64.
func TestPlanEgressAllowlistMaxSize(t *testing.T) {
	cases := []struct {
		plan Plan
		want int
	}{
		{PlanFree, 0},
		{PlanHobby, 0},
		{PlanPro, 16},
		{PlanScale, 64},
	}
	for _, c := range cases {
		if got := c.plan.EgressAllowlistMaxSize(); got != c.want {
			t.Errorf("%s.EgressAllowlistMaxSize() = %d, want %d", c.plan, got, c.want)
		}
	}
}

// TestPlanEgressAllowlistMonotonic pins the Pro→Scale ordering so a
// future bump that flips the ratio (e.g. Scale 32 < Pro 64) is caught
// here. Mirrors the TestPlansAreMonotonic style — Pro MaxSize must be
// ≤ Scale MaxSize because Scale is the bigger tier.
func TestPlanEgressAllowlistMonotonic(t *testing.T) {
	pro := MustLimitsFor(PlanPro).EgressAllowlistMaxSize
	scale := MustLimitsFor(PlanScale).EgressAllowlistMaxSize
	if scale < pro {
		t.Errorf("Scale EgressAllowlistMaxSize=%d < Pro=%d — Scale must keep the larger CIDR budget", scale, pro)
	}
}

// TestPlanCronLimits pins the cron cap per plan (spec §4.4). Free is
// 0/0 (handler returns 402 before the store is touched); Hobby gets
// 5 per-app / 10 per-account; Pro 20/50; Scale 100/500. Unknown plans
// must fail closed (return 0) so a missing row never silently unlocks
// crons — same contract as EgressAllowlistMaxSize above.
// TestPlanLogDeploymentFilterMax pins the per-plan cap on the
// `?deployment=` log-stream filter (issue #517 / PR-B, AC3). Free
// returns 0 so the handler rejects with
// `plan_deployment_filter_not_allowed`; Hobby unlocks the filter for
// the typical one-staging-deployment workload; Pro/Scale get the
// larger caps the multi-deployment fan-out needs. Unknown plans
// must fail closed (return 0) so a missing row never silently
// unlocks a paid feature — same contract as CronLimitPerApp.
func TestPlanLogDeploymentFilterMax(t *testing.T) {
	cases := []struct {
		plan Plan
		want int
	}{
		{PlanFree, 0},
		{PlanHobby, 1},
		{PlanPro, 10},
		{PlanScale, 50},
		{Plan("unknown"), 0},
	}
	for _, c := range cases {
		if got := c.plan.LogDeploymentFilterMax(); got != c.want {
			t.Errorf("%s.LogDeploymentFilterMax() = %d, want %d", c.plan, got, c.want)
		}
	}
}

func TestPlanCronLimits(t *testing.T) {
	cases := []struct {
		plan                    Plan
		wantPerApp, wantPerAcct int
	}{
		{PlanFree, 0, 0},
		{PlanHobby, 5, 10},
		{PlanPro, 20, 50},
		{PlanScale, 100, 500},
		{Plan("unknown"), 0, 0},
	}
	for _, c := range cases {
		if got := c.plan.CronLimitPerApp(); got != c.wantPerApp {
			t.Errorf("%s.CronLimitPerApp() = %d, want %d", c.plan, got, c.wantPerApp)
		}
		if got := c.plan.CronLimitPerAccount(); got != c.wantPerAcct {
			t.Errorf("%s.CronLimitPerAccount() = %d, want %d", c.plan, got, c.wantPerAcct)
		}
	}
}

// TestPlanKeysMax pins the per-account API-key cap for the plan
// (issue #189 / IAM-5). Free 3, Hobby 10, Pro 50, Scale 200 — see
// pkg/api/limits.go::KeysMax docstring. apid's createKey handler
// reads this value (via Plan.KeysMax()) and rejects with 409
// api_key_limit_exceeded at the cap; rotateKey is quota-neutral
// and is allowed at the cap. Unknown plans must fail closed (return 0)
// so a missing plan row never silently unlocks the auth surface — same
// contract as CronLimitPerAccount above.
func TestPlanKeysMax(t *testing.T) {
	cases := []struct {
		plan Plan
		want int
	}{
		{PlanFree, 3},
		{PlanHobby, 10},
		{PlanPro, 50},
		{PlanScale, 200},
		{Plan("unknown"), 0},
	}
	for _, c := range cases {
		if got := c.plan.KeysMax(); got != c.want {
			t.Errorf("%s.KeysMax() = %d, want %d", c.plan, got, c.want)
		}
	}
}

// TestPlanKeysMaxAccessorsMatchTable pins that the accessor reads the
// same value the Limits struct holds. Catches a regression where a
// future contributor edits the struct field but forgets the accessor
// (or vice versa). Mirrors TestOrgAccessorsMatchTable.
func TestPlanKeysMaxAccessorsMatchTable(t *testing.T) {
	for _, p := range Plans {
		l := MustLimitsFor(p)
		if got, want := p.KeysMax(), l.KeysMax; got != want {
			t.Errorf("Plan(%s).KeysMax() = %d, table = %d", p, got, want)
		}
	}
}

// TestPlanEvictionPriorityReservedAllowed pins the per-plan tier gate
// for the reserved eviction tier (issue #475). Free = false (no
// reserved apps on the abuse-floor tier); Hobby+ = true. apid's
// updateApp handler reads this via Plan.EvictionPriorityReservedAllowed()
// and rejects a `reserved` PATCH on Free with 403
// plan_eviction_priority_reserved_not_allowed. Unknown plans must fail
// closed (return false) so a missing plan row never silently unlocks
// the reserved tier — same contract as WarmSnapshotAllowed above.
func TestPlanEvictionPriorityReservedAllowed(t *testing.T) {
	cases := []struct {
		plan Plan
		want bool
	}{
		{PlanFree, false},
		{PlanHobby, true},
		{PlanPro, true},
		{PlanScale, true},
		{Plan("unknown"), false},
	}
	for _, c := range cases {
		if got := c.plan.EvictionPriorityReservedAllowed(); got != c.want {
			t.Errorf("%s.EvictionPriorityReservedAllowed() = %v, want %v", c.plan, got, c.want)
		}
	}
}

// TestPlanReservedConcurrencyPerAccount pins the per-account cap on
// apps with eviction_priority='reserved' (issue #475). Free 0; Hobby 1;
// Pro 2; Scale 4. apid's updateApp path enforces this under an
// apps-row FOR UPDATE lock (mirrors CreateCronIfUnderQuota). Unknown
// plans must fail closed (return 0) — same contract as
// CronLimitPerAccount above.
func TestPlanReservedConcurrencyPerAccount(t *testing.T) {
	cases := []struct {
		plan Plan
		want int
	}{
		{PlanFree, 0},
		{PlanHobby, 1},
		{PlanPro, 2},
		{PlanScale, 4},
		{Plan("unknown"), 0},
	}
	for _, c := range cases {
		if got := c.plan.ReservedConcurrencyPerAccount(); got != c.want {
			t.Errorf("%s.ReservedConcurrencyPerAccount() = %d, want %d", c.plan, got, c.want)
		}
	}
}

// TestPlanEvictionPriorityAccessorsMatchTable pins that the accessors
// read the same values the Limits struct holds. Catches a regression
// where a future contributor edits the struct fields but forgets the
// accessors (or vice versa). Mirrors TestKeysMaxAccessorsMatchTable.
func TestPlanEvictionPriorityAccessorsMatchTable(t *testing.T) {
	for _, p := range Plans {
		l := MustLimitsFor(p)
		if got, want := p.EvictionPriorityReservedAllowed(), l.EvictionPriorityReservedAllowed; got != want {
			t.Errorf("Plan(%s).EvictionPriorityReservedAllowed() = %v, table = %v", p, got, want)
		}
		if got, want := p.ReservedConcurrencyPerAccount(), l.ReservedConcurrencyPerAccount; got != want {
			t.Errorf("Plan(%s).ReservedConcurrencyPerAccount() = %d, table = %d", p, got, want)
		}
	}
}

// TestPlanRateLimitPerAccount pins the per-account requests/minute cap
// per plan (ADR-040 / issue #292). Free 50/min, Hobby 200/min, Pro
// 1000/min, Scale 5000/min. Unknown plans must fail closed (return 0)
// so a missing row never silently unlocks cross-app botnets — same
// contract as CronLimitPerAccount above.
func TestPlanRateLimitPerAccount(t *testing.T) {
	cases := []struct {
		plan    Plan
		wantRPM int
	}{
		{PlanFree, 50},
		{PlanHobby, 200},
		{PlanPro, 1000},
		{PlanScale, 5000},
		{Plan("unknown"), 0},
	}
	for _, c := range cases {
		if got := c.plan.RateLimitPerAccountRPM(); got != c.wantRPM {
			t.Errorf("%s.RateLimitPerAccountRPM() = %d, want %d", c.plan, got, c.wantRPM)
		}
	}
}

// TestPlanStreaming pins the per-plan streaming flags (issue #471 /
// ADR-047 PR-A). Free is gated out (CodePlanStreamingNotAllowed in
// apid's validateUpdateApp); Hobby/Pro/Scale unlock streaming. MaxResponse
// bytes cap is the 100 MiB / 25 MiB pin; ResponseWriteTimeout is the
// 900 s / 300 s pin. Unknown plans must fail closed on all three
// flags so a missing plan never silently unlocks streaming.
func TestPlanStreaming(t *testing.T) {
	enabledCases := []struct {
		plan Plan
		want bool
	}{
		{PlanFree, false},
		{PlanHobby, true},
		{PlanPro, true},
		{PlanScale, true},
		{Plan("unknown"), false},
	}
	for _, c := range enabledCases {
		if got := c.plan.StreamingEnabled(); got != c.want {
			t.Errorf("%s.StreamingEnabled() = %v, want %v", c.plan, got, c.want)
		}
	}

	allowedCases := []struct {
		plan Plan
		want bool
	}{
		{PlanFree, false},
		{PlanHobby, true},
		{PlanPro, true},
		{PlanScale, true},
		{Plan("unknown"), false},
	}
	for _, c := range allowedCases {
		if got := c.plan.StreamingResponseAllowed(); got != c.want {
			t.Errorf("%s.StreamingResponseAllowed() = %v, want %v", c.plan, got, c.want)
		}
	}

	bodyCases := []struct {
		plan Plan
		want int64
	}{
		{PlanFree, 26_214_400},
		{PlanHobby, 104_857_600},
		{PlanPro, 104_857_600},
		{PlanScale, 104_857_600},
		// Unknown plans fail closed via the MaxResponseBodyBytesDefault
		// fallback (25 MiB) — the spec §4.1 pre-#471 buffer ceiling
		// is the conservative binding cap. Returns the default, not 0,
		// to guarantee a runaway stream never leaves the cap.
		{Plan("unknown"), MaxResponseBodyBytesDefault},
	}
	for _, c := range bodyCases {
		if got := c.plan.MaxResponseBodyBytes(); got != c.want {
			t.Errorf("%s.MaxResponseBodyBytes() = %d, want %d", c.plan, got, c.want)
		}
	}

	rwCases := []struct {
		plan Plan
		want time.Duration
	}{
		{PlanFree, 300 * time.Second},
		{PlanHobby, 900 * time.Second},
		{PlanPro, 900 * time.Second},
		{PlanScale, 900 * time.Second},
		// Unknown plans fall back to ResponseWriteTimeoutDefault
		// (300 s) — same conservative-fallback shape as the body
		// cap above. The listener ceiling always ends up bound by
		// the spec §4.1 default, never "no timeout".
		{Plan("unknown"), time.Duration(ResponseWriteTimeoutDefault) * time.Second},
	}
	for _, c := range rwCases {
		if got := c.plan.ResponseWriteTimeout(); got != c.want {
			t.Errorf("%s.ResponseWriteTimeout() = %v, want %v", c.plan, got, c.want)
		}
	}
}

// TestPlanTail pins the per-plan matrix for the waitUntil
// post-response tail primitive (issue #667 / ADR-078). Every plan
// unlocks the primitive (TailEnabled = true), with the per-plan
// TailTimeoutS / TailCapMax / ConcurrentTailsPerInstance values
// pinned verbatim from the issue's "Rules" section:
//
//   Free   5s  / 16 cap / 4 concurrent
//   Hobby 15s  / 16 cap / 16 concurrent
//   Pro   30s  / 16 cap / 64 concurrent
//   Scale 60s  / 16 cap / 256 concurrent
//
// The structural TailCapMax = 16 is a single source of truth — the
// accessor returns the constant regardless of the field value, so
// the cap is enforced even if a future plan row accidentally drops
// it. TailTimeoutSeconds clamps up to TailTimeoutFloorSeconds (5 s)
// for any plan whose row is unset / below the floor; this guarantees
// the reaper's park-watchdog can never be shorter than the per-plan
// timeout. Unknown plans fail closed on the boolean + integer
// accessors (return false / 0) but fall back to the floor on
// TailTimeoutSeconds.
func TestPlanTail(t *testing.T) {
	enabledCases := []struct {
		plan Plan
		want bool
	}{
		{PlanFree, true},
		{PlanHobby, true},
		{PlanPro, true},
		{PlanScale, true},
		// Unknown plans fail closed (return false) — same contract
		// as StreamingEnabled / WarmSnapshotEnabled / RequireAuthn.
		{Plan("unknown"), false},
	}
	for _, c := range enabledCases {
		if got := c.plan.TailEnabled(); got != c.want {
			t.Errorf("%s.TailEnabled() = %v, want %v", c.plan, got, c.want)
		}
		if got := c.plan.TailAllowed(); got != c.want {
			t.Errorf("%s.TailAllowed() = %v, want %v", c.plan, got, c.want)
		}
	}

	timeoutCases := []struct {
		plan Plan
		want int
	}{
		{PlanFree, 5},
		{PlanHobby, 15},
		{PlanPro, 30},
		{PlanScale, 60},
		// Unknown plans fall back to the floor — the
		// ParkTailDrainTimeoutSeconds (5 s) watchdog must always
		// be able to drain a tail mid-task.
		{Plan("unknown"), TailTimeoutFloorSeconds},
	}
	for _, c := range timeoutCases {
		if got := c.plan.TailTimeoutSeconds(); got != c.want {
			t.Errorf("%s.TailTimeoutSeconds() = %d, want %d", c.plan, got, c.want)
		}
	}

	// TailCapMax is structural — the accessor returns the constant
	// regardless of the plan row's field. Pin every plan to 16
	// (the issue's single source of truth).
	capMaxCases := []struct {
		plan Plan
		want int
	}{
		{PlanFree, TailCapMax},
		{PlanHobby, TailCapMax},
		{PlanPro, TailCapMax},
		{PlanScale, TailCapMax},
		{Plan("unknown"), TailCapMax},
	}
	for _, c := range capMaxCases {
		if got := c.plan.TailCapMax(); got != c.want {
			t.Errorf("%s.TailCapMax() = %d, want %d", c.plan, got, c.want)
		}
	}

	concurrentCases := []struct {
		plan Plan
		want int
	}{
		{PlanFree, 4},
		{PlanHobby, 16},
		{PlanPro, 64},
		{PlanScale, 256},
		// Unknown plans fail closed (return 0) — same contract
		// as the boolean accessors above.
		{Plan("unknown"), 0},
	}
	for _, c := range concurrentCases {
		if got := c.plan.ConcurrentTailsPerInstance(); got != c.want {
			t.Errorf("%s.ConcurrentTailsPerInstance() = %d, want %d", c.plan, got, c.want)
		}
	}

	// Pin the structural constants themselves so a future refactor
	// cannot silently move them.
	if TailCapMax != 16 {
		t.Errorf("TailCapMax = %d, want 16 (issue #667 single source of truth)", TailCapMax)
	}
	if TailTimeoutFloorSeconds != 5 {
		t.Errorf("TailTimeoutFloorSeconds = %d, want 5 (matches ParkTailDrainTimeoutSeconds)", TailTimeoutFloorSeconds)
	}
	if ParkTailDrainTimeoutSeconds != TailTimeoutFloorSeconds {
		t.Errorf("ParkTailDrainTimeoutSeconds = %d, must equal TailTimeoutFloorSeconds (%d) so the watchdog is never shorter than the shortest per-plan timeout",
			ParkTailDrainTimeoutSeconds, TailTimeoutFloorSeconds)
	}
}

// TestPlanTailTimeoutClamp pins the clamp-up behaviour on
// TailTimeoutSeconds (issue #667 / ADR-078 §"Why the host ships
// entropy" parallel): a buggy planLimits entry that drops below
// the floor is clamped up by Plan.TailTimeoutSeconds() so the
// reaper's 5 s park-watchdog always has at least a chance to drain
// the tail before force-park. The accessor is the only entry point
// used by schedd / apid / runner, so the clamp is the load-bearing
// invariant — a regression here would let a runaway tail hold a
// wake open past the watchdog ceiling.
func TestPlanTailTimeoutClamp(t *testing.T) {
	// Confirm the floor is non-zero (otherwise the clamp is a no-op
	// and the watchdog contract breaks).
	if TailTimeoutFloorSeconds <= 0 {
		t.Fatalf("TailTimeoutFloorSeconds = %d, must be > 0 so the watchdog always has drain headroom", TailTimeoutFloorSeconds)
	}

	// All four known plans must return >= the floor (the per-plan
	// values 5/15/30/60 are all strictly >= the 5 s floor, but the
	// clamp guards against future regressions).
	for _, p := range Plans {
		if got := p.TailTimeoutSeconds(); got < TailTimeoutFloorSeconds {
			t.Errorf("%s.TailTimeoutSeconds() = %d, must be >= TailTimeoutFloorSeconds (%d)",
				p, got, TailTimeoutFloorSeconds)
		}
	}
}

// TestOCIPullTimeoutSeconds pins the per-pull HTTP timeout (ADR-021) —
// pkg/oci.RegistryClient consults this when no WithTimeout override is
// passed. The number is a platform constant: every plan shares the same
// ceiling so the cold-boot latency contract stays predictable. 60s is
// well above the largest manifest + image-config GET and a generous
// safety margin over the fail-fast PullImageConfig path.
func TestOCIPullTimeoutSeconds(t *testing.T) {
	if OCIPullTimeoutSeconds != 60 {
		t.Errorf("OCIPullTimeoutSeconds = %d, want 60", OCIPullTimeoutSeconds)
	}
	if OCIPullTimeoutSeconds < 10 {
		t.Errorf("OCIPullTimeoutSeconds = %d must be >= 10s so a slow registry cannot starve the cold-boot latency budget", OCIPullTimeoutSeconds)
	}
}

// TestPlanRequireAuthnAllowed pins the per-plan gate that apid's
// updateApp handler uses for the per-app require_authn field
// (issue #560). Free/Hobby → false (Cloud Run's
// `--no-allow-unauthenticated` is a paid-tier feature); Pro/Scale →
// true. Unknown plans must default to false (fail-closed: a missing
// plan never silently unlocks a premium feature). Mirrors
// TestPlanScaleUpTargetRPSAllowed shape — same boolean accessor,
// same plan row count.
func TestPlanRequireAuthnAllowed(t *testing.T) {
	cases := []struct {
		plan Plan
		want bool
	}{
		{PlanFree, false},
		{PlanHobby, false},
		{PlanPro, true},
		{PlanScale, true},
		{Plan("unknown"), false},
	}
	for _, c := range cases {
		if got := c.plan.RequireAuthnAllowed(); got != c.want {
			t.Errorf("%s.RequireAuthnAllowed() = %v, want %v", c.plan, got, c.want)
		}
	}
}

// TestRequireAuthnAccessorMatchesTable pins that the accessor reads
// the same value the Limits struct holds. Mirrors
// TestOrgAccessorsMatchTable above — catches regressions where a
// future contributor edits the struct field but forgets the
// accessor (or vice versa).
func TestRequireAuthnAccessorMatchesTable(t *testing.T) {
	for _, p := range Plans {
		l := MustLimitsFor(p)
		if got, want := p.RequireAuthnAllowed(), l.RequireAuthn; got != want {
			t.Errorf("Plan(%s).RequireAuthnAllowed() = %v, table = %v", p, got, want)
		}
	}
}

// TestCharacterizationDeadlines pins the ADR-051 observation window.
// The guest (characterize_linux.go::waitForBind) and the host
// (pkg/fcvm/manager.go::characterizationWait) both read from this
// single source so a future bump moves both sides together. The
// invariants: guest >= host (the guest's full observation window
// must cover the host's dial+read window or the host gives up
// first and reports a false timeout), both > 0 (a zero deadline
// would make waitForBind return instantly without observing the
// bind), and both < readyTimeout (the legacy vmmd waitReady
// default of 30s — characterization is the faster gate).
func TestCharacterizationDeadlines(t *testing.T) {
	if CharacterizationDeadline <= 0 {
		t.Errorf("CharacterizationDeadline = %s, want > 0", CharacterizationDeadline)
	}
	if CharacterizationHostDeadline <= 0 {
		t.Errorf("CharacterizationHostDeadline = %s, want > 0", CharacterizationHostDeadline)
	}
	if CharacterizationDeadline < CharacterizationHostDeadline {
		t.Errorf("guest deadline %s < host deadline %s (host would time out before guest has a chance to ship)",
			CharacterizationDeadline, CharacterizationHostDeadline)
	}
	const readyTimeout = 30 * time.Second
	if CharacterizationDeadline >= readyTimeout {
		t.Errorf("CharacterizationDeadline = %s must be < readyTimeout (%s) so characterization gates boot faster than the legacy :8080 accept path",
			CharacterizationDeadline, readyTimeout)
	}
}

// TestLogRingBufferBytes pins the ADR-051 Slice A PR-B ring buffer
// capacity. The characterize probe reads this buffer's Tail() into
// the report's LogTail field (after truncateLog's wire-side clamp
// at VsockCharacterizationMaxBody = 32 KiB). Three invariants:
//
//   - non-zero: a zero-sized buffer would silently drop every boot
//     log byte, regressing the LogTail field back to the pre-PR-B
//     empty-string sentinel.
//   - >= 32 KiB: the buffer must be at least as large as the
//     wire-body cap so a customer's boot log that fills the buffer
//     can still surface the wire's full 32 KiB without truncation
//     inside the buffer itself.
//   - <= 1 MiB: a single-megabyte ring buffer per guest is the
//     largest reasonable allocation; anything larger would silently
//     bloat the per-guest memory budget (every Supervisor carries
//     one of these, even on boxes where the characterize probe is
//     disabled).
func TestLogRingBufferBytes(t *testing.T) {
	if LogRingBufferBytes <= 0 {
		t.Fatalf("LogRingBufferBytes = %d, want > 0", LogRingBufferBytes)
	}
	const wireBodyCap = 32 * 1024 // VsockCharacterizationMaxBody
	if LogRingBufferBytes < wireBodyCap {
		t.Errorf("LogRingBufferBytes = %d, want >= %d (VsockCharacterizationMaxBody) so the buffer holds the full wire body without internal truncation",
			LogRingBufferBytes, wireBodyCap)
	}
	const saneUpperBound = 1024 * 1024 // 1 MiB
	if LogRingBufferBytes > saneUpperBound {
		t.Errorf("LogRingBufferBytes = %d, want <= %d (1 MiB sanity upper bound; per-guest ring buffer must not silently bloat memory)",
			LogRingBufferBytes, saneUpperBound)
	}
}
