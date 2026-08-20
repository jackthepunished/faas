# ADR-110 · Declarative split-box deployment manifest

- **Status:** **Accepted** (revised 2026-08-16, amended 2026-08-20) (PR-0 of the issue #911 PR cluster)
- **Date:** 2026-08-15
- **Decision:** Adopt a single, versioned, declarative deployment
  manifest as the source of truth for every host in a multi-box
  Gregale fleet. The manifest is a YAML document at
  `deploy/manifest/splitbox.yaml` (operator-supplied) plus a typed
  schema at `pkg/manifest/` (in tree). Every reader of the manifest
  — the validator (`gregalectl manifest validate`), the renderer
  (PR-2), the release bundle installer (PR-3), the doctor
  preflight (PR-4), and the metal harness (PR-6) — consumes the same
  `pkg/manifest` package. **One canonical validation path**. The
  legacy `deploy/controlplane/bootstrap.sh` is retired (PR-1) once
  the per-secret replacement (`gregalectl secrets init`, PR-X) and the
  replacement ansible roles land.
- **Why:** Issue #911 records that a 10+ hour live-debugging session
  on the GCP split-box deployment was caused by configuration drift
  between the boxes — the code assumed a multi-box install while the
  fleet was partially wired as a single-box install, and every
  operator-driven fix (TOML edits, hostname → VPC address, cgroup
  remount shim, file ownership, image refresh) had to be applied
  by hand. The codebase already has partial multi-box scaffolding
  (`pkg/role`, `pkg/pki.RolesForBox`, `pkg/wire.PGNodeVerifier`,
  `deploy/ansible/host_vars/faas-fsn-{1,2}.yml`) but the deployed
  machines weren't generated from it. The fix is not "more ansible
  roles" — it's inverting the dependency so that one declarative
  manifest drives every provisioning step, with the existing scripts
  becoming thin renderers that consume the manifest.
- **Consequences:**
  - **Schema (PR-0, this PR):** new `pkg/manifest/` package with a
    typed schema covering hostnames, roles, endpoints, overlay,
    DNS, PostgreSQL, release digests, storage roots, cgroup
    requirements, and PKI. Schema is SemVer (currently 1.0.0);
    major-version bumps are breaking (rename, mandatory field,
    tightened enum); minor + patch are backward-compatible.
  - **Canonical validation path (PR-0):** every reader of the
    manifest consumes `pkg/manifest.Manifest.Validate`. Issue #911's
    "completeness contract" explicitly requires this; the
    `cmd/gregalectl manifest validate` subcommand is the operator
    surface, and `make lint-manifest` is the CI gate. There is no
    "third-party" validator that disagrees with the canonical one.
  - **TOML table-placement catalog (PR-0):** the bug at
    `deploy/ansible/roles/vmmd_service/files/vmmd.toml.example`
    lines 33-40 (duplicated `tls_*_path` cluster declared inside
    `[compute_node]` — the canonical location is the top-level
    `tls_cert_path` / `tls_key_path` / `tls_ca_path` group) is
    the load-bearing failure mode
    issue #911 calls out. The schema's `pkg/manifest/toml_check.go`
    catalog is the source of truth for which key belongs to
    which TOML table. The renderer (PR-2) consumes the same
    catalog, so the renderer and the validator cannot drift.
  - **Migration slots reserved (PR-0):** `migrations/00266*.sql`
    and `migrations/00267*.sql` are reservation fences added in
    PR-0 per the team's slot-race posture (see MEMORY index
    entries on "migration gates collision" and "cross-PR slot
    precheck"). The bodies land in PR-3a (the `compute_nodes`
    release columns + the `release_bundles` table).
  - **Renderer (PR-2):** `gregalectl manifest render --role=…`
    consumes the manifest and emits `/etc/faas/*.toml`,
    systemd units, tmpfiles (including `/run/faas/stream`),
    cgroup v2 `subtree_control` (the load-bearing gap — only
    `deploy/lima/run-metal.sh:84` writes `subtree_control` today),
    and PKI leaves via `pkg/pki.RolesForBox()`. Idempotent
    (hash-match short-circuit); atomic publication via tmpfs `mv`.
  - **Doctor (PR-4):** `gregalectl doctor` is the operator-facing
    preflight that surfaces every drift the issue's
    "must report actionable failures for" list names. The doctor
    consumes the manifest schema and the rendered TOML catalog;
    the manifest drives the checks.
  - **Release bundle (PR-3):** `gregalectl release bundle --git-sha <sha>`
    produces a content-addressed tuple at
    `/opt/faas/releases/<id>/` containing the binaries, the
    rendered config hash, the migration version, the FC/kernel
    hashes, the per-host digests, and the manifest hash. The
    install path is idempotent + digest-verified.
  - **Bootstrap secrets (PR-X):** `deploy/controlplane/bootstrap.sh`
    is the only writer today for `deploy_ed25519` (the CD
    deploy-key), `host.age`, `session.key`, and the storage-box
    `rclone.conf` / `box-age-key` files. PR-X ships
    `gregalectl secrets init` (env-var → canonical paths) plus the
    parallel ansible roles. Once PR-X lands, PR-1 deletes
    bootstrap.sh.
  - **Metal harness (PR-6):** `deploy/lima/faas-metal-splitbox.yaml`
    is the two-role Lima fleet that runs the issue #911
    acceptance chain under `make metal-lima-splitbox`. The
    harness consumes the same manifest the renderer consumes,
    so the dev loop and the production deploy path cannot diverge.
  - **Issue #911 acceptance criteria mapping:**
    - "Fresh control-plane + compute-node pair provisions from
      empty machines." → PR-2 + PR-3 + PR-X.
    - "No manual host-file edits, TOML edits, direct SQL
      repairs, or ad-hoc binary copies." → PR-2 + PR-3a + PR-5.
    - "`doctor` passes on both boxes." → PR-4 + PR-6.
    - "Compute node remains active through multiple heartbeat
      cycles." → PR-5 (the `pkg/wire/pgverifier.go` receiver
      fix at the `Run` loop).
    - "mTLS handshakes without manual DB row insertion." →
      PR-3a + PR-2.
    - "CLI deploy + cold wake + HTTP 200." → PR-6.
    - "Same flow passes in local metal harness before fleet
      rollout." → PR-6 (the gate).

