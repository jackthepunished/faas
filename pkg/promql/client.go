// Package promql is a thin HTTP client for the Prometheus query API
// used by apid for both the public status page (cmd/apid/status.go)
// and the per-app metrics endpoint behind issue #273 / ADR-041.
//
// Extracted from cmd/apid/status.go so two consumers (the existing
// /status/slo.json handler and the new GET /v1/apps/{slug}/metrics
// handler) share one tested transport and one sealed seam. The seam
// takes an HTTPDoer so tests can use httptest.Server without
// depending on http.DefaultClient's state.
package promql

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// HTTPDoer is the minimal interface Client needs from net/http. Matches
// http.Client.Do so the production wiring is `http.DefaultClient`. Tests
// pass httptest.Server.Client() which also satisfies it.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Client is a Prometheus query client bound to one base URL. Construct
// with NewClient; do not zero-initialise (the http.Client would be nil).
type Client struct {
	baseURL string
	doer    HTTPDoer
	timeout time.Duration
}

// NewClient builds a client. baseURL is the Prometheus root (no
// trailing slash, no /api/v1 suffix). doer is the http.Client
// (production: nil → 3s-timeout client; tests: an httptest.Server
// client). timeout is the per-query context timeout; pass 0 for the
// 3s default.
func NewClient(baseURL string, doer HTTPDoer) *Client {
	if doer == nil {
		doer = &http.Client{Timeout: 3 * time.Second}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), doer: doer, timeout: 3 * time.Second}
}

// SetTimeout overrides the per-query timeout. Use for slow queries in
// tests; production uses the 3s default.
func (c *Client) SetTimeout(d time.Duration) { c.timeout = d }

// BaseURL returns the configured Prometheus base URL.
func (c *Client) BaseURL() string { return c.baseURL }

// QueryScalar runs query against Prometheus and returns the first
// scalar. Returns an error on transport failure, non-2xx, parse
// error, unsupported resultType (only vector and scalar are valid
// here), or empty result.
//
// PromQL has three result shapes; only "scalar" and "vector" can
// appear for the instant-query API endpoint we call, and they're
// encoded differently in the JSON envelope:
//   - vector → {"resultType":"vector", "result":[{"value":[ts,"x"]}, ...]}
//   - scalar → {"resultType":"scalar", "result":[{"value":[ts,"x"]}]}
//   - matrix → {"resultType":"matrix", "result":[{"values":[...]}], ...}
//
// Both vector and scalar must be supported because e.g.
// `count(ALERTS{alertstate="firing"}) > 0` returns a scalar (the
// §12 degraded flag depends on this). The Value[0] is a timestamp in
// both shapes; the parsed scalar lives at Value[1].
func (c *Client) QueryScalar(ctx context.Context, query string) (float64, error) {
	if c == nil || c.baseURL == "" {
		return 0, fmt.Errorf("promql: client not configured")
	}
	qctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	u := c.baseURL + "/api/v1/query?query=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(qctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.doer.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return 0, fmt.Errorf("prometheus %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var pr struct {
		Data struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Value [2]any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return 0, err
	}
	if pr.Data.ResultType != "vector" && pr.Data.ResultType != "scalar" {
		return 0, fmt.Errorf("unsupported resultType %q for query %q", pr.Data.ResultType, query)
	}
	if len(pr.Data.Result) == 0 {
		return 0, fmt.Errorf("no data for query %q", query)
	}
	raw, ok := pr.Data.Result[0].Value[1].(string)
	if !ok {
		return 0, fmt.Errorf("unexpected value shape for query %q", query)
	}
	// ParseFloat (not fmt.Sscanf "%f") — locale-safe and consistent
	// with pkg/fcvm/metrics.go::DefaultLvFcUsedPct.
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %q for query %q: %w", raw, query, err)
	}
	return f, nil
}
