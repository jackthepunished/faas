package state

import (
	"encoding/json"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/onebox-faas/faas/pkg/api"
)

// Domain types mirroring the schema (spec §5). These are the rows apid and
// schedd read and write; the Store abstracts the actual Postgres access (sqlc
// in production, the in-memory store in tests).

// AccountStatus tracks billing/dunning state (spec §4.7).
type AccountStatus string

const (
	AccountActive         AccountStatus = "active"
	AccountPastDue        AccountStatus = "past_due"
	AccountSuspended      AccountStatus = "suspended"
	AccountDeletedPending AccountStatus = "deleted_pending"
)

// AppType distinguishes a plain App from a Function (spec §2, ADR-003).
type AppType string

const (
	AppTypeApp      AppType = "app"
	AppTypeFunction AppType = "function"
)

// AppStatus is the app's lifecycle (distinct from an instance's State).
type AppStatus string

const (
	AppActive      AppStatus = "active"
	AppEvictedCold AppStatus = "evicted_cold"
	AppDeleted     AppStatus = "deleted"
)

// DeploymentKind distinguishes image / tarball / dockerfile deploys (spec §9).
type DeploymentKind string

const (
	DeploymentKindImage      DeploymentKind = "image"
	DeploymentKindTarball    DeploymentKind = "tarball"
	DeploymentKindDockerfile DeploymentKind = "dockerfile"
)

// DeploymentStatus tracks a deployment through the pipeline (spec §5, §9).
type DeploymentStatus string

const (
	DeployPending      DeploymentStatus = "pending"
	DeployBuilding     DeploymentStatus = "building"
	DeployImaging      DeploymentStatus = "imaging"
	DeploySnapshotting DeploymentStatus = "snapshotting"
	DeployLive         DeploymentStatus = "live"
	DeployFailed       DeploymentStatus = "failed"
	DeploySuperseded   DeploymentStatus = "superseded"
)

// BuildStatus tracks the build row's lifecycle (spec §9).
type BuildStatus string

const (
	BuildQueued    BuildStatus = "queued"
	BuildRunning   BuildStatus = "running"
	BuildSucceeded BuildStatus = "succeeded"
	BuildFailed    BuildStatus = "failed"
)

// FailureClass tags the cause of a build failure (spec §9).
type FailureClass string

const (
	FailureOOM       FailureClass = "oom"
	FailureTimeout   FailureClass = "timeout"
	FailureUserError FailureClass = "user_error"
	FailureInfra     FailureClass = "infra"
)

// Account is a customer account.
type Account struct {
	ID     string
	Email  string
	Plan   api.Plan
	Status AccountStatus
	// ProviderCustomerID is the per-account `cus_…` returned by Stripe when
	// the customer signs up (spec §4.7). The unique index makes it a
	// stable webhook lookup key.
	ProviderCustomerID string
	// StripeSubscriptionItem is the per-account `si_…` (metered
	// subscription item) that meterd pushes hourly usage against
	// (issue #52, §4.7). Empty until pkg/billing/stripe::EnsureCustomer
	// receives the customer.subscription.created webhook and stamps it.
	// PushUsageRecord skips when this is blank so a customer that hasn't
	// subscribed yet never lands on the billing dashboard.
	StripeSubscriptionItem string
	CreatedAt              time.Time
	// DeletionRequestedAt is stamped when the customer schedules the
	// account for deletion (G6, ADR-021). NULL on every row that has
	// never been scheduled. pkg/grace uses it to decide whether the
	// 30-day grace window has lapsed and a hard delete should run.
	DeletionRequestedAt *time.Time
	// LastQuotaWarningAt is the UTC day (midnight-truncated timestamptz)
	// the meterd quota loop last emitted a `quota_warning` pg_notify for
	// this account (spec §4.7). The dedupe gate at quota.go reads +
	// stamps this column atomically so a paid-tier overage produces
	// exactly one warning event per UTC day — across daemon restarts.
	// NULL on every row that has never tripped.
	LastQuotaWarningAt *time.Time
	// PastDueAt is the moment the account entered `past_due` (set by
	// the apid invoice.payment_failed webhook). pkg/meter.Dunning uses
	// it as the anchor for the 7-day past_due → suspended and 21-day
	// suspended → deleted_pending transitions. NULL on accounts that
	// have never been past_due.
	PastDueAt *time.Time
	// MFAEnrolledAt is stamped by /v1/account/mfa/confirm on the first
	// successful TOTP verification. NULL = never enrolled. The gate
	// the login handlers check is (MFARequired && MFAEnrolledAt == nil)
	// — issued as an mfa_pending session cookie. See ADR-035 and the
	// IAM-2 plan in issue #186.
	MFAEnrolledAt *time.Time
	// MFASecretEncrypted is the age-sealed base32 TOTP secret produced
	// by pkg/auth/totp.GenerateSecret and sealed in pkg/secretbox
	// (same host age key as app_secrets). The plaintext never enters
	// logs or audit; the envelope is decrypted only inside the verify
	// handler. NULL when MFAEnrolledAt is NULL. CHECK constraint
	// accounts_mfa_enrolled_shape_chk enforces the (enrolled ⇒
	// secret+recovery present) shape at the DB layer.
	MFASecretEncrypted []byte
	// MFARecoveryCodesHash is the array of SHA-256 hashes of the
	// customer's 10 single-use recovery codes. Consumed (and removed)
	// by /v1/account/mfa/recover via SELECT FOR UPDATE + UPDATE,
	// because Postgres bytea[] has no array-diffing write. Stored as
	// bytea[] so the consume path is a single-row serialised update.
	MFARecoveryCodesHash [][]byte
	// MFARequired is the policy flag set by the three chokepoints:
	// plan upgrade, card attached, 2nd deploy. The customer clears it
	// only by completing /enroll + /confirm (MarkMFAEnrolled flips it
	// to false on the first successful confirm) or by /disable. API
	// keys ignore this column per the IAM-2 design decision (keys are
	// already cryptographically scoped).
	MFARequired bool
}

