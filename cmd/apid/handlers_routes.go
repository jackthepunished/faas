package main

// Per-app per-route label reader (issue #273 / ADR-093).
//
// GET /v1/apps/{slug}/routes
//
// Read-only, scoped to api.ScopesReadSurface (admin or apps:read).
// No MFA required — the primary caller is an API key. IDOR-safe
// via the existing loadApp (cross-account slug → 404, not 200
// with another tenant's route labels — leaking per-route labels
// would let a customer enumerate another tenant's API surface).
//
// The handler reverse-proxies the request to gatewayd-internal's
// control listener at /v1/internal/apps/{slug}/routes via a
// short HTTP dial. The control listener is loopback-only (default
// 127.0.0.1:9090) — the response is the bounded in-memory label
// snapshot Handler.RoutesFor returns (capped at 50 + the
// __route_other__ overflow bucket).
//
// Wire format
//
//	200  {"slug":"...","app_id":"..." (omitted if empty),
//	      "routes":["GET /users","..."],
//	      "source":"live"}                    on gatewayd dial ok
//	200  {"slug":"...","routes":[],
//	      "source":"unavailable"}             on dial error
//	                                            (sets
//	                                            X-Faas-Routes-State:
//	                                            unavailable)
//	404  problem+json                         on cross-account slug
//
// Why this lives on apid, not the gatewayd public listener:
//   - The auth chain (ScopesReadSurface) lives in apid where it
//     belongs; the per-account rate limit applies naturally.
//   - The control listener is loopback-only by design (ADR-070
//     single-purpose control mux); exposing it publicly would
//     leak per-app route labels to any customer who could hit
//     the dial.
//   - The apid→gatewayd hop is in-box (loopback), so the only
//     callers that reach the control listener are trusted
//     apid-side code paths. No mTLS required between daemons on
//     the same box.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// routesDialTimeout bounds the apid→gatewayd-internal control
// hop. The control listener is in-box and unbounded on a hot
// day could pile up; 2s is generous for an in-memory map read on
// the gatewayd side and matches the per-request envelope used by
// the existing apid→apid internal calls.
const routesDialTimeout = 2 * time.Second

// getAppRoutes serves GET /v1/apps/{slug}/routes. The auth chain
// matches /v1/apps/{slug}/metrics (read-only, no MFA, primary
// caller is an API key with ScopesReadSurface). IDOR-safe via
// loadApp — cross-account slug is a 404, not a 200 with another
// tenant's route labels.
func (s *server) getAppRoutes(w http.ResponseWriter, r *http.Request, acct state.Account) { //nolint:contextcheck // loadApp takes r and uses r.Context() for its DB calls.
	_, ok := s.loadApp(w, r, acct, r.PathValue("slug"))
	if !ok {
		// loadApp already wrote the 404.
		return
	}
	if s.gatewaydControlURL == "" {
		// Pre-ADR-093 / dev-mode posture: the operator never
		// wired a gatewayd control URL, so the reverse-proxy
		// surface is disabled. Surface a degraded-state
		// response rather than 503 so the dashboard can render
		// the empty state without a redirect to a status page.
		w.Header().Set("X-Faas-Routes-State", "unavailable")
		writeJSON(w, http.StatusOK, api.AppRoutesResponse{
			Slug:   r.PathValue("slug"),
			Routes: []string{},
			Source: "unavailable",
		})
		return
	}
	slug := r.PathValue("slug")
	dialCtx, cancel := context.WithTimeout(r.Context(), routesDialTimeout)
	defer cancel()
	url := s.gatewaydControlURL + "/v1/internal/apps/" + slug + "/routes"
	req, err := http.NewRequestWithContext(dialCtx, http.MethodGet, url, nil)
	if err != nil {
		w.Header().Set("X-Faas-Routes-State", "unavailable")
		writeJSON(w, http.StatusOK, api.AppRoutesResponse{
			Slug:   slug,
			Routes: []string{},
			Source: "unavailable",
		})
		return
	}
	client := &http.Client{Timeout: routesDialTimeout}
	resp, err := client.Do(req)
	if err != nil {
		// Dial failure — gatewayd not running on this box
		// (single-box dev), the loopback bind is wrong, or the
		// in-box network is partitioned. Surface as
		// unavailable rather than 502 so the dashboard doesn't
		// escalate to "platform outage".
		s.log.Debug("apid→gatewayd control dial failed", "err", err.Error(), "url", url)
		w.Header().Set("X-Faas-Routes-State", "unavailable")
		writeJSON(w, http.StatusOK, api.AppRoutesResponse{
			Slug:   slug,
			Routes: []string{},
			Source: "unavailable",
		})
		return
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		w.Header().Set("X-Faas-Routes-State", "unavailable")
		writeJSON(w, http.StatusOK, api.AppRoutesResponse{
			Slug:   slug,
			Routes: []string{},
			Source: "unavailable",
		})
		return
	}
	if resp.StatusCode != http.StatusOK {
		w.Header().Set("X-Faas-Routes-State", "unavailable")
		writeJSON(w, http.StatusOK, api.AppRoutesResponse{
			Slug:   slug,
			Routes: []string{},
			Source: fmt.Sprintf("unavailable: gatewayd status %d", resp.StatusCode),
		})
		return
	}
	// Decode the gatewayd-side response and reserialise via the
	// apid-side DTO. We could pass-through the raw bytes, but
	// the field rename (gatewayd emits "app_id", apid emits
	// "app_id,omitempty") is small enough that doing the decode
	// keeps the apid contract honest and gives the SDK
	// generator one shape to chew on.
	var upstream routesUpstreamResponse
	if err := json.Unmarshal(body, &upstream); err != nil {
		w.Header().Set("X-Faas-Routes-State", "unavailable")
		writeJSON(w, http.StatusOK, api.AppRoutesResponse{
			Slug:   slug,
			Routes: []string{},
			Source: "unavailable: bad gatewayd response",
		})
		return
	}
	w.Header().Set("X-Faas-Routes-State", "ok")
	writeJSON(w, http.StatusOK, api.AppRoutesResponse{
		Slug:   slug,
		AppID:  upstream.AppID,
		Routes: upstream.Routes,
		Source: "live",
	})
}

// routesUpstreamResponse mirrors cmd/gatewayd-internal/
// routes_handler.go's routesResponseJSON. Kept as a separate
// type from api.AppRoutesResponse because the field renames
// (omitempty for AppID) and the Source field (only present on
// the apid side) shouldn't bleed across the package boundary.
type routesUpstreamResponse struct {
	Slug   string   `json:"slug"`
	AppID  string   `json:"app_id"`
	Routes []string `json:"routes"`
}
