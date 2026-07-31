package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Wire DTOs for the v1 REST API (spec Appendix A). Defined once here so apid and
// the faas CLI share exactly one contract; `--json` output stability (UX §3.2)
// depends on these shapes.

// CreateAppRequest creates an app or function.
type CreateAppRequest struct {
	Slug           string `json:"slug"`
	Type           string `json:"type,omitempty"`    // "app" (default) | "function"
	Runtime        string `json:"runtime,omitempty"` // node22|python312|go124|go124-alpine|node24|python313 for functions
	RAMMB          int    `json:"ram_mb,omitempty"`  // 0 => plan default
	MaxConcurrency int    `json:"max_concurrency,omitempty"`
	IdleTimeoutS   int    `json:"idle_timeout_s,omitempty"`
}

// UpdateAppRequest is the partial-update payload for PATCH /v1/apps/{slug}.
// All fields are pointers so the wire form can distinguish "not set" from
// "set to zero".
type UpdateAppRequest struct {
	RAMMB          *int `json:"ram_mb,omitempty"`
	IdleTimeoutS   *int `json:"idle_timeout_s,omitempty"`
	MaxConcurrency *int `json:"max_concurrency,omitempty"`
	// MinInstances is the per-app cold-wake floor (ux_spec §6.5).
	// 0 / unset => scale to zero; >0 => keep at least this many
	// RUNNING instances alive. Pro/Scale only — Free/Hobby get
	// 403 plan_min_instances_not_allowed (apid gate). Must be <=
	// plan MaxConcurrency (422 invalid_min_instances).
	MinInstances *int `json:"min_instances,omitempty"`
	// EgressAllowlist (ADR-031 + ADR-032, tier-2 of the network
	// roadmap) is the per-app outbound IP allowlist. Each entry is
	// a CIDR string ("1.2.3.0/24" for v4, "2001:db8::/32" for v6);
	// the slice replaces the full list (atomic full-overwrite at the
	// apps row). Plan-gated upstream (Free/Hobby return 403
	// plan_egress_allowlist_not_allowed); size-capped at
	// plan.EgressAllowlistMaxSize() (Pro 16, Scale 64) — v4 + v6
	// entries share the same count budget. Empty slice / nil
	// pointer = clear the allowlist (back to the default-accept
	// chain policy). The non-/0 contract is enforced by the DB
	// trigger `apps_egress_allowlist_cidr` (migration 00033).
	EgressAllowlist *[]string `json:"egress_allowlist,omitempty"`
	// AutoscaleTargetRPS is the per-instance RPS target for the
	// reactive scale-up trigger (issue #169 / #172 / pkg/sched/scaleup).
	// When measured RPS / live_instance_count exceeds this value,
	// schedd admits another instance up to plan.MaxConcurrency. Plan-gated
	// upstream: Free returns 403 CodePlanScaleUpNotAllowed. Hobby/Pro/Scale
	// accept values > 0; values <= 0 return 422 CodeInvalidAutoscaleTargetRPS.
	// Autoscale is "enabled" iff at least one of AutoscaleTargetRPS /
	// AutoscaleTargetCPUPct is non-nil (no separate boolean, per user
	// direction).
	AutoscaleTargetRPS *int `json:"autoscale_target_rps,omitempty"`
	// AutoscaleTargetCPUPct is the per-instance CPU% target (1..100)
	// for the scale-up trigger. Same semantics as AutoscaleTargetRPS
	// but the signal source is pkg/sched/instancestats.Reader (PR #205);
	// nil reader falls back to RPS-only mode (PR #169 never lands the
	// CPU path). Pro/Scale only; Free/Hobby return 403 CodePlanScaleUpNotAllowed.
	// Values outside [1, 100] return 422 CodeInvalidAutoscaleTargetCPUPct.
	AutoscaleTargetCPUPct *int `json:"autoscale_target_cpu_pct,omitempty"`
}

// RenameAppRequest is the body of POST /v1/apps/{slug}/rename (issue #63).
// Validated server-side via the same validSlug regex used at CreateApp
// time; rejected on conflict with 409 CodeAppRenameFailed when another
// live app already holds NewSlug.
type RenameAppRequest struct {
	NewSlug string `json:"new_slug"`
}

// AppResponse is an app as returned by the API.
type AppResponse struct {
	ID             string `json:"id"`
	Slug           string `json:"slug"`
	Type           string `json:"type"`
	Runtime        string `json:"runtime,omitempty"`
	RAMMB          int    `json:"ram_mb"`
	MaxConcurrency int    `json:"max_concurrency"`
	IdleTimeoutS   int    `json:"idle_timeout_s,omitempty"`
	// MinInstances is the per-app cold-wake floor (ux_spec §6.5).
	// 0 => scale to zero; >0 => keep N warm. Pro/Scale only.
	MinInstances int    `json:"min_instances"`
	Status       string `json:"status"`
	URL          string `json:"url"`
	// Manifest is the runner-scaffold payload (env, healthz path,
	// entrypoint). Surfaced so the dashboard's app detail page can
	// show the function handler + env without a separate round-trip.
	// The DTO reuses the existing api.AppManifest (defined in
	// appmanifest.go) so the wire shape stays a single source of truth.
	Manifest AppManifest `json:"manifest"`
	// EgressAllowlist (ADR-031 + ADR-032, tier-2 of the network
	// roadmap) is the per-app outbound CIDR allowlist. Each entry
	// is the canonical CIDR string form: v4 ("1.2.3.0/24") or v6
	// ("2001:db8::/32"). The v4-mapped v6 form ("::ffff:1.2.3.0/120")
	// is silently rewritten to its v4 form at PATCH time by
	// validateUpdateApp, so the read-back never carries a
	// "::ffff:" prefix. Materialised as `[]` (never `null`) at
	// the conversion boundary (cmd/apid/handlers.go::appResponse)
	// so Free / Hobby and pre-PATCH apps always have a predictable
	// JSON shape — the per-netns renderer treats the empty list as
	// "no allowlist rule" (the chain falls back to default-accept).
	// The list is first-seen-wins-dedup'd at write time; the read
	// order matches insertion order. NOT in `required:` because the
	// empty-slice case is the contract.
	EgressAllowlist []string `json:"egress_allowlist"`
	// AutoscaleTargetRPS / AutoscaleTargetCPUPct are the per-app
	// reactive scale-up targets (issue #169 / #172 / pkg/sched/scaleup).
	// Each is 0 when unset ("disabled") and > 0 when configured.
	// Surfaces on GET /v1/apps/{slug} so dashboards can show the
	// current target. Plan-gated upstream.
	AutoscaleTargetRPS    int `json:"autoscale_target_rps"`
	AutoscaleTargetCPUPct int `json:"autoscale_target_cpu_pct"`
}

// CreateDeploymentRequest ships a version (JSON variant; the multipart
// variant is used for tarball/dockerfile deploys).
type CreateDeploymentRequest struct {
	Image string `json:"image,omitempty"` // registry.gregale.dev/...@sha256:...
	// Overrides is the Fargate-shaped deploy-time override object
	// (issue #460 / ADR-053). Lets a customer redeploy the same
	// digest-pinned image with a different entrypoint/cmd/env/port
	// without rebuilding the image. The field list is frozen by
	// ADR-053 §Decision 1 — any new override field requires a new
	// ADR. Nil/omitted means "no overrides; deploy the image as-is".
	Overrides *CreateDeploymentOverrides `json:"overrides,omitempty"`
}

// CreateDeploymentOverrides is the optional override object on
// CreateDeploymentRequest (issue #460 / ADR-053). Six fields, frozen
// by ADR-053 §Decision 1. The handler calls Validate(limits) before
// persisting — a failed validation 400s the whole request (the
// override is never silently dropped; the customer who set it
// expects it to apply).
//
// Env / env_secrets share Limits.EnvVarsMax (ADR-045 §Decision 1 +
// ADR-053 §Decision 1): the total len(env) + len(env_secrets) is
// checked against the cap, so a customer cannot bypass the per-app
// quota by mixing the two surfaces.
//
// Env values are persisted plaintext into override_env jsonb. Env
// secret values are NOT plaintext — they are refs of the shape
// "secret:NAME" where NAME matches ^[A-Z][A-Z0-9_]*$ and resolves at
// wake time against the existing app_secrets table. The refs are
// stored verbatim in override_env_secrets jsonb; runtime resolution
// is a follow-up PR (imaged layer injection).
type CreateDeploymentOverrides struct {
	// Entrypoint replaces the OCI image's ENTRYPOINT/CMD argv when
	// the guest execs the workload. Required to be non-empty if
	// present; each element must be non-empty. nil = no override.
	Entrypoint []string `json:"entrypoint,omitempty"`
	// Cmd is appended to Entrypoint (mirrors the OCI runtime
	// contract: argv = entrypoint + cmd). nil = no override.
	Cmd []string `json:"cmd,omitempty"`
	// Env is the plaintext env map applied at boot. Key per
	// ValidateEnvKey (^[A-Z][A-Z0-9_]*$); per-value byte cap per
	// limits.EnvValueMaxBytes. nil/empty = no override.
	Env map[string]string `json:"env,omitempty"`
	// EnvSecrets is the sealed-secret-ref env map applied at boot.
	// Each VALUE is a "secret:NAME" ref (NAME matching the same
	// identifier grammar); each KEY is the env-var name set inside
	// the guest. Counts toward the same env_vars_max cap as Env.
	// nil/empty = no override.
	EnvSecrets map[string]string `json:"env_secrets,omitempty"`
	// Port is the listen port; 1..65535. 0 means "absent / fall
	// back to image default" (DefaultAppPort, today = 8080). The
	// host-side plumbing that propagates this value to netns +
	// vmmd waitReady + runners ships in PR-C; PR-A persists the
	// column and surfaces it on the response.
	Port int `json:"port,omitempty"`
	// Healthcheck is the optional readiness probe. PR-A persists
	// the shape; PR-A does NOT yet extend vmm.waitReady to issue
	// an HTTP probe — the probe stays a bare TCP accept. The
	// HTTP-probe variant is its own ADR + property test.
	Healthcheck *DeploymentHealthcheck `json:"healthcheck,omitempty"`
}

