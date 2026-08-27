# ADR-124 · Traffic mirroring — customer-facing CRUD surface (issue #72 / ADR-125 PR-A2)

- **Status:** accepted
- **Date:** 2026-08-23
- **Issue:** #72 (PR-A1 #1019, PR-A2 current, PR-A3 follow-on)
- **Supersedes:** none
- **Related:** ADR-016 (additive proto field discipline); ADR-041 (migration slot reservation); ADR-070 (gatewayd-public / gatewayd-internal split); ADR-084 (traffic-splitting picker signal — the read-side sibling); ADR-098 (wake-coord detached-ctx pattern, preview of A3's mirror goroutine); ADR-125 (storage half — PR-A1); memory [[pr-1019-issue-72-traffic-mirror-pr-a1-shipped-2026-08-22]]

## Context

PR-A1 (head `7cc2a573a`, merged to main 2026-08-23) shipped the
**storage half** of traffic mirroring: schema (`mirror_rules`,
`mirror_invocation_results`, `instances.mode`), 8 store methods
(MemStore parity), sampler/reaper `mode='mirror'` skip, and the plan
caps (Free/Hobby 0; Pro 1; Scale 3; `MirrorMaxLifetimeSeconds=5`).
The feature is **not** usable in production yet — there is no HTTP
route, no CLI verb, no ADR, no audit/notify emit. A customer POSTing
`/v1/apps/{slug}/mirrors` today gets 404.

PR-A2 closes the gap by shipping the **customer-facing CRUD
surface**:

1. Six apid HTTP routes under `/v1/apps/{slug}/mirrors` —
   POST/GET/GET-one/PATCH/DELETE + `/summary`.
2. Six CLI verbs under `gregale mirror <list|create|info|update|rm|summary>`
   — every leaf threads through `authedClient()` and the typed SDK
   methods on `pkg/api/client.go`.
3. The design record (this file).
4. Five new DTOs (`CreateMirrorRuleRequest`, `UpdateMirrorRuleRequest`,
   `MirrorRuleResponse`, `MirrorRuleListResponse`, `MirrorSummaryResponse`)
   + seven error sentinels in `pkg/api/errors.go` + audit kinds +
   `kind="mirror"` notify payload.
5. The OpenAPI spec parity entries the spec_compliance_test gates on.

The runtime dispatch (gateway goroutine, redaction, proto widening,
schedd stamping) lands in **PR-A3** — not in scope for A2. After
A2 lands, a customer can POST a rule and see it via GET, but no
traffic is actually mirrored yet; the GET `/summary` endpoint reads
rows from the comparison ledger that PR-A1 left empty.

## Constraints honoured

- **Plan caps** (user decision, frozen in PR-A1): Free 0 / Hobby 0
  / Pro 1 / Scale 3 (`Limits.MirrorRuleAllowed` and
  `Limits.MirrorTargetsPerApp`).
- **Default `include_body=false`** (spec hint: "sensitive headers
  and bodies must be redacted or disabled by default"). The CLI
  defaults to `--no-include-body`; the SDK defaults to `false`.
- **Always-strip headers** (Authorization, Cookie, Set-Cookie,
  X-API-Key, Proxy-Authorization, WWW-Authenticate) are documented
  in the rule-create response and in this ADR; the actual stripping
  happens in PR-A3's `mirror_redact.go` — A2 only stores the
  customer-supplied `redact_headers` list.
- **IDOR posture** (cross-account → silent 404, never 403, never
  leak existence) — same pattern as traffic split's deployment-id
  surface.
- **Quota enforcement is *transactional*** in
  `CreateMirrorRuleIfUnderQuota` (`FOR UPDATE` on the apps row, then
  `SELECT count(enabled)` vs `Limits.MirrorTargetsPerApp` per
  `pkg/state/store.go:1878-1894`); the handler still does the plan
  gate + range check first.

## Decision

### D1 — Six routes under `/v1/apps/{slug}/mirrors`

```
POST   /v1/apps/{slug}/mirrors                createMirrorRule
GET    /v1/apps/{slug}/mirrors                listMirrorRules
GET    /v1/apps/{slug}/mirrors/{id}           getMirrorRule
PATCH  /v1/apps/{slug}/mirrors/{id}           updateMirrorRule
DELETE /v1/apps/{slug}/mirrors/{id}           deleteMirrorRule
GET    /v1/apps/{slug}/mirrors/{id}/summary   getMirrorRuleSummary
```

Path is `{slug}`-scoped (not deployment-id-scoped) so the IDOR guard
is the cheaper `s.loadApp(slug) → app.AccountID == acct.ID` (vs the
deployment-id lookup chain). The `{id}` segment is the mirror-rule
UUID, not a deployment.

Auth chains mirror traffic split exactly:

| Verb | Chain |
|---|---|
| POST, PATCH, DELETE | `authLimited(requireMFA(requireScope(ScopesDeployWriteSurface)))` |
| GET, summary | `authLimited(requireMFA(requireScope(ScopesReadSurface)))` |

### D2 — Gate order: IDOR → decode → range → plan → store → audit → notify

Mirrors `updateDeploymentTraffic` in `handlers_ext.go` exactly:

1. Resolve app via `loadApp(slug)` — IDOR guard: cross-account slug
   returns silent 404.
2. Decode body; reject unknown JSON fields (`decodeJSON`'s
   `DisallowUnknownFields`).
3. **Range check FIRST** (no plan context) — 422
   `invalid_mirror_percent`. Range-before-plan is intentional: a
   malformed value is loud regardless of plan, and the plan gate
   only fires on a legal value (so the operator sees the 403 "plan
   locked" not a 422 "value illegal").
4. **Plan tier gate** — 403 `plan_mirror_not_allowed` (Hobby/Free
   locked). 403 (not 422) because the request shape is legal; only
   the plan forbids it.
5. **Store call** (transactional; `FOR UPDATE` on apps row enforces
   the per-app quota). The `*state.QuotaError` is translated at the
   handler boundary: `qe.Kind == QuotaErrorKindMirror && qe.NotAllowed`
   → `ErrPlanMirrorNotAllowed`; otherwise → `ErrMirrorRuleQuotaExceeded`.
6. **Audit emit** (best-effort) — `kind=mirror_rule.{created,updated,deleted}`.
7. **pg_notify** — `kind="mirror"` on `NotifyDeploymentChanged`;
   gateway refresh subscriber (PR-A3) picks this up. Reuses the
   traffic-split channel — the `kind` discriminant distinguishes.

### D3 — CLI: `mirror <list|create|info|update|rm|summary> --app <slug>`

```
gregale mirror list    --app <slug>                      List mirror rules for an app
gregale mirror create  --app <slug> --source <depID> --mirror <depID>
                       [--percent N] [--include-body]
                       [--redact-header Name]…           Create a mirror rule
gregale mirror info    --app <slug> --id <mirrorID>      Show one mirror rule
gregale mirror update  --app <slug> --id <mirrorID>
                       [--percent N] [--enable|--disable]
                       [--include-body|--no-include-body]
                       [--redact-header Name]…           Patch a mirror rule
gregale mirror rm      --app <slug> --id <mirrorID>      Delete a mirror rule
gregale mirror summary --app <slug> --id <mirrorID>
                       [--window 1h|24h|7d]              Aggregate drift counts
```

Six leaves, dispatched via `cmdMirror`. The pattern mirrors
`cmdEdgeRules` (`commands_edge_rules.go`) exactly: each leaf is its
own function with its own flag set, `--json` round-trips through
`jsonOut(writeJSON(...))` so the SDK DTOs reach the customer's
pipeline unmodified. Human output is a labelled `fmt.Fprintf` block
per the rest of the codebase.

**Patch semantics on update.** `cmdMirrorUpdate` uses `fs.Visit` to
distinguish "flag not passed" from "flag passed with empty value",
so `--percent 0` (legal — disable without removing) is
distinguishable from no `--percent` at all (keep existing value).
The `--enable`/`--disable` and `--include-body`/`--no-include-body`
pairs are mutually exclusive at the CLI level.

**Default window for summary.** `?window=1h` is the default; the
server's `ParseMirrorWindow` enforces `1h | 24h | 7d` and returns
422 `invalid_mirror_window` on anything else.

### D4 — DTOs

```go
type CreateMirrorRuleRequest struct {
    SourceDeploymentID string   `json:"source_deployment_id"`
    MirrorDeploymentID string   `json:"mirror_deployment_id"`
    Percent            int      `json:"percent"`              // 0..100; 100 = every request
    IncludeBody        bool     `json:"include_body"`         // default false
    RedactHeaders      []string `json:"redact_headers"`       // customer-extra, beyond always-stripped
}

type UpdateMirrorRuleRequest struct {
    Percent       *int      `json:"percent,omitempty"`        // pointer = "absent" vs "zero"
    Enabled       *bool     `json:"enabled,omitempty"`
    IncludeBody   *bool     `json:"include_body,omitempty"`
    RedactHeaders *[]string `json:"redact_headers,omitempty"`
}

type MirrorRuleResponse struct {
    ID                    string    `json:"id"`
    AppID                 string    `json:"app_id"`
    SourceDeploymentID    string    `json:"source_deployment_id"`
    MirrorDeploymentID    string    `json:"mirror_deployment_id"`
    Percent               int       `json:"percent"`
    Enabled               bool      `json:"enabled"`
    IncludeBody           bool      `json:"include_body"`
    RedactHeaders         []string  `json:"redact_headers"`
    AlwaysStrippedHeaders []string  `json:"always_stripped_headers"`  // A3 guarantee, A2 manifest
    CreatedAt             time.Time `json:"created_at"`
    UpdatedAt             time.Time `json:"updated_at"`
}

type MirrorRuleListResponse struct {
    Rules []MirrorRuleResponse `json:"rules"`
    Count int                  `json:"count"`
}

type MirrorSummaryResponse struct {
    TotalInvocations    int64   `json:"total_invocations"`
    StatusDiffCount     int64   `json:"status_diff_count"`
    SchemaDiffCount     int64   `json:"schema_diff_count"`
    BodyDiffCount       int64   `json:"body_diff_count"`
    MeanLatencyDiffMs   int64   `json:"mean_latency_diff_ms"`     // signed: mirror - source
    P99LatencyDiffMs    int64   `json:"p99_latency_diff_ms"`
    CrashCount          int64   `json:"crash_count"`
    WindowSeconds       int     `json:"window_seconds"`
}
```

JSON convention: snake_case tags, no `omitempty` on response fields
(always emit even if zero — clients can detect absent vs zero).

### D5 — Error sentinels

Seven constructors, mirroring `ErrPlanTrafficSplitNotAllowed`
(`errors.go:3128-3133`):

| Sentinel | HTTP | Code | Triggered by |
|---|---|---|---|
| `ErrPlanMirrorNotAllowed(p Plan)` | 403 | `CodePlanMirrorNotAllowed` | Plan != Pro/Scale |
| `ErrMirrorRuleQuotaExceeded(l Limits, observed int)` | 422 | `CodeMirrorRuleQuotaExceeded` | `*QuotaError{Kind: QuotaErrorKindMirror, !NotAllowed}` |
| `ErrInvalidMirrorPercent(got int)` | 422 | `CodeInvalidMirrorPercent` | Range check on create/update |
| `ErrMirrorSourceTargetSame()` | 422 | `CodeMirrorSourceTargetSame` | `ErrMirrorSourceTargetSame` |
| `ErrMirrorDeploymentNotLive()` | 409 | `CodeMirrorDeploymentNotLive` | `ErrMirrorDeploymentNotLive` |
| `ErrMirrorCrossAppMismatch()` | 422 | `CodeMirrorCrossAppMismatch` | `ErrMirrorCrossAppMismatch` |
| `ErrMirrorRuleNotFound()` / `ErrInvalidMirrorWindow(got string)` | 404 / 422 | `CodeMirrorRuleNotFound` / `CodeInvalidMirrorWindow` | Store `ErrNotFound` / bad `?window=` |

**Quota-error translation rule (handler boundary, single place).**
The `*state.QuotaError` is constructed inside
`CreateMirrorRuleIfUnderQuota`'s `FOR UPDATE`-locked transaction
(`store.go:1878-1894`). Handler does `errors.As(err, &qe)`, checks
`qe.Kind == state.QuotaErrorKindMirror`, then routes to
`ErrPlanMirrorNotAllowed(acct.Plan)` (if `qe.NotAllowed`) or
`ErrMirrorRuleQuotaExceeded(limits, qe.Observed)`.

**Hobby/Free 403 prose** explains the cost: mirror's wake = 1 VM per
request, billed per running second, capped at
`MirrorMaxLifetimeSeconds=5`.

### D6 — Audit kinds

`mirror_rule.{created,updated,deleted}` — best-effort, never rolls
back. The audit payload includes `app`, `rule`, `source`,
`mirror`, `percent`, `enabled`, `include_body`; updated emits a
`prev` map for drift detection. Failure to emit is logged and
continued (matches traffic-split pattern).

### D7 — Notify: reuse `NotifyDeploymentChanged` with `kind="mirror"`

Mirrors traffic-split precedent; subscribers re-read the row on
receipt. Best-effort: a notify outage is logged and continued
(`handlers_ext.go:1549-1554` pattern). Pre-PR-A3 the gateway ignores
this signal; the audit + row write still happen so the customer
sees the rule via GET.

### D8 — Plan / quota gates

- **Create path:** 403 `plan_mirror_not_allowed` for Hobby/Free; 422
  `mirror_rule_quota_exceeded` once `Limits.MirrorTargetsPerApp` is
  reached (Pro = 1; Scale = 3).
- **Update path:** range check fires only when `Percent` is
  supplied (pointer-semantics); the plan gate is **deliberately
  skipped** on update — a Pro customer's existing rule survives an
  upgrade to Hobby; the rule is disabled by the reaper at the next
  read window so the mirror VM doesn't keep waking. Matches the
  traffic-split precedent where the plan gate fires on the create
  path but not the update path.
- **Always-strip headers** are A3's job (the redaction itself runs
  in `mirror_redact.go`); A2 stores the customer-supplied
  `redact_headers` list and documents the always-stripped list in
  the rule-create response.

### D9 — IDOR posture

Every `{id}`-route resolves to a rule via `GetMirrorRuleByID`, then
asserts `rule.AccountID == acct.ID && rule.AppID == app.ID`. Failure
path: `s.notFound(w, "no such mirror rule")` — never
`ErrMirrorRuleNotFound` leaking existence. Same posture as
traffic-split's deployment-id surface.

### D10 — No migration, no schema change

PR-A2 ships zero migrations. All schemas landed in PR-A1
(`00384_mirror_rules.sql` family). The slot fence for downstream
PRs (A3 and beyond) is the responsibility of those PRs (per
ADR-041); A2 does not need one.

## Consequences

- The mirror CRUD surface is reachable from the dashboard and CLI
  today; the runtime dispatch is still A3's job.
- Cross-account probing cannot distinguish "exists" from "deleted" —
  the IDOR posture holds for every `{id}`-route.
- The plan gate's intentional absence on update lets a customer's
  existing rule survive a downgrade; the reaper is what enforces
  "no mirrors on Hobby".
- The `kind="mirror"` notify discriminant shares a channel with
  traffic-split. Subscribers MUST re-read the row on receipt (the
  channel's payload is a hint, not a source of truth).
- Mirror billing stays opt-in: a customer has to explicitly POST a
  rule, so a misconfigured reaper can't surprise-bill them.

## Alternatives considered

- **(a) Putting mirror on the same path as traffic split
  (`/v1/deployments/{id}/mirror`).** Rejected because mirror is
  per-app-pair, not per-deployment. The `{id}` here is the rule
  UUID; the `source` + `mirror` deployment IDs are *fields* on the
  rule body. The slug-scoped path keeps the IDOR guard cheap.
- **(b) PUT-only (no PATCH).** Rejected because PATCH lets
  customers toggle `enabled` without re-sending the whole body.
  Pointer fields preserve the patch semantics that
  `UpdateDeploymentTraffic` already uses for `traffic_percent`.
- **(c) Mirroring via a separate daemon.** Rejected — mirrors ride
  the same `gatewayd-internal` routing path as traffic split; no
  new daemon is needed.

## Rollback

Revert the PR-A2 commit cluster. The CRUD surface is purely additive
(no schema migration, no partial index) — the rollback is clean.
Any rules created in the A2 window would still be reachable via
PR-A1's storage methods but ignored by the (pre-A3) gateway; the
reaper disables them on the next read window.

## Verification

15 new pinned tests (11 handler + 4 CLI), plus the openapi.yaml
schema + route entries the spec_compliance_test gates on. The
end-to-end operator path is exercised in PR-A3 via
`cmd/e2e/provision_mirror_test.go` (out of scope here).

| Test | Pins |
|---|---|
| `TestCreateMirrorRule_HappyPath` | Free→403; Hobby→403; Pro→201; Scale→201 |
| `TestCreateMirrorRule_QuotaEnforced` | Pro cap=1: 2nd POST → 422; Scale cap=3: 4th POST → 422 |
| `TestCreateMirrorRule_RangeCheck` | percent=-1/101 → 422; percent=100 → 201 |
| `TestCreateMirrorRule_DeploymentsNotLive` | CreateDeployment with no MarkLive → 409 |
| `TestCreateMirrorRule_SourceTargetSame` | POST with source==mirror → 422 |
| `TestCreateMirrorRule_CrossAppMismatch` | source.app_id != mirror.app_id → 422 |
| `TestUpdateMirrorRule_PatchSemantics` | pointer fields: omit Percent keeps existing; explicit 0 → 0; explicit 100 → 100 |
| `TestDeleteMirrorRule_CascadesResults` | InsertMirrorResult then DeleteMirrorRule → result row gone (FK CASCADE) |
| `TestGetMirrorRule_IDOR` | rule owned by account B; GET from account A → 404 (not 403, not 422) |
| `TestMirrorSummary_WindowParam` | `?window=1h` parses; `?window=garbage` → 422 |
| `TestMirrorRule_NotifyEmitted` | happy-path POST calls `s.notif.Notify` with `kind="mirror"` payload |
| `TestMirrorRule_AuditEmitted` | kind=`mirror_rule.created` payload includes source/mirror/percent/include_body; updated emits `prev`; deleted emits the rule IDs only |
| `TestCmdMirrorList_HappyPath` | GET /v1/apps/{slug}/mirrors; path interpolation |
| `TestCmdMirrorUpdate_PatchSemantics` | only flagged fields end up in the body |
| `TestCmdMirrorSummary_WindowDefault` | no `--window` → `window=1h` |

## Follow-ons (PR-A3 and beyond)

Per ADR-125:

1. Multi-target mirror (N candidates per source).
2. Dashboard widget: mirror drift over time.
3. Retention sweeper for `mirror_invocation_results` (>7d rollup
   into hourly `mirror_invocation_summary`).
4. Mirror across deployments on different apps.
5. Mirror across regions.
6. Auto-promote canary on zero diff over rolling window.
