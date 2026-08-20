# ADR-122 · Endpoint Discovery (issue #975 item #1)

- **Status:** Proposed
- **Date:** 2026-08-20
- **Supersedes:** none
- **Related:** ADR-051 (characterization probe),
  ADR-085 (OpenAPI spec-sync), ADR-090 (api_keys),
  ADR-120 (app consumer_keys — companion identity primitive),
  ADR-091 (per-app public auth), ADR-079 (closed
  `public_auth_mode` enum), ADR-089 (per-app metrics
  vocabulary), ADR-093 (per-route metrics — downstream
  consumer), ADR-104 (per-consumer throttle keying — downstream
  consumer).

## Context

Gregale's cold-boot characterization probe
(`guest/init/characterize_linux.go`) ALREADY fetches
`/openapi.json` from the customer's app — but **discards the
body**. Only the HTTP status line is inspected to disambiguate
`http` from `graphql` and `grpc`. The same gap exists in
`[/graphql]` introspection (4 KiB into a stack buffer then
discarded).

This is the load-bearing miss in issue #975's 12-item
capability audit. A customer running on Gregale cannot answer
"what HTTP endpoints does my deployed app expose?" without
leaving the platform. The customer's only options today are:

1. Hand-craft the OpenAPI doc as part of the deploy — but
   Gregale never exposes it back to anyone (no apid
   endpoint, no dashboard widget).
2. Stand up a separate observation pipeline (Datadog, New
   Relic, etc.) that re-flattens the function surface.
3. Maintain a parallel hand-written API catalog that drifts.

The audit (`docs/faas_implementation_spec.md` §17 + Tier-A
gap tracking) flags this as the first item to close because:

- The wire format already exists —
  `pkg/api/characterization.go::CharacterizationReport` is
  the JSON the guest ships over AF_VSOCK STREAM (port 1026,
  msgtype 3). Today it carries `ObservedClass`, `ObservedPort`,
  `ExitCode`, `ListeningAddrs`, `OutboundCount`, `LogTail`,
  `PortNormalizationMode`. Adding two fields is wire-additive.
- The vsock transport already exists —
  `pkg/fcvm/vmm.go::WaitCharacterizationReport` reads the
  framed JSON body. A constant bump (`VsockCharacterizationMaxBody`
  32 KiB → 128 KiB) is the only depth change needed.
- The store pattern already exists —
  `pkg/state/store.go::UpsertDeploymentScanResult` (per-deployment
  jsonb blob, idempotent overwrite, `ErrNotFound` on miss) is
  the canonical new-blob precedent. The same shape handles an
  OpenAPI doc.
- The OpenAPI export infrastructure already exists —
  `pkg/apid/openapi_handler.go` serves the platform's own
  OpenAPI 3.1 spec at `/v1/openapi.{yaml,json}` with `//go:embed`
  + `Cache-Control: public, max-age=300`. The new endpoints
  mirror this posture.

The downstream unblocks are the audit's #6 (consumer
analytics), #7 (route metrics cold-start), #8 (queryable
request logs — all from the audit). Each of those needs a
way to enumerate endpoints per deployment; this ADR hands
them the data.

## Decision

Five sub-decisions land together because each constrains the
others.

### D1 · Per-deployment, not per-app

The deployment lifecycle dominates; multiple deployments of
the same app can expose different surfaces (different build,
different env, different `openapi.json` upstream). The store
keys on `deployment_id`, not `app_id`. Mirrors
`UpsertDeploymentScanResult`'s `deployment_id` scoping
exactly — the per-deploy grype scan uses the same shape, and
both end up indexed by `account_id` for the quota gate.

`tombstoning` is `ON DELETE CASCADE` from `deployments`. GDPR
hard-delete of the deployment drops the doc row. The audit
log emits `app.openapi_doc.deleted` so the tombstone is
visible to the customer-facing audit reader even after the
row is gone.

### D2 · Wire-format additive, no version bump

`pkg/api/characterization.go::CharacterizationReport` grows
by two fields:

```go
// OpenAPIDoc is the captured OpenAPI document body, if any.
// Empty when the probe found no JSON document at /openapi.json.
OpenAPIDoc []byte `json:"openapi_doc,omitempty"`
// OpenAPIDocTruncated is true when the captured body was
// truncated at VsockCharacterizationMaxBody. Receivers must
// surface this to the user (dashboard widget + CLI).
OpenAPIDocTruncated bool `json:"openapi_doc_truncated,omitempty"`
```

