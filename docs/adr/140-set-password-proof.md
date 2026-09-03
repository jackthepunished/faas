# ADR-140 · Proof of presence for `POST /dashboard/account/set-password`

- **Status:** proposed
- **Date:** 2026-09-03
- **Decision:** The set-password handler chooses its own proof of presence from what the account has — a fresh step-up, the current password, or (for an OAuth-only account with no MFA) the session itself — instead of sitting behind the blanket `requireStepUpHandler(5m)` mount from ADR-077.
- **Why:** The only writer of a step-up stamp is `POST /v1/account/mfa/verify`, so the blanket gate meant an OAuth-only customer without MFA — the customer the route exists for — could never set a password (403 `step_up_required` on every attempt). At the same time, a customer who *had* a password and *had* stepped up could replace it without re-proving anything; the knowledge factor was never consulted.
- **Consequences:** `SetPasswordRequest` gains an optional `current_password`. Two new audit kinds: `account.password_set` (with `proof` ∈ {`step_up`, `current_password`, `session`} and `had_password`) and `account.password_set_denied`. The ADR-077 routes table row for this path is superseded by this ADR. The console gets a "current password" step in a follow-up, ideally with a `has_password` field on `GET /v1/account` so it can decide up front.
- **Rejected alternatives:** (a) keep the blanket step-up and require every customer to enrol MFA before setting a password — makes the OAuth opt-in unusable for the majority; (b) drop the gate and verify nothing — reopens "stolen browser sets a password" for every account; (c) re-run the OAuth consent as the proof for OAuth-only accounts — no server surface for it today, and the consent redirect cannot be completed from a fetch.

## Context

`POST /dashboard/account/set-password` was introduced (ADR-032 PR #2)
as the opt-in for customers who signed up through Google or GitHub
and want email + password as well. ADR-077 later mounted it behind
`sessionAuth → requireStepUpHandler(5m)` alongside account deletion,
closing the "stolen browser, post-MFA-clear" threat.

Two facts make that mount wrong for this particular route:

1. `pkg/session` only ever receives a step-up stamp from
   `cmd/apid/handlers_mfa.go::mfaVerify` (`reissueSessionCookieWithStepUp(…, time.Now())`).
   `sessionAuth` stamps `env.StepUpAt` onto every cookie request, so
   `StepUpFrom` reports `has=true, ts=zero` for any session that has
   not verified TOTP in the last five minutes, and the gate answers
   403 `step_up_required`. A customer with no MFA enrolled has no way
   to obtain a stamp. The route was therefore unreachable for exactly
   the customers it was built for.
2. `postSetPassword` never read the existing `account_passwords` row.
   For a customer who already had a password, a fresh step-up was
   sufficient to replace it — the knowledge factor that `/login`,
   `/v1/account/mfa/disable` (`disableByPassword`) and account
   deletion all re-verify was skipped here.

Neither surfaced in production because the customer console
(`poyrazK/faas-web`) did not route the POST to `apid` at all — the
request hit the SPA fallback and the old panel reported the resulting
`200 index.html` as success. That routing bug is fixed on the console
side; this ADR fixes the server side it exposed.

## Decision

`cmd/apid/handlers_auth_login.go::postSetPassword` calls
`setPasswordProof`, which decides in this order and stops at the first
match:

| Account state | Session state | Outcome | Audit `proof` |
|---|---|---|---|
| any | step-up stamp ≤ 5 min | accept | `step_up` |
| has password | no fresh stamp | require `current_password`; `auth.Verify` against the stored PHC; missing or wrong → 401 `invalid_credentials` | `current_password` |
| no password, MFA enrolled | no fresh stamp | 403 `step_up_required` (same problem and audit row the ADR-077 gate emitted) | — |
| no password, no MFA | any | accept | `session` |

The 5-minute TTL is `setPasswordStepUpTTL`, the same window every
other ADR-077 route uses.

Missing and wrong `current_password` are deliberately the same 401.
The caller is already authenticated as the account, so there is
nothing to enumerate; the shared answer keeps the handler from
becoming an oracle for "does this account have a password" to a
script running in a stolen session. `auth.Verify` is called on the
stored hash in both cases so the two paths cost the same.

The `session` row is the one that accepts without a second factor.
That is a conscious posture, not an omission: an OAuth-only account
with no MFA has no factor the server could re-verify short of
re-running the provider consent, and a session obtained through that
consent is the strongest proof the account can currently offer. It is
the same trust the account already extends to every other
session-authenticated write. The audit row makes it visible; the
follow-up that adds `has_password` to `GET /v1/account` lets the
console explain it.

The mount in `cmd/apid/server.go` drops `requireStepUpHandler`; the
handler emits the identical `auth.step_up_required` audit row on the
MFA-enrolled branch so ADR-077's downstream queries keep working.

## Wire

`SetPasswordRequest` (form-encoded):

```
password          string  required, 12–256 chars
current_password  string  optional; required by the "has password" row
```

Responses: `302` → `/dashboard/account/` on success; `400`
`password_too_weak`; `401` `invalid_credentials` (also the no-session
answer); `403` `step_up_required`.

## Tests

`cmd/apid/set_password_test.go` pins the full matrix: OAuth-only
no-MFA accepts without a stamp; has-password refuses a missing and a
wrong `current_password` (401, stored hash untouched) and accepts the
right one; a fresh stamp stands in for `current_password`; MFA-enrolled
without a password or a stamp gets 403; the length rule still applies
after a valid proof.

## Follow-ups

- `GET /v1/account` → `has_password: bool`, so the console can render
  the "current password" step only when it applies.
- Console (`faas-web`): insert that step ahead of the choose/confirm
  wizard and send `current_password`.
- Consider a "password added to your account" mail on the `session`
  proof path, mirroring the notification most providers send.
