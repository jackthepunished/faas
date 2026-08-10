# ADR-089 · First-class build status endpoint (issue #741 / DEPLOY-PROV-6)

- **Status:** accepted
- **Date:** 2026-08-10
- **Issue:** #741 / DEPLOY-PROV-6
- **Supersedes:** the implicit "SSE-only + one-shot `GetDeployment` fallback" status-read pattern at `cmd/gregale/commands2.go:2208-2318` (`streamDeployLogs` + `pollDeploymentFinal`); the implicit "Postgres-only" operator-debugging path for "which builds are queued or running".
- **Related:** ADR-038 (origin of the `builds` table + the existing `/v1/builds/{id}/provenance` and `/v1/builds/{id}/sbom` routes), ADR-034 (scope rules — read surface uses `ScopesReadSurface`), spec §14 build lifecycle. `pkg/builderd/builderd.go:543-650` (`markSucceeded`/`markFailed` — the only status transition sources). `cmd/gregale/commands_builds.go` (the `gregale build provenance|sbom` subcommands this extends).

## Context

Today, the only way for a customer (or a CI system) to ask "is my build still running, or did it fail?" is to either:

1. Open an SSE stream against `GET /v1/deployments/{id}/logs?follow=1` (`pkg/api/logs.go:140-159` `StreamDeploymentLogs`; CLI wraps in `cmd/gregale/commands2.go:2218-2303` `streamDeployLogs`).
2. If the SSE breaks (server bounce, network drop, 10-min backstop per `pkg/api/logs.go:136-139`), fall back to a one-shot `GetDeployment` call (`commands2.go:2309-2318` `pollDeploymentFinal`) that does NOT loop.

There is no `gregale build <id>` — only `gregale build provenance <id>` (ADR-038 read half, post-mortem export) and `gregale build sbom <id>` (ADR-038 Phase 3, post-mortem blob). The two existing `/v1/builds/{id}/*` routes are AFTER-the-fact export surfaces, not lifecycle.

Operators currently need to log into Postgres (`select id, status, started_at, finished_at from builds where status='running'`) to answer "which builds are queued or running." CI integrations that want to fail-fast on build error have to scrape SSE, which is fragile (the SSE backstop is 10 minutes; a build that legitimately runs longer is indistinguishable from a broken stream).

The build state machine already has every column a new endpoint needs:

- `builds.status` — 4-state enum `queued|running|succeeded|failed` (`schema.sql:662` CHECK constraint; typed `BuildStatus` at `pkg/state/types.go:119-126`).
- `started_at`, `finished_at`, `enqueued_at` — `pgtype.Timestamptz` (nullable until the build starts/finishes).
- `failure_class` — low-cardinality enum `oom|timeout|user_error|infra` (`pkg/state/types.go:129-136`).
- `kind`, `source_bytes`, `log_path` — informational.

`BuildByID` already exists (`pkg/state/queries.sql:349-351` + `pkg/state/pgstore.go:4497-4519`). The new endpoint is a thin read-side wrapper.

## Decision

Add `GET /v1/builds/{id}` returning a `BuildResponse` DTO. Mount it in `cmd/apid/server.go` next to the existing provenance + sbom routes. Wire it through the Go SDK (Node + Python via regen). Add a `gregale build status <id>` subcommand (terminal-friendly text + `-j` for raw JSON). Replace `pollDeploymentFinal` with a proper `pollBuildStatus` loop that backs the SSE fallback.

### Sub-decisions

1. **Status enum stays at the existing 4 values** (`queued|running|succeeded|failed`). The issue's example mentions `cancelled`, but the schema CHECK constraint at `schema.sql:662` does not allow it; no transition code path exists in `pkg/builderd` (the only transitions are `markSucceeded`, `markFailed`, the reaper's `SweepStuckRunningBuilds`, and `RequeueBuild`). Adding `cancelled` requires a separate migration + builderd path + a `--cancel` CLI flag. Out of scope for this PR; a follow-up ADR can extend the CHECK if/when the cancel path lands.

2. **`BuildResponse` carries the columns the customer actually needs.** Status + timestamps + `failure_class` + `duration_seconds` (server-computed). The free-text `error_message` lives on `deployments.error_message` and is NOT mirrored here — clients that need the per-failure string call `GetDeployment(deployment_id)`. Keeps the response shape stable across all 4 statuses (the `omitempty` tags on `failure_class`, `started_at`, `finished_at`, `log_path`, `duration_seconds` make the JSON minimal for queued/running builds).

