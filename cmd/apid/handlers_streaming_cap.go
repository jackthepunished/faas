package main

// Per-app streaming-cap probe (ADR-102 D6).
//
// GET /v1/apps/{slug}/streaming-cap
//
// Read-only, scoped to api.ScopesReadSurface (admin or apps:read).
// No MFA required — the primary caller is an API key. IDOR-safe
// via the existing loadApp (cross-account slug → 404, not 200 with
// another tenant's streaming-cap data — leaking cap data lets a
// customer probe another tenant's plan tier).
//
// What this returns
//
// The probe is the apid-side mirror of
// pkg/gateway.(*Handler).decideStreaming. It computes the same
// status enum (api.StreamingStatus*) the gateway would stamp on
// the next inbound request, plus the same effective response body
// cap the gateway would install via capWriter.
//
// What this does NOT return
//
// The probe does NOT dial gatewayd-internal to resolve the
// per-edge-rule cap override (D4) or to learn the operator's
// FAAS_GATEWAY_STREAMING env state. Both live in the gatewayd
// process and are not part of the apid cache. The probe returns
// the plan-level cap as EffectiveCap and labels CapKind="plan"
// when the rule-override path is unknown. A customer who needs
// the live decision fires a real request and reads the
// Streaming-Status response header.
//
// Wire format
//
//	200  {"app_id":"...","status":"streaming",
//	      "effective_cap_bytes":104857600,
//	      "plan_cap_bytes":104857600,
//	      "flag_enabled":true,"plan_allowed":true}
//	404  problem+json                         on cross-account slug
//
// Why this lives on apid, not the gatewayd public listener: the
// auth chain (ScopesReadSurface) lives in apid where it belongs;
// the per-account rate limit applies naturally. The probe is a
// pure read against the per-app cache; no wake, no state mutation.

import (
	"net/http"
	"strings"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// isUpgradeRequest is the apid-side mirror of
// pkg/gateway.isUpgradeRequest (pkg/gateway/upgrade.go:115). Lives
// locally because importing pkg/gateway from cmd/apid would
// create a cycle (apid ↔ gatewayd is gRPC over unix socket, not
// direct linkage). The two definitions MUST stay byte-identical;
// divergence breaks the streaming-cap probe's upgrade-bypass
// classification relative to the gateway.
//
// Mirrors the gateway's full RFC 7230 §6.7 parsing: iterates every
// Connection header value (multiple Connection headers are
// allowed per RFC 7230 §3.2; net/http's Values() flattens into a
// slice), splits each on commas, trims per-spec whitespace, and
// EqualFolds each token against "Upgrade". A Connection header
// that mentions Upgrade without an actual Upgrade header is a
// malformed request and is treated as plain HTTP (returns false).
func isUpgradeRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	for _, conn := range r.Header.Values("Connection") {
		for _, tok := range strings.Split(conn, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "Upgrade") {
				return r.Header.Get("Upgrade") != ""
			}
		}
	}
	return false
}

// isAcceptJSON is the apid-side mirror of
// pkg/gateway.isAcceptJSON (pkg/gateway/handler.go:3241). Lives
// locally for the same cycle-avoidance reason as isUpgradeRequest
// above. Mirrors the gateway's "any token equals application/json"
// rule so the probe matches what the gateway would classify.
func isAcceptJSON(accept string) bool {
	if accept == "" {
		return false
	}
	for _, raw := range strings.Split(accept, ",") {
		// Strip parameters and whitespace per RFC 7231 §5.3.2.
		mediaType := strings.TrimSpace(strings.SplitN(raw, ";", 2)[0])
		if strings.EqualFold(mediaType, "application/json") {
			return true
		}
	}
	return false
}

