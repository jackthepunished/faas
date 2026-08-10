# ADR-089 PR-cluster outline

This file is the commit strategy for ADR-089. It mirrors the Tier A / Tier A7
PR-cluster pattern (see `[[tier-a7-pr-cluster-strategy]]` memory). Three PRs,
each independently mergeable, sized to review in ≤10 minutes.

**Slot fence:** Drop a `migrations/00XXX_reserve_slot.sql` fence (idempotent
`select 1;`) at the next-free slot before PR-A lands. The 00166 slot is
reserved by a fence file with `00XXX = 165` (the canonical "next slot" pattern
from ADR-041). Rename 165 → 166 in PR-A's migration file at the same time
the fence is removed.

## PR-A — Schema + Replayer skeleton (the only PR that touches data)

**Branch:** `worktree-adr-089-secret-rotation/pr-a-schema-rekey`
**Size:** ~280 LoC across 8 files
**Review time:** ~8 min

**Files:**

| Action | Path | Content |
|---|---|---|
| New | `migrations/00166_app_secrets_kid.sql` | `kid text` column on `app_secrets` + best-effort backfill; index on `(kid)` for "find all rows sealed under previous key" queries. |
| New | `pkg/secretbox/kid.go` | `IdentityFingerprint(identities []*age.X25519Identity) string` — returns the first identity's recipient string. |
| New | `pkg/secretbox/kid_test.go` | Multi-identity ordering + empty-slice rejection. |
| New | `pkg/rekey/rekey.go` | `Replayer` struct + `RekeyConfig` + `Run(ctx, progress)` + `RekeyProgress`. |
| New | `pkg/rekey/rekey_test.go` | Idempotent walk (Run twice = same result), rate-limit, batch transaction rollback, progress callback accumulation. |
| Modified | `pkg/state/store.go` | `GetAppSecret(ctx, accountID, appID, key)` interface method. |
| Modified | `pkg/state/pgstore.go` | `GetAppSecret` impl; `ListAppSecrets` widens to include `kid`; `UpsertAppSecret` widens to accept `kid`. |
| Modified | `pkg/state/memstore.go` | Mirror `GetAppSecret` + `kid` field for parity. |
| Modified | `pkg/api/audit.go` | Register `secret.rotated` kind (no `pkg/audit/audit.go` change — the existing `actor` constructor argument already separates `actor="apid"` from `actor="rekey"` emissions). |

**Why this is PR-A:** schema + state surface + audit semantic. Nothing in
this PR is user-facing. Reviewers can verify the data model + the
replayer's idempotency without reading any HTTP or CLI code.

**Acceptance:**

- `make test` — unit tests pass.
- `make spec-check` — audit kinds in sync (even though no openapi change yet).
- `make migrate-test` — migration 00166 reads clean on a fresh PG.

**Risk:** the migration's backfill COULD be slow on large tables. Pin
the backfill query via `EXPLAIN ANALYZE` in the migration's comment so
operators running it on a 100k-row table know to expect 10-30s. The
backfill is wrapped in `BEGIN; ... COMMIT;` so the rollout is atomic.

---

## PR-B — Rotate handler + CLI (user-facing surface)

**Branch:** `worktree-adr-089-secret-rotation/pr-b-handler-cli`
**Depends on:** PR-A
**Size:** ~340 LoC across 7 files
**Review time:** ~10 min

**Files:**

| Action | Path | Content |
|---|---|---|
| New | `cmd/apid/handlers_secrets_rotate.go` | `rotateAppSecret(w, r, acct)` — mirrors `PUT /secrets/{key}` body but uses the rotate path; emits `secret.rotated` when `prev != nil`, `secret.set` when `prev == nil`. |
| New | `cmd/apid/handlers_secrets_rotate_test.go` | MFA-gate, scope check, audit kind distinction (rotate vs set), envelope tamper rejection, missing key → 404. |
| New | `cmd/apid/handlers_rekey.go` | `GET /v1/admin/secrets/rekey-progress` handler returning the persisted `RekeyProgress` snapshot. |
| New | `cmd/gregale/commands_secrets_rotate.go` | `secrets rotate` subcommand with `--app <slug> KEY` flag. |
| New | `cmd/gregale/commands_secrets_rotate_test.go` | Parser + dispatch + redacting tests (must scrub the new value from logs). |
| Modified | `pkg/api/secrets.go` | `RotateAppSecretRequest` + `RotateAppSecretResponse` + `RekeyProgressResponse` DTOs. |
| Modified | `cmd/apid/server.go` | `POST /v1/apps/{slug}/secrets/{key}/rotate` route + `GET /v1/admin/secrets/rekey-progress`. |
| Modified | `cmd/gregale/cli_meta.go` | `secrets rotate` subcommand hint line. |
| Modified | `api/openapi.yaml` | New routes + DTOs + audit kind enumeration. |
| Modified | `pkg/apid/openapi.yaml` | The `//go:embed` copy (must run `make spec-sync` after openapi.yaml change). |

