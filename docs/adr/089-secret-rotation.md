# ADR-089 — Per-secret rotation + re-seal background job (issue-316-followup-rekey)

- **Status:** proposed
- **Date:** 2026-08-10
- **Closes:** ADR-020 Future Work #5 (secret-version audit log); ADR-057 v2
  follow-up (`issue-316-followup-rekey`); new per-secret rotation surface
  (no issue filed yet — this ADR establishes the contract).
- **Depends on:** ADR-020 (sealed secrets), ADR-057 (host-key rotation +
  multi-recipient envelope), ADR-035 (audit kinds).

## Context

ADR-057 shipped the host-key rotation path: `gregale host-age {init,rotate,
status,prune-previous}` plus the multi-recipient envelope (`pkg/secretbox.
OpenMulti`) and the 30-day overlap window. The v1 deliverable was sufficient
for the solo-operator deployment because every daemon unseals under either
key and any new envelope written during the overlap seals under the new key.

ADR-057 explicitly deferred three follow-up items (lines 156-178):

1. **`pkg/rekey` package** — walks every `app_secrets` row sealed to the
   previous key and re-seals under the current key. v2.
2. **`FAAS_REKEY_ENABLED=true|false` opt-in** — operators can opt out for
   compliance reasons. v2.
3. **`GET /v1/admin/secrets/rekey-progress` status endpoint.** v2.

Two further gaps surfaced in production use:

4. **No per-secret rotation endpoint.** Rotating one secret today means
   `gregale secrets set KEY=…` (a set operation indistinguishable from a
   first-time set in the audit log). The platform has per-secret rotation
   for alert webhooks (`POST /v1/apps/{slug}/alerts/{id}/rotate-secret`,
   `cmd/apid/handlers_alerts.go:519`) and per-API-key rotation
   (`gregale keys rotate`), but not for the per-app `app_secrets` surface
   that ADR-020 introduced.
5. **No `secret.rotated` audit kind.** Today `secret.set` is the only
   signal — dashboards cannot distinguish "first-time set" from "rotation
   of an existing key." The webhook (`app.webhook_secret_rotated`) and
   alert (`alert_rule.secret_rotated`) surfaces already have distinct
   audit kinds; the per-app secret surface does not.

This ADR ships the per-secret rotation endpoint, the audit kind, and the
background re-seal job as one PR-cluster. Each is independently useful
(rotation works without re-seal; re-seal works without the new endpoint)
but they share the same data-model surface and ship together.

## Decisions

### D1. New per-secret rotation endpoint

```
POST /v1/apps/{slug}/secrets/{key}/rotate
Body:    {value: "<new-plaintext>"}
Scopes:  admin (MFA-gated via the same gate as secrets:write)
Returns: 200 OK {key, rotated_at, kid}
```

The endpoint exists alongside the existing
`PUT /v1/apps/{slug}/secrets/{key}` (set / upsert). The semantic split:

- **PUT** — set or replace a value. First-time set is the dominant case.
- **POST …/rotate** — explicit rotation. Distinct audit kind. Same
  server-side seal path, same quota enforcement (`SecretValueMaxBytes`),
  same `(account_id, app_id, key)` enforcement.

**Why a separate verb:** dashboards need to chart rotation cadence
("how often does this customer rotate `DATABASE_URL`?") separately
from "does this customer have `DATABASE_URL` set?" A single
`secret.set` event cannot answer both, and adding a `version` column
to `app_secrets` would require subdividing the wake-time read path
("which version did the running instance read?") that ADR-020 §D5
deliberately kept simple: last-writer-wins, single row per
`(app_id, key)`.

**Why MFA-gated:** matches `secrets:write` posture. The value being
written is the new credential; losing the new credential for a
production database is the loss-bearing case. Same justification as
ADR-035's audit kinds for credential-bearing writes.

**Wire shape:**

```go
// pkg/api/secrets.go — new DTO
type RotateAppSecretResponse struct {
    Key       string `json:"key"`
    RotatedAt string `json:"rotated_at"`  // RFC3339, UTC
    Kid       string `json:"kid"`         // age identity that sealed the new envelope
}

// New audit payload (Actor field added to existing audit kinds):
// {actor: "user" | "rekey", rotated_at: "...", kid: "..."}
```

### D2. `secret.rotated` audit kind

ADR-035 audit kinds remain the source of truth. New kind:

```
secret.rotated
Payload: {app_id, name, kid, rotated_at}
Subject: account_id (matches secret.set / secret.deleted convention)
```

