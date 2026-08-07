// Upgrade detection helper for the raw-bytes bridge path
// (issue #676 / ADR-080). The detector lives in pkg/gateway so
// both the gatewayd-internal handler (Handler.ServeHTTP) and the
// gatewayd-public → gatewayd-internal hop (internal_proxy.go) can
// share the same case-insensitive RFC 7230 §3.2 check without
// duplicating the parse.
//
// Why the raw-bytes bridge needs Upgrade detection:
//
//   - The plain-HTTP forwarder (pkg/gateway/forwardproxy.go) strips
//     `Connection` + `Upgrade` from inbound headers (RFC 7230 §6.1
//     hop-by-hop). That strip is correct for plain HTTP — it would
//     destroy the WebSocket / h2c / MQTT-over-WS handshake otherwise.
//   - gatewayd-internal must therefore ROUTE upgrade requests to a
//     different forwarder BEFORE the strip fires. The detector here
//     is the single source of truth for "is this an upgrade request?"
//     and is called from both the handler and the internal hop.
//
// What the detector accepts:
//
//   - Connection: Upgrade (case-insensitive per RFC 7230 §3.2). The
//     `Connection` header value is a comma-separated list of tokens;
//     a token equal to `Upgrade` (case-insensitive) is the load-bearing
//     match.
//   - A non-empty Upgrade header. The detector is protocol-agnostic —
//     any value (`websocket`, `h2c`, `mqtt`, ...) flows through the
//     raw-bytes bridge verbatim because the gRPC ForwardRawStream
//     carries bytes end-to-end.
//
// What it rejects:
//
//   - Requests with no Upgrade token (default HTTP/1.1 traffic). The
//     plain-HTTP forwarder keeps handling these.
//   - Requests with an Upgrade token but an empty Upgrade header
//     (malformed; treated as plain HTTP so the customer sees a normal
//     200 / 404 / 502 instead of a confusing 101).
package gateway

import (
	"net/http"
	"strings"
)

// isUpgradeRequest reports whether the inbound request carries the
// RFC 7230 §6.1 hop-by-hop Upgrade token. The detector accepts ANY
// value of the Upgrade header (websocket, h2c, mqtt, ...) — the raw
// RPC carries bytes verbatim so a non-WS token simply flows to the
// guest unchanged. Case-insensitive per RFC 7230 §3.2.
//
// Both `Connection: Upgrade` (single token) and
// `Connection: keep-alive, Upgrade` (token list) match; the parser
// splits the value on commas and trims per-spec whitespace.
//
// The `Upgrade:` header MUST be non-empty; a Connection header that
// mentions Upgrade without an actual Upgrade header is a malformed
// request and is treated as plain HTTP so the customer sees a normal
// response. The bridge's load-bearing protocol detection (does the
// guest speak websocket? h2c? mqtt?) is delegated to the guest — the
// gateway is just a transparent byte pipe.
func isUpgradeRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	// Iterate every Connection header value (multiple Connection
	// headers are allowed per RFC 7230 §3.2 — comma-separated or
	// repeated; net/http's Values() flattens into a slice).
	for _, conn := range r.Header.Values("Connection") {
		for _, tok := range strings.Split(conn, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "Upgrade") {
				return r.Header.Get("Upgrade") != ""
			}
		}
	}
	return false
}
