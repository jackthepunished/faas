# ADR-120 — Domain doctor (issue #961 follow-on)

- **Status:** proposed
- **Date:** 2026-08-18
- **Decision:** A new `domain_doctor_observations` table persists
  per-domain probe results from a new probe engine. The existing
  `dns_poller` writes the rows on its 30 s tick. A new read-only
  endpoint `GET /v1/domains/{domain}/doctor` returns the latest row
  plus remediation copy. A new CLI subcommand
  `gregale domains doctor <domain>` renders the report. The
  `api.DomainDoctorEnabled()` env flag gates the poller branch
  (mirroring `api.TenantSurfacesEnabled()`). The cert engine
  remains the SOLE writer of `tenant_surfaces.cert_state`; the
  doctor only reads it.
- **Why:** Issue #961 / Mega-A PR-3 (shipped 2026-08-18) gave
  Gregale a `gregale domains verify` / `show` surface that
  answers the question "did it verify?" with a 422 and a single
  reason string. Custom-domain setup is a major activation
  drop-off point and Render-style competitors treat verification
  + automatic TLS as core product behavior, surfacing a doctor
  with parallel checks (DNS found / points to us / TLS / CAA /
  IPv6 conflict) and the exact record to change. Today 1.5 of
  those 5 checks exist; the others are absent repo-wide (zero
  `CAA` / `LookupAAAA` hits in `pkg / cmd / migrations / sdk /
  docs`). The cert status that PR-3 added
  (`CustomDomainResponse.CertStatus` at `pkg/api/dto.go:1663-1676`)
  is not persisted — it is reconstructed by a live port-443 dial
  on every request, which means a stuck "waiting for DNS" domain
  has no "since 09:14" story, no alertable signal, and re-dials
  the customer's edge on every dashboard page view. Persisting
  probe results turns PR-3's 422 reason path into a queryable,
  alertable, dashboardable signal and closes the activation
  drop-off.