3. **`duration_seconds` is server-computed.** CI scripts shouldn't parse RFC3339 to do arithmetic. The server subtracts `finished_at - started_at` only when both `pgtype.Timestamptz` values are valid; the field is omitted otherwise (so a queued build shows nothing, a running build shows nothing, a succeeded build shows the elapsed time).

4. **`error_code` / `error_message` are NOT in the response** despite the issue's suggested shape. They are not columns on `builds`. The closest analog is `failure_class` (low-cardinality enum, intentional — distinguishes user-fixable errors from infra-side problems) plus `deployments.error_message`. Documenting the non-inclusion here so future contributors don't add them speculatively.

5. **Polling replaces the one-shot fallback.** `pollBuildStatus` replaces `pollDeploymentFinal` in `cmd/gregale/commands2.go`. Backoff: 1s base, capped at 5s, jittered ±20% to avoid thundering herd when many CI jobs exit SSE at the same instant. Default deadline 60s; CLI flag `--wait <SECS>` overrides. On deadline elapse or transient error the function returns `(zero, false)` so the SSE caller can still emit the existing "follow manually: gregale logs --deployment <id>" hint.

6. **Scope = `ScopesReadSurface`.** Same as the existing `/v1/builds/{id}/*` routes. No new `ScopeBuildRead` constant — the comment at `cmd/apid/server.go:802-803` already calls the existing scope the "build:read scope" semantically; we just inherit it. Every authenticated `/v1/builds/...` route uses the `authLimited(requireScope(ScopesReadSurface...))` chain (per-IP 10/min/IP per spec §11).

7. **SDK coverage gate auto-detects `GetBuildsId`.** `cmd/sdk-coverage/main.go::deriveMethodName` derives `Get<PathSegments>` from the route — for `GET /v1/builds/{id}` the natural form is `GetBuildsId` (mirroring the existing `GetBuildsIdProvenance` / `GetBuildsIdSbom`). No `methodRouteMap` entry needed. Node SDK gets a new `getBuild` method under `DeploymentsService` (the OpenAPI `tags: [deployments]` group pulls it there per existing convention); Python SDK gets `get_build.py` + `build_response.py`.

8. **Rollback: revert the 4 commits.** No DB migration. No schema change. `BuildByID` was already present. The SSE fallback reverts to the one-shot `pollDeploymentFinal` behavior — strictly less correct, but no customer-facing API breaks. The `pkg/api.BuildResponse` DTO is additive; existing SDK callers don't see it.

### File-by-file change list

| File | Change |
|------|--------|
| `pkg/api/dto.go` | ADD `BuildResponse` next to `BuildProvenanceResponse`. |
| `pkg/api/client.go` | ADD `GetBuildsId` (auto-derived name; no `methodRouteMap` entry). |
| `pkg/api/errors.go` | ADD `CodeBuildNotFound = "build_not_found"` + `ErrBuildNotFound()` constructor. |
| `api/openapi.yaml` | ADD `/v1/builds/{id}` path + `BuildResponse` schema next to existing builds block. |
| `pkg/apid/openapi.yaml` | regenerated via `make spec-sync`. |
| `cmd/apid/server.go` | ADD `GET /v1/builds/{id}` route registration. |
| `cmd/apid/handlers_ext.go` | ADD `getBuild` handler (IDOR chain `BuildByID → DeploymentByID → AppByID`) + `buildResponse` converter. |
| `cmd/apid/handlers_build_test.go` | NEW. Whitebox tests: OK / 404 / IDOR / 401 / rate-limit. |
| `pkg/api/client_method_sweep_test.go` | ADD `TestSweep_GetBuildsId`. |
| `cmd/gregale/commands_builds.go` | ADD `cmdBuildStatus` + `printBuildStatus` + dispatch case. |
| `cmd/gregale/commands2.go` | ADD `pollBuildStatus` + `terminalExitForBuild`; swap call site in `streamDeployLogs`. |
| `cmd/gregale/commands_builds_test.go` | ADD text-rendering tests for `cmdBuildStatus`. |
| `cmd/gregale/commands2_test.go` | ADD happy + deadline-elapse tests for `pollBuildStatus`. |
| `pkg/builderd/builderd_test.go` | ADD `BuildByID` round-trip from `markSucceeded`/`markFailed`. |
| `sdk/node/src/generated/services/DeploymentsService.ts` | regenerated via `make sdk-gen-node`. |
| `sdk/python/faas_sdk/api/deployments/get_build.py` + `models/build_response.py` | regenerated via `make sdk-gen-python`. |