// Active reports whether the account may deploy (not suspended/deleted).
func (a Account) Active() bool { return a.Status == AccountActive || a.Status == AccountPastDue }

// MFAEnrolled reports whether the customer has at least one
// successful TOTP confirmation. Distinct from MFARequired: a
// customer who has enrolled is no longer blocked even if a future
// plan change again sets MFARequired=true. The LATCH is on
// MFAEnrolled, not MFARequired — the chokepoints set required=true,
// the customer clears it once via /confirm, and the chokepoints
// re-arm on the next trigger.
func (a Account) MFAEnrolled() bool { return a.MFAEnrolledAt != nil }

// APIKey is a hashed, account-scoped credential. Scopes is the set of
// authorization scopes attached to the key (e.g. "admin", "read", "write");
// the apid middleware checks them on every authenticated request. See
// ADR-034 and the IAM-1 plan.
type APIKey struct {
	ID         string
	AccountID  string
	Hash       []byte
	Label      string
	Scopes     []string
	LastUsedAt time.Time
	CreatedAt  time.Time
}

// App is a deployed application (or function). The Manifest carries the
// runner-scaffold payload (env, healthz path, entrypoint) the guest-init
// consumes inside the microVM (spec §4.6, §4.9).
type App struct {
	ID             string
	AccountID      string
	Slug           string
	Type           AppType
	Runtime        string // node22|python312|go124|go124-alpine for functions
	RAMMB          int
	IdleTimeoutS   int // 0 => plan default
	MaxConcurrency int
	// MinInstances is the per-app floor the reaper honors when parking
	// idle instances (ux_spec §6.5). 0 => scale to zero (default);
	// >0 => keep at least this many RUNNING instances alive regardless
	// of idle timeout. Pro/Scale only — the apid updateApp handler
	// rejects Hobby/Free with 403 plan_min_instances_not_allowed.
	MinInstances int
	// EgressAllowlist is the per-app outbound CIDR allowlist (ADR-031,
	// tier-2 of the network roadmap). Empty => no allowlist rule
	// emitted, current behaviour preserved; non-empty => the per-netns
	// forward chain gains an `iifname tap0 ip daddr { … } accept`
	// rule after the lateral-movement deny. v4 only in v1 (the v6
	// mirror is a separate ADR). Plan-gated: Free/Hobby always read
	// empty (apid updateApp rejects PATCH with 403
	// plan_egress_allowlist_not_allowed); Pro max 16 entries; Scale
	// max 64 entries — see pkg/api/limits.go.
	EgressAllowlist []netip.Prefix
	// AutoscaleTargetRPS is the per-instance RPS target for the
	// reactive scale-up trigger (issue #169 / #172). 0 means
	// "disabled" — the trigger skips this app. Plan-gated upstream
	// (Free returns 403 plan_autoscale_not_allowed); apid enforces
	// value > 0 in [1, max-int]. Hobby/Pro/Scale only. When
	// measured RPS / live_instance_count exceeds this, schedd admits
	// another instance up to plan.MaxConcurrency.
	AutoscaleTargetRPS int
	// AutoscaleTargetCPUPct is the per-instance CPU% target (1..100)
	// for the scale-up trigger. 0 means "disabled". Pro/Scale only
	// (the plan gate is stricter than RPS — Hobby does not get CPU
	// because the cost shape is unbounded on the cheaper tiers).
	// Signal source is pkg/sched/instancestats.Reader (PR #205); a
	// nil reader falls back to RPS-only mode and this target is
	// silently skipped.
	AutoscaleTargetCPUPct int
	Status                AppStatus
	Manifest              AppManifest
	CreatedAt             time.Time
}

