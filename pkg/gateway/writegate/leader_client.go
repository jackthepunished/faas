// leader_client.go — cross-box mTLS client for the Tier A9 /
// ADR-084 standby write-redirect layer.
//
// The writeGate (cmd/gatewayd-internal/write_gate.go, PR-B
// sub-task B6) classifies a mutating request on a standby
// node as `relayed` and calls `LeaderHTTPClient.Relay` to
// forward the request to the LEADER node's gatewayd-public
// listener over mTLS. This file holds the production client.
//
// # Cert material
//
// The leaf lives at
//
//	/etc/faas/tls/gatewayd-internal-public/leader-client.crt
//	/etc/faas/tls/gatewayd-internal-public/leader-client.key
//
// CN="gatewayd-internal-public.faas" with SAN
// "gatewayd-public.faas" (the receiving server's CN). The
// server leaf it dials is at
//
//	/etc/faas/tls/gatewayd-public/leader-server.crt
//
// with CN="gatewayd-public.faas" and SAN
// "gatewayd-internal-public.faas". Both roles were added to
// pkg/pki/pki.go::Roles in PR-B sub-task B5. Operators run
// `gregale pki init` / `gregale pki rotate` to mint the
// material; the runtime never issues certs.
//
// # Transport
//
// `Relay` copies the inbound request verbatim to the leader:
// preserves method, path, query, body, and the
// `x-faas-request-id` header. Strips hop-by-hop headers
// (Connection, Keep-Alive, Proxy-Authenticate, Proxy-
// Authorization, TE, Trailers, Transfer-Encoding,
// Upgrade — RFC 7230 §6.1) and the inbound
// X-Faas-Forwarded-Leader (the loop-guard sentinel; the
// outbound hop REWRITES this header to the LOCAL node name
// per ADR-084 §Decision #5). Forbids automatic redirect
// following — the gate emits its own 307 response so the
// client always lands on the leader (a stdlib follow would
// loop if the leader emitted a redirect).
//
// The ResponseHeaderTimeout is the gate's
// `pkg/api/limits.go::StandbyWriteRedirectTimeoutMS` (5 s).
// A timeout maps to `OutcomeError` (transport-level
// failure, distinct from a 5xx the leader returned — the
// gate observes upstream status verbatim, regardless of
// 2xx/4xx/5xx, and increments `relayed` only on transport
// success; transport failure increments `error`).
package writegate

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// LeaderHTTPClient is the gate's view of the cross-box
// transport. PR-B ships one production implementation
// (`MTLSLeaderClient`); tests pass an `httptest`-backed fake
// so the gate can be exercised end-to-end without spinning
// up a second node.
type LeaderHTTPClient interface {
	// Relay copies the inbound request to leaderURL and
	// returns the leader's response. The body and headers
	// of the response are owned by the caller — Relay
	// returns the response open and the caller must
	// Close it (the writeGate copies headers + body
	// verbatim into its own ResponseWriter before
	// closing).
	//
	// Errors are transport-level only (TLS handshake,
	// dial timeout, response-header timeout). A 5xx from
	// the leader is NOT an error — it's a successful
	// `Relayed` outcome that the gate copies back to the
	// client.
	Relay(ctx context.Context, leaderURL string, req *http.Request) (*http.Response, error)
}

// MTLSLeaderClient is the production LeaderHTTPClient. It
// holds a single *http.Client with a pre-loaded TLS config
// and a per-call timeout.
type MTLSLeaderClient struct {
	httpClient *http.Client
	timeout    time.Duration
}

