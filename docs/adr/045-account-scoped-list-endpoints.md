# ADR-045 · Account-scoped list endpoints (issue #393)

- **Status:** accepted
- **Superseded (in part, PR-E):** prose referred to the monolithic
  `cmd/gatewayd/` daemon split by ADR-070 into `gatewayd-public` (TLS-only
  edge) and `gatewayd-internal` (routing + wake + proxy). Body is preserved
  verbatim; readers should substitute "gatewayd-internal" for the
  routing/wake/proxy path and "gatewayd-public" for the certmagic/TLS path.
  `cmd/gatewayd/<file>.go` citations in this body are stale; see PR-E for
  the new file locations.
- **Date:** 2026-07-28
- **Decision:** Add three additive account-scoped list endpoints — `GET /v1/instances`, `GET /v1/secrets`, `GET /v1/apps/metrics` — that each replace a per-app fan-out (issue #393). Reuse the existing per-app pgstore / PromQL / middleware surfaces verbatim; no middleware change on apid.
- **Why:** Today three dashboard pages (Workers, Secrets, Overview) issue one HTTP request per app to render — **N+1** at Pro (25) and Scale (100). Each call is independently `authLimited`, so a customer opening the dashboard is the most likely path to trip *their own* rate limit while merely reading their own data. Fan-out also creates a partial-failure mode the console papers over with a footnote ("could not read instances for: x, y") because one 404 must not blank the page.
- **Consequences:**
  - **New handlers** in `cmd/apid/handlers_account_scoped.go`: `listInstancesForAccount`, `listSecretsForAccount`, `getAppsMetrics`. Per-app endpoints (`/v1/apps/{slug}/instances`, `/v1/apps/{slug}/secrets`, `/v1/apps/{slug}/metrics`) are **unchanged** — additive opt-in.
  - **New DTOs** in `pkg/api/dto.go` and `pkg/api/secrets.go`: `ListInstancesResponse`, `AccountAppSecretResponse`, `ListSecretsForAccountResponse`, `AppsMetricsResponse`. The `AccountAppSecretResponse` carries `app_id` + `app_slug` so the dashboard renders "foo-app / DATABASE_URL" without a parallel `/v1/apps` round-trip.
  - **New pgstore methods** in `pkg/state/pgstore.go` + matching memstore: `ListInstancesForAccountPaged(accountID, limit, before)` and `ListAppSecretsForAccount(accountID, limit, before)`. Both JOIN on `apps.account_id = $1` — that SQL is the **only** IDOR guard; there's no per-handler `loadApp` because there's no slug path. New `AccountAppSecret` row type in `pkg/state/types.go`.
  - **New promql helpers** in `pkg/promql/client.go`: `Client.QueryMap(query) (map[string]float64, error)` and `Client.QueryBuckets(query) (map[string]map[string]float64, error)`. The rollup runs **6 PromQL round-trips regardless of N apps** (3 vector queries via `QueryMap`, 1 vector query via `QueryBuckets`, 1 scalar via `QueryScalar` for the FLEET wake p95, plus the response build). The naive per-app loop would be `7N` round-trips — at Scale (100 apps) that is ~3.5 s upper bound vs. ~30 ms colocated with the batched queries.
  - **New server-side helper** in `pkg/api/paging.go`: `api.ParseLimit(raw, defaultN, maxN, label) (*Problem, int)`. Wraps the strict 400 logic from `parseInvoiceListParams` (`cmd/apid/handlers_ext.go:1701-1714`) so future endpoints don't re-implement. Returns an RFC 7807 `Problem` with `WithLimit(max, observed)` + `WithDocs(...)` on bad input, matching the `/v1/invoices` contract.
  - **Cursor shapes**: instances — `?before=<instances.id>` (UUIDv7, monotonic). Secrets — `?before=<slug>|<key>` (the pair, encoded; SQL splits via `split_part`). Both emitted as `next_before` only when `len(page) == limit`.
  - **No middleware change on apid.** All three new routes wrap with the same `s.authLimited(s.requireScope(api.ScopesReadSurface...)(...))` chain the per-app endpoints use. The per-account rate-limit bucket (ADR-040 / PR #368) is wired **only** into gatewayd's wake-path edge (`pkg/gateway/ratelimit.go`); apid's `authLimited` is the per-IP failed-auth limiter (10/min/IP). The OpenAPI descriptions document the "1 call replaces N" tiering so a customer reading the docs sees why per-page-load token spend drops without the auth middleware changing.
  - **OpenAPI entries** in `api/openapi.yaml`: three new paths + four new schemas (`ListInstancesResponse`, `AccountAppSecretResponse`, `ListSecretsForAccountResponse`, `AppsMetricsResponse`). `make spec-check` gate stays green.
  - **ADR cross-refs**: ADR-040 (per-account rate limit — gatewayd-only), ADR-042 (per-app metrics + cold_boot rename — the precedent for the rollup), ADR-043 (Move 4 app-logs — the recent precedent for additive, account-scoped reads).
- **Rejected alternatives:**
  - **Replace the per-app endpoints.** Breaks every existing dashboard + SDK consumer. Out of scope for a non-blocking fix; explicit additive is the safer contract.
  - **Charge N tokens per aggregate call.** Customer-facing rate-neutral (a Pro account's 25-app Workers page still costs 25), but a semantic break in `authLimited` that ADR-040 didn't authorise. The aggregate call replaces N per-app calls on the customer side; charging more than 1 per aggregate would penalise the very customer behaviour this PR is trying to enable.
  - **Naive per-app `fetchAppMetrics` loop in `getAppsMetrics`.** Reuses existing code 1:1 but blows up to `7N` round-trips. At Scale (100 apps) that's ~3.5 s colocated — a customer-visible regression. The `QueryMap` + `QueryBuckets` helpers keep it constant at 6 round-trips.
  - **Single PromQL selector `app=~"<id1>|<id2>|..."`.** Faster than per-app queries but adds new PromQL surface + new failure modes (an `app=~` regex with 100 alternatives is a known Prometheus footgun on large fleets). Defer to a follow-up if scale ever pushes past the 6-round-trip budget.
  - **Single PromQL rollup query that aggregates across apps server-side.** Would lose the per-app granularity the dashboard renders today (`AppMetricsResponse` is per-app, not aggregated).
  - **Extract a shared `pkg/state/cursor` package.** `ListInvocationsForAccount` already has a cursor pattern; pulling a third copy into a shared package is a separate refactor (issue #393 is a feature PR, not a refactor). The new methods follow the existing SQL shape verbatim.

## Cross-references

- Issue #393.
- Spec §4.2 (apid), §6.2 (invariants — the new endpoints don't relax any), §11 (security — ciphertext-only invariant preserved).
- ADR-040 (per-account rate limit), ADR-042 (per-app metrics rollup precedent), ADR-043 (Move 4 app-logs — additive precedent).
- `cmd/apid/handlers_account_scoped.go`, `pkg/api/paging.go::ParseLimit`, `pkg/promql/client.go::QueryMap/QueryBuckets`, `pkg/state/pgstore.go::ListInstancesForAccountPaged/ListAppSecretsForAccount`.