// DeploymentHealthcheck is the readiness-probe shape on the
// override object. Defaults: interval 5s, timeout 2s, retries 3.
// Path is required (and must start with "/") when the parent
// healthcheck is set.
type DeploymentHealthcheck struct {
	Path      string `json:"path"`
	IntervalS int    `json:"interval_s,omitempty"`
	TimeoutS  int    `json:"timeout_s,omitempty"`
	Retries   int    `json:"retries,omitempty"`
}

// secretRefPrefix is the wire prefix on env_secrets values that
// flags the value as a sealed-secret ref rather than a plaintext
// fallback. ADR-053 §Decision 1 — the runtime resolver (PR-B) will
// strip this prefix and look up the trailing name against the
// app_secrets table. PR-A only validates the shape; resolution is
// a follow-up.
const secretRefPrefix = "secret:"

// secretRefNameRe matches the NAME portion of a sealed-secret ref.
// Same identifier grammar as env keys / secret keys (ADR-045
// §Decision 1 mirror) — one regex, no drift.
var secretRefNameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// Validate enforces every override field's constraint from
// ADR-053 §Decision 1. Returns nil on success or a *Problem with
// RFC 7807 status 400 (or 413 for value-too-large, mirroring
// ErrEnvVarValueTooLarge). The handler maps this directly to
// api.WriteProblem; no further error wrapping needed.
//
// Limits is passed in by the caller (apid looks up via
// api.MustLimitsFor(acct.Plan)) so this stays a pure function —
// testable without an account / DB.
func (o *CreateDeploymentOverrides) Validate(limits Limits) *Problem {
	if o == nil {
		return nil
	}

	// entrypoint: non-empty if present; every element non-empty.
	for i, e := range o.Entrypoint {
		if e == "" {
			return NewProblem(http.StatusBadRequest, CodeValidation,
				"Invalid override",
				fmt.Sprintf("entrypoint[%d] is empty; every argv element must be non-empty.", i))
		}
	}

	// cmd: non-empty if present; every element non-empty.
	for i, c := range o.Cmd {
		if c == "" {
			return NewProblem(http.StatusBadRequest, CodeValidation,
				"Invalid override",
				fmt.Sprintf("cmd[%d] is empty; every argv element must be non-empty.", i))
		}
	}

	// env + env_secrets share EnvVarsMax. Compute total first so
	// the cap error names BOTH surfaces when both contribute.
	totalEnv := len(o.Env) + len(o.EnvSecrets)
	if limits.EnvVarsMax > 0 && totalEnv > limits.EnvVarsMax {
		return NewProblem(http.StatusBadRequest, CodeValidation,
			"Override env count exceeded",
			fmt.Sprintf("%s plan allows %d env+env_secrets entries per override; got %d (env=%d, env_secrets=%d).",
				limits.Plan, limits.EnvVarsMax, totalEnv, len(o.Env), len(o.EnvSecrets))).
			WithLimit(int64(limits.EnvVarsMax), int64(totalEnv)).
			WithDocs("https://docs.gregale.dev/deploy-overrides#env")
	}

	// env: key grammar + per-value byte cap. The same byte cap
	// covers env_secrets ref strings (they are also text the
	// customer sends); the ref grammar check is below.
	for k, v := range o.Env {
		if p := ValidateEnvKey(k); p != nil {
			return p
		}
		if limits.EnvValueMaxBytes > 0 && len(v) > limits.EnvValueMaxBytes {
			return ErrEnvVarValueTooLarge(limits, len(v))
		}
	}

	// env_secrets: key grammar + "secret:NAME" ref shape + per-value
	// byte cap on the ref string.
	for k, v := range o.EnvSecrets {
		if p := ValidateEnvKey(k); p != nil {
			return p
		}
		if !strings.HasPrefix(v, secretRefPrefix) {
			return NewProblem(http.StatusBadRequest, CodeValidation,
				"Invalid override",
				fmt.Sprintf("env_secrets[%q] value must start with %q (e.g. %qDB-URL); got %q.",
					k, secretRefPrefix, secretRefPrefix, v))
		}
		name := strings.TrimPrefix(v, secretRefPrefix)
		if !secretRefNameRe.MatchString(name) {
			return NewProblem(http.StatusBadRequest, CodeValidation,
				"Invalid override",
				fmt.Sprintf("env_secrets[%q] ref name %q must match %s.",
					k, name, secretRefNameRe.String()))
		}
		if limits.EnvValueMaxBytes > 0 && len(v) > limits.EnvValueMaxBytes {
			return ErrEnvVarValueTooLarge(limits, len(v))
		}
	}

	// port: 0 means absent (fall back to image default). 1..65535
	// when present.
	if o.Port != 0 && (o.Port < 1 || o.Port > 65535) {
		return NewProblem(http.StatusBadRequest, CodeValidation,
			"Invalid override",
			fmt.Sprintf("port %d out of range; must be 0 (absent) or 1..65535.", o.Port))
	}

	// healthcheck: path must start with "/" if set; defaults
	// applied on Persist side (the column shape is the raw shape).
	if o.Healthcheck != nil {
		if !strings.HasPrefix(o.Healthcheck.Path, "/") {
			return NewProblem(http.StatusBadRequest, CodeValidation,
				"Invalid override",
				fmt.Sprintf("healthcheck.path must start with %q; got %q.",
					"/", o.Healthcheck.Path))
		}
		if o.Healthcheck.IntervalS < 0 {
			return NewProblem(http.StatusBadRequest, CodeValidation,
				"Invalid override",
				fmt.Sprintf("healthcheck.interval_s must be >= 0; got %d.", o.Healthcheck.IntervalS))
		}
		if o.Healthcheck.TimeoutS < 0 {
			return NewProblem(http.StatusBadRequest, CodeValidation,
				"Invalid override",
				fmt.Sprintf("healthcheck.timeout_s must be >= 0; got %d.", o.Healthcheck.TimeoutS))
		}
		if o.Healthcheck.Retries < 0 {
			return NewProblem(http.StatusBadRequest, CodeValidation,
				"Invalid override",
				fmt.Sprintf("healthcheck.retries must be >= 0; got %d.", o.Healthcheck.Retries))
		}
	}

	return nil
}

// BuildProvenanceResponse is the public surface of build_provenance
// (ADR-038, Tier 3 / issue #197 B3.10-read half). Field names mirror
// the table columns with snake_case naming so the customer-visible
// JSON stays self-documenting on a `curl`.
//
// Fields are nullable strings; empty values map to "" so the customer
// reads "buildkit_version = \"\"" for a pre-Phase-3 build that the
// populator hasn't back-filled. The dashboard branches on
// `sbom_storage_key != ""` to enable the "Download SBOM" link;
// every other field is observational metadata for audits.
type BuildProvenanceResponse struct {
	ID             string `json:"id"`
	BuildID        string `json:"build_id"`
	BuildkitVer    string `json:"buildkit_version"`
	RailpackVer    string `json:"railpack_version"`
	BaseDigest     string `json:"base_digest"`
	SourceSHA256   string `json:"source_sha256"`
	SourceURL      string `json:"source_url"`
	CommitSHA      string `json:"commit_sha"`
	Plan           string `json:"plan"`
	RunnerDigest   string `json:"runner_digest"`
	BuilderNodeID  string `json:"builder_node_id"`
	StartedAt      string `json:"started_at"`
	FinishedAt     string `json:"finished_at"`
	SBOMStorageKey string `json:"sbom_storage_key"`
}