// NewMTLSLeaderClient loads the leaf + CA from disk and
// returns a configured client. certFile/keyPath/caPath are
// the paths the operator materialised via `gregale pki
// init` (see pkg/pki.Roles — the leader-client and
// leader-server roles added in PR-B sub-task B5).
//
// timeout caps the time from request write to response
// header read (the gate maps a timeout to `OutcomeError` +
// 503 Retry-After: 5). A zero timeout is treated as 5 s
// (the Tier A9 quota `StandbyWriteRedirectTimeoutMS`) —
// callers should pass that explicitly so the gate's metric
// and log agree with the actual bound.
//
// # Import-cycle note
//
// We deliberately do NOT call
// `wire.LoadClientTLSConfigWithPrefix` here. `pkg/wire`
// already imports `pkg/gateway/writegate` for the closed
// label vocabulary (writeRedirectOutcomes / writeRedirect-
// AuthKinds), so a `writegate → wire` import would cycle.
// Instead we replicate the loader's three-line behaviour
// inline — LoadX509KeyPair + x509.NewCertPool + MinVersion
// TLS 1.3. The two paths diverge ONLY in error string
// formatting (`wire` prefixes errors with `wire:`; we use
// `writegate:`), and the contract is identical.
func NewMTLSLeaderClient(certFile, keyFile, caFile string, timeout time.Duration) (*MTLSLeaderClient, error) {
	tlsCfg, err := loadLeaderTLSConfig(certFile, keyFile, caFile)
	if err != nil {
		return nil, fmt.Errorf("writegate: load mTLS material: %w", err)
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &MTLSLeaderClient{
		httpClient: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig:       tlsCfg,
				ResponseHeaderTimeout: timeout,
				// Pool semantics: the cross-box hop is
				// short-lived; disable keep-alives so a
				// failed standby doesn't half-open a
				// connection to the leader.
				DisableKeepAlives: true,
			},
			// Forbid automatic redirect following — the
			// gate emits its own 307 to the leader, and
			// a stdlib follow would loop if the leader
			// emitted a redirect (the loop-guard
			// sentinel stops the loop after one hop,
			// but explicit is better than implicit).
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Timeout: timeout,
		},
		timeout: timeout,
	}, nil
}

// loadLeaderTLSConfig is the in-package replica of
// pkg/wire.loadClientTLSConfig (no prefix argument — the
// caller knows which fields are required and embeds them
// in the returned error message). See "Import-cycle note"
// on NewMTLSLeaderClient for the rationale.
func loadLeaderTLSConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	if certFile == "" && keyFile == "" && caFile == "" {
		return nil, errors.New("writegate: leader_redirect_tls_cert_path, leader_redirect_tls_key_path, leader_redirect_tls_ca_path all empty")
	}
	if certFile == "" || keyFile == "" || caFile == "" {
		var missing []string
		if certFile == "" {
			missing = append(missing, "leader_redirect_tls_cert_path")
		}
		if keyFile == "" {
			missing = append(missing, "leader_redirect_tls_key_path")
		}
		if caFile == "" {
			missing = append(missing, "leader_redirect_tls_ca_path")
		}
		return nil, fmt.Errorf("writegate: leader mTLS material incomplete; missing %s", strings.Join(missing, ","))
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("writegate: load leader client keypair: %w", err)
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("writegate: read leader CA %q: %w", caFile, err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("writegate: leader CA file %q contained no PEM-encoded certificates", caFile)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caPool,
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{"h2"},
	}, nil
}

// Timeout reports the per-call bound. Used by the gate's
// metric label and by runbook tests that want to assert
// "the bound was set from the Tier A9 quota, not from a
// hard-coded 5s".
func (c *MTLSLeaderClient) Timeout() time.Duration { return c.timeout }

