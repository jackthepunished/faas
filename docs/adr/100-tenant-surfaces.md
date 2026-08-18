# ADR-100: Tenant surfaces (multi-tenant hostname routing)

Status: proposed (PR-0 of the [cluster outline](./100-pr-cluster-outline.md))
Track: Tier A — gateway/multi-tenancy
Supersedes: ADR-028 line 182-184 deferral of "Multi-tenant gatewayd routing
policies" (see the Amendment below).
Issue: [#879](https://github.com/poyrazK/faas/issues/879)

## Context

Gregale today supports a one-app-one-cert custom-domains model via the
`custom_domains` table (`migrations/00001_init.sql:106`). That covers the
case of one customer exposing one brand on one Gregale app. It does NOT
cover the SaaS pattern where a single Gregale account exposes one managed
surface that fans out to N end-customer hostnames — `api.customer-a.com`,
`api.customer-b.com`, `api.customer-c.com` — all sharing a single Gregale
managed certificate and resolving to a single Gregale app.

The absence is documented: `docs/adr/028-gatewayd-remote-routing.md`
line 182-184 deliberately defers "Multi-tenant gatewayd routing policies"
out of scope. This ADR lifts that deferral and defines the v1 entity
(`tenant_surface`), the cert engine, the routing-layer plumbing, and the
quota shape. The customer-zone wildcard cert flavor is wired in the
schema but the DNS-01 solver ships in a separate ADR.

## Decision

Introduce a new first-class entity, the **tenant surface**, that groups
N verified hostnames under one managed certificate and routes them to one
app. A surface is owned by an account and pinned to exactly one app.
The owner (apid) writes `tenant_surfaces` + `tenant_hostnames`; the cert
engine wakes on notification, mints one cert with per-host SANs (up to the
Let's Encrypt 100-SAN cap), and `gatewayd-internal` gains a routing branch
in `pgRouter.ResolveHost` that resolves a request through the surface table
before falling through to `custom_domains`.

## Sub-decisions

- **D1 — One surface ↔ one app.** `tenant_surfaces.app_id` is `NOT NULL`.
  The multi-app variant (a surface routing to N apps by hostname pattern)
  is a future feature.
- **D2 — Hetzner-only DNS-01 in v1.** Cert-mint pulls the existing
  `pkg/gateway/dns01_hetzner.go:43-244` factory. Cloudflare / Route53
  follow in the deferred ADR.
- **D3 — Re-mint on surface create / hostname add / hostname remove.**
  The cert engine subscribes to `NotifyTenantSurfaceChanged` and
  re-issues the SAN cert when the underlying set mutates. The alternative
  — feeding SANs to the on-demand `GetCertificate` closure — is documented
  as a future optimization if re-mint frequency becomes a bottleneck.
- **D4 — Routing order in `pgRouter.ResolveHost`:**
  `slugFor` → `SurfaceByHostname` → `DomainByName` → preview parser.
  Customer surfaces never collide with a single-app owner because the
  surface branch precedes the legacy `DomainByName` branch.
- **D5 — Quotas live in `pkg/api/limits.go`.** Per-account surfaces
  (0/1/5/25 by plan) and per-surface hostnames (10/50/250/1000 by plan)
  are first-class fields on `Limits` so the cluster never inlines a
  number. See the `TenantSurfaces*` rows added in PR-0.

## Amendment (ADR-028 line 182-184)

ADR-028 line 182-184 deferred "Multi-tenant gatewayd routing policies" out
of scope. This ADR lifts that deferral. The amendment is in effect for
v1 of the cluster: surfaces are part of the gateway routing layer;
they share the cert engine and the pgRouter routing table; the §5.3
secrets-in-rest posture is unchanged. ADR-028 stays canonical for its
other decisions (remote routing at the host boundary, gRPC over unix
sockets, etc.).

## Cert engine shape

- `pkg/gateway/cert_issuer.go` (new) — `CertIssuer.RequestCertForSurface(ctx, surfaceID)`:
  - loads the surface and its hostnames,
  - verifies **every** hostname is verified (fail-closed otherwise),
  - branches on `cert_kind`:
    - `per_host_san` (default): `magic.Obtain(certmagic.Certificate{Name: primary, SANs: altNames})`,
    - `per_host` (fallback when SAN-set > 100): today's one-cert-per-hostname path,
    - `shared_wildcard`: returns `ErrUnsupportedCertKind` — the deferred
      ADR's solver is not in v1.
- `pkg/gateway/cert_expiry.go` — add `CertExpiry(ctx, surfaceID) (time.Time, error)`
  that reads the on-disk cert file (mirror `refreshCertExpiryOnce` at
  `cert_expiry.go:173`) and returns the not-after timestamp. PR-C wires
  the `cert_expires_at` field into the DTO so the customer can see the
  cert health.

## Amendment (PR-A, 2026-08-13)

PR-A implements the cert engine stub, the state surface, the apid
notification plumbing, and the TXT verifier. Four open questions were
resolved during the implementation; this amendment records the choices
and their consequences so the PR-B routing layer and PR-C HTTP surface
build on a stable contract.

### Q1 — Who receives `tenant_surface_changed` notifications?

**Decision:** a dedicated subscriber goroutine alongside the existing
`watchInvalidations` switch.

The pg_notify consumer in `cmd/gatewayd-internal/backend.go` extends its
`invalidator` interface so the in-process handler (`handleInvalidation`)
can dispatch to the cert engine on `db.NotifyTenantSurfaceChanged`
without the cert engine importing `pkg/state`. This keeps the cert
engine pluggable (production wires `TenantSurfaceCertIssuer`; tests
inject a fake) and avoids shipping an out-of-process HTTP bridge
between gatewayd-internal and the cert workflow.

### Q2 — Cert engine shape today (no certmagic dependency yet)

**Decision:** ship the state-side shell in PR-A; defer the certmagic
`magic.Obtain(...)` call to a follow-up ADR that bundles the CA
dependency.

The PR-A `TenantSurfaceCertIssuer` validates the full input set — soft-
delete state, cert_kind against the closed supported set, verified-
hostname count, sort-by-hostname SAN determinism — and writes
`cert_state=failed` with a clear `last_error` so the wiring is visible
to operators without a live certmagic client. The follow-up ADR only
needs to fill the "mint succeeded" branch; the error branches are
already exercised by tests. Cert renewals on `shared_wildcard` remain
out of scope (Q4 below); the v1 PR-A engine refuses `shared_wildcard`
at the last line.

### Q3 — Where does the tenant-hostname TXT verification run?

**Decision:** extend `cmd/apid/dns_poller.go::runVerifyOnce` in place;
do not stand up a separate goroutine.

The custom-domain and tenant-hostname paths share the same
`_faas-verify.<hostname>` TXT record format and the same `checkTXT`
helper, so a single goroutine polls both. The branch is gated on
`api.TenantSurfacesEnabled()` so the feature flag suppresses the
poller's load when the surface module is dark. The lookup itself is
indirected through a package-level `txtLookupFunc` so a unit test can
inject a fake `net.Resolver` without monkey-patching globals.

### Q4 — Where is `shared_wildcard` rejected?

**Decision:** schema accepts; issuer rejects.

The migration at slot 00243 includes `cert_kind` with all three values
in the CHECK constraint (forward compatibility — a customer upgrading
a surface from `per_host_san` to `shared_wildcard` does not require a
migration). The apid handler validates `CreateTenantSurfaceRequest` and
returns `CodeTenantSurfaceCertKindInvalid` (RFC 7807, 400) before write.
The `TenantSurfaceCertIssuer` is the last-line reject: a
`shared_wildcard` surface that slips past the API still lands in
`cert_state=failed` with a `last_error` naming the deferred ADR. This
mirrors the "schema-accepts-API-rejects-issuer-backstops" layering used
elsewhere in the project (e.g. edge rules: ADR-091 D1-D25).

### Implementation outcomes

- **Migration slot:** `00243_tenant_surfaces.sql` (the PR-0 fence at
  00238 was promoted; the cross-PR slot precheck ran before each push
  — see `pr-867-seven-cycle-renumber` for the fence pattern).
- **Feature flag:** `FAAS_TENANT_SURFACES_ENABLED` (env, parsed by
  `api.TenantSurfacesEnabled`). Until PR-C ships, the flag defaults
  off; the apid routes refuse `POST /v1/.../tenant-surfaces`, the
  poller skips the tenant-hostname branch, and the gateway cert engine
  is `nil`-safe (no remint goroutine, no LISTEN registration).
- **State surface:** `pkg/state/{store,types}.go` gain 10 new methods
  on the `Store` interface; `*MemStore` and `*PgStore` mirror them.
  The `*QuotaError` shape is reused (no new typed error); RFC 7807
  codes `CodeTenantSurfaceQuota`, `CodeTenantHostnameQuota`,
  `CodeTenantHostnameAlreadyClaimed`, `CodeTenantSurfaceCertKindInvalid`,
  and `CodeTenantSurfacesNotAllowed` (HTTP 402 for the plan-gate off)
  are added to `pkg/api/errors.go`.
- **Cert engine:** `pkg/gateway/cert_issuer_tenant_surface.go` is the
  production impl; `pkg/gateway/cert_issuer.go` owns the interface
  (`CertIssuer.RequestCertForSurface(ctx, surfaceID)`); `pkg/gateway/
  pgbackend.go` carries the optional cert issuer via `WithCertIssuer`;
  `cmd/gatewayd-internal/backend.go` dispatches the invalidation. The
  Prometheus counter `gateway_tenant_surface_cert_total{result,kind}`
  is pre-instantiated across `result ∈ {issued, failed, skipped}` and
  `kind ∈ {per_host_san, per_host, shared_wildcard}` so dashboards
  render the closed cartesian from t=0.
- **Verifier:** the `dns_poller` extension is feature-flagged, fails
  open on DNS errors (stays pending), and emits the gateway-friendly
  log line `tenant hostname verified hostname=... surface=...` so the
  operator can correlate.

### Open work carried forward

- PR-A does **not** stand up the surface HTTP routes (`POST
  /v1/apps/{slug}/tenant-surfaces` etc.). Those land with PR-C.
- `pgRouter.ResolveHost` is **not** extended yet; PR-B adds the
  surface branch between `slugFor` and `DomainByName`. PR-A's
  visibility is the state layer + cert-state transitions + verifier.
- `pkg/gateway/cert_expiry.go::CertExpiry` is **not** added yet — the
  certmagic dependency ship with the follow-up ADR, and the on-disk
  cert file reader for SAN-aggregated certs is deferred in kind.
- The CLI subcommand family (`gregale tenant-surfaces add|list|rm`) lands
  with PR-C alongside the apid routes.

## Consequences

- The `custom_domains` table stays as-is for the single-app single-cert
  path. New customers wanting the SaaS pattern use surfaces; existing
  single-domain customers do nothing.
- `pgRouter.ResolveHost` learns one new branch. The branch is positioned
  before `DomainByName` so a hostname covered by a surface never falls
  through to the legacy path.
- Cert renewals on `shared_wildcard` are explicitly out of scope; PR-A
  surfaces the API for it but the DNS solver is a follow-up ADR.
- `pkg/api/limits.go` grows three rows; no other quota site changes.

## Open follow-ups

- Wildcard cert flavor (`shared_wildcard`) — follow-up ADR.
- Multi-app surfaces — follow-up feature; the schema is forward-compatible.
- Dashboard surfaces page — Tier B dashboard work.
- Adaptive cert fan-out via on-demand alt-name closure — optimization.

## References

- Issue [#879](https://github.com/poyrazK/faas/issues/879).
- Cluster outline [`100-pr-cluster-outline.md`](./100-pr-cluster-outline.md).
- Spec §4.1.2.14 (tenant surfaces) — new section.
- ADR-028 line 182-184 (deferral being reversed).
- `pkg/gateway/tls.go:24-130` (TLS config + `OnDemandAllowlist`).
- `cmd/gatewayd-internal/backend.go:30-87` (`pgRouter.ResolveHost`).