// DeploymentResponse is a deployment as returned by the API.
type DeploymentResponse struct {
	ID          string `json:"id"`
	AppID       string `json:"app_id"`
	BuildID     string `json:"build_id,omitempty"`
	ImageDigest string `json:"image_digest"`
	Kind        string `json:"kind"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
	// ErrorCode carries the RFC 7807 code ADR-021 lifted from the
	// puller-side sentinels (image_not_found / image_egress_denied /
	// image_manifest_invalid). Empty for every deployment created
	// before migrations/00021 OR that is not in a failure state —
	// api/state.SerializeDeployment knows the column is a string and
	// that "" is the canonical empty value, so the dashboard /
	// programmatic consumer can branch on ErrorCode != "".
	ErrorCode string `json:"error_code,omitempty"`
	CreatedAt string `json:"created_at"`
	// HasOverrides is true when the deployment carries an
	// override_* column set (issue #460 / ADR-053). Lets dashboards
	// render "this deploy pinned overrides" without re-parsing the
	// six sibling fields.
	HasOverrides bool `json:"has_overrides,omitempty"`
	// OverrideEntrypoint is the argv override echoed verbatim; nil
	// when the deployment carried no override. ADR-053 §Decision 4:
	// these fields are non-secret and safe to echo.
	OverrideEntrypoint []string `json:"override_entrypoint,omitempty"`
	// OverrideCmd is the cmd override echoed verbatim; nil when
	// the deployment carried no override.
	OverrideCmd []string `json:"override_cmd,omitempty"`
	// OverrideEnvKeys is the set of env-var keys set by the env
	// override. VALUES ARE NEVER ECHOED (ADR-053 §Decision 4 +
	// ADR-045 §Decision 6 mirror). Empty when no env override.
	OverrideEnvKeys []string `json:"override_env_keys,omitempty"`
	// OverrideEnvSecretKeys is the set of env-var keys set by the
	// env_secrets override. VALUES (the "secret:NAME" refs) are
	// echoed verbatim because the ref shape is non-secret by
	// design — the customer needs to see which secret they bound
	// to which env var to debug a misconfigured deploy. Empty
	// when no env_secrets override.
	OverrideEnvSecretKeys []string `json:"override_env_secret_keys,omitempty"`
	// OverrideEnvSecretRefs is the verbatim "secret:NAME" map,
	// parallel to OverrideEnvSecretKeys. Same rationale: the refs
	// are non-secret. nil when no env_secrets override.
	OverrideEnvSecretRefs map[string]string `json:"override_env_secret_refs,omitempty"`
	// OverridePort is the listen-port override (0 = absent /
	// fall back to image default). ADR-053 §Decision 1.
	OverridePort int `json:"override_port,omitempty"`
	// OverrideHealthcheck is the readiness-probe override
	// verbatim. Persisted; the actual HTTP probe is a follow-up.
	OverrideHealthcheck *DeploymentHealthcheck `json:"override_healthcheck,omitempty"`
}

// AccountResponse is the whoami payload. Limits is the plan's
// quota/limit table (RAM MB, max concurrency, included GB-h,
// deployed-app cap) so the dashboard /account page can show
// "you have X of Y apps" without a second round trip. UsageGBHours
// is the roll-up for the current month (caller-aggregated from
// Store.UsageByHour in apid; included here so the dashboard can
// render the meter in one fetch).
type AccountResponse struct {
	ID            string        `json:"id"`
	Email         string        `json:"email"`
	Plan          string        `json:"plan"`
	Status        string        `json:"status"`
	Limits        AccountLimits `json:"limits"`
	UsageGBHours  float64       `json:"usage_gb_hours"`
	AppCount      int           `json:"app_count"`
	GitHubInstall string        `json:"github_install_id,omitempty"`
}

// AccountLimits is the read-only copy of api.Limits that survives
// serialization. Stripped of fields the dashboard doesn't need
// (eg. internal ops); mirror pkg/api/limits.go for the wiring.
type AccountLimits struct {
	Plan            string `json:"plan"`
	RAMMB           int    `json:"ram_mb"`
	MaxConcurrency  int    `json:"max_concurrency"`
	DeployedApps    int    `json:"deployed_apps"`
	IncludedGBHours int64  `json:"included_gb_hours"`
	AppLayerMaxMB   int    `json:"app_layer_max_mb"`
}

// APIKeyResponse is an API key returned to the customer. The plaintext
// appears ONLY on creation (POST /v1/keys), never on GET — only the prefix
// + label + scopes + last_used_at + id are returned thereafter. Scopes is
// the explicit permission set attached to the key (e.g. ["admin"],
// ["apps:read", "deploy:write"]); see ADR-034 rev2.
type APIKeyResponse struct {
	ID         string   `json:"id"`
	Prefix     string   `json:"prefix"` // "fp_live_abc12345…" (first 16 chars)
	Label      string   `json:"label,omitempty"`
	Scopes     []string `json:"scopes"`
	LastUsedAt string   `json:"last_used_at,omitempty"`
	CreatedAt  string   `json:"created_at"`
	// Plaintext appears ONLY on the create response, never persisted.
	Plaintext string `json:"plaintext,omitempty"`
}

// CreateKeyRequest is the body of POST /v1/keys. Label is optional
// (max 100 chars per spec); empty label is allowed and renders as
// `{}` so the server's optional-field handling stays in scope. Scopes
// is the requested permission set; the server validates each entry
// against the closed vocabulary (admin, apps:read, deploy:write,
// secrets:read, secrets:write, usage:read) and defaults to
// ["admin"] when omitted so existing callers keep full access. See
// ADR-034 rev2.
type CreateKeyRequest struct {
	Label  string   `json:"label,omitempty"`
	Scopes []string `json:"scopes,omitempty"`
}

// CustomDomainResponse is a custom domain's wire shape. VerifiedAt is the
// zero time on unverified rows; the verifier goroutine polls DNS and updates
// it (spec §7).
type CustomDomainResponse struct {
	Domain         string `json:"domain"`
	AppID          string `json:"app_id"`
	ChallengeToken string `json:"challenge_token,omitempty"`
	Verified       bool   `json:"verified"`
	VerifiedAt     string `json:"verified_at,omitempty"`
	TXTRecord      string `json:"txt_record,omitempty"` // convenience for the customer
}

// CreateCustomDomainRequest accepts a domain to bind.
type CreateCustomDomainRequest struct {
	Domain string `json:"domain"`
	AppID  string `json:"app_id"`
}

// CronResponse mirrors the crons table. LastFiredAt is the most
// recent fire stamp schedd wrote (MarkCronFired). Zero-valued
// crons serialize as "" — the dashboard only shows the column
// when populated.
type CronResponse struct {
	ID          string `json:"id"`
	AppID       string `json:"app_id"`
	Schedule    string `json:"schedule"`
	Path        string `json:"path"`
	Enabled     bool   `json:"enabled"`
	CreatedAt   string `json:"created_at"`
	LastFiredAt string `json:"last_fired_at,omitempty"`
}

// CreateCronRequest creates a scheduled synthetic POST.
type CreateCronRequest struct {
	AppID    string `json:"app_id"`
	Schedule string `json:"schedule"`
	Path     string `json:"path,omitempty"`
	Enabled  *bool  `json:"enabled,omitempty"`
}

// UpdateCronRequest is a partial update.
type UpdateCronRequest struct {
	Schedule *string `json:"schedule,omitempty"`
	Path     *string `json:"path,omitempty"`
	Enabled  *bool   `json:"enabled,omitempty"`
}

// InstanceResponse is the read-only instance view (spec §4.2 / §6).
type InstanceResponse struct {
	ID            string `json:"id"`
	AppID         string `json:"app_id"`
	DeploymentID  string `json:"deployment_id"`
	State         string `json:"state"`
	HostIP        string `json:"host_ip,omitempty"`
	RAMMB         int    `json:"ram_mb"`
	StartedAt     string `json:"started_at,omitempty"`
	LastRequestAt string `json:"last_request_at,omitempty"`
	ParkedAt      string `json:"parked_at,omitempty"`
	// WakeID is the per-wake stable identifier minted by schedd at
	// CreateInstance time (UUIDv7). Distinct from `id` (the row PK):
	// one row can carry many WakeIDs over its lifetime as the app is
	// parked and re-woken. Surfaced on `faas ps` and the dashboard
	// detail page so operators can correlate the request that woke
	// the app against gateway logs and slog entries (which also
	// carry this field).
	WakeID string `json:"wake_id,omitempty"`
}

// ListInstancesResponse is the page shape for GET /v1/instances
// (issue #393). Cursor is the instances.id UUIDv7 — the handler
// emits the last row's id as NextBefore when len(Instances) == limit,
// so the caller can pass it back unchanged as ?before=<id> on the
// next request. Empty NextBefore means the page is the end. An
// account with zero live instances returns 200 with an empty
// `instances` array — never 404.
type ListInstancesResponse struct {
	Instances  []InstanceResponse `json:"instances"`
	NextBefore string             `json:"next_before,omitempty"`
}

// UsageResponse is one app's monthly usage slice (spec §10).
//
// CPUUsageUsec is the cumulative host cgroup CPU-µs this app
// consumed during the month (issue #279 / PR-B). Measurement
// only — billing is on plan RAM (spec §4.7). Exposed as
// cpu_usec (integer) so the dashboard can compute hours lazily
// without an integer→float→integer round trip; the SDK exposes
// both the raw integer and the derived CPUHours getter.
type UsageResponse struct {
	AppID     string `json:"app_id"`
	MBSeconds int64  `json:"mb_seconds"`
	Requests  int64  `json:"requests"`
	// IncludedGBHours is the included quota for the account's plan at the
	// requested month; the CLI computes the overage from this and the rows.
	IncludedGBHours int64 `json:"included_gb_hours"`
	// CPUUsageUsec is the per-app monthly CPU-µs — informational
	// only (issue #279 / PR-B). 0 when no meterd sample has
	// accumulated yet (boot, or the schedd reader has no row for
	// this app).
	CPUUsageUsec int64 `json:"cpu_usec"`
	// TXBytes (ADR-046, step 10) is the per-app monthly
	// HTTP-response byte delta — informational only. Source:
	// gateway statusRecorder.Bytes → meterd SampleAndRoll →
	// usage_minutes.tx_bytes. Not billed (ADR-046 §6); the
	// gateway-side producer lands in PR-2. 0 when no meterd
	// sample has accumulated yet.
	TXBytes int64 `json:"tx_bytes"`
	// NetTxBytes (ADR-046, step 10) is the per-app monthly
	// byte delta on root-side vethHost.rx_bytes —
	// informational only. Source: vmmd netstats.Cache →
	// schedd instancestats.Poller → schedd
	// ListInstanceStats → meterd SampleAndRoll →
	// usage_minutes.net_tx_bytes. Not billed (ADR-046 §6).
	// 0 when no meterd sample has accumulated yet.
	NetTxBytes int64 `json:"net_tx_bytes"`
	// NetRxBytes (ADR-048) is the per-app monthly byte
	// delta on root-side vethHost.tx_bytes — mirror of
	// NetTxBytes on the ingress direction (root → guest).
	// Source: vmmd netstats.Cache TX path → schedd
	// instancestats.Poller → meterd SampleAndRoll →
	// usage_minutes.net_rx_bytes. Informational only —
	// not billed (ADR-048 §5). 0 when no meterd sample has
	// accumulated yet or the wire regen that surfaces the
	// ingress field has not yet landed (PR-A commit #2
	// follow-up).
	NetRxBytes int64 `json:"net_rx_bytes"`
	// ColdBootCount (ADR-048) is the per-app monthly
	// count of WAKE_RESTORE → WAKE_COLD_BOOT transitions
	// observed across this app's instances. Source:
	// scheddgrpc.InstanceStatsRow.LastWakeMethod, sampled
	// by meterd SampleAndRoll → usage_minutes.
	// cold_boot_count. Informational only — not billed.
	// 0 when no meterd sample has accumulated yet or the
	// wire regen has not yet landed.
	ColdBootCount int64 `json:"cold_boots"`
}

// CPUHours returns CPUUsageUsec converted to CPU-hours. 1 hour
// = 3.6e9 µs. Convenience getter for the SDK and the CLI; the
// dashboard can compute the same value with `pkg/meter.CPUHours`.
func (u UsageResponse) CPUHours() float64 {
	return float64(u.CPUUsageUsec) / 3.6e9
}

// TotalEgressGB returns (TXBytes + NetTxBytes) converted to GB
// (1 GB = 1024^3 bytes).
//
// IMPORTANT (ADR-046, PR-414 I5): the value INCLUDES Ethernet
// framing (~14 + 20 bytes per packet) because net_tx_bytes
// reads the kernel `/sys/class/net/<vethHost>/statistics/rx_bytes`
// counter — interface bytes, not IP-payload bytes. A 1 GB HTTP
// workload can show as ~1.2-1.5 GB on this counter. The two
// columns are exposed separately so callers can distinguish
// gateway response bytes (HTTP only, exact) from netns tap0
// egress (HTTP + 80/443/53 + DNS, includes framing).
//
// For HTTP-payload-only bytes, callers should use TXBytes
// directly (do not divide by 1 GiB and call it "egress GB").
// The future billing PR will pick the unit; this convenience
// getter exists so the SDK and the CLI have a single
// "all-bytes" surface for informational dashboards.
//
// Convention:
//   - TotalEgressGB = interface bytes, includes framing.
//   - TXBytes = HTTP response bytes, exact.
//   - NetTxBytes = interface bytes on root-side vethHost.rx_bytes.
func (u UsageResponse) TotalEgressGB() float64 {
	return float64(u.TXBytes+u.NetTxBytes) / (1024 * 1024 * 1024)
}

// DeploymentListResponse is the page shape for GET /v1/deployments.
// Items is the page (in created_at DESC order); NextBefore is the
// cursor the caller should pass on the next request to page BACKWARDS
// (the dashboard's "older deploys" link). Empty NextBefore means the
// page is the end of the list.
//
// Cursor format: RFC3339Nano (matches state.Deployment.CreatedAt).
type DeploymentListResponse struct {
	Items      []DeploymentResponse `json:"items"`
	NextBefore string               `json:"next_before,omitempty"`
}

// --- Invoice history (issue #259) -----------------------------------------

// Invoice is one persisted invoice from a billing provider, surfaced
// via GET /v1/invoices. Money is integer cents in the provider's
// currency (the financial model distills to EUR at the API edge).
// PDFAvailable is the only PDF surface we expose — the hosted PDF URL
// is provider-scoped and customer-fetched via the Stripe/Paddle
// portal, not via this API. HostedURL is intentionally not on the
// wire; see state.Invoice for the rationale.
type Invoice struct {
	ID                string    `json:"id"`
	Provider          string    `json:"provider"`
	ProviderInvoiceID string    `json:"provider_invoice_id"`
	Number            string    `json:"number"`
	Status            string    `json:"status"`
	PeriodStart       time.Time `json:"period_start"`
	PeriodEnd         time.Time `json:"period_end"`
	SubtotalCents     int64     `json:"subtotal_cents"`
	TaxCents          int64     `json:"tax_cents"`
	TotalCents        int64     `json:"total_cents"`
	AmountPaidCents   int64     `json:"amount_paid_cents"`
	Currency          string    `json:"currency"`
	PDFAvailable      bool      `json:"pdf_available"`
	CreatedAt         time.Time `json:"created_at"`
}

// InvoiceListResponse is the page shape for GET /v1/invoices.
// Items is the page (in period_end DESC, id DESC order). NextBefore
// is the cursor the caller passes on the next request to fetch the
// older page. Empty NextBefore means the page is the end. Empty
// Items with 200 OK is the empty-history shape — never 404.
type InvoiceListResponse struct {
	Items      []Invoice `json:"items"`
	NextBefore string    `json:"next_before,omitempty"`
}

// --- Account credits (issue #279) -----------------------------------------

// AccountCreditResponse is the wire shape for one row in
// account_credits. Cents is integer (CLAUDE.md: never float on money).
// ExpiresAt is RFC 3339 when set; empty when the credit has no
// expiry. CreatedAt is the issuance timestamp. The handler echoes the
// row back to the operator on POST /v1/admin/accounts/{id}/credits and
// on GET /v1/admin/accounts/{id}/credits (list, when it lands).
type AccountCreditResponse struct {
	ID             string     `json:"id"`
	AccountID      string     `json:"account_id"`
	CentsRemaining int64      `json:"cents_remaining"`
	Reason         string     `json:"reason"`
	CreatedAt      time.Time  `json:"created_at"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
}

