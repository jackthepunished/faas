# ADR-076 · Session binding-hash auto-revoke

- **Status:** accepted
- **Date:** 2026-08-05
- **Issue:** IAM-hardening mega-PR logical change 5
- **Supersedes:** none

## Decision

Stamp every dashboard session at mint time with an HMAC-SHA256
fingerprint of (client IP, UA-family). Seal the fingerprint into
the cookie envelope (additive `binding_hash` field, `omitempty`)
and persist it on the `sessions` row in a new
`sessions.binding_hash` column. Require the apid auth middleware
to compare the two on every authenticated request; mismatch ⇒
auto-revoke + audit + 401.

```
HMAC-SHA256(session_key[:32], ip || "\x00" || ua_family)
   ├── bound to the host's session-key secret so a leaked DB
   │   blob can't pre-compute fingerprints offline
   ├── 64 hex chars, truncated to 8 in the audit row for readability
   └── one-shot per session; refreshed on every /v1/account/mfa/*
       step-up so a customer's "natural" re-auth keeps the
       fingerprint in sync with where they are
```

The UA classifier buckets into 8 families
(`{chrome, firefox, safari, edge, curl, wget, cli, unknown}`)
— substring match on the User-Agent header. Bucket size matters
for clarity in operator triage, not for collision resistance
(the fingerprint is per-sid, never reused across accounts).

## Files

- **New**: `pkg/bindinghash/bindinghash.go` + `_test.go`
- **New**: `migrations/00140_sessions_binding.sql` + `_test.go`
- **New**: this ADR
- **Modify**: `pkg/session/manager.go` — Envelope gains `BindingHash string`;
  two `Issue*` helpers (`IssueWithSessionAndBindingHash`,
  `IssueWithSessionAndGithubLoginAndBindingHash`); `Manager.BindingKey()`
  exposes the AEAD key bytes for HMAC use
- **Modify**: `pkg/auth/middleware/middleware.go` — Middleware gains
  `BindingKeyFn func() []byte`; `SessionLookup` gains
  `RevokeSession`; `RequireSessionCookie` gains step 3.5
  (binding-mismatch ⇒ RevokeSession + audit + 401); `prefix8`
  helper at end of file
- **Modify**: `pkg/state/store.go`, `pkg/state/pgstore.go`,
  `pkg/state/memstore.go` — `Session.BindingHash`,
  `CreateSessionWithBinding`, `sessionSelectCols` extended,
  `scanSession` reads the new column
- **Modify**: `cmd/apid/{server,issue_session,handlers_mfa}.go` —
  wire `s.bindingKeyFn` from `*session.Manager.BindingKey()`;
  `issueDashboardSession{,WithGithub}` stamps the binding at
  mint; `reissueSessionCookie` re-stamps on every MFA step-up
- **Modify**: `cmd/apid/auth_adapters.go` — `storeAuthAdapter`
  already embeds `state.Store` so `RevokeSession` is inherited;
  `cmd/gatewayd-internal/auth_adapters.go` adds an explicit
  `RevokeSession` method on `sessionLookupAdapter`
- **Modify**: `cmd/gatewayd-internal/run.go` — pass `nil`
  `BindingKeyFn` (the public listener does its own check at the
  edge; the internal routing daemon sees the unbound cookie)
- **Modify**: `docs/adr/035-auth-audit-events.md` — add the
  `auth.session.binding_mismatch` kind

## Why

Today the only auto-revoke the dashboard recognises is the
**explicit** revoke path (`RevokeSession` after a customer hits
"sign out everywhere", or after an admin disables the account).
A stolen `faas_sid` cookie is indistinguishable from a
legitimate cookie to the middleware: the AEAD bind protects
against forgery but says nothing about whether the cookie is
sitting in the right browser. The fingerprint cross-check is
the missing layer.

The IP+UA-family fingerprint is the smallest signal that's
still useful. A naive per-IP exact-match would misfire every
time a phone's cell flips tower; per-IP+per-UA would misfire
every Chrome minor-update. Per-IP+UA-family tolerates both
modes of drift (same browser, IP flip; same IP, browser update)
and only triggers when an attacker carries the cookie to a
*different* fingerprint.

