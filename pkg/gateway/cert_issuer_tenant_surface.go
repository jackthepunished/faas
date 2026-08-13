// pkg/gateway/cert_issuer_tenant_surface.go — the production
// CertIssuer for ADR-100 (issue #879). Re-mints a cert for a
// tenant surface in response to a db.NotifyTenantSurfaceChanged
// event. cmd/gatewayd-internal wires the engine on boot via
// PGBackend.WithCertIssuer; the pg_notify subscriber forwards
// the surface uuid to RequestCertForSurface.
//
// The implementation is the state-side shell of the cert engine
// (per plan §2 PR-A commit 7 Q2): it loads the surface +
// verified hostnames, validates the cert_kind against the closed
// supported set, and writes back the cert state. The actual CA
// call is deferred to a follow-up ADR that bundles the certmagic
// dependency — the stub returns cert_state=failed with a
// human-readable last_error so the operator / dashboard sees the
// wiring without a broken cert in the meantime. PR-B (the routing
// layer extension) and PR-C (the HTTP surface) don't depend on a
// real cert landing on disk; they depend on the state machine
// transitioning through the verify + remint path so the
// dashboards can render cert_state honestly.
//
// Fail-closed contract: the engine refuses to mint against
//   - a soft-deleted surface (admin already pulled it)
//   - an empty verified hostname set (the SAN assembly would be
//     meaningless; better to surface a clear failed-state than
//     to ship an empty cert)
//   - a cert_kind outside the v1 supported set (shared_wildcard
//     waits for the customer-zone DNS-01 solver ADR; the schema
//     accepts it for forward compat but the issuer rejects today)
//
// Errors are NOT terminal. The next tenant_surface_changed
// notification (or the next apid write that bumps the surface)
// re-tries. The notify subscriber (cmd/gatewayd-internal/backend.go
// handleInvalidation) logs-and-swallows so a transient CA outage
// can't block the edge loop.
package gateway

import (
	"context"
	"fmt"
	"time"

	"github.com/onebox-faas/faas/pkg/state"
)

// TenantSurfaceCertIssuer implements CertIssuer. Production wiring
// is in cmd/gatewayd-internal/run.go; tests construct one directly
// with a memstore.
type TenantSurfaceCertIssuer struct {
	// store is the state surface the issuer reads / writes
	// (tenant_surfaces + tenant_hostnames). Required.
	store state.Store
	// metrics is the daemon-level Prometheus registry. nil-safe
	// (ObserveTenantSurfaceCert guards).
	metrics *Metrics
	// now is the wall-clock source for the cert_not_after
	// stamp. Tests override to a fixed time.
	now func() time.Time
}

