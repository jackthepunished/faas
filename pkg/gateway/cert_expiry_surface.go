// pkg/gateway/cert_expiry_surface.go — ADR-100 cert health
// surface (issue #879, PR-D commit 2).
//
// Spec §4.1.2.14 says:
//
//	"CertExpiry(ctx, surfaceID) (time.Time, error) in
//	 pkg/gateway/cert_expiry.go"
//
// PR-D splits the function into a per-host lookup so it doesn't
// need to enumerate the on-disk tree (the existing refresher
// in cert_expiry.go walks everything every interval; the
// per-host lookup is the synchronous seam a dashboard or a
// future HealthCheck endpoint uses).
//
// Lookup algorithm:
//   1. Read the surface row.
//   2. Look up the verified hostnames for the surface.
//   3. Pick the lexicographically smallest hostname as the
//      primary (the wrapper at cert_issuer_tenant_surface.go
//      sorts the same way so the on-disk leaf path is
//      deterministic).
//   4. Read <storageDir>/certificates/<issuerKey>/<primary>/<primary>.crt
//      via parseCertNotAfter.
//   5. Return the parsed NotAfter.
//
// Returns a zero time + nil error when the surface has no
// verified hostnames yet (the dashboard renders "no cert yet"
// against a zero-time); returns a non-nil error on read /
// parse / state-lookup failure.
//
// The function is read-only and safe to call concurrently; it
// does NOT touch the state store beyond a SELECT.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/onebox-faas/faas/pkg/state"
)

// CertExpiry reads the on-disk leaf cert for the surface's
// primary hostname and returns the leaf's NotAfter. Returns
// (time.Time{}, nil) when the surface has no verified hostnames
// — callers render "no cert yet" against the zero time.
//
// surfaceID must be non-empty. The function fails closed on:
//   - empty surfaceID
//   - missing surface row (state.ErrNotFound)
//   - state lookup failure
//   - on-disk read failure (cert magic hasn't written the
//     leaf yet, or the file is corrupt)
//
// storageDir is the same path the LetsEncryptCertIssuer
// writes to (cert_issuer_letsencrypt.go:initConfig).
//
// The function does NOT call the daemon-level metrics — the
// dashboard panel reads the gateway_tls_cert_expiry_by_host_seconds
// gauge populated by StartCertExpiryRefresher (cert_expiry.go).
// This function is the synchronous lookup the dashboard
// hits on demand when the operator clicks a surface row.
func CertExpiry(ctx context.Context, store state.Store, storageDir, surfaceID string) (time.Time, error) {
	if surfaceID == "" {
		return time.Time{}, errors.New("gateway: empty surface id")
	}
	if store == nil {
		return time.Time{}, errors.New("gateway: nil store")
	}
	if storageDir == "" {
		return time.Time{}, errors.New("gateway: empty storage dir")
	}
	_, err := store.GetTenantSurfaceByID(ctx, surfaceID)
	if err != nil {
		return time.Time{}, fmt.Errorf("gateway: load surface %s: %w", surfaceID, err)
	}
	verified, err := store.ListVerifiedTenantHostnamesForSurface(ctx, surfaceID)
	if err != nil {
		return time.Time{}, fmt.Errorf("gateway: list verified hostnames for surface %s: %w", surfaceID, err)
	}
	if len(verified) == 0 {
		return time.Time{}, nil
	}
	// Pick the primary — sort-by-hostname matches the
	// wrapper's ordering so the on-disk leaf path is the same
	// one the issuer writes to.
	primary := verified[0].Hostname
	for _, h := range verified[1:] {
		if h.Hostname < primary {
			primary = h.Hostname
		}
	}
	leafPath, err := primaryLeafPath(storageDir, primary)
	if err != nil {
		return time.Time{}, fmt.Errorf("gateway: primary leaf path for surface %s: %w", surfaceID, err)
	}
	return parseCertNotAfter(leafPath)
}

// primaryLeafPath returns the on-disk path certmagic writes
// the leaf cert to. The layout matches
// LetsEncryptCertIssuer.leafPath
// (cert_issuer_letsencrypt.go); the duplication is intentional
// — the issuer's path is internal to its own struct, and
// exporting it would expose the certmagic layout detail on
// the public surface. A future ADR that changes the layout
// must update BOTH functions.
//
// parseCertNotAfter returns a zero time + error when the file
// is missing — the dashboard renders "no cert yet" against
// the zero time AND against a read error; the synchronous
// seam treats both the same.
func primaryLeafPath(storageDir, primary string) (string, error) {
	if primary == "" {
		return "", errors.New("gateway: empty primary hostname")
	}
	if storageDir == "" {
		return "", errors.New("gateway: empty storage dir")
	}
	return storageDir + "/certificates/acme-v02.api.letsencrypt.org-directory/" + primary + "/" + primary + ".crt", nil
}