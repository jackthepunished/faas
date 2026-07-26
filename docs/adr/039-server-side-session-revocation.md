# ADR-039 · Server-side session revocation (IAM-3)

- **Status:** accepted
- **Date:** 2026-07-26
- **Decision:** Add a `sessions` table with one row per dashboard login;
  stamp a `sid` (UUID v4) into the cookie envelope. Every authenticated
  dashboard request does one additional primary-key SELECT against
  `sessions`. New routes expose per-session and per-account revocation.
  The existing `pkg/session.Manager` stays stateless; the DB lookup
  lives in the apid cookie branch of `s.auth`. Bearer API keys never
  touch `sessions`.

- **Why:** Issue #187 + #244 merged. The dashboard `faas_sid` cookie is
  a stateless AES-GCM envelope (MfaPending flag added by IAM-2 / PR #318).
  It carries only `{account_id, issued_at, expires_at, mfa_pending}` —
  no server-side row, no per-session identifier. A leaked cookie (XSS,
  shared laptop, Slack paste) is valid for the full 7-day TTL. The only
  mitigation today is rotating the host `FAAS_SESSION_KEY`, which logs
  out every customer of the entire platform at once. There is no "log
  out everywhere", no per-device kill switch, no fraud-response tool.

  Two competing designs existed in the repo:
  - **#187** — epoch column on `accounts` + `epoch_at_mint` envelope
    field. One UPDATE invalidates every session for an account.
  - **#244** — `sessions` table with one row per login, `sid` claim in
    the envelope. Per-session revocation via
    `DELETE /v1/auth/sessions/:id`.

  We chose the **#244 sessions-table** design. Sibling-session
  independence is load-bearing for "log out other devices but not this
  one", and a per-row `revoked_at` gives clean audit semantics. The
  epoch design would force re-login on every device when a customer
  changes their password — that's worse UX than the cookie-staleness
  problem it solves. ADR-035 already accepts the audit-events backbone;
  this ADR documents the chosen design and the rollout semantics.

  **Schedule:** IAM-3 must land **before PR #2 (email + password)**.
  Without IAM-3, "I forgot my password → reset email" has no way to
  invalidate the existing 7-day valid cookies on the customer's other
  devices.

  **Compliance drivers:** SOC 2 CC6.2 (deprovisioning — terminate
  access on a defined event) and CC6.3 (removal of access — least
  privilege + timely revocation). The audit kind `auth.session.stolen`
  (emitted on found-revoked row) is the operator signal that someone
  attempted to replay a previously-revoked session — distinct from
  the missing-row case, which is silent.

- **Wire compatibility:** `json:"sid,omitempty"` on the Envelope
  means pre-IAM-3 cookies still unmarshal cleanly with `Sid == ""`.
  The apid middleware treats empty `Sid` as revoked: clears the
  cookie and returns 401 `CodeSessionExpired` (the load-bearing
  "rollout invalidation" behaviour). Customers re-login on next
  request. This is honest about the gap and forces a known-clean
  state — silent minting would weaken the rollout guarantee.

- **Design notes:**

  - `revoked_at IS NULL` partial index supports
    `ListSessions + active-row portion of RevokeSession/RevokeAllSessions`.
    `GetSession` is a PK lookup.

  - Every revoke SQL includes an `account_id = $2` predicate so IDOR
    is a persistence invariant: cross-account DELETE returns 404
    (`false` from `RevokeSession`), not 403. The handler never lists
    another account's sessions.

  - `last_seen_at` continues to update post-revoke (operational
    signal only, not authorization). Useful for "when did this
    stolen session last attempt to use the API?" forensics.

  - Per-request DB cost: one extra `GetSession` SELECT on every
    authenticated dashboard request. Mitigations: PK lookup on UUID;
    pgx pool; 5-min `last_seen_at` debounce via `sessionTouchDebounce
    sync.Map`. A Redis cache may be evaluated later — out of scope
    here.

  - MFA interaction: `MfaPending` envelope field retained. Allowlist
    covers the new `/v1/auth/*` paths so a customer with
    `mfa_pending=true` can still log out / revoke sessions / list
    devices while completing MFA. MFA verification reuses the existing
    sid — no second row.

  - Revoked-cookie replay: same 401 to the client (don't leak
    existence), but emit `auth.session.stolen` for found-revoked rows
    (distinct from missing-row, which is silent). Operators can pivot
    off the audit kind.

  - MemStore mirrors all semantics: ownership, ordering, post-revoke
    touch, revoke-all current-sid exclusion. Tests pin parity
    explicitly so the in-process tests stay representative of
    production.

- **Schema (migration `00050_sessions.sql`):**

  ```sql
  create table if not exists sessions (
      id           uuid primary key default gen_random_uuid(),
      account_id   uuid not null references accounts(id) on delete cascade,
      issued_ip    inet,
      issued_ua    text,
      issued_at    timestamptz not null default now(),
      last_seen_at timestamptz,
      revoked_at   timestamptz
  );
  create index if not exists sessions_active_account_idx
      on sessions (account_id, issued_at desc)
      where revoked_at is null;
  ```

- **Store interface (six methods, sqlc-generated):**

  - `CreateSession(ctx, id, accountID, issuedIP, issuedUA) (Session, error)`
  - `GetSession(ctx, id) (Session, error)` — `state.ErrNotFound` on miss
  - `RevokeSession(ctx, id, accountID) (bool, error)` —
    `false` = not-found / already-revoked / wrong-account
  - `ListSessions(ctx, accountID) ([]Session, error)` — active only,
    newest first
  - `RevokeAllSessions(ctx, accountID, exceptID) (int, error)`
  - `TouchSessionLastSeen(ctx, id) error`

- **Routes (mounted in apid):**

  - `POST   /v1/auth/logout` — CSRF action `logout`
  - `GET    /v1/auth/sessions` — list active sessions for caller
  - `DELETE /v1/auth/sessions/{id}` — CSRF action `session_revoke`
  - `POST   /v1/auth/sessions/revoke_all` — CSRF action
    `sessions_revoke_all`; caller session stays active

  All four mount behind the standard `auth → requireMFA → requireScope`
  chain. `requireSession` runs inline inside `s.auth`'s cookie branch
  — not as a route wrapper — so each cookie request does exactly
  one `GetSession`.

- **Audit kinds (extension of ADR-035's taxonomy):**

  | Kind | Emitter | Data payload |
  |---|---|---|
  | `auth.session.created` | `issueDashboardSession` | `{sid, method, issued_ip}` |
  | `auth.session.revoke` | `logout`, `revokeSession` | `{sid, reason: "logout"\|"explicit"}` |
  | `auth.sessions.revoke_all` | `revokeAllSessions` | `{revoked_count, retained_sid}` |
  | `auth.session.stolen` | `requireSession` (revoked-row case) | `{sid, method, path}` — never the cookie value |

- **Rollout notes:**

  - All pre-IAM-3 cookies fail closed (treated as revoked). The apid
    middleware clears them and returns 401 `CodeSessionExpired`.
  - Communicate the forced re-login in deploy notes + release
    announcement.
  - Do **not** auto-mint a sid from an old cookie — there is no
    corresponding login row, and silent minting would weaken the
    rollout guarantee.
  - Password reset (PR #2) depends on this landing: when a customer
    clicks "forgot password", the email-verified re-login must
    invalidate the existing 7-day valid cookies on the customer's
    other devices. Without IAM-3, password reset only changes the
    password row, leaving the existing `faas_sid` cookies valid.

- **Out of scope (deferred):**

  - Cross-device session-difference alerts (separate issue).
  - Rolling-window forced re-login (separate issue).
  - WebAuthn-bound sessions (separate issue, deferred by #244 itself).
  - Per-session IP binding (future; today's cookie is already
    AEAD-bound to the envelope).
  - Redis or in-memory session cache (future perf optimization).
  - Epoch / account-wide revocation (rejected).
  - Session revocation for bearer API keys (out of scope — keys are
    scoped + bearer, not cookie).