## Consequences

### Positive

- CI scripts can fail-fast on build error via `gregale build status <id> -j | jq -e '.status == "succeeded"'` (or the equivalent SDK call). No SSE scraping.
- The SSE fallback is no longer one-shot: a fast build that races the SSE open or a build that legitimately runs longer than the SSE backstop is now waited on for up to 60s before falling back to "follow manually".
- Operators get a first-class API surface for build status instead of needing direct Postgres access.
- The new DTO is additive — no existing customer-facing API changes.
- The `duration_seconds` field removes the need for CI scripts to parse RFC3339 (a class of timezone + locale bugs).

### Negative

- The new endpoint is a tiny additional load on the IDOR chain (3 SELECTs per call: builds, deployments, apps). All three are indexed; with the per-IP 10/min/IP rate limit, the load is bounded.
- The CLI's polling fallback can now wait up to 60s by default. A CI script that hits the SSE fallback path may take longer to exit on a stuck build. The `--wait` flag overrides; CI scripts that want fast-fail can pass `--wait 5` (or `0` to disable waiting entirely — falls back to the immediate "follow manually" hint).
- Adds one route to the spec-check AST gate. New drift surface: any future change to `BuildResponse` field tags must update the OpenAPI schema, or `make spec-check` fails.

### Neutral

- `cancelled` stays out of scope. A follow-up ADR can add it when a `--cancel` path lands; the migration would be additive (new CHECK value, new transition code, no breaking change to the response shape).
- `error_message` stays on `deployments`, not `builds`. Customers who want the per-failure string call `GetDeployment(id)` — a single extra round-trip but a cleaner schema.

## Verification

### Local

- `make sqlc-check` — green (no sqlc change).
- `make spec-check` — green (new DTO + route + schema detected by `cmd/apid/spec_compliance_test.go::TestSpecCompliance`).
- `make lint` — green (`gofmt -l` repo-wide; new files gofmt-clean).
- `go test ./pkg/api/... ./pkg/builderd/... ./cmd/apid/... ./cmd/gregale/...` — green.

### CI

- `lint + build` — green.
- `unit tests (pure Go shard 1/2 + pg shard 1/2)` — green. New whitebox handler tests + SDK sweep test live in pg shard 2.
- `spec-check` — green. New route + schema + DTO detected by AST.
- `sdk-go` (hand-written, gated by `make sdk-check`) — green.
- `sdk-node (gen-check + smoke + unit)` — green. Regenerated `DeploymentsService.getBuild` + `BuildResponse` model.
- `sdk-python (gen-check + smoke + unit)` — green. Regenerated `get_build.py` + `build_response.py` model.
- `daemonunit-check (generated drift)` — green. No daemon unit change.
- `migrations (contiguity + apply)` — green. No migration.
- `e2e (4 shards)` — green. The blackbox e2e stays untouched; `gregale deploy` still works (the SSE fallback is now backed by the new endpoint, behavior is identical for happy-path customers).

### Acceptance gate

```
$ go test ./pkg/api/... -run TestSweep_GetBuildsId -v
--- PASS: TestSweep_GetBuildsId

$ go test ./cmd/apid/... -run TestGetBuild -v
--- PASS: TestGetBuild (5 subtests: OK, NotFound, IDOR_OtherAccount, RequiresAuth, RateLimit)

$ go test ./cmd/gregale/... -run 'TestCmdBuildStatus|TestPollBuildStatus' -v
--- PASS: ... (text rendering + happy + deadline elapse)

$ make spec-check && make sdk-check
ok  github.com/onebox-faas/faas/cmd/apid
ok  github.com/onebox-faas/faas/cmd/sdk-coverage
```

### Rollback

Revert the 4 commits in reverse order. No DB migration. No schema change. `BuildByID` was already present. The SSE fallback reverts to the one-shot `pollDeploymentFinal` behavior — strictly less correct, but no customer-facing API breaks.
