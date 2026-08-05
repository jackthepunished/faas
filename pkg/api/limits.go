// Package api holds cross-component API types shared by every daemon.
//
// limits.go is the ONE place every plan quota, ceiling, and hard limit lives.
// The financial model (ex44_faas_financial_model.xlsx) is the source of these
// numbers; the implementation spec §1/§4/§13 encodes them here. Never inline a
// limit at its point of use — read it from this table so a single edit moves the
// whole platform (spec §15 conventions).
//
// Money is integer millicents (1 cent = 1000 millicents). Floats near money fail
// review (spec §Conventions).
package api

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

// Plan is a customer subscription tier. The zero value is intentionally invalid
// so an unset plan never silently reads as Free.
type Plan string

const (
	PlanFree  Plan = "free"
	PlanHobby Plan = "hobby"
	PlanPro   Plan = "pro"
	PlanScale Plan = "scale"
)

// Plans lists every plan low-to-high. Order matters for upgrade/downgrade logic
// and for deterministic tests — do not reorder.
var Plans = []Plan{PlanFree, PlanHobby, PlanPro, PlanScale}

// Limits is the full quota/limit set for one plan. Every field has a spec
// reference. Add a field here (never a literal elsewhere) when a new limit
// appears, and cover it in limits_test.go.
type Limits struct {
	Plan Plan

	// Deploy-time quotas (enforced by apid before work happens, spec §4.2).
	DeployedApps       int // max apps in state active|evicted_cold
	MaxConcurrency     int // max instances of one app in {WAKING,COLD_BOOTING,RUNNING}
	RAMMB              int // max ram_mb per app (memory.max = RAMMB + PerVMOverheadMB)
	AppLayerMaxMB      int // drive1 ext4 cap (spec §4.6)
	SourceTarballMaxMB int // upload cap; >cap => 413 (spec §4.2)

	// ConcurrencyPerVMBound (issue #559) is the platform-advertised
	// upper bound on concurrent in-flight requests one VM can handle
	// at the listener layer. Distinct from MaxConcurrency (the per-app
	// *instance* cap, spec §6.2-1) — this is per-VM, not per-app.
	// Concurrency above 1 is the customer's runner/process
	// responsibility: Node.js (single-event-loop), Python asyncio,
	// and Go net/http handlers process concurrent requests within
	// one process; a synchronous subprocess-per-request handler
	// (e.g. a Python reader-from-stdin script) does not. All five
	// current runners spawn one subprocess per request via cmd.Run()
	// (guest/runners/<runtime>/main.go), so the bound is the
	// listener's goroutine count, not a single process's request
	// queue. Surfaced on GET /v1/apps/{slug} as concurrency_per_vm
	// so dashboards + CLI can render the platform's per-VM bound
	// without reading limits.go. Spec §13 hard-limits table.
	ConcurrencyPerVMBound int // Free 1, Hobby 5, Pro 25, Scale 80

	// Runtime shape.
	VCPU         int // firecracker vcpu_count (spec §4.4)
	IdleTimeoutS int // default idle-reaper timeout (spec §4.3)

	// CPU fairness (issue #301 / ADR-044). The 3-level cgroup hierarchy
	// (faas-tenant.slice/tenant-<plan>.slice/<instance>) enforces these
	// per-plan via two complementary channels:
	//
	//   - CPUWeight is fed to the jailer --cgroup cpu.weight=N argv, so
	//     the kernel schedules bursts under the plan's share of the
	//     tenant slice. Ratio 2:4:8:16 (Free:Hobby:Pro:Scale) so
	//     Scale-customer bursts can preempt Free-customer bursts but
	//     never starve them out of their weight.
	//   - CPUQuotaUS / CPUPeriodUS are written directly to the
	//     instance's cpu.max file (jailer v1.7 has no --cgroup cpu.max
	//     arg). Together they cap per-instance compute so a Plan H
	//     hot-loop never burns the box's full multi-core share.
	//
	// The values are the issue #301 spec: Free 100ms/100ms, Hobby
	// 200ms/200ms, Pro 500ms/500ms, Scale 1000ms/1000ms.
	CPUWeight   int // kernel cpu.weight (1..10000); ratio per plan
	CPUQuotaUS  int // cpu.max quota half (microseconds)
	CPUPeriodUS int // cpu.max period half (microseconds)

	// Metering (spec §1, §10). Money in millicents.
	IncludedGBHours int   // included GB-RAM-hours per calendar month
	PriceMillicents int64 // monthly subscription price

	// Edge (gatewayd, spec §4.1).
	RateLimitRPS   int // token-bucket refill rate
	RateLimitBurst int // token-bucket burst
	// RateLimitPerAccountRPM is the per-account requests/minute cap
	// (ADR-040 / issue #292). Distinct from RateLimitRPS/Burst which are
	// per-app. Bucket parameters consumed by pkg/gateway.Limiter.AllowAccount.
	// Bounds the cross-app botnet signature — a customer rotating across
	// many apps stays under per-app rps individually but cannot exceed
	// the per-minute sum across all their apps.
	RateLimitPerAccountRPM int

	// Networking (spec §7).
	EgressMbit int // per-instance egress bandwidth cap via tc

	// Secrets (spec §11/G2). Ciphertext quota per app; per-value byte cap.
	// SecretCountMax bounds the (app_id, key) row count. SecretValueMaxBytes
	// bounds the plaintext value the customer may PUT — apid rejects larger
	// values with 413 CodeSecretValueTooLarge before sealing.
	SecretCountMax      int // max secrets per app (Free 3, Hobby 25, Pro 50, Scale 100)
	SecretValueMaxBytes int // per-secret value byte cap (Free 4K, Hobby 8K, Pro 16K, Scale 32K)

	// Customer env vars (issue #395 / ADR-045). Plaintext per-app store
	// for non-sensitive runtime config (LOG_LEVEL, FEATURE_X, etc.). The
	// quota shape mirrors secrets minus the per-secret seal cost — values
	// are stored as-is, no ciphertext. EnvVarsMax bounds the (app_id,
	// key) row count. EnvValueMaxBytes bounds the per-value byte cap.
	// Per-plan values are tuned to cover typical 12-factor config
	// surface without letting one app monopolise the table.
	EnvVarsMax       int // max env vars per app (Free 8, Hobby 32, Pro 64, Scale 256)
	EnvValueMaxBytes int // per-value byte cap (Free 4K, Hobby 8K, Pro 16K, Scale 32K)

	// TrustedSignerCountMax bounds the (app_id, signer_name) row count
	// in app_trusted_signers (issue #472 / ADR-054). Mirrors
	// EnvVarsMax's posture — a config cap, not a credential one.
	// Per-plan values are tuned to cover the typical CI rotation
	// surface (3-5 publishers: GitHub Actions, GitLab CI, Jenkins,
	// Argo CD, custom in-house) without letting one app accumulate
	// an unbounded allowlist. Free plan is 0 — the open-deploy
	// posture for Free means customers on Free never need
	// require_signed=true and so never need signers either.
	// Spec §11: signature enforcement is a regulated-workload feature;
	// Free tier keeps its "ship any image" path.
	TrustedSignerCountMax int // Free 0, Hobby 4, Pro 8, Scale 16

	// RegistryCredentialMax (issue #461 / ADR-062) bounds the per-app
	// count of sealed Basic Auth credentials, one row per (app, host).
	// Free = 0 — Free cannot pull from private registries (the abuse
	// path of credentialed pulls on a single-concurrency plan is not
	// the product target). Hobby/Pro/Scale opt in with a small
	// fan-out budget: a Hobby customer's typical surface is one
	// staging + one prod registry (2); Pro/Scale absorb the
	// multi-region CI shape (5/20). Per-app, not per-account, because
	// the credential is app-scoped (different apps can target
	// different registries). apid's setRegistryCredential handler
	// gates 403 plan_registry_credentials_not_allowed when
	// RegistryCredentialMax == 0 and 413 plan_registry_credential_quota
	// when the count reaches the cap and the upsert is a fresh host.
	RegistryCredentialMax int

	// MinInstancesAllowed toggles the per-app cold-wake floor (ux_spec
	// §6.5). Free keeps the default scale-to-zero behaviour because
	// `min_instances = N` keeps N × RAMMB resident at all times, which
	// is the cost shape of the always-on tier. Hobby + Pro + Scale opt
	// in (issue #462 / ADR-058 PR-A tier-up: Hobby unlocked at PR-A
	// because the bill auto-counts via pkg/meter/sampler.go:238-239 and
	// the max_concurrency cap is bounded). apid's updateApp handler
	// gates the PATCH body on this flag.
	MinInstancesAllowed bool

	// MaxInstancesAllowed (issue #462 / ADR-058) toggles the per-app
	// ceiling on live instances. Mirrors MinInstancesAllowed: Hobby+
	// unlock, Free stays off. The customer-authored `max_instances`
	// is bounded above by the plan's MaxConcurrency (already a
	// hard cap on the wake path); the gate here is the plan-tier
	// lock, not the value-lock. The accessor
	// `Plan.MaxInstancesAllowed()` reads this field.
	MaxInstancesAllowed bool

	// MaxMinInstances (issue #557 / ADR-071) bounds the per-app
	// cold-wake floor independent of MaxConcurrency. ADR-071
	// §Decision 5: Hobby 1, Pro 3, Scale 10, Free 0. The cap is
	// tighter than today's implicit MaxConcurrency clamp (1/2/5/20)
	// because the floor is resident RAM against the §6.2-2
	// 47,600 MB ceiling — a Scale customer pinning the floor at
	// MaxConcurrency=20 would commit ~20.6 GB (~43% of ceiling)
	// from a single API call. Mirrors TrustedSignerCountMax's
	// posture: a per-plan value cap, not a plan-tier lock. Plan
	// lock is the existing MinInstancesAllowed bool. Free=0 here
	// means even if MinInstancesAllowed is unlocked, Free has no
	// floor — but Free's MinInstancesAllowed=false keeps it
	// locked off regardless.
	MaxMinInstances int // Free 0, Hobby 1, Pro 3, Scale 10

	// ScaleUpTargetRPSAllowed toggles `autoscale_target_rps` per plan
	// (issue #169 / #172). Hobby + Pro + Scale opt in; Free does not
	// (Free is single-concurrency and the per-request cost envelope
	// already covers any reasonable load). apid's updateApp handler
	// returns 403 CodePlanScaleUpNotAllowed when the plan lacks this
	// gate.
	ScaleUpTargetRPSAllowed bool

	// ScaleUpTargetCPUAllowed toggles `autoscale_target_cpu_pct` per
	// plan (issue #169 / #172). Pro + Scale only — "scale on CPU"
	// without `min_instances` set is unbounded on Hobby, where the cost
	// shape is too steep for the cheaper tier. apid's updateApp handler
	// returns 403 CodePlanScaleUpNotAllowed when the plan lacks this
	// gate.
	ScaleUpTargetCPUAllowed bool

	// Move 1 event-shaped surfaces (spec §4.4, §4.9, CLAUDE.md Hard
	// limits). The apid cap checks on POST .../queues/invocations:send
	// and POST /v1/apps/{slug}/delayed-tasks read these via
	// MustLimitsFor; schedd's drain re-checks delayed_tasks before
	// claiming a row in case the cap shifted between enqueue and tick.
	//
	// MaxQueueDepth bounds the per-app live (pending+dispatching)
	// 'queue' rows at any moment. The drain long-poll (queueReceive)
	// holds the cap stable; an empty queue stops draining.
	//
	// MaxDelayedTasksPerApp caps how many delayed_tasks an app may
	// have scheduled (pending+dispatching). Scale gets the cap-check
	// ceiling (1e6); per-task payload remains the binding
	// constraint via MaxSourceBytesPerInvocation.
	//
	// MaxSourceBytesPerInvocation is the body size cap the apid
	// enforces on every event-shaped POST (sync invoke, async
	// invoke, queue :send, delayed-task create). The matrix scales
	// sub-linearly with the source-tarball ratio so a customer can
	// send roughly the same fraction of their tier budget per
	// payload regardless of plan.
	//
	// AsyncInvokeAllowed gates the async-invoke surface: Free is
	// false (spec §4.4 reserves event-shaped primitives for paid
	// tiers). sync-invoke and queueReceive are still reachable; only
	// the 202 surface is plan-conditional.
	MaxQueueDepth               int
	MaxDelayedTasksPerApp       int
	MaxSourceBytesPerInvocation int
	AsyncInvokeAllowed          bool

	// MaxQueueAttempts (issue #394 / Move 1 dead-letter) is the
	// per-plan retry budget for queue messages that hit a transient
	// failure during drain (pkg/sched/drain.go). Once a row's
	// `attempts` reaches this value, the next transient failure
	// transitions it to state='dead_letter' (terminal) instead of
	// 'pending' (re-queued). Free = 0 means queues aren't entitled
	// anyway (MaxQueueDepth = 0); the drain keeps the legacy
	// infinite-retry behaviour because Free never queues. Hobby/Pro/
	// Scale follow the same shape as the other async-event caps:
	// small on the cheap tier, larger as we move up. The dead-letter
	// rows are readable via GET /v1/apps/{slug}/queues/dead_letter
	// (migrations/00060_invocations_dead_letter.sql lands the
	// 'dead_letter' state value + partial index).
	MaxQueueAttempts int

	// LogDeploymentFilterMax (issue #517 / PR-B, AC3) caps how
	// many concurrent `?deployment=` filters a customer may scope
	// their log stream to. The wire surface is single-valued today
	// (the SDK takes one deploymentID arg); this field is the
	// plan-tier gate the handler enforces before forwarding to
	// schedd. Free returns 0 so a Free customer's `?deployment=`
	// is rejected with `plan_deployment_filter_not_allowed`; Hobby
	// unlocks it for the typical Hobby customer's single-staging-
	// deployment workload; Pro/Scale get the higher caps the
	// per-tier multi-deployment fan-out needs.
	LogDeploymentFilterMax int

	// Cron limits (spec §4.4 / event-shaped surface). Two independent
	// caps; both populated for every plan, Free is 0/0 so the
	// per-store check returns QuotaError immediately. The handler
	// also gates Free to 402 via ErrPlanCronsNotAllowed before
	// reaching the store; the store still reads 0/0 from Limits
	// and refuses (defence in depth).
	//
	// CronLimitPerApp caps how many crons a single app may hold at
	// any moment.
	//
	// CronLimitPerAccount caps how many crons an account may hold
	// across all its apps. Independent of CronLimitPerApp — the
	// per-account cap defends against the N-apps-times-cap-per-app
	// bypass. Both enforced under the same apps-row lock in
	// pkg/state.PgStore.CreateCronIfUnderQuota.
	CronLimitPerApp     int
	CronLimitPerAccount int

	// KeysMax (issue #189 / IAM-5) caps the per-account count of
	// active + grace API keys. Revoked keys are exempt — they
	// remain in the table for audit lineage but no longer count
	// against quoata. apid's createKey handler rejects with 409
	// api_key_limit_exceeded once the count reaches the cap;
	// rotateKey is quota-neutral (one new key replaces one old:
	// -1 +1 = 0 net) and is allowed at the cap. Per-plan values:
	// Free 3, Hobby 10, Pro 50, Scale 200 — tracks the typical
	// auth surface per tier (Free's 1-app customer might run 1 key
	// per deploy target; Scale's 100-app customer might run a
	// handful per team).
	KeysMax int

	// Organization limits (issue #190 / IAM-6 / ADR-061). Two
	// per-org caps; both populated for every plan, currently 0
	// until the financial model authorizes the per-plan values.
	// The same fail-closed contract as CronLimitPerApp applies:
	// 0 means the gate refuses all membership / invitation
	// operations, which is the safe default before the financial
	// model is updated. PR 1 ships the fields and the accessors
	// but does NOT invent per-plan values — those land in PR 2
	// alongside the schema.
	//
	// OrgMembersMax caps the count of active members
	// (`removed_at IS NULL`) on a non-personal org. The owner
	// counts toward the cap; the personal org is exempt (its
	// membership is exactly one). The PR 2 schema adds the
	// `removed_at` column so the cap reflects only live members.
	//
	// OrgPendingInvitationsMax caps the count of pending
	// invitations on a non-personal org. Independent of
	// OrgMembersMax — defends against the N-invites × fast-accept
	// botnet signature without blocking quiet growth.
	OrgMembersMax            int
	OrgPendingInvitationsMax int

	// AlertRuleLimitPerApp caps how many alert rules an account may pin
	// to a single app. Account-wide rules (Issue #396 / ADR-045,
	// AppID == "") count toward the per-app cap only when the rule pins
	// an app — they do not count against any per-app cap because they
	// have no app to bind against. The cap defends against a noisy
	// customer deploying N rules on a hot app.
	AlertRuleLimitPerApp int
	// AlertRuleLimitPerAccount caps the total alert rules an account
	// holds across every app plus account-wide. Independent of
	// AlertRuleLimitPerApp — the per-account cap defends against the
	// N-apps-times-cap-per-app bypass that the cron shape closed in
	// M7. Both enforced under the same apps-row lock + per-account
	// read in pkg/state.CreateAlertRuleIfUnderQuota.
	AlertRuleLimitPerAccount int

	// EgressAllowlistAllowed toggles the per-app outbound IP allowlist
	// (ADR-031, tier-2 of the network roadmap). Free + Hobby keep
	// allowlist opt-out because the abuse-desk use case is a
	// Pro+ concern (Scale customers are the ones with the budget to
	// care about egress hygiene). Pro/Scale cap their max entries
	// differently — Pro is 16, Scale 64 — the higher scale tier gets
	// a larger entry budget because SaaS-scale apps tend to integrate
	// with more upstream services. apid's updateApp handler rejects a
	// PATCH with 403 plan_egress_allowlist_not_allowed when this is
	// false.
	EgressAllowlistAllowed bool
	// EgressAllowlistMaxSize is the per-app count cap on CIDR entries.
	// 0 with Allowed=false (Free/Hobby); non-zero with Allowed=true
	// (Pro: 16; Scale: 64). apid's updateApp rejects with 400
	// egress_allowlist_too_long when the PATCH body has more entries.
	EgressAllowlistMaxSize int

	// WarmSnapshotEnabled (issue #470 / ADR-055) is the plan-gated
	// default for the per-app two-tier snapshot flag. Free/Hobby =
	// false (warm-tier apps keep both warm.snap + init.snap, which
	// is +130 MB per app on the parked disk budget — Hobby's pricing
	// tier is too cheap for that). Pro/Scale = true (the doubled
	// parked footprint is inside the 452 GB budget). Apid's
	// updateApp handler rejects Free/Hobby PATCH-true with
	// 403 plan_warm_snapshot_not_allowed; the default is applied
	// at CreateApp time so a Pro customer's brand-new app gets a
	// warm.snap without an extra PATCH.
	WarmSnapshotEnabled bool
	// WarmSnapshotMinRequestsDefault is the per-app request-count
	// threshold for warm-tier capture, applied at CreateApp when
	// the plan allows it. Free/Hobby = 0 (irrelevant because
	// WarmSnapshotEnabled = false there). Pro/Scale = 5. Range
	// [1, 100] (migration 00109 CHECK). The per-app PATCH may
	// override; both the SQL CHECK and the apid handler reject
	// out-of-range values.
	WarmSnapshotMinRequestsDefault int
	// WarmSnapshotMinMsDefault is the per-app time-since-first-ready
	// threshold for warm-tier capture, applied at CreateApp when
	// the plan allows it. Free/Hobby = 0 (irrelevant). Pro/Scale =
	// 2000 (matches Node.js Express / Flask framework startup).
	// Range [100, 60000] (migration 00109 CHECK).
	WarmSnapshotMinMsDefault int

	// StreamingEnabled (issue #471) gates the per-app streaming
	// response path through gatewayd (Flusher + periodic 200 ms /
	// 256 KiB tx_bytes flush; ADR-047). Free defaults off — the
	// buffered path is the v1 contract and Free is the abuse-floor
	// tier where an unbounded stream would let one app monopolise
	// gatewayd. Hobby/Pro/Scale default on; apid's updateApp handler
	// rejects Free PATCH with 403 plan_streaming_not_allowed (issue
	// #471 AC #3). The plan-level default is applied at CreateApp
	// time via buildApp so a Hobby customer's brand-new app is
	// streaming-ready without an extra PATCH round-trip.
	StreamingEnabled bool
	// MaxResponseBodyBytes is the per-response body cap (spec §4.1
	// for the legacy 25 MB bound; issue #471 raises the cap for
	// Hobby+ to 100 MB so LLM-style streams have headroom). 0 means
	// "fall back to api.MaxResponseBodyBytesDefault" so an unknown
	// plan fails closed rather than silently inheriting Free's cap.
	// gatewayd wraps the response writer in http.MaxBytesWriter at
	// this number; PR-A leaves the writer unused on the buffered
	// path and PR-B activates it on the streaming path.
	MaxResponseBodyBytes int64
	// ResponseWriteTimeoutSeconds is the total-response-write window
	// for streaming responses (spec §4.1: 300 s; issue #471 raises
	// it to 900 s for Hobby+ so 30 s LLM streams + slow client reads
	// fit). The http.Server-level WriteTimeout is the safety net;
	// the per-flush deadline is enforced via http.ResponseController.
	// 0 means "fall back to api.ResponseWriteTimeoutDefault".
	ResponseWriteTimeoutSeconds int
}