## Why HMAC-SHA256 and not bare SHA-256

Bare SHA-256 of (ip, ua_family) on a leaked PG blob is
rainbow-reversible for any common IP family combination.
HMAC-SHA256 keyed by the host's session-key secret requires
either the secret or an offline brute-force of 32 bytes — the
latter is the same threat as the AEAD itself, so no new attack
surface is created. The audit preamble for `pkg/auth.HashEmail`
established the same precedent for the email-hashing path; this
decision mirrors it.

The HMAC uses the same 32-byte key the AEAD consumes, captured
at `NewManager` time into a parallel `bindingKey` field on
`Manager`. This avoids doubling the file-load dance at boot
(`/etc/faas/secrets/session.key`) and pins the rotation contract
to the existing key-rotation runbook.

## Why "binding not armed" on empty inputs

The empty-input branches (no client IP, no UA, no keyFn) return
`""` from `bindinghash.Compute` and the envelope omits the field
(`omitempty`). The middleware cross-check is then a no-op for
that cookie. This is the documented pre-PR-076 + unix-socket +
CLI-auth code-path tolerance: the storage column is NULL and the
envelope field is absent, so neither side ever sees an empty
match fail.

The trade-off is "no fingerprint ⇒ no auto-revoke" for those
code paths. The mitigation is that none of those code paths
expose the cookie to the public dashboard in the first place:
the unix-socket gatewayd-internal listener terminates the request
itside the box, and the CLI-auth code path exchanges a one-shot
token that never carries a cookie. The threat surface is the
*public* dashboard, which always lands at apid with a real IP
and a real UA.

## Why per-host key reuse (not a separate secret)

Reusing the session-AEAD key means one file to provision, one
file to rotate, one failure mode to script. The HMAC threat
model (offline pre-computation against a leaked PG blob) is the
same as the AEAD threat model (forge the envelope against a
leaked AEAD key); the operator rotation semantics line up.
A separate secret would double the boot-time provisioning cost
and create two failure modes that need to stay in lockstep.

## Audit kind

`auth.session.binding_mismatch`:

```jsonc
{
  "sid":              "<uuid>",
  "method":           "GET|POST|...",
  "path":             "/v1/...",
  "expected_prefix":  "<first 8 hex chars of sessions.row>",
  "presented_prefix": "<first 8 hex chars of envelope>"
}
```

8 chars hex = 32 bits. Enough for an operator to disambiguate
("presented `7a2f…` but stored `b81c…` → likely a UA-family
flip on the customer side") without leaking the HMAC keys.
Distinct from `auth.session.stolen`, which fires when the row
was *already* revoked at lookup time — the binding-mismatch
row fires *because of* the auto-revoke, not *before* it.

## Wire compatibility

Both sides are additive:

- Envelope field `binding_hash` is `omitempty`; a pre-PR-076
  cookie decodes with `BindingHash == ""` (the cross-check
  skips).
- Sessions column is nullable; a pre-PR-076 row has NULL (the
  cross-check skips).
- The `IssueWithSession` 4-arg helper is preserved for callers
  that don't bind a fingerprint; a new `IssueWithSessionAndBindingHash`
  helper is added for the binding code path. No existing caller
  changes signature.

## Replay-revoked behaviour

The auto-revoke path goes through `SessionLookup.RevokeSession`,
which already stamps `revoked_at` and supports the
"already-revoked" idempotent path. A second stolen-cookie
replay after the first auto-revoke fires the existing
`auth.session.stolen` audit branch (the row is revoked at
lookup time), not the `auth.session.binding_mismatch` branch.
The two are deliberately separate: the auto-revoke row is the
*successful* defence ("we detected the drift and revoked the
sid"); the stolen row is the *late* defence ("a thief tried
the same cookie after we revoked it").

## Verification

- `make test` + `make lint`
- Manual smoke (EX44): `/login` from one (IP, UA-family),
  replay from a different UA → expect 401 + a new
  `auth.session.binding_mismatch` audit row in
  `GET /v1/audit-events`. Cross-check that
  `sessions.revoked_at` is non-null on the row that fired.
