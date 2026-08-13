// /v1/internal/apps/{slug}/routes — ADR-093 control-listener
// reader for the per-route observability surface.
//
// Mounted on the loopback-only control listener
// (default 127.0.0.1:9090, see pkg/gateway/control.go) so apid
// can reverse-proxy /v1/apps/{slug}/routes →
// /v1/internal/apps/{slug}/routes without going through the
// public :443 listener (which would self-rate-limit and expose
// internal route labels to a customer-scoped probe).
//
// Query:   GET /v1/internal/apps/{slug}/routes
//
//	slug is required (path segment). Resolved through the
//	Handler's existing route-set map; the control listener
//	doesn't have a Postgres connection (ADR-070 single-purpose
//	control mux) so it relies on the public-listener Handler's
//	in-process state. The first request after boot therefore
//	sees an empty Routes array until the first customer request
//	to that app exercises Handler.routeSetFor — same caveat as
//	the existing /v1/internal/quota (no PG round-trip, no
//	wait-for-warmup).
//
// Auth:    loopback bind only; no token. The control listener is
//
//	unauthenticated by design (operator-Prometheus scrape is
//	the main consumer — see pkg/gateway/control.go). The
//	loopback bind is the auth. Mounting this on the public
//	listener would let a customer enumerate per-app route
//	labels.
//
// Response (JSON):
//
//	200  {"slug":"...","app_id":"...","enabled":bool,
//	      "routes":[...]}        on a known app
//	200  {"slug":"...","app_id":"","enabled":false,
//	      "routes":[]}            when the app is not opted in
//	                              (per-app flag off OR operator
//	                              kill-switch off — the handler
//	                              can't tell the two apart from
//	                              the in-process state alone, so
//	                              it always reports enabled=false
//	                              on the empty path)
//	400  problem+json            on missing slug
//
// Why this lives on the control listener, not the public one:
//   - The public listener self-rate-limits; a dashboard that polled
//     the public listener would 429 its own bucket (same reasoning
//     as /v1/internal/quota, see quota_handler.go:37-44).
//   - The control listener is loopback-only (spec §11 single-
//     public-listener invariant), so the only in-box caller is
//     apid via the existing apidProxy.
//   - Per-app route labels are arguably a customer-facing signal,
//     but we still go through the control listener + apidProxy so
//     the auth chain (ScopesReadSurface) and rate-limit envelope
//     (per-account) live in apid where they belong. The control
//     side is the trusted, in-box hop; apid is the customer
//     surface.
package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/gateway"
	"github.com/onebox-faas/faas/pkg/logsanitize"
)