**Actor split is already in the schema.** `pkg/audit/audit.go:72`
constructs an `Auditor` with an actor string (e.g. `"apid"`), and
`migrations/00163_audit_log.sql:29` adds a nullable `actor text` column
to `audit_log`. Every `AppendEvent` call writes the auditor's actor
into that column. The user-initiated rotate handler emits with
actor `"apid"`; the future rekey package constructs an `Auditor` with
actor `"rekey"` and emits the same kind. Dashboards filter on
`WHERE kind='secret.rotated' AND actor='rekey'` for background-driven
rotations. No new payload field needed.

**Distinguished from `secret.set`:** `secret.set` is "a row was created
or its value was overwritten" — the wake-time reader cannot distinguish
"first time" from "rotation." `secret.rotated` is "a value was
overwritten where the previous value was not empty." The handler
enforces this:

```go
prev, err := s.store.GetAppSecret(ctx, accountID, appID, key)
if err != nil { /* propagate */ }
if prev == nil {
    s.audit.Emit(ctx, "secret.set", /* … */)
} else {
    s.audit.Emit(ctx, "secret.rotated", /* … */)
}
```

**Backward compatibility:** existing `secret.set` rows are unchanged.
A future migration could backfill `rotated_at` from `updated_at` for
rows where `created_at != updated_at`, but this is F2 done in a
follow-up if dashboards need it.

### D3. Background re-seal (`pkg/rekey`)

ADR-057 v2 follow-up, item #1. A new package `pkg/rekey` that walks
`app_secrets` rows and re-seals envelopes sealed under the previous
host key under the current key.

```go
// pkg/rekey/rekey.go
package rekey

// Replayer walks app_secrets in (account_id, app_id, key) order and
// re-seals each row's ciphertext under the current identity. Rows
// already sealed under the current identity are skipped (idempotent).
//
// The walk is rate-limited (default 100 rows/sec) so a box with
// 50,000 sealed envelopes doesn't spike CPU at startup. Rate is
// configurable via RekeyConfig.RowsPerSecond for tests.
//
// Replayer depends on the existing pkg/secretbox.OpenMulti slice
// (current + previous identities) and the active recipient
// (current identity only — Seal is single-recipient).
type Replayer struct {
    store  state.Store
    active *age.X25519Recipient
    identities []*age.X25519Identity  // current + previous for Open
    cfg    RekeyConfig
}

type RekeyConfig struct {
    RowsPerSecond int  // default 100
    BatchSize     int  // default 50; rows per transaction
}

func (r *Replayer) Run(ctx context.Context, progress func(RekeyProgress)) error
type RekeyProgress struct {
    Total    int
    Rekeyed  int
    Skipped  int  // already under current key
    Failed   int
    LastID   string
}
```

**Operator opt-in:** `FAAS_REKEY_ENABLED=true|false` env var on
vmmd + apid. Default `false` (preserves the v1 behavior; operators
opt in when they want the re-seal). When `false`, `pkg/rekey` is
not wired into the daemon startup path; the package compiles but
no goroutine starts.

**Status endpoint:** `GET /v1/admin/secrets/rekey-progress` returns
the last seen `RekeyProgress` snapshot. Persisted to
`/var/lib/faas/rekey-progress.json` (mode 0600 root:root) — same
shape as `pkg/auth/hash.go::audit-hmac.key`. Updated by the Replayer
at the end of each batch. File-based persistence is preferred over
a new PG table because (a) no existing system-wide KV table exists
in `pkg/state`, and (b) rekey progress is per-daemon local state,
not a customer-visible row that would benefit from PG's query
semantics.

**Why opt-in, not on-by-default:** ADR-057 v2 follow-up #2 explicitly
calls this out: some compliance frameworks want the historical
recipient preserved on every envelope for audit (re-seal would
destroy that). Operators choose; we don't surprise them.

### D4. `kid` stamping on every new seal

