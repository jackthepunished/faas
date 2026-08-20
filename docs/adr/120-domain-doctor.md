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
