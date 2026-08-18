# ADR-100 Amendment: Cert engine real-mint (PR-D cert-engine-real-mint)

Status: proposed (PR-D of the [ADR-100 cluster outline](./100-pr-cluster-outline.md))
Track: Tier A — gateway/multi-tenancy
Amends: [ADR-100](./100-tenant-surfaces.md) §"Cert engine shape"
Issue: [#879](https://github.com/poyrazK/faas/issues/879)

## Context

ADR-100 §"Cert engine shape" describes a v1 cert engine that bundles
up to `MaxSANPerCert` (100) verified hostnames into a single Let's
Encrypt order via the certmagic `ObtainCertSync` path, and falls back
to a per-hostname shape for verified sets above the cap. PR-D ships
the production cert engine and corrects the SAN-set shape against
what certmagic v0.25 actually exposes.

The certmagic v0.25.4 public Obtain path
([config.go:640 generateCSR(privKey, []string{name}, false)](https://github.com/caddyserver/certmagic/blob/v0.25.4/config.go))
generates a CSR for a single `name` per call. Multi-SAN orders are
only reachable through the on-demand TLS-handshake code path, which
is the wrong seam for the background "remint on pg_notify" engine
described in ADR-100. The on-demand path assumes an in-flight TLS
handshake that owns the answer channel; the background remint does
not.

PR-D therefore mints **one cert per hostname**. The
`cert_kind = per_host_san` value is preserved in the schema
(ADR-100 §"Cert kinds") for forward compatibility, but the
implementation is per-host. A future ADR-114 wires the multi-SAN
bundler once certmagic exposes a synchronous multi-SAN Obtain API.

This amendment records the four load-bearing deviations from
ADR-100:

1. Dependency bundle: certmagic v0.25.4, acmez/v3 (transitive),
   libdns v1.1.1.
2. Renewer shape: the renewer goroutine rides the existing
   `tenant_surface_changed` pg_notify pipeline by bumping the
   surface row's `updated_at` via `TouchTenantSurfaceForRenewal`;
   every replica touches; certmagic's per-host cache key lock
   deduplicates.
3. Per-host cert shape: see "San-set semantics" below.
4. `shared_wildcard` deferral: schema accepts the value
   (`migrations/00277_tenant_surfaces_per_host_kind.sql`) but the
   issuer rejects with a typed sentinel
   (`state.ErrUnsupportedCertKind`) until the customer-zone
   DNS-01 solver lands.

## Decision

### San-set semantics (the load-bearing deviation)

The wrapper at `pkg/gateway/cert_issuer_tenant_surface.go` flips
the surface `cert_state` through `none → pending → issued/failed`
exactly as ADR-100 describes, but the `IssueSet` call at
`pkg/gateway/cert_issuer_letsencrypt.go:IssueSet` mints one cert
per hostname and returns the soonest NotAfter across the issued
set. The MaxSANPerCert cap (encoded at `pkg/api/limits.go`) is
enforced in the wrapper before reaching the issuer so a future
SAN-bundler implementation is bounded by the same constant.

Per-host certs are individually renewable via the renewer and
individually observable via the
`gateway_tls_cert_expiry_by_host_seconds{kind="per_host_san"}`
gauge. A surface's `cert_not_after` column is the soonest cert in
the set so a renewer tick that hits the threshold re-mints the
whole set (not just the soonest cert).

### Dependency bundle

PR-D pins the following dependency bundle:

- `github.com/caddyserver/certmagic v0.25.4` — the v1 certmagic
  pin. v0.25 is the first line that exposes the synchronous
  `ObtainCertSync` API.
- `github.com/mholt/acmez/v3` (transitive via certmagic) — the
  ACME client certmagic wraps.
- `github.com/libdns/libdns v1.1.1` — the libdns adapter
  interface that the `dnsProvider` field on
  `LetsEncryptCertIssuer` accepts. Production wires a real
  Route53 / Cloudflare / etc. provider; the PR-D E2E wires a
  stub that records every call.

### Renewer shape

`pkg/gateway/cert_renewer.go::SurfaceCertRenewer` is the
background goroutine that re-mints surfaces whose
`cert_not_after` is within `CertRenewBeforeNotAfterDays` (30).
The renewer reads surfaces via the
`state.Store.ListTenantSurfacesNearingExpiry(ctx, cutoff)` method
that PR-D commit 3 adds, then bumps the surface row's
`updated_at` via `TouchTenantSurfaceForRenewal` so the existing
`tenant_surface_changed` pg_notify trigger fires. The pg_notify
subscriber at `cmd/gatewayd-internal/backend.go::handleInvalidation`
forwards the surface uuid to `RequestCertForSurface`, and the
wrapper runs the full mint path. certmagic's per-host cache key
lock deduplicates the underlying Obtain call when multiple
replicas touch the same surface in the same tick window.

The renewer rides the existing pg_notify pipeline rather than
calling `Issue` directly so the renewer + the customer-mutation
path converge on the same state-machine write path. A future
optimisation could short-circuit the bump when the surface was
already in `cert_state=pending`; today's shape is simpler and the
certmagic lock keeps the worst case O(1) under-load.

Multi-host behaviour: every gatewayd-internal replica starts a
renewer goroutine. They each run `ListTenantSurfacesNearingExpiry`
independently and each `TouchTenantSurfaceForRenewal` per due
surface. The pg_notify fan-out delivers each touch to every
replica, and the per-host cache key lock in certmagic prevents
duplicate mints. The wasted CA-handshakes in the worst case are
bounded by the replica count.

### Wire-name reconciliation

PR-D wires one new metric, one new gauge label, and three new
pg_notify-touchable surfaces:

- `gateway_tenant_surface_cert_total{result, kind}` (counter,
  PR-D commit 1): closed cartesian `{result ∈ {issued, failed,
  skipped}, kind ∈ {per_host_san, shared_wildcard, per_host, ""}}`
  pre-instantiated at boot. Mirrors the convention in
  `pkg/gateway/metrics.go`.
- `gateway_tls_cert_expiry_by_host_seconds{kind="per_host_san"}`
  (gauge, PR-D commit 2): per-host expiry, refreshed by
  `pkg/gateway/cert_expiry_surface.go::CertExpiry(ctx, store,
  storageDir, surfaceID)`.
- `state.Store.ListTenantSurfacesNearingExpiry(ctx, cutoff)
  ([]TenantSurface, error)` (interface, PR-D commit 3): the
  renewer's predicate reader.
- `state.Store.TouchTenantSurfaceForRenewal(ctx, id) error`
  (interface, PR-D commit 3): the renewer's pg_notify kick.
- `migrations/00277_tenant_surfaces_per_host_kind.sql`
  (migration, PR-D commit 5): widens the `cert_kind` CHECK to
  admit `per_host` for the future ADR-114 bundler shape.
- `state.ErrUnsupportedCertKind` (typed sentinel, PR-D commit 5):
  returned for `shared_wildcard` and `per_host` cert_kinds until
  ADR-114 lands.

### Fail-closed contract

The fail-closed invariants from ADR-100 are preserved:

- A soft-deleted surface is skipped (the state-machine writes
  are gated on `surf.Status != SurfaceStatusDeleted`).
- A surface with an empty verified hostname set is rejected with
  `cert_state=failed` and a `last_error` naming the missing TXT
  record (the dns_poller will flip at least one hostname to
  verified soon and the next pg_notify triggers another remint).
- A `cert_kind` outside the v1 supported set is rejected with the
  typed `state.ErrUnsupportedCertKind` sentinel so the apid
  handler can `errors.As` it uniformly.
- A verified-hostname count exceeding `MaxSANPerCert` is rejected
  with `cert_state=failed` and a clear `last_error`. The per-host
  path doesn't hit the cap today but the wrapper enforces it so
  a future SAN bundler is bounded.

### Errors are NOT terminal

The next `tenant_surface_changed` notification (or the next apid
write that bumps the surface) re-tries. The notify subscriber
(`cmd/gatewayd-internal/backend.go::handleInvalidation`)
logs-and-swallows so a transient CA outage can't block the edge
loop.

## Consequences

### Positive

- Real certs: PR-D unblocks the issue #879 customer-facing surface
  by giving the dark-launch flag a real CA-backed engine to flip
  on.
- Renewer converges on the existing pg_notify pipeline: no
  second subsystem to monitor.
- Per-host certs are individually observable; a customer
  dashboard showing "cert X expires 2026-09-15, cert Y expires
  2026-09-12" is a useful diagnostic when one cert goes stale.
- Fail-closed + audit emit (PR-D commit 6's
  `tenant_surface.cert_state_changed` row) preserves the
  observability contract ADR-035 mandates.

### Negative

- Per-host cert issuance costs `N` LE orders for an
  `N`-hostname surface. ADR-100's wording implied 1 order. A 50-
  hostname surface today costs 50 orders; LE's
  `new-order` rate limit is 50 / account / week. The renewal
  frequency (`CertRenewBeforeNotAfterDays=30`) keeps the steady-
  state load well below the cap (one renew per cert per 60 days,
  spread across the week), but a `>>MaxSANPerCert` cert set is
  not viable today.
- The per-host path means the surface's `cert_not_after` is the
  soonest cert, not a single shared expiry. A renewer tick that
  hits the threshold re-mints the whole set even if only one cert
  is near expiry. ADR-114's bundler collapses this to one
  per-set expiry.

### Neutral

- The `cert_kind = per_host_san` value is misleading today
  (per-host is the implementation). Kept for schema forward
  compat so the future bundler doesn't need a schema-touching
  migration.
- `shared_wildcard` and `per_host` are schema-valid but issuer-
  rejected. The customer-facing dashboard reads
  `cert_state=failed` and `last_error` to surface the deferral;
  no new RFC 7807 code is needed.

## Deferred (ADR-114 candidate)

- **Multi-SAN bundler**: when certmagic exposes a synchronous
  multi-SAN Obtain API (current state: not exposed; an upstream
  PR is the path). On landing, the bundler
  flips the `per_host_san` implementation from "N orders" to "1
  order, N SANs" and the LE rate-limit economics from "N orders
  per renew" to "1 order per renew".
- **Customer-zone wildcard DNS-01 solver**: the `shared_wildcard`
  cert_kind. The customer publishes a NS delegation to
  `tenant-zone.<surface>.<account>.gregale.app`; the solver
  validates the NS record and then drives the DNS-01 challenge
  against Gregale's authoritative servers for the customer zone.
  Schema is ready (`cert_kind IN ('per_host_san',
  'shared_wildcard', 'per_host')`); only the solver + issuer
  path remains.
- **`per_host` bundler**: today rejected with
  `state.ErrUnsupportedCertKind`. Future ADR-114 ships the
  per-host bundler (1 cert per hostname) as the explicit
  implementation of `cert_kind = per_host`.
- **Gatewayd-internal audit seam**: today the wrapper's
  `emitCertTransition` helper short-circuits on a nil auditor.
  The gatewayd-internal daemon has no equivalent of
  `cmd/apid/audit_subscriber.go`. Future commit wires a real
  `*audit.Auditor` at `cmd/gatewayd-internal/run.go::certIssuerFor`
  so the cert_state audit rows land in the `events` table under
  the `gatewayd-internal` actor.

## Cross-references

- [ADR-100](./100-tenant-surfaces.md) (amends §"Cert engine
  shape" + §"Cert kinds")
- [ADR-024](./024-certmagic-cutover.md) (the on-demand+SAN-
  aggregate model; this amendment's on-demand model converges
  there for the wildcard flow)
- [ADR-035](./035-auth-audit-events.md) (the audit emit contract
  the cert_state transition rows follow)
- [ADR-070](./070-tier-a7-edge-split.md) (the gatewayd-internal
  daemon that hosts the renewer + the pg_notify subscriber)
- [ADR-095](./095-pr-preview-environments.md) (the shape-matcher
  precedent for "reject with a typed sentinel, surface in
  dashboard")
- [ADR-114 (proposed)](./114-cert-real-mint-followups.md) (the
  follow-up ADR that ships the multi-SAN bundler +
  customer-zone DNS-01 solver + per-host bundler)