// planLimits is the authoritative table. Values: spec §1 quota row, §4.1 rate
// limits, §4.3 idle timeouts, §4.6 app-layer caps, §7 egress, §10 prices.
//
// Plans (deployed/concurrent/RAM MB/GB-h):
//
//	Free  1 / 1  / 128  / 5
//	Hobby 5 / 2  / 256  / 50
//	Pro   25/ 5  / 512  / 250
//	Scale 100/20 / 1024 / 1500
var planLimits = map[Plan]Limits{
	PlanFree: {
		Plan:           PlanFree,
		DeployedApps:   1,
		MaxConcurrency: 1,
		RAMMB:          128,
		// ConcurrencyPerVMBound (issue #559): Free is the
		// single-concurrency tier — one VM serves one request at a
		// time. Mirrors MaxConcurrency (= 1) because a Free customer
		// cannot have more than one VM per app anyway, so the
		// per-VM and per-app bounds collapse to the same number.
		ConcurrencyPerVMBound: 1,
		// AppLayerMaxMB 256 — Free is the lowest cap tier; spec §1 ("App-
		// layer build ... Free 256 MB") and the limits table both read 256
		// (PR #241 spec-drift audit, 2026-07-26). This is a no-op
		// alignment comment; the value was 256 before this audit too.
		AppLayerMaxMB:       256,
		SourceTarballMaxMB:  100,
		VCPU:                2,
		IdleTimeoutS:        30,
		IncludedGBHours:     5,
		PriceMillicents:     0,
		RateLimitRPS:        5,
		RateLimitBurst:      20,
		EgressMbit:          10,
		SecretCountMax:      3,
		SecretValueMaxBytes: 4 * 1024,
		EnvVarsMax:          8,
		EnvValueMaxBytes:    4 * 1024,
		// TrustedSignerCountMax: Free keeps the open-deploy posture;
		// signature enforcement is a regulated-workload feature that
		// Free never needs (issue #472 / ADR-054).
		TrustedSignerCountMax: 0,
		// Issue #461: Free has no private-registry credential surface.
		// Handler returns 403 plan_registry_credentials_not_allowed.
		RegistryCredentialMax: 0,
		// Move 1: async invoke and queues are paid-only (§4.4); Free
		// keeps HTTP-only. The tiny 1 KB payload cap is the binding
		// constraint should a Free customer spoof the gate.
		MaxQueueDepth:               0,
		MaxDelayedTasksPerApp:       0,
		MaxSourceBytesPerInvocation: 0,
		AsyncInvokeAllowed:          false,
		// Free: queues aren't entitled (MaxQueueDepth = 0). Value is
		// kept at 0 for symmetry with the rest of the async-event
		// caps. The drain falls back to legacy infinite-retry when
		// budget == 0, but Free customers never reach the queue
		// surface so the path is unreachable.
		MaxQueueAttempts: 0,
		// Autoscale (issue #169 / #172): Free stays off. The per-request
		// cost envelope already covers Free's load shape, and a "scale
		// up" trigger on a 1-concurrency plan is meaningless.
		ScaleUpTargetRPSAllowed: false,
		ScaleUpTargetCPUAllowed: false,
		// Cron: Free has no crons (spec §4.4 paid-only, like async
		// invoke). Handler returns 402 ErrPlanCronsNotAllowed before
		// the store is touched; the 0/0 here is a defence-in-depth
		// value the store still reads.
		CronLimitPerApp:     0,
		CronLimitPerAccount: 0,
		// IAM-5 (issue #189): Free gets 3 keys — one for the customer's
		// primary deploy target + one for a staging slot + one for
		// break-glass. The abuse-vector (scripted key rotation under
		// 1-concurrency) is bounded by the per-account rate limit.
		KeysMax: 3,
		// IAM-6 / ADR-061: org membership is plan-gated until the
		// financial model authorizes a per-plan value. Free reads
		// 0/0 so the membership gate refuses before the store is
		// touched, mirroring the cron fail-closed shape.
		OrgMembersMax:            0,
		OrgPendingInvitationsMax: 0,
		// Alert rules (issue #396 / ADR-045): Free stays at 0/0.
		// Gates via CodePlanAlertRulesNotAllowed at the handler level
		// — the value is informational here for fail-closed accessors.
		AlertRuleLimitPerApp:     0,
		AlertRuleLimitPerAccount: 0,
		// Per-account rate limit (ADR-040): Free gets 50/min — enough for
		// the 1-concurrency plan's traffic envelope.
		RateLimitPerAccountRPM: 50,
		// Log deployment filter (issue #517 / PR-B): Free is the
		// abuse-floor tier — the filter is a paid feature.
		// Handler returns WritePlanDeploymentFilterNotAllowedError
		// before the store is touched; the 0 here is a
		// defence-in-depth value the handler still reads.
		LogDeploymentFilterMax: 0,
		// CPU fairness (issue #301 / ADR-044): Free gets the smallest
		// slice weight=2 and the tightest quota (100ms/100ms). 100 ms
		// is enough headroom for a Free-tier app to handle a handful of
		// requests without a throttle trip but stops a tight loop from
		// preempting other slice members.
		CPUWeight:   2,
		CPUQuotaUS:  100_000,
		CPUPeriodUS: 100_000,
		// Streaming (issue #471 / ADR-047): Free is the abuse-floor
		// tier — buffered path stays the contract, default off, no
		// cap lift (spec §4.1 baseline 25 MB / 300 s).
		StreamingEnabled:            false,
		MaxResponseBodyBytes:        MaxResponseBodyBytesDefault,
		ResponseWriteTimeoutSeconds: ResponseWriteTimeoutDefault,
		// Warm-snapshot (issue #470 / ADR-055): Free is off by
		// plan. Warm-tier apps keep warm.snap + init.snap on the
		// parked disk budget; doubling the per-app snapshot
		// footprint is incompatible with the Free pricing tier.
		WarmSnapshotEnabled:            false,
		WarmSnapshotMinRequestsDefault: 0,
		WarmSnapshotMinMsDefault:       0,
	},
	PlanHobby: {
		Plan:               PlanHobby,
		DeployedApps:       5,
		MaxConcurrency:     2,
		RAMMB:              256,
		AppLayerMaxMB:      512,
		SourceTarballMaxMB: 100,
		VCPU:               2,
		IdleTimeoutS:       60,
		IncludedGBHours:    50,
		PriceMillicents:    900_000, // €9.00
		// ConcurrencyPerVMBound (issue #559): Hobby allows up to
		// 5 concurrent in-flight requests per VM — tracks Cloud
		// Run's "smallest paid tier" framing while staying inside
		// Hobby's 256 MB RAM budget (one Node event loop comfortably
		// handles 5 concurrent requests, a typical Hobby customer's
		// usage pattern).
		ConcurrencyPerVMBound: 5,
		RateLimitRPS:          20,
		RateLimitBurst:        100,
		EgressMbit:            25,
		SecretCountMax:        25,
		SecretValueMaxBytes:   8 * 1024,
		EnvVarsMax:            32,
		EnvValueMaxBytes:      8 * 1024,
		// TrustedSignerCountMax: Hobby is the lowest paid tier; the
		// 4-publisher cap covers a hobbyist running a single CI
		// (GitHub Actions) + a backup CI (Codeberg) + a personal
		// signing key + an emergency break-glass. Anything beyond
		// that is "you're a Pro" territory.
		TrustedSignerCountMax: 4,
		// Issue #461: Hobby = 2 — staging + production.
		RegistryCredentialMax: 2,
		// 64 KB envelope = 0.25 % of Hobby's 25 MB tarball budget — small
		// enough to keep the drain tick bounded, large enough for typical
		// JSON event payloads.
		MaxQueueDepth:               5,
		MaxDelayedTasksPerApp:       5,
		MaxSourceBytesPerInvocation: 64 * 1024,
		AsyncInvokeAllowed:          true,
		// Hobby: 3 attempts. Tight on the cheap tier — a worker that
		// keeps re-trying a bad payload would otherwise burn the
		// per-app rps budget and starve the rest of the queue.
		MaxQueueAttempts: 3,
		// Autoscale: Hobby is gated on Pro+ for both RPS and CPU
		// (2026-07-28: ADR-037 amendment — Hobby→Pro re-tier on
		// ScaleUpTargetRPSAllowed). CPU-driven scaling is gated
		// on Pro+ because the cost shape of "scale on CPU without
		// a min_instances floor" is unbounded on Hobby.
		ScaleUpTargetRPSAllowed: false,
		ScaleUpTargetCPUAllowed: false,
		// Scaling policy (issue #462 / ADR-058, PR-A tier-up):
		// Hobby now unlocks `MinInstancesAllowed` (warm-floor
		// charge is bounded — Hobby's MaxConcurrency is 2 and
		// the bill auto-counts via pkg/meter/sampler.go:238-239).
		// MaxInstancesAllowed follows the same Hobby+ gate.
		// Hobby still does NOT unlock `ScaleUpTargetRPSAllowed`
		// nor `ScaleUpTargetCPUAllowed` — those remain Pro+ on
		// the existing cost-shape rationale. The doc copy on
		// the dashboard's "Plan" page names "Hobby+ unlocks
		// warm floor" so a Hobby customer opting in knows what
		// they're paying for.
		MinInstancesAllowed: true,
		MaxInstancesAllowed: true,
		// MaxMinInstances (ADR-071): Hobby gets 1 — one warm
		// instance is the minimum the floor feature exists to
		// deliver (the customer's "first request never pays the
		// §6.3 wake budget" expectation).
		MaxMinInstances: 1,
		// Cron: Hobby gets a small per-app budget (5) and a per-account
		// budget that absorbs ~2 Hobby-tier apps (10). Tracks the
		// Hobby apps cap (5) with headroom for the cron-example
		// template's tutorials.
		CronLimitPerApp:     5,
		CronLimitPerAccount: 10,
		// IAM-5 (issue #189): Hobby gets 10 keys — 2 per app across
		// the Hobby app budget (5) keeps every deploy target
		// (CI / staging / prod / personal / monitoring) with a
		// dedicated key.
		KeysMax: 10,
		// IAM-6 / ADR-061: org caps land in PR 2 once the financial
		// model authorizes them. PR 1 ships 0/0 so the fail-closed
		// gate refuses across every plan until the values are
		// sourced — see limits_test.go::TestOrgMembersLimits_ZeroUntilAuthorised.
		OrgMembersMax:            0,
		OrgPendingInvitationsMax: 0,
		// Alert rules (issue #396): Hobby gets 3 per-app and 10
		// per-account — a Hobby customer with 2 apps + 1 account-wide
		// rule lands inside both caps. The per-account floor tracks the
		// cron shape (10) because the typical Hobby customer configures
		// "one alert per app" and the spare capacity is for a couple of
		// account-wide rules.
		AlertRuleLimitPerApp:     3,
		AlertRuleLimitPerAccount: 10,
		// Per-account rate limit (ADR-040): Hobby gets 200/min — ~10× the
		// Hobby per-app rps (20) so per-app trips first on a single hot
		// app, and the account limit catches the cross-app botnet.
		RateLimitPerAccountRPM: 200,
		// Log deployment filter (issue #517 / PR-B): Hobby gets
		// 1 — the typical Hobby customer runs one staging
		// deployment alongside their prod slot, and the filter
		// scopes the log stream to it. Mirror shape of Hobby's
		// per-app cron cap (5).
		LogDeploymentFilterMax: 1,
		// CPU fairness (issue #301): Hobby weight=4, quota 200ms/200ms.
		// Doubles Free's quota — tracks the per-app concurrency bump
		// (1 → 2) and the per-app rps (5 → 20).
		CPUWeight:   4,
		CPUQuotaUS:  200_000,
		CPUPeriodUS: 200_000,
		// Streaming (issue #471 / ADR-047): Hobby is the first paid
		// tier — streaming is opt-in by default (the LLM use case is
		// the Hobby customer's entry point). Cap lifts to 100 MB / 900 s
		// to cover a 30–120 s chat completion plus headroom.
		StreamingEnabled:            true,
		MaxResponseBodyBytes:        100 * 1024 * 1024,
		ResponseWriteTimeoutSeconds: 900,
		// Warm-snapshot (issue #470 / ADR-055): Hobby is gated off
		// for the same cost-shape reason as Free — doubling the
		// parked per-app snapshot footprint doesn't fit the
		// €9/month Hobby price point. Pro/Scale customers pay
		// enough that the +130 MB per warm-tier app is comfortably
		// inside the 452 GB parked budget.
		WarmSnapshotEnabled:            false,
		WarmSnapshotMinRequestsDefault: 0,
		WarmSnapshotMinMsDefault:       0,
	},
	PlanPro: {
		Plan:               PlanPro,
		DeployedApps:       25,
		MaxConcurrency:     5,
		RAMMB:              512,
		AppLayerMaxMB:      1024,
		SourceTarballMaxMB: 250,
		VCPU:               2,
		IdleTimeoutS:       300,
		IncludedGBHours:    250,
		PriceMillicents:    2_900_000, // €29.00
		// ConcurrencyPerVMBound (issue #559): Pro allows up to
		// 25 concurrent in-flight requests per VM. Matches the
		// typical SaaS-tier workload envelope (one Node/Python
		// service handling fan-out from a single client request).
		ConcurrencyPerVMBound: 25,
		RateLimitRPS:          100,
		RateLimitBurst:        500,
		EgressMbit:            100,
		SecretCountMax:        50,
		SecretValueMaxBytes:   16 * 1024,
		EnvVarsMax:            64,
		EnvValueMaxBytes:      16 * 1024,
		// Issue #461: Pro = 5 — multi-region + CI shapes.
		RegistryCredentialMax: 5,
		MinInstancesAllowed:   true,
		MaxInstancesAllowed:   true,
		// MaxMinInstances (ADR-071): Pro = 3 — covers a small
		// "always-warm fan-out for a customer-facing API" pattern
		// without letting one Pro app reserve a quarter of the
		// box's RAM ceiling.
		MaxMinInstances: 3,
		// TrustedSignerCountMax: Pro covers a small-team rotation
		// matrix (5-8 publishers). Enough for "every dev has their own
		// key" workflows without letting the table grow unbounded.
		TrustedSignerCountMax: 8,
		// 256 KB = 0.1 % of Pro's 250 MB tarball.
		MaxQueueDepth:               25,
		MaxDelayedTasksPerApp:       50,
		MaxSourceBytesPerInvocation: 256 * 1024,
		AsyncInvokeAllowed:          true,
		// Pro: 10 attempts. Trades tolerance against "a poisoned row
		// churns indefinitely". At 10 retries a transient downstream
		// flap has plenty of room, while a permanently-bad payload
		// exits the worker pool within ~50 s at the default retry
		// backoff (5 s).
		MaxQueueAttempts: 10,
		// ADR-031: Pro gets 16 CIDR entries — enough for "1 SaaS +
		// 1 webhook + 1 monitoring + ~10 partner integrations" which
		// is the typical Pro-tier reachability graph.
		EgressAllowlistAllowed: true,
		EgressAllowlistMaxSize: 16,
		// Autoscale: Pro gets both RPS and CPU targets. The CPU target
		// is gated on Pro+ to bound the "scale on CPU without a
		// min_instances floor" cost shape.
		ScaleUpTargetRPSAllowed: true,
		ScaleUpTargetCPUAllowed: true,
		// Cron: Pro gets 20 per-app and 50 per-account. The per-app
		// ceiling is 4× Hobby (5→20); the per-account ceiling is 5×
		// Hobby (10→50) — slightly steeper because Pro customers
		// run more apps (25) than Hobby (5).
		CronLimitPerApp:     20,
		CronLimitPerAccount: 50,
		// IAM-5 (issue #189): Pro gets 50 keys — 2 per app across the
		// Pro app budget (25) plus a per-team allowance (CI / staging
		// / prod / personal / monitoring / break-glass).
		KeysMax: 50,
		// IAM-6 / ADR-061: PR 1 placeholder. PR 2 populates actual
		// per-plan values from the financial model.
		OrgMembersMax:            0,
		OrgPendingInvitationsMax: 0,
		// Alert rules (issue #396): Pro gets 10 per-app and 30
		// per-account. ~2× the Hobby per-account budget tracks the
		// Pro app budget (25 apps vs Hobby's 5).
		AlertRuleLimitPerApp:     10,
		AlertRuleLimitPerAccount: 30,
		// Per-account rate limit (ADR-040): Pro gets 1000/min — ~10× the
		// Pro per-app rps (100), same rationale as Hobby.
		RateLimitPerAccountRPM: 1000,
		// Log deployment filter (issue #517 / PR-B): Pro gets 10
		// — covers the typical multi-staging fan-out (prod + 3-5
		// staging branches + a few ephemeral preview slots) without
		// letting one app monopolise the schedd's per-instance
		// goroutine fan-out.
		LogDeploymentFilterMax: 10,
		// CPU fairness (issue #301): Pro weight=8, quota 500ms/500ms.
		// Half-bandwidth of 2 cores — tracks the per-app concurrency
		// (5) and the per-app rps (100).
		CPUWeight:   8,
		CPUQuotaUS:  500_000,
		CPUPeriodUS: 500_000,
		// Streaming (issue #471 / ADR-047): Pro is paid-tier streaming
		// — same cap as Hobby. 100 MB / 900 s covers LLM chat
		// completions and JSON/CSV exports; SaaS-scale apps don't
		// need a higher cap because gatewayd's per-instance egress
		// bandwidth ceiling (250 Mbit for Scale) is the binding
		// constraint long before 100 MB matters.
		StreamingEnabled:            true,
		MaxResponseBodyBytes:        100 * 1024 * 1024,
		ResponseWriteTimeoutSeconds: 900,
		// Warm-snapshot (issue #470 / ADR-055): Pro is the first
		// tier where warm-snapshot is on by default. Per the issue
		// body's acceptance: "for a Pro+ app that has served ≥5
		// successful requests ≥2 s after first-ready, restore
		// from warm.snap should be ≤50 % of init.snap p50".
		WarmSnapshotEnabled:            true,
		WarmSnapshotMinRequestsDefault: 5,
		WarmSnapshotMinMsDefault:       2000,
	},
	PlanScale: {
		Plan:               PlanScale,
		DeployedApps:       100,
		MaxConcurrency:     20,
		RAMMB:              1024,
		AppLayerMaxMB:      2048,
		SourceTarballMaxMB: 250,
		VCPU:               4,
		IdleTimeoutS:       600,
		IncludedGBHours:    1500,
		PriceMillicents:    9_900_000, // €99.00
		// ConcurrencyPerVMBound (issue #559): Scale = 80 — same
		// default as Cloud Run's `80 × vCPU` heuristic (the issue
		// body cites this number directly). 80 concurrent requests
		// per VM is comfortably reachable at Scale's 1024 MB RAM
		// for a typical Node.js / Go service; a sync-subprocess
		// Python customer would saturate before hitting this cap.
		ConcurrencyPerVMBound: 80,
		RateLimitRPS:          500,
		RateLimitBurst:        2000,
		EgressMbit:            250,
		SecretCountMax:        100,
		SecretValueMaxBytes:   32 * 1024,
		EnvVarsMax:            256,
		EnvValueMaxBytes:      32 * 1024,
		// Issue #461: Scale = 20 — broad fan-out for SaaS-scale apps.
		RegistryCredentialMax: 20,
		MinInstancesAllowed:   true,
		MaxInstancesAllowed:   true,
		// MaxMinInstances (ADR-071): Scale = 10 — half of
		// MaxConcurrency (20). At Scale's 1024 MB instance RAM
		// and 8 MB overhead, 10 instances resident = 10,320 MB
		// (~22% of the §6.2-2 47,600 MB ceiling), leaving
		// comfortable headroom for live wakes while still
		// delivering the "always-warm for traffic spikes" UX
		// the tier promises.
		MaxMinInstances: 10,
		// TrustedSignerCountMax: Scale is the regulated-workload
		// tier; 16 publishers covers "every platform team's CI
		// plus break-glass" without letting the table grow into
		// config-management territory. The byte-size cap on each
		// key (1024 bytes per migration 00083 CHECK) keeps the
		// table on-disk under ~16 KiB regardless.
		TrustedSignerCountMax: 16,
		// Soft ceiling: the binding constraint on Scale is the per-payload
		// byte cap (1 MiB), not the row count.
		MaxQueueDepth:               100,
		MaxDelayedTasksPerApp:       1_000_000,
		MaxSourceBytesPerInvocation: 1024 * 1024,
		AsyncInvokeAllowed:          true,
		// Scale: 25 attempts. The highest tier gets the most
		// tolerance so an upstream outage lasting a few minutes
		// doesn't dump the queue into dead_letter on a single bad
		// minute. Above 25 is irrational — that's 2 minutes at the
		// 5 s default backoff.
		MaxQueueAttempts: 25,
		// ADR-031: Scale gets 64 CIDR entries — broad enough for
		// SaaS-scale apps with many upstream integrations; doubling
		// the Pro budget tracks the doubling in DeployedApps (25 -> 100).
		EgressAllowlistAllowed: true,
		EgressAllowlistMaxSize: 64,
		// Autoscale: Scale gets both targets; same rationale as Pro.
		ScaleUpTargetRPSAllowed: true,
		// Cron: Scale gets 100 per-app and 500 per-account. 5× Pro's
		// per-app ceiling (20→100) and 10× Pro's per-account ceiling
		// (50→500); the per-account figure absorbs 5× Scale-tier apps
		// at the per-app cap, the typical SaaS fan-out.
		CronLimitPerApp:     100,
		CronLimitPerAccount: 500,
		// IAM-5 (issue #189): Scale gets 200 keys — 2 per app across
		// the Scale app budget (100) plus a per-team allowance, with
		// headroom for the rotating-CI shape of a SaaS-scale customer.
		KeysMax: 200,
		// IAM-6 / ADR-061: PR 1 placeholder. PR 2 populates actual
		// per-plan values from the financial model.
		OrgMembersMax:            0,
		OrgPendingInvitationsMax: 0,
		// Alert rules (issue #396): Scale gets 25 per-app and 100
		// per-account — 2.5× Pro's per-app (10→25) and ~3× the
		// per-account (30→100). Scale's app budget is 4× Pro's, so
		// the per-account figure absorbs the fan-out.
		AlertRuleLimitPerApp:     25,
		AlertRuleLimitPerAccount: 100,
		// Per-account rate limit (ADR-040): Scale gets 5000/min — ~10× the
		// Scale per-app rps (500). The fleet-summed alert at 100/min/5m
		// (FaasPerAccountRateLimitSpike) triggers well before any single
		// paid customer's bucket fills, which is the intended signal:
		// coordinated abuse, not baseline load.
		RateLimitPerAccountRPM: 5000,
		// Log deployment filter (issue #517 / PR-B): Scale gets 50
		// — 5× Pro (10→50), tracks Scale's larger app budget
		// (100 apps vs Pro's 25) and the multi-region staging fan-out
		// SaaS-scale customers typically run.
		LogDeploymentFilterMax:  50,
		ScaleUpTargetCPUAllowed: true,
		// CPU fairness (issue #301): Scale weight=16, quota 1000ms/1000ms
		// — i.e. the full bandwidth of one core. Scale runs 20 concurrent
		// VMs by plan, so the slice's aggregate quota is 20× at burst.
		// The kernel cpu.weight ratio with the lower tiers keeps single
		// VM bursts from monopolising the parent slice.
		CPUWeight:   16,
		CPUQuotaUS:  1_000_000,
		CPUPeriodUS: 1_000_000,
		// Streaming (issue #471 / ADR-047): Scale is paid-tier
		// streaming — same cap as Hobby/Pro. 100 MB / 900 s is
		// already the LLM-token-stream ceiling; Scale customers who
		// need >100 MB are rare (large JSON exports are dwarfed by
		// the per-instance egress bandwidth cap of 250 Mbit/s). A
		// future PR can lift this if telemetry shows Scale customers
		// tripping the cap.
		StreamingEnabled:            true,
		MaxResponseBodyBytes:        100 * 1024 * 1024,
		ResponseWriteTimeoutSeconds: 900,
		// Warm-snapshot (issue #470 / ADR-055): Scale stays on
		// by default — the per-app parked footprint cost fits
		// inside the 452 GB budget, and the customer's wake-p50
		// win is the largest dollar lever for SaaS workloads.
		WarmSnapshotEnabled:            true,
		WarmSnapshotMinRequestsDefault: 5,
		WarmSnapshotMinMsDefault:       2000,
	},
}