## Schema

Every required field is documented in `pkg/manifest/manifest.go`.
The loader refuses non-YAML files (TOML is explicitly rejected, per
the same convention as `pkg/gregalemanifest/manifest.go` — silent
ignoring would let customers think their manifest was applied).
The schema's TOML table-placement catalog is at
`pkg/manifest/toml_check.go`.

## Out of scope (explicit, v1.1+)

- **Vault integration for secrets** — PR-X's `gregalectl secrets init`
  accepts base64-encoded env vars. A Hashicorp Vault / AWS Secrets
  Manager integration is a v1.1 follow-up; the issue doesn't
  mention a vault.
- **Operator-facing manifest editor** — the v1 surface is a YAML
  file plus `gregalectl manifest validate`. A `gregalectl manifest
  edit` interactive surface is a v1.1 follow-up.
- **Schema auto-migration** — when a manifest's schema_version
  is older than the running binary's SchemaVersion, the
  validator flags the diff but does not auto-rewrite. A migration
  helper is a v1.1 follow-up.
- **Per-host overlay provider** — the manifest declares one
  provider for the whole fleet. Per-host overlay provider is a
  v1.1 follow-up.
- **Multi-region** — the manifest models a single region. A
  `region` field per host is a v1.1 follow-up (ADR-053 covers the
  per-node capacity signature, but the deployment manifest
  doesn't yet model cross-region).
- **Schema-evolution policy beyond the previous major** — when
  the schema bumps to v2.0.0, the previous major is supported
  for one release cycle. Longer-term deprecation policy is a
  v2 follow-up.

## Migration slot reservation

PR-0 reserves slots 00266 and 00267 with no-op `select 1;` bodies
per the `migrations/00056_reserve_slot.sql:1-50` pattern. The
bodies land in PR-3a:

- `00266_compute_nodes_release.sql` — `ALTER TABLE compute_nodes
  ADD COLUMN release_id text, manifest_hash text,
  host_certificate text, cert_fingerprint text, role text,
  generation int` (all nullable per the `00069` `region`/`zone`
  precedent).
- `00267_release_bundles.sql` — new `release_bundles` table
  recording `id`, `git_sha`, `manifest_hash`, `daemon_hashes
  jsonb`, `created_at timestamptz`, `applied_at timestamptz`.

## Cross-references

- Issue #911: "Make split-box deployment declarative and eliminate
  manual fleet configuration drift."
- ADR-025: cross-box mTLS — the original Tier 1 gate.
- ADR-052: PeerCN — the load-bearing chain + SAN + EKU + PeerCN
  primitive.
- ADR-056: `pkg/wire.PGNodeVerifier` — the per-CN handshake hook.
- ADR-070: Tier A7 edge split — the gatewayd-public /
  gatewayd-internal separation that completes the multi-box
  migration.
- ADR-083: active-passive HA — the next multi-box surface above
  the Gate-B primitives.
- ADR-092: Gate-B cross-box mTLS hardening — the operational
  scaffolding this ADR builds on top of.
- `pkg/role/role.go`: per-daemon box-role gate.
- `pkg/pki/pki.go`: `RolesForBox` partition.
- `pkg/wire/pgverifier.go`: the receiver fix in PR-5.
- `docs/runbooks/multi-host-rollout.md`: the operator narrative
  this ADR replaces.
- `docs/runbooks/manifest-renderer-cutover.md` (PR-7): the
  cutover runbook from legacy single-box to this world. The
  canonical operator narrative for first-time deployment.
- `docs/ops/gregalectl-operator-quickstart.md` (PR-7): the
  one-page operator reference; install `gregalectl`, bootstrap,
  write a manifest, validate, render, install, doctor.
- `deploy/lima/faas-metal-splitbox.yaml` (PR-7): the two-role
  Lima fleet that runs the issue #911 acceptance chain under
  `make metal-lima-splitbox`. The harness consumes the same
  manifest the renderer consumes, so the dev loop and the
  production deploy path cannot diverge.

## Amendment 1: Error-explanations cluster (spec §6.4 amendment 1)

This ADR is the closest umbrella for the error-explanations
cluster because it covers platform plumbing (Problem wire
shape, RFC 7807 codes, customer surfaces) — the same surface
set the cluster extends. No new ADR file is filed; the
amendment lives here, inline, so all customer-facing wire-shape
changes remain in a single canonical doc.

**Decision:** widen the `pkg/api.Problem` envelope with 4
optional fields (`Hint`, `Why`, `Fix`, `RelevantLogs`) plus a
`LogExcerpt` companion type; add 9 new RFC 7807 stable codes
covering the source-side failure modes that today produce only
raw `fmt.Errorf` strings or the catch-all `CodeDeployFailed`
422. Persist the same prose on the `deployments` row
(`migrations/00290` adds `error_hint`, `error_why`, `error_fix`,
`error_relevant_logs jsonb`) so post-mortem retrieval via
`gregale inspect <slug> --errors` surfaces the same 5-line
shape the live `Problem` emits.

**Catalog:** the customer-facing prose lives in a single
static catalog at `pkg/whycopy/` (table-driven, ~150 LoC).
Each row maps one stable `Code…` to Title/Hint/Why/Fix/
DocsURL + an optional per-code Observed renderer that templates
the observed value into Why/Fix. Detection sites call
`whycopy.Decorate(p, code, observed)` after the constructor
so the wire `Problem` carries the full block on every code
path.

**CLI surfaces:**

- `cmd/gregale/commands.go::renderAPIError` — extended to
  render Hint/Why/Fix/RelevantLogs in the standard order; legacy
  3-line shape preserved when the new fields are empty.
- `cmd/gregale/commands_doctor.go` — `gregale doctor [path]`
  preflight scans cwd for the source-side failure modes.
- `cmd/gregale/pack.go::runPackPreflight` — warn-only
  preflight during `gregale deploy`.
- `cmd/gregale/commands2.go::runLogs --explain` — 4-line
  summary on stream end.
- `cmd/gregale/commands_inspect_errors.go` — post-mortem leaf.

**Tripwires (every new code MUST satisfy):**

- `cmd/gregale/lint_tripwires_test.go::TestEveryCodeHasWhycopyEntry`
  — every `Code…` constant has a matching `whycopy` row.
- `pkg/whycopy/whycopy_test.go::TestDecorate_AllCodesHaveProse`
  — every catalog row has non-empty Title/Hint/Fix (Hint
  ≤200 bytes, Why ≤512 bytes).
- `cmd/gregale/lint_tripwires_test.go::TestLintTripwire_NoGlyphLiteralOutsideOutput`
  — every customer-facing glyph centralised in
  `cmd/gregale/output.go`.
- `cmd/gregale/lint_tripwires_test.go::TestLintTripwire_NoLiteralDocsDomainEverywhere`
  — every docs URL routes through `wire.DocsHost`.

**Wire-shape impact:** the 4 new fields carry `omitempty` on
the `pkg/api.Problem` struct, so every existing problem+json
site (~1,236 emitters) keeps its current shape unchanged.
SDK + OpenAPI regen handle the rest mechanically.

**Migration slot:** `migrations/00290` (cluster fences
`migrations/00288_reserve_slot.sql` + `00289_reserve_slot.sql`
mirror PR #910's slot-claim pattern per
`cross-pr-slot-fence-reservation-fence-pattern.md`).

**Out of scope (noted as followups):**

- Dashboard template `pkg/dashboard/views/error_explanation.go`
  (separate PR — `pkg/dashboard/` is a different surface area
  than the wire-shape plumbing this ADR amends).
- Three runtime detection points (listening_addrs →
  app_loopback_bound; cgroup.events → app_runtime_oom;
  reposcan env-var scan → env_var_missing) — already noted
  in commits `ac7507e2` and `f0bda4f5` as followups.
- Per-app custom error templates + i18n — keep the catalog
  platform-side for v1.

## Amendment 2: Error-explanations cluster surfaces (spec §6.4 amendment 2 — Cluster A follow-up to PR #987)

Amendment 1 closed detection → wire → DB → CLI render. The audit
[`memory/error-explanations-gap-audit-2026-08-18.md`](../memory/error-explanations-gap-audit-2026-08-18.md)
moved 1.5/9 → 8/9; amendment 2 closes the remaining 1 + 1 to push
the cluster to 9/9 + wire-shape forward-compat for `app_healthz_unauthorized`.

**Decision:** three targeted seams, no new wire-shape for the
detection side, no new ADR file (this amendment stays in ADR-110
to keep customer-facing wire-shape changes in a single canonical doc).

1. **Dashboard rendering of the persisted prose.** `pkg/dashboard/templates/deployment_detail.html`
   gains a conditional `<section class="error-explanation">` block
   gated on `{{ if .Data.Deployment.ErrorCode }}`. The block renders
   the 5 prose fields (`ErrorCode`/`ErrorHint`/`ErrorWhy`/`ErrorFix`/
   `ErrorRelevantLogs`) plus a docs link. The 5 fields are projected
   from the `state.Deployment` row in
   `cmd/apid/handlers_dashboard.go::dashboardDeploymentItem`. Legacy
   pre-amendment-1 rows render unchanged (the conditional gate is
   empty). Scoped CSS rules under `.error-explanation` parent
   selector so no global bleed across pages. Tests:
   `pkg/dashboard/dashboard_test.go::TestRender_DeploymentDetail_StatelessViolation`
   + `…_LegacyRowRenders` (the latter asserts the section is absent
   when `ErrorCode=""`).
2. **Pre-upload doctor gate.** `cmd/gregale/commands2.go::cmdDeployTarball`
   adds `--doctor-strict` (NOT `--strict` — already taken by the
   `--diff` deploy-diff cluster at `commands2.go:838`). The flag
   runs `runDoctorChecks(cwd)` BEFORE any HTTP / pack call.
   Error-class findings exit 1 with the doctor report on stderr;
   warnings render + continue. Implementation uses the
   `cmd/gregale/commands3.go::osStderr` writer seam so tests can
   capture the rendered prose. New helpers on `doctorReport`:
   `HasErrors()` (hard-fail signal) and `HasWarnings()` (render +
   continue). Flag-name scoping guarded by the new
   `cmd/gregale/lint_tripwires_test.go::TestLintTripwire_DoctorStrictMutex`
   tripwire, which fails on any new unscoped `--strict` Bool/String
   declaration outside the two documented call sites
   (`commands2.go:838` for `--diff`, `commands_doctor.go:124` for
   `gregale doctor`).
3. **`app_healthz_unauthorized` wire-shape forward-compat.** The
   guest-init → vmmd probe wire (`guest/init/liveness_linux.go::livenessResp`
   + `cmd/vmmd/liveness_recv.go::livenessResponseBody`) gains a
   `WWWAuthenticate string` field with `omitempty` JSON. Today's
   discriminator is closed-set (any 401/403 → `livenessOutcomeUnauthorized`),
   but the new field lets a future platform-side probe auth PR read
   the realm without another wire-shape bump. The closed-set
   comments on `pkg/fcvm/metrics.go:324` and
   `cmd/vmmd/liveness_recv.go:67-87` are updated from 5 to 6 values.
   New unit tests in `cmd/vmmd/liveness_recv_test.go` cover
   `livenessOutcomeUnauthorized` (counted, classified, reset on
   success, forbidden arm). No production behavior change.

**Migration slot:** no new migration needed. Cluster A is
wire-shape + UI + CLI gating only; it does not introduce new
state. The dashboard projection consumes the 5 columns added by
`migrations/00290` in amendment 1.

**Out of scope (noted as followups):**

- Live-app doctor probes (`--app=SLUG` needs a gateway endpoint — Cluster C).
- i18n / locale-aware `whycopy` lookup (Cluster B).
- `pg_get_log_archive(deployment_id, since=failure_ts)` lookup (Cluster B).
- Build-stream `failure_class` UX coverage (Cluster B).
- Renaming `--diff --strict` to free up `--strict` (out — would break scripts).
- `realm="customer"` probe-auth discriminator (Explore agent finding A.1:
  the platform doesn't auth the guest-init probe, so the collision doesn't
  exist today; the wire-shape bump is forward-compat only).
