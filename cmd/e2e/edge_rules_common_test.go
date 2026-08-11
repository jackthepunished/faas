// edge_rules_common_test.go — shared helpers for the per-kind edge-rules
// e2e files (rewrite / redirect / headers / cors / jwt / ip; see ADR-091
// D18 / issue #561 PR 6).
//
// The package-level doReq in quota_e2e_test.go is intentionally NOT
// modified: 138 call sites across cmd/e2e/ rely on its (body, status)
// shape and none captures resp.Header. The new doReqHeaders below is
// additive; gatewayd-bound requests use h.GatewayURL and set req.Host
// explicitly because no DNS is wired up in the harness (mirroring
// deploy_wake_metal_test.go:doGetWithHost at lines 372-388).
//
// Bitmask for all six e2e files: e2etest.APID | e2etest.Gatewayd.
// Edge-rule handlers all short-circuit or 404-fallthrough before
// Backend.Lookup, so no schedd/vmmd/imaged is required (D20.1
// defers kind=route e2e to a DeployWake-bitmask follow-on PR).

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/e2etest"
)

// gatewayReqOptions captures the gateway-bound knobs that doReqHeaders
// needs but the apid-bound package-level doReq does not: the explicit
// Host header (the gateway router keys on it) and an optional pre-built
// body (the rewrite/headers/cors cases send nil; the jwt case sends no
// body but a Bearer header).
type gatewayReqOptions struct {
	Host          string
	Authorization string
	Extra         map[string]string
}

// gatewayReq is the gateway-bound counterpart to package-level doReq.
// Always uses h.GatewayURL (gatewayd-internal serves plain HTTP per
// Tier A7; no TLS), always sets req.Host for the host-based router,
// and returns the full resp.Header so the per-kind tests can assert on
// Access-Control-*, Location, X-*, WWW-Authenticate, etc.
func gatewayReq(t *testing.T, h *e2etest.Harness, method, path string, body any, opts gatewayReqOptions) (*http.Response, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, h.GatewayURL+path, r)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	req.Host = opts.Host
	if opts.Authorization != "" {
		req.Header.Set("Authorization", opts.Authorization)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range opts.Extra {
		req.Header.Set(k, v)
	}
	ctx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
	defer cancel()
	req = req.WithContext(ctx)
	resp, err := h.HTTPClient().Do(req)
	if err != nil {
		t.Fatalf("%s %s (Host=%s): %v", method, path, opts.Host, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

// doReqHeaders is the helper that captures resp.Header alongside
// (body, status). Returns (header, body, status) so a test can assert
// on stamped headers (Location, Access-Control-Allow-Origin, X-*, etc.)
// without re-issuing the request.
//
// host is the Host header the gateway router keys on. method is
// GET/POST/OPTIONS/PUT/DELETE. path is the request path (no leading
// host; h.GatewayURL supplies the scheme + IP). body is JSON-marshalled
// if non-nil; pass nil for GET/OPTIONS. extraHeaders are set last so
// they can override the default Content-Type (rare; needed for
// application/x-www-form-urlencoded preflight, etc.).
func doReqHeaders(t *testing.T, h *e2etest.Harness, host, method, path string, body any, extraHeaders ...map[string]string) (http.Header, []byte, int) {
	t.Helper()
	var merged map[string]string
	if len(extraHeaders) > 0 {
		merged = make(map[string]string)
		for _, m := range extraHeaders {
			for k, v := range m {
				merged[k] = v
			}
		}
	}
	resp, bodyBytes := gatewayReq(t, h, method, path, body, gatewayReqOptions{
		Host:  host,
		Extra: merged,
	})
	return resp.Header, bodyBytes, resp.StatusCode
}
