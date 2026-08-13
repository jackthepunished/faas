// Issue #676 / ADR-080 / PR-C follow-up 5: table-driven parser
// coverage for the raw-bytes bridge's load-bearing Upgrade
// detector. The detector (pkg/gateway/upgrade.go:isUpgradeRequest)
// is the single source of truth for "is this an upgrade request?"
// across the gatewayd-internal handler (Handler.ServeHTTP three-input
// gate) and the gatewayd-public → gatewayd-internal hop
// (internal_proxy.go). It's also the first guard that runs against
// every inbound HTTP request — a misparse there either routes plain
// HTTP through the raw bridge (silently wrong) or routes an upgrade
// through the plain-HTTP forwarder (which strips the load-bearing
// Connection + Upgrade headers). Both are SRE-visible failures;
// table-driven coverage is cheap insurance.
//
// RFC 7230 §3.2 case-insensitivity is the load-bearing rule: an
// incoming `connection: UPGRADE` + `upgrade: websocket` is just as
// much an upgrade request as `Connection: Upgrade` + `Upgrade:
// websocket`. The detector uses strings.EqualFold for both the
// Connection-token match AND the implicit Upgrade-header value;
// every case below exercises a different shape.

package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestIsUpgradeRequest_AcceptsValidShapes covers the load-bearing
// positive cases the detector MUST accept. RFC 7230 §6.1 hop-by-hop
// semantics make these the ONLY request shapes that should land on
// the raw-bytes bridge; a regression here means either plain HTTP
// traffic is being misrouted to the bridge (worse — silent
// corruption) or upgrade traffic is being misrouted to the plain
// forwarder (Connection + Upgrade stripped → handshake fails).
func TestIsUpgradeRequest_AcceptsValidShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// h is applied via req.Header.Set / Add — the test
		// composes the request then runs isUpgradeRequest.
		headers map[string][]string
	}{
		{
			name: "single_token_Connection_Upgrade",
			headers: map[string][]string{
				"Connection": {"Upgrade"},
				"Upgrade":    {"websocket"},
			},
		},
		{
			name: "comma_list_Connection_keep_alive_upgrade",
			headers: map[string][]string{
				"Connection": {"keep-alive, Upgrade"},
				"Upgrade":    {"websocket"},
			},
		},
		{
			name: "comma_list_with_padding",
			headers: map[string][]string{
				// RFC 7230 §3.2: OWS around tokens is permitted.
				// Strings.TrimSpace in the detector trims per
				// token. Verify the OWS path is covered.
				"Connection": {"  keep-alive  ,  Upgrade  "},
				"Upgrade":    {"websocket"},
			},
		},
		{
			name: "multiple_Connection_headers",
			headers: map[string][]string{
				"Connection": {"keep-alive", "Upgrade"},
				"Upgrade":    {"websocket"},
			},
		},
		{
			name: "case_insensitive_Connection_uppercase",
			headers: map[string][]string{
				"Connection": {"UPGRADE"},
				"Upgrade":    {"websocket"},
			},
		},
		{
			name: "case_insensitive_Connection_mixedcase",
			headers: map[string][]string{
				"Connection": {"kEeP-AlIvE, UpGrAdE"},
				"Upgrade":    {"websocket"},
			},
		},
		{
			name: "case_insensitive_Upgrade_header_value",
			headers: map[string][]string{
				"Connection": {"Upgrade"},
				"Upgrade":    {"WEBSOCKET"},
			},
		},
		{
			name: "non_websocket_Upgrade_value_h2c",
			headers: map[string][]string{
				// The detector is protocol-agnostic — h2c flows
				// through the bridge verbatim. The guest is the
				// protocol authority; the gateway is a byte
				// pipe.
				"Connection": {"Upgrade"},
				"Upgrade":    {"h2c"},
			},
		},
		{
			name: "non_websocket_Upgrade_value_mqtt",
			headers: map[string][]string{
				"Connection": {"Upgrade"},
				"Upgrade":    {"mqtt"},
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			for k, vs := range tc.headers {
				for _, v := range vs {
					req.Header.Add(k, v)
				}
			}
			if !isUpgradeRequest(req) {
				t.Fatalf("isUpgradeRequest(%s) = false; want true", tc.name)
			}
		})
	}
}

// TestIsUpgradeRequest_RejectsInvalidShapes covers the
// negative paths. Each one is a regression trap: a regression
// that turns any of these `false` answers into `true` would route
// either plain HTTP through the raw bridge (silent corruption
// of every non-upgrade request) or a malformed upgrade through
// the raw bridge (502 + "raw bridge response cap" instead of a
// proper upgrade fail).
func TestIsUpgradeRequest_RejectsInvalidShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// mutate takes the constructed request and applies the
		// test-specific header shape. Most cases set headers
		// directly; the nil-receiver case constructs a nil
		// request — see the dedicated sub-test.
		headers map[string][]string
	}{
		{
			name: "no_Connection_no_Upgrade",
			headers: map[string][]string{
				"Accept": {"text/html"},
			},
		},
		{
			name: "Connection_without_upgrade_token",
			headers: map[string][]string{
				"Connection": {"keep-alive"},
				"Upgrade":    {"websocket"},
			},
		},
		{
			name: "Connection_with_similar_token_close",
			headers: map[string][]string{
				// The detector matches ONLY "Upgrade" — not
				// "close", "keep-alive", or anything else. RFC
				// 7230 §6.1 hop-by-hop tokens; the raw bridge
				// only fires on Upgrade. A regression that
				// matches "close" would route
				// Connection: close through the raw bridge,
				// which is wrong.
				"Connection": {"close"},
				"Upgrade":    {"websocket"},
			},
		},
		{
			name: "Connection_mentions_Upgrade_but_Upgrade_header_empty",
			headers: map[string][]string{
				// RFC 7230 says the Upgrade header must
				// accompany the token; an empty Upgrade
				// header is malformed and the detector
				// routes it to the plain-HTTP forwarder (so
				// the customer gets a normal 200/404/502
				// instead of a confusing 101 attempt).
				"Connection": {"Upgrade"},
				"Upgrade":    {""},
			},
		},
		{
			name: "Connection_Upgrade_token_but_no_Upgrade_header_at_all",
			headers: map[string][]string{
				"Connection": {"Upgrade"},
				// No Upgrade header at all — net/http's
				// Get returns "" → detector returns false.
			},
		},
		{
			name: "Upgrade_header_present_but_no_Connection_upgrade_token",
			headers: map[string][]string{
				// An Upgrade header without a Connection
				// upgrade token is not a hop-by-hop upgrade;
				// the detector rejects it. (In practice this
				// is what a misbehaving client sends; we
				// route it through the plain-HTTP forwarder
				// and the guest sees the literal Upgrade
				// header if it cares.)
				"Upgrade": {"websocket"},
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			for k, vs := range tc.headers {
				for _, v := range vs {
					req.Header.Add(k, v)
				}
			}
			if isUpgradeRequest(req) {
				t.Fatalf("isUpgradeRequest(%s) = true; want false", tc.name)
			}
		})
	}
}

// TestIsUpgradeRequest_NilRequest is the defensive nil-receiver
// guard. Production callers (handler.go:2899, internal_proxy.go)
// guard with `r != nil` before calling; this test ensures the
// detector itself does NOT panic on nil — it's a defence-in-depth
// against future call-site drift where a constructor returns nil
// request and the detector is called without the upstream guard.
func TestIsUpgradeRequest_NilRequest(t *testing.T) {
	t.Parallel()
	if isUpgradeRequest(nil) {
		t.Fatal("isUpgradeRequest(nil) = true; want false")
	}
}