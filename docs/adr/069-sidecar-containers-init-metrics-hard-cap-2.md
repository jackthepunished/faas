# ADR-069 — Sidecar containers: init + metrics (issue #463)

- **Status:** proposed
- **Date:** 2026-08-02
- **Issue:** #463
- **Decision:** customers can attach up to **2 stateless sidecars**
  (1 init + 1 sidecar) to any deployment via a new
  `sidecars: [...]` field on the existing
  `POST /v1/apps/{slug}/deployments` request. Each sidecar is
  its own OCI pull, its own diff_ids-above-base, its own drive1.
  Sidecars share the tenant netns (one netns per instance); each
  sidecar gets a separate cgroup scope (PR-B wires the runtime).
  The hard cap is **2 globally** (`SidecarCapMax = 2` in
  `pkg/api/limits.go`), not a per-plan matrix. Stateless
  deny list (`StatefulBaseImageDenylist`) applies to sidecar
  images; envelope-sealed env via
  `secretbox.SealBytes(namespace="sidecar_env")`; image references
  must be digest-pinned (`repo@sha256:...`). Billing is
  `plan.RAMMB + Σ(sidecar.ram_mb) + PerVMOverheadMB` per running
  second.

## Why

Today, a one-box FaaS customer deploys exactly one workload per
app: a single OCI image running as PID 1 inside a Firecracker
microVM. There is no surface for the two adjacent stateless
patterns every customer hits sooner or later:

1. **DB migrator as init.** A customer rolling schema changes
   today bakes the migrator into the main image, which means
   redeploying the image every time the schema moves and exposing
   the migrator's process to the runtime. The right shape is
   "run this image once, before the main workload, exit 0 → ok,
   exit non-zero → fail the deploy".
2. **Metrics scraper as sidecar.** A customer wiring Prometheus
   today must bake the scraper into the main image (so they can
   expose `/metrics` on the same socket) or run a sidecar
   container on a separate host (which doesn't fit the one-box
   model). The right shape is "run this image alongside the main
   workload, share the netns, separate the cgroup for OOM
   isolation".

Both shapes mirror AWS Fargate's init-container + sidecar model
minus the stateful options (no EFS, no EBS, no emptyDir). The
acceptance gate from issue #463 is 8 checkboxes; PR-A closes the
6 that don't require runtime wiring.

## Decision

**1. Hard 2-sidecar cap is a global constant, NOT a per-plan
   matrix.** `pkg/api/limits.go` gains
   `const SidecarCapMax = 2` in the existing
   `const (...)` block (alongside `WakeQueueCap = 512`). No
   per-plan `SidecarsMax` field on `Limits`. The cap is
   structurally tight by design: every per-app quota the
   platform ships — cron × N, secret × N, env × N, registry
   creds × N — was bounded to defensively cap an abuse vector;
   2 is the smallest cap that's useful for "init + sidecar".
   A future PR can add a per-plan matrix if telemetry shows
   demand; today's single constant is the simplest correct
   shape. The cap is **also** pinned at the schema layer
   (`CHECK jsonb_array_length(sidecars) <= 2`) so a hand-call
   to the store layer can't bypass the API gate.

**2. JSONB column on `deployments`, not a normalized table.**
   `migrations/00096_deployments_sidecars.sql` adds
   `deployments.sidecars jsonb NOT NULL DEFAULT '[]'::jsonb`
   plus the CHECK constraint. JSONB is adequate because:
   (a) per-instance O(2) reads; (b) no per-sidecar queries;
   (c) atomic CREATE-DEPLOYMENT write; (d) ships in one
   `ALTER TABLE`. The schema shape matches the
   `migrations/00079_deployment_overrides.sql` and
   `migrations/00082_apps_scaling_policy.sql` precedents: a
   single jsonb column with a `NOT NULL DEFAULT '<shape>'::jsonb`
   default, no GIN index, application-layer validation.

**3. Each sidecar envelope-seals env via
   `secretbox.SealBytes(recipient, "sidecar_env", plaintext,
   maxBytes)`.** Mirror of `AppSecret`'s `secrets` namespace.
   Plaintext values are NEVER logged, audited, or persisted in
   cleartext. Only metadata (`name`, `image`, `type`, `cmd`
   argv shape) is. PR-A's apid handler seals post-decode; PR-B's
   `pkg/imaged` unseals transiently at the pull path (the same
   pattern as `app_registry_credentials` OpenBytes in ADR-062).