// ConsumeInvoiceResponse is the wire shape returned by the credit
// consumption reducer on POST /v1/invoices/{id}/consume-credits
// (issue #279 PR-C). ConsumedCents is the integer cents of overage
// that were drained against this invoice (floored to whole cents).
// RemainingCreditsCents is the sum of cents_remaining across the
// account's active credits after the call. AlreadyConsumedForInvoice
// is true on idempotent replays (e.g. webhook redelivery) — the
// reducer returns the same ConsumedCents without double-decrementing.
// PerCredit mirrors the per-credit delta rows so the operator can
// see FIFO drain order. Money is integer cents (CLAUDE.md).
type ConsumeInvoiceResponse struct {
	InvoiceID                 string              `json:"invoice_id"`
	ConsumedCents             int64               `json:"consumed_cents"`
	RemainingCreditsCents     int64               `json:"remaining_credits_cents"`
	AlreadyConsumedForInvoice bool                `json:"already_consumed_for_invoice"`
	PerCredit                 []ConsumedCreditRow `json:"per_credit"`
}

// ConsumedCreditRow is one entry in ConsumeInvoiceResponse.PerCredit.
// NewBalance is cents_remaining after the decrement — 0 means the
// credit was fully drained, > 0 means a partial drain.
type ConsumedCreditRow struct {
	CreditID   string `json:"credit_id"`
	DeltaCents int64  `json:"delta_cents"`
	NewBalance int64  `json:"new_balance"`
}

// --- Dashboard auth (issue #165, ADR-032 PR #2) ----------------------------

// OAuthProvider is the issuer name used by the dashboard OAuth flows
// (the email/identity brokers). The set is intentionally closed — adding
// a new provider is a Store + handler + OpenAPI change, not a config
// flag. "google" and "github" are wired in PR #2.
type OAuthProvider string

const (
	OAuthProviderGoogle OAuthProvider = "google"
	OAuthProviderGitHub OAuthProvider = "github"
)

// AuthCapabilities is the body of GET /v1/auth/capabilities
// (issue #419 / ADR-046). The dashboard reads this on /login to
// decide whether to render the "Sign in with Google" / "Sign in
// with GitHub" buttons. Each per-provider entry reports whether
// the consent route is wired (Enabled == true) or whether it would
// 503 with oauth_provider_unavailable because both ID+SECRET were
// unset at boot.
//
// The set of provider names is closed; new providers land as new
// keys, not as a list. The Wire-shape deliberately keeps the keys
// named (`providers.google`, `providers.github`) so the dashboard
// template can reach them directly via {{.Auth.GoogleEnabled}}-
// style guards, and the spec_compliance_test (cmd/apid/spec_compliance_test.go)
// can pin the schema parity.
type AuthCapabilities struct {
	Providers AuthProviders `json:"providers"`
}

// AuthProviders is the per-provider capability map. Closed set
// (google, github) — handlers MUST add a new field here when
// adding a new provider, not relax this to map[string]… .
type AuthProviders struct {
	Google OAuthProviderCapability `json:"google"`
	GitHub OAuthProviderCapability `json:"github"`
}

// OAuthProviderCapability is one provider's capability flag.
// Source is auth.SignInProvider.Enabled() — the boot-resolved
// state loaded once at apid startup and pinned for the process
// lifetime.
type OAuthProviderCapability struct {
	Enabled bool `json:"enabled"`
}

