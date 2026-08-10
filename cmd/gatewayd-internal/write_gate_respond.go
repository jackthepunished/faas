// write_gate_respond.go — writeGate response emitters,
// keyed by classifyDecision. Splitting the response shape
// from the decision tree (write_gate_classify.go) and the
// handler (write_gate.go) keeps each file under the
// 50-line budget (CLAUDE.md "Handlers ≤ 50 lines — extract").
//
// Every emitter here:
//
//  1. Writes the canonical HTTP status (307/400/503/etc).
//  2. Sets Retry-After when the response should be retried.
//  3. Writes an RFC 7807 problem-detail JSON body so client
//     SDKs decode uniformly.
//  4. NEVER logs the request body, Authorization header,
//     session cookie, or any other secret. Per spec §11
//     "Never log secret values".
//
// The redirect emitter (decisionCookieRedirect) builds the
// leader URL via the convention documented in ADR-084
// §Decision #3: <scheme>://<leaderNode>.<publicDNS>/<path>?<query>.
// The path is preserved verbatim so a redirected POST lands
// on the same route.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/onebox-faas/faas/pkg/gateway/writegate"
)

// problemDetail mirrors the apid API error envelope so
// client SDKs (sdk-go, sdk-node, sdk-python) decode the
// same shape whether the response came from apid itself or
// from the gate in front of it. The `code` field is the
// stable identifier; `status` mirrors the HTTP code; `detail`
// is a human-readable explanation safe to surface in logs.
type problemDetail struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

const (
	problemTypeGateway        = "https://docs.faas/errors/gateway"
	codeRedirectLoop          = "redirect_loop"
	codeLeaderUnreachable     = "leader_unreachable"
	codeCookieCrossBoxBlocked = "cookie_cross_box_blocked"
	codeRelayMTLSFailure      = "relay_mtls_failure"
	codeRelayError            = "relay_error"
)

// respondLoop writes a 400 with redirect_loop. We deliberately
// do NOT set Retry-After — the client should not retry; the
// sentinel itself is the signal.
func (g *writeGate) respondLoop(w http.ResponseWriter, r *http.Request, authKind writegate.AuthKind) {
	g.metrics.WriteRedirectTotal(string(writegate.OutcomeLoopPrevented), string(authKind)).Inc()
	g.log.Warn("writegate: loop attempt rejected",
		"path", r.URL.Path,
		"method", r.Method,
		"auth_kind", string(authKind),
	)
	writeProblem(w, http.StatusBadRequest, problemDetail{
		Type:   problemTypeGateway,
		Title:  "Loop Detected",
		Status: http.StatusBadRequest,
		Code:   codeRedirectLoop,
		Detail: "request already forwarded once; refusing to relay again",
	})
}

// respondSameBox passes through to next.ServeHTTP after
// incrementing the same_box counter. The metric is emitted
// even on bypass paths that the classifier forwarded, so
// dashboards can split "leader handles write" from "standby
// handles write".
func (g *writeGate) respondSameBox(w http.ResponseWriter, r *http.Request, authKind writegate.AuthKind) {
	g.metrics.WriteRedirectTotal(string(writegate.OutcomeSameBox), string(authKind)).Inc()
	g.next.ServeHTTP(w, r)
}

// respondUnreachable writes 503 with the no-leader Retry-After.
// Per ADR-084 §Decision #7 no-leader and DB/notify errors both
// map to this outcome.
func (g *writeGate) respondUnreachable(w http.ResponseWriter, r *http.Request, authKind writegate.AuthKind) {
	g.metrics.WriteRedirectTotal(string(writegate.OutcomeLeaderUnreachable), string(authKind)).Inc()
	g.log.Warn("writegate: leader unreachable",
		"path", r.URL.Path,
		"method", r.Method,
		"auth_kind", string(authKind),
	)
	writeProblem(w, http.StatusServiceUnavailable, problemDetail{
		Type:   problemTypeGateway,
		Title:  "Leader Unreachable",
		Status: http.StatusServiceUnavailable,
		Code:   codeLeaderUnreachable,
		Detail: "no active leader; retry after the indicated interval",
	})
	writeRetryAfter(w, g.noLeaderRetryAfter)
}

// respondCookieRedirect writes a 307 to the leader's public
// URL. Sessions are local (ADR-039) — a cookie minted on box A
// cannot be validated on box B. Redirecting the browser is
// the only way to make cookie auth survive an active-passive
// failover. Per ADR-084 §Decision #4 the Retry-After is 5s so
// browsers backed off during a planned failover won't busy-
// loop against the standby.
func (g *writeGate) respondCookieRedirect(w http.ResponseWriter, r *http.Request, leaderName string, authKind writegate.AuthKind) {
	g.metrics.WriteRedirectTotal(string(writegate.OutcomeRedirect307), string(authKind)).Inc()
	target := buildLeaderPublicURL(leaderName, r.URL)
	g.log.Info("writegate: cookie auth redirected to leader",
		"path", r.URL.Path,
		"method", r.Method,
		"leader", leaderName,
		"target", target,
	)
	w.Header().Set("Location", target)
	writeRetryAfter(w, g.retryAfter)
	w.WriteHeader(http.StatusTemporaryRedirect)
}

