# ADR-053 — Deploy-time overrides for OCI image deploys (issue #460)

Status: Accepted, 2026-07-31. Owner: @poyrazK. Related: issue #460
("CONTAINERS: deploy-time overrides for customer OCI image deploys
(entrypoint/cmd/env/port/healthcheck)").

## Context

The customer-facing OCI image deploy path (`POST /v1/apps/{slug}/deployments`,
JSON branch in `cmd/apid/handlers.go:122-222`) accepts only a
digest-pinned reference (`req.Image`) and inherits `Cmd`, `Env`,
`WorkingDir`, `User` from the OCI image config (`pkg/oci/image.go:96-111
ManifestFromConfig`). A customer who wants a different `--port`, a
staging-only env, or a `node ./server.js` override has to rebuild and
re-push the image. That rebuild invalidates the snapshot, costs a cold
wake on the next request, and is the same friction the `app_envs`
table (ADR-045) closed for runtime config.

The shape of the gap is Fargate-shaped and is the closest cross-cutting
override currently missing from the customer surface. The current
Wave 0 stateless contract (ADR-051 characterization boot + ADR-045
mutable env + ADR-031/033 egress allowlist) is load-bearing; this ADR
sits inside that envelope and does not widen it.

Two facts about the existing codebase shape the design:

1. **The port plumbing is half-built today.** `AppManifest.Port` and
   `AppManifest.Healthz` are declared at `pkg/api/appmanifest.go:25-48`
   and `EffectivePort()` is implemented, but the host path
   (`pkg/netns/config.go:36-86`, `pkg/fcvm/alloc.go:38-53`,
   `pkg/fcvm/vmm.go:1322-1340 waitReady`,
   `pkg/vmmdgrpc/proto.go ForwardHTTPRequest`,
   `pkg/vmmdgrpc/forward.go:188,205-213`, every
   `guest/runners/*/main.go`) hardcodes `:8080` and never reads the
   manifest. Likewise `manifest.Healthz` is declared but never read;
   `waitReady` is a bare TCP accept, not an HTTP probe. Issue #460
   does not introduce a new guest contract; it activates the existing
   one for the override path.

2. **Env quota is per-app, not per-deployment.** ADR-045's
   `Limits.EnvVarsMax` and `Limits.EnvValueMaxBytes` already exist
   (`pkg/api/limits.go:98-106`). The override reuses these caps —
   adding a per-deployment quota would let a customer bypass the
   per-app quota by issuing many deploys.

## Decision

**1. New `CreateDeploymentOverrides` DTO in `pkg/api/dto.go`, with six
fields and `Validate(limits api.Limits) *Problem`.** The fields are:

| Field | Type | Validation |
|---|---|---|
| `entrypoint` | `[]string` | non-empty if present; each element non-empty |
| `cmd` | `[]string` | non-empty if present; each element non-empty |
| `env` | `map[string]string` | key per `ValidateEnvKey`; per-value ≤ `Limits.EnvValueMaxBytes` |
| `env_secrets` | `map[string]string` (refs `secret:<name>`) | key per `ValidateEnvKey`; ref matches `secret:[A-Z][A-Z0-9_]*` |
| `port` | `int` | 1..65535; 0 means "absent / fall back to image default" |
| `healthcheck` | `{path, interval_s, timeout_s, retries}` | path must start with `/`; defaults: interval 5s, timeout 2s, retries 3 |

**`env` + `env_secrets` share `Limits.EnvVarsMax`** (no new column on
`pkg/api/limits.go`). The total `len(env) + len(env_secrets)` is
checked against the cap, so a customer cannot bypass the per-app
quota by mixing the two.

**No new field is added to `CreateDeploymentOverrides` beyond this
list.** Volume mounts, log destination, sidecars, ulimits, scaling,
tags, remote-secret refs, pull-time registry auth — each is a
separate ADR that explicitly supersedes or extends this one.

**2. New JSON shape on `CreateDeploymentRequest`.** A single optional
`overrides` field carries the override; existing JSON bodies (image
only) continue to validate and behave exactly as today. Validation
order in the handler:

1. `isDigestPinned(req.Image)` (existing — 400 `CodeImageRequired`).
2. `req.Overrides.Validate(api.MustLimitsFor(acct.Plan))` if present
   (new — 400 `CodeValidation`).

A failed override validation NEVER silently drops the override — the
whole request 400s. This is the only correct behaviour; customers
who specified an override expect it to apply.

**3. Six new columns on `deployments` (NOT on `apps`).**
Migration `00076_deployment_overrides.sql` adds, replay-safe
(`ADD COLUMN IF NOT EXISTS`):
```
override_entrypoint   text[]
override_cmd         text[]
override_env         jsonb
override_env_secrets jsonb
override_port        int
override_healthcheck jsonb
```
Per-deployment (not per-app) so re-deploying the same image with a
different port is a normal flow without a separate app update. JSON
validation lives in apid, not the DB — the jsonb columns accept any
JSON shape, but the handler never writes one that hasn't passed
`CreateDeploymentOverrides.Validate`.

**4. `DeploymentResponse` echoes the override shape, values NEVER.**
The response carries `override_entrypoint`, `override_cmd`,
`override_env_keys`, `override_env_secrets_keys`, `override_port`,
`override_healthcheck` — but env values are NEVER echoed. The shape
mirrors `AppEnvListResponse` at `pkg/api/env.go:47-61`: customers
see "which keys are set" without ever seeing the values. Logs also
never carry env values (mirror `logsanitize.RedactValue` from
ADR-045 §Decision 6).

**5. Three-PR rollout.** The change is too large for one PR; it splits
along review-load lines:

- **PR A (this PR scope)** — ADR + migration + DTO + handler + tests.
  Persists the overrides. Customers see the override echo in GET
  responses, but the override does NOT yet take runtime effect at
  boot. Approx 300-500 LoC. **Honest contract**: the field is wired
  end-to-end at the API and DB layer; runtime wiring is a follow-up.
- **PR B** — imaged layer injection: when `ManifestFromConfig` builds
  the `api.AppManifest`, layer overrides on top of the OCI-derived
  manifest (entrypoint override → replaces argv; env override → merges
  with image env, override wins on key collision; port/healthz/user
  override → replaces). The rest of the staging pipeline is unchanged
  because the manifest struct is unchanged — only its contents differ.
- **PR C** — port plumbing: thread `Port` through `netns.Config`,
  `fcvm.Lease`, `vmm.waitReady`, `vmmdgrpc.ForwardHTTPRequest`,
  `scheddgrpc.Wake`, and bind `:EffectivePort()` in the five runners.
  Touches §11 netns + ADR-009 (identical inner world) — the seam is
  safe because distinctness is per-netns/IP/uid/RNG, not per-port, and
  each per-app port sits inside its own netns.

PR A ships alone. PR A + PR B together are sufficient to make
entrypoint/cmd/env take effect at boot (port + healthcheck still
need PR C). PR C closes the loop. None of them require the others to
merge first; the contract is honest at each step.

**6. Healthcheck override persists but the probe stays a TCP accept.**
The override shape is persisted and surfaced in the response, but
`pkg/fcvm/vmm.go::waitReady` continues to be a bare TCP accept on the
configured port. The HTTP-probe variant of `waitReady` would touch
§6.3 row 4 (40 ms readiness budget) and is its own ADR + property test.

**7. Healthcheck path as a metric label is deferred.**
`gateway_request_duration_seconds` already labels `{app, class}` (see
`pkg/gateway/metrics.go:105,242`); a `path` label would blow up
cardinality unless the override's path set is bounded. The path is
persisted as a deployment row field and surfaces as metadata, not as
a label, until the cardinality argument is made.

## Consequences

- `api/openapi.yaml` gains a `CreateDeploymentOverrides` schema
  component (next to `CreateDeploymentRequest` at ~line 3942);
  `CreateDeploymentRequest` references it. The mirror at
  `pkg/apid/openapi.yaml` is regenerated via `make spec-sync` per
  Makefile:351-363. The OpenAPI `description` for the override shape
  pulls its docs URL from `wire.DocsHost + "/deploy-overrides"`, not a
  literal.
- `pkg/api/dto.go` gains `CreateDeploymentOverrides` and the six
  optional fields on `DeploymentResponse`. The SDK aggregator
  (`make sdk-gen`) regenerates the Node + Python SDKs; the Go SDK
  flow is via `make sdk-check` (the Go SDK is hand-written per
  `pr327-public-go-sdk-surface`).
- `pkg/state/state.go::Deployment` gains the six override fields with
  `json.RawMessage` for the jsonb columns and `[]string` for the
  text[] columns. `pkg/state/pgstore.go::CreateDeployment` extends its
  INSERT to write them. `pkg/state/queries.sql` (sqlc) needs the new
  columns scanned in `GetDeployment` and `ListDeployments` to satisfy
  `scanDeploymentInto` — the canonical column-list constant pattern
  from PR #440 (`scanApp` + `appsSelectColumns`) is mirrored here.
- `cmd/apid/handlers.go::createDeployment` (JSON branch) gains the
  `Validate` call and the `state.Deployment` field assignments. The
  multipart branch is unchanged — multipart deploys (source tarballs
  / dockerfile) take their manifest from the build pipeline, not
  from request-body overrides.
- `cmd/apid/handlers.go::deploymentResponse` populates the six echo
  fields. Env values are never copied into the response — only keys
  (slice of strings derived from the jsonb map).
- `pkg/api/limits.go` is unchanged in PR A. No financial-model
  reconcile is needed because env + env_secrets reuse `EnvVarsMax`
  and `EnvValueMaxBytes` exactly. PR B and PR C do not change limits
  either.
- `cmd/apid/handlers_test.go` gains `TestCreateDeployment_Overrides_*`
  table-driven cases covering: valid overrides happy path; empty
  entrypoint → 400; env key violating `ValidateEnvKey` → 400; env
  count > `Limits.EnvVarsMax` → 400 with `ErrEnvVarValueTooLarge`
  shape; port = 0 / port = 70000 → 400; env_secrets ref not matching
  `secret:[A-Z][A-Z0-9_]*` → 400; healthcheck.path not starting with
  `/` → 400; response never echoes env values.
- `cmd/e2e/deploy_wake_metal_test.go` gains a `deploy-with-overrides-
  roundtrip` subtest using the `e2etest.Start(t, pool, e2etest.DeployWake)`
  harness at `pkg/e2etest/harness.go:75-122`. PR A ships the API
  round-trip subtest (POST overrides → GET deployment → assert echo);
  PR B/C add the boot-effect subtests.
- `migrations/00076_deployment_overrides.sql` is replay-safe via
  `ADD COLUMN IF NOT EXISTS` — `TestNewMigrationsAreReplaySafe`
  (PR #377 / ADR-041) passes without further work. The migration has
  no DOWN-block complication: the columns are nullable, so a DOWN
  that drops them is safe for any row that didn't write them; rows
  that did write them in production would 404 their override fields
  on rollback, which is the correct degraded behaviour.
- The handler's `s.audit.Emit("app.deployed", ...)` payload gains a
  `has_overrides bool` field so dashboards can render "this deploy
  pinned overrides" without parsing the response shape. The values
  are NEVER in the audit payload (ADR-045 §Decision 6 mirror).
- New SDK method surface: `CreateDeployment` on `pkg/api.Client`
  gains the optional `Overrides` field on the request struct; no new
  method is needed because the route is unchanged.
- `cmd/sdk-coverage/main.go::methodRouteMap` is unchanged (the route
  was already mapped to `CreateDeployment`); the new optional fields
  flow through the existing `CreateDeploymentRequest` DTO and the
  generator picks them up automatically.

## Rejected alternatives

- **Per-deployment `apps.listen_port` column instead of per-deploy
  override column.** Couples port to the app, not the deploy. A
  customer who redeploys the same image with a different port would
  have to PATCH the app AND redeploy — extra round trip for a normal
  flow. Per-deployment override columns keep the deploy self-contained.
- **New `Limits.EnvSecretsMax` tighter than `Limits.EnvVarsMax`.** No
  empirical reason to differentiate — sealed secrets already have
  their own per-app quota (`SecretCountMax` ≤ 100 across plans,
  `pkg/api/limits.go:95-96`). Adding a tighter cap for override
  env_secrets would require a financial-model reconcile for marginal
  benefit. The env + env_secrets sharing rule keeps the customer-
  facing surface simple.
- **Live HTTP probe in `vmm.waitReady` for the healthcheck override.**
  Would couple the override to a §6.3 row 4 (40 ms readiness) change
  and need new property tests. Defer to a follow-up ADR; PR A persists
  the field without taking runtime effect.
- **`healthcheck.path` as a new label on `gateway_request_duration_seconds`.**
  Cardinality risk — a customer with 100 apps × N healthcheck paths
  blows up the metric series. Defer until the path set is proven
  bounded; PR A surfaces the path as a deployment-row field, not a
  label.
- **Block PR A until PR C lands.** PR A is honest about its limits:
  it ships the contract (persisted overrides echo in GET responses).
  Blocking would couple a customer-visible API surface to a risky
  host-side wiring change, which is the wrong coupling direction.
- **Allow `mountPoints`, `ulimits`, `logConfiguration`, `secrets`
  from a remote store, or any other field on the override object.**
  The field list is frozen in this ADR. Each future field requires a
  follow-up ADR that explicitly extends this one — the same review
  gate that ADR-045 §Decision 1 imposed on `app_envs` scope widening.