// Global platform constants (spec §1, §13). These are the physics of the one
// box; code enforces them, telemetry verifies them.
const (
	// RAM ledger (megabytes).
	HostOSReserveMB       = 2_048  // system.slice
	ControlPlaneReserveMB = 6_144  // faas-cp.slice
	TenantRAMBudgetMB     = 56_000 // tenant budget
	TenantSliceMaxMB      = 57_344 // faas-tenant.slice memory.max hard fence
	// RAMAdmissionCeilingMB is 85% of the tenant budget — schedd admits only up
	// to this (spec §1, §4.3, invariant §6.2-2).
	RAMAdmissionCeilingMB = 47_600
	// PerVMOverheadMB is added to every instance's ram_mb for admission and
	// billing (VMM + jailer + TAP slack, spec §1, §4.7).
	PerVMOverheadMB = 8

	// FloorDecisionIntervalSeconds (issue #557 / ADR-071 §Decision 1)
	// is the cadence at which the proactive floor trigger in
	// pkg/sched/floor wakes instances up to the per-app floor. 1 s
	// is the customer-facing promise: a Hobby customer who PATCHes
	// min_instances=1 must see one RUNNING instance within one
	// second. Tunable via FAAS_FLOOR_INTERVAL_SECONDS at schedd.
	FloorDecisionIntervalSeconds = 1

	// MaxFloorBackoffSeconds (ADR-071 §Decision 4) caps the per-app
	// exponential backoff the floor trigger applies on a non-nil
	// AdmitInstance error. 60 s bounds the FAILED-row hazard on a
	// RAM-saturated box: a stuck ceiling produces at most ~6 FAILED
	// rows per app per hour, not 3,600 (one per second).
	MaxFloorBackoffSeconds = 60

	// CPU (spec §1).
	CPUOvercommit = 8
	VCPUSlots     = 160

	// Metering (spec §1, §10).
	OverageMillicentsPerGBHour = 1_000 // €0.01 per GB-RAM-hour

	// Builder VM (spec §4.5, §1). Builds live in the control-plane slice, never
	// tenant RAM.
	BuildVMRAMMB           = 2_048
	BuildVMVCPU            = 2
	BuildTimeoutSeconds    = 600 // 10 min build
	BuildE2ETimeoutSeconds = 900 // 15 min end-to-end

	// Snapshots / disk (spec §1, §8).
	FleetSnapshotAvgTargetMB = 130 // business metric; alert >160 warn, >200 page
	SnapshotBudgetGB         = 452
	// SnapshotBudgetAlarmPct is the lv-fc percentage at which the nightly
	// imaged GC switches from per-app retention (keep current+previous
	// deployments per app) to fleet budget pressure (evict from the
	// biggest-over-quota accounts first). Matches spec §12. NaN lv-fc
	// readings (lvs missing on dev/macOS) short-circuit the pressure branch.
	SnapshotBudgetAlarmPct = 90.0
	// SnapshotStaleRetention is how long a snapshot lives in stale state
	// after the F2 FC-version sweep marks it before imaged evicts it
	// (F-07). Spec §4.4 + ADR-005: stale snapshots must remain
	// restore-able for a brief window so an operator rollback across a
	// firecracker upgrade doesn't pay an extra cold boot. 7 days is the
	// v1 box's typical reset cycle.
	SnapshotStaleRetention = 7 * 24 * time.Hour
	// LvFcName is the LVM logical volume apps + snapshots live on (spec §8).
	// Schedd's dashboard gauge shells out to `lvs -o data_percent <LvFcName>`
	// to populate `fcvm_lv_fc_used_pct`. Empty on dev/macOS — the
	// DefaultLvFcUsedPct closure returns 0 and the gauge degrades to "no data".
	LvFcName = "lv-fc"

	// Characterization boot (ADR-051 §"Characterization window"). On the
	// first cold boot of a new deployment, guest-init observes what the
	// app binds, runs L7 probes, and ships a report over AF_VSOCK
	// STREAM (port 1026 / msgtype 3). Both bounds live here so a single
	// edit moves the whole observation window; the guest and the host
	// mirror against this single source.
	//
	// CharacterizationDeadline bounds the GUEST's observation window
	// (guest/init/characterize_linux.go::waitForBind). 10 s covers the
	// L7 probe budget (2 s) + shipReport's 4-attempt retry budget
	// (~1.85 s with backoff) + headroom for a slow customer app boot.
	// Never below the host's wait (CharacterizationHostDeadline) — the
	// guest gives up earlier than the host and both sides fall back to
	// the scan-hint class without failing the deploy (per
	// ADR-051 §"Failure messages become specific").
	CharacterizationDeadline = 10 * time.Second
	// CharacterizationHostDeadline bounds the HOST's
	// WaitCharacterizationReport dial+read inside Wake
	// (pkg/fcvm/manager.go::characterizationWait). 4 s gives margin
	// for the guest's 4 shipReport attempts + slow vsock proxies on
	// nested KVM (Lima caveat, spec §14).
	CharacterizationHostDeadline = 4 * time.Second

	// LogRingBufferBytes is the capacity of the Supervisor's
	// stdout/stderr ring buffer (ADR-051 Phase 4 Slice A PR-B).
	// 64 KiB covers the boot-time tail of any realistic customer
	// app (a Node cold start, a Python import chain) without
	// forcing a multi-page journal capture on every cold boot.
	// Characterized at plan time: a typical FastAPI app's
	// first-second log volume is ~4-8 KiB, so 64 KiB preserves the
	// entire boot window with margin. The wire-side truncation
	// (VsockCharacterizationMaxBody, 32 KiB) still clamps the
	// reported LogTail; this buffer is the over-budget source the
	// report reads from. A future bump to VsockCharacterizationMaxBody
	// must be matched here.
	LogRingBufferBytes = 64 * 1024

	// Build artifact export (M6): vmmd loopback-mounts the chroot-local drive1
	// on Destroy to copy out /build/out/image.tar (and friends). 4 GiB is
	// well above the §14 target (~130 MB) so it's not the limiting factor; it's
	// the ceiling we refuse to copy past. See pkg/fcvm/vmm.go::exportBuildArtifacts.
	MaxExportedLayerBytes int64 = 4 << 30

	// Edge request caps (spec §4.1).
	MaxRequestBodyBytes = 25 * 1024 * 1024 // 25 MB either direction
	WakeQueueCap        = 512              // per-app wake queue
	WakeQueueTTLSeconds = 30

	// API-key lifetime (issue #189 / IAM-5). New non-admin keys
	// minted by createKey get `expires_at = now + DefaultAPIKeyLifetimeDays`.
	// 365 days is the issue-189 spec: long enough to be
	// "set-and-forget" for a customer's CI rotation, short enough
	// that an exfiltrated key expires within a year of theft even
	// without rotation. Admin keys default to nil expiry (per
	// existing admin semantics — never expire, must be explicitly
	// revoked). Legacy admin keys (pre-IAM-5) keep null expiry
	// forever; rotation is the migration path for customers who
	// want a finite window on their admin keys.
	DefaultAPIKeyLifetimeDays = 365

	// DefaultAPIKeyGraceWindowDays (issue #189 / IAM-5) is the
	// plan-level default for the rotation grace window. 7 days
	// gives the customer's CI / staging / prod fleet one
	// rotation cycle to switch over without coordinated downtime.
	// The per-account override (accounts.key_grace_window_days)
	// takes precedence; 0 in the per-account column means
	// "atomic revocation" (no grace).
	DefaultAPIKeyGraceWindowDays = 7

	// Sidecar containers (issue #463 / ADR-070). The 2-sidecar
	// hard cap is a GLOBAL constant, not a per-plan matrix field.
	// Every plan inherits the same `SidecarCapMax = 2` (Free
	// included). The cap is structurally tight: 1 init + 1
	// sidecar is the smallest useful surface for a stateless
	// workload, and the schema CHECK on `deployments.sidecars`
	// (migration 00118) pins the cap at the second-line defence
	// layer (migrations/00118_deployments_sidecars.sql). A future
	// PR can grow this to a per-plan matrix if telemetry shows
	// demand — the constant is the single source of truth.
	SidecarCapMax = 2

	// Streaming response caps (issue #471 / ADR-047). Free stays on the
	// 25 MB / 300 s envelope (spec §4.1 baseline) so the abuse-floor
	// tier can't pin a long stream against the box. Hobby/Pro/Scale
	// raise the cap to 100 MB / 900 s so LLM token streams (typical
	// 30–120 s chat completions) and large JSON/CSV exports have
	// headroom. Limits.MaxResponseBodyBytes /
	// Limits.ResponseWriteTimeoutSeconds override these defaults per
	// plan; 0 in those fields falls back to the *Default constants
	// below so a missing plan row fails closed to the spec baseline
	// rather than inheriting a paid tier's relaxed cap.
	MaxResponseBodyBytesDefault   int64 = 25 * 1024 * 1024 // 25 MB (spec §4.1)
	ResponseWriteTimeoutDefault         = 300              // 300 s (spec §4.1)
	StreamingFlushBytesDefault          = 256 * 1024       // 256 KiB flush window (ADR-047)
	StreamingFlushIntervalDefault       = 200 * time.Millisecond

	// OCI puller (spec §17 G1, ADR-021). Per-pull HTTP timeout for the
	// registry client. cmd/imaged passes this to oci.WithTimeout; the
	// daemon may override at boot via FAAS_OCI_PULL_TIMEOUT_SECONDS but
	// there is no per-deployment knob — every plan shares the same
	// ceiling so the cold-boot latency contract (§14, wake < 350 ms)
	// stays predictable. 60s is well above the largest manifest +
	// image-config GET and a generous safety margin over the
	// fail-fast PullImageConfig path.
	OCIPullTimeoutSeconds = 60

	// Idle timeout tuning (spec §4.3): app-configurable down to this floor, and
	// no higher than plan default × this multiplier.
	IdleTimeoutFloorSeconds = 10
	IdleTimeoutMaxMultiple  = 2

	// Autoscale (issue #169 / §17 G8). ScaleUpDecisionIntervalSeconds
	// is the trigger's tick rate — 1 s balances "admit the Nth
	// instance before the gateway wake queue builds" against "don't
	// hammer Postgres with a full app list on every tick". ScaleUpWindowSeconds
	// is the rolling RPS window — 5 s is the smallest window that
	// smooths a single-tick spike without lagging so much that a burst
	// is already over by the time the trigger fires.
	ScaleUpDecisionIntervalSeconds = 1
	ScaleUpWindowSeconds           = 5

	// Scaling policy cooldowns (issue #462 / ADR-058). The
	// customer-facing knobs are `scale_out_cooldown_s` /
	// `scale_in_cooldown_s` on the wire; the floor / ceiling
	// constants below are the admission time clamp apid uses to
	// validate the PATCH. The floors prevent a self-DoS via
	// `cooldown_s: 0` (the engine would otherwise admit every
	// request inside the same tick). The ceilings bound the
	// customer against accidentally making the engine inert
	// (24 h ceiling on scale-in is the maximum a customer
	// reasonably wants to dampen oscillation — anything longer
	// is a "stuck running" footgun).
	//
	// MinScaleOutCooldownS = 1 (1 s floor — the engine's tick is
	//   1 s, so 0 would always be honored as "now" and 1 is the
	//   smallest strictly-positive value).
	//
	// MaxScaleOutCooldownS = 3600 (1 h ceiling — any longer makes
	//   a legitimate burst unresponsive; 1 h is the practical
	//   upper bound for a "shock absorber" knob).
	//
	// MinScaleInCooldownS = 5 (5 s floor — matches the reaper's
	//   5 s floor on `ReapIdle`, so a manually-tuned scale-in
	//   cooldown cannot be tighter than the reaper's idle window).
	//
	// MaxScaleInCooldownS = 86400 (1 day ceiling — the customer
	//   who wants a "never scale-in" knob uses max_instances = 0
	//   via the legacy code path; values >= 1 day are degenerate
	//   but legal and the engine clamps to today+1d internally).
	MinScaleOutCooldownS = 1
	MaxScaleOutCooldownS = 3600
	MinScaleInCooldownS  = 5
	MaxScaleInCooldownS  = 86400

	// Tier A4 (cross-node app rebalance, ADR-064 follow-up to
	// ADR-062): pacing + per-tick cap on pkg/sched/rebalancer.go.
	//
	// RebalanceCooldownSeconds is the minimum gap between two
	// successful reassignments of the same app. A flap-loop
	// (operator toggles compute_nodes.active=false / true rapidly)
	// is suppressed by stamping apps.reassigned_at and filtering
	//   now() - reassigned_at < RebalanceCooldownSeconds.
	// Defaults to 60s; tunable via FAAS_REBALANCE_COOLDOWN_SECONDS
	// (cmd/schedd/config.go reads the env, the live watcher
	// stamps the value through Store.ListOrphanedApps's bound
	// parameter).
	//
	// RebalanceMaxPerTickPerNode caps the per-drain-event batch so
	// a 5,000-app orphaned node doesn't monopolise the schedd
	// worker pool. Excess apps stay pinned; the next
	// compute_node_changed event retries (heartbeat-staleness also
	// re-fires). Tunable via FAAS_REBALANCE_MAX_PER_TICK.
	RebalanceCooldownSeconds   = 60
	RebalanceMaxPerTickPerNode = 50

	// Tier A5 (cross-node live-instance migration, ADR-070
	// follow-up to ADR-064): pacing + lease window on
	// pkg/sched/migration_handoff.go.
	//
	// MigrateLiveMaxPerTick caps the per-drain-event batch so a
	// node with 500 RUNNING instances doesn't monopolise the
	// schedd worker pool. The rebalancer breaks each candidate
	// instance into a fresh four-phase handoff; excess candidates
	// stay on the dead node and retry on the next
	// compute_node_changed re-fire. Defaults to 10; tunable via
	// FAAS_MIGRATE_LIVE_MAX_PER_TICK (env-overridable, see
	// cmd/schedd/main.go::runWithDeps; propagated via
	// Engine.WithMigrateLiveConfig).
	//
	// MigrateLiveLeaseSeconds is the upper bound on the four-phase
	// handoff — Phase 1 mints a lease_token, Phase 3 commits or
	// the lease expires. The dying vmmd resumes the VM on lease
	// expiry (the snapshot stays). Tuned to comfortably exceed the
	// snapshot-upload + restore round-trip on the OCIRegistry
	// backend (latency dominated by the registry pull, not the
	// local VM lifecycle). Defaults to 90s; tunable via
	// FAAS_MIGRATE_LIVE_LEASE_SECONDS (env-overridable, see
	// cmd/schedd/main.go::runWithDeps; propagated via
	// Engine.WithMigrateLiveLeaseSeconds).
	//
	// Hard limits policy (CLAUDE.md): every limit is a constant
	// here, never inlined.
	MigrateLiveMaxPerTick   = 10
	MigrateLiveLeaseSeconds = 90

	// Tier A6 (migrating-instance watchdog, ADR-067 follow-up to
	// ADR-070): self-heal stuck state='migrating' rows that
	// never committed (the new owner vmmd died mid-handoff, the
	// network partition dropped the gRPC, the operator killed
	// the new owner before the commit). The watchdog is the
	// only writer that can move a row out of 'migrating' without
	// a peer commit — every Phase 4 path (CancelInstanceMigration)
	// requires a peer, and the peer is the very thing that's gone.
	//
	// MigratingWatchdogTickLimit is the per-tick cap on the
	// reconcile batch. A backlog of stuck rows past this cap is
	// itself a "you broke something" event (the metric fires
	// `outcome="cap_exceeded"`); a backlog over 50 means a peer
	// dropped tens of migrations in flight and the operator
	// should investigate before the next drain. Defaults to 50;
	// tunable via FAAS_MIGRATING_WATCHDOG_TICK_LIMIT (env-
	// overridable, see cmd/schedd/main.go::runWithDeps).
	//
	// MigratingWatchdogIntervalSeconds is the per-tick cadence
	// of the watchdog. Default 1s; matches the existing reaper /
	// cron tick. A 1s cadence is overkill for a 90s lease window
	// but matches the existing pattern (every other 1s tick in
	// pkg/sched/loop.go is the same shape). Tunable via
	// FAAS_MIGRATING_WATCHDOG_INTERVAL_SECONDS.
	//
	// Hard limits policy (CLAUDE.md): every limit is a constant
	// here, never inlined.
	MigratingWatchdogTickLimit       = 50
	MigratingWatchdogIntervalSeconds = 1

	// Tier A7 (edge split — gatewayd-public / gatewayd-internal,
	// ADR-070): drain + replica registry + warm-hint-cache tunables.
	//
	// GatewayDrainGraceSeconds is the upper bound on the in-flight
	// request window after SIGTERM before http.Server.Shutdown
	// returns. Tuned to be 5s shorter than the systemd unit's
	// TimeoutStopSec (60s) so the daemon exits cleanly inside the
	// unit's grace. Tunable via FAAS_GATEWAY_DRAIN_GRACE_SECONDS.
	//
	// ReplicaHeartbeatIntervalSeconds is the cadence at which an
	// internal daemon re-asserts its presence to the public daemon
	// over the /run/faas/gatewayd-public.sock unix socket. The
	// public daemon marks a peer unready after 2× this interval
	// without a heartbeat (no extra constant — the 2x factor lives
	// in cmd/gatewayd-public/replicas.go::isStale). Tunable via
	// FAAS_REPLICA_HEARTBEAT_SECONDS.
	//
	// WarmHintCacheSize caps the per-daemon in-memory mirror of the
	// `warm_hint` table. Hot apps fit in the first 1000 entries; a
	// larger cache trades RAM for cold-miss latency. Tunable via
	// FAAS_WARM_HINT_CACHE_SIZE.
	//
	// CertSyncIntervalSeconds is the leader-side safety-net cron
	// cadence for the legacy certsync replicator (the fast-path is
	// the certmagic OnEvent callback). 30s is the worst-case lag
	// a follower replica carries a stale cert; in steady state
	// the OnEvent fast path keeps lag ≤1s. Tunable via
	// FAAS_CERT_SYNC_INTERVAL_SECONDS.
	//
	// Legacy daemon only (revised 2026-08-04): the certsync
	// replicator + this constant are owned by `cmd/gatewayd/` for
	// the migration window. PR #633 stripped certsync from
	// `gatewayd-public`; PR-C will sweep the constant + the
	// `pkg/gateway/certsync` package together once the legacy
	// daemon is retired.
	//
	// Hard limits policy (CLAUDE.md): every limit is a constant
	// here, never inlined.
	GatewayDrainGraceSeconds        = 25
	ReplicaHeartbeatIntervalSeconds = 5
	WarmHintCacheSize               = 1000
	CertSyncIntervalSeconds         = 30

	// Free-tier disk reaper (spec §4.3): zero requests this long => EVICTED_COLD.
	FreeTierColdEvictDays = 14

	// Instance retention (spec §17 follow-up, PR #74): STOPPED/FAILED
	// rows are DELETED by pkg/sched.Retention this long after entering
	// the terminal state. Tunable in cmd/schedd config; this default is
	// the spec baseline (30 days). Retention only touches terminal
	// instances — it never affects quota/RAM/concurrency counts because
	// those only sum non-terminal rows (state/machine.go CountsFor*).
	DefaultInstanceRetention = 30 * 24 * time.Hour
	// DefaultRetentionInterval is how often the retention sweep actually
	// runs. Once per hour is plenty — the sweep itself reads now-30d, so
	// hourly cadence means a row that just crossed 30d is deleted within
	// the next hour.
	DefaultRetentionInterval = 1 * time.Hour

	// DefaultDiskDriftInterval is the cadence for the read-only
	// /srv/fc/snap vs DB size-tracking drift sweep (PR scale-out
	// readiness #3). Hourly matches DefaultRetentionInterval so the
	// two hourly tickers fire on aligned boundaries and don't drift
	// apart by minute-precision. The sweep never writes — it only
	// increments OpsMetrics.SnapshotDiskDrift when a disk-vs-DB
	// discrepancy is observed.
	DefaultDiskDriftInterval = 1 * time.Hour

	// WarmAffinityTTL is how long pkg/sched.WarmAffinity remembers the
	// last-warm compute node for an app (placement scheduler, ADR-025).
	// The chooser biases a wake toward the remembered node so a hot
	// app's snapshot + page cache stay warm (ADR-009). 30 minutes
	// matches the Pro plan idle-timeout default — a hot app on a
	// 30-minute TTL keeps the snapshot warm across one reaper cycle.
	// Overridable via FAAS_WARM_AFFINITY_TTL at the schedd daemon.
	// Sticky-warm is bias, never a gate (ADR-005: cold boot must
	// always work); an expired or missing hint falls through to
	// least-loaded RAM headroom.
	WarmAffinityTTL = 30 * time.Minute

	// DefaultConntrackCap is the spec §7 per-instance conntrack cap
	// (docs/faas_implementation_spec.md:344). One platform-wide number;
	// not per-plan tiered — every tenant sees the same cap because the
	// failure mode (host conntrack exhaustion) is a single shared
	// resource. ADR-018 deferred the enforcement to this PR; the value
	// is the spec literal. vmmd wires it into netns.Config at every
	// Wake (pkg/fcvm/manager.go:236) and the nft rule that consumes
	// it lives in pkg/netns/config.go::NftCommands.
	DefaultConntrackCap = 4096

	// ConntrackCap is the spec §7 per-instance conntrack cap value.
	// Use ConntrackCapProbe() at runtime to get the effective value,
	// which falls back to 0 on kernels without per-netns conntrack
	// support (CONFIG_NF_CONNTRACK_NET_NS=n). The egress tc cap is
	// unaffected.
	ConntrackCap = DefaultConntrackCap
)

