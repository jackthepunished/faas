# ADR-035 · Auth audit log surface

- **Status:** accepted
- **Date:** 2026-07-25
- **Decision:** Extend the existing `events` table (spec §5) to receive
  auth-relevant action rows from `apid` handlers via a thin `auditor.Emit`
  seam. Expose the result to customers through `GET /v1/audit-events` and
  fold security events into the existing GDPR export bundle. Audit writes
  are best-effort: a failed insert logs Warn and increments
  `apid_audit_write_failures_total` but never rolls back the auth action.

- **Why:** Issue #188. Today every auth-relevant action (login, key mint,
  secret change, plan change, OAuth callback, account delete/restore)
  lands only as `slog.Info(...)` lines that operators can grep Loki but
  customers cannot query and SOC2 auditors cannot produce a timeline
  for. The events table is already provisioned (it backs schedd's
  state-transition audit at `pkg/sched/engine.go:1324`) and already has
  the right shape (`id, at, actor, kind, subject, data`); only the
  customer-facing reader and the apid-side writer are missing.

  This ADR is the **observability backbone for IAM-2 (TOTP MFA),
  IAM-3 (server-side session revocation), and IAM-5 (key expiry +
  rotation)** — every later IAM feature writes a security event into
  the same queryable timeline. Without it, the operators flying blind
  today stay blind after those land.

- **Kind taxonomy (new values; all share the `subject = account_id`
  convention except where noted):**

  | Kind | Emitter | Data payload |
  |---|---|---|
  | `auth.login` | `postLogin`, `verify`, `handleGoogleOAuthCallback`, `exchangeCliAuthCode`, `postCliAuthPage` | `{method: "password"\|"magic_link"\|"google"\|"cli_code", auto_created?: bool, email?: string}` |
  | `auth.logout` | `logout` | `{}` |
  | `key.created` | `createKey`, `exchangeCliAuthCode` mint path | `{key_id, scopes}` |
  | `key.deleted` | `deleteKey` (REST + dashboard) | `{key_id}` |
  | `secret.set` | `setSecret` | `{app_id, name}` — never the plaintext |
  | `secret.deleted` | `deleteSecret` | `{app_id, name}` |
  | `account.plan_changed` | `changePlan` (success branch) | `{from, to}` |
  | `account.deletion_scheduled` | `scheduleDeletion` (REST + dashboard) | `{via: "rest"\|"dashboard"}` |
  | `account.deletion_restored` | `cancelDeletion` (REST + dashboard) | `{via: ...}` |
