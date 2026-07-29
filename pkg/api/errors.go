package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// AsProblem walks err's chain and returns the first *Problem. Returns nil
// if none of the wrapped errors is a *Problem. Used by gRPC handlers in
// pkg/vmmdgrpc to lift a Manager-emitted error without leaking internal
// strings through the wire.
func AsProblem(err error) *Problem {
	if err == nil {
		return nil
	}
	var p *Problem
	if errors.As(err, &p) {
		return p
	}
	return nil
}

// Problem is an RFC 7807 problem+json body. It is the platform's single error
// contract: apid emits it, the CLI and dashboard render it verbatim (spec
// §Conventions, UX spec §7). Every limit error carries the limit, the observed
// value, and a docs URL so the surface never has to invent copy.
type Problem struct {
	// Type is a docs URL identifying the problem class (RFC 7807 "type").
	Type string `json:"type"`
	// Title is a short, stable, human-readable summary.
	Title string `json:"title"`
	// Status is the HTTP status code, duplicated in the body per RFC 7807.
	Status int `json:"status"`
	// Code is a stable machine-readable string (e.g. "plan_limit_apps") that
	// clients branch on. It must never change once shipped.
	Code string `json:"code"`
	// Detail is the specific cause including the observed value.
	Detail string `json:"detail,omitempty"`
	// Limit and Observed are set on quota/limit errors (spec §Conventions).
	Limit    *int64 `json:"limit,omitempty"`
	Observed *int64 `json:"observed,omitempty"`
	// DocsURL points the user at the single next action.
	DocsURL string `json:"docs_url,omitempty"`
	// BillingPortalURL is set on payment_required (CodePayment) errors:
	// the operator-controlled billing portal URL with the account id
	// substituted. Optional; omitempty keeps the existing API shape
	// unchanged for every other error code.
	BillingPortalURL string `json:"billing_portal_url,omitempty"`
	// PaddleCheckoutURL is set on payment_required (CodePayment) errors
	// when the platform is running on the Paddle provider. Mirrors
	// BillingPortalURL's shape — the customer's next action is to land
	// on a Paddle-hosted checkout page for the target plan. Optional +
	// omitempty so the Stripe-default response shape is unchanged.
	// Mutually exclusive with BillingPortalURL on a single Problem: the
	// 402 carries either billing_portal_url (Stripe) or
	// paddle_checkout_url+tx_id (Paddle), never both.
	PaddleCheckoutURL string `json:"paddle_checkout_url,omitempty"`
	// TxID is the provider's transaction handle (Paddle: txn_…,
	// Stripe: empty). The dashboard renders this as a confirmation id
	// after the customer completes checkout. Empty on the Stripe path.
	TxID string `json:"tx_id,omitempty"`
	// extraHeaders are non-JSON response headers attached via WithHeader.
	// Kept unexported so the wire body (RFC 7807 problem+json) is
	// exactly the spec; WriteProblem flushes these onto the wire
	// before WriteHeader. nil = no extras.
	extraHeaders map[string][]string `json:"-"`
}

// Error implements the error interface so a Problem can flow through %w chains.
func (p *Problem) Error() string {
	if p.Detail != "" {
		return fmt.Sprintf("%s: %s", p.Code, p.Detail)
	}
	return p.Code
}

