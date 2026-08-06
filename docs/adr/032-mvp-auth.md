# ADR-032 · MVP auth: harden /login against #165 + real sign-in methods

- **Status:** accepted
- **Date:** 2026-07-24
- **Decision:** Replace the magic-link placeholder login with a hardened
  sign-in path (PR #1) and the full real auth surface (PR #2 — email +
  password (Argon2id), Google OAuth with `email_verified`, GitHub OAuth
  login). The PR #1 commit closes issue #165 with the smallest possible
  change; PR #2 lands the password table, OAuth hardening, and the
  end-of-life of the X-Dashboard-Key fallback.
- **Why:** `POST /login` (cmd/apid/handlers_auth.go:74-136, pre-PR #1)
  auto-created an account for any email, minted a `web-console` API key,
  returned the key in the response body, and set a 7-day session cookie
  — with zero verification. A single `curl -d '{"email":"victim"}' /login`
  was a full pre-auth account-takeover (spec §11 violation). The fix
  must (i) close the takeover with a minimal PR for review urgency,
  and (ii) replace the placeholder with a real auth surface in a
  follow-up so we don't ship a "we'll fix it later" intermediate.
- **Consequences:**
  - PR #1 ships a hardened login path that requires both an `email`
    form field AND a valid `X-Dashboard-Key` header (a pre-existing
    "web-console" API key from the buggy pre-#165 deploy). The
    response body never carries an `api_key` field. Unknown email +
    no key → 401 `invalid_credentials`. Email/key mismatch → 401
    `invalid_credentials`. The two failure modes collapse to the
    same body so an attacker cannot probe for valid emails.
  - Six new RFC 7807 stable codes land in `pkg/api/errors.go`:
    `invalid_credentials`, `email_not_verified`, `password_too_weak`,
    `reset_token_invalid`, `reset_token_expired`, `account_exists`.
    Both `invalid_credentials` and `email_not_verified` map to 401;
    the dashboard form renders a single "sign-in failed" copy for
    both so the surface does not leak which case fired.
  - The dashboard now needs a real auth surface. PR #2 lands:
    - Migration `00029_oauth_links` (PK on `(provider, sub)` — the
      §11 anti-takeover invariant: one OAuth subject maps to one
      account, period).
    - Migration `00030_account_passwords` (one row per account;
      OAuth-only accounts have no row).
    - New `pkg/auth/password.go` (Argon2id, PHC string format so
      future parameter bumps don't break old hashes).
    - New `cmd/apid/handlers_auth_login.go` with full email +
      password (Argon2id), `POST /signup`, `POST /login/forgot`,
      `GET /auth/reset`, `POST /auth/reset`,
      `POST /dashboard/account/set-password`.
    - `cmd/apid/handlers_google.go` gates on `email_verified=true`
      and resolves accounts by OAuth subject FIRST (sub-first
      lookup), with email as a fallback for the legacy "account
      created pre-OAuth" case.
    - New `cmd/apid/handlers_github.go` for the GitHub login
      (`/v1/auth/github` sibling of `/v1/auth/google`; the existing
      `/oauth/callback` install-bind stays for already-signed-in
      users).
    - Daily cleanup goroutine in `cmd/apid/main.go` for
      `login_tokens` — the `DeleteOldLoginTokens` Store primitive
      has existed since M7.5 but had no production caller; the
      password-reset flow is the first to use it.
  - PR #3 (polish) lands the "Set a password" dashboard link,
    structured log events for operator observability
    (`event=login_session_issued`, etc.), and the one-off sweep
    of legacy `api_keys where label='web-console'` rows.
- **Rejected alternatives:**
  - *Magic-link only (status quo, hardened):* rejected — the bug
    is in the magic-link dispatcher; rebuilding on it doesn't fix
    the email-verification gap.
  - *Sliding session refresh:* rejected — for an MVP the 7-day
    fixed window matches the CLI's 7-day key TTL and is simpler
    to reason about. A future "remember me" toggle can extend
    the window.
  - *SendGrid-only mail transport:* rejected — keeps the existing
    `FAAS_MAIL_TRANSPORT` env-var seam.
  - *GitHub-only OAuth:* rejected — Google is the largest
    single-provider cohort; cutting it loses too many signups.
  - *bcrypt instead of Argon2id:* rejected — Argon2id is the
    OWASP recommendation for new systems, and the marginal CPU
    cost (one verify per login) is negligible.
  - *PR #1 also blocking on email_verified:* rejected — PR #1
    must close #165 with the minimum surface change; the OAuth
    hardening lands in PR #2 alongside the password table that
    the verification depends on.
  - *Constant-time pad via a single dummy Argon2id hash in
    PR #1:* deferred — `account_passwords` does not exist yet
    in PR #1, so the Argon2id pad has no real branch to equalise.
    The PR #1 path simply does not perform a CPU-heavy operation
    on the no-account branch, which closes the most important
    attack (no account creation, no key mint). PR #2 ships the
    Argon2id pad when the password table lands.

## PR #2 follow-up (issue #165, landed 2026-07-24)

PR #2 lands the items above (schema, pkg/auth, handlers, server
wiring, cleanup goroutine, templates, OpenAPI). This section
records the decisions that landed in PR #2 — they are not
re-decisions of the PR #1 surface, but details that did not need
to be locked in until the password table and the OAuth hardening
were concrete.

### Why X-Dashboard-Key was removed only in PR #2, not PR #1

PR #1 needed to close the takeover with the smallest possible
diff. The customers the buggy handler created before #165 still
held a `web-console` API key from that deploy, and those keys are
the only out-of-band credential they have. Removing the header in
PR #1 would have locked those customers out before the password
migration was available; PR #2's sweep gate (the
`delete from api_keys where label='web-console'` operator
sweep documented below) clears the table only after the password
path is in production.

### The `oauth_links` PK rationale

The composite primary key on `(provider, provider_subject)` is
the §11 anti-takeover invariant in database form. The invariant
is "one OAuth subject binds to one account, period" — without it,
an attacker who creates a password account on `victim@example.com`
first can claim the row when the victim signs in with Google on
the same email (the pre-#165 OAuth flow took the email as the
binding key). The PK plus the
`WHERE oauth_links.account_id = excluded.account_id` clause on
the upsert makes the second case return `ErrConflict` rather
than overwrite.

### The `login_tokens` reuse decision (vs. a new `password_reset_tokens` table)

`login_tokens` has been a Store primitive since M7.5
(`IssueLoginToken`, `ConsumeLoginToken`, `DeleteOldLoginTokens`).
PR #1 kept the primitives alive after the magic-link was retired
because the reuse is the cheaper path: a dedicated
`password_reset_tokens` table is more code without adding
security (the row schema is identical — a one-shot token with
an expiry). The dedicated table is a single migration rename if
a future audit demands it.

### The 24h cleanup tick

`pkg/logintoken` runs `DeleteOldLoginTokens` every 24h with a
first-pass-at-startup so a daemon restart catches up. The 24h
cadence is gross-overkill for the 15-min expiry: a tighter
cadence would burn CPU on what is, in practice, an empty
deletion. The 24h tick keeps the `login_tokens` table bounded
by (rate of reset requests) × 15min.

### The Argon2id constant-time pad

Every login attempt runs ONE Argon2id verify under identical
parameters (`m=65536,t=1,p=2`). The no-account path runs the
verify against `pkg/auth.DummyPHC` (a real Argon2id hash of a
known placeholder string). The cost is identical between
"unbound email", "wrong password", and "no password row" — the
three failure modes cannot be distinguished by request timing.
The 64MiB × 1 verify is ~50ms on the reference node; under the §11
10/min/IP bucket the worst case is 10 × 50ms = 500ms/sec/core
of CPU, well under the 6 GB control-plane slice.

### The `GITHUB_CLIENT_ID` / `GITHUB_CLIENT_SECRET` env-var addition

GitHub OAuth is the new dashboard login surface (PR #2). The
callback fetches `/user` and `/user/emails` (never trusting the
profile's `email` field directly — that field is null when the
customer has no public email), filters
`primary=true && verified=true`, and rejects otherwise with 401
`email_not_verified`. The `GITHUB_CLIENT_ID` + `GITHUB_CLIENT_SECRET`
env vars are required; the optional `GITHUB_REDIRECT_URI` overrides
the default-request-hosts-derived URL.

### The sweep gate (operator doc)

Before X-Dashboard-Key was removed at the end of PR #2, the
operator runs the following in a one-off SQL session:

```sql
delete from api_keys where label = 'web-console' returning id, account_id;
```

The shape (selective pre-#165 deploys only) means the row count
is bounded by the customer count from the buggy deploy window.
The handler code in `cmd/apid/handlers_auth.go::postLogin` was
removed in the same PR — the only remaining code path that
references the header is the removed function itself. `go test
./cmd/apid/...` confirms no in-process caller.

### Anti-enumeration closure

The /login response is identical between "no such email", "wrong
password", and "no password row" (401 `invalid_credentials`).
The /signup response is identical between "new account" and
"existing account with the same password" (200 + session cookie).
The /signup response is 401 (NOT 409) on "existing account with
a different password" — the surface does not leak account presence.
The /login/forgot response is identical between "email is bound"
and "email is unbound" (200) — the reset URL is mailed only on
the bound branch, but the response never reveals that.