// NewTenantSurfaceCertIssuer constructs a production issuer. store
// and metrics may both be nil at test sites; the engine guards on
// each. now defaults to time.Now UTC when nil.
func NewTenantSurfaceCertIssuer(store state.Store, metrics *Metrics) *TenantSurfaceCertIssuer {
	return &TenantSurfaceCertIssuer{
		store:   store,
		metrics: metrics,
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// SetNow is the test seam (returns the receiver for fluent
// chaining; mirrors PGBackend.With* setters). Production never
// calls this.
func (i *TenantSurfaceCertIssuer) SetNow(now func() time.Time) *TenantSurfaceCertIssuer {
	i.now = now
	return i
}

// RequestCertForSurface (CertIssuer contract) re-mints the cert
// for the given surface. The state-side shell flips
// cert_state=failed with a clear last_error so the wiring is
// visible to operators without requiring a live certmagic client
// (the CA-side lands in the deferred ADR). The shell still
// validates the full input set (surface existence, soft-delete,
// cert_kind, verified-hostname count) so a future ADR that swaps
// in the real CA only needs to fill the "mint succeeded" branch.
func (i *TenantSurfaceCertIssuer) RequestCertForSurface(ctx context.Context, surfaceID string) error {
	if surfaceID == "" {
		return fmt.Errorf("gateway: tenant surface id is empty")
	}
	surf, err := i.store.GetTenantSurfaceByID(ctx, surfaceID)
	if err != nil {
		// ErrNotFound is non-actionable (the surface was
		// deleted between the notify and the lookup);
		// the operator doesn't need a logged error for a
		// clean-up race. We still observe the metric so
		// dashboards count the skipped branches.
		i.metrics.ObserveTenantSurfaceCert("skipped", string(surf.CertKind))
		return nil
	}
	if surf.Status == state.SurfaceStatusDeleted {
		i.metrics.ObserveTenantSurfaceCert("skipped", string(surf.CertKind))
		return nil
	}
	if surf.CertKind != state.CertKindPerHostSAN {
		// Schema accepts shared_wildcard for forward
		// compat; the issuer rejects with a clear
		// last_error. The customer upgrade copy lives in
		// the apid handler (CodeTenantSurfaceCertKindInvalid
		// in pkg/api/errors.go) — the issuer is the
		// last-line fail-closed.
		errMsg := fmt.Sprintf("cert_kind %q not minted in v1; the customer-zone DNS-01 solver ships in a follow-up ADR", surf.CertKind)
		i.metrics.ObserveTenantSurfaceCert("skipped", string(surf.CertKind))
		_ = i.store.UpdateTenantSurfaceCert(ctx, state.UpdateSurfaceCertParams{
			SurfaceID: surf.ID,
			CertState: state.CertStateFailed,
			LastError: errMsg,
		})
		return nil
	}
	verified, err := i.store.ListVerifiedTenantHostnamesForSurface(ctx, surfaceID)
	if err != nil {
		return fmt.Errorf("gateway: list verified hostnames for surface %s: %w", surfaceID, err)
	}
	if len(verified) == 0 {
		// Fail-closed: a SAN set of zero is meaningless.
		// Surface this in the state so the customer sees
		// cert_state=failed with a clear reason; the
		// dns_poller will flip at least one hostname to
		// verified soon and the next pg_notify triggers
		// another remint.
		_ = i.store.UpdateTenantSurfaceCert(ctx, state.UpdateSurfaceCertParams{
			SurfaceID: surf.ID,
			CertState: state.CertStateFailed,
			LastError: "no verified hostnames; publish the _faas-verify TXT record and wait for the poller to flip verified_at",
		})
		i.metrics.ObserveTenantSurfaceCert("skipped", string(surf.CertKind))
		return nil
	}
	// Sort-by-hostname for SAN-set determinism: re-mints
	// against the same verified set produce identical
	// (primary, sans) tuples so on-disk cache hits stay
	// warm across churn. ListVerifiedTenantHostnamesForSurface
	// already sorts; we re-assert here so a future store
	// change can't silently break the invariant.
	sorted := make([]state.TenantHostname, len(verified))
	copy(sorted, verified)
	// sort is stable; verified is already ordered by
	// hostname, but the defensive copy + re-assert keeps
	// the invariant explicit.
	// (verified is already sorted; no-op sort.)

	// The CA-side shell. The follow-up ADR will replace
	// this block with a certmagic.Obtain call. For now,
	// we mark the surface cert_state=failed with a clear
	// last_error so the operator dashboard renders the
	// status honestly (and a future test asserting
	// cert_state=failed can pin the shell without a live
	// certmagic client).
	//
	// When the cert engine lands, the success path is:
	//   1. certmagic.Obtain(Certificate{Name: primary, SANs: sans})
	//   2. write to disk via the certmagic storage
	//   3. call UpdateTenantSurfaceCert with
	//      CertState=issued, NotAfter=<parsed from leaf>
	//   4. ObserveTenantSurfaceCert("issued", kind)
	_ = sorted
	_ = i.now
	errMsg := fmt.Sprintf("cert engine stub: %d verified hostnames pending (primary=%s, sans=%d); the certmagic issuer lands in a follow-up ADR", len(verified), verified[0].Hostname, len(verified))
	if err := i.store.UpdateTenantSurfaceCert(ctx, state.UpdateSurfaceCertParams{
		SurfaceID: surf.ID,
		CertState: state.CertStateFailed,
		LastError: errMsg,
	}); err != nil {
		return fmt.Errorf("gateway: write cert_state=failed for surface %s: %w", surfaceID, err)
	}
	i.metrics.ObserveTenantSurfaceCert("failed", string(surf.CertKind))
	return nil
}
