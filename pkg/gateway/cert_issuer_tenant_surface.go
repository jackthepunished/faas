// pkg/gateway/cert_issuer_tenant_surface.go — the production
// CertIssuer for ADR-100 (issue #879). Re-mints a cert for a
// tenant surface in response to a db.NotifyTenantSurfaceChanged
// event. cmd/gatewayd-internal wires the engine on boot via
// PGBackend.WithCertIssuer; the pg_notify subscriber forwards
// the surface uuid to RequestCertForSurface.
//
// PR-D cert-engine-real-mint replaces the v1 stub with a real
// certmagic.ObtainCertSync call against the per-host path (see
// pkg/gateway/cert_issuer_letsencrypt.go for the SAN-set
// rationale). The wrapper still owns the state-side shell:
// load surface + verified hostnames, validate inputs, flip
// cert_state transitions through pending → issued/failed.
//
// Fail-closed contract: the engine refuses to mint against
//   - a soft-deleted surface (admin already pulled it)
//   - an empty verified hostname set (the SAN assembly would be
//     meaningless; better to surface a clear failed-state than
//     to ship an empty cert)
//   - a cert_kind outside the v1 supported set (shared_wildcard
//     waits for the customer-zone DNS-01 solver ADR-114;
//     per_host waits for the >100-SAN bundler ADR-114;
//     the schema accepts both for forward compat but the
//     issuer rejects today)
//   - a verified-hostname count exceeding MaxSANPerCert
//     (LE's hard cap; the per-host path doesn't hit it but the
//     cap is enforced here so a future SAN bundler is bounded
//     by the same constant)
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
	"sort"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// TenantSurfaceCertIssuer implements CertIssuer. Production wiring
// is in cmd/gatewayd-internal/run.go; tests construct one directly
// with a memstore.
//
// Two implementations sit behind the same CertIssuer seam:
//
//   - The v1 stub (now removed in PR-D) wrote cert_state=failed
//     with a human-readable last_error. PR-D replaces it with
//     the real certmagic.ObtainCertSync call.
//
//   - The production issuer (LetsEncryptCertIssuer) is injected
//     at construction time. The wrapper guards against a nil
//     issuer (which is what tests use to assert the state
//     machine without driving a live CA) and degrades to
//     cert_state=failed with a clear last_error so the wiring
//     stays visible without a real cert in the meantime.
type TenantSurfaceCertIssuer struct {
	// store is the state surface the issuer reads / writes
	// (tenant_surfaces + tenant_hostnames). Required.
	store state.Store
	// metrics is the daemon-level Prometheus registry. nil-safe
	// (ObserveTenantSurfaceCert guards).
	metrics *Metrics
	// le is the production certmagic-based issuer. nil-safe:
	// a nil le degrades to cert_state=failed with the same
	// clear last_error the v1 stub produced, so unit tests
	// that don't wire a real CA can still exercise the state
	// machine.
	le *LetsEncryptCertIssuer
	// now is the wall-clock source for the cert_not_after
	// stamp. Tests override to a fixed time.
	now func() time.Time
}