// PasswordLoginRequest is the body of POST /login. The email is the
// canonical handle (lowercase + trim — the handler runs the same
// canonicalisation the account-create path uses so an "alice@example.com
// vs ALICE@example.com" login pair collapses to one row). Password is
// the plaintext the client sent over TLS; the Argon2id verify is in
// pkg/auth.Verify and runs on the server only.
type PasswordLoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// PasswordLoginResponse is what POST /login (and POST /signup) return
// on success. The session cookie rides on the Set-Cookie header — the
// body deliberately carries NO api_key field. Pre-#165 (PR #1) the
// response minted a "web-console" key and returned it in the body; that
// was the takeover surface. The SDK path is the device-code CLI
// (MintCliAuthCode / ExchangeCliAuthCode), not a login-bundled key, so
// removing the field here doesn't break programmatic auth.
type PasswordLoginResponse struct {
	AccountID string `json:"account_id"`
	Plan      string `json:"plan"`
}

// PasswordSignupRequest is the body of POST /signup. Same shape as
// PasswordLoginRequest — we accept the same argon2id-shaped ciphertext
// at signup and re-verify at login, so the handler-side error
// equivalence ("wrong password" vs "no account" vs "weak password") is
// kept intact under the same JSON keys.
type PasswordSignupRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// PasswordResetRequest is the body of POST /login/forgot. The email
// is optional — the same-shape internal handler is hit by the form
// page (no body) and the SDK (email in body). The handler always
// returns 200 with an identical body and identical timing whether or
// not the email exists, so the surface does not leak account presence.
type PasswordResetRequest struct {
	Email string `json:"email,omitempty"`
}

// PasswordResetConfirm is the body of POST /auth/reset. Token is the
// 32-byte value the email link carried (base64url-encoded, NOT the
// SHA-256 hash the server stored). NewPassword is the plaintext the
// user is opting into; the server Argon2id-encodes it server-side and
// runs ConsumeLoginToken atomically so a replay returns 410.
type PasswordResetConfirm struct {
	Token       string `json:"token"`
	NewPassword string `json:"new_password"`
}

// SetPasswordRequest is the body of POST /dashboard/account/set-password.
// Lets OAuth-only users opt into password login. Same shape as the
// reset-confirm NewPassword field — the handler runs auth
// (sessionAuth) before encoding, so this is an authenticated surface.
type SetPasswordRequest struct {
	Password string `json:"password"`
}

// UsageSummaryResponse is the roll-up for the current month (or any
// month passed as a query param). Used by the dashboard usage page so
// the customer sees a single number ("used X of Y GB-h, overage $Z")
// without having to sum rows.
//
// Overage math: anything above IncludedGBHours is billable at the
// overage rate in the financial model (€0.01/GB-h). Cents are integer.
//
// Issue #279 / PR-B: UsedCPUHours is informational and NOT billed.
// The billing math is on UsedGBHours (plan RAM + 8 MB per running
// second). The CPU dimension is a measurement the dashboard will
// surface in a separate panel without affecting the billing total.
//
// ADR-046 (step 10): UsedEgressGB is informational and NOT
// billed. The two egress columns (tx_bytes + net_tx_bytes) are
// exposed separately at the per-app UsageResponse level; the
// summary rolls them up for the dashboard's single-number
// panel. The gateway-side tx_bytes producer lands in PR-2.
type UsageSummaryResponse struct {
	Month           string  `json:"month"`             // YYYY-MM
	UsedGBHours     float64 `json:"used_gb_hours"`     // Σ mb_seconds / 3_600_000
	IncludedGBHours int64   `json:"included_gb_hours"` // from plan limits
	OverageGBHours  float64 `json:"overage_gb_hours"`  // max(0, used - included)
	OverageCents    int64   `json:"overage_cents"`     // overage * 1.0 (€0.01/GB-h in cents)
	// UsedCPUHours is the per-month CPU-hours Σ CPUUsageUsec /
	// 3.6e9. Informational only — billing is on UsedGBHours.
	// Issue #279 / PR-B.
	UsedCPUHours float64 `json:"used_cpu_hours"`
	// UsedEgressGB is the per-month egress Σ (TXBytes +
	// NetTxBytes) / 1024^3. Informational only — not
	// billed (ADR-046 §6). The two columns are exposed
	// separately at the per-app level; this is the
	// single-number roll-up for the dashboard's
	// "egress this month" panel.
	UsedEgressGB float64 `json:"used_egress_gb"`
	// UsedIngressGB (ADR-048) is the per-month ingress Σ
	// NetRxBytes / 1024^3. Informational only — not billed
	// (ADR-048 §5). Same Ethernet-framing caveat as
	// UsageResponse.TotalEgressGB. The dashboard's "ingress
	// this month" panel reads this single number; the
	// per-app breakdown lives at UsageResponse.NetRxBytes.
	UsedIngressGB float64 `json:"used_ingress_gb"`
	// ColdBootTotal (ADR-048) is the per-month Σ of
	// WAKE_RESTORE → WAKE_COLD_BOOT transitions across
	// every app on this account. Informational only — not
	// billed. The dashboard's "this customer's cold-boot
	// bill of health" panel reads this single number; the
	// per-app breakdown lives at UsageResponse.ColdBootCount.
	ColdBootTotal int64 `json:"cold_boots"`
}

// ValidateAppConfig checks a requested app config against its plan caps (spec
// §4.2: validation before work). It returns the first violating *Problem, or nil.
// The deployed-app COUNT check is done in apid (it needs the store).
func ValidateAppConfig(l Limits, ramMB, maxConcurrency int) *Problem {
	if ramMB > l.RAMMB {
		return ErrPlanLimitRAM(l, ramMB)
	}
	if maxConcurrency > l.MaxConcurrency {
		return NewProblem(403, CodePlanLimitConcur,
			"Concurrency over plan limit",
			fmt.Sprintf("%s plan caps max_concurrency at %d; requested %d.", l.Plan, l.MaxConcurrency, maxConcurrency)).
			WithLimit(int64(l.MaxConcurrency), int64(maxConcurrency)).
			WithDocs("https://docs.gregale.dev/plans#concurrency")
	}
	return nil
}

// --- G6 account self-service (spec §17 G6, ADR-021) -------------------------

// AccountExportResponse is the GET /v1/account/export bundle. A
// single JSON document with one slice per resource type the customer
// owns (apps, deployments, builds, instances, usage, domains, crons,
// API keys, app_secrets). Ciphertext passthrough for the secrets
// slice — the plaintext VALUE never lands in PG (ADR-020), so the
// customer can rotate their host age key after a restore-from-export
// without losing the per-secret envelope.
type AccountExportResponse struct {
	ExportedAt  string                    `json:"exported_at"`
	Account     AccountResponse           `json:"account"`
	Apps        []AppResponse             `json:"apps"`
	Deployments []DeploymentResponse      `json:"deployments"`
	Builds      []BuildExportResponse     `json:"builds"`
	Instances   []InstanceResponse        `json:"instances"`
	Usage       []UsageExportResponse     `json:"usage"`
	Domains     []CustomDomainResponse    `json:"domains"`
	Crons       []CronResponse            `json:"crons"`
	APIKeys     []APIKeyExportResponse    `json:"api_keys"`
	AppSecrets  []AppSecretExportResponse `json:"app_secrets"`
	// AuditTrail is the customer's own GDPR ledger slice: every
	// export/delete/restore the customer has hit. Surfaced in the
	// bundle so the export is self-describing (the customer can see
	// "yes, my last deletion request fired at <ts>") without a
	// separate GET round trip.
	AuditTrail []GdprAuditExportResponse `json:"audit_trail,omitempty"`
}

// BuildExportResponse is the per-build row in the export bundle.
// Reduced shape (no internal IDs the customer can't act on).
type BuildExportResponse struct {
	ID           string `json:"id"`
	DeploymentID string `json:"deployment_id"`
	AppID        string `json:"app_id"`
	Kind         string `json:"kind"`
	Status       string `json:"status"`
	SourceBytes  int64  `json:"source_bytes"`
	StartedAt    string `json:"started_at,omitempty"`
	FinishedAt   string `json:"finished_at,omitempty"`
}

// UsageExportResponse is the per-month roll-up in the export bundle.
// `month` is YYYY-MM (matches the dashboard's usage page render).
// CPUUsageUsec is the per-app monthly CPU-µs — informational
// only (issue #279 / PR-B). 0 when no meterd sample has
// accumulated.
type UsageExportResponse struct {
	AppID        string `json:"app_id"`
	Month        string `json:"month"`
	MBSeconds    int64  `json:"mb_seconds"`
	Requests     int64  `json:"requests"`
	CPUUsageUsec int64  `json:"cpu_usec"`
	// ADR-046 (step 10): per-app monthly egress bytes —
	// informational only, not billed. Mirrors the new
	// UsageResponse.TXBytes / UsageResponse.NetTxBytes fields
	// (the export bundle and the API shape stay in lockstep).
	// The gateway-side tx_bytes producer lands in PR-2.
	TXBytes    int64 `json:"tx_bytes"`
	NetTxBytes int64 `json:"net_tx_bytes"`
	// ADR-048: mirror of UsageResponse.NetRxBytes /
	// UsageResponse.ColdBootCount on the export surface.
	// Informational only — not billed.
	NetRxBytes    int64 `json:"net_rx_bytes"`
	ColdBootCount int64 `json:"cold_boots"`
}