**4. Stateless deny list applies to sidecar base images.**
   `Sidecar.Validate()` rejects any image whose digest is in
   the existing `pkg/imaged/handler_image_build_test.go`
   `StatefulBaseImageDenylist` set (Postgres, Redis, MySQL,
   MongoDB, etc.). The state is shape-locked by the spec: a
   sidecar that holds a database or a key-value store breaks
   snapshot reuse (drive1 is per-app, not shared). Defence in
   depth — PR-B's runtime calls the same gate again at the
   pull path.

**5. Image references are digest-pinned** (`repo@sha256:...`),
   never by tag. Tag-pinning is the documented OCI
   supply-chain attack vector; the sidecar contract inherits
   the runtime image's stance from the deploy-override ADR-053
   hard rule. The OpenAPI `description` advertises the
   requirement so the SDK / CLI surfaces a useful error at
   client time.

**6. Billing math locked**:
   `per_instance_billable_mb = plan.RAMMB + Σ(sidecar.ram_mb) +
   PerVMOverheadMB`. PR-A defines the math via
   `BillableRAMMBWithSidecars(ramMB int, sidecarMBs []int) int`
   helper alongside the existing `BillableRAMMB(ramMB int)`.
   PR-B wires the consumer (`pkg/meter/sampler.go` and schedd
   admission). The "+8 MB" baseline is `PerVMOverheadMB`
   (`pkg/api/limits.go:591`), unchanged.

**7. No shared writable layer between sidecars and main
   workload. No inter-workload bind mounts.** Each sidecar is
   its own OCI pull, its own `diff_ids` above the base, its
   own drive1. The tenant netns is shared (one netns per
   instance, per ADR-009); separate cgroup scopes inside that
   netns give OOM isolation. This is load-bearing — a shared
   writable layer breaks snapshot reuse (drive1 is per-app)
   and violates §11 "no shared host directories with guests".

**8. PR-A ships the contract + storage. PR-B wires the runtime
   effect** (`pkg/imaged`/`pkg/fcvm`/`guest-init`, separate
   cgroups). PR-C ships the e2e + observability. The merge
   order is A → B → C; PR-A has zero runtime effect on
   `schedd`, `vmmd`, `pkg/fcvm`, `guest/init`, `pkg/imaged`,
   or `pkg/netns`. The contract is honest at every PR
   boundary: the field persists in PR-A, validates at PR-A,
   executes at PR-B.

**9. SDK surface unchanged.** Sidecars ride the existing
   `POST /v1/apps/{slug}/deployments` request body. No new
   HTTP routes. `cmd/sdk-coverage/main.go::methodRouteMap` is
   frozen. The CLI shape (`faas deploy --sidecar` flags) is a
   follow-up after PR-B lands.