**Why this is PR-B:** every user-facing wire surface lands here. PR-A
gave us the data model; PR-B gives us the API on top of it. The CLI
subcommand is small (~80 LoC) and mirrors the existing `secrets set`
shape closely.

**Acceptance:**

- `make test` — unit tests pass.
- `make spec-check` — openapi.yaml ↔ Go code parity.
- `cmd/e2e/secrets_rotate_api_e2e_test.go` — full PG-backed acceptance:
  seed row → POST /rotate → assert audit kind is `secret.rotated` not
  `secret.set`, assert `kid` matches current identity.

**Risk:** the openapi.yaml regen touches the `pkg/apid/openapi.yaml`
embed (memory: `spec-sync-stale-embed-on-openapi-change.md`). Run
`make spec-sync` after every openapi.yaml edit, or the CI's spec-check
gate fails.

---

## PR-C — Rekey wiring + e2e + ops surface (the operational close)

**Branch:** `worktree-adr-089-secret-rotation/pr-c-wiring-e2e`
**Depends on:** PR-A, PR-B
**Size:** ~220 LoC across 6 files
**Review time:** ~8 min

**Files:**

| Action | Path | Content |
|---|---|---|
| New | `cmd/apid/rekey_runner.go` | Wires `pkg/rekey.Replayer` startup behind `FAAS_REKEY_ENABLED=true`; writes progress to `system_metadata` JSONB row. |
| New | `cmd/apid/rekey_runner_test.go` | Idempotent startup (no goroutine spawned when `FAAS_REKEY_ENABLED=false`), progress callback writes correctly. |
| New | `cmd/e2e/secrets_rotate_e2e_test.go` | Full E2E: seed envelope under previous identity → enable rekey → assert replayer walks → assert row now under current key + audit emit with `actor: "rekey"`. |
| New | `cmd/e2e/secrets_rotate_box_e2e_test.go` | Cross-daemon: rotate host-age → rekey runs → unseal succeeds under current key only (after pruning previous). |
| Modified | `cmd/apid/main.go` | CLI flag for `FAAS_REKEY_ENABLED` with admin warning at startup if set without persistence. |
| Modified | `docs/ops/host-age-rotation.md` | Add "Per-secret rotation" section + "Background re-seal" section + `FAAS_REKEY_ENABLED` operator runbook. |
| Modified | `docs/adr/README.md` | Add ADR-089 row to the index. |

**Why this is PR-C:** everything in PR-C is the operational close. The
core feature ships without PR-C (operator can run `gregale secrets set`
to manually re-seal; or run `gregale host-age rotate` and let the
overlap window work). PR-C adds the automated background job that
makes the rotation complete in the background.

**Acceptance:**

- `make test` — unit tests pass.
- `make leakcheck` — no leaked goroutines after rekey walk (the
  replayer must `defer cancel()` on its context and wait for the
  goroutine to drain before main returns).
- `make metal-lima` — wake path unchanged (MD5 of
  `/etc/faas/secrets.env` byte-identical before vs after rotate).
- `cmd/e2e/secrets_rotate_*` — full E2E green.
- `make spec-check` — last sanity check.

**Risk:** the rekey goroutine runs at daemon startup. If it blocks
daemon readiness, health checks fail. Pin the goroutine to start
AFTER the daemon reports ready (use the same `ready.Signal` pattern
that `pkg/fcvm/manager.go` uses for snapshot warm-up).

---

## Commit order within each PR

Each PR is a single squash-merge commit on its branch. The commit
sequence across PRs is:

1. PR-A squash: "feat(state+secretbox+rekey): kid column + Replayer skeleton (ADR-089 PR-A)"
2. PR-B squash: "feat(apid+cli+openapi): per-secret rotation endpoint + CLI + secret.rotated audit (ADR-089 PR-B)"
3. PR-C squash: "feat(apid+ops+e2e): FAAS_REKEY_ENABLED opt-in + rekey-progress e2e (ADR-089 PR-C)"

Each PR's commit message names the ADR. The PR description names the
milestone (none — this is post-M8 hardening). When the PR-cluster lands,
the ADR slot fence file is removed in PR-A's commit (the rename from
158 → 159 happens in the same commit).

## Cross-cutting notes

- **Per-scope secrets:** rejected (memory: `secrets-envs-roadmap-decisions-2026-08-10.md`).
  Sealed secrets stay per-app. This ADR does not introduce per-scope
  sealed secrets; that's Phase 2 / ADR-090.
- **Live update of running instance:** rejected (ADR-045 §5). The
  rotation applies on the next wake (cold boot OR snapshot restore).
- **Snapshot-time plaintext exposure:** out of scope. The
  `secret_class: ephemeral` opt-in is still future work from ADR-020 D5.
- **Background re-seal of webhook + alert secrets:** out of scope. Those
  surfaces are operated separately via `alerts rotate-secret` and
  `webhooks rotate-webhook-secret`. The `pkg/rekey` package handles
  `app_secrets` only.
