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

  All `auth.*`, `key.*`, `secret.*`, and `account.*` values are namespaced
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

  - **Failed-login emission** at volume. High-frequency DoS amplifier
    via sync DB write; v1 emits `auth.*` only on success paths.
    Failed attempts continue to land as `slog` lines until a follow-up
    PR lands an async-batched audit channel.
  - **App/Deployment/Cron/Domain audit emissions.** Developer actions,
    not security-relevant; cover in a separate PR if the customer
    audit page ever asks.
  - **Audit retention policy (§17 G3).** Append-only is the contract;
    a 90-day trim lives behind a separate ADR.
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