Symmetric back-compat: an old receiver ignores the new
fields (extra JSON keys are skipped by `encoding/json`); a
new receiver treats `OpenAPIDoc == nil` as "no doc captured"
(which is the only case an old probe would produce). The
"every json tag is wire-stable" comment at line 21 of
`characterization.go` is preserved — adding fields is
allowed, renaming or retyping is not.

`VsockCharacterizationMaxBody` (guest + host mirror) bumps
from `32 * 1024` to `128 * 1024`. Real-world OpenAPI docs
typically run 8-30 KiB; complex apps (Stripe-scale: ~700
operations, deep `components.schemas`) exceed 32 KiB. 128
KiB leaves headroom. Guest truncates BEFORE the json.Marshal
so the receiver never sees a malformed body.

### D3 · Free plan OFF, paid plans ladder up

Endpoint discovery is paid-only. Modeled after
`pkg/api/limits.go::ConsumerKeysPerApp` (line 587). Per-plan
table:

| Plan  | `OpenAPIDocsPerDeployment` | `OpenAPIDocMaxBytes` | `OpenAPIDocsPerAccount` |
|-------|---------------------------|----------------------|--------------------------|
| Free  | 0                         | 0                    | 0                        |
| Hobby | 1                         | 131072               | 100                      |
| Pro   | 1                         | 131072               | 500                      |
| Scale | 1                         | 131072               | 2500                     |

The microVM still captures the doc — the read cost is one
TCP `ReadAll` against `/openapi.json`, throughput-irrelevant
relative to the 350 ms wake budget. The apid refuses to
expose it on free plans via `enforceOpenAPIPlan(plan)`. The
collected doc is `DO` (dead on arrival) for free — the
microVM ate the read cost, the customer never sees the
benefit. This is the deliberate "always capture, gate at
apid" posture (rejected the alternative of a per-wake vsock
var opt-in: it would have required a wake-side plumbing
change for one boolean).

`OpenAPIDocsPerDeployment = 1`: the natural cardinality is
exactly one doc per deployment. The per-plan value is
`1` for paid plans (the table is closed for future expansion).
`OpenAPIDocsPerAccount` is the load-bearing cap — a Scale
plan customer running 2500 deployments still gets a
discoverable surface for each.

### D4 · 128 KiB hard cap, truncate at the guest

The guest-init probe hard-truncates the body to 128 KiB
before `json.Marshal`. The flow:

1. `probeHTTP` opens a TCP socket to `127.0.0.1:<port>`,
   sends `GET /openapi.json HTTP/1.1`.
2. After the status line, the response body is read into a
   heap-allocated `bytes.Buffer` capped at 128 KiB via
   `io.LimitReader(resp.Body, 128*1024)`.
3. If the read buffer length is `== 128*1024` AND the socket
   is closed cleanly (EOF), the doc is exactly 128 KiB.
   If the read length is `== 128*1024` AND the body might
   have more, set `OpenAPIDocTruncated = true`.
4. The `Content-Type` is checked: `application/json` or
   `application/openapi+json` qualifies. Other content types
   (e.g. `text/html`) are dropped silently — the lifecycle
   never fails on capture.
5. The doc is set on `CharacterizationReport.OpenAPIDoc`
   only when (a) `Content-Type` matches, (b) the body
   contains a top-level `openapi` (`"3.0"`/`"3.1"`) or
   `swagger` key (cheap JSON shape sniff prevents a serving
   SPA from being mis-classified as an OpenAPI doc).

The shape sniff is conservative — it accepts the body if
ANY of the canonical keys is present. The downstream
validator (apid) does the strict Draft 2020-12 compilation
via `pkg/edgevalidate/jsonschema.go` (OpenAPI 3.1 native
reuses JSON Schema 2020-12). A doc that passes the sniff
but fails the strict validation is still persisted with
`source = 'cold_boot'` — the validator lives at the apid
PATCH path, not the cold-boot path.

### D5 · Manual upload (`PATCH`) vs. cold-boot capture

Both flows converge on the same store method
`UpsertDeploymentOpenAPIDoc`. The cold-boot path is the
default. The `PATCH` endpoint on
`/v1/apps/{slug}/deployments/{deployment}/openapi` exists
for:

1. **Retry after transient failures** — the cold-boot probe
   can fail because the app wasn't ready in time, the body
   was huge, the connection was reset. The customer can
   re-trigger capture by hitting PATCH.
2. **GraphQL / gRPC-style apps** — they don't serve
   `/openapi.json`. The customer calls POST on a GraphQL
   introspection query URL or hands up a follow-up
   `survey.json` shape. PATCH lets them hand-author the
   doc.