// AppManifest is the runner-scaffold payload. Stored as jsonb in Postgres;
// lives inside the snapshot for guest-init.
type AppManifest struct {
	Entrypoint []string          `json:"entrypoint,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	WorkingDir string            `json:"working_dir,omitempty"`
	Port       int               `json:"port,omitempty"`
	Healthz    string            `json:"healthz,omitempty"`
	User       string            `json:"user,omitempty"`
}

// GitHubBinding is the (app → github_installation) edge persisted on
// the apps row by the /oauth/callback handler after it verifies the
// installation against api.github.com (ADR-012, review finding #1+#2
// closure). githubd reads this via the BindingsLookup interface so
// CheckRun writes go out under the right installation token instead
// of the hardcoded install_id=1 placeholder that M7.5 shipped with.
//
// PR-B adds AccountID + BindingID + LinkedAt so the bind row carries
// the (account → app → install) shape and the dashboard's "connected
// on" pill has a single source.
type GitHubBinding struct {
	AppID            string
	AccountID        string
	BindingID        string
	LinkedAt         time.Time
	InstallID        int64
	RepoFullName     string
	ProductionBranch string
}

// GitHubInstall is the durable OAuth handshake state for one account's
// GitHub App install (PR-C, audit-gap closure). Pre-PR-C this lived
// only in pkg/githubd/realservice.go's in-memory `s.installs` map
// and evaporated on kill -TERM, breaking the dashboard's
// /v1/install/repos/list + /v1/apps/{slug}/install/bind paths with
// 502s the moment githubd restarted. PR-C moves the source of truth
// to the github_installations table (migration 00059).
//
// AccountID is the PK (a uuid, references accounts(id) ON DELETE
// CASCADE — GDPR §17 G2 path deletes the row when the owning account
// goes away). InstallationID is GitHub's int64. DefaultBranch is
// captured at the OAuth handshake so the bind picker doesn't need a
// re-fetch. SealedToken holds the age-encrypted install token (the
// "ghs_…" form, minted via AppAuth.ExchangeInstallationToken and
// sealed with pkg/secretbox.SealOne before persisting — the
// plaintext token never touches the database). TokenExpiresAt is
// when the sealed blob expires (GitHub's install tokens are 1 h);
// cold-start readers unseal via pkg/secretbox.Open only when
// expires_at > now()+30s, otherwise re-mint + re-seal. SealedAt
// records when this row's sealed blob was last written (telemetry
// only — rotation cadence). AuditGithubLogin is the §11 paper trail
// on the durable row: the GitHub login who owned the install at
// seal time, used by cold-start re-verification to assert the
// session envelope's expected_login matches the durable record.
type GitHubInstall struct {
	AccountID        string
	InstallationID   int64
	DefaultBranch    string
	SealedToken      []byte
	TokenExpiresAt   time.Time
	SealedAt         time.Time
	AuditGithubLogin string
}

// MarshalJSON encodes a zero-value Manifest as {} so the jsonb default
// round-trips cleanly.
func (m AppManifest) MarshalJSON() ([]byte, error) {
	type alias AppManifest
	if m.Entrypoint == nil && m.Env == nil && m.WorkingDir == "" && m.Port == 0 && m.Healthz == "" && m.User == "" {
		return []byte("{}"), nil
	}
	return json.Marshal(alias(m))
}

// Deployment is one attempt to ship a version of an app.
type Deployment struct {
	ID          string
	AppID       string
	BuildID     string // empty for image: deploys
	ImageDigest string
	Kind        DeploymentKind
	SourcePath  string // tarball spool path (kind=tarball|dockerfile)
	SourceBytes int64
	Handler     string // function handler (kind=tarball when type=function)
	LogPath     string // build log spool path
	// SourceURL is the canonical upstream URL the build was triggered
	// from (Tier 3 / issue #197 B3.10). For githubd-triggered deploys
	// this is the repo + branch; for registry pulls it is the OCI
	// reference; for tarball / dockerfile deploys it is empty.
	// Populated by githubd's CreateDeployment callback. Phase 2 reads
	// it for build_provenance.source_url.
	SourceURL string
	// CommitSHA is the upstream commit SHA (when known). Length-bounded
	// at 64 hex chars in the DB (deployments_commit_sha_len_chk,
	// migrations/00047). Empty for image/tarball deploys that don't
	// have an upstream commit.
	CommitSHA string
	// RootfsPath / RootfsBytes are stamped by imaged after the per-app ext4 layer
	// is built (spec §4.6, drive1). schedd's prime handshake reads this row so
	// it can attach drive1 from the right path on the cold boot (ADR-018).
	RootfsPath string
	// RootfsKey is the canonical StorageBackend key for the same layer
	// (issue #96 / ADR-025 axis 2, PR #116). Mirror column of
	// RootfsPath: every row carries both. schedd carries the key on the
	// wake wire; vmmd resolves it via Storage.Get and stages into the
	// jail chroot. Local backends map the key to the same file as
	// RootfsPath; remote backends (OCI registry) resolve over HTTP. The
	// key is stamped by imaged at the same time as RootfsPath (see
	// SetDeploymentRootfs) and backfilled by migrations/00025 from the
	// legacy path on the default apps root. Empty only on rows written
	// before the migration landed and whose apps root was non-default;
	// imaged re-stamps them on the next build via SetDeploymentRootfs.
	RootfsKey   string
	RootfsBytes int64
	Status      DeploymentStatus
	Error       string
	// ErrorCode is the RFC 7807 code stamped at the same time as
	// Error when a deployment transitions to `failed`. ADR-021:
	// oci.ErrImageNotFound / ErrImageEgressDenied /
	// ErrImageManifestInvalid map via pkg/api.SentinelToCode to
	// the stable codes that imaged writes here. Empty for every
	// other transition (and for deployments created before the
	// migrations/00021 column add).
	ErrorCode string
	CreatedAt time.Time
}

// Build is one build pipeline run for a deployment (spec §9). Builderd writes
// status transitions; apid only creates the queued row.
type Build struct {
	ID           string
	DeploymentID string
	Kind         DeploymentKind // railpack|dockerfile in production; we mirror kind here
	SourceBytes  int64
	Status       BuildStatus
	FailureClass FailureClass
	LogPath      string
	StartedAt    time.Time
	FinishedAt   time.Time
	EnqueuedAt   time.Time // set at CreateBuild; builderd measures queue wait against it (ADR-030)
}

// BuildProvenance is the post-mortem "what ran?" record for a Build
// (ADR-038, Tier 3 / issue #197 B3.1). Populated by builderd at the
// two markSucceeded sites; read by apid at GET /v1/builds/{id}/provenance
// and by the CLI at `faas build provenance <id>`.
//
// One row per build_id (UNIQUE constraint enforces it). Fields mirror
// the table shape with no translation; empty strings round-trip the
// nullable columns. sbom_storage_key is empty in this PR — Phase 3's
// syft populator fills it.
type BuildProvenance struct {
	ID             string
	BuildID        string
	BuildkitVer    string
	RailpackVer    string
	BaseDigest     string
	SourceSHA256   string
	SourceURL      string
	CommitSHA      string
	Plan           string
	RunnerDigest   string
	BuilderNodeID  string
	StartedAt      time.Time
	FinishedAt     time.Time
	SBOMStorageKey string
}

// CustomDomain is a customer's CNAME'd domain. apid owns this table;
// gatewayd reads it to decide whether to mint a cert (spec §4.1, §7).
type CustomDomain struct {
	Domain         string
	AppID          string
	ChallengeToken string
	VerifiedAt     time.Time // zero = unverified
}

// Verified reports whether the TXT challenge has been satisfied.
func (d CustomDomain) Verified() bool { return !d.VerifiedAt.IsZero() }

// Cron is a scheduled synthetic POST through gatewayd (spec §4.3).
type Cron struct {
	ID          string
	AppID       string
	Schedule    string // cron expression
	Path        string
	Enabled     bool
	CreatedAt   time.Time
	LastFiredAt time.Time // zero until first fire; updated by MarkCronFired
}

// InvocationSource tags the API surface that originated a row on the
// invocations table (Move 1 — async_invoke / queue / delayed_task / cron).
// Mirrored as a CHECK constraint in migrations/00030_invocations.sql.
type InvocationSource string

const (
	InvocationAsyncInvoke InvocationSource = "async_invoke"
	InvocationQueue       InvocationSource = "queue"
	InvocationDelayedTask InvocationSource = "delayed_task"
	InvocationCron        InvocationSource = "cron"
)

// InvocationState is the row lifecycle on the invocations table. The
// allowed transitions are pending→dispatching→completed (happy path)
// and pending/dispatching→failed or pending→cancelled (terminal). The
// CHECK constraint enforces the discrete values; the engine admits
// transitions only through the Store.ClaimInvocation / Complete / Fail /
// Cancel methods.
type InvocationState string

const (
	InvocationPending     InvocationState = "pending"
	InvocationDispatching InvocationState = "dispatching"
	InvocationCompleted   InvocationState = "completed"
	InvocationFailed      InvocationState = "failed"
	InvocationCancelled   InvocationState = "cancelled"
	// InvocationDeadLetter (issue #394 / Move 1) is the terminal state
	// for queue messages that exhausted their per-plan retry budget
	// (see pkg/api.Limits.MaxQueueAttempts). Rows reach this state only
	// via pkg/state.Store.FailInvocation with budget > 0; the drain
	// (pkg/sched/drain.go) is the sole writer in production. The
	// invocations_state_check CHECK constraint (migrations/00060) and
	// the invocations_app_dead_letter_idx partial index back the
	// reader surface (GET /v1/apps/{slug}/queues/dead_letter).
	InvocationDeadLetter InvocationState = "dead_letter"
)

// Invocation mirrors a row on the invocations table. apid writes
// customer-intent rows; schedd's drain loop owns state transitions
// pending → dispatching → completed/failed via the Store.Claim /
// Complete / Fail methods. InstanceID is NULL on the inbound INSERT path
// and is stamped by the drain's claim step (state→dispatching); the
// meter reads it via CountInstanceInvocationsInMinute to set
// usage_minutes.requests.
type Invocation struct {
	ID             string           `json:"id"`
	AppID          string           `json:"app_id"`
	AccountID      string           `json:"account_id"`
	InstanceID     string           `json:"instance_id,omitempty"`
	Source         InvocationSource `json:"source"`
	State          InvocationState  `json:"state"`
	Method         string           `json:"method"`
	Path           string           `json:"path"`
	Payload        json.RawMessage  `json:"payload"`
	Headers        json.RawMessage  `json:"headers"`
	DueAt          time.Time        `json:"due_at"`
	ScheduledAt    *time.Time       `json:"scheduled_at,omitempty"`
	CronID         *string          `json:"cron_id,omitempty"`
	AckURL         string           `json:"ack_url,omitempty"`
	Result         json.RawMessage  `json:"result,omitempty"`
	LeaseExpiresAt *time.Time       `json:"lease_expires_at,omitempty"`
	ReceivedAt     *time.Time       `json:"received_at,omitempty"`
	CompletedAt    *time.Time       `json:"completed_at,omitempty"`
	Attempts       int              `json:"attempts"`
	LastError      string           `json:"last_error,omitempty"`
	CreatedAt      time.Time        `json:"created_at"`
}

// QueueStats is the projection returned by Store.QueueState (issue
// #394 / Move 1 dead-letter). It is the read-side mirror of the three
// counters apid's GET /v1/apps/{slug}/queues/state surfaces to the
// customer:
//
//	Depth:          pending + dispatching rows on the per-app queue.
//	                Same numerator the apid cap check uses on POST
//	                .../queues/send (CountPendingInvocations), so the
//	                two numbers cannot disagree without a race.
//	InFlight:       dispatching rows with lease_expires_at either
//	                NULL or in the future. A row whose lease has
//	                expired is treated as effectively pending again —
//	                the next drain tick will re-claim it. Mapping
//	                makes "in_flight" a tight upper bound on the
//	                worker queue, excluding zombie leases.
//	OldestPendingAt: zero-time when no pending rows exist. cmd/apid
//	                  translates this to a nil pointer + omitempty
//	                  on the JSON wire so dashboards can render
//	                  "queue is empty" cleanly.
type QueueStats struct {
	Depth           int
	InFlight        int
	OldestPendingAt time.Time
}

// GdprAction enumerates the GDPR self-service actions recorded in
// the gdpr_requests ledger. The DB CHECK constraint enforces these
// three values; exporting the constants avoids typo bugs in apid +
// schedd callers.
type GdprAction string

const (
	GdprActionExport  GdprAction = "export"
	GdprActionDelete  GdprAction = "delete"
	GdprActionRestore GdprAction = "restore"
)

// GdprRequest is one row of the gdpr_requests ledger. Inserted on
// the customer-facing path; completed_at is stamped after the
// downstream action (export bundle returned, DeleteAccount fired,
// restore succeeded). The ledger is INSERT-only from the application
// side; the table survives the account's DeleteAccount so a DPO can
// audit completed erasure against an email + timestamp.
type GdprRequest struct {
	ID           string
	AccountID    string
	AccountEmail string
	Action       GdprAction
	RequestedAt  time.Time
	CompletedAt  time.Time // zero until the downstream action completes
}

// Instance mirrors the instances row; schedd is the sole writer (spec §6).
type Instance struct {
	ID            string
	AppID         string
	DeploymentID  string
	State         string
	Netns         string
	GuestUID      int
	HostIP        string
	RAMMB         int
	StartedAt     time.Time
	LastRequestAt time.Time
	ParkedAt      time.Time
	// TerminalAt is stamped by Engine.transition on the same UPDATE that
	// writes state = 'stopped' or 'failed' (PR #74, spec §17 follow-up).
	// It is the dedicated retention anchor: started_at means "row
	// creation" and parked_at is overloaded (also means "entered
	// PARKED"). A STOPPED row whose vmmd boot succeeded 25 days ago
	// has a stale started_at; terminal_at is the only correct age.
	// The retention sweep (pkg/sched.Retention) DELETEs rows where
	// state ∈ {STOPPED, FAILED} AND terminal_at < now-30d.
	TerminalAt *time.Time
	// NodeID is the compute_node the instance lives on
	// (issue #97 / ADR-025 axis 3). Set by Engine.Wake via
	// sched.ChoosePlacement at instance creation; read by Park /
	// snapshotAndPark to route the vmmd RPC through the right
	// target URL. NOT NULL enforced by migrations/00024_compute_nodes;
	// pre-existing rows were backfilled to DefaultLocalNodeID.
	// Empty in test fixtures only when the fixture is exercising a
	// pre-#97 code path that predates the column add.
	NodeID string
	// WakeID is the per-wake-attempt correlation handle (gaps analysis
	// 2026-07-23). Distinct from ID (the row PK): every fresh WAKING /
	// COLD_BOOTING transition mints a new UUIDv7 Go-side in schedd's
	// Engine.Wake so a single instance row can carry many wake_ids over
	// its lifetime as the app parks and wakes again. UUIDv7 (time-ordered)
	// picked over UUIDv4 so the partial index `(app_id, wake_id)` on the
	// dashboard's recent-wakes scan serves time-range queries without a
	// separate sort. NOT NULL enforced by migrations/00028_instances_wake_id;
	// pre-existing rows were backfilled to gen_random_uuid() (v4) on apply.
	// Empty in test fixtures only when the fixture predates the column add.
	WakeID string
}

// ComputeNode is one vmmd host in the fleet (issue #97 / ADR-025 axis
// 3). schedd's single-leader CP owns placement across N rows; the
// legacy single-host deployment has exactly one row (the synthetic
// 'default-local' seeded by migrations/00024_compute_nodes.sql).
// Operators register additional nodes via cmd/apid's
// POST /v1/compute-nodes admin endpoint; the heartbeat loop in
// cmd/schedd/main.go keeps LastHeartbeatAt fresh on a tick.
//
// The struct's field names track the SQL columns 1:1; Active == false
// is a runtime "drained" flag (placement skips), distinct from a row
// delete (re-registration is idempotent on conflict).
type ComputeNode struct {
	ID                 string
	Name               string
	TargetURL          string // wire.ParseTarget-compatible
	VPCPUs             int
	MemMB              int
	MaxConcurrency     int
	AdmissionCeilingMB int
	Active             bool
	LastHeartbeatAt    time.Time
	CreatedAt          time.Time
}

// InstanceTouch is one entry in a last_request_at flush batch (spec §4.1). The
// gateway accumulates these in memory and hands them to schedd every 15 s.
type InstanceTouch struct {
	InstanceID  string
	LastRequest time.Time
}

// Event is one row in the append-only audit log (spec §6.1).
type Event struct {
	ID      int64
	At      time.Time
	Actor   string
	Kind    string
	Subject *uuid.UUID
	Data    json.RawMessage
}

// Usage is one row of monthly usage (spec §10). meterd is the writer in
// production; for tests we seed rows directly.
type Usage struct {
	AccountID string
	AppID     string
	Month     time.Time // truncated to month
	MBSeconds int64
	// CPUUsec is the cumulative host cgroup CPU-µs consumed by
	// this app in this month (issue #279 / PR-B). Measurement
	// only — billing is on plan RAM. Populated by UsageByMonth
	// from the usage_monthly view; zero on the mb-only legacy
	// rows.
	CPUUsec  int64
	Requests int64
}

// Invoice is one persisted invoice from a billing provider (issue #259,
// BILLING: plan comparison + invoice history). Rows arrive via the
// webhook ingestion path (PR B); the read API and dashboard read this
// table. Per-account filter is enforced by the Store method that
// returns this type — never expose the cross-account scan.
//
// Money is integer cents in the provider's currency; the financial
// model distills to EUR at the API edge. Currency is preserved per
// row so future multi-currency support can land without a backfill.
//
// HostedURL is intentionally NOT exposed on the read surface — the
// column lives in invoices.hosted_url for PR-B audit only. Provider
// invoice URLs and PDF URLs are session-scoped; we never hand them to
// the customer via this API.
type Invoice struct {
	ID                string
	AccountID         string
	Provider          string // "stripe" | "paddle"
	ProviderInvoiceID string
	Number            string
	Status            string // "draft" | "open" | "paid" | "uncollectible" | "void"
	PeriodStart       time.Time
	PeriodEnd         time.Time
	SubtotalCents     int64
	TaxCents          int64
	TotalCents        int64
	AmountPaidCents   int64
	Currency          string
	PDFAvailable      bool
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// AccountCredit is one positive-cents balance issued by an operator
// via POST /v1/admin/accounts/{id}/credits (issue #279). cents_remaining
// is decremented at consumption time (the consumption reducer is the
// PR #323 invoice-finalization follow-up; this PR only lands the
// issuance surface). Cents is integer — never float on money (CLAUDE.md).
//
// @migration 00049 creates this table. expires_at is optional; a NULL
// expiry means the credit is valid until fully consumed. The active
// partial index (where cents_remaining > 0) speeds up the
// "consume next credit" query when the consumption reducer lands.
type AccountCredit struct {
	ID             string
	AccountID      string
	CentsRemaining int64
	Reason         string
	CreatedAt      time.Time
	ExpiresAt      *time.Time
}

// CreditLedgerEntry is one immutable row in the append-only audit log
// of credit deltas (issue #279). One row is inserted per issuance
// (delta positive) and per consumption (delta negative, when the
// consumption reducer lands). The handler always supplies a reason
// text and the operator account ID as actor; the row is never
// updated or deleted by code convention (no surface grants a write).
//
// @migration 00049 creates this table. ON DELETE CASCADE on account_id
// and credit_id so GDPR DeleteAccount scrubs both tables in the same
// transaction that scrubs the rest of the customer's data.
//
// ProviderInvoiceID is NULL on issuance rows (today's only writer);
// the consumption reducer (issue #279 PR-C, @migration 00058) sets it
// to the provider's invoice identifier and pairs it with CreditID in
// a unique partial index so a webhook re-fire or admin endpoint
// replay cannot double-decrement cents_remaining.
type CreditLedgerEntry struct {
	ID                string
	AccountID         string
	CreditID          string
	DeltaCents        int64
	Reason            string
	Actor             string
	CreatedAt         time.Time
	ProviderInvoiceID *string
}

// UpdateAppParams is the partial-update payload for PATCH /v1/apps/{slug}.
// Nil pointers mean "leave unchanged" (only the slug/ram/idle/concurrency/
// min_instances/status fields are user-mutable; type and runtime are
// immutable).
type UpdateAppParams struct {
	RAMMB          *int
	IdleTimeoutS   *int // explicit 0 clears to plan default
	SetIdleTimeout bool // distinguishes nil from zero
	MaxConcurrency *int
	// MinInstances is the per-app floor for idle reaping
	// (ux_spec §6.5). SetMinInstances distinguishes "unset" (don't
	// touch the column) from "explicit zero" (scale to zero, the
	// default for Free/Hobby).
	MinInstances    *int
	SetMinInstances bool
	// EgressAllowlist is the per-app outbound CIDR allowlist
	// (ADR-031). SetEgressAllowlist distinguishes "unset" from
	// "explicit empty" (= "no allowlist rule, current behaviour").
	// A nil pointer when SetEgressAllowlist is false leaves the
	// column unchanged; a non-nil empty slice with
	// SetEgressAllowlist true replaces the stored array with '{}'
	// (the default — see migration 00029).
	EgressAllowlist    *[]netip.Prefix
	SetEgressAllowlist bool
	// AutoscaleTargetRPS is the per-instance RPS target for the
	// reactive scale-up trigger (issue #169 / #172). SetAutoscaleTargetRPS
	// distinguishes "unset" (don't touch the column) from "explicit
	// zero" (disable autoscale for RPS). Plan-gated upstream (apid
	// rejects Free PATCH with 403). Hobby/Pro/Scale only.
	AutoscaleTargetRPS    *int
	SetAutoscaleTargetRPS bool
	// AutoscaleTargetCPUPct is the per-instance CPU% target (1..100).
	// SetAutoscaleTargetCPUPct has the same "unset" vs "explicit zero"
	// semantics as AutoscaleTargetRPS. Pro/Scale only.
	AutoscaleTargetCPUPct    *int
	SetAutoscaleTargetCPUPct bool
	Status                   *AppStatus
	Manifest                 *AppManifest
}

// Snapshot is one restoreable microVM state (spec §4.6, ADR-005).
//
// imaged is the only writer; schedd reads the latest non-stale row per
// deployment to decide whether to wake-from-snapshot or cold-boot. The
// `Stale` flag is flipped on Firecracker upgrades (snapshots are pinned to
// the FC version that made them — see ADR-005).
type Snapshot struct {
	ID           string
	DeploymentID string
	FCVersion    string
	MemBytes     int64
	DiskBytes    int64
	// StorageKey is the canonical StorageBackend key for the mem
	// blob (issue #96, ADR-025 axis 2). Local backends resolve it
	// to a file under /srv/fc; remote backends (OCI registry)
	// resolve it to a manifest tag. Always populated by the
	// production write path (imaged copies it from the
	// snapshot_written payload); empty only on rows written by
	// test fixtures that bypass the storage contract. Wake sends
	// StorageKey on the wire; vmmd resolves it through the
	// configured StorageBackend.
	StorageKey string
	Stale      bool
	CreatedAt  time.Time
}

// SnapshotForGC is the join-projection used by the imaged nightly GC
// (spec §4.6: keep current + previous deployment's snapshots per app;
// fleet budget pressure evicts from biggest-over-quota accounts first).
// It denormalises snapshot → deployment → app → account into one row so
// the GC algorithm doesn't have to round-trip per row.
//
// Snapshots for soft-deleted apps (apps.status = 'deleted') are filtered
// at the SQL layer; they have no in-flight wake target and keeping them
// would leak the 452 GB budget indefinitely.
type SnapshotForGC struct {
	ID           string
	DeploymentID string
	AppID        string
	AccountID    string
	// AppSlug is the apps.slug of the parent app. Populated from the
	// snapshot → deployments → apps JOIN so the GC algorithm doesn't
	// have to issue per-eviction DeploymentByID + AppByID lookups to
	// build the apps/<slug>/<dep>.ext4 storage key (issue #195 B1.1).
	// An empty AppSlug after the projection runs is an invariant
	// violation — call sites should log + skip, never silently fall
	// back to a slow path.
	AppSlug   string
	FCVersion string
	MemBytes  int64
	DiskBytes int64
	// StorageKey mirrors Snapshot.StorageKey; populated from the
	// join so imaged's snapshot GC can Storage.Delete under the
	// canonical key (issue #96, ADR-025 axis 2 final slice).
	StorageKey string
	Stale      bool
	CreatedAt  time.Time
}

// LoginToken is one row in login_tokens (M7.5 magic-link). The token
// itself never appears in storage — only its SHA-256 hash does. The
// raw token is emailed to the user once and is consumed by
// /auth/verify?token=… (one-shot).
type LoginToken struct {
	TokenHash  []byte
	AccountID  string
	ExpiresAt  time.Time
	ConsumedAt *time.Time
}

// CliAuthCode is one row of the cli_auth_codes table (spec §2.2
// device-code flow). AccountID is empty between mint and claim; the
// claim statement fills it in atomically. The 4-byte entropy + 5-min
// TTL + per-IP rate limit means brute-force on the code space is not
// realistic, so we don't bump the byte length here.
type CliAuthCode struct {
	TokenHash  []byte
	AccountID  string // empty until ClaimCliAuthCode
	ExpiresAt  time.Time
	ConsumedAt *time.Time
}

// AccountPassword is one row of the account_passwords table
// (issue #165 / ADR-032 PR #2). It carries the Argon2id PHC string
// for an account that has set a password. OAuth-only accounts have
// no row — the absence of a row is the signal that an OAuth-only
// flow is required to mint a session on that account.
//
// Hash is the PHC wire format ($argon2id$v=19$m=...,t=...,p=...$<salt>$<hash>),
// produced by pkg/auth.Encode. The Argon2id parameters (memory,
// time, threads) are EMBEDDED in the stored string so a future
// parameter bump is a no-op migration.
//
// UpdatedAt is stamped at every SetAccountPassword call. A future
// "rotate hash on login" hardening (PR #2.5 follow-up) reads this
// to decide whether to re-hash.
type AccountPassword struct {
	AccountID string
	Hash      string
	UpdatedAt time.Time
}

// OAuthLink is one row of the oauth_links table (issue #165 /
// ADR-032 PR #2). It binds an OAuth (provider, subject) pair to
// exactly one account_id. The composite primary key on the table
// enforces the §11 anti-takeover invariant: one OAuth subject maps
// to one account, period.
//
// Email is captured at link time so the dashboard can render "this
// Google account is bound" without a re-fetch. EmailVerified is a
// snapshot of the provider's email_verified value at link time.
// Once true at link, the row stays; a future "re-verify" flow can
// refresh EmailVerified (ADR-032 "Open follow-ups"). Per spec §11,
// no session is ever minted with EmailVerified=false at link time.
type OAuthLink struct {
	Provider        string // "google" | "github" | (future providers)
	ProviderSubject string // Google's `sub`, GitHub's numeric `id`
	AccountID       string
	Email           string
	EmailVerified   bool
	CreatedAt       time.Time
}

// LogEntry is one line of build output for a deployment (slice 5).
// The dashboard's SSE stream tails this row at seq > cursor; clients
// use the combination (DeploymentID, Seq) to dedupe across reconnects
// (an id-replay after a network blip will see the same seqs).
type LogEntry struct {
	DeploymentID string
	Seq          int64
	Stream       string // "stdout" | "stderr" | "system"
	Line         string
	WrittenAt    time.Time
}

// Session is one server-side record of a dashboard login (IAM-3,
// issue #187 + #244 merged). The cookie envelope carries the row's
// ID as `sid`; every authenticated dashboard request re-validates
// the row before the handler runs.
//
// Revocation is `RevokedAt != nil`. The store methods that mutate
// RevokedAt are account-scoped at the SQL level so IDOR is a
// persistence invariant — a cross-account DELETE returns false
// (mapped to 404 in the handler), not 403.
//
// LastSeenAt continues to update post-revoke (TouchSessionLastSeen
// is a no-op gate-wise but updates the column for ops triage). This
// is observability only; authorization uses RevokedAt exclusively.
//
// IssuedIP / IssuedUA are recorded at login and surface on
// GET /v1/auth/sessions so the customer can recognize "this is my
// phone" / "this is the laptop I logged in from last Tuesday".
// IssuedIP is empty when RemoteAddr is unparseable (rare; never
// log a parse-failure string verbatim). Dashboard-only — bearer API
// keys never create or query rows on this table.
type Session struct {
	ID         string // uuid, primary key, also the cookie `sid`
	AccountID  string // uuid, FK to accounts.id
	IssuedIP   string // empty when RemoteAddr unparseable
	IssuedUA   string // user-agent at login, may be empty
	IssuedAt   time.Time
	LastSeenAt *time.Time // nil until first authenticated request post-mint
	RevokedAt  *time.Time // nil == active; non-nil == revoked
}

// AppSecret is one row of customer secrets (spec §11/G2). apid is the only
// writer. Ciphertext is the age-sealed Envelope produced by pkg/secretbox;
// the plaintext VALUE is never stored, never logged, and only exists
// transiently in apid's PUT handler and vmmd's per-wake staging path.
//
// AccountID is the row's owning account. Both PgStore and MemStore filter
// on (AccountID, AppID, Key) so cross-account access returns ErrNotFound
// (handlers render 400 CodeSecretNotFound by design — the URL resource IS
// the secret name).
type AppSecret struct {
	AccountID  string
	AppID      string
	Key        string
	Ciphertext []byte
	CreatedAt  time.Time
	UpdatedAt  time.Time
}