// DefaultComputeNodeCeilingMB is the per-compute-node admission ceiling
// schedd hands out when no operator override is present. It mirrors
// RAMAdmissionCeilingMB (85% of the tenant budget) because a single
// compute node today owns the entire tenant slice on the one-box; when
// a future multi-node world splits tenant traffic across nodes, this
// helper is the single place to revisit (e.g. per-node share = ceiling
// / node count). Migrated from inline literals in
// pkg/state/memstore.go:seedDefaultLocalNodeLocked and
// cmd/vmmd/config.go:LoadConfig (PR scale-out readiness #4). The
// helper resolves to the same integer today — no behavior change.
func DefaultComputeNodeCeilingMB() int {
	return RAMAdmissionCeilingMB
}

// ConntrackCapProbe returns the effective per-instance conntrack cap.
const (
	probeNS        = "faas-ct-probe"
	probeTable     = "faas_ct_probe"
	probeFamily    = "ip"
	probeChain     = "forward"
	probeNftCmd    = "nft"
	probeNftAdd    = "add"
	probeNetnsExec = "exec"
	probeNetnsCmd  = "netns"
)

// Returns DefaultConntrackCap when the kernel supports the ct expression
// inside network namespaces (CONFIG_NF_CONNTRACK_NET_NS=y); returns 0
// when it doesn't so the ct cap rules are silently omitted (egress tc
// cap is unaffected). Callers call this once at setup and cache the
// result — the kernel conntrack netns capability never changes at runtime.
func ConntrackCapProbe() int64 {
	// Skip probe in tests: tests that don't use metal don't need netns,
	// and metal tests create their own netns under leakcheck supervision.
	if testing.Testing() {
		return DefaultConntrackCap
	}
	bail := func() int64 { return 0 }

	// Clean up any stale probe namespace from a previous crash.
	if _, err := os.Stat("/run/netns/" + probeNS); err == nil {
		go func() { _, _ = execCmd("ip", probeNetnsCmd, "del", probeNS) }()
	}

	// Create a temporary netns for the probe.
	if _, err := execCmd("ip", probeNetnsCmd, "add", probeNS); err != nil {
		// Cannot create netns at all (e.g. Lima nested virt). Disable.
		return bail()
	}
	// Unconditional delete regardless of outcome.
	go func() { _, _ = execCmd("ip", probeNetnsCmd, "del", probeNS) }()

	// Quick probe: add a table + a rule using "ct state" (simpler than
	// "ct count over") inside the netns. If the kernel lacks conntrack
	// netns support, nft returns "No such file or directory".
	probe := func(expr string) bool {
		cmds := [][]string{
			{"ip", probeNetnsCmd, probeNetnsExec, probeNS, probeNftCmd, probeNftAdd, "table", probeFamily, probeTable},
			{"ip", probeNetnsCmd, probeNetnsExec, probeNS, probeNftCmd, probeNftAdd, "chain", probeFamily, probeTable, probeChain,
				"{", "type", "filter", "hook", probeChain, "priority", "filter", ";", "policy", "accept", ";", "}"},
			{"ip", probeNetnsCmd, probeNetnsExec, probeNS, probeNftCmd, probeNftAdd, "rule", probeFamily, probeTable, probeChain, expr},
		}
		for _, cmd := range cmds {
			if _, err := execCmd(cmd[0], cmd[1:]...); err != nil {
				return false
			}
		}
		return true
	}

	if probe("ct state established,related accept") && probe("ct count over 4096") {
		return DefaultConntrackCap
	}
	return bail()
}

