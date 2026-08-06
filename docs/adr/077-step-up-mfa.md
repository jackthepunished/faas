# ADR-077 · 5-minute step-up MFA for sensitive operations

- **Status:** accepted
- **Date:** 2026-08-05
- **Issue:** IAM-hardening mega-PR logical change 6
- **Supersedes:** none

## Decision

Gate eight sensitive operations behind a fresh TOTP step-up on
the cookie envelope. The step-up stamp persists on the envelope
as `Envelope.StepUpAt time.Time` (`omitempty`); the new
middleware `pkg/auth.Middleware.RequireStepUp(ttl)` reads it off
`r.Context()` and rejects when the stamp is missing, zero, or
older than the configured TTL. Default TTL is **5 minutes**
(user-confirmed; range is industry-comparable — GitHub sudo-mode
5m, AWS console 15m, GCP IAM 10m — and matches Phase-2's
shortest option so a single confirmation-click latency fits
inside the window).

The reissue seam is the single
`/v1/account/mfa/verify` handler, which now stamps
`time.Now()` into `step_up_at` on every successful TOTP verify
and re-seals the envelope. Other reissue paths (enrollment
confirm, recovery, disable) deliberately leave `step_up_at`
cleared — they're not the step-up gate themselves.

## Routes

Sensitive-op routes mounted in `cmd/apid/server.go:642,650-657,
834-857, 1077-1133`:

| Route | Pre-existing chain | Rewired chain |
|---|---|---|
| `POST /v1/keys/{id}/rotate` | `authLimited → requireMFA → requireScope(Admin) → rotateKey` | `… → requireScope(Admin) → requireStepUp(5m) → rotateKey` |
| `PATCH /v1/account/plan` | `authLimited → requireMFA → requireScope(Admin) → changePlan` | `… → requireScope(Admin) → requireStepUp(5m) → changePlan` |
| `DELETE /v1/account` | `auth → requireScope(Admin) → deleteAccount` (no `requireMFA`) | `auth → requireMFA → requireScope(Admin) → requireStepUp(5m) → deleteAccount` (the missing `requireMFA` from the Phase 1 audit is closed in the same commit) |
| `POST /v1/orgs/{slug}/transfer_ownership` | `authLimited → requireMFA → requireScope(Deploy) → loadOrg → transferOrgOwnership` | `… → requireScope(Deploy) → loadOrg → requireStepUp(5m) → transferOrgOwnership` |
| `POST /v1/orgs/{slug}/keys` | `authLimited → loadOrg → createOrgAPIKey` (no `requireMFA`) | `authLimited → requireMFA → loadOrg → requireStepUp(5m) → createOrgAPIKey` |
| `POST /v1/orgs/{slug}/keys/{id}/rotate` | `authLimited → loadOrg → rotateOrgAPIKey` (no `requireMFA`) | `authLimited → requireMFA → loadOrg → requireStepUp(5m) → rotateOrgAPIKey` |
| `POST /dashboard/account/set-password` | `dashboardChain → sessionAuth → postSetPassword` | `dashboardChain → sessionAuth → requireStepUpHandler(5m) → postSetPassword` |
| `POST /dashboard/account/delete` | `dashboardChain → sessionAuth → dashboardDelete` | `dashboardChain → sessionAuth → requireStepUpHandler(5m) → dashboardDelete` |

The `POST /v1/orgs/{slug}/keys` and `…/keys/{id}/rotate` route
re-wirings also close the **missing `requireMFA`** regression
the Phase 1 audit flagged as the highest-blast-radius gap (a
session-cookie principal with no MFA enrolment could mint an
org API key from a stolen browser).

## Files

- **New**: this ADR (`docs/adr/077-step-up-mfa.md`)
- **Modify**: `pkg/session/manager.go` — `Envelope.StepUpAt`
  (`omitempty`); `IssueWithSessionAndBindingHashAndStepUp`
  helper (single AEAD round for the union of sid + binding +
  step_up)
- **Modify**: `pkg/auth/middleware/context.go` — `stepUpCtxKey`,
  `WithStepUp(ctx, ts)`, `StepUpFrom(r) (time.Time, bool)`
- **Modify**: `pkg/auth/middleware/middleware.go` — `RequireStepUp(ttl)`
  (AccountHandler-shaped) + `RequireStepUpHandler(ttl)`
  (http.Handler-shaped twin for dashboard routes). `RequireSession`
  cookie branch stamps `env.StepUpAt` onto `r.Context()` via
  `WithStepUp`.
