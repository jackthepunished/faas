# ADR-130 · Status-page wiring via /v1/internal/slo.json

- **Status:** proposed
- **Date:** 2026-08-24
- **Issue / PR:** [#599](https://github.com/poyrazK/faas/issues/599)
- **Decision:** Ship a Postgres-backed `status_incidents` table
  (migrations/00412), a `Store` interface triple
  (InsertStatusIncident / ResolveStatusIncident /
  ListOpenStatusIncidents), a `gatewayd-internal`
  `/v1/internal/slo.json` endpoint that composes the open
  incident list with meterd's loopback Prometheus exporter, a
  `gregalectl status incident` CLI subcommand, and a static
  status-page HTML that fetches the endpoint on load. The
  closed-set vocabulary at every layer (component, severity,
  message length) is enforced at the SQL CHECK layer so a
  typo at the CLI surface fails closed (23514).

## Context

Issue #599's three unanswered operator questions today:

1. **"Is the platform currently healthy?"** — operators today
   answer from a Twitter search, a customer ticket, or by
   running `kubectl` queries against their mental model of
   what "healthy" means. There is no canonical surface.
2. **"What components are degraded right now?"** — the
   apid/meterd/vmmd/schedd/gatewayd tower has no per-component
   health summary. Each daemon has its own `/healthz` (or
   doesn't), but the aggregate is invisible.
3. **"What's the customer-facing impact of an incident?"** —
   operators today narrate an outage in Slack / a tweet / a
   customer email. There's no canonical artifact the status
   page can render without manual curation.

The current observability surfaces are inadequate:

- **No status-incidents store.** Status updates today are
  written by hand on Twitter / a static blog. No canonical
  source of truth.
- **gatewayd-internal has no `/v1/internal/slo.json`** endpoint.
  The public-facing `/v1/apps/{slug}` etc. don't expose SLOs.
- **`deploy/statuspage/index.html` doesn't exist.** The status
  page is currently a third-party (Statuspage.io / BetterStack)
  that operators update manually.

The closed-vocab precedent (ADR-016) for the incidents
vocabulary — component ∈ {apid, schedd, vmmd, gatewayd, meterd,
imaged, builderd, faas-control-plane}, severity ∈ {degraded,
partial_outage, full_outage, maintenance} — bounds the cartesian
at 8 × 4 = 32 cells. The 1024-char message cap (CHECK) prevents
a paste of a 50 KB stack trace from bloating the response.

## Decision

### 1. `status_incidents` table (migrations/00412)

A Postgres table with:

- `id BIGSERIAL PRIMARY KEY` — surrogate key for the CLI.
- `component TEXT NOT NULL` — closed-set (apid, schedd, vmmd,
  gatewayd, meterd, imaged, builderd, faas-control-plane),
  enforced by `status_incidents_component_chk` CHECK.
- `severity TEXT NOT NULL` — closed-set (degraded,
  partial_outage, full_outage, maintenance), enforced by
  `status_incidents_severity_chk` CHECK.
- `message TEXT NOT NULL` — operator-authored free-text, capped
  at 1024 chars by `status_incidents_message_len_chk` CHECK.
- `posted_at TIMESTAMPTZ NOT NULL DEFAULT NOW()`.
- `resolved_at TIMESTAMPTZ NULL`.
- Partial index `status_incidents_open ON status_incidents(component)
  WHERE resolved_at IS NULL` — for the hot read path.

Replay-safe (every CREATE / ALTER uses IF NOT EXISTS guards so
a partial-apply replay is idempotent — same convention as 00411
and 00264).

### 2. `Store` interface triple

`pkg/state/store.go` adds three methods to the `Store`
interface, implemented by both `PgStore` and `MemStore`:

- `InsertStatusIncident(ctx, component, severity, message) (StatusIncident, error)`
- `ResolveStatusIncident(ctx, id int64) error` — idempotent;
  re-issuing on an already-resolved row returns nil.
- `ListOpenStatusIncidents(ctx) ([]StatusIncident, error)` —
  uses the partial index.

`pkg/state/types.go` adds the `StatusIncident` struct + the
closed-set vocabulary as named constants so the CLI can
range-check before hitting the SQL CHECK.

### 3. `gatewayd-internal` `/v1/internal/slo.json` endpoint

`cmd/gatewayd-internal/handlers/slo.go` (new) handles
`GET /v1/internal/slo.json`. The response composes:

```json
{
  "api_availability": {"ratio": 0.9987, "target": 0.999},
  "wake_latency": {"p95_seconds": 0.31, "target": 0.35},
  "build_success": {"ratio": 0.992, "target": 0.99},
  "degraded": false,
  "incidents": [...]
}
```

The SLO ratios come from meterd's loopback Prometheus exporter
(port 9100 on the meterd cgroup). `degraded: true` fires when
any SLO ratio is below target OR any open incident is in the
list. The handler caches for 15s via `Cache-Control: max-age=15`
so a status-page refresh doesn't hammer the DB.

### 4. `gregalectl status incident` CLI

`cmd/gregalectl/incident.go` (new) adds two subcommands:

- `gregale status incident post --component=<X> --severity=<Y> --message=<Z>`
- `gregale status incident resolve <id>`

The CLI range-checks against the closed-set constants in
`pkg/state/types.go` BEFORE hitting the SQL CHECK, so a typo
prints a friendly error instead of the SQL 23514.

### 5. `deploy/statuspage/index.html`

A static HTML page that:

- Fetches `/v1/internal/slo.json` on load (with a 15s refresh
  interval).
- Renders a green / yellow / red banner based on the
  `degraded` flag.
- Renders the open incidents list with `component`,
  `severity`, `message`, `posted_at`.
- Inline SVG (no JS chart deps) for the three SLO ratio
  sparklines.

### 6. `docs/runbooks/StatusPageDegraded.md`

Mirrors the `FaasApidAuditWriteFailures.md` template
(Symptom · Why now · Triage 3-signal ladder · Mitigate ·
Follow-up). Names the four recurring failure modes:

- **Operator filed an incident** — verify the underlying
  trigger, resolve when fixed.
- **Automated post** (degraded: true flag) — check which SLO
  is below target, investigate.
- **Stale incident** — operator forgot to resolve; clean up
  weekly.
- **Multiple components in one incident** — file a separate
  incident per component OR link via the `message` field.

## Consequences

- The closed-set vocabulary (8 components × 4 severities)
  bounds the table's vocabulary surface — operators can't
  invent new values at the CLI without updating both the
  Go constant AND the SQL CHECK (canonical DROP+ADD pair).
- The 1024-char message cap prevents a paste-bomb attack at
  the CLI surface.
- `degraded: true` flag on the endpoint lets a downstream
  consumer (the status page, a Slack webhook, an on-call
  routing engine) react to the binary "is the platform
  healthy right now" signal without parsing the SLO ratios.
- The partial index `status_incidents_open` keeps the hot
  read path (every status-page load) bounded — closed
  incidents stay in the table for audit but don't bloat the
  working set.
- The `gregalectl` CLI surface replaces manual Twitter /
  blog updates — one canonical artifact, fully
  version-controlled, fully replay-able via `gregale status
  incident resolve <id>`.

## Out of scope

- **Automated posting of incidents** when the alertmanager
  `severity: page` route group fires. A follow-on ADR +
  PR wires the alertmanager webhook to the CLI surface.
- **Customer notification email** — separate from the
  status page; out of scope here.
- **Multi-region status pages** — single-region today;
  multi-region is a separate ADR (ADR-066 follow-on).
- **Resolved-incident retention** — closed rows stay in
  the table forever. A vacuum job is a separate ops
  decision.

## References

- ADR-016 (closed-set label vocabulary).
- ADR-127 / ADR-128 / ADR-129 / ADR-131 (sibling ADRs; same
  mega-PR boundary).
- `migrations/00412_status_incidents.sql` (the table).
- `pkg/state/types.go` `StatusIncident*` constants (the Go
  vocabulary).
- `cmd/gatewayd-internal/handlers/slo.go` (the endpoint).