// execCmd runs argv and returns combined output. Isolated here so
// limits.go stays a pure config package without external syscall
// imports polluting its API surface.
func execCmd(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// LimitsFor returns the limits for a plan and whether the plan is known. Callers
// that already trust the plan (e.g. read from a CHECK-constrained column) can use
// MustLimitsFor.
func LimitsFor(p Plan) (Limits, bool) {
	l, ok := planLimits[p]
	return l, ok
}

// MustLimitsFor returns the limits for a plan and panics on an unknown plan.
// Use only where the plan is already validated (DB CHECK constraint upstream).
func MustLimitsFor(p Plan) Limits {
	l, ok := planLimits[p]
	if !ok {
		panic(fmt.Sprintf("api: unknown plan %q", p))
	}
	return l
}

// PlanIncludedGBHours returns the included GB-RAM-hours per calendar month
// for the plan. Returns 0 for unknown plans so callers default to "no
// quota band" rather than treating unknown as Free. The meter aggregator
// (pkg/meter.CheckQuota) compares monthly usage against this number.
func (p Plan) PlanIncludedGBHours() int {
	l, ok := LimitsFor(p)
	if !ok {
		return 0
	}
	return l.IncludedGBHours
}

// Valid reports whether p is a known plan.
func (p Plan) Valid() bool {
	_, ok := planLimits[p]
	return ok
}

// IsPaid reports whether the plan is a paid tier (hobby/pro/scale).
// Free is the only non-paid plan; the changePlan handler (cmd/apid
// handlers_ext.go) uses this to decide whether an API-only upgrade
// requires a Stripe subscription item (issue #142).
func (p Plan) IsPaid() bool {
	return p == PlanHobby || p == PlanPro || p == PlanScale
}

// RequiresStripeUpgradeTo reports whether moving from p → next counts as
// a paid-upgrade that needs a Stripe subscription item. Downgrades
// (any → free) and same-tier moves return false; the customer can
// always downgrade without Stripe. The only free → paid direct path
// is free → hobby (the v0 upgrade); free → pro/scale and any
// hobby → pro/scale and pro → scale require a Stripe subscription item.
//
// The Stripe webhook is the legitimate path to set a paid plan — it
// stamps StripeSubscriptionItem on the account record before the plan
// change, so the same handler that rejects free → pro for an
// API-key-only call accepts free → pro when the Stripe item is set.
//
// Fail-closed on unknown plans: an unknown `from` (e.g. a future
// enterprise tier added without updating this switch) returns true so
// the 402 gate fires — a missing case must never silently let a
// customer upgrade without billing. Reviewers: keep this default in
// place if you extend the switch above.
func (p Plan) RequiresStripeUpgradeTo(next Plan) bool {
	if !next.Valid() {
		return false // caller's plan.Valid() check already covers this
	}
	switch p {
	case PlanFree:
		return next == PlanPro || next == PlanScale
	case PlanHobby:
		return next == PlanPro || next == PlanScale
	case PlanPro:
		return next == PlanScale
	case PlanScale:
		return false
	default:
		return true // unknown source plan: require Stripe, do not silently allow
	}
}

// MinInstancesAllowed reports whether the plan may set the per-app
// cold-wake floor (ux_spec §6.5). Hobby + Pro + Scale opt in; Free
// stays scale-to-zero by default. apid's updateApp handler gates
// `req.MinInstances` on this; the CLI surfaces the rejection with
// CodePlanMinInstancesNotAllowed. PR-A history (issue #462 / ADR-058):
// Hobby unlocked at PR-A. The pre-#462 contract was Pro + Scale only;
// the tier-up landed because the bill auto-counts via
// pkg/meter/sampler.go:238-239 and Hobby's MaxConcurrency is bounded
// (2) so the worst-case residency cost is 2 × RAMMB + 16 MB overhead.
func (p Plan) MinInstancesAllowed() bool {
	l, ok := LimitsFor(p)
	if !ok {
		return false
	}
	return l.MinInstancesAllowed
}

// MaxInstancesAllowed (issue #462 / ADR-058) reports whether the
// plan may set a per-app live-instances ceiling. Mirrors the
// MinInstancesAllowed tier-up: Hobby + Pro + Scale opt in; Free
// stays off. The value the customer passes is bounded above by
// the plan's MaxConcurrency (which is the existing hard cap on the
// wake path), so the gate here is the plan-tier lock, not the
// value-lock. apid's updateApp handler gates
// `req.ScalingPolicy.MaxInstances` on this; the CLI surfaces the
// rejection with CodePlanMaxInstancesNotAllowed.
func (p Plan) MaxInstancesAllowed() bool {
	l, ok := LimitsFor(p)
	if !ok {
		return false
	}
	return l.MaxInstancesAllowed
}

// SidecarAllowed (issue #463 / ADR-070 §Decision 1) reports whether
// the plan may attach sidecars to a deployment. PR-A's accessor
// returns true for every plan — the load-bearing gate is the GLOBAL
// `SidecarCapMax` constant, not a per-plan matrix. A future PR
// may grow this to a per-plan field (e.g. Free = 0, Hobby/Pro/Scale
// = 2) if telemetry shows Free-tier abuse; for PR-A the method
// exists so the apid handler can read a single source of truth
// without inlining the global cap. The companion
// `ErrSidecarNotAllowedOnPlan` constructor is reserved for that
// future per-plan gate.
func (p Plan) SidecarAllowed() bool {
	return true
}

// EgressAllowlistAllowed reports whether the plan may set a per-app
// outbound IP allowlist (ADR-031). Pro + Scale opt in; Free + Hobby
// stay off — the abuse-desk hygiene this surface gives is a paid
// concern. apid's updateApp handler gates `req.EgressAllowlist` on
// this; the CLI surfaces the rejection with
// CodePlanEgressAllowlistNotAllowed. Unknown plans fail closed
// (return false) so a missing row never silently unlocks a
// premium feature — same contract as MinInstancesAllowed above.
func (p Plan) EgressAllowlistAllowed() bool {
	l, ok := LimitsFor(p)
	if !ok {
		return false
	}
	return l.EgressAllowlistAllowed
}

// EgressAllowlistMaxSize returns the per-plan CIDR-entry cap for an
// allowlist (ADR-031). 0 for Free/Hobby (the gate above rejects
// before this matters); 16 for Pro; 64 for Scale. apid rejects a
// PATCH whose `req.EgressAllowlist` has more entries with 400
// egress_allowlist_too_long. Returning 0 on unknown plans makes a
// missing plan row a fail-closed denial, not a silent default.
func (p Plan) EgressAllowlistMaxSize() int {
	l, ok := LimitsFor(p)
	if !ok {
		return 0
	}
	return l.EgressAllowlistMaxSize
}

// StreamingEnabled reports whether the plan defaults the per-app
// streaming_enabled column to true (issue #471 / ADR-047). Hobby/Pro/
// Scale opt in; Free stays off (spec §4.1 baseline; Free is the
// abuse-floor tier where an unbounded stream would let one app pin
// gatewayd). The plan-level default is applied at CreateApp time in
// cmd/apid/handlers.go::buildApp; an existing app may still flip the
// flag via PATCH (gated by StreamingResponseAllowed so Free stays off
// even when an admin backfills the column). Unknown plans fail closed
// (return false) — same contract as MinInstancesAllowed /
// EgressAllowlistAllowed.
func (p Plan) StreamingEnabled() bool {
	l, ok := LimitsFor(p)
	if !ok {
		return false
	}
	return l.StreamingEnabled
}

// StreamingResponseAllowed reports whether the plan permits a customer
// to set apps.streaming_enabled=true via PATCH. Hobby+ opt in; Free
// returns false so apid's updateApp handler can surface 403
// plan_streaming_not_allowed (issue #471 AC #3). Same fail-closed
// contract as StreamingEnabled above.
func (p Plan) StreamingResponseAllowed() bool {
	l, ok := LimitsFor(p)
	if !ok {
		return false
	}
	return l.StreamingEnabled
}

// WarmSnapshotEnabled reports whether the plan's default for the
// per-app two-tier snapshot flag is on. Pro/Scale return true; Free /
// Hobby return false. The accessor is fail-closed — an unknown plan
// reads as false, matching the Free default. Used by buildApp in
// cmd/apid/handlers.go to populate a brand-new app's flag.
//
// Issue #470 / ADR-055: the equivalent gate ("can the customer opt in
// to warm-snapshot?") lives on WarmSnapshotAllowed (separate method
// below) so Free + Hobby PATCH-true can be rejected cleanly without
// conflating the default and the gate.
func (p Plan) WarmSnapshotEnabled() bool {
	l, ok := LimitsFor(p)
	if !ok {
		return false
	}
	return l.WarmSnapshotEnabled
}

// WarmSnapshotAllowed reports whether the plan permits a customer to
// set apps.warm_snapshot_enabled=true via PATCH. Pro/Scale return true;
// Free / Hobby return false so apid's updateApp handler can surface
// 403 plan_warm_snapshot_not_allowed. Customers on any plan may PATCH
// true → false (opt-out per-app).
func (p Plan) WarmSnapshotAllowed() bool {
	l, ok := LimitsFor(p)
	if !ok {
		return false
	}
	return l.WarmSnapshotEnabled
}

// WarmSnapshotMinRequestsDefault returns the per-plan default for the
// per-app request-count threshold. Pro/Scale: 5. Free/Hobby: 0 (the
// column default — unused because warm-snapshot is gated off).
func (p Plan) WarmSnapshotMinRequestsDefault() int {
	l, ok := LimitsFor(p)
	if !ok {
		return 0
	}
	return l.WarmSnapshotMinRequestsDefault
}

// WarmSnapshotMinMsDefault returns the per-plan default for the per-app
// time-since-first-ready threshold (ms). Pro/Scale: 2000. Free/Hobby:
// 0 (unused). Used by buildApp in cmd/apid/handlers.go.
func (p Plan) WarmSnapshotMinMsDefault() int {
	l, ok := LimitsFor(p)
	if !ok {
		return 0
	}
	return l.WarmSnapshotMinMsDefault
}

// MaxResponseBodyBytes returns the per-response body cap in bytes for
// this plan, falling back to MaxResponseBodyBytesDefault (spec §4.1's
// 25 MB) when the plan row's field is unset or the plan is unknown.
// The default is the strict spec baseline (a guest cannot exceed it);
// when limits are missing the cap clamps to that baseline rather
// than dropping to a permissive ceiling. Used by gatewayd to wrap
// the response writer in http.MaxBytesWriter at this number (PR-B
// activates it on the streaming path; PR-A's buffered path stays
// under the cap naturally).
func (p Plan) MaxResponseBodyBytes() int64 {
	l, ok := LimitsFor(p)
	if !ok {
		return MaxResponseBodyBytesDefault
	}
	if l.MaxResponseBodyBytes <= 0 {
		return MaxResponseBodyBytesDefault
	}
	return l.MaxResponseBodyBytes
}

// ResponseWriteTimeout returns the per-response write timeout for this
// plan, falling back to ResponseWriteTimeoutDefault (spec §4.1's 300 s)
// when the plan row's field is unset. Same fail-closed shape as
// MaxResponseBodyBytes. Used by gatewayd to configure http.Server
// .WriteTimeout so a single response cannot pin the listener.
func (p Plan) ResponseWriteTimeout() time.Duration {
	l, ok := LimitsFor(p)
	if !ok {
		return time.Duration(ResponseWriteTimeoutDefault) * time.Second
	}
	if l.ResponseWriteTimeoutSeconds <= 0 {
		return time.Duration(ResponseWriteTimeoutDefault) * time.Second
	}
	return time.Duration(l.ResponseWriteTimeoutSeconds) * time.Second
}

// CronLimitPerApp returns the per-app cron cap for the plan (spec §4.4).
// 0 for Free (the handler returns 402 ErrPlanCronsNotAllowed before
// the store is touched) and a positive value for Hobby/Pro/Scale.
// Unknown plans fail closed (return 0) — same contract as
// EgressAllowlistMaxSize above.
func (p Plan) CronLimitPerApp() int {
	l, ok := LimitsFor(p)
	if !ok {
		return 0
	}
	return l.CronLimitPerApp
}

// CronLimitPerAccount returns the per-account cron cap for the plan
// (spec §4.4). Independent of CronLimitPerApp — defends against the
// N-apps-times-cap-per-app bypass. 0 for Free; positive for paid
// tiers. Unknown plans fail closed (return 0) — same contract as
// CronLimitPerApp above.
func (p Plan) CronLimitPerAccount() int {
	l, ok := LimitsFor(p)
	if !ok {
		return 0
	}
	return l.CronLimitPerAccount
}

// KeysMax returns the per-account API-key cap for the plan
// (issue #189 / IAM-5). Free 3, Hobby 10, Pro 50, Scale 200. The
// handler enforces the cap at createKey (409
// api_key_limit_exceeded); rotateKey is quota-neutral and is allowed
// at the cap. Revoked keys (status='revoked') are excluded from the
// count so the customer's historical lineage doesn't pin them out of
// quota. Unknown plans fail closed (return 0) — same contract as
// CronLimitPerAccount above.
func (p Plan) KeysMax() int {
	l, ok := LimitsFor(p)
	if !ok {
		return 0
	}
	return l.KeysMax
}

// AlertRuleLimitPerApp returns the per-app alert-rule cap for the
// plan (issue #396 / ADR-045). 0 for Free (handler returns 402
// CodePlanAlertRulesNotAllowed before the store is touched);
// positive for Hobby/Pro/Scale. Account-wide rules (AppID == "")
// bypass this; only the per-account cap applies. Same fail-closed
// contract as CronLimitPerApp.
func (p Plan) AlertRuleLimitPerApp() int {
	l, ok := LimitsFor(p)
	if !ok {
		return 0
	}
	return l.AlertRuleLimitPerApp
}

// AlertRuleLimitPerAccount returns the per-account alert-rule cap.
// Independent of AlertRuleLimitPerApp — same N-apps-times-cap-per-app
// defence the cron shape used. Same fail-closed contract as
// CronLimitPerAccount.
func (p Plan) AlertRuleLimitPerAccount() int {
	l, ok := LimitsFor(p)
	if !ok {
		return 0
	}
	return l.AlertRuleLimitPerAccount
}

// TrustedSignerCountMax returns the per-app cosign trusted-publisher
// cap for the plan (issue #472 / ADR-054). 0 for Free (the open-deploy
// posture for Free means customers on Free never need require_signed=true
// and so never need signers either); positive for Hobby/Pro/Scale.
// Unknown plans fail closed (return 0) — same contract as the cron +
// alert-rule getters above.
func (p Plan) TrustedSignerCountMax() int {
	l, ok := LimitsFor(p)
	if !ok {
		return 0
	}
	return l.TrustedSignerCountMax
}

// MaxMinInstances returns the per-plan cap on the per-app
// cold-wake floor (issue #557 / ADR-071). Free 0, Hobby 1, Pro 3,
// Scale 10. The apid updateApp handler rejects values above this
// with CodeMaxMinInstancesExceeded (422) carrying the limit + the
// observed value + a docs URL — the CLI renders the rejection
// with actionable retry guidance. Unknown plans fail closed
// (return 0) — same contract as TrustedSignerCountMax.
func (p Plan) MaxMinInstances() int {
	l, ok := LimitsFor(p)
	if !ok {
		return 0
	}
	return l.MaxMinInstances
}

// ConcurrencyPerVMBound returns the platform-advertised upper
// bound on concurrent in-flight requests one VM can handle at the
// listener layer (issue #559). Free 1, Hobby 5, Pro 25, Scale 80.
// Distinct from MaxConcurrency (the per-app *instance* cap, spec
// §6.2-1) — this is per-VM. Unknown plans fail closed (return 0) —
// same contract as MaxMinInstances above.
func (p Plan) ConcurrencyPerVMBound() int {
	l, ok := LimitsFor(p)
	if !ok {
		return 0
	}
	return l.ConcurrencyPerVMBound
}

// LogDeploymentFilterMax returns the per-plan cap on the
// `?deployment=` filter the customer may scope their app-logs
// stream to (issue #517 / PR-B, AC3). Free returns 0 so the
// handler rejects with `plan_deployment_filter_not_allowed`; paid
// tiers return 1 / 10 / 50 (Hobby/Pro/Scale). Unknown plans fail
// closed (return 0) — same contract as CronLimitPerApp above.
func (p Plan) LogDeploymentFilterMax() int {
	l, ok := LimitsFor(p)
	if !ok {
		return 0
	}
	return l.LogDeploymentFilterMax
}

// OrgMembersMax returns the per-non-personal-org member cap for the
// plan (issue #190 / IAM-6 / ADR-061). 0 for unknown plans — the
// fail-closed contract mirrors CronLimitPerApp above. The handler
// gates membership creation on this accessor and surfaces 403
// org_member_cap_exceeded once the cap is reached; the store still
// checks the same value as a defence-in-depth back-stop. PR 1 ships
// 0 for every plan; PR 2 sets the actual per-plan values from the
// financial model.
func (p Plan) OrgMembersMax() int {
	l, ok := LimitsFor(p)
	if !ok {
		return 0
	}
	return l.OrgMembersMax
}

// OrgPendingInvitationsMax returns the per-non-personal-org pending
// invitation cap for the plan (issue #190 / IAM-6 / ADR-061).
// Independent of OrgMembersMax — defends against the N-invites ×
// fast-accept botnet signature. Same fail-closed contract as
// OrgMembersMax above.
func (p Plan) OrgPendingInvitationsMax() int {
	l, ok := LimitsFor(p)
	if !ok {
		return 0
	}
	return l.OrgPendingInvitationsMax
}

// RateLimitPerAccountRPM returns the per-account requests/minute cap for
// the plan (ADR-040 / issue #292). Independent of RateLimitRPS/Burst
// which are per-app — defends against the N-apps-times-cap-per-app
// botnet signature. 0 for unknown plans (fail closed; the limiter math
// then returns zero rps and zero burst, refusing all traffic) — same
// contract as CronLimitPerAccount above.
func (p Plan) RateLimitPerAccountRPM() int {
	l, ok := LimitsFor(p)
	if !ok {
		return 0
	}
	return l.RateLimitPerAccountRPM
}

// ScaleUpTargetRPSAllowed reports whether the plan may set
// `autoscale_target_rps` (issue #169 / #172). Hobby + Pro + Scale opt
// in; Free stays off. apid's updateApp handler gates `req.AutoscaleTargetRPS`
// on this and surfaces the rejection with CodePlanScaleUpNotAllowed.
// Unknown plans fail closed (return false) — same contract as
// MinInstancesAllowed / EgressAllowlistAllowed.
func (p Plan) ScaleUpTargetRPSAllowed() bool {
	l, ok := LimitsFor(p)
	if !ok {
		return false
	}
	return l.ScaleUpTargetRPSAllowed
}

// ScaleUpTargetCPUAllowed reports whether the plan may set
// `autoscale_target_cpu_pct`. Pro + Scale opt in; Free + Hobby stay
// off (cost shape of "scale on CPU without a min_instances floor"
// is unbounded on the cheaper tiers). Same fail-closed contract as
// ScaleUpTargetRPSAllowed above.
func (p Plan) ScaleUpTargetCPUAllowed() bool {
	l, ok := LimitsFor(p)
	if !ok {
		return false
	}
	return l.ScaleUpTargetCPUAllowed
}

// SliceName returns the systemd sub-slice name for this plan. The
// 3-level cgroup hierarchy (issue #301 / ADR-044) is
//
//	/sys/fs/cgroup/faas-tenant.slice/<sliceName>/<instance>
//
// systemd drops the per-plan sub-slice at boot via
// deploy/ansible/roles/systemd_slices (4 copies: free/hobby/pro/scale);
// the jailer expects the parent dir to exist before it creates the
// per-instance scope. The slice name is the canonical form
// "tenant-<plan>" — it carries the "tenant-" prefix so the
// faas.rules.yml `slice=~"tenant-.*"` matcher stays stable for any
// future tenant-customer slice hierarchy. Unknown plans return the
// empty string so call sites fail closed (jailer will reject a zero
// parent cgroup path) rather than silently writing the wrong scope.
func (p Plan) SliceName() string {
	switch p {
	case PlanFree:
		return "tenant-free"
	case PlanHobby:
		return "tenant-hobby"
	case PlanPro:
		return "tenant-pro"
	case PlanScale:
		return "tenant-scale"
	default:
		return ""
	}
}

// CPUWeight returns the kernel cpu.weight value for the plan, used as
// the jailer `--cgroup cpu.weight=N` argv (issue #301 / ADR-044). The
// ratio 2:4:8:16 (Free:Hobby:Pro:Scale) ensures a Scale-customer
// burst can preempt a Free-customer burst but never starves them out of
// their weight. Unknown plans fail closed (return 100 — the kernel
// default) so a missing Limits row never silently disables the cgroup
// weight; the cpu.max quota still bounds the impact.
func (p Plan) CPUWeight() int {
	l, ok := LimitsFor(p)
	if !ok {
		return 100
	}
	return l.CPUWeight
}

// CPUQuotaUS returns the cpu.max quota half (microseconds) for the
// plan — written directly to the per-instance cpu.max file. Issue
// #301 spec: Free 100ms / Hobby 200ms / Pro 500ms / Scale 1000ms.
// Unknown plans fail closed (return 0 — disabled quota, which the
// kernel treats as "no limit", so a misconfigured plan is detectable
// in dashboards rather than silently denied).
func (p Plan) CPUQuotaUS() int {
	l, ok := LimitsFor(p)
	if !ok {
		return 0
	}
	return l.CPUQuotaUS
}

// CPUPeriodUS returns the cpu.max period half (microseconds) for the
// plan. Always equal to CPUQuotaUS for the issue #301 spec — the
// potential quota is "<period> microseconds per <period>". Unknown
// plans fail closed (return 100_000 — the standard default period),
// which then makes the quota half easy to reason about even when the
// row is missing.
func (p Plan) CPUPeriodUS() int {
	l, ok := LimitsFor(p)
	if !ok {
		return 100_000
	}
	return l.CPUPeriodUS
}

// AdmissionMB is the RAM an instance charges against the admission ceiling and
// tenant slice: its plan RAM plus the fixed per-VM overhead (spec §4.3, §6.2-2).
func (l Limits) AdmissionMB() int {
	return BillableRAMMB(l.RAMMB)
}

// BillableRAMMB returns the RAM one instance charges against both the admission
// ceiling (schedd's ledger, invariant §6.2-2) and the metering ledger (meterd's
// sampler, spec §4.7): the customer's configured ram_mb plus the fixed per-VM
// overhead. Single source of truth — every site that previously inlined
// `ram_mb + PerVMOverheadMB` now goes through this helper so a future change
// to the overhead constant updates exactly one place.
func BillableRAMMB(ramMB int) int {
	return ramMB + PerVMOverheadMB
}

// BillableRAMMBWithSidecars is the sidecar-shape variant of
// BillableRAMMB (issue #463 / ADR-070 §Decision 6). The billable
// shutter is `plan.RAMMB + Σ(sidecar.ram_mb) + PerVMOverheadMB`:
// sidecars share the per-VM overhead (one netns, one cgroup
// scope per instance), but each sidecar contributes its own
// RAM to the admission ceiling. Caller is responsible for
// enforcing the SidecarCapMax bounds — this helper is purely
// the arithmetic.
//
// PR-A defines the math; PR-B wires the consumer (schedd's
// admission ledger + meterd's sampler). The sibling helper
// (no-sidecars form) is BillableRAMMB — both shapes coexist.
// A future cleanup can fold the single-arg form into this
// helper as a variadic / empty-slice overload, but for PR-A
// the two-form separation keeps the no-sidecar call sites
// unambiguous.
func BillableRAMMBWithSidecars(ramMB int, sidecarMBs []int) int {
	total := ramMB + PerVMOverheadMB
	for _, m := range sidecarMBs {
		if m > 0 {
			total += m
		}
	}
	return total
}

// IdleTimeoutBounds returns the [floor, ceiling] seconds a customer may configure
// their idle timeout to for this plan (spec §4.3).
func (l Limits) IdleTimeoutBounds() (floor, ceiling int) {
	return IdleTimeoutFloorSeconds, l.IdleTimeoutS * IdleTimeoutMaxMultiple
}