// internalRoutesHandler returns an http.HandlerFunc that serves
// the per-app route label snapshot from h.RoutesFor(appID).
// appLookup is the (slug → appID, ok) resolver the daemon wired at
// construction; nil disables the lookup and every request 400s
// (the unit-test path — production always wires a real
// gateway.ResolveSlugFn).
//
// h is the public-listener Handler; the in-process state on h is
// the same state the public-listener requests observe, so the
// snapshot agrees with what the per-route histogram has been
// emitting since boot. logger is the access-log sink; pass nil
// to disable access logging (tests).
func internalRoutesHandler(h *gateway.Handler, appLookup gateway.ResolveSlugFn, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if logger != nil {
			// r.URL.RawPath / Path are attacker-controllable —
			// sanitize at the source so a CR/LF in the slug
			// can't break the log-injection invariant (one log
			// line per event). Precedent: quota_handler.go:69-72.
			logger.Debug("internal routes poll", "remote", r.RemoteAddr, "path", logsanitize.Field(r.URL.Path))
		}
		if r.Method != http.MethodGet {
			writeProblemRoutes(w, http.StatusMethodNotAllowed, "method_not_allowed",
				"only GET is supported on this endpoint")
			return
		}
		// Slug is the last path segment; we accept the literal
		// /v1/internal/apps/<slug>/routes form. http.ServeMux's
		// pattern routing doesn't carry path segments into the
		// mux-keyed handler — we read r.URL.Path and trim. The
		// /routes suffix is required: the mux mounts on the
		// prefix /v1/internal/apps/ (run.go), so a request to
		// /v1/internal/apps/foo would otherwise reach the
		// handler with slug="foo" and serve foo's routes.
		// Validate the suffix BEFORE trimming so the malformed
		// path 404s instead of silently answering the wrong app.
		rest := strings.TrimPrefix(r.URL.Path, "/v1/internal/apps/")
		if !strings.HasSuffix(rest, "/routes") {
			writeProblemRoutes(w, http.StatusNotFound, "not_found",
				"path must match /v1/internal/apps/<slug>/routes")
			return
		}
		slug := strings.TrimSuffix(rest, "/routes")
		slug = strings.Trim(slug, "/")
		if slug == "" {
			writeProblemRoutes(w, http.StatusBadRequest, "missing_slug",
				"path segment slug is required")
			return
		}
		if appLookup == nil {
			// Mis-wired control listener. Production always
			// passes a real gateway.ResolveSlugFn (set in
			// run.go next to internalQuotaHandler); nil is
			// the unit-test seam.
			writeProblemRoutes(w, http.StatusServiceUnavailable, "lookup_unavailable",
				"slug→appID resolver is not wired in this build")
			return
		}
		appID, ok := appLookup(slug)
		if !ok || appID == "" {
			// Don't 404 — the dashboard treats unknown apps
			// as "not yet routed" (same shape as the legacy
			// /v1/internal/quota endpoint, which returns
			// ok=false on missing buckets rather than 404).
			// The empty Routes array is the signal.
			writeRoutesJSON(w, http.StatusOK, routesResponseJSON{
				Slug:   slug,
				AppID:  "",
				Routes: []string{},
			})
			return
		}
		routes, capHit := h.RoutesFor(appID)
		if routes == nil {
			// Either the app is not opted in, or no traffic
			// has reached the per-route path yet (routeSetFor
			// is lazy — created on first sight of an opted-in
			// app). Both shapes are reported identically so
			// the dashboard doesn't leak the "is the operator
			// kill-switch off?" property (which would let a
			// customer enumerate operator state).
			routes = []string{}
		}
		writeRoutesJSON(w, http.StatusOK, routesResponseJSON{
			Slug:   slug,
			AppID:  appID,
			Routes: routes,
			CapHit: capHit,
		})
	}
}

// routesResponseJSON is the wire shape returned by
// /v1/internal/apps/{slug}/routes. Field order is fixed so the
// JSON output is stable (Go's encoding/json sorts by declaration
// order). The shape mirrors AppMetricsResponse.Routes (pkg/api/
// dto.go) so the apid-side wrapper can deserialise without a
// shape-bridging translation step.
//
// Routes is an empty slice (not nil) on the "not opted in" /
// "no traffic yet" paths so the JSON encoder emits `[]` rather
// than `null` — the dashboard JS would crash on the null shape.
//
// CapHit (ADR-093 Tier B item #1) is true iff the app's
// routeLabelSet has reached RouteMetricsPerAppCap (50) and
// additional routes are collapsing into the reserved
// __route_other__ bucket. The apid-side AppRoutesResponse mirrors
// this field so the dashboard renders "you have hit the 50-route
// cap" without counting Routes (which is ambiguous: 5 real
// routes + __route_other__ for a one-off wildcard probe is
// indistinguishable from 50 real routes + overflow).
type routesResponseJSON struct {
	Slug   string   `json:"slug"`
	AppID  string   `json:"app_id"`
	Routes []string `json:"routes"`
	CapHit bool     `json:"cap_hit"`
}

func writeRoutesJSON(w http.ResponseWriter, status int, body routesResponseJSON) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// json.Marshal escapes user-supplied values per RFC 8259
	// §7 — patch for CodeQL alert #146 (go/reflected-xss). The
	// slug is operator-supplied today (apid is the only caller
	// and reverse-proxies from a trusted in-box), but the
	// marshal is the safe-by-default for any future dashboard
	// dial that renders the response as HTML.
	b, _ := json.Marshal(body)
	_, _ = w.Write(b)
}

func writeProblemRoutes(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	api.WriteProblem(w, api.NewProblem(status, code, "Routes read failed", detail))
}