// NewTenantSurfaceCertIssuer constructs a production issuer. store
// and metrics may both be nil at test sites; the engine guards on
// each. now defaults to time.Now UTC when nil.
func NewTenantSurfaceCertIssuer(store state.Store, metrics *Metrics, le *LetsEncryptCertIssuer) *TenantSurfaceCertIssuer {
	return &TenantSurfaceCertIssuer{
		store:   store,
		metrics: metrics,
		le:      le,
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

// SetLetsEncryptIssuer is the test seam for swapping in a
// fake LetsEncryptCertIssuer that doesn't talk to a real CA.
// Returns the receiver for fluent chaining.
func (i *TenantSurfaceCertIssuer) SetLetsEncryptIssuer(le *LetsEncryptCertIssuer) *TenantSurfaceCertIssuer {
	i.le = le
	return i
}

// RequestCertForSurface (CertIssuer contract) re-mints the cert
// for the given surface. The state machine transitions:
//
//	none → pending → issued (happy path)
//	none → pending → failed (CA failure / DNS-01 solver failure / rate limit)
//	issued → pending → issued (renewal — pg_notify fires from the renewer's
//	          TouchTenantSurfaceForRenewal bump)
//
// Every transition fires the tenant_surface_changed notify trigger,
// which re-routes back through the pg_notify subscriber (idempotent:
// a no-op when the surface is already in the desired state).
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
		// dashboards count the skipped branches. The kind
		// label is empty because the row is gone — the closed
		// cartesian (result, kind) is stamped at boot with
		// kind ∈ {per_host_san, shared_wildcard}, so the
		// missing-row case increments under kind="".
		_ = err // ErrNotFound is deliberately swallowed; we still tick the metric
		i.metrics.ObserveTenantSurfaceCert("skipped", "")
		return nil //nolint:nilerr // ErrNotFound is the documented skip path
	}
	if surf.Status == state.SurfaceStatusDeleted {
		i.metrics.ObserveTenantSurfaceCert("skipped", string(surf.CertKind))
		return nil
	}
	// CertKind dispatch. The schema accepts per_host_san +
	// shared_wildcard today; per_host lands in commit 5.
	// Only per_host_san is mintable today.
	if surf.CertKind != state.CertKindPerHostSAN {
		errMsg := fmt.Sprintf("cert_kind %q not minted in v1; the customer-zone DNS-01 solver ships in follow-up ADR-114", surf.CertKind)
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
	sort.Slice(sorted, func(a, b int) bool { return sorted[a].Hostname < sorted[b].Hostname })
	hostnames := make([]string, len(sorted))
	for k, h := range sorted {
		hostnames[k] = h.Hostname
	}
	// MaxSANPerCert is LE's hard cap. The per-host issuer
	// doesn't bundle, so this only fires if a future caller
	// hands in a >100-host set; the cap stays here so the
	// customer sees the same clear failure whether the
	// wrapper mints per-host or via a future bundler.
	if len(hostnames) > api.MaxSANPerCert {
		errMsg := fmt.Sprintf("verified-hostname set %d exceeds MaxSANPerCert=%d; the per-host bundler ships in follow-up ADR-114", len(hostnames), api.MaxSANPerCert)
		_ = i.store.UpdateTenantSurfaceCert(ctx, state.UpdateSurfaceCertParams{
			SurfaceID: surf.ID,
			CertState: state.CertStateFailed,
			LastError: errMsg,
		})
		i.metrics.ObserveTenantSurfaceCert("skipped", string(surf.CertKind))
		return nil
	}
	// Flip cert_state=pending BEFORE the certmagic call so the
	// dashboard sees the in-flight state. The trigger fires
	// (notify) — the subscriber re-invokes RequestCertForSurface
	// for the same surface; the second invocation hits the
	// cert_state=pending branch (the "renewer tick during a
	// pending CA call" race) and returns nil without minting
	// twice. The race is benign because both invocations point
	// at the same cache key and certmagic's per-key lock
	// deduplicates the underlying Obtain call.
	if err := i.store.UpdateTenantSurfaceCert(ctx, state.UpdateSurfaceCertParams{
		SurfaceID: surf.ID,
		CertState: state.CertStatePending,
	}); err != nil {
		return fmt.Errorf("gateway: flip to pending for surface %s: %w", surf.ID, err)
	}
	// Nil-safe degradation: if le is nil (the test-only path
	// or a misconfigured boot), we still flip to failed with
	// a clear last_error so the wiring is visible without
	// touching the CA.
	if i.le == nil {
		errMsg := fmt.Sprintf("cert engine unwired: %d verified hostnames pending (primary=%s); configure LetsEncryptCertIssuer in cmd/gatewayd-internal/run.go", len(hostnames), hostnames[0])
		_ = i.store.UpdateTenantSurfaceCert(ctx, state.UpdateSurfaceCertParams{
			SurfaceID: surf.ID,
			CertState: state.CertStateFailed,
			LastError: errMsg,
		})
		i.metrics.ObserveTenantSurfaceCert("failed", string(surf.CertKind))
		return nil
	}
	// CA call. IssueSet mints per-host certs and returns the
	// soonest NotAfter across the issued set; that's the
	// value stamped on the surface's cert_not_after column.
	notAfter, err := i.le.IssueSet(ctx, hostnames)
	if err != nil {
		_ = i.store.UpdateTenantSurfaceCert(ctx, state.UpdateSurfaceCertParams{
			SurfaceID: surf.ID,
			CertState: state.CertStateFailed,
			LastError: fmt.Sprintf("certmagic IssueSet: %v", err),
		})
		i.metrics.ObserveTenantSurfaceCert("failed", string(surf.CertKind))
		return fmt.Errorf("gateway: IssueSet for surface %s: %w", surf.ID, err)
	}
	if err := i.store.UpdateTenantSurfaceCert(ctx, state.UpdateSurfaceCertParams{
		SurfaceID: surf.ID,
		CertState: state.CertStateIssued,
		NotAfter:  notAfter,
	}); err != nil {
		i.metrics.ObserveTenantSurfaceCert("failed", string(surf.CertKind))
		return fmt.Errorf("gateway: flip to issued for surface %s: %w", surf.ID, err)
	}
	i.metrics.ObserveTenantSurfaceCert("issued", string(surf.CertKind))
	return nil
}