// DailyUsageResponse is one row of GET /v1/usage/daily — the
// per-(account, app, day) rollup the dashboard's hot path reads
// (migrations/00067_extend_metering_telemetry.sql::usage_daily).
// Distinct from UsageResponse which is the per-app monthly
// rollup (pkg/state.UsageByMonth); the daily route is for
// yesterday / today / single-day queries where the monthly grain
// is over-aggregated. ADR-048 §5.
type DailyUsageResponse struct {
	AppID          string `json:"app_id"`
	Day            string `json:"day"` // YYYY-MM-DD
	MBSeconds      int64  `json:"mb_seconds"`
	Requests       int64  `json:"requests"`
	CPUUsageUsec   int64  `json:"cpu_usec"`
	TXBytes        int64  `json:"tx_bytes"`
	NetTxBytes     int64  `json:"net_tx_bytes"`
	NetRxBytes     int64  `json:"net_rx_bytes"`
	ColdBootCount  int64  `json:"cold_boots"`
	BuilderSeconds int64  `json:"builder_seconds"`
}

// CPUHours returns CPUUsageUsec converted to CPU-hours. Mirror
// of UsageResponse.CPUHours so the export bundle and the API
// shape stay in lockstep.
func (u UsageExportResponse) CPUHours() float64 {
	return float64(u.CPUUsageUsec) / 3.6e9
}

// DailyUsageListResponse is the page shape for GET /v1/usage/daily.
// Mirrors the invoice / deployment list shapes — Items is always
// non-nil so the JSON encodes an empty array, not null, when the
// requested day has no rollup rows yet (ADR-048 §5).
type DailyUsageListResponse struct {
	Items []DailyUsageResponse `json:"items"`
}

// StorageUsageResponse is one row of GET /v1/usage/storage — the
// per-(account, app, day) storage rollup (migrations/
// 00070_snapshot_storage_daily.sql). Mirrors the snapshot+layer
// byte totals that the meterd storage rollup cron (pkg/meter/
// storage.go) populates. ADR-049 §B.3.
type StorageUsageResponse struct {
	AppID         string `json:"app_id"`
	Day           string `json:"day"` // YYYY-MM-DD
	SnapshotBytes int64  `json:"snapshot_bytes"`
	LayerBytes    int64  `json:"layer_bytes"`
}

// StorageUsageListResponse is the page shape for GET /v1/usage/storage.
// Items is always non-nil so the JSON encodes an empty array, not
// null, when the requested day has no rollup rows yet.
type StorageUsageListResponse struct {
	Items []StorageUsageResponse `json:"items"`
}

// BillingPortalResponse is the wire shape for GET /v1/billing/portal
// (issue #253). URL is the operator-configured billing portal link —
// today: FAAS_BILLING_PORTAL_URL with `{account_id}` substituted.
// Empty URL is a 200 (the request itself succeeded); it is the
// "absent" sentinel meaning the box has no portal configured and
// the CLI should print a friendly hint instead of opening the
// browser to "". The field is omitempty so an unset URL on a Free
// account does not surface as JSON null in either the dashboard's
// SSR page or the SDK response.
type BillingPortalResponse struct {
	URL string `json:"url,omitempty"`
}

// APIKeyExportResponse is one row in the export's API key slice.
// The plaintext key never appears here (and never reappears after
// the create response, per §4.2). Only the prefix + label + scopes +
// timestamps. Scopes is included so the customer's GDPR export carries
// the full audit trail of which keys had which permissions at the
// moment of export (ADR-034 rev2).
type APIKeyExportResponse struct {
	ID        string   `json:"id"`
	Prefix    string   `json:"prefix"`
	Label     string   `json:"label,omitempty"`
	Scopes    []string `json:"scopes"`
	CreatedAt string   `json:"created_at"`
	LastUsed  string   `json:"last_used_at,omitempty"`
}

// GdprAuditExportResponse is one row of the customer's own audit trail
// as surfaced in the export bundle. Two row kinds live here:
//
//   - source="gdpr"   — a self-service GDPR action (export/delete/restore
//     from the gdpr_requests table). Action is "export" | "delete" |
//     "restore"; CompletedAt is empty when the action is still in
//     flight.
//   - source="event"  — a security event from the events table (IAM-4,
//     ADR-035). Kind is the namespaced event kind (e.g. "auth.login",
//     "key.created"); Data is the original jsonb payload.
//
// Rows from both sources are interleaved by timestamp descending in
// the bundle so a reviewer sees one ordered timeline. Existing GDPR
// consumers can ignore unknown fields per the standard JSON rule.
type GdprAuditExportResponse struct {
	Source      string          `json:"source"`           // "gdpr" | "event"
	Action      string          `json:"action,omitempty"` // "export" | "delete" | "restore" (gdpr)
	RequestedAt string          `json:"requested_at"`     // RFC 3339 (event.at for source="event")
	CompletedAt string          `json:"completed_at,omitempty"`
	Kind        string          `json:"kind,omitempty"` // auth.*|key.*|account.*|secret.* (event)
	Data        json.RawMessage `json:"data,omitempty"` // kind-specific payload (event)
}