- **Modify**: `cmd/apid/auth_facade.go` — `requireStepUp(ttl)` and
  `requireStepUpHandler(ttl)` facades mirroring `requireMFA`
- **Modify**: `cmd/apid/server.go` — 8 route re-wirings (6 in
  `handler()`, 2 in the dashboard mount block)
- **Modify**: `cmd/apid/handlers_mfa.go` — `reissueSessionCookie`
  becomes a thin wrapper around
  `reissueSessionCookieWithStepUp`; the mfaVerify success branch
  passes `time.Now()` to refresh the stamp and emits the new
  `auth.step_up_verified` audit row
- **Modify**: `docs/adr/035-auth-audit-events.md` — gain the
  `auth.step_up_required` + `auth.step_up_verified` kinds

## Why

Today's `RequireMFA` only guards the *enrollment* boundary — it
lets a customer who cleared MFA at session-mint time freely
rotate their API key 12 hours later. The threat model the user
called out (stolen browser, post-MFA-clear replay) lands squarely
on that gap: once the cookie's `mfa_pending` flag is cleared the
attacker inherits the step-up-equivalent trust for the rest of
the session lifetime (7 days by default). For the eight routes
above this is too much standing trust.

A 5-minute freshness window forces the attacker to either
brute-force TOTP at the moment of the action (subject to the
existing `apiAuthLimiter`) or wait for the customer to drive the
re-auth themselves — same trade-off the rest of the industry
makes for the same reasons.

The compose-order argument: a step-up stamp on a session whose
MFA is unenrolled is meaningless. `RequireMFA → RequireScope →
RequireStepUp` chains guarantee the customer is at minimum
MFA-cleared; the `RequireStepUp` is the *additional* proof that
they cleared it recently. The two gates trip at distinct events
(`auth.mfa_gate_hit` vs `auth.step_up_required`) so an operator
can tell which one is the friction point.

## Missing-`requireMFA` on 3 org-key routes

The Phase 1 audit flagged that `POST /v1/orgs/{slug}/keys`
(server.go:836) had no `requireMFA` at all — a session-cookie
principal could mint an org API key from a browser that hadn't
cleared the MFA-pending gate. This mega-PR closes that gap in
the same commit (the three org-key routes the audit named all
land the missing `requireMFA` + the new step-up gate). A
revert of this PR re-opens the missing-`requireMFA` hole, so
the missing-`requireMFA` fix and the step-up gate are
intentionally locked into the same commit.

## Bypass tolerance

The middleware short-circuits when `StepUpFrom(r)` returns `has
== false`. The conditions under which `has == false`:

1. **Bearer-key principal.** The RequireSession bearer branch
   doesn't stamp `step_up_at` — an API key is itself step-up-
   equivalent proof. The threat model (stolen browser) doesn't
   apply to a stolen key because the key never lived in the
   browser.
2. **Pre-PR-077 cookie.** `Envelope.StepUpAt` is `omitempty`,
   so a cookie issued before the merge decodes with `StepUpAt
   == time.Time{}`. The middleware treats this as "missing"
   and passes the request through. This is the documented
   rolling-migration behaviour: the gate can't trip on a
   cookie that pre-dates it. Within one TOTP rotation cycle
   (the natural re-auth cadence of an active customer) every
   cookie carries a fresh stamp and the bypass closes.

The bypass tolerance is the only place the gate is soft. The
auth audit row classifies the branch (`reason: "missing"`) so
operators can identify legacy cookies in production.

## Audit kinds

- `auth.step_up_required` — `{path, method, reason: "missing"
  \| "expired", ttl_sec}` — fired by RequireStepUp on every
  blocked request. Distinct from `auth.mfa_gate_hit` (the
  enrollment gate) so a downstream query can split the two.
- `auth.step_up_verified` — `{path, method, ttl_sec}` — fired
  by `mfaVerify` on every successful TOTP step-up. The
  paired-counter shape lets operators answer "is the gate
  tripping customers?" alongside "are customers successfully
  re-auth?" without joining.

## Verification

- `make test`, `make lint`
- **Manual smoke on a reference control-plane node**: full step-up sweep — rotate key,
  change plan, delete account (and cancel via `/account/restore`),
  transfer org ownership, mint + rotate org key, set-password,
  dashboard delete. Each 403s without fresh TOTP, then succeeds
  with one. Confirm `/v1/audit-events?kind_prefix=auth.step_up`
  shows both `required` and `verified` rows during the sweep.