**10. Handler surface: per-sidecar envelope-seal happens at
    apid.** The plaintext env lives only in the request body
    decode frame and the in-call seal; the persisted shape is
    sealed ciphertext per value. PR-B's `pkg/imaged` is the
    sole consumer of the sealed payload (transient unseal at
    pull time, plaintext GC'd on return). Failure modes:
    sealed `OpenBytes` with wrong namespace → PR-B treats as
    "credential unusable" and fails the deploy with a redacted
    error. The audit event `sidecar.set` carries
    `{app_id, deployment_id, sidecar_count, sidecar_names}`
    only — no env, no image digest, no Cmd.

## Failure modes

| Scenario | Behaviour |
|---|---|
| Request carries `len(sidecars) > 2` | `ErrSidecarCapExceeded` 400 at apid; CHECK on the schema is the second-line defence. |
| Request carries `len([s for s in sidecars if s.Type=="init"]) > 1` | `ErrSidecarInvalidType` 400 at apid; only one init + one sidecar per deployment. |
| `image: "repo:tag"` (not digest-pinned) | `ErrSidecarInvalidImage` 400 at apid; OpenAPI description advertises the requirement. |
| Stateful image (`postgres:15`, `redis:7`, etc.) | `ErrSidecarStatefulDenied` 403 at apid; even with admit `pkg/imaged` rejects. |
| Free plan + sidecars | Same as Hobby+; PR-A does NOT apply a per-plan sidecar gate (the global cap is the gate). |
| apid boot without age recipient loaded | `ErrCapacity` 503 at POST (recipient not loaded → refusing to seal). Same posture as `setSecret` handlers. |
| Sidecar env value exceeds `EnvValueMaxBytes` | `ErrSecretValueTooLarge` 413 at apid; value clamped before sealing. |
| Sealed env `OpenBytes` failure at wake (PR-B runtime) | PR-A doesn't wire Open. PR-B treats as "credential unusable" — fails the deploy with redacted error. PR-A's only contract here is: never store plaintext, never log plaintext, never put plaintext in audit. |
| Slot 96 vs sibling PR #526 holding slot 95 | PR #526's `00095_reserve_slot.sql` already fences 95 on its branch; PR-A (this ADR) renumbered 95 → 96 and planted `00095_reserve_slot.sql` of its own. Whichever lands second drops the other's reservation on rebase. The cross-PR slot gate's `slots_from_paths` regex carve-out hides reservations. |
| `e2eMigrationTarget` 94 → 96 in `pkg/e2etest/harness.go` | Migrations tests use literal UUIDs (renumbered 95 → 96); e2e harness waits for `96`. |
| `pkg/api ↔ pkg/state cycle` | `pkg/state/Deployment.Sidecars` is `json.RawMessage` (no `pkg/api` import). Decoder helper at the handler boundary. |
| Migration bytea literal pitfall | n/a — jsonb shape (`migration-test-bytea-literal-shape.md` does not apply). |
| Field name overlap with future fields (e.g. `Sidecar.Priority`) | The Type field is a closed set enum string; future fields are additive. |
| SDK regen misses the new `Sidecar` shape | `make spec-sync` + `make sdk-gen` lands both yaml + SDK in one go; CI `make sdk-check` flags drift. |
| `sdk-go/internal/api/errors.go` mirror gap | 8 new error constructors exist in the hand-curated subset (per `sdk-go-errors-hand-curated-subset.md`); CI root-checked by `make sdk-check`. |

## Security

- **§11 unchanged.** No new cgroup / uid / netns surface in PR-A;
  imaged is non-root; egress enforcement unchanged; envelope-sealed
  env. PR-B wires the cgroup carve-out; PR-A only persists the
  field.
- **No widening of auth scope.** Sidecars ride `ScopeDeployWrite`
  + `ScopeAdmin` (existing `ScopesDeployWriteSurface`). No new
  scope.
- **No widening of egress.** `apps.egress_allowlist` remains
  authoritative. A sidecar pulling from a non-allowlisted
  registry fails the dial; `oci_egress_deny_total` increments.
- **Plaintext env NEVER appears** in any `slog` field, audit
  payload, error string, or HTTP response. Pinned by capture-
  based tests in `handlers_deployments_test.go`.
- **Image by tag → reject 400.** Tag-pinning is the documented
  OCI supply-chain attack vector; the runtime image already
  enforces this in `pkg/imaged`; PR-A replicates the gate at
  the API layer so a customer with a typo gets a useful error
  in their browser, not a 500 from imaged.
- **Stateful image → reject 403.** Hard denied. Stateful
  workloads go on dedicated infra, not FaaS. The denylist set
  lives in `pkg/imaged`; PR-A imports the constant for the API
  gate.
- **Per-VM cgroup ceiling** (PR-B) bounds the
  `Σ(sidecar.ram_mb)` term via the existing `RAMAdmissionCeilingMB`
  invariant; schedd admission re-checks each wake.

## Rejected alternatives

- **Per-plan matrix (Free=0, Hobby=2, Pro=2, Scale=2).** The
  issue body says "hard cap 2" with no per-plan tier. The
  global constant is the simplest correct shape; tier-ups
  follow when telemetry shows which plans need them.
- **Normalised `deployment_sidecars` table.** Overkill for the
  2-cap: join cost on every wake read for 2 rows; no
  per-sidecar query patterns. JSONB is fine.
- **Allowing stateful sidecars.** No — the spec is stateless
  only; stateful workloads break snapshot reuse + violate §11.
- **Shared writable layer** (`/tmp` mount between sidecar and
  main) — no, breaks snapshot reuse (drive1 is per-app) and
  violates §11 no-shared-host-dirs rule.
- **PR-A wires the runtime effect.** No — would balloon into a
  multi-thousand-line PR; the cascading shape keeps PR-A
  reviewable in ~10 min (CLAUDE.md "Small PRs") and isolates
  the runtime work for PR-B (mirrors ADR-053 §Decision 5).
- **Embedding password / sealed-env in `pg_notify` payload.**
  pg_notify is plaintext WAL (mirrors ADR-062). The persisted
  jsonb is the source of truth; PR-B reads it on wake.
- **Per-instance, not per-deployment, sidecar config.** A
  customer can already re-deploy with a different sidecar set;
  the deploy-time semantic is the right binding for v1.
- **CLI shape in PR-A.** No — the runtime effect isn't wired,
  so a `faas deploy --sidecar` flag without a runtime effect
  is misleading. CLI lands after PR-B (avoids back-compat
  churn on the runtime surface).

## Consequences

- **`migrations/00096_deployments_sidecars.sql`** (new):
  `ALTER TABLE deployments ADD COLUMN IF NOT EXISTS
  sidecars jsonb NOT NULL DEFAULT '[]'::jsonb` + CHECK
  `jsonb_array_length(sidecars) <= 2`. Replay-safe via the
  IF NOT EXISTS guard (ADR-041 / PR #377).
- **`migrations/00096_deployments_sidecars_test.go`** (new):
  literal-UUID pins `...000096` / `...000196` / `...000296` /
  `...000396` mirroring `00094_app_registry_credentials_test.go`. PR-A also plants `00095_reserve_slot.sql` to fence the slot held by open PR #526.
  7 cases: apply-through, 0-cap insert, 2-cap insert, 3-cap
  CHECK-rejection, round-trip shape preservation, NULL DEFAULT
  fill, replay-safety.
- **`pkg/state/types.go::Deployment`** gains
  `Sidecars json.RawMessage` (mirrors
  `OverrideHealthcheck json.RawMessage` at `types.go:554`).
  State layer never imports `pkg/api`; decoder lives at the
  handler boundary.
- **`pkg/state/pgstore.go`** grows by one column on three
  `deploymentSelectColumns*` projections + one parameter on
  the `CreateDeployment` INSERT + one destination on three
  `scanDeployment*` functions. Selections use
  `coalesce(sidecars, '[]'::jsonb)` so legacy rows read back
  as `[]` (never `null`). The handler normalises empty/nil to
  `[]` before write so the `NOT NULL` constraint is satisfied
  without relying on PG DEFAULT-on-insert.
- **`pkg/state/memstore.go`** carries `Sidecars` byte-for-byte
  through the `Deployment` struct; no explicit logic needed.
- **`pkg/api/dto.go`** gains `Sidecar` struct + `Sidecars`
  field on `DeployRequest` + `Sidecar.Validate()` +
  `Sidecars.Validate(limits) *Problem`. Lives in
  `pkg/api/dto.go` alongside `CreateDeploymentOverrides`
  (the existing override surface — same shape patterns:
  per-element regex, per-byte cap, type-uniqueness, count cap).
- **`pkg/api/limits.go`** gains `SidecarCapMax = 2` global
  constant + `BillableRAMMBWithSidecars(ramMB int, sidecarMBs
  []int) int` sibling helper + `Plan.SidecarAllowed() bool`
  accessor (currently returns true for all plans; reserved
  for future per-plan tier-up).
- **`pkg/api/errors.go`** gains 8 RFC 7807 problem constructors
  (`ErrSidecarCapExceeded`, `ErrSidecarInvalidType`,
  `ErrSidecarInvalidImage`, `ErrSidecarStatefulDenied`,
  `ErrSidecarInvalidName`, `ErrSidecarInvalidPort`,
  `ErrSidecarInvalidRamMB`, `ErrSidecarNotAllowedOnPlan` —
  the last is unwired in PR-A, defence-in-depth for future
  PRs).
- **`cmd/apid/handlers_deployments.go`** decode/validate/persist
  extended in-place; envelope-seal helper is local to the
  handler file. PR-A's audit emission is `sidecar.set` with
  metadata only.
- **`api/openapi.yaml`** gains the `Sidecar` schema + the
  `sidecars` field on `CreateDeploymentRequest`. `make
  spec-sync` + `make sdk-gen` regen.
- **`pkg/apid/openapi.yaml`** (the `//go:embed` copy) refresh
  via `make spec-sync` (per `spec-sync-stale-embed-on-openapi-change.md`).
- **`sdk/{go,node,python}`** regen via `make sdk-gen`.
- **`sdk/go/internal/api/errors.go`** (hand-curated subset,
  per `sdk-go-errors-hand-curated-subset.md`) gains 8 mirror
  constructors so the Go SDK compile stays green.
- **`pkg/e2etest/harness.go::e2eMigrationTarget`** 94 → 96.
- **`pkg/state/embed_test.go`** + `apply_walk_test.go` expected
range 1..96.
- **`docs/adr/README.md`** table gains the ADR-069 row.
- **No new migration beyond 00096.** Slot policy per
  `cross-pr-slot-gate-reservation-fence-pattern.md`; open PR #526
  holds slot 095, so PR-A renumbered 95 → 96 and planted
  `00095_reserve_slot.sql` (the canonical `select 1;` fence body
  copy from `00087_reserve_slot.sql`).
- **No SDK surface change** (no new HTTP routes).
- **No CLI surface change** in PR-A.
- **No `pkg/fcvm` / `pkg/imaged` / `guest/init` /
  `pkg/netns` touch.**

## Downstream

- **Issue #463 closes 6 of 8 acceptance criteria.** The
  remaining 2 ("init runs first", "OOM isolation") are PR-B's
  runtime-effect scope.
- **PR-B** wires the runtime effect: `pkg/imaged.buildImageLayer`
  extended to also call `buildSidecarLayer(sidecar)` per sidecar;
  `pkg/fcvm.ColdBootSpec` grows a `Workloads []Workload` slice;
  `guest/init/supervise.go` adds `runSidecar` alongside the
  existing `runAppWithEnv`; separate cgroup scopes inside the
  per-instance tenant netns.
- **PR-C** wires the e2e + observability: `cmd/e2e/sidecar_e2e_test.go`
  exercises init-then-main + sidecar-along-main + restart-loop;
  `pkg/wire/metrics.go` reserves `schedd_sidecar_restart_total
  {app,sidecar}` (bounded by the 2-cap); metric cardinality
  validated across redeploys with novel sidecar names.
- **Note (PR-C 2026-08-04):** the counter is registered as
  `vmmd_sidecar_restart_total` (vmmd is the canonical
  producer today, in `dispatchSidecarRestart`); the
  `<daemon>_sidecar_restart_total` shape is preserved so a
  future schedd-side producer can host the same family under
  its own prefix without a rename. See ADR-072 §"Decisions 2".
- **No follow-up ADR proposed for PR-A.** Future ADR scope
  (per-account sidecar fan-out, sidecar priority ordering,
  sidecar resource classes) is unaddressed and deferred.

## Reused on main (no redesign)

- `pkg/api/limits.go::PerVMOverheadMB = 8` (limits.go:591) —
  the "+8 MB" baseline. Sidecar math builds on top; the +8
  stays unchanged.
- `pkg/api/limits.go::BillableRAMMB(ramMB int) int`
  (limits.go:1238) — PR-A adds `BillableRAMMBWithSidecars`
  as a sibling helper. The single-arg form stays for PR-B to
  migrate.
- `pkg/secretbox.SealBytes(recipient, namespace, plaintext,
  maxBytes)` (pkg/secretbox/seal.go:183-214) — env envelope.
- `pkg/secretbox.OpenBytes(identity, blob)` (pkg/secretbox/
  seal.go:227-232) — PR-B's wake-time unseal.
- `pkg/api/dto.go::CreateDeploymentOverrides.Validate(limits
  Limits) *Problem` (dto.go:440+) — the mirror surface for
  `Sidecars.Validate`.
- `pkg/api/errors.go::CodePlanMinInstancesNotAllowed`
  (errors.go:1026) — code-naming pattern.
- `pkg/api/limits.go::RegistryCredentialMax` + accessor
  pattern (limits.go on `origin/main`) — mirrors PR #522's
  per-app quota matrix style; PR-A ships the constant
  variant.
- `pkg/state/memstore.go::MemStore.CreateDeployment` —
  carries the new `Sidecars` field through automatically;
  no explicit logic needed.
- `pkg/state/pgstore.go::nullJSONRaw(b json.RawMessage) any`
  (lines 7123-7133) — handles empty/nil jsonb; PR-A
  normalises empty/nil to `'[]'::jsonb` so the NOT NULL
  constraint is satisfied.
- `migrations/00094_app_registry_credentials_test.go` —
  literal-UUID pin template; `decode('NN','hex')` for bytea
  (n/a here; jsonb); FK cascade (n/a); replay safety
  (mirrored).
- `migrations/00079_deployment_overrides.sql` +
  `migrations/00082_apps_scaling_policy.sql` — the single-
  column `jsonb NOT NULL DEFAULT 'shape'::jsonb` precedent,
  no GIN index, application-layer validation.
- `migrations/00087_reserve_slot.sql` — canonical fence
  body — this PR uses that fence to hold slot 95 against PR #526.
- `cmd/apid/auth_facade.go::loadApp` (auth_facade.go:75-80) —
  IDOR-safe slug→App with `app.AccountID == acct.ID` predicate.
- `cmd/apid/handlers_secrets.go::setSecretRecipient` —
  `func() *age.X25519Recipient`; PR-A's envelope-seal helper
  reuses the same pattern for sidecar env.
- `pkg/logsanitize.Field` — for `name`, `image`, `type`,
  `cmd` shape. Sidecar env values NEVER.
- `pkg/state/queries.sql` (hand-coded SQL per ADR-017) —
  pgstore uses hand-coded SQL on the jsonb column.
- `pkg/state/migration_apply_walk_test.go` + `embed_test.go` —
  the contiguous-range gate; PR-A bumps to 1..96.
- `sdk-go-errors-hand-curated-subset.md` — the hand-curated
  subset pattern; PR-A mirrors the 8 new errors in
  `sdk/go/internal/api/errors.go`.
- `spec-sync-stale-embed-on-openapi-change.md` — re-run
  `make spec-sync` after the yaml change.
- `cross-pr-slot-gate-reservation-fence-pattern.md` — slot
  fence; PR-A plants `00095_reserve_slot.sql` against PR #526.
- `pkg/imaged/handler_image_build_test.go`'s
  `StatefulBaseImageDenylist` fixture — replicated in
  `Sidecar.Validate` via the same hash set; `pkg/api`
  imports the constant.

## Financial-model addendum (PREREQUISITE)

Per CLAUDE.md ("financial model is source of truth"), the
`ex44_faas_financial_model.xlsx` Hobby / Pro / Scale break-even
rows need a `sidecars: 1` (and `sidecars: 2`) scenario column
pinning GB-h BEFORE this PR merges. The column should pin:

- Hobby with one init + one sidecar at 64 MB each:
  `per_instance_billable_mb` = 256 + 64 + 64 + 8 = 392 MB.
  720h × 30 days × 392 MB / 1024 = ~6.91 GB-h/day =
  ~207 GB-h/month at the binary divisor (decimal-vs-binary
  ADR-061 will reconcile this). Verifies Hobby's 50 GB-h
  included ceiling is exceeded under sidecar usage; overage
  rate €0.01/GB-h = ~€2.26 overage at `sidecars: 2`.
- Pro with one init only: `per_instance_billable_mb` = 512 + 64
  + 0 + 8 = 584 MB. 720h × 30 × 584 / 1024 = ~12.3 GB-h/day =
  ~370 GB-h/month. Verifies Pro's 250 GB-h included ceiling is
  exceeded; overage ~€1.20.
- Scale with two sidecars (init + sidecar): `per_instance_
  billable_mb` = 1024 + 64 + 64 + 8 = 1160 MB. 720h × 30 ×
  1160 / 1024 = ~24.4 GB-h/day = ~735 GB-h/month at
  `max_concurrency = 20` × 1 instance = 735 GB-h ≪ Scale's
  1500 GB-h included ceiling. Verifies Scale customers fit
  inside their ceiling under the 2-sidecar cap.

The xlsx is git-ignored (`ex44_faas_financial_model.xlsx`
lives on the EX44 box only). The PR cannot merge until the
spreadsheet row is committed to the EX44 box out-of-band.

## Verification

Unit tests (no KVM):

- `pkg/api/dto_sidecars_test.go` (new) — table-driven
  `Sidecar.Validate` Accepts/Rejects (15 cases: good init,
  good sidecar, good pair, double init, double sidecar, len
  > 2, image-by-tag, bad name regex, port 65536, port -1,
  ram_mb 1024, stateful image, env too long, empty cmd
  element) + JSON round-trip via
  `DisallowUnknownFields`.
- `pkg/api/limits_test.go` (extend) — single
  `SidecarCapMax == 2` assertion + 4-case
  `SidecarAllowed()` loop.
- `pkg/api/errors_test.go` (extend) — 8 new constructor
  shape tests (HTTP code, body code, message presence).
- `pkg/state/memstore_*_test.go` (extend) — JSONB
  round-trip (`{nil, '', '[1,2,3]'}` preserves bytes).
- `pkg/state/pgstore_*_test.go` (extend) — JSONB
  round-trip + 2-cap rejection + nil→`[]` default
  fill.
- `migrations/00096_deployments_sidecars_test.go` (new) —
  pgtest (7 cases mirroring `00094_app_registry_
  credentials_test.go`).
- `pkg/state/migration_apply_walk_test.go` (extend) —
  bump range to 1..96.
- `migrations/embed_test.go` (extend) — bump N to 96.
- `cmd/apid/handlers_deployments_test.go` (extend) —
  passthrough coverage:
  - 4 cases of `TestCreateDeployment_AllPlansAllowSidecars`
    (Free / Hobby / Pro / Scale, 2 sidecars each → 201).
  - `TestCreateDeployment_3RejectsSidecars` →
    400 `ErrSidecarCapExceeded`.
  - `TestCreateDeployment_DigestPinning_RejectsTagReference` →
    400 `ErrSidecarInvalidImage`.
  - `TestCreateDeployment_StatefulDenylist_RejectsStatefulImage`
    (fixture hash from the imaged denylist test) →
    403 `ErrSidecarStatefulDenied`.
  - `TestCreateDeployment_SealFailure_503` (recipient nil →
    503 `ErrCapacity`).
  - `TestCreateDeployment_EnvNeverInAuditPayload` (capture
    slog + audit; plaintext env + sealed ciphertext both
    absent).
- `sdk/go/internal/api` test suite — re-run with the
  mirrored error constructors; compile must pass.
- `pkg/wire/metrics*_test.go` — assert no `schedd_sidecar_*`
  collectors exist in PR-A (PR-C owns the metric surface).

Integration:

- `make lint` — golangci-lint + custom checks must pass.
- `make test PKG=./pkg/api/...` — DTO + limits + errors.
- `make test PKG=./pkg/state/...` — store round-trip.
- `make test PKG=./migrations/...` — pgtest fixtures.
- `make test PKG=./cmd/apid/...` — handler passthrough.
- `make test PKG=./sdk/go/...` — SDK Go compile + tests.
- `make gen` — `sdk-gen` + `spec-sync` regen.
- `make sdk-check` — sdk-go error mirror gate.
- `make spec-check` — `api/openapi.yaml` ↔
  `pkg/apid/openapi.yaml` parity.

`make test-metal` and `make leakcheck` — NOT required for
PR-A (no runtime effect, no cgroup/netns/uid surface).
PR-B inherits these gates.

E2E: deferred to PR-C.

Cross-PR slot gate:

```sh
# PR-creation time check
ls migrations/ | grep -E '^[0-9]' | sort | tail -3
git log --all --oneline -- migrations/ | head -5
# Confirm 096 is free; if not, renumber-fence per the
# cross-pr-slot-gate-reservation-fence-pattern.md memory.
```

CI:

```sh
gh workflow run ci.yml --ref issue-463-sidecars-prA
```

Pre-merge checklist:

- [ ] Financial-model addendum row + scenario columns committed
      to the EX44 box `ex44_faas_financial_model.xlsx`
      (out-of-band ops commit; documented above).
- [ ] `docs/adr/README.md` table row for ADR-069 added on the
      PR.
- [ ] `migrations/00096_deployments_sidecars.sql` applied
      cleanly via `make test` on a fresh pgtest DB.
- [ ] Cross-PR slot gate: PR #526 holds slot 95 as a fence;
      PR-A plants its own `00095_reserve_slot.sql` and uses
      slot 96.
- [ ] `pkg/e2etest/harness.go::e2eMigrationTarget = 96` on the PR.
- [ ] `api/openapi.yaml` + `pkg/apid/openapi.yaml` parity
      (`make spec-check` green).
- [ ] `sdk-go/internal/api/errors.go` mirrors all 8 new
      error constructors (`make sdk-check` green).