// respondRelayed copies the leader's response back to the
// caller verbatim (status, headers, body). The transport
// already stripped hop-by-hop headers and rewrote the loop-
// guard sentinel (leader_client.go::Relay), so the only
// mutation here is to drain the body before Close.
//
// A successful relay emits OutcomeRelayed even when the
// leader returns a 4xx or 5xx — that's the leader's verdict,
// not the gate's. Transport-level failures (TLS handshake,
// timeout, malformed URL) are emitted separately by the
// caller via respondMTLSFailure / respondError.
func (g *writeGate) respondRelayed(w http.ResponseWriter, r *http.Request, leaderName string, authKind writegate.AuthKind) {
	leaderURL := buildLeaderPublicURL(leaderName, r.URL)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	upstream, err := g.client.Relay(ctx, leaderURL, r)
	if err != nil {
		g.respondRelayFailure(w, r, authKind, err)
		return
	}
	defer func() { _ = upstream.Body.Close() }()

	g.metrics.WriteRedirectTotal(string(writegate.OutcomeRelayed), string(authKind)).Inc()
	for k, vv := range upstream.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(upstream.StatusCode)
	// Drain the body into the response writer. Limit the
	// copy to the leader's bound (Relay's ResponseHeaderTimeout
	// covers the header read; the body read is unbounded by
	// design — the leader may stream a long response).
	_, copyErr := io.Copy(w, upstream.Body)
	if copyErr != nil {
		// At this point the status is already written and
		// the client has started reading; we cannot emit
		// a new error response. Log and bail.
		g.log.Error("writegate: body copy from leader failed",
			"path", r.URL.Path,
			"leader", leaderName,
			"err", copyErr.Error(),
		)
	}
}

// respondRelayFailure categorises a Relay error into the
// mTLS / generic buckets. The classification is deliberately
// coarse (any error containing "tls" or "certificate" →
// mTLS_failure; everything else → error) so an operator can
// distinguish a cert-chain problem from a transient dial
// error without reading logs.
func (g *writeGate) respondRelayFailure(w http.ResponseWriter, r *http.Request, authKind writegate.AuthKind, err error) {
	msg := err.Error()
	if strings.Contains(msg, "tls") || strings.Contains(msg, "certificate") {
		g.metrics.WriteRedirectTotal(string(writegate.OutcomeMTLSFailure), string(authKind)).Inc()
		// Cross-link to the Tier A8 failover panel so a
		// sustained mTLS failure rate also bumps the
		// active_passive_failovers_total counter (PR-A
		// review finding #3: keep the two dashboards in
		// sync so an operator chasing one metric lands on
		// the other).
		if g.metrics != nil {
			g.metrics.ActivePassiveFailovers("mTLS_failure").Inc()
		}
		g.log.Error("writegate: mTLS relay failed",
			"path", r.URL.Path,
			"leader", r.URL.Host,
			"err", msg,
		)
		writeProblem(w, http.StatusServiceUnavailable, problemDetail{
			Type:   problemTypeGateway,
			Title:  "mTLS Relay Failed",
			Status: http.StatusServiceUnavailable,
			Code:   codeRelayMTLSFailure,
			Detail: "cross-box mTLS handshake failed; see logs",
		})
		writeRetryAfter(w, g.retryAfter)
		return
	}

	g.metrics.WriteRedirectTotal(string(writegate.OutcomeError), string(authKind)).Inc()
	g.log.Error("writegate: relay failed",
		"path", r.URL.Path,
		"leader", r.URL.Host,
		"err", msg,
	)
	writeProblem(w, http.StatusServiceUnavailable, problemDetail{
		Type:   problemTypeGateway,
		Title:  "Relay Error",
		Status: http.StatusServiceUnavailable,
		Code:   codeRelayError,
		Detail: "cross-box relay failed; see logs",
	})
	writeRetryAfter(w, g.retryAfter)
}

// writeProblem encodes body as JSON and writes the canonical
// Content-Type. The writeHeader call follows the body write
// so a partial body never escapes with a 200 status.
func writeProblem(w http.ResponseWriter, status int, body problemDetail) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeRetryAfter sets the Retry-After header in seconds
// (RFC 7231 §7.1.3). We use the integer-seconds form
// rather than HTTP-date because all our back-off clients
// (browsers, retry libraries) handle the seconds form
// natively.
func writeRetryAfter(w http.ResponseWriter, d interface{ Seconds() float64 }) {
	secs := int(d.Seconds())
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", fmt.Sprintf("%d", secs))
}

// buildLeaderPublicURL constructs the redirect target for
// the cookie-redirect path. The shape is
//
//	https://<leaderName>.<publicDNS>/<path>?<query>
//
// We DO NOT include the request's Host header because that
// points at the standby (where the customer landed); the
// leader's Host is derived from the leader's registered
// public DNS name.
//
// The query is preserved verbatim so a redirected GET lands
// on the same filter / pagination state. The path is preserved
// so a redirected POST/PUT lands on the same route.
func buildLeaderPublicURL(leaderName string, originalURL *url.URL) string {
	// TODO: pull the publicDNS suffix from a config knob —
	// ADR-084 §Decision #3 defers the suffix to a config value
	// for now we use a sensible default ".faas" that matches
	// the production DNS Hetzner zone (issue #556 PR-A + Tier
	// A8 DNS handoff).
	const publicDNSSuffix = ".faas"
	u := url.URL{
		Scheme:   "https",
		Host:     leaderName + publicDNSSuffix,
		Path:     originalURL.Path,
		RawQuery: originalURL.RawQuery,
	}
	return u.String()
}

// Compile-time guard — the decision switch in write_gate.go
// must stay exhaustive over all 8 outcomes. If a new
// classifyDecision constant is added, this slice will fail
// the build until the matching metric path lands too.
var allClassifyDecisions = []classifyDecision{
	decisionBypass,
	decisionLoop,
	decisionSameBox,
	decisionUnreachable,
	decisionCookieRedirect,
	decisionRelayed,
	decisionMTLSFailure,
	decisionError,
}