// AppSecretExportResponse is one row in the export's app_secrets slice.
// Ciphertext is the age-sealed envelope (base64). Plaintext never
// lands here — the customer imports the envelope into another faas
// install (or their own age tool) to unseal.
type AppSecretExportResponse struct {
	AppID      string `json:"app_id"`
	Key        string `json:"key"`
	Ciphertext string `json:"ciphertext"` // base64
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

// AccountDeletionResponse is the response from DELETE /v1/account
// (and the same shape is replayed on every repeat call — the
// idempotent endpoint guarantees the response body is identical
// across retries inside the 24 h window).
type AccountDeletionResponse struct {
	Status       string `json:"status"`        // always "deleted_pending"
	ScheduledAt  string `json:"scheduled_at"`  // deletion_requested_at, RFC 3339
	RestoreUntil string `json:"restore_until"` // scheduled_at + 30 d, RFC 3339
}

// StatusPage is the JSON shape served by GET /status/slo.json (spec
// §12, M8 acceptance). Lives in pkg/api so the CLI can import it
// without a back-reference into cmd/apid; cmd/apid/status.go embeds
// the same JSON tags so the wire shape stays identical.
//
// Fields are documented in deploy/statuspage/index.html; renames here
// must propagate to that file (and to the statusCache JSON encoder in
// cmd/apid/status.go).
type StatusPage struct {
	// APIAvailabilityPct is the rolling 5-minute 2xx rate over
	// gateway_requests_total, expressed 0..100.
	APIAvailabilityPct float64 `json:"api_availability_pct"`
	// WakeP95MS is the p95 of gateway_wake_latency_seconds over the
	// last 5 minutes, in milliseconds.
	WakeP95MS float64 `json:"wake_p95_ms"`
	// BuildSuccessPct is the rolling 5-minute success rate of
	// builderd builds (completed/success ÷ (completed/success +
	// completed/failure)).
	BuildSuccessPct float64 `json:"build_success_pct"`
	// Degraded is true when at least one page- or warn-severity alert
	// is currently firing on the local Prometheus. The public status
	// page renders a "degraded" pill when this is true so prospects
	// and customers see the same picture the operator's pager sees.
	//
	// The flag is intentionally conservative: a transient PromQL
	// error against ALERTS{} is treated as "no firing alerts" rather
	// than poisoning the snapshot. Prometheus being completely
	// unreachable still surfaces via Source = "degraded: <reason>"
	// (the pre-existing contract).
	Degraded bool `json:"degraded"`
	// AsOf is the UTC timestamp the snapshot was taken. The HTML
	// renders "Updated 3 min ago" off this.
	AsOf time.Time `json:"as_of"`
	// Source is "prometheus", "degraded: firing alerts", or
	// "degraded: <reason>" so an operator tailing the JSON can tell
	// at a glance why a snapshot is or isn't trustworthy.
	Source string `json:"source"`
}

// --- Move 2: event-driven surface response shapes ----------------------------
//
// AsyncInvokeResponse is the 202-side of POST /v1/apps/{slug}/invoke/async.
// StatusURL is the well-known read endpoint so the dashboard (and the
// SDK) can poll without parsing the id.
type AsyncInvokeResponse struct {
	ID        string `json:"id"`
	StatusURL string `json:"status_url"`
}

// InvokeResponse is the sync-side of POST /v1/apps/{slug}/invoke.
// Status is the final row state (one of "completed" | "failed"
// | "cancelled"); Result is the per-app payload the drain stamped
// (nil while pending, populated by drain.emitDone).
type InvokeResponse struct {
	ID     string          `json:"id"`
	Status string          `json:"status"`
	Result json.RawMessage `json:"result,omitempty"`
}

// QueueSendResponse is returned on POST /v1/apps/{slug}/queues/invocations:send.
// 201 Created with the new id; the customer pairs this with the
// /receive long-poll.
type QueueSendResponse struct {
	ID string `json:"id"`
}

// QueueReceiveResponse is returned on POST /v1/apps/{slug}/queues/invocations:receive.
// 200 with the dequeued row's payload + result; 204 on timeout.
type QueueReceiveResponse struct {
	ID      string          `json:"id"`
	Payload json.RawMessage `json:"payload"`
	Result  json.RawMessage `json:"result,omitempty"`
}

// DelayedTaskResponse is the create/get shape for delayed tasks.
// ScheduledAt is the customer-facing UTC dispatch time; State is
// populated on get, omitted on create (always "pending" there).
type DelayedTaskResponse struct {
	ID          string    `json:"id"`
	ScheduledAt time.Time `json:"scheduled_at"`
	State       string    `json:"state,omitempty"`
}

// ListInvocationsResponse lives in cmd/apid because pkg/api cannot
// import pkg/state (cyclic). The handler-local type is `[]state.Invocation`
// — the wire shape is identical, only the package differs.

// InvokeRequest is the body for POST /v1/apps/{slug}/invoke[/async].
// Method defaults to POST; path defaults to `/` (the handler fills
// defaults; the zero values are not persisted).
type InvokeRequest struct {
	Payload json.RawMessage `json:"payload,omitempty"`
	Headers json.RawMessage `json:"headers,omitempty"`
	Method  string          `json:"method,omitempty"`
	Path    string          `json:"path,omitempty"`
}

// QueueSendRequest is the body for POST /v1/apps/{slug}/queues/send.
// Cap-checked against MaxQueueDepth at the handler.
type QueueSendRequest struct {
	Payload json.RawMessage `json:"payload,omitempty"`
}

// DelayedTaskRequest is the body for POST /v1/apps/{slug}/delayed-tasks.
// ScheduledAt must be in the future (UTC); the handler rejects past
// timestamps with invalid_scheduled_at.
type DelayedTaskRequest struct {
	Payload     json.RawMessage `json:"payload,omitempty"`
	ScheduledAt time.Time       `json:"scheduled_at"`
}

// Invocation is the SDK-side mirror of state.Invocation. The wire
// is the same JSON the handler emits (writeJSON(w, 200, inv) where
// inv is a state.Invocation), but pkg/api cannot import pkg/state
// (import cycle — state pkg is the lowest layer). The mirror is
// exhaustive: every field with a JSON tag on state.Invocation gets a
// typed row here so the SDK gets proper Go types and JSON tags. The
// name `Invocation` matches the OpenAPI schema (api/openapi.yaml
// `Invocation`) so the spec_compliance test sees a 1:1 mapping.
type Invocation struct {
	ID             string          `json:"id"`
	AppID          string          `json:"app_id"`
	AccountID      string          `json:"account_id"`
	InstanceID     string          `json:"instance_id,omitempty"`
	Source         string          `json:"source"`
	State          string          `json:"state"`
	Method         string          `json:"method"`
	Path           string          `json:"path"`
	Payload        json.RawMessage `json:"payload"`
	Headers        json.RawMessage `json:"headers"`
	DueAt          time.Time       `json:"due_at"`
	ScheduledAt    *time.Time      `json:"scheduled_at,omitempty"`
	AckURL         string          `json:"ack_url,omitempty"`
	Result         json.RawMessage `json:"result,omitempty"`
	LeaseExpiresAt *time.Time      `json:"lease_expires_at,omitempty"`
	ReceivedAt     *time.Time      `json:"received_at,omitempty"`
	CompletedAt    *time.Time      `json:"completed_at,omitempty"`
	Attempts       int             `json:"attempts"`
	LastError      string          `json:"last_error,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
}

// ListInvocationsResponse is the wire shape for GET /v1/invocations.
// The handler emits a `[]state.Invocation` under the `invocations`
// key; here we declare the same shape with the SDK-side mirror type
// so pkg/api stays decoupled from pkg/state.
type ListInvocationsResponse struct {
	Invocations []Invocation `json:"invocations"`
}

// --- Issue #394 — queue introspection -------------------------------
//
// QueueStateResponse is the read-only depth/stats contract for
// GET /v1/apps/{slug}/queues/state. NO lease is acquired by calling
// this endpoint. PlanCap is the static per-plan MaxQueueDepth so
// dashboards can render "depth / cap" without a second lookup.
//
// Wire-shape note on plan downgrades: PlanCap reflects the *current*
// account plan's cap at read time. After a downgrade (e.g. Pro
// MaxQueueDepth=25 → Free MaxQueueDepth=0), a customer whose queue
// has not yet drained will see `Plan: "free"` + `PlanCap: 0` +
// `Depth: <5-or-whatever>` — a "you have messages but no cap" wire
// shape. The dashboard surface should display the post-downgrade
// `PlanCap` as the *enforceable* cap and surface "over limit after
// downgrade" if `Depth > PlanCap`. Documented in the README so the
// dashboard team knows to mirror it.
//
// OldestPendingAt / OldestPendingAgeSeconds are omitted when the queue
// is empty (zero value); clients should treat absence as "no backlog".
type QueueStateResponse struct {
	AppSlug                 string     `json:"app_slug"`
	Plan                    string     `json:"plan"`
	PlanCap                 int        `json:"plan_cap"`
	Depth                   int        `json:"depth"`
	InFlight                int        `json:"in_flight"`
	OldestPendingAt         *time.Time `json:"oldest_pending_at,omitempty"`
	OldestPendingAgeSeconds *int64     `json:"oldest_pending_age_seconds,omitempty"`
	GeneratedAt             time.Time  `json:"generated_at"`
}

// QueuePeekMessage is one pending row returned by GET .../queues/peek.
// The handler does NOT acquire a lease and does NOT increment attempts —
// repeated peeks leave the underlying state byte-identical. Payload
// is rendered as a JSON string (the stored column is jsonb, surfaced
// verbatim) so callers can decode with their preferred JSON lib.
//
// LastError omits when the row has not yet failed (most rows in a
// healthy queue). Pending rows can carry a last_error if they were
// transiently failed and re-queued before being claimed again.
type QueuePeekMessage struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Attempts  int       `json:"attempts"`
	Payload   string    `json:"payload"`
	LastError string    `json:"last_error,omitempty"`
}

// QueuePeekResponse is the paginated contract. NextBefore is the
// id (UUID) of the LAST row in the returned page — invariant across
// endpoints: it is always "rows[len-1].ID in the order returned", not
// an anchor in some sort direction. Pass it as `?before=<id>` on the
// next call. Empty NextBefore means "no more pages" (caller stops).
// Caveat: NextBefore being present does NOT guarantee more rows
// exist — if the underlying table has exactly `limit` rows, the
// handler emits NextBefore and the next request returns empty.
// Clients must continue until NextBefore is absent on an empty list.
type QueuePeekResponse struct {
	AppSlug    string             `json:"app_slug"`
	Messages   []QueuePeekMessage `json:"messages"`
	NextBefore string             `json:"next_before,omitempty"`
}

// QueueDeadLetterMessage is one row that exhausted its plan's retry
// budget (state='dead_letter'). FailedAt is the moment the drain
// transitioned the row to terminal (== state.Invocation.CompletedAt).
// LastError is the most recent failure; Payload is preserved verbatim
// so an operator can replay it as a fresh send if needed.
//
// LastError has no omitempty: a dead-letter row that exhausted its
// retry budget ALWAYS carries a last_error (that's what dead-letter
// means). An absent last_error here would be a bug — pin it as
// required so a regression that drops it surfaces at PR review.
type QueueDeadLetterMessage struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	FailedAt  time.Time `json:"failed_at"`
	Attempts  int       `json:"attempts"`
	LastError string    `json:"last_error"`
	Payload   string    `json:"payload"`
}

// QueueDeadLetterResponse is the paginated contract for
// GET /v1/apps/{slug}/queues/dead_letter. Same cursor convention as
// QueuePeekResponse (NextBefore = last id, `?before=<id>` for the next
// page). Rows are ordered newest-first (created_at DESC) so operators
// see the most recent failures at the top.
type QueueDeadLetterResponse struct {
	AppSlug    string                   `json:"app_slug"`
	Messages   []QueueDeadLetterMessage `json:"messages"`
	NextBefore string                   `json:"next_before,omitempty"`
}

// --- IAM-4 (ADR-035) — auth audit event surface -----------------------------
//
// AuditEventResponse is one row of the customer's own security event
// timeline. The kind taxonomy is documented in
// docs/adr/035-auth-audit-events.md; common values include
// "auth.login", "auth.logout", "key.created", "key.deleted",
// "secret.set", "secret.deleted", "account.plan_changed",
// "account.deletion_scheduled", "account.deletion_restored".
//
// Subject is the account_id the event was recorded against (string,
// not the raw uuid UUID type — pkg/api stays string-typed for wire
// stability). Data is the raw jsonb the apid auditor wrote; the schema
// varies by kind and is documented per-kind in the ADR.
type AuditEventResponse struct {
	ID      string `json:"id"`    // bigint as string
	At      string `json:"at"`    // RFC 3339
	Actor   string `json:"actor"` // "apid" today; "schedd" for state-transition events
	Kind    string `json:"kind"`
	Subject string `json:"subject,omitempty"` // account_id (uuid string form)
	// Severity (Mega-PR B) is the highest-severity classification
	// for stateless.advisory rows; "" for all other kinds. Carried
	// at the top level so an SDK consumer can triage rows without
	// re-parsing the data JSONB blob — the data.severity field is
	// still the canonical storage shape, but the SDK shouldn't have
	// to know the kind-specific schema to learn the severity.
	// omitempty: pre-PR-427 rows and non-stateless kinds render
	// with no Severity field at all (backwards-compatible wire).
	Severity string          `json:"severity,omitempty"`
	Data     json.RawMessage `json:"data"`
}

// ListAuditEventsResponse is the wire shape for GET /v1/audit-events.
// Limit echoes the effective limit applied by the handler (capped at
// 100), so the SDK can display "showing 50 of N" without re-issuing
// the request.
type ListAuditEventsResponse struct {
	Events []AuditEventResponse `json:"events"`
	Limit  int                  `json:"limit"`
}

// --- GitHub install bind picker (PR-B; §11) ---------------------------------
//
// InstallBindRequest is the body for both POST /v1/install/repos/list
// and POST /v1/apps/{slug}/install/bind. ProductionBranch is
// optional — when omitted, githubd uses the install's default_branch
// from /installations/{id}.
//
// RepoFullName matches GitHub's owner/name shape (e.g. "octocat/
// hello-world"). The pattern is enforced server-side in handlers_install_github.go
// but kept loose here so the SDK can serialise any GitHub-shaped
// string the dashboard holds.
type InstallBindRequest struct {
	InstallationID   int64  `json:"installation_id"`
	RepoFullName     string `json:"repo_full_name"`
	ProductionBranch string `json:"production_branch,omitempty"`
}

// InstallBindResponse is the body the dashboard parses after a
// successful bind. BindingID is the deterministic
// "bind-<appID>-<repo>" form RealService.BindAppRepo emits; audit
// log entries reference it directly.
type InstallBindResponse struct {
	BindingID        string `json:"binding_id"`
	RepoFullName     string `json:"repo_full_name"`
	ProductionBranch string `json:"production_branch"`
}

// RepoResponse is one repo visible to the user's GitHub App
// installation, as returned by githubd's
// /user/installations/{id}/repositories. Carries only the fields the
// dashboard bind picker renders; no nested owner object (the
// install URL already disambiguates).
type RepoResponse struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
}

// AppMetricsResponse is the per-app metrics payload returned by
// GET /v1/apps/{slug}/metrics?range= (issue #273 / ADR-042).
//
// Time-windowed via the `range` query param (closed vocabulary, see
// server handler). When the underlying Prometheus client is
// unavailable, every numeric field is zero and Source is prefixed
// with "degraded: <reason>" — same contract as the public status
// page so the dashboard has one empty-state path.
//
// All percentage fields are clamped to [0, 100]; all latency
// fields are milliseconds ≥ 0. NaN/Inf come back as zero — see
// the server handler for the guard order.
type AppMetricsResponse struct {
	AppID  string `json:"app_id"`
	Range  string `json:"range"`  // echoed window, e.g. "5m"
	Source string `json:"source"` // "prometheus" on success, "degraded: <err>" otherwise
	AsOf   string `json:"as_of"`  // RFC3339Nano UTC
	// RequestCount is the count of gateway_requests_total{app} over the
	// window. Drives the empty-state message: 0 means "no requests in
	// the last 5m" rather than a row of zeros.
	RequestCount int64 `json:"request_count"`
	// LatencyP50MS / P95MS / P99MS are histogram_quantile(q) over
	// the 2xx class only — failures surface separately as
	// ErrorRatePct. NaN from histogram_quantile on an empty window
	// is coerced to 0 by the handler.
	LatencyP50MS float64 `json:"latency_p50_ms"`
	LatencyP95MS float64 `json:"latency_p95_ms"`
	LatencyP99MS float64 `json:"latency_p99_ms"`
	// ErrorRatePct is the share of [45]xx requests in the window.
	ErrorRatePct float64 `json:"error_rate_pct"`
	// ColdStartPct is the share of requests that triggered a cold
	// boot (the WakeGate leader — see ADR-042 §cold semantics).
	// Followers waiting on the gate show as zero cold contribution
	// but their wait is visible via gateway_wake_queue_wait_seconds
	// on the §12 dashboard.
	ColdStartPct float64 `json:"cold_start_pct"`
	// WakeP95MS is the FLEET wake p95 (gateway_wake_latency_seconds
	// is unlabeled — there is no per-app wake histogram). Labelled
	// as such in the UI; here it's named plainly because the
	// dashboard copy does the labelling.
	WakeP95MS float64 `json:"wake_p95_ms"`
	// EgressBytes (ADR-046, step 10) is the total
	// per-app egress byte delta over the window,
	// queried from vmmd_egress_net_tx_bytes_total{app}
	// (the Prometheus mirror of usage_minutes.net_tx_bytes;
	// the gateway-side tx_bytes mirror lands in PR-2).
	// Informational only — not billed. 0 when Prometheus
	// is degraded or the metric hasn't been emitted yet.
	// Unit: interface bytes (includes framing). The
	// future egress-billing PR picks the unit; this field
	// reports the Prometheus counter verbatim.
	EgressBytes int64 `json:"egress_bytes"`
}

// --- Account-scoped metrics rollup (issue #393) --------------------------

// AppsMetricsResponse is the rollup for GET /v1/apps/metrics?range=
// (issue #393) — one call replacing N per-app fan-outs. The wire
// shape mirrors AppMetricsResponse at the row level (each value is
// an AppMetricsResponse) so the SDK can reuse the per-app type for
// row decoding.
//
// Apps is keyed by app_slug so the dashboard can render the rows
// without a parallel /v1/apps lookup. Apps is nil (not {}) when the
// Prometheus client is unavailable — the Source field carries the
// "degraded: <reason>" contract from the per-app handler, so the
// dashboard has one empty-state branch across both endpoints.
//
// Range / Source / AsOf follow the per-app shape exactly. The
// per-app WakeP95MS is the FLEET p95 (gateway_wake_latency_seconds
// is unlabeled) — same here.
type AppsMetricsResponse struct {
	Range  string                        `json:"range"`
	Source string                        `json:"source"`
	AsOf   string                        `json:"as_of"`
	Apps   map[string]AppMetricsResponse `json:"apps"`
}

// ProjectScanRequest is the multipart body for POST /v1/projects/scan.
// Defined as a DTO (rather than an inline handler struct) so the
// schema-parity AST gate can assert field-for-field equivalence with
// the OpenAPI spec.
type ProjectScanRequest struct {
	Source           string `json:"source"`            // tar.gz binary blob
	ProjectSlug      string `json:"project_slug"`      // kebab slug
	ProductionBranch string `json:"production_branch"` // default "main"
	InstallID        int64  `json:"install_id"`        // GitHub install id (--repo); 0 for unbound
	Only             string `json:"only"`              // CSV of workload names
}

// ProjectApplyRequest is the multipart body for POST /v1/projects.
// Shape mirrors ProjectScanRequest — the handler re-runs the scan
// and re-checks the plan token internally.
type ProjectApplyRequest struct {
	Source           string `json:"source"`
	ProjectSlug      string `json:"project_slug"`
	ProductionBranch string `json:"production_branch"`
	InstallID        int64  `json:"install_id"`
	Only             string `json:"only"`
}

// PlanWorkload mirrors reposcan.Workload (Phase 3 wire shape).
// Field names match the OpenAPI schema verbatim — the spec-check
// AST gate enforces the field-for-field mapping.
type PlanWorkload struct {
	Name       string   `json:"name"`
	RootDir    string   `json:"root_dir"`
	Dockerfile string   `json:"dockerfile,omitempty"`
	Command    []string `json:"command"`
	Class      string   `json:"class,omitempty"`
	Schedule   string   `json:"schedule,omitempty"`
	Ports      []int    `json:"ports"`
	EnvKeys    []string `json:"env_keys,omitempty"`
	Source     string   `json:"source,omitempty"`
	Tier       string   `json:"tier,omitempty"`
}

// PlanManaged mirrors reposcan.Managed.
type PlanManaged struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	EnvHint string `json:"env_hint"`
	Source  string `json:"source"`
	Image   string `json:"image"`
}

// PlanCron is the per-cron line in the scan response. Carries the
// workload name (NOT the AppID — that's resolved at apply time).
type PlanCron struct {
	WorkloadName string `json:"workload_name"`
	Schedule     string `json:"schedule"`
	Path         string `json:"path"`
	Enabled      bool   `json:"enabled"`
}

// QuotaBlock is the limit + observed extension on a plan-quota
// problem. Mirrors api.Problem.WithLimit — emitted alongside any
// 402/403 quota response so the CLI can render "X/Y apps" without
// a second request.
type QuotaBlock struct {
	Limit    int64  `json:"limit,omitempty"`
	Observed int64  `json:"observed,omitempty"`
	DocsURL  string `json:"docs_url,omitempty"`
}

// PlanResponse is the dry-run response from POST /v1/projects/scan.
// Fields mirror scanPlanResponse in cmd/apid/scan_service.go; the
// DTO is the wire shape, the in-process struct is the
// handler-internal carrier.
type PlanResponse struct {
	ProjectSlug     string         `json:"project_slug"`
	RepoFullName    string         `json:"repo_full_name,omitempty"`
	ScanSource      string         `json:"scan_source"`
	Tier            string         `json:"tier"`
	Workloads       []PlanWorkload `json:"workloads"`
	Managed         []PlanManaged  `json:"managed"`
	Crons           []PlanCron     `json:"crons"`
	Warnings        []string       `json:"warnings,omitempty"`
	ObservedApps    int            `json:"observed_apps"`
	ObservedCrons   int            `json:"observed_crons"`
	LimitApps       int            `json:"limit_apps"`
	LimitCrons      int            `json:"limit_crons"`
	CanApply        bool           `json:"can_apply"`
	CronsNotAllowed bool           `json:"crons_not_allowed,omitempty"`
	PlanToken       string         `json:"plan_token"`
}

// ApplyResponse is the success body for POST /v1/projects. Carries
// the inserted project_id + per-app IDs so the CLI's --yes flow
// can render "applied: <slug> → <app_id>".
type ApplyResponse struct {
	PlanResponse
	ProjectID string             `json:"project_id"`
	Apps      []ApplyResponseApp `json:"apps"`
}

// ApplyResponseApp is the per-app line in the apply response.
type ApplyResponseApp struct {
	Slug string `json:"slug"`
	ID   string `json:"id"`
}
