// pkg/gateway/cert_renewer.go — periodic cert-renewal goroutine
// for ADR-100 tenant surfaces (issue #879, PR-D commit 3).
//
// The renewer watches tenant_surfaces for rows whose
// cert_not_after < now + CertRenewBeforeNotAfterDays. Each tick
// it:
//
//  1. Calls Store.ListTenantSurfacesNearingExpiry(ctx, cutoff)
//     — reads the partial index tenant_surfaces_cert_expiry_idx
//     so the query stays bounded regardless of fleet size.
//  2. For each due surface, calls Store.TouchTenantSurfaceForRenewal
//     — bumping updated_at which fires the tenant_surface_changed
//     notify trigger.
//  3. The pg_notify subscriber routes the bare surface UUID
//     back through CertIssuer.RequestCertForSurface, which
//     re-runs the full state machine (issued → pending →
//     issued).
//
// Why the renewer rides the existing notify pipeline instead
// of calling RequestCertForSurface directly:
//
//   - The wrapper owns the state machine. The renewer is a
//     TRIGGER, not a writer.
//   - A direct call from the renewer would race the pg_notify
//     subscriber (a surface mutation arriving between the
//     renewer's tick and the subscriber's dispatch would mint
//     the surface twice). Touching updated_at serialises
//     through the same notify channel the subscriber is already
//     listening on.
//   - The renewer stays a single dependency (state.Store)
//     instead of reaching into CertIssuer.
//
// Multi-host behaviour: the renewer runs in every
// gatewayd-internal replica. Each replica reads its own copy
// of the same row set, each fires its own UPDATE, and the
// certmagic per-host cache key deduplicates the underlying
// Obtain call (certmagic v0.25 acquireLock on lockKey). The
// redundant UPDATEs are bounded by CertRenewTickSeconds and
// the fleet count; the lock makes the redundant Obtain
// calls no-ops.
//
// Cadence: CertRenewTickSeconds (api.CertRenewTickSeconds =
// 300s). The interval is a deliberate trade-off: a tighter
// tick hits the renewal SLA faster at the cost of a SELECT
// per replica per tick; a looser tick spreads the SELECT
// load but delays a renewal by up to one interval.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// SurfaceCertRenewer is the periodic renewal goroutine. Wire
// it from cmd/gatewayd-internal/run.go when the tenant-surfaces
// feature flag is on; the renewer is a no-op on a nil store
// so a unit test can pass in an empty MemStore without
// panicking.
type SurfaceCertRenewer struct {
	store state.Store
	// log is the per-daemon slog. nil defaults to
	// slog.Default().
	log *slog.Logger
	// tick is the renewal cadence. Tests override via
	// SetTick; production keeps the api.CertRenewTickSeconds
	// default.
	tick time.Duration
	// renewBefore is the renewal window — surfaces with
	// cert_not_after < now + renewBefore are queued. Default
	// is api.CertRenewBeforeNotAfterDays days.
	renewBefore time.Duration
	// clock is the wall-clock source for the cutoff
	// computation. Tests override via SetClock; production
	// uses time.Now.
	clock func() time.Time
	// now is unused but kept for the SetNow fluent seam that
	// mirrors TenantSurfaceCertIssuer.SetNow. Reserved for a
	// future ADR that surfaces the renewer's tick-time on the
	// metrics panel.
	now func() time.Time
}

// NewSurfaceCertRenewer constructs a renewer with the
// default cadence + renewal window. store is required; log
// may be nil.
func NewSurfaceCertRenewer(store state.Store, log *slog.Logger) *SurfaceCertRenewer {
	if log == nil {
		log = slog.Default()
	}
	nowFn := func() time.Time { return time.Now().UTC() }
	return &SurfaceCertRenewer{
		store:       store,
		log:         log,
		tick:        time.Duration(api.CertRenewTickSeconds) * time.Second,
		renewBefore: time.Duration(api.CertRenewBeforeNotAfterDays) * 24 * time.Hour,
		clock:       nowFn,
		now:         nowFn,
	}
}

// SetTick is the test seam for the renewal cadence.
func (r *SurfaceCertRenewer) SetTick(d time.Duration) *SurfaceCertRenewer {
	r.tick = d
	return r
}

// SetRenewBefore is the test seam for the renewal window.
func (r *SurfaceCertRenewer) SetRenewBefore(d time.Duration) *SurfaceCertRenewer {
	r.renewBefore = d
	return r
}

// SetClock is the test seam for the wall-clock source.
func (r *SurfaceCertRenewer) SetClock(c func() time.Time) *SurfaceCertRenewer {
	r.clock = c
	r.now = c
	return r
}

// Run blocks until ctx is cancelled. Mirrors the
// pkg/gateway/cert_expiry.go ticker pattern: one ticker,
// ctx-cancel or done-channel exits the goroutine. Returns
// when ctx is Done; errors are logged inside tickOnce and
// do NOT fail the goroutine (transient DB blips are not
// page-worthy — the next tick recovers).
//
// Run is safe to call once per process. A second call would
// race the ticker reset; production wires it via a
// once-only boot path (cmd/gatewayd-internal/run.go).
func (r *SurfaceCertRenewer) Run(ctx context.Context) {
	if r.store == nil {
		r.log.Error("cert: renewer run with nil store; exiting")
		return
	}
	if r.tick <= 0 {
		r.log.Error("cert: renewer run with non-positive tick; exiting")
		return
	}
	t := time.NewTicker(r.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.tickOnce(ctx); err != nil {
				r.log.Warn("cert: renewer tick", "err", err)
			}
		}
	}
}

// tickOnce is the per-tick body of Run. Split out so a unit
// test can drive a single pass without spinning a ticker.
// Returns a non-nil error only when ListTenantSurfacesNearingExpiry
// fails; per-surface Touch failures are logged + skipped
// inside the loop so one bad row doesn't poison the tick.
func (r *SurfaceCertRenewer) tickOnce(ctx context.Context) error {
	if r.store == nil {
		return errors.New("gateway: renewer nil store")
	}
	cutoff := r.clock().Add(r.renewBefore)
	due, err := r.store.ListTenantSurfacesNearingExpiry(ctx, cutoff)
	if err != nil {
		return fmt.Errorf("gateway: list tenant surfaces nearing expiry: %w", err)
	}
	for _, s := range due {
		if err := r.store.TouchTenantSurfaceForRenewal(ctx, s.ID); err != nil {
			r.log.Warn("cert: renewer touch surface",
				"surface", s.ID,
				"account", s.AccountID,
				"err", err)
			continue
		}
	}
	if len(due) > 0 {
		r.log.Info("cert: renewer tick", "due", len(due), "cutoff", cutoff)
	}
	return nil
}