// WriteProblem renders p as an RFC 7807 problem+json response with its status
// code. Every HTTP surface (gatewayd, apid) uses this so error shape is uniform.
func WriteProblem(w http.ResponseWriter, p *Problem) {
	w.Header().Set("Content-Type", "application/problem+json")
	for k, vs := range p.extraHeaders {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}

// NewProblem builds a Problem with the common fields set.
func NewProblem(status int, code, title, detail string) *Problem {
	return &Problem{Status: status, Code: code, Title: title, Detail: detail}
}

// WithLimit annotates a Problem with the limit and observed value that tripped
// it, returning the same pointer for chaining.
func (p *Problem) WithLimit(limit, observed int64) *Problem {
	p.Limit = &limit
	p.Observed = &observed
	return p
}

// WithDocs sets the docs URL and returns the same pointer for chaining.
func (p *Problem) WithDocs(url string) *Problem {
	p.DocsURL = url
	return p
}

// WithHeader attaches a single response header to the Problem so
// gatewayd's writeWakeError can write it onto the wire without
// branches on each error code. Used today by the build-attestation
// transient-I/O path (Retry-After: 5 — review finding #1a on
// PR #322). Multiple WithHeader calls compose: each call appends a
// new key. Returns the same pointer for chaining.
func (p *Problem) WithHeader(key, value string) *Problem {
	if p.extraHeaders == nil {
		p.extraHeaders = map[string][]string{}
	}
	p.extraHeaders[key] = append(p.extraHeaders[key], value)
	return p
}

// HasHeader returns the slice of values attached to key (nil if
// none). Exposed so tests + callers can verify the header was
// recorded without reaching into the unexported field.
func (p *Problem) HasHeader(key string) []string {
	if p.extraHeaders == nil {
		return nil
	}
	return p.extraHeaders[key]
}

// Stable error codes (spec Appendix A, UX spec §7). Keep in sync with docs and
// the CLI's exit-code mapping.
const (
	CodePlanLimitApps   = "plan_limit_apps"
	CodePlanLimitRAM    = "plan_limit_ram"
	CodePlanLimitConcur = "plan_limit_concurrency"
	CodeSourceTooLarge  = "source_too_large"
	CodeSourceInvalid   = "source_invalid"
	CodeAppLayerTooBig  = "app_layer_too_large"
	CodeBuildUndetected = "build_undetected"
	CodeBuildOOM        = "build_oom"
	CodeBuildTimeout    = "build_timeout"
	CodeQuotaExhausted  = "quota_exhausted"
	CodeBillingPastDue  = "billing_past_due"
	// CodeBillingNotImplemented is returned when the selected
	// billing provider (FAAS_BILLING_PROVIDER) does not implement the
	// requested method (issue #279: Paddle's Refund). Distinct from
	// CodeForbidden / CodeValidation so the dashboard / CLI can
	// surface "switch providers to use this surface" instead of a
	// generic error. Maps to HTTP 501.
	CodeBillingNotImplemented = "billing_not_implemented"
	CodeCapacity              = "capacity_unavailable"
	CodeUnauthorized          = "unauthorized"
	// CodeForbidden is returned when the authenticated principal lacks
	// the scope required by the route (IAM-1, ADR-034). Distinct from
	// CodeUnauthorized so a customer can tell "I need to log in" from
	// "my key does not have permission for this endpoint".
	CodeForbidden  = "insufficient_scope"
	CodeNotFound   = "not_found"
	CodeValidation = "validation_failed"
	CodeConflict   = "conflict"
	// CodeInternal is returned by handlers when an unexpected server-side
	// failure surfaces to the caller (DB Tx commit, network blip, partial
	// state). Distinct from CodeCapacity (503, "we ran out of headroom")
	// because the failure mode is "we tried and didn't succeed" rather
	// than "we deliberately refused". Use this for any 500 where the
	// handler can't recover; pair with api.ErrInternal for a one-liner.
	CodeInternal = "internal_error"
	// CodeMFARequired is returned by requireMFA when a session-cookie
	// principal is mfa_pending and the route is not on the MFA
	// allowlist (IAM-2 / issue #186). Distinct from CodeForbidden so
	// the dashboard can pivot the message from "your key is wrong"
	// to "complete enrollment or step-up to continue".
	CodeMFARequired = "mfa_required"
	// CodeMFAInvalidCode is returned when /confirm, /verify, or
	// /recover validate a presented TOTP code / recovery code and
	// the comparison fails. The audit Emit fires regardless.
	CodeMFAInvalidCode = "mfa_invalid_code"
	// CodeSessionExpired is returned by the IAM-3 (ADR-039) cookie-
	// branch cross-check when the cookie's sid is empty (pre-
	// rollout), the row is gone, or the row is revoked. Distinct
	// from CodeUnauthorized so the dashboard can pivot to
	// "sign in again" rather than the umbrella "unauthorized"
	// message.
	CodeSessionExpired = "session_expired"
	// CodeSessionInvalid is the defensive sibling when the cookie's
	// AEAD-bound AccountID disagrees with the sessions row. AEAD
	// forgery on the same key ought to be unreachable; if it ever
	// fires the operator should investigate the key-sealing path.
	CodeSessionInvalid    = "session_invalid"
	CodeDomainNotVerified = "domain_not_verified"
	CodeCronInvalid       = "cron_invalid"
	CodeAlertRuleInvalid  = "alert_rule_invalid"
	CodeHandlerMissing    = "handler_missing"
	CodeImageRequired     = "image_required"
	CodeDeployFailed      = "deploy_failed"
	// CodeSigInvalid is returned by schedd when the layer's
	// signature fails verification (or is missing) on cold-boot.
	// The deployment transitions to DeployFailed with this code;
	// the wake that triggered the verify returns 503 to gatewayd
	// with the same code. ADR-038 §Consequences Compatibility.
	CodeSigInvalid       = "sig_invalid"
	CodeNoRollbackTarget = "no_rollback_target"

	// CodeScanCritical is returned by vmmd when the staged base
	// ext4's Grype scan sidecar reports a CRITICAL finding (or
	// is missing/unreadable) at boot time (issue #299). The
	// failure mode is policy-driven (a CRITICAL CVE is a known
	// bad, not an operator fault), so the code is SLO-exempt —
	// it's not a customer-actionable signal in the same way
	// capacity / build-failure codes are, but a sustained
	// non-zero rate signals an imaged regression (the
	// fail-closed scan-sidecar write path in
	// pkg/imaged/base_stage.go) or a fresh CVE drop in a base
	// image. The 503 status mirrors CodeSigInvalid's posture —
	// the wake request fails closed, the caller can retry after
	// the operator rebuilds the base.
	CodeScanCritical = "scan_critical"

	// CodeBuildSBOMUnavailable is returned by the SBOM GET handler
	// when no SBOM populator has run for this build (pre-PR build, or
	// the imaged syft populator in pkg/imaged/loop.go has not yet
	// landed). Distinct from build_provenance_not_found (which means
	// the populator INSERT itself failed at WARN — the build is
	// genuine but observability broke). 503 — the artefact may exist
	// later if the operator re-deploys imaged; the customer can
	// branch on the code and re-fetch.
	CodeBuildSBOMUnavailable = "build_sbom_unavailable"

	// CodePayment is the 402 response when an API-only plan change requires
	// a Stripe subscription the customer does not have (issue #142 / PR).
	// The Problem carries a BillingPortalURL extension so the dashboard
	// renders an actionable upsell button without a separate /v1/billing
	// endpoint. Distinct from CodeBillingPastDue because the failure mode
	// is "you cannot upgrade via API" rather than "your account is past
	// due" — the dashboard renders different copy for each.
	CodePayment = "payment_required"

	// Customer secrets (spec §11/G2). Plaintext VALUES never enter logs;
	// these codes are returned for quota / shape / size violations only.
	CodePlanLimitSecrets    = "plan_limit_secrets"
	CodeSecretInvalidKey    = "secret_invalid_key"
	CodeSecretValueTooLarge = "secret_value_too_large"
	CodeSecretNotFound      = "secret_not_found"

	// Customer env vars (issue #395 / ADR-045). Distinct codes from
	// CodeSecret* so the quota + audit shape is unambiguous to
	// dashboards and SDK callers — a `plan_limit_env_vars` is a config
	// quota, not a credential one.
	CodePlanLimitEnvVars    = "plan_limit_env_vars"
	CodeEnvVarInvalidKey    = "env_var_invalid_key"
	CodeEnvVarValueTooLarge = "env_value_too_large"
	CodeEnvVarNotFound      = "env_var_not_found"

	// Plan-tier feature gates (M8 §6.5). Distinct from CodePlanLimit*
	// because the failure mode is "your plan doesn't unlock this knob
	// at all" rather than "you used more than the plan allows".
	// Pro + Scale unlock min_instances; Free + Hobby get 403 and the
	// docs URL tells them which plans do.
	CodePlanMinInstancesNotAllowed = "plan_min_instances_not_allowed"
	// CodeInvalidMinInstances is a 422 for shape violations: < 0 or
	// > plan MaxConcurrency. Distinct from CodeValidation so the CLI
	// can render actionable retry guidance ("raise your plan or lower
	// --max-concurrency").
	CodeInvalidMinInstances = "invalid_min_instances"

	// Move 1 event-shaped surfaces (spec §4.4, §4.9). The CLI exit-code
	// table treats them as 403/422/402; surfacing the codes separately
	// lets the dashboard render a "move to Scale to lift the cap"
	// hint without parsing prose.
	CodePlanQueueDepth     = "plan_queue_depth"
	CodePlanSourceBytes    = "plan_source_bytes"
	CodePlanFeatureGated   = "plan_feature_gated"
	CodePlanDelayedCap     = "plan_delayed_tasks_cap"
	CodeInvocationNotFound = "invocation_not_found"
	// CodeBuildProvenanceNotFound is the ADR-038 / Tier 3 #197
	// B3.10-read sentinel. Distinct from a generic "no such build"
	// so the customer can branch: a build that exists with no
	// provenance row is the "populator INSERT failed + WARN logged"
	// outcome, not a 404 of the build itself.
	CodeBuildProvenanceNotFound = "build_provenance_not_found"

	// ADR-031 (tier-2 of the network roadmap) — per-app egress
	// allowlist. Same gate shape as MinInstances: the feature is
	// plan-locked (Pro/Scale only), and there are two distinct
	// failure modes that warrant distinct codes so the CLI can
	// render actionable retry guidance.
	//   * CodePlanEgressAllowlistNotAllowed = 403 "your plan does
	//     not unlock this knob at all" (Free/Hobby).
	//   * CodeEgressAllowlistTooLong = 400 "the PATCH carries more
	//     CIDRs than your plan caps" (Pro/Scale but the slice is
	//     too long; not a billing failure).
	CodePlanEgressAllowlistNotAllowed = "plan_egress_allowlist_not_allowed"
	CodeEgressAllowlistTooLong        = "egress_allowlist_too_long"

	// Issue #169 / #172 — per-app reactive scale-up targets. Same gate
	// shape as MinInstances: a single plan-locked feature with two
	// failure modes that warrant distinct codes so the CLI can render
	// actionable retry guidance.
	//   * CodePlanScaleUpNotAllowed = 403 "your plan does not unlock
	//     this knob at all" (Free for either target; Hobby for CPU).
	//   * CodeInvalidAutoscaleTargetRPS = 422 "value < 1 — RPS target
	//     must be positive".
	//   * CodeInvalidAutoscaleTargetCPUPct = 422 "value outside [1, 100]".
	CodePlanScaleUpNotAllowed     = "plan_autoscale_not_allowed"
	CodeInvalidAutoscaleTargetRPS = "invalid_autoscale_target_rps"
	CodeInvalidAutoscaleTargetCPU = "invalid_autoscale_target_cpu_pct"
	// CodeInvalidEgressAllowlist is a 400 for shape violations:
	// an entry that doesn't ParsePrefix, or a v6 CIDR (v1 is v4
	// only; v6 mirror is a separate ADR).
	CodeInvalidEgressAllowlist = "invalid_egress_allowlist"

	// Account self-service (spec §17 G6, ADR-021). The
	// "confirm_required" code is returned when a DELETE arrives without
	// the confirmation header so a stale CLI prompt can't silently wipe
	// an account. The "pending" code carries the restore_until envelope
	// the customer needs to call POST /v1/account/restore. The
	// "not_restorable" code is the post-grace 409.
	CodeAccountDeletionConfirm = "account_deletion_confirm_required"
	CodeAccountDeletionPending = "account_deletion_pending"
	CodeAccountNotRestorable   = "account_not_restorable"

	// App rename (issue #63). One code covers both "slug taken by
	// another live app" and "DB unique violation"; the Detail field
	// distinguishes the two so the CLI can render actionable guidance.
	CodeAppRenameFailed = "app_rename_failed"

	// Image pull failure modes (ADR-021, spec §17 G1). The three codes
	// here are the customer-facing stable string for the puller-side
	// sentinels in pkg/oci/errors.go. imaged's buildImageLayer failure
	// path runs SentinelToCode(err) to pick one of these, persists it on
	// deployments.error_code, and the wake path lifts it into the
	// RFC 7807 Problem at the corresponding HTTP status below.
	//
	// Why three codes, not one: each signals a different remediation
	// path. image_not_found → check the digest / tag. image_egress_denied
	// → check the registry is in the public ranges (and isn't metadata
	// 169.254/16). image_manifest_invalid → pin to a single-arch digest,
	// the manifest-list rejection is part of the same code so dashboards
	// can group "wrong artifact shape" together.
	CodeImageNotFound        = "image_not_found"
	CodeImageEgressDenied    = "image_egress_denied"
	CodeImageManifestInvalid = "image_manifest_invalid"

	// CodeStatelessOnlyViolation is the single customer-facing code for
	// the stateless-only contract (Wave 0, year-one positioning). It
	// fires in three cases:
	//   - apid at deploy-accept time when a Dockerfile contains a
	//     VOLUME instruction, a mkfs/mount -t ext4|xfs call inside a
	//     RUN, or a top-level data/ or db/ directory (cmd/apid/deploy_inputs.go).
	//   - imaged at build time when the resolved OCI base image matches
	//     StatefulBaseImageDenylist (pkg/imaged/base.go).
	//   - guest-init at runtime (advisory only, never blocking — see
	//     guest/init/stateless_advisory_linux.go).
	// The single code keeps the customer-facing remediation path
	// identical (bring your own managed state) regardless of where the
	// violation was caught. The Detail field distinguishes the three.
	CodeStatelessOnlyViolation = "stateless_only_violation"

	// CLI auth (spec §2.2 device-code flow). Pending is the "user has
	// not yet approved" signal the CLI's poll loop keys off; the CLI
	// keeps polling until it sees 200 OK or a different 4xx. The
	// "unavailable" code covers every other failure mode (expired,
	// already used, unknown) — the CLI does not need to distinguish
	// them, and returning a single code avoids probing.
	CodeCliAuthPending     = "cli_auth_code_pending"
	CodeCliAuthUnavailable = "cli_auth_code_unavailable"

	// CodeAppConcurReached is the typed "already at max_concurrency"
	// result from Engine.AdmitInstance (issue #168). Distinct from
	// CodePlanLimitConcur because the gateway treats this as a benign
	// no-op when it already has ≥1 cached target, while plan_limit
	// (the Wake path) is always fatal to the requesting call.
	CodeAppConcurReached = "app_concurrency_reached"

	// Dashboard auth (issue #165, ADR-032). Pre-#165, POST /login
	// auto-created an account + minted a "web-console" API key + set
	// the session cookie on ANY email with zero verification, which
	// was a full pre-auth account-takeover (spec §11 violation).
	// Post-#165, the dashboard surfaces are real auth:
	//
	//   - invalid_credentials: 401 for both "no such email" and
	//     "wrong password" — the two paths share the same response
	//     body so the surface doesn't leak which case it hit. The
	//     constant-time Argon2id pad on the no-account path closes
	//     the timing oracle; see cmd/apid/handlers_auth.go.
	//   - email_not_verified: 401 when a Google / GitHub OAuth
	//     callback returns a profile whose primary email is not
	//     verified by the provider. Distinct from invalid_credentials
	//     because the customer can fix this by verifying the email
	//     upstream; we never mint an unverified session.
	//   - password_too_weak: 400 at /signup when the password fails
	//     the NIST-style floor (≥12 chars). The Detail names the
	//     rule so the dashboard form can highlight which constraint
	//     tripped.
	//   - reset_token_invalid / reset_token_expired: 410 for GET /
	//     POST /auth/reset when the token doesn't exist (invalid)
	//     or has aged past 15 minutes (expired). 410 Gone is the
	//     semantically correct status: the resource was a one-shot
	//     and is no longer addressable.
	//   - account_exists: never returned directly. Anti-enumeration
	//     keeps the body identical between "signed in via /signup"
	//     and "email already taken"; the constant exists so future
	//     surfaces (e.g. an explicit "claim this email" admin tool)
	//     can branch on it without inventing a new code.
	CodeInvalidCredentials = "invalid_credentials"
	CodeEmailNotVerified   = "email_not_verified"
	CodePasswordTooWeak    = "password_too_weak"
	CodeResetTokenInvalid  = "reset_token_invalid"
	CodeResetTokenExpired  = "reset_token_expired"
	CodeAccountExists      = "account_exists"
)

// SecretKeyPattern is the regex enforced by the app_secrets.key CHECK constraint
// (migrations/00005_secrets.sql) AND the apid input validator. Uppercase ASCII,
// digits, underscores; must start with a letter. Plain ASCII keeps the path
// stable across runtimes (no Unicode normalization gotchas) and matches what
// every shell / k8s / systemd treats as an env-var name.
const SecretKeyPattern = `^[A-Z][A-Z0-9_]*$`

// MaxSecretKeyLen bounds the secret key name. Mirrors Unix env-var limits
// (NAME_MAX is 255 on Linux) and keeps per-row index size reasonable.
const MaxSecretKeyLen = 128

// StatusForCode returns the HTTP status a given stable Code maps to. It is the
// inverse of the per-code status the constructors below hardcode, kept in one
// table so any surface that reconstructs a Problem without a Status (notably
// pkg/grpcerr.FromStatus, which lifts a gRPC error back into a Problem carrying
// only the Code) can recover the right HTTP status. Unknown codes default to
// 500 — a reconstructed Problem is never served without a real status.
func StatusForCode(code string) int {
	switch code {
	case CodePlanLimitApps, CodePlanLimitRAM, CodeAppLayerTooBig, CodeBillingPastDue:
		return http.StatusForbidden
	case CodePlanLimitConcur, CodeQuotaExhausted, CodeAppConcurReached:
		return http.StatusTooManyRequests
	case CodeSourceTooLarge:
		return http.StatusRequestEntityTooLarge
	case CodeSourceInvalid, CodeBuildUndetected, CodeValidation, CodeCronInvalid,
		CodeAlertRuleInvalid, CodeHandlerMissing, CodeImageRequired:
		return http.StatusBadRequest
	case CodeCapacity, CodeBuildOOM, CodeBuildTimeout:
		return http.StatusServiceUnavailable
	case CodeScanCritical:
		// 503 — the base ext4 has a CRITICAL Grype finding
		// (issue #299). SLO-exempt: a CRITICAL CVE is a known
		// bad, not an operator fault, and the wake must fail
		// closed regardless of customer SLO posture. The
		// operator rebuilds the base to clear the sidecar.
		return http.StatusServiceUnavailable
	case CodeBuildSBOMUnavailable:
		// 503 — the SBOM populator hasn't run (issue #299 /
		// ADR-038 Phase 3). The build row itself is final; the
		// SBOM artefact is best-effort post-mortem. SLO-exempt
		// for the same reason as CodeScanCritical: "missing
		// observational metadata" is not a customer-impacting
		// fault, and the SDK distinguishes 404 build-not-found
		// from 503 SBOM-missing so customer agents can branch.
		return http.StatusServiceUnavailable
	case CodeUnauthorized:
		return http.StatusUnauthorized
	case CodeSessionExpired, CodeSessionInvalid:
		return http.StatusUnauthorized
	case CodeNotFound:
		return http.StatusNotFound
	case CodeConflict, CodeDomainNotVerified, CodeNoRollbackTarget:
		return http.StatusConflict
	case CodeDeployFailed:
		return http.StatusUnprocessableEntity
	case CodeImageNotFound, CodeImageManifestInvalid:
		return http.StatusUnprocessableEntity
	case CodeImageEgressDenied:
		return http.StatusForbidden
	case CodeStatelessOnlyViolation:
		// 422 — the deploy shape (or resolved base image) is a stateful
		// one this platform does not support in year one. Sits next to
		// CodeDeployFailed: well-formed request, content policy refuses.
		// imaged also lifts this code onto deployments.error_code, so
		// the GET /v1/deployments/{id} response and the CLI's
		// `faas deployment <id>` render it identically.
		return http.StatusUnprocessableEntity
	case CodePayment:
		return http.StatusPaymentRequired
	case CodePlanLimitSecrets:
		return http.StatusForbidden
	case CodeSecretInvalidKey, CodeSecretNotFound:
		return http.StatusBadRequest
	case CodeSecretValueTooLarge:
		return http.StatusRequestEntityTooLarge
	// Env vars (issue #395 / ADR-045): mirror the secrets status shape
	// so SDK callers can reuse the same error-decoding pattern. Plan
	// quota is 403, value size is 413, key regex + not-found are 400.
	case CodePlanLimitEnvVars:
		return http.StatusForbidden
	case CodeEnvVarInvalidKey, CodeEnvVarNotFound:
		return http.StatusBadRequest
	case CodeEnvVarValueTooLarge:
		return http.StatusRequestEntityTooLarge
	case CodeAccountDeletionConfirm, CodeAccountDeletionPending, CodeAccountNotRestorable:
		return http.StatusConflict
	case CodeAppRenameFailed:
		return http.StatusConflict
	case CodeCliAuthPending:
		return http.StatusNotFound
	case CodeCliAuthUnavailable:
		return http.StatusGone
	case CodePlanQueueDepth, CodePlanDelayedCap:
		return http.StatusForbidden
	case CodePlanSourceBytes:
		return http.StatusRequestEntityTooLarge
	case CodePlanFeatureGated:
		return http.StatusPaymentRequired
	case CodePlanCronsNotAllowed:
		return http.StatusPaymentRequired
	case CodePlanCronQuota:
		return http.StatusForbidden
	case CodePlanAlertRulesNotAllowed:
		return http.StatusPaymentRequired
	case CodePlanAlertRuleQuota:
		return http.StatusForbidden
	case CodeInvocationNotFound:
		return http.StatusNotFound
	case CodeInvalidCredentials, CodeEmailNotVerified:
		return http.StatusUnauthorized
	case CodePasswordTooWeak, CodeAccountExists:
		return http.StatusBadRequest
	case CodeResetTokenInvalid, CodeResetTokenExpired:
		return http.StatusGone
	default:
		return http.StatusInternalServerError
	}
}

// Convenience constructors for the most common limit errors keep call sites to
// one line and guarantee the limit/observed/docs fields are always populated.

// ErrPlanLimitApps is returned when a deploy would exceed the plan's app count.
func ErrPlanLimitApps(l Limits, observed int) *Problem {
	return NewProblem(http.StatusForbidden, CodePlanLimitApps,
		"App limit reached",
		fmt.Sprintf("%s plan allows %d deployed app(s); you have %d.", l.Plan, l.DeployedApps, observed)).
		WithLimit(int64(l.DeployedApps), int64(observed)).
		WithDocs("https://docs.DOMAIN/plans#apps")
}

// ErrPlanLimitRAM is returned when a requested ram_mb exceeds the plan cap.
func ErrPlanLimitRAM(l Limits, requestedMB int) *Problem {
	return NewProblem(http.StatusForbidden, CodePlanLimitRAM,
		"RAM over plan limit",
		fmt.Sprintf("%s plan caps %d MB/app; requested %d MB.", l.Plan, l.RAMMB, requestedMB)).
		WithLimit(int64(l.RAMMB), int64(requestedMB)).
		WithDocs("https://docs.DOMAIN/plans#ram")
}

// ErrAppLayerTooLarge is returned when the built app layer (deps + code) exceeds
// the plan's drive1 cap (spec §4.6). The message names the cap and observed size
// so the deploy failure is actionable.
func ErrAppLayerTooLarge(l Limits, observedBytes int64) *Problem {
	capBytes := int64(l.AppLayerMaxMB) * 1024 * 1024
	return NewProblem(http.StatusForbidden, CodeAppLayerTooBig,
		"App too large",
		fmt.Sprintf("%s plan caps the app layer at %d MB; built layer is %.1f MB.",
			l.Plan, l.AppLayerMaxMB, float64(observedBytes)/(1024*1024))).
		WithLimit(capBytes, observedBytes).
		WithDocs("https://docs.DOMAIN/build/limits#app-layer")
}

// ErrPlanLimitConcurrency is returned when waking another instance would exceed
// the app's concurrency (spec §4.3 admission, invariant §6.2-1).
func ErrPlanLimitConcurrency(l Limits, observed int) *Problem {
	return NewProblem(http.StatusTooManyRequests, CodePlanLimitConcur,
		"Concurrency limit reached",
		fmt.Sprintf("%s plan allows %d concurrent instance(s) per app; %d already live.", l.Plan, l.MaxConcurrency, observed)).
		WithLimit(int64(l.MaxConcurrency), int64(observed)).
		WithDocs("https://docs.DOMAIN/plans#concurrency")
}

// ErrCapacity is returned when admission is refused for lack of box capacity
// (RAM headroom or vCPU slots, spec §4.3). This should be near-impossible in
// practice — admission alerts fire long before customers see it (spec §12) — so
// it is a page for us, not just a message for them (UX spec §7).
// ErrAppConcurrencyReached is returned by Engine.AdmitInstance when the
// app is already at its effective max_concurrency (issue #168). The
// gateway treats this as a benign no-op when it already has ≥1 cached
// target; the Wire RPC carries the same information as a typed
// at_capacity boolean so the gateway never has to parse problems.
func ErrAppConcurrencyReached(l Limits, observed int) *Problem {
	return NewProblem(http.StatusTooManyRequests, CodeAppConcurReached,
		"App concurrency reached",
		fmt.Sprintf("%s plan allows %d concurrent instance(s) per app; %d already live.", l.Plan, l.MaxConcurrency, observed)).
		WithLimit(int64(l.MaxConcurrency), int64(observed)).
		WithDocs("https://docs.DOMAIN/plans#concurrency")
}

func ErrCapacity(detail string) *Problem {
	return NewProblem(http.StatusServiceUnavailable, CodeCapacity,
		"Briefly at capacity", detail).
		WithDocs("https://status.DOMAIN")
}

// ErrInternal is the catch-all 500 envelope for handler-side failures
// that aren't a deliberate refusal (use ErrCapacity for those) — DB
// commit errors, partial state, unexpected plumbing. Pairs with
// CodeInternal; the detail rides through verbatim because it surfaces
// in the operator's browser console as the only breadcrumb for the
// on-call engineer (the audit row carries the same text).
func ErrInternal(detail string) *Problem {
	return NewProblem(http.StatusInternalServerError, CodeInternal,
		"Internal Error", detail)
}

// ErrBillingNotImplemented is returned by an apid handler that
// invoked a billing.Provider method the selected provider (per
// FAAS_BILLING_PROVIDER) does not support (issue #279: Paddle's
// Refund). The 501 surfaces the seam so an operator picking the
// billing backend knows up front which surface areas it disables;
// today no apid handler invokes Provider.Refund — refunds are
// Stripe-webhook-observational only — so this helper exists for
// the future operator-initiated refund path. callers branch on
// errors.Is(err, billing.ErrNotImplemented) and route here.
func ErrBillingNotImplemented(detail string) *Problem {
	return NewProblem(http.StatusNotImplemented, CodeBillingNotImplemented,
		"Billing provider does not support this surface", detail).
		WithDocs("https://docs.DOMAIN/billing/providers")
}

// ErrSourceTooLarge is returned when an uploaded tarball exceeds the plan cap.
func ErrSourceTooLarge(l Limits, observedBytes int64) *Problem {
	capBytes := int64(l.SourceTarballMaxMB) * 1024 * 1024
	return NewProblem(http.StatusRequestEntityTooLarge, CodeSourceTooLarge,
		"Source too large",
		fmt.Sprintf("%s plan caps source at %d MB.", l.Plan, l.SourceTarballMaxMB)).
		WithLimit(capBytes, observedBytes).
		WithDocs("https://docs.DOMAIN/build/limits")
}

// ErrSourceInvalid is returned when a tarball fails shape validation
// (symlink escape, absolute path, >10k files, wrong magic bytes, etc.).
func ErrSourceInvalid(reason string) *Problem {
	return NewProblem(http.StatusBadRequest, CodeSourceInvalid,
		"Source invalid", reason).
		WithDocs("https://docs.DOMAIN/build/source")
}

// ErrStatelessOnlyViolation is returned when a deploy shape (or resolved
// base image) requires persistent state — VOLUME in Dockerfile, mkfs/mount
// of a block device, a top-level data/ or db/ directory in the tarball, or
// a base image like postgres:16 / redis:7 / mysql:8 — and the platform is
// stateless-only in year one.
//
// kind classifies where the violation was caught so the customer can fix
// the right thing: "dockerfile" → edit the Dockerfile, "tarball" → move
// data/, "base_image" → switch to a managed service. detail is the offending
// path/image and lands verbatim in the RFC 7807 body.
func ErrStatelessOnlyViolation(kind, detail string) *Problem {
	return NewProblem(http.StatusUnprocessableEntity, CodeStatelessOnlyViolation,
		"Stateless-only platform",
		fmt.Sprintf("%s: %s — this platform is stateless in year one; "+
			"use a managed service (S3/R2/Neon/Upstash/MongoDB Atlas).",
			kind, detail)).
		// docs.DOMAIN is the placeholder convention used by every
		// WithDocs() in this file — the placeholder resolves to the
		// real domain when the docs site ships. The actual /storage page
		// is added by PR-B (Wave 0, faas init + reference templates).
		// Until PR-B ships, the 404 is consistent with every other
		// placeholder URL in the file.
		WithDocs("https://docs.DOMAIN/storage")
}

// ErrDomainNotVerified is returned when a customer tries to bind a domain
// whose TXT challenge hasn't been satisfied yet (spec §7).
func ErrDomainNotVerified(domain string) *Problem {
	return NewProblem(http.StatusConflict, CodeDomainNotVerified,
		"Domain not verified",
		fmt.Sprintf("TXT challenge for %q not yet satisfied; publish the required TXT record and retry.", domain)).
		WithDocs("https://docs.DOMAIN/domains/verify")
}

// ErrCronInvalid is returned for malformed cron expressions.
func ErrCronInvalid(reason string) *Problem {
	return NewProblem(http.StatusBadRequest, CodeCronInvalid,
		"Invalid cron schedule", reason).
		WithDocs("https://docs.DOMAIN/crons")
}

// CodePlanCronsNotAllowed is the 402 the customer sees when the
// plan doesn't unlock crons at all (Free today). Mirrors the
// CodePlanFeatureGated shape — the dashboard renders an upsell
// prompt, not a "delete a cron to add another" hint, because the
// only path forward is a plan upgrade.
const CodePlanCronsNotAllowed = "plan_crons_not_allowed"

// CodePlanCronQuota is the 403 the customer sees when the plan
// DOES unlock crons but the per-app or per-account cap was reached.
// Distinct from CodePlanCronsNotAllowed so the CLI can branch on
// upsell-vs-delete copy without parsing the body.
const CodePlanCronQuota = "plan_cron_quota"

// CodePlanAlertRulesNotAllowed is the 402 the customer sees when
// the plan doesn't unlock alert rules at all (Free today; the
// plan-tier gate fires before loadApp so the slug's existence is
// never leaked). Mirrors the CodePlanFeatureGated shape — the
// dashboard renders an upsell prompt, not a quota hint, because
// the only path forward is a plan upgrade.
const CodePlanAlertRulesNotAllowed = "plan_alert_rules_not_allowed"

// CodePlanAlertRuleQuota is the 403 the customer sees when the
// plan DOES unlock alert rules but the per-app or per-account
// cap was reached. Distinct from CodePlanAlertRulesNotAllowed so
// the CLI can branch on upsell-vs-delete copy without parsing
// the body.
const CodePlanAlertRuleQuota = "plan_alert_rule_quota"

// ErrPlanCronsNotAllowed is returned by apid's createCron handler
// when the customer's plan has CronLimitPerApp == 0 (Free today).
// Fires BEFORE the store is touched so a Free customer gets a clean
// 402 instead of a quota-error round-trip.
func ErrPlanCronsNotAllowed(p Plan) *Problem {
	return NewProblem(http.StatusPaymentRequired, CodePlanCronsNotAllowed,
		"Crons unavailable on this plan",
		fmt.Sprintf("the %s plan does not include cron; upgrade to Hobby or above to schedule synthetic requests.", p)).
		WithDocs("https://docs.DOMAIN/plans#crons")
}

// ErrPlanCronQuota is returned when CreateCronIfUnderQuota surfaces
// a *state.CronQuotaError. Scope "app" or "account" tells the
// handler which cap fired so the body can name it. 403 (not 402)
// because the plan DOES unlock crons — the right copy is
// "delete a cron to add another", not "upgrade to Hobby".
func ErrPlanCronQuota(plan Plan, scope string, limit, observed int) *Problem {
	var scopeName string
	if scope == "account" {
		scopeName = "this account"
	} else {
		scopeName = "this app"
	}
	return NewProblem(http.StatusForbidden, CodePlanCronQuota,
		"Cron limit reached",
		fmt.Sprintf("%s plan caps crons at %d for %s; you have %d. Delete one to add another.",
			plan, limit, scopeName, observed)).
		WithLimit(int64(limit), int64(observed)).
		WithDocs("https://docs.DOMAIN/plans#crons")
}

// ErrAlertRuleInvalid is returned for malformed alert-rule bodies:
// closed-set metric/comparison/window_spec/failure_source drift,
// non-finite threshold, cooldown band breach, oversized webhook
// secret, or a metric-family swap that the xor_chk constraint
// would reject at the DB. Mirrors ErrCronInvalid's shape so the
// CLI can use one problem-code table for both surfaces.
func ErrAlertRuleInvalid(reason string) *Problem {
	return NewProblem(http.StatusBadRequest, CodeAlertRuleInvalid,
		"Invalid alert rule", reason).
		WithDocs("https://docs.DOMAIN/alerts")
}

// ErrPlanAlertRulesNotAllowed is returned by apid's createAlertRule
// / listAlertRules handlers when the customer's plan has
// AlertRuleLimitPerApp == 0 (Free today). Fires BEFORE loadApp so a
// Free customer posting to a non-existent slug gets a clean 402
// instead of a 404 that would leak the slug's existence (PR review
// finding F4). Mirrors ErrPlanCronsNotAllowed.
func ErrPlanAlertRulesNotAllowed(p Plan) *Problem {
	return NewProblem(http.StatusPaymentRequired, CodePlanAlertRulesNotAllowed,
		"Alert rules unavailable on this plan",
		fmt.Sprintf("the %s plan does not include alert rules; upgrade to Hobby or above to fire alerts.", p)).
		WithDocs("https://docs.DOMAIN/plans#alerts")
}

// ErrPlanAlertRuleQuota is returned when
// CreateAlertRuleIfUnderQuota surfaces a *state.AlertRuleQuotaError.
// Scope "app" or "account" tells the handler which cap fired so
// the body can name it. 403 (not 402) because the plan DOES unlock
// alert rules — the right copy is "delete a rule to add another",
// not "upgrade to Hobby". Mirrors ErrPlanCronQuota.
func ErrPlanAlertRuleQuota(plan Plan, scope string, limit, observed int) *Problem {
	var scopeName string
	if scope == "account" {
		scopeName = "this account"
	} else {
		scopeName = "this app"
	}
	return NewProblem(http.StatusForbidden, CodePlanAlertRuleQuota,
		"Alert rule limit reached",
		fmt.Sprintf("%s plan caps alert rules at %d for %s; you have %d. Delete one to add another.",
			plan, limit, scopeName, observed)).
		WithLimit(int64(limit), int64(observed)).
		WithDocs("https://docs.DOMAIN/plans#alerts")
}

// ErrHandlerMissing is returned when a function source upload doesn't
// include a handler (spec §4.9).
func ErrHandlerMissing() *Problem {
	return NewProblem(http.StatusBadRequest, CodeHandlerMissing,
		"Handler required",
		"function deploys require a handler path (e.g. handler.handler)").
		WithDocs("https://docs.DOMAIN/functions")
}

// ErrDeployFailed wraps a deployment failure message into a Problem so the
// CLI can render it uniformly with quota errors.
func ErrDeployFailed(detail string) *Problem {
	return NewProblem(http.StatusUnprocessableEntity, CodeDeployFailed,
		"Deploy failed", detail).
		WithDocs("https://docs.DOMAIN/deploys")
}

// ErrNoRollbackTarget is returned by POST /v1/apps/{slug}/rollback when no
// superseded deployment exists (spec §9 line 376).
func ErrNoRollbackTarget() *Problem {
	return NewProblem(http.StatusConflict, CodeNoRollbackTarget,
		"No previous deployment",
		"there's no superseded deployment to roll back to; deploy at least twice.").
		WithDocs("https://docs.DOMAIN/deploys#rollback")
}

// ErrPlanLimitSecrets is returned when a secret PUT would exceed the plan's
// per-app secret count (spec §11/G2). Observed is the post-write count.
func ErrPlanLimitSecrets(l Limits, observed int) *Problem {
	return NewProblem(http.StatusForbidden, CodePlanLimitSecrets,
		"Secret count limit reached",
		fmt.Sprintf("%s plan allows %d secret(s) per app; you have %d.", l.Plan, l.SecretCountMax, observed)).
		WithLimit(int64(l.SecretCountMax), int64(observed)).
		WithDocs("https://docs.DOMAIN/secrets#limits")
}

// ErrSecretInvalidKey is returned when a secret key fails the
// ^[A-Z][A-Z0-9_]*$ pattern. Detail names the specific failure so the CLI can
// render an actionable message.
func ErrSecretInvalidKey(detail string) *Problem {
	return NewProblem(http.StatusBadRequest, CodeSecretInvalidKey,
		"Invalid secret key",
		fmt.Sprintf("secret keys must match %s; %s", SecretKeyPattern, detail)).
		WithDocs("https://docs.DOMAIN/secrets#keys")
}

// ErrSecretValueTooLarge is returned when a PUT value exceeds
// Limits.SecretValueMaxBytes. apid checks the byte length of the request body
// BEFORE sealing so the cap is enforced on the wire (no over-quota ciphertext
// ever lands in PG).
func ErrSecretValueTooLarge(l Limits, observedBytes int) *Problem {
	return NewProblem(http.StatusRequestEntityTooLarge, CodeSecretValueTooLarge,
		"Secret value too large",
		fmt.Sprintf("%s plan caps secret values at %d bytes; got %d.", l.Plan, l.SecretValueMaxBytes, observedBytes)).
		WithLimit(int64(l.SecretValueMaxBytes), int64(observedBytes)).
		WithDocs("https://docs.DOMAIN/secrets#limits")
}

// ErrSecretNotFound is returned by DELETE /v1/apps/{slug}/secrets/{key} when
// the key isn't set on the app. Distinct from CodeNotFound because the URL
// shape (the resource IS the secret) is intentional.
func ErrSecretNotFound(key string) *Problem {
	return NewProblem(http.StatusBadRequest, CodeSecretNotFound,
		"Secret not set",
		fmt.Sprintf("no secret named %q on this app.", key)).
		WithDocs("https://docs.DOMAIN/secrets")
}

// ErrPlanLimitEnvVars is returned when an env PUT would exceed the plan's
// per-app env-var count (issue #395 / ADR-045). Observed is the post-write
// count. The 403 mirrors ErrPlanLimitSecrets so the SDK's error decoder can
// share the quota-reached branch.
func ErrPlanLimitEnvVars(l Limits, observed int) *Problem {
	return NewProblem(http.StatusForbidden, CodePlanLimitEnvVars,
		"Env var count limit reached",
		fmt.Sprintf("%s plan allows %d env var(s) per app; you have %d.", l.Plan, l.EnvVarsMax, observed)).
		WithLimit(int64(l.EnvVarsMax), int64(observed)).
		WithDocs("https://docs.DOMAIN/env#limits")
}

// ErrEnvVarInvalidKey is returned when an env key fails the
// ^[A-Z][A-Z0-9_]*$ pattern. Detail names the specific failure so the CLI
// can render an actionable message. The regex intentionally reuses the
// SecretKeyPattern constant because POSIX env-var naming and the secrets
// naming surface share the same ASCII identifier grammar — keeping one
// pattern avoids the drift where two regexes diverge over time.
func ErrEnvVarInvalidKey(detail string) *Problem {
	return NewProblem(http.StatusBadRequest, CodeEnvVarInvalidKey,
		"Invalid env var key",
		fmt.Sprintf("env var keys must match %s; %s", SecretKeyPattern, detail)).
		WithDocs("https://docs.DOMAIN/env#keys")
}

// ErrEnvVarValueTooLarge is returned when a PUT value exceeds
// Limits.EnvValueMaxBytes. The byte length is checked against the request
// body BEFORE the row hits PG so the cap is enforced on the wire (no
// over-quota value ever lands in app_envs).
func ErrEnvVarValueTooLarge(l Limits, observedBytes int) *Problem {
	return NewProblem(http.StatusRequestEntityTooLarge, CodeEnvVarValueTooLarge,
		"Env var value too large",
		fmt.Sprintf("%s plan caps env values at %d bytes; got %d.", l.Plan, l.EnvValueMaxBytes, observedBytes)).
		WithLimit(int64(l.EnvValueMaxBytes), int64(observedBytes)).
		WithDocs("https://docs.DOMAIN/env#limits")
}

// ErrEnvVarNotFound is returned by DELETE /v1/apps/{slug}/env/{key} when
// the key isn't set on the app. Distinct from CodeNotFound for the same
// reason as ErrSecretNotFound: the URL shape makes the resource the env
// var, not the app.
func ErrEnvVarNotFound(key string) *Problem {
	return NewProblem(http.StatusBadRequest, CodeEnvVarNotFound,
		"Env var not set",
		fmt.Sprintf("no env var named %q on this app.", key)).
		WithDocs("https://docs.DOMAIN/env")
}

// ErrPlanMinInstancesNotAllowed is returned when a Free or Hobby account
// tries to set apps.min_instances (ux_spec §6.5, plan-tier gate). The
// customer's bill on these plans is built around scale-to-zero; a
// floor keeps N × RAMMB resident at all times, which is the cost
// shape of Pro / Scale.
func ErrPlanMinInstancesNotAllowed(p Plan) *Problem {
	return NewProblem(http.StatusForbidden, CodePlanMinInstancesNotAllowed,
		"Plan doesn't allow a min-instances floor",
		fmt.Sprintf("the %s plan always scales to zero; upgrade to Pro or Scale to keep instances warm.", p)).
		WithDocs("https://docs.DOMAIN/plans#min-instances")
}

// ErrInvalidMinInstances is returned when the requested min_instances
// is negative or exceeds the plan's max concurrency. 422 (not 403)
// because the request shape is wrong, not the plan.
func ErrInvalidMinInstances(got, maxConcur int) *Problem {
	return NewProblem(http.StatusUnprocessableEntity, CodeInvalidMinInstances,
		"Invalid min_instances",
		fmt.Sprintf("min_instances must be in [0, %d] (plan max_concurrency); got %d.", maxConcur, got)).
		WithLimit(int64(maxConcur), int64(got)).
		WithDocs("https://docs.DOMAIN/apps#min-instances")
}

// ErrPlanEgressAllowlistNotAllowed (ADR-031) is returned when a Free or Hobby
// account tries to set apps.egress_allowlist. Same gate shape as
// ErrPlanMinInstancesNotAllowed: the knob is plan-locked, and Pro/Scale
// is where the operator surface lives. The plan is named in the body so
// a CLI prompt can render "upgrade to Pro to unlock this knob" without
// a second lookup.
func ErrPlanEgressAllowlistNotAllowed(p Plan) *Problem {
	return NewProblem(http.StatusForbidden, CodePlanEgressAllowlistNotAllowed,
		"Plan doesn't allow an egress allowlist",
		fmt.Sprintf("the %s plan cannot pin an egress IP allowlist; upgrade to Pro or Scale to unlock this operator surface.", p)).
		WithDocs("https://docs.DOMAIN/apps#egress-allowlist")
}

// ErrEgressAllowlistTooLong (ADR-031) is returned when the PATCH carries more
// CIDRs than the plan's per-app cap. 400 (not 422) because the request shape is
// well-formed — only the count is over budget. The limit + observed pair rides
// on the Problem so the CLI can branch on its own copy of the cap (no re-fetch).
func ErrEgressAllowlistTooLong(got, maxSize int) *Problem {
	return NewProblem(http.StatusBadRequest, CodeEgressAllowlistTooLong,
		"Egress allowlist too long",
		fmt.Sprintf("egress_allowlist has %d entries; plan caps it at %d.", got, maxSize)).
		WithLimit(int64(maxSize), int64(got)).
		WithDocs("https://docs.DOMAIN/apps#egress-allowlist")
}

// ErrInvalidEgressAllowlist (ADR-031 + ADR-032) is a 400 for
// entries that don't ParsePrefix as a v4 or v6 CIDR, or that
// have masklen /0. The detail names the offending entry so an
// operator triaging a rejected PATCH sees exactly which line is
// bad. ADR-032 — v6 entries are accepted alongside v4 entries;
// the non-/0 contract is shared with the DB trigger.
func ErrInvalidEgressAllowlist(entry string, reason error) *Problem {
	return NewProblem(http.StatusBadRequest, CodeInvalidEgressAllowlist,
		"Invalid egress allowlist entry",
		fmt.Sprintf("entry %q is not a valid v4 or v6 CIDR (non-/0): %v.", entry, reason)).
		WithDocs("https://docs.DOMAIN/apps#egress-allowlist")
}

// ErrValidation is a 400 fallback for malformed request bodies. Used by
// handlers when JSON decode fails — the underlying error detail isn't
// surfaced (it's attacker-influenced) but the cause class is the same
// across handlers.
func ErrValidation(detail string) *Problem {
	return NewProblem(http.StatusBadRequest, CodeValidation,
		"Validation failed", detail)
}

// ErrPlanQueueDepth is returned by the apid handlers on POST
// .../queues/invocations:send (and on delayed-task create) when
// accepting the row would push the per-app live queue/depth past
// the plan cap. Observed is the current live count (matching the
// response payload so dashboards can render the gauge without a
// second round-trip).
func ErrPlanQueueDepth(limit, observed int) *Problem {
	return NewProblem(http.StatusForbidden, CodePlanQueueDepth,
		"Per-app queue depth exceeded",
		fmt.Sprintf("the plan caps this app at %d pending + dispatching rows; observed %d.", limit, observed)).
		WithLimit(int64(limit), int64(observed)).
		WithDocs("https://docs.DOMAIN/event-driven#queue-depth")
}

// ErrPlanSourceBytes is returned when a request body for an event-shaped
// surface (sync / async / queue :send / delayed-task create) exceeds
// the per-plan MaxSourceBytesPerInvocation.
func ErrPlanSourceBytes(limit int, observed int64) *Problem {
	return NewProblem(http.StatusRequestEntityTooLarge, CodePlanSourceBytes,
		"Invocation payload too large",
		fmt.Sprintf("this plan caps each invocation at %d bytes; observed %d.", limit, observed)).
		WithLimit(int64(limit), observed).
		WithDocs("https://docs.DOMAIN/event-driven#payload-size")
}

// ErrPlanFeatureGated is returned when the customer's plan does not
// unlock the requested surface (spec §4.4 reserves async invoke and
// queues for paid tiers; Free customers get 402 with the upgrade
// nudge). Code differs from CodePlanLimit* because the failure mode
// is plan-gating, not "you used more than the plan allows".
func ErrPlanFeatureGated(feature string, p Plan) *Problem {
	return NewProblem(http.StatusPaymentRequired, CodePlanFeatureGated,
		"Plan doesn't include this feature",
		fmt.Sprintf("the %s plan doesn't unlock %s; upgrade to Hobby or higher to use event-driven features.", p, feature)).
		WithDocs("https://docs.DOMAIN/plans#event-driven")
}

// ErrPlanDelayedTasksCap is the variant surfaced when a delayed-task
// schedule would push the per-app delayed-task count past the plan
// cap. Distinct code so the dashboard can suggest "schedule later"
// vs the queue-depth case which is a stricter 403.
func ErrPlanDelayedTasksCap(limit, observed int) *Problem {
	return NewProblem(http.StatusForbidden, CodePlanDelayedCap,
		"Per-app delayed-task cap exceeded",
		fmt.Sprintf("the plan caps this app at %d scheduled delayed_tasks; observed %d.", limit, observed)).
		WithLimit(int64(limit), int64(observed)).
		WithDocs("https://docs.DOMAIN/event-driven#delayed-tasks")
}

// ErrInvocationNotFound is the Move 1 counterpart to ErrSecretNotFound:
// the URL name (the resource IS the invocation) is intentional, and a
// generic not_found would force the CLI to parse the message.
func ErrInvocationNotFound(id string) *Problem {
	return NewProblem(http.StatusNotFound, CodeInvocationNotFound,
		"Invocation not found",
		fmt.Sprintf("no invocation with id %q on this account.", id)).
		WithDocs("https://docs.DOMAIN/event-driven#invocations")
}

// ErrBuildProvenanceNotFound is the ADR-038 surface for a build
// whose populator INSERT never landed (best-effort WARN inside
// builderd.recordProvenance) OR for a pre-PR build that pre-dates
// build_provenance entirely. Distinct from "no such build" so the
// customer (and the dashboard) can branch on it. The build row is
// authoritative for the success/fail transition; the missing
// provenance is observational metadata.
func ErrBuildProvenanceNotFound() *Problem {
	return NewProblem(http.StatusNotFound, CodeBuildProvenanceNotFound,
		"Build provenance not found",
		"the build succeeded but no provenance row exists; builderd logged a warning when the populator failed").
		WithDocs("https://docs.DOMAIN/builds#provenance")
}

// ErrBuildSBOMUnavailable is the issue #299 / ADR-038 Phase 3 surface
// for `faas build sbom <id>` (and the SDK GetBuildsIdSbom) when no
// SBOM artefact has been stored for this build yet — either the imaged
// syft populator in pkg/imaged/loop.go hasn't landed (pre-PR build) or
// the populator INSERT was best-effort WARNed away. The 503 distinguishes
// "may exist later, retry" from the 404 "no such build". The SDK errors
// stays parseable so the CLI's "no SBOM for this build" path can branch
// on the code.
func ErrBuildSBOMUnavailable() *Problem {
	return NewProblem(http.StatusServiceUnavailable, CodeBuildSBOMUnavailable,
		"Build SBOM unavailable",
		"no SBOM has been generated for this build; imaged's syft populator did not run or did not persist the artefact").
		WithDocs("https://docs.DOMAIN/builds#sbom")
}

// ErrLongPollTimeout is returned by the long-poll handlers (sync
// invoke, queueReceive) when the server-side wait budget ran out.
// Distinct code so the CLI can retry transparently — a 504 Gateway
// Timeout would force the customer to disambiguate "server is down"
// from "no event yet, retry". The HTTP status is 504 (the SLO is
// server-side); the body type is the only ordering.
func ErrLongPollTimeout() *Problem {
	return NewProblem(http.StatusGatewayTimeout, "long_poll_timeout",
		"Long-poll wait budget ran out",
		"the server waited for the configured long-poll window and the event did not arrive; retry.").
		WithDocs("https://docs.DOMAIN/event-driven#long-poll")
}

// ErrInvalidScheduledAt is returned when a delayed-task POST carries a
// scheduled_at that is in the past (or zero). The handler uses time.Now()
// as the source of truth so a clock-skewed client gets a 400 rather than
// a row that fires immediately on insert.
func ErrInvalidScheduledAt() *Problem {
	return NewProblem(http.StatusBadRequest, "invalid_scheduled_at",
		"Invalid scheduled_at",
		"scheduled_at must be a future timestamp; the server clock rejected the value").
		WithDocs("https://docs.DOMAIN/event-driven#delayed-tasks")
}

// --- Dashboard auth (issue #165, ADR-032 PR #2) ----------------------------

// ErrInvalidCredentials is the 401 returned by POST /login (and the
// colliding /signup anti-enumeration path). The body is identical
// whether the email is unbound, the password is wrong, or the account
// has no password row — the spec §11 anti-enumeration invariant. The
// constant-time Argon2id pad on the no-account path closes the timing
// oracle; the response body and the wire status are the same on both
// branches.
func ErrInvalidCredentials() *Problem {
	return NewProblem(http.StatusUnauthorized, CodeInvalidCredentials,
		"Sign in failed",
		"email or password is incorrect.").
		WithDocs("https://docs.DOMAIN/auth/sign-in")
}

// ErrEmailNotVerified is the 401 returned by the Google / GitHub OAuth
// callback when the provider's profile has no primary verified email.
// Distinct from invalid_credentials because the customer can fix it
// upstream (verify the email on the provider) and retry. We never
// mint an unverified session.
func ErrEmailNotVerified(provider string) *Problem {
	return NewProblem(http.StatusUnauthorized, CodeEmailNotVerified,
		"Email not verified",
		fmt.Sprintf("the %s account's primary email is not verified; verify it on the provider and retry.", provider)).
		WithDocs("https://docs.DOMAIN/auth/oauth")
}

// ErrPasswordTooWeak is the 400 returned by POST /signup and POST
// /auth/reset when the password fails the NIST-style floor (≥12 chars,
// no complexity rules). The Detail names the rule so the form can
// highlight which constraint tripped.
func ErrPasswordTooWeak(reason string) *Problem {
	return NewProblem(http.StatusBadRequest, CodePasswordTooWeak,
		"Password too weak", reason).
		WithDocs("https://docs.DOMAIN/auth/password")
}

// ErrResetTokenInvalid is the 410 returned by GET / POST /auth/reset
// when the token doesn't exist (unknown / typo'd / already consumed).
// 410 Gone is the right status: the resource was a one-shot and is
// no longer addressable.
func ErrResetTokenInvalid() *Problem {
	return NewProblem(http.StatusGone, CodeResetTokenInvalid,
		"Reset link invalid",
		"this password-reset link is unknown or has already been used.").
		WithDocs("https://docs.DOMAIN/auth/reset")
}

// ErrResetTokenExpired is the 410 returned by GET / POST /auth/reset
// when the token has aged past the 15-minute TTL. Same 410 as the
// invalid-token case but distinct code so the dashboard can render
// "link expired, request a new one" vs "link is invalid".
func ErrResetTokenExpired() *Problem {
	return NewProblem(http.StatusGone, CodeResetTokenExpired,
		"Reset link expired",
		"this password-reset link has expired; request a new one.").
		WithDocs("https://docs.DOMAIN/auth/reset")
}