- **Consequences:**
  - New schema: `migrations/00296_domain_doctor_observations.sql`
    — single new table (one row per domain, last-writer-wins)
    with FKs to `custom_domains` (`ON DELETE CASCADE`) and
    `tenant_surfaces` (`ON DELETE SET NULL`), a closed-set
    `cert_state` CHECK including a `dial_failed` token
    re-used from PR-3's `CustomDomainResponse.CertStatus` enum,
    and per-check observation timestamps.
  - New probe engine in `cmd/apid/dns_probes.go` with five
    package-level test seams (`aLookupFunc`, `aaaaLookupFunc`,
    `caaLookupFunc` siblings of the existing
    `cnameLookupFunc` and `dialCertFunc` at
    `cmd/apid/dns_verify.go:51,86`).
  - New endpoint `GET /v1/domains/{domain}/doctor` reusing
    `loadDomain` (`cmd/apid/handlers_ext.go:1722`) for the
    IDOR-safe load. The handler reads the latest observation
    row; if older than `FAAS_DOMAIN_DOCTOR_TTL_SECONDS`
    (default 300), it triggers a synchronous re-probe with a
    5 s budget so the 99% case is a no-op DB read.
  - New CLI subcommand `gregale domains doctor <domain>` in
    the customer-facing `gregale` binary, NOT the operator
    `gregalectl doctor` (different audience, different
    binary, different abstraction). The `cli_meta.go` manifest
    entry is updated per the dual-registration rule
    (`gregalectl-dispatch-manifest-completeness.md`).
  - New DTO + error code: `api.DomainDoctorReport` and
    `api.CodeDoctorUnavailable` /
    `api.ErrDoctorUnavailable`. New SDK method
    `Client.DomainDoctor` in BOTH `pkg/api/client.go` and the
    hand-mirrored `sdk/go/internal/api/client.go` (per the
    PR-3 partial-mirror finding, the regen pipeline is not
    yet end-to-end).
  - New env var: `FAAS_DOMAIN_DOCTOR_TTL_SECONDS` in
    `cmd/apid/config.go` next to the existing
    `verifyInterval = 30 * time.Second` const
    (`cmd/apid/dns_poller.go:22`).
  - Cert engine ownership unchanged: the
    `pkg/gateway/cert_issuer_tenant_surface.go` writers at
    lines 203-316 remain the sole writers of
    `tenant_surfaces.cert_state`. The doctor reads via
    `s.store.GetTenantSurfaceByID` and never writes.
  - Cross-PR slot precheck: PR #910 (triggers, ADR-100
    amendment) holds 00277-00279 + 00288-00292 fences; PR #978
    (issue #975 mega foundation) holds 00288-00295 reservations
    plus its real 00293_validate_mode.sql schema; main is at
    00287. Safe slot is 00296 — the highest unowned integer above
    the 00288-00295 cluster. The precheck
    (`git ls-tree refs/pull/<N>/head -- migrations/` for all
    open PRs) must be re-run at branch-open time.
- **Rejected alternatives:**
  1. **Live fan-out per request** — no history, no alerts,
     re-dials the customer's edge on every dashboard page
     load. This is what PR-3's `verifyDomain` does today, and
     it's the reason `stale` and `dial_failed:<reason>` are
     wire-shape afterthoughts rather than first-class state.
  2. **Extend `custom_domains` and `tenant_hostnames` with
     observation columns** — pollutes the customer-intent
     tables with telemetry, requires parallel column sets
     across the two surface families, and the two tables
     have different ownership rules (`custom_domains` is
     legacy, `tenant_hostnames` is ADR-100 go-forward).
  3. **Extend the operator `gregalectl doctor`**
     (`cmd/gregalectl/commands_doctor.go`, PR #921) — wrong
     audience (operator vs customer), wrong binary, wrong
     abstraction (cluster/package diagnostic vs per-domain
     customer diagnostic). The output shapes
     (`doctorFinding{Check, Severity, Target, Message,
     Detail}` at `commands_doctor.go:128-136`) are borrowed
     as a stylistic reference only.

## Tier A rollout (2026-08-20, post-soak)

The mega-PR shipped the schema, probe engine, poller branch,
endpoint, and CLI subcommand behind
`FAAS_DOMAIN_DOCTOR_ENABLED=false`. After a 7-day production
soak with no regressions, a Tier-A follow-up PR moves the
doctor from "wired but inert" to "operator observes it on a
dashboard, an alert fires when it goes stale, default-on."
The three tiers are individually-revertable so a regression
in one tier doesn't block the other two.

### Tier A1 — observability

- **New gauge `apid_domain_doctor_oldest_observation_seconds`**
  (cmd/apid/dns_poller.go::emitDoctorOldestObservationGauge).
  The dns_poller Sets the wall-clock age of the oldest row
  in `domain_doctor_observations` after each pass. Cold start
  (empty table) → 0. Healthy loop → ~30 s. Stalled loop → the
  value freezes and Prometheus's
  `time() − timestamp(gauge) > X` expression pages on-call.
- **New counter `apid_domain_doctor_skipped_flag_disabled_total`**
  (cmd/apid/dns_poller.go::emitDoctorSkip). Bumped once per
  dns_poller tick when the operator has set the env var to a
  falsy value. Backs the FaasDomainDoctorDisabledByOperator
  info alert so an explicit opt-out surfaces in Alertmanager
  without paging.
- **New Store method `OldestDoctorObservation(ctx) (time.Time, error)`**
  on `state.Store` (PkgStore + MemStore). Hand-rolled (not
  sqlc) — the SQL is a single MIN(observed_at) scan.
- **New OpsMetrics getter methods**
  `DomainDoctorOldestObservationSeconds()` and
  `DomainDoctorSkippedFlagDisabled()` on `*wire.OpsMetrics`,
  nil-safe (the dns_poller nil-checks `s.ops` first).
- **New Prometheus alerts** at
  deploy/ansible/roles/prometheus/files/faas.rules.yml:
  - `FaasDomainDoctorStalled` (page, `for: 30m`,
    `> 1560s` stale)
  - `FaasDomainDoctorStretched` (warn, `for: 30m`,
    `> 90s` stale — cadence is broken but the loop is alive)
  - `FaasDomainDoctorDisabledByOperator` (info,
    `for: 1h`, `rate(skipped_total[5m]) > 0` — explicit opt-out)
  Mirrors the canonical "stuck X" precedent at
  `FaasAuditRetentionLoopStalled` / `Stretched`
  (deploy/ansible/roles/prometheus/files/faas.rules.yml:413-448).
- **New operator runbook**
  docs/runbooks/FaasDomainDoctorStalled.md — 30-second
  summary, 5-step triage, 4 linked docs (this ADR, flags.go,
  dns_poller.go, the dashboard template).

### Tier A2 — customer dashboard surface

- **New dashboard route** `GET /dashboard/apps/{slug}/domains/{domain}/doctor`
  (cmd/apid/handlers_dashboard.go::parseDomainDoctorPath +
  renderDomainDoctor). IDOR posture mirrors
  `renderDeploymentDetail` (AppBySlug + AccountID rejection +
  loadDomain ownership check, 404 not 403 on cross-tenant).
- **New dashboard template**
  pkg/dashboard/templates/domain_doctor.html — htmx-loaded
  + CSP-nonce'd + inline `<style>` (TypeScript-free). Renders
  the 5-check table with per-row glyph + `observed:` + `→ fix:`
  lines, a `stale-banner` when the observation row is older
  than `FAAS_DOMAIN_DOCTOR_TTL_SECONDS`, and a
  `docs →` link to docs.gregale.dev/domains/doctor.
- **New dashboard structs**
  `pkg/dashboard.DomainDoctorView` + `DashboardDoctorCheck`
  mirror the wire DTO verbatim so a future regen can swap the
  type without rewriting the template.

### Tier A3 — default-on cutover

- **Default flip**: `pkg/api/flags.go::DomainDoctorEnabled`
  returns `true` when `FAAS_DOMAIN_DOCTOR_ENABLED` is unset
  or empty. The explicit-off token set
  (`0` / `false` / `no` / `off`) keeps the operator escape
  hatch — the dns_poller's `emitDoctorSkip` bumps the
  counter so the opt-out is observable via
  `FaasDomainDoctorDisabledByOperator`.
- **Unknown tokens default-on**: any token outside the
  explicit-off set returns true so a typo doesn't silently
  turn the doctor off. Mirrors `TestCertEngineStagingDefaultsOn`'s
  safe-default posture.
- **Test coverage**: `pkg/api/flags_test.go` adds 4 new cases
  (`TestDomainDoctorEnabledDefaultsOn`,
  `AcceptsOnTokens`, `AcceptsExplicitOffTokens`,
  `IgnoresUnknownTokens`); `cmd/apid/handlers_doctor_test.go`
  adds 6 new cases covering the 503 doctor_disabled path, the
  happy-path 5-row report, IDOR (cross-tenant 404), stale-row
  response shape, parser, and the dashboard route render.

### Rollout posture

The cutover is irreversible by feature flag alone — operators
who want to disable the doctor MUST set
`FAAS_DOMAIN_DOCTOR_ENABLED=false` (or any other explicit-off
token). This is the intended Tier-A3 semantics per ADR-120:
the dark-launch was a soak-only construct, not a permanent
switch. On-call engineers triaging
`FaasDomainDoctorDisabledByOperator` should not interpret the
info alert as a customer-impacting incident — the opt-out is
operator choice.
