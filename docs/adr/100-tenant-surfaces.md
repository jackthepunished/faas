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