3. **Override** — the customer disagrees with the captured
   doc (the auto-served one is a stale `/openapi.json` from
   a left-behind nginx). They PATCH their preferred doc.

The `source` column is `IN ('cold_boot', 'manual_upload')`.
Last-write-wins. The audit emits:

- `cold_boot` first capture: `app.openapi_doc.captured`
  (data: `{deployment_id, byte_size, source, truncated}`).
- `manual_upload` PATCH: `app.openapi_doc.updated` (data:
  `{deployment_id, byte_size, source='manual_upload'}`).
- `manual_upload` DELETE: `app.openapi_doc.deleted`.

The `openapi_doc_changed` `pg_notify` channel fires on every
write/delete, mirroring `edge_rule_changed`. The schedd
subscribes for cache invalidation on the next wake. The
gateway doesn't subscribe — endpoint discovery is a customer
surface, not a hot-path enforcement.

## Consequences

+ A new identity surface for the audit's #6/#7/#8: each
  downstream item reads `deployment_openapi_docs.doc` to
  enumerate endpoints, then plumbs the result into the
  per-app metrics / per-consumer request log surface.
+ Slot 00330 (smallest free above main's 00329). Migration
  is replay-safe (no `IF NOT EXISTS` hazards; the table
  is brand new).
+ `VsockCharacterizationMaxBody` bump to 128 KiB is
  additive — old guest builds skip the OpenAPI fields
  entirely (`omitempty` on the marshal path).
+ The PATCH endpoint's strict JSON Schema validation
  (Draft 2020-12 / OpenAPI 3.1) reuses
  `pkg/edgevalidate/jsonschema.go` — the wire validation
  path is the same one `kind='validate'` edge rules use.
  No new validator code.
- The cold-boot read is a single TCP `ReadAll` capped at
  128 KiB. Worst case (a slow-IO customer app that
  buffers the whole doc before serving) costs ≤ 128 KiB
  of in-guest RAM and one TCP read. Throughput-irrelevant
  vs. the 350 ms wake budget. **Verified**: the probe
  runs in a goroutine (`runL7Probes` at line 397 of
  `characterize_linux.go`), bounded by the 2 s context —
  even a hostile 30 s response can't wedge the wake.
- The persisted doc is a snapshot of the cold-boot
  probe, NOT a live projection. If the customer updates
  their `openapi.json` mid-deployment, the new doc is
  captured on the next cold boot. Customers who need
  hot-reload use the PATCH endpoint.
- A 403 on free plan is a deliberate friction. The
  customer sees "endpoint discovery is a paid feature" —
  not a silent denial. The error code is
  `plan_openapi_doc_not_allowed` (next to
  `plan_openapi_doc_quota_reached`, `plan_openapi_doc_too_large`).
- Per-deployment cardinality is exactly 1 (no PATCH on
  the same deployment can grow a list). The
  `OpenAPIDocsPerAccount` ladder is the cap that matters
  for customer BOLA exposure.

## Alternatives considered (and rejected)

1. **Per-app keying** — rejected: deployment lifecycle
   dominates. Multiple deployments of the same app expose
   different surfaces. The audit's #6/#7/#8 all read
   per-deployment, not per-app.
2. **Per-wake vsock var opt-in (`OPENAPI_DISCOVERY=1`)** —
   rejected: the read cost is one TCP ReadAll. The wake
   wiring already runs the probe unconditionally. Adding
   a vsock var forces a wake-side plumbing change for a
   no-op cost win. The plan-tier gate lives at the apid,
   which is the right layer.
3. **In-memory only (no Postgres persistence)** — rejected:
   the audit's #6/#7/#8 survive across cold boots and
   across the edge-rule cache rebuild. Persisting in
   `deployment_openapi_docs` is the only way the surface
   is observable. The cost is one row per deployment
   (≤ 128 KiB of JSONB), well under the per-account
   `OpenAPIDocsPerAccount` budget.
4. **GraphQL introspection capture in this PR** — rejected:
   `probeGraphQL` lives in the same file but reads a
   different path (POST to `/` with the `__schema` query).
   The introspection result is a separate schema, not an
   OpenAPI doc. Capturing it requires a separate
   `GraphQLSchemaDoc []byte` field on the wire and a
   separate `deployments_graphql_schemas` table. That's
   issue #975 item #2 and beyond. The audit's #1 is the
   OpenAPI gap; the GraphQL gap is item #2.
5. **gRPC reflection capture in this PR** — rejected for
   the same reason: `probeGRPC` is a binary protocol,
   the capture path is `pkg/fcvm/grpcreflect.go` (does
   not exist yet; would need a new dependency on the
   `google.golang.org/grpc` reflection package). Item #3.
6. **Encrypted-at-rest (always)** — rejected: the doc is
   a public-facing description of the customer's own API.
   The customer's app already serves it unencrypted at
   `/openapi.json`. Storing at rest in plaintext
   `jsonb` matches the customer's serving posture. The
   GDPR CASCADE handles hard-delete.

## Migration plan

A single PR landing all 5 atomic commits:

- **PR #1-A** — `docs/adr/122-endpoint-discovery.md`.
  Slot **none** (pure docs).
- **PR #1-B** — `migrations/00330_endpoint_discovery.sql`
  + pin test. Slot **00330** (smallest free above main's
  00329).
- **PR #1-C** — wire-format additions
  (`pkg/api/characterization.go` + `pkg/fcvm/vmm.go`
  `VsockCharacterizationMaxBody` bump + `pkg/fcvm/vmm_test.go`
  round-trip). Slot **none** (pure Go).
- **PR #1-D** — guest-init probe
  (`guest/init/characterize_linux.go` probeHTTP body
  capture + `runL7Probes` relaxation + truncation flag).
  Slot **none**.
- **PR #1-E** — Store interface + pgstore + memstore
  (4 methods + `OpenAPIDocMeta` struct + IDOR guard).
  Slot **none**.
- **PR #1-F** — Limits + accessors + error codes
  (`pkg/api/limits.go`, `pkg/api/limits_test.go`,
  `pkg/api/apierror.go`). Slot **none**.
- **PR #1-G** — Apid endpoints + audit + pg_notify
  (`cmd/apid/handler_openapi_doc.go` + handler test +
  `pkg/db/notify.go` `NotifyOpenAPIDocChanged` constant).
  Slot **none**.
- **PR #1-H** — OpenAPI regen + SDK regen + push. Slot
  **none**.

The PR cluster is small enough to land as a single PR
with 5-7 atomic commits (the ADR + migration + 5 Go
commits + spec-sync commits). A single PR is preferred
over a multi-PR cluster because the wire-format bump is
backward-compatible (`omitempty`) — there is no migration
window where old probes need new receivers.

Slot precheck runs at PR-open time via
`scripts/ci/check_migration_slots.sh`. If a sibling PR
claims 00330 (e.g. PR #1000's apps.consumer_auth_mode
add-on — slot 00330 was reserved in its own branch
lineage), this PR renumbers to **00331** + lands a
`00330_reserve_slot.sql` fence per ADR-041.

## Spec & SDK propagation

`api/openapi.yaml` gains two new operations under the
existing `/v1/apps/{slug}/deployments/{deployment}` path:

- `GET /v1/apps/{slug}/deployments/{deployment}/openapi`
  — 200 returns the JSON document; 404 on miss; 403 on
  free plan.
- `PATCH /v1/apps/{slug}/deployments/{deployment}/openapi`
  — request body `{doc: object, source: "manual_upload"}`;
  200 returns the stored doc; 413 on size cap; 403 on free
  plan; 400 on draft-2020-12 validation failure.

`make spec-sync` regenerates `pkg/apid/openapi.yaml` (the
`//go:embed` copy per ADR-085). `make spec-check` is the
CI gate. `make sdk-gen-node-check` + `make sdk-gen-python-check`
+ `make sdk-check` verify the SDK diffs.

## Security & GDPR notes

- The OpenAPI doc is a description of the customer's own
  API. It is not a secret. Storing at rest in plaintext
  `jsonb` matches the customer's serving posture.
- `byte_size` is bounded `1..131072` at the SQL CHECK
  layer (`migrations/00330_endpoint_discovery.sql`).
  Per-plan upper bounds at the apid layer
  (`OpenAPIDocMaxBytes`).
- GDPR hard-delete: `ON DELETE CASCADE` from `deployments`
  drops the doc row. The audit log emits
  `app.openapi_doc.deleted` so the tombstone is visible
  to the customer-facing audit reader.
- The cold-boot capture path is in the guest's
  read-only `/proc` namespace (no guest-side write
  to the customer file system). The customer's app
  sees one extra TCP GET per wake — observable in the
  customer's own access log, but not a security
  concern (the customer already serves `/openapi.json`
  to anyone).
- Manual-upload PATCH validates the body via
  `pkg/edgevalidate/jsonschema.go` (Draft 2020-12 compile).
  A malformed body is 400 ErrValidation, not silently
  persisted. The cold-boot path skips the strict
  validation (it only does the cheap shape sniff) — a
  malformed cold-boot doc is just a "no doc captured"
  outcome (the customer can hit PATCH to overwrite).
