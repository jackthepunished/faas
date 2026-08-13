package main

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/db"
)

// dnsPoller polls DNS for unverified custom-domain TXT challenges and marks
// them verified in the Store. Spec §7: customer publishes a TXT at
// _faas-verify.<domain>; apid polls and flips verified_at when it matches.
//
// This is a poll-only loop — it does NOT subscribe to pg_notify. A LISTEN
// path would replace the ticker once a domain_verify producer lands. Channel
// names use pkg/db constants to stay aligned with the apid NotifyChannels
// table.
const verifyInterval = 30 * time.Second

// startDNSPoller runs the DNS poll loop until ctx is cancelled. Caller is
// responsible for surfacing errors via the slog logger.
func startDNSPoller(ctx context.Context, s *server, log *slog.Logger) {
	if s.store == nil {
		return
	}
	go func() {
		t := time.NewTicker(verifyInterval)
		defer t.Stop()
		// Run once immediately so freshly-added domains don't wait a minute.
		s.runVerifyOnce(ctx, log)
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.runVerifyOnce(ctx, log)
			}
		}
	}()
}

func (s *server) runVerifyOnce(ctx context.Context, log *slog.Logger) {
	pending, err := s.pendingUnverifiedDomains(ctx)
	if err != nil {
		log.Warn("dns_poller: list failed", "err", err)
		return
	}
	for _, d := range pending {
		if checkTXT(ctx, d.Domain, d.ChallengeToken) {
			if err := s.store.MarkDomainVerified(ctx, d.Domain); err != nil {
				log.Warn("dns_poller: mark verified failed", "domain", d.Domain, "err", err)
				continue
			}
			// Use the canonical channel constant (no LISTEN consumer yet —
			// recorded here so the next dns_poller→imaged LISTEN path picks up
			// the right name without a find/replace).
			_ = s.notif.Notify(ctx, db.NotifyDomainVerify, `{"domain":"`+d.Domain+`"}`)
			log.Info("domain verified", "domain", d.Domain)
		}
	}
	// ADR-100 / issue #879: poll tenant hostnames alongside
	// custom domains. Both use the same _faas-verify.<hostname>
	// TXT record format, so checkTXT is shared. The poller is
	// gated on api.TenantSurfacesEnabled() so a feature-flag
	// disable suppresses the LISTEN load on the poller goroutine
	// even when the table is empty.
	if !api.TenantSurfacesEnabled() {
		return
	}
	pendingHostnames, err := s.pendingUnverifiedHostnames(ctx)
	if err != nil {
		log.Warn("dns_poller: list tenant hostnames failed", "err", err)
		return
	}
	for _, h := range pendingHostnames {
		if checkTXT(ctx, h.Hostname, h.ChallengeToken) {
			if err := s.store.MarkTenantHostnameVerified(ctx, h.Hostname); err != nil {
				log.Warn("dns_poller: mark tenant hostname verified failed", "hostname", h.Hostname, "err", err)
				continue
			}
			// tenant_surface_changed fires on the tenant_hostnames
			// UPDATE (the trigger at migrations/00243 fires on
			// every relevant column change including verified_at).
			// The gatewayd cert-remint subscriber picks it up and
			// asks the issuer for a fresh SAN-aggregated cert.
			log.Info("tenant hostname verified", "hostname", h.Hostname, "surface", h.SurfaceID)
		}
	}
}

// pendingUnverifiedHostnames (ADR-100 / issue #879) returns the
// batch of unverified tenant hostnames due for a TXT poll. The
// batcher is ListPendingTenantHostnames — bounded by the poller
// limit (50 per pass) so a single batch doesn't dominate the
// goroutine.
func (s *server) pendingUnverifiedHostnames(ctx context.Context) ([]pendingHostnameRow, error) {
	rows, err := s.store.ListPendingTenantHostnames(ctx, time.Now(), 50)
	if err != nil {
		return nil, err
	}
	out := make([]pendingHostnameRow, len(rows))
	for i, h := range rows {
		out[i] = pendingHostnameRow{
			Hostname:       h.Hostname,
			ChallengeToken: h.ChallengeToken,
			SurfaceID:      h.SurfaceID,
		}
	}
	return out, nil
}

// pendingHostnameRow is the poller's view of an unverified
// tenant hostname. SurfaceID is logged for operator triage so a
// failed poll maps back to the customer surface without a
// second store hop.
type pendingHostnameRow struct {
	Hostname       string
	ChallengeToken string
	SurfaceID      string
}

// pendingUnverifiedDomains reads the unverified index directly. Implemented
// as a tiny helper here (rather than a Store method) because the poller
// goroutine is the only consumer.
func (s *server) pendingUnverifiedDomains(ctx context.Context) ([]pendingDomainRow, error) {
	// We can't reach a *sql.DB from server without exposing one on the
	// struct. The simpler path is to walk all apps and ListDomainsForApp,
	// which works fine at M5 scale (one-box, single-digit accounts). The
	// Store interface grows a dedicated method when this matters.
	var out []pendingDomainRow
	// Fast path: if the Store exposes ListAllUnverifiedDomains (PgStore),
	// use it; otherwise fall back to the per-account walk.
	type listUnverified interface {
		ListAllUnverifiedDomains(ctx context.Context) ([]pendingDomainRow, error)
	}
	if lu, ok := s.store.(listUnverified); ok {
		return lu.ListAllUnverifiedDomains(ctx)
	}
	// Fallback: not implemented for MemStore in tests; return empty.
	return out, nil
}

// pendingDomainRow is the poller's view of an unverified custom domain.
type pendingDomainRow struct {
	Domain         string
	ChallengeToken string
}

// checkTXT does a TXT lookup for _faas-verify.<domain> and reports whether
// any returned record equals the expected token.
func checkTXT(ctx context.Context, domain, expected string) bool {
	target := "_faas-verify." + domain
	records, err := txtLookupFunc(ctx, target)
	if err != nil {
		return false
	}
	for _, r := range records {
		if strings.TrimSpace(r) == expected {
			return true
		}
	}
	return false
}

// txtLookupFunc is the test seam for the TXT verifier. Production
// uses the real net.Resolver; tests inject a fake that returns
// canned records. ADR-100 / issue #879: the same seam covers
// the custom-domain and tenant-hostname verification paths.
var txtLookupFunc = func(ctx context.Context, target string) ([]string, error) {
	return (&net.Resolver{}).LookupTXT(ctx, target)
}