// Relay implements LeaderHTTPClient.
func (c *MTLSLeaderClient) Relay(ctx context.Context, leaderURL string, req *http.Request) (*http.Response, error) {
	parsed, err := url.Parse(leaderURL)
	if err != nil {
		return nil, fmt.Errorf("writegate: parse leader URL %q: %w", leaderURL, err)
	}
	if parsed.Scheme != "https" {
		return nil, fmt.Errorf("writegate: leader URL must be https, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, errors.New("writegate: leader URL has empty host")
	}
	if parsed.User != nil {
		return nil, errors.New("writegate: leader URL must not contain userinfo")
	}

	// Build the outbound request. We do NOT use
	// req.Clone(ctx) — stdlib's Clone copies the URL
	// verbatim, and we need to re-target.
	outURL := *parsed
	outURL.Path = singleSlashJoin(parsed.Path, req.URL.Path)
	outURL.RawQuery = mergeQuery(parsed.RawQuery, req.URL.RawQuery)
	outReq, err := http.NewRequestWithContext(ctx, req.Method, outURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("writegate: build outbound request: %w", err)
	}

	// Copy headers. Strip hop-by-hop + the inbound loop-
	// guard sentinel (the outbound hop REWRITES this
	// header to the local node name; preserving the
	// inbound value would let an attacker spoof the
	// sentinel via the very request the relay is
	// processing).
	copyRequestHeaders(req, outReq)

	// Loop-guard sentinel: ALWAYS overwrite with the
	// local node name (set in cmd/gatewayd-internal/main.go
	// as FAAS_NODE_NAME). If the inbound already had a
	// sentinel value (an attacker trying to spoof, or a
	// legitimate cross-box hop that was already
	// loop-prevented upstream), the writer here erases
	// it. The receiving leader checks for the presence
	// of the header — NOT its value — so the rewrite is
	// idempotent on the receiving side.
	if local := strings.TrimSpace(req.Header.Get("X-Faas-Node")); local != "" {
		outReq.Header.Set(LoopGuardSentinel, local)
	} else {
		// We don't have a reliable local node name at
		// this layer; the gate will set it before
		// calling Relay. Defensive: set a non-empty
		// placeholder so the receiving leader's
		// "presence-of-header" check still trips.
		outReq.Header.Set(LoopGuardSentinel, "unknown")
	}

	// Preserve x-faas-request-id verbatim — it's the
	// request-correlation token minted by
	// gatewayd-public; rewriting it would orphan every
	// log line for this request.
	if rid := req.Header.Get("x-faas-request-id"); rid != "" {
		outReq.Header.Set("x-faas-request-id", rid)
	}

	// Body copy: the inbound req.Body may be nil (a
	// POST without a body) or a single-shot
	// io.ReadCloser. We MUST read it before handing to
	// http.NewRequestWithContext because the relay runs
	// inside a request goroutine — if the inbound
	// goroutine returns before we read, the body would
	// be drained on the wrong side of the connection.
	var bodyBytes []byte
	if req.Body != nil {
		buf := &bytes.Buffer{}
		if _, err := io.Copy(buf, req.Body); err != nil {
			return nil, fmt.Errorf("writegate: read inbound body: %w", err)
		}
		_ = req.Body.Close()
		bodyBytes = buf.Bytes()
		outReq.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		outReq.ContentLength = int64(len(bodyBytes))
	}

	//nolint:gosec // G704: outReq URL is constructed via buildOutboundRequest()
	// which validates scheme=https, host non-empty, no userinfo, and chains
	// only the path/query from the inbound request. The taint analysis
	// can't see the validation chain; the cross-box mTLS hop IS the SSRF
	// mitigation we want (the destination is operator-deployed infra).
	return c.httpClient.Do(outReq)
}

// copyRequestHeaders copies headers from src to dst, dropping
// hop-by-hop headers per RFC 7230 §6.1 and the loop-guard
// sentinel (the outbound hop REWRITES the sentinel below).
func copyRequestHeaders(src, dst *http.Request) {
	hopByHop := map[string]bool{
		"Connection":          true,
		"Keep-Alive":          true,
		"Proxy-Authenticate":  true,
		"Proxy-Authorization": true,
		"Te":                  true,
		"Trailers":            true,
		"Transfer-Encoding":   true,
		"Upgrade":             true,
	}
	for k, v := range src.Header {
		if hopByHop[k] {
			continue
		}
		// Skip the inbound loop-guard sentinel — the
		// outbound hop overwrites it with the local
		// node name.
		if k == LoopGuardSentinel {
			continue
		}
		dst.Header[k] = v
	}
}

// singleSlashJoin joins a base path and a request path with
// exactly one "/" between them, handling the case where
// either side already ends/starts with a slash.
func singleSlashJoin(base, reqPath string) string {
	baseSlash := strings.HasSuffix(base, "/")
	reqSlash := strings.HasPrefix(reqPath, "/")
	switch {
	case baseSlash && reqSlash:
		return base + reqPath[1:]
	case !baseSlash && !reqSlash:
		return base + "/" + reqPath
	default:
		return base + reqPath
	}
}

// mergeQuery concatenates two raw query strings, preserving
// any existing ordering. Returns "" when both inputs are
// empty so the resulting URL has no `?`.
func mergeQuery(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		// Both non-empty: join with "&" unless either
		// already has "&" trailing. Most callers pass
		// canonical key=value strings; this avoids
		// double-ampersand.
		sep := "&"
		if strings.HasSuffix(a, "&") || strings.HasPrefix(b, "&") {
			sep = ""
		}
		return a + sep + b
	}
}

// Compile-time interface check — fails the build if
// MTLSLeaderClient ever drifts from the interface.
var _ LeaderHTTPClient = (*MTLSLeaderClient)(nil)