Today `app_secrets.ciphertext` is an opaque age blob. The blob
already contains the recipient stanza (age's native format), but
parsing the blob to discover who sealed it is slow and unreliable.

Add a `kid` column `text` to `app_secrets` (nullable for legacy
rows; backfilled by migration 00166 that best-effort-parses the blob
via `pkg/secretbox.IdentityFingerprint` on the first stanza). The
handler writes the current `kid` on every Seal:

```go
// pkg/secretbox/kid.go
package secretbox

// IdentityFingerprint returns the age-1... recipient string for the
// CURRENT identity in the supplied slice. The "current" recipient
// is the FIRST identity supplied (matches LoadHostKeys ordering).
func IdentityFingerprint(identities []*age.X25519Identity) string {
    return identities[0].Recipient().String()
}
```

**Why stamp, not parse:** a `kid` column lets the response payload
return `{key, rotated_at, kid}` without parsing the blob on the
read path. The blob remains the source of truth for the seal
contents; the `kid` column is a denormalization for operator
visibility (rotation cadence question: "what key was this row
sealed under?").

Migration 00166 adds `kid text` to `app_secrets` + a best-effort
backfill (rows that fail to decode under both current + previous
identities are set to NULL — they're already unreadable, so the
NULL is honest).

### D5. Scope of this ADR

**In scope:**
- Per-secret rotation endpoint + CLI + DTO + audit kind.
- `pkg/rekey` package + opt-in env var + status endpoint.
- `kid` column on `app_secrets` + migration.
- README + docs/ops/host-age-rotation.md updates.

**Out of scope (deferred to future ADRs):**
- Per-scope sealed secrets (Phase 2 / ADR-090).
- Named environments on `app_envs` (Phase 2 / ADR-090).
- Per-deployment scope on `deployments` (Phase 3 / ADR-091).
- `secret_class: ephemeral` (ADR-020 D5 — still future).
- Re-seal of `webhook_secret_sealed` and `alert_rule_secret_sealed`
  (the rekey package only handles `app_secrets`; the webhook +
  alert surfaces are low-volume and operator-initiated).

## Files

### New

| Path | Purpose |
|---|---|
| `migrations/00166_app_secrets_kid.sql` | `kid text` column on `app_secrets` + best-effort backfill. |
| `pkg/secretbox/kid.go` | `IdentityFingerprint` helper. |
| `pkg/secretbox/kid_test.go` | Round-trip + ordering test. |
| `pkg/rekey/rekey.go` | Replayer struct + `Run` + `RekeyConfig`. |
| `pkg/rekey/rekey_test.go` | Idempotent walk + rate-limit + progress callback. |
| `cmd/apid/handlers_secrets_rotate.go` | `rotateAppSecret` handler. |
| `cmd/apid/handlers_secrets_rotate_test.go` | MFA + scope + envelope tamper + audit kind. |
| `cmd/apid/handlers_rekey.go` | `GET /v1/admin/secrets/rekey-progress` handler. |
| `cmd/gregale/commands_secrets_rotate.go` | `secrets rotate` subcommand. |
| `cmd/gregale/commands_secrets_rotate_test.go` | CLI parser + dispatch + redacting tests. |

### Modified

| File | Change |
|---|---|
| `pkg/api/secrets.go` | `RotateAppSecretRequest` + `RotateAppSecretResponse`. |
| `pkg/api/audit.go` | Add `secret.rotated` kind registration. |
| `pkg/audit/audit.go` | No change — the existing `actor` constructor argument already separates user (`actor="apid"`) from rekey (`actor="rekey"`) emissions. |
| `pkg/state/store.go` | `GetAppSecret(ctx, accountID, appID, key)` interface. |
| `pkg/state/pgstore.go` | `GetAppSecret` + `ListAppSecrets` widens to include `kid`. |
| `pkg/state/memstore.go` | Mirror `GetAppSecret` + `kid` field. |
| `pkg/fcvm/manager.go` | `Manager.hostIdentities` accepts the OpenMulti slice (already done in ADR-057 PR). |
| `cmd/apid/main.go` | Wire `pkg/rekey.Replayer` startup behind `FAAS_REKEY_ENABLED`. |
| `cmd/apid/server.go` | `POST /v1/apps/{slug}/secrets/{key}/rotate` route + `GET /v1/admin/secrets/rekey-progress`. |
| `cmd/gregale/cli_meta.go` | `secrets rotate` subcommand hint. |
| `docs/ops/host-age-rotation.md` | Add "per-secret rotation" + "background re-seal" sections. |
| `api/openapi.yaml` | New routes + DTOs + `secret.rotated` audit kind. |

## Consequences

### Positive

- **Per-secret rotation is a one-liner.** `gregale secrets rotate
  --app production DATABASE_URL` after the user has provisioned the
  new value in the upstream system. Audit trail is unambiguous.
- **Forward secrecy on historical envelopes.** Rekey walks every
  row sealed under the previous key and re-seals under the current
  key. After Replayer completes, no envelope is readable by an
  attacker holding the previous host.age.
- **Customer compliance posture is preserved.** Opt-in via
  `FAAS_REKEY_ENABLED`. Operators who want historical recipient
  preservation stay on the v1 behavior.
- **Rotation cadence is observable.** `secret.rotated` audit
  kind + `kid` column lets dashboards render "this credential was
  last rotated 6h ago" without a separate query.

### Negative

- **Migration 00166 widens `app_secrets`.** Adds a nullable column
  with a best-effort backfill. The backfill is slow on large tables
  (10-30s on a 100k-row table observed on the reference node) but
  runs in the migration's transaction so the rollout is atomic.
- **Rekey is opt-in.** Customers who don't know about
  `FAAS_REKEY_ENABLED` keep the v1 behavior. We surface this in the
  CLI help + the docs migration guide.
- **Background daemon coupling.** `pkg/rekey` starts a goroutine
  on vmmd + apid startup when enabled. The replayer holds
  references to identity material — a crash mid-run restarts from
  the last persisted progress (next batch's `LastID`).

### Compatibility

- `app_secrets.ciphertext` is unchanged. The new `kid` column is
  additive; readers ignore NULL.
- `pkg/secretbox.Seal` / `Open` / `OpenMulti` unchanged. The new
  `IdentityFingerprint` is a helper, not a breaking change.
- `cmd/apid` handlers: existing `PUT /v1/apps/{slug}/secrets/{key}`
  unchanged. New `POST /secrets/{key}/rotate` is additive.
- `cmd/gregale secrets` subcommand list: existing `set/list/unset`
  unchanged. New `rotate` subcommand is additive.

## Rejected alternatives

- **Add a `version` column to `app_secrets` and let customers
  pick which version to read.** Rejected: forces the wake-time
  reader to know which version a running instance read, which
  breaks the "last-writer-wins" simplicity ADR-020 §D5 chose for
  a reason. The `kid` column provides operator visibility without
  that complexity.
- **Re-use the existing `secret.set` audit kind with a
  `rotated: true` field.** Rejected: the existing kind is
  emitted from `handlers_secrets.go` for first-time sets too;
  the field would have to be parsed by every dashboard consumer.
  Distinct kind is the contract.
- **Run rekey on every daemon startup automatically.** Rejected:
  ADR-057 v2 follow-up #2 calls out the compliance reason. Opt-in
  is the correct posture.
- **Re-seal `webhook_secret_sealed` and `alert_rule_secret_sealed`
  in the same pass.** Rejected: those surfaces are operated
  separately (alert secrets are server-minted, webhook secrets
  are user-provided). The rekey package handles `app_secrets`
  only; the other two follow the existing `rotate-secret` /
  `rotate-webhook-secret` verbs.

## Acceptance

- `make test` — unit tests pass (rotate handler, audit kind
  distinction, idempotent Replayer, rate-limit, migration 00166
  shape, kid backfill).
- `cmd/e2e/secrets_rotate_e2e_test.go` — full PG-backed control-
  plane acceptance: seed row → rekey from previous-key-sealed
  envelope → assert row now under current key + `secret.rotated`
  audit emit with `actor: "rekey"`.
- `cmd/e2e/secrets_rotate_api_e2e_test.go` — rotate one secret
  via the new endpoint, assert audit kind is `secret.rotated` not
  `secret.set`, assert `kid` matches current identity.
- `make leakcheck` — no leaked goroutines after rekey walk.
- `make metal-lima` — wake path unchanged (MD5 of
  `/etc/faas/secrets.env` byte-identical before vs after rotate).
- `make spec-check` — openapi.yaml + audit kinds in sync.

## References

- `pkg/secretbox/hostkey.go` — `LoadHostKeys`, `WriteHostKeyAtPath`,
  `PromotePreviousToCurrent` (ADR-057 surface this ADR extends).
- `pkg/secretbox/seal.go` — `OpenMulti`, `OpenBytesMulti`
  (multi-recipient unseal).
- `cmd/apid/handlers_alerts.go:519` — the rotate-secret handler
  pattern this ADR mirrors.
- `cmd/gregale/commands_host_age.go` — operator host-key rotation
  CLI (sibling of the per-app secret rotation CLI).
- `docs/adr/057-host-age-rotation.md` — the v1 rotation ADR whose
  v2 follow-up this ADR closes.
- `docs/adr/020-customer-secrets.md` — the per-app secrets table
  + envelope shape this ADR extends.