| `account.mfa_required_enabled` | `flipMFARequiredIfUnenrolled` (first-arm branch, IAM-2 chokepoints `plan_upgrade` / `card_attached` / `second_deploy`) | `{reason, ...extra}` — first time the row transitions to `mfa_required=true`. |
| `account.mfa_required_armed_again` | `flipMFARequiredIfUnenrolled` (unchanged branch, IAM hardening mega-PR) | `{reason, ...extra}` — chokepoint fired on an account whose `mfa_required` was already true (webhook redelivery, second chokepoint hit in the same session). Distinct from `enabled` so a downstream query can answer "did this account ever re-trip a chokepoint?" without a join. Closes the silent-re-arm audit gap. |
| `auth.session.binding_mismatch` | `pkg/auth/middleware/middleware.go` RequireSessionCookie step 3.5 (IAM hardening mega-PR / ADR-076) | `{sid, method, path, expected_prefix (8 chars), presented_prefix (8 chars)}` — the cookie envelope's `binding_hash` disagreed with the sessions row's `binding_hash`; auto-revoke + 401. Both prefixes are first-8-hex of the HMAC-SHA256 fingerprints (32 bits each), enough for an operator to disambiguate the kind of drift (`presented 7a2f… but stored b81c…`) without leaking the HMAC keys. Distinct from `auth.session.stolen` (which fires when the row is already revoked at lookup time). |
| `auth.step_up_required` | `pkg/auth/middleware/middleware.go` RequireStepUp / RequireStepUpHandler (IAM hardening mega-PR / ADR-077) | `{path, method, reason: "missing"\|"expired", ttl_sec}` — the customer tried a step-up-gated route with a stale (or absent) `step_up_at` cookie stamp. Bearer-key principals don't trip this kind. Distinct from `auth.mfa_gate_hit` (which fires when MFA isn't enrolled at all). |
| `auth.step_up_verified` | `cmd/apid/handlers_mfa.go` `mfaVerify` (IAM hardening mega-PR / ADR-077) | `{path, method, ttl_sec}` — a step-up stamp was refreshed on a successful `/v1/account/mfa/verify` TOTP check. The counter-paired to `auth.step_up_required` so an operator can answer "how often does the gate succeed vs. block?". |
  | `stateless.advisory` | `cmd/apid/advisory_receiver.go` (vmmd → apid gRPC forward) | `{instance, app_id, count, events: [{path, mask, pid, ts_unix_ms}, ...]}` — Wave 0 PR-C / ADR-047 |
  | `app.security_updated` | `patchAppSecurity` (issue #472 / ADR-058) | `{app_id, slug, old_require, new_require}` — admin toggled `apps.require_signed`. Distinct from generic `app.updated` so the audit-log panel can filter signature-related config changes. |
  | `app.trusted_signer_added` | `upsertTrustedSigner` (issue #472 / ADR-058) | `{app_id, slug, signer_name}` — admin onboarded a cosign trusted publisher. The PEM bytes are never logged (operator-side mirror at `/etc/faas/secrets/trusted-publishers/<name>.pem` is the canonical store). |
  | `app.trusted_signer_removed` | `deleteTrustedSigner` (issue #472 / ADR-058) | `{app_id, slug, signer_name}` — admin offboarded a publisher. |
  | `app.signed_image_accepted` | imaged `verifyImageSignature` (issue #472 / ADR-058) | `{app_id, slug, deployment_id, signer_name, digest}` — deploy passed signature check. Emitted via `pg_notify('audit_event')` from imaged so apid is the single-source writer to the events table. |
  | `app.signature_missing` | imaged `verifyImageSignature` (issue #472 / ADR-058) | `{app_id, slug, deployment_id, ref, digest}` — registry had no `.sig` blob. Fail-closed: deployment marked FAILED with `failure_reason="signature_missing"`. |
  | `app.signature_invalid` | imaged `verifyImageSignature` (issue #472 / ADR-058) | `{app_id, slug, deployment_id, ref, digest}` — sig exists but no trusted publisher matched. Fail-closed: deployment marked FAILED with `failure_reason="signature_invalid"`. |

  All `auth.*`, `key.*`, `secret.*`, `account.*`, and `stateless.*` values are namespaced
  with a dot prefix; schedd's existing kinds (`state_transition`,
  `wake_boot_error`, `park_snapshot_error`, `watchdog_timeout`) are
  bare names. Grep verifies no overlap.

- **Consequences:**

  - **Zero new migrations.** `events_subject_idx (subject, at desc)` was
    shipped in `migrations/00002_app_manifest_and_domains.sql:46-56` for
    schedd; it covers the IAM-4 list query unchanged.
  - New `cmd/apid/audit.go` (the `auditor` struct) holds the seam.
    `Emit(ctx, kind, accountID *string, data map[string]any)` is the
    only public method. The pointer-not-required shape lets cron-fired
    system events land without an account scope; the handler call sites
    always pass `&acct.ID`.
  - `apid` middleware composes as `auth → requireScope → handler →
    audit.Emit` — same shape as `auth → requireScope → handler`. The
    audit call sits **after** the underlying store write returns
    success, before the HTTP response is written. A 4xx handler path
    does **not** emit (only successful actions are auditable in v1).
  - New `pkg/wire.OpsMetrics.AuditWriteFailures()` counter named
    `apid_audit_write_failures_total`. Mirrors the schedd
    `EventsWriteFailures` precedent at `pkg/sched/engine.go:1326-1328`.
  - New `GET /v1/audit-events?since=&kind_prefix=&limit=` and
    `GET /v1/audit-events/{id}` customer-facing routes, gated by
    `requireScope(GET, ScopeAdmin, MethodDefaultScope(GET))` — same
    gating as `GET /v1/keys`. Session cookie or any read/write key
    works; admin-only routes (`/v1/compute-nodes`) remain admin-only
    via the separate `adminAllows` email gate.
  - `gatherExport` in `cmd/apid/handlers_account.go` unions the
    existing `gdpr_requests` rows with the customer's
    `auth.*`/`key.*`/`account.*`/`secret.*` events rows, sorted by
    timestamp descending. The customer sees one ordered timeline that
    combines GDPR actions and security events. Existing GDPR consumers
    ignore the new optional `Kind`/`Data` fields.
  - OpenAPI spec gains `/v1/audit-events` + `/v1/audit-events/{id}`
    paths and `AuditEventResponse` + `ListAuditEventsResponse` schemas.
    `make spec-sync` mirrors into `pkg/apid/openapi.yaml`. `make
    spec-check` enforces parity.
  - No `actor` enum. Keep `actor="apid"` text constant in `audit.go`;
    the column is free-form `text` and the API surface does not need
    a per-actor enum yet.

- **Failure semantics (load-bearing):**

  `auditor.Emit` mirrors `pkg/sched/engine.go:1317-1329`: an
  `AppendEvent` failure logs `Warn`, increments
  `apid_audit_write_failures_total`, and returns. The customer-facing
  handler has already returned 200 by the time the audit write
  happens; the audit row is observation, not source of truth. The
  reverse — rolling back the auth action because the audit failed —
  is explicitly rejected. A `failingEventStore` test (mirror of
  `pkg/sched/events_test.go:99-118`) proves this in CI.

  **Idempotency** — the audit emit is **not** idempotent at the row
  level: a replayed request mints a fresh `id`, a fresh `at`, and a
  fresh row. The customer-facing handler layer is responsible for
  being idempotent (CLI auth exchange uses
  `ConsumeCliAuthCode` which is a CAS, so the second call returns
  410 `cli_auth_code_unavailable` and never reaches `Emit`; key
  deletion uses the underlying `DeleteAPIKey` idempotency; account
  deletion is naturally idempotent because `deleted_pending` +
  `scheduleDeletion` is a no-op). A customer reading the audit log
  therefore sees exactly one row per **successful** underlying
  action, regardless of HTTP-level retries. This is the correct
  behavior — auditors want one row per "thing that happened", not
  one row per "request that ran".

- **Customer-scoping (load-bearing for tenant isolation, spec §11):**

  Every read endpoint in this surface is **subject-pinned to the
  caller's `account_id`**. The list handler reads via
  `store.ListEvents(r.Context(), acct.ID, ...)` — the events table's
  primary subject index is `(subject, at desc)`, so the planner
  walks only the caller's slice. The single-row handler re-uses
  the same `ListEvents(acct.ID, ...)` call and filters by id in
  Go, so a customer who guesses another account's row id gets a
  404 — byte-for-byte identical to an unknown-id 404. There is no
  admin-operator override; a SOC2 auditor wanting the platform-wide
  audit log reads `events` directly against the database, not
  through this API. This is a deliberate constraint: spec §11
  forbids any tenant-visible surface from leaking cross-tenant
  data, and the GDPR export bundle is the only "give me everything
  I own" answer.

- **GDPR interaction:**

  - `GdprAuditExportResponse` gains optional `Kind` and `Data` fields
    (`omitempty`). Existing GDPR consumers ignore them.
  - `listEventsForAccountExport(ctx, accountID)` (sibling of
    `listGdprRequestsForAccountExport`) calls
    `store.ListEvents(ctx, accountID, 1000)` and maps each row into a
    `GdprAuditExportResponse` with `Kind`/`Data` populated and
    `Action` empty.
  - `gatherExport` interleaves the two lists by timestamp descending.
    The combined `audit_trail` is the canonical SOC2 evidence
    artifact.

- **Out of scope (deferred):**

  - **Failed-login emission** at volume. Status (issue #286, 2026-07-28):
    **delivered**. The first deferred item now lands via an async-batched
    `auditor.EmitFailedLogin(ip, emailHash, userAgent)` channel drained by
    a single background goroutine that batches INSERTs every 250 ms or
    1000 rows (whichever first). The handler-side `select { case ch <- row:
    default: dropCounter.Inc() }` is non-blocking by ADR-035 §"Rejected
    alternatives" reason #4 — the sync-write DoS amplifier is the exact
    shape this PR is avoiding. Per-IP metric `apid_failed_login_total{ip}`
    with bounded admission (`maxIPLabelValues = 10_000` → `__other__`
    overflow bucket) plus companion `apid_failed_login_audit_dropped_total`
    counter + `FaasFailedLoginSpike` Prometheus alert at 20/min/IP/5m.
    Code: `cmd/apid/audit.go`, `cmd/apid/handlers_auth_login.go`,
    `cmd/apid/handlers_google.go`, `cmd/apid/handlers_github.go`,
    `pkg/wire/metrics.go`, `pkg/middleware/authlimit.go` (exports
    `ClientIP`), `pkg/auth/hash.go` (new — `HashEmail` is **HMAC-SHA256**
    keyed by a per-box audit HMAC secret; the doc comment documents the
    rainbow-table-resistance property. **Update (PR #386, CodeQL #121):**
    the SHA-256 form documented here was replaced by HMAC-SHA256 before
    ship — the SHA-256 form is rainbow-table-reversible for common
    emails, and the CodeQL `go/weak-sensitive-data-hashing` rule
    correctly flagged it. The audit HMAC key is loaded at apid startup
    via `auth.SetHMACSecret`, with precedence: `FAAS_AUDIT_HMAC_KEY`
    env var (production), `/var/lib/faas/audit-hmac.key` (dev-mode
    auto-generated, 0o600), or zero-key fallback (logs a Warn).).
    Runbook: `docs/runbooks/FaasFailedLoginSpike.md`.
  - **App/Deployment/Cron/Domain audit emissions.** Developer actions,
    not security-relevant; cover in a separate PR if the customer
    audit page ever asks.
  - **Audit retention policy (§17 G3).** Closed by ADR-075
    (pkg/eventretention, daily 90-day trim). The append-only
    contract is unchanged — older rows are deleted wholesale,
    not edited in place.
  - **`actor` enum.** Keep the column text-form; introduce an enum
    only when the dashboard needs to filter by actor.
  - **Per-kind partial index** for `kind`-keyed customer queries
    ("all my login failures this week"). Land when a real customer
    need surfaces.

- **Rejected alternatives:**

  1. **Separate `audit_events` table.** Rejected: two tables is two
     queries, two retention rules, two auditors; the existing events
     table already has the right shape (per schedd's precedent).
  2. **Synchronous pg_notify channel "audit" + SSE subscriber.**
     Rejected: pg_notify has no retention; the dashboard customer
     needs a queryable history.
  3. **Pulling from Loki at query time.** Rejected: customers don't
     have Loki access; ops-side data should not leak into
     customer-facing surfaces.
  4. **Failed-login emission in v1 (sync write).** Rejected: a sync
     INSERT on every failed login creates a DoS amplifier under
     credential-stuffing. v1 emits success only; follow-up PR adds an
     async-batched writer.