// streamingCapDialGatewayd is the optional future dial path. When
// the operator wires apid→gatewayd-internal for the streaming-cap
// probe (matching the routes handler at handlers_routes.go), the
// dial result refines EffectiveCap with the per-edge-rule
// override. Not wired in this PR — the apid-side decision tree
// covers all six enum variants without the gatewayd hop, and the
// D6 endpoint contract is satisfied with EffectiveCap=PlanCap.
//
// ADR-102 D6 keeps the probe simple: read what apid already has
// in cache, return the canonical decision. The customer who needs
// sub-second cap accuracy fires a real request and reads the
// Streaming-Status header. The live cap lives in the gateway; the
// probe is a static snapshot.

// getAppStreamingCap serves GET /v1/apps/{slug}/streaming-cap.
// The auth chain matches /v1/apps/{slug}/routes (read-only, no MFA,
// primary caller is an API key with ScopesReadSurface). IDOR-safe
// via loadApp — cross-account slug is a 404.
func (s *server) getAppStreamingCap(w http.ResponseWriter, r *http.Request, acct state.Account) { //nolint:contextcheck // loadApp takes r and uses r.Context() for its DB calls.
	app, ok := s.loadApp(w, r, acct, r.PathValue("slug"))
	if !ok {
		// loadApp already wrote the 404.
		return
	}

	// Compute the static decision (no edge-rule lookup; see the
	// file-level doc). The decision tree mirrors the gateway's
	// decideStreaming for the four non-edge-rule conjuncts.
	//
	//   1. !app.StreamingEnabled → flag-disabled
	//   2. !app.Plan.StreamingResponseAllowed() → plan-disallows
	//   3. isUpgradeRequest(r) → upgrade-bypass
	//   4. isAcceptJSON(r) → accept-json-downgrade (D3 advisory)
	//   5. otherwise → streaming
	//
	// The operator opt-in (FAAS_GATEWAY_STREAMING env) is
	// gatewayd-side state and is not part of the apid cache;
	// the probe cannot reflect it. A customer evaluating "will
	// my next request stream?" must consider the operator-side
	// flag separately. The Streaming-Status response header on
	// a real request IS the canonical signal.
	planCap := acct.Plan.MaxResponseBodyBytes()
	status := decideStaticStreamingStatus(r, app, acct.Plan)
	// accept-json-downgrade is informational post-D3; the
	// request DOES stream in that case. The effective cap is
	// still the plan cap (no endpoint-rule lookup in the probe).
	writeJSON(w, http.StatusOK, api.AppStreamingStatus{
		AppID:        app.ID,
		Status:       status,
		EffectiveCap: planCap,
		PlanCap:      planCap,
		FlagEnabled:  app.StreamingEnabled,
		PlanAllowed:  acct.Plan.StreamingResponseAllowed(),
		CapKind:      "plan",
	})
}

// decideStaticStreamingStatus is the apid-side mirror of the
// gateway's decideStreaming, scoped to the four conjuncts apid can
// evaluate without a gatewayd hop. Returns the streaming-status
// enum value the customer would see stamped on the Streaming-Status
// header for a representative request of the same shape.
//
// The Plan lives on the Account, not the App (a single account
// can have multiple apps at the same tier), so the caller threads
// it explicitly. Edge-rule lookup, operator env, and per-request
// Accept/Upgrade evaluation are gatewayd-side; this helper only
// mirrors the per-app flag + plan-tier portion of the decision.
// The full decision tree is in pkg/gateway.(*Handler).decideStreaming.
func decideStaticStreamingStatus(r *http.Request, app state.App, plan api.Plan) api.StreamingStatus {
	if !app.StreamingEnabled {
		return api.StreamingStatusFlagDisabled
	}
	if !plan.StreamingResponseAllowed() {
		return api.StreamingStatusPlanDisallows
	}
	if isUpgradeRequest(r) {
		return api.StreamingStatusUpgradeBypass
	}
	if isAcceptJSON(r.Header.Get("Accept")) {
		return api.StreamingStatusAcceptJSONDowngrade
	}
	return api.StreamingStatusStreaming
}
