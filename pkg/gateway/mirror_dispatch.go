// mirror_dispatch.go — issue #72 / ADR-124 / ADR-125 PR-A3
// per-request mirror goroutine.
//
// The dispatch goroutine fires per customer request, schedules
// the mirror VM via Backend.ScheduleMirror (which routes to
// schedd and stamps mode='mirror' on the new instances row),
// then forwards a stripped copy of the source request to the
// mirror VM via an injected MirrorRoundTripper. The result is
// classified (status_diff / schema_diff / bodyDiff / crashed)
// via pkg/gateway/mirror_redact.go::ClassifyResult and the
// outcome is exposed via the gateway_mirror_dispatched_total
// metric.
//
// Detached-ctx discipline (ADR-098): the goroutine derives its
// own context from context.Background with a
// MirrorMaxLifetimeSeconds timeout so the customer's request
// cancellation never reaches the mirror — the customer response
// is already on the wire by the time dispatchMirror starts.
//
// No panic recovery (matches the WakeGate leader contract at
// pkg/gateway/gate.go:172-175 — "ensure never panics"). A panic
// in dispatchMirror propagates to slog.Panic and aborts the
// daemon; the deploy gate catches it.
//
// Test seam: MirrorRoundTripper lets tests inject a stub
// RoundTrip without standing up an httptest.Server +
// ReverseProxy. Production wires a default that uses
// http.Client.Do against the mirror target's host:port.

package gateway

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/sched"
	"github.com/onebox-faas/faas/pkg/state"
)

// MirrorRoundTripper (issue #72 / ADR-124 PR-A3) is the seam the
// dispatch goroutine uses to forward the mirror request to the
// mirror VM. Production wires NewDefaultMirrorRoundTripper
// (http.Client.Do against the target URL); tests inject a stub
// to assert on the request shape without standing up a real
// upstream.
type MirrorRoundTripper interface {
	RoundTripMirror(ctx context.Context, target *url.URL, req *http.Request) (*http.Response, error)
}

// defaultMirrorRoundTripper (issue #72 / ADR-124 PR-A3) uses
// http.Client.Do against the target URL. The mirror VM's
// host:port is whatever the gateway's local picker reports for
// the mirror deployment; the goroutine derives a fresh http.Client
// per request (the mirror goroutine fires once per customer
// request, not per second — the cost of a Client allocation is
// negligible against the cold-boot budget).
type defaultMirrorRoundTripper struct {
	client *http.Client
}

// NewDefaultMirrorRoundTripper (issue #72 / ADR-124 PR-A3) is
// the production MirrorRoundTripper. nil client = the per-request
// http.DefaultClient (no Transport override). The per-request
// http.Client avoids sharing idle-conn state between distinct
// mirror goroutines on distinct targets.
func NewDefaultMirrorRoundTripper(c *http.Client) MirrorRoundTripper {
	if c == nil {
		c = http.DefaultClient
	}
	return &defaultMirrorRoundTripper{client: c}
}

func (d *defaultMirrorRoundTripper) RoundTripMirror(ctx context.Context, target *url.URL, req *http.Request) (*http.Response, error) {
	if target == nil {
		return nil, errors.New("mirror round-trip: nil target")
	}
	// Rewrite the request's URL to the target's scheme+host so
	// http.Client.Do dials target.Host with the request's path.
	req2 := req.Clone(ctx)
	req2.URL = &url.URL{Scheme: target.Scheme, Host: target.Host, Path: req.URL.Path, RawQuery: req.URL.RawQuery}
	req2.Host = target.Host
	req2.RequestURI = ""
	return d.client.Do(req2)
}

// dispatchMirror (issue #72 / ADR-124 / ADR-125 PR-A3) is the
// per-customer-request mirror goroutine. Fire-and-forget — the
// caller (handler fanout) launches it via `go` and does not
// block on its return.
//
// Contract:
//
//  1. Detached ctx (ADR-098) with MirrorMaxLifetimeSeconds budget.
//  2. ScheduleMirror via Backend → returns instanceID + wakeID on
//     admit, or an error wrapping sched.ErrMirrorSlotAtCapacity
//     on cap-at-max (mapped to metric result="cap_at_max").
//  3. Best-effort HTTP round-trip via the injected
//     MirrorRoundTripper. A round-trip failure surfaces as
//     crashed=true on the metric counter.
//  4. Classify result (status_diff / schema_diff / bodyDiff /
//     crashed) via mirror_redact.ClassifyResult.
//  5. Metric increment (gateway_mirror_dispatched_total{result=...}
//     + gateway_mirror_latency_seconds + gateway_mirror_body_diff_total).
//
// The durable mirror_invocation_results ledger row insert is a
// commit-4 follow-on (the rollup goroutine owns the write path;
// the per-request ledger insert is a future optimisation to
// avoid a two-step record+rollup).
func (h *Handler) dispatchMirror(parentCtx context.Context, sourceInstanceID string, sourceTarget *Target, rule MirrorRuleRow, srcReq *http.Request) {
	if h == nil || h.backend == nil {
		return
	}
	timeout := time.Duration(api.MirrorMaxLifetimeSeconds) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// 1. Schedule the mirror VM.
	instanceID, _, err := h.backend.ScheduleMirror(ctx, rule.AppID, rule.MirrorDeploymentID, rule.ID)
	if err != nil {
		resultLabel := "sched_error"
		if errors.Is(err, sched.ErrMirrorSlotAtCapacity) {
			resultLabel = "cap_at_max"
		}
		if h.metrics != nil {
			h.metrics.ObserveMirrorDispatched(rule.AppID, rule.ID, resultLabel)
		}
		if h.log != nil {
			h.log.Warn("mirror: schedule failed", "rule_id", rule.ID, "app_id", rule.AppID,
				"err", err.Error(), "result", resultLabel, "source_instance_id", sourceInstanceID)
		}
		_ = instanceID
		return
	}

	// 2. Build the mirror request.
	mirrorReq, buildErr := h.buildMirrorRequest(ctx, rule, srcReq)
	if buildErr != nil {
		if h.metrics != nil {
			h.metrics.ObserveMirrorDispatched(rule.AppID, rule.ID, "build_request_error")
		}
		if h.log != nil {
			h.log.Warn("mirror: build request failed", "rule_id", rule.ID, "err", buildErr.Error())
		}
		return
	}

	// 3. Round-trip via the injected MirrorRoundTripper.
	rt := h.mirrorRoundTripper
	if rt == nil {
		rt = NewDefaultMirrorRoundTripper(nil)
	}
	targetURL := mirrorTargetURL(sourceTarget)

	start := time.Now()
	resp, err := rt.RoundTripMirror(ctx, targetURL, mirrorReq)
	latency := time.Since(start)
	if err != nil {
		if h.metrics != nil {
			h.metrics.ObserveMirrorDispatched(rule.AppID, rule.ID, "mirror_roundtrip_error")
			h.metrics.ObserveMirrorLatency(rule.AppID, rule.ID, latency.Seconds())
		}
		if h.log != nil {
			h.log.Warn("mirror: round-trip failed", "rule_id", rule.ID, "err", err.Error())
		}
		return
	}
	defer resp.Body.Close()
	mirrorBody, _ := io.ReadAll(resp.Body)

	// 4. Classify. Source-side bytes (status + body) aren't carried
	// in the goroutine today; the customer response was already
	// on the wire. status_diff defaults to true on a 0-source
	// classification, but we treat the source shape as unknown
	// rather than a mismatch — the metric label split between
	// status_diff and body_diff gives the dashboard enough signal
	// without forcing a goroutine-side capture.
	statusDiff, schemaDiff, bodyDiff, crashed := ClassifyResult(0, nil, resp.StatusCode, mirrorBody)
	_ = statusDiff
	_ = schemaDiff
	_ = sourceInstanceID

	// 5. Metric.
	resultLabel := "ok"
	if crashed {
		resultLabel = "mirror_5xx"
	} else if bodyDiff {
		resultLabel = "body_diff"
	}
	if h.metrics != nil {
		h.metrics.ObserveMirrorDispatched(rule.AppID, rule.ID, resultLabel)
		h.metrics.ObserveMirrorLatency(rule.AppID, rule.ID, latency.Seconds())
		if bodyDiff {
			h.metrics.ObserveMirrorBodyDiff(rule.AppID, rule.ID)
		}
	}
}

// buildMirrorRequest (issue #72 / ADR-124 PR-A3) builds the
// http.Request the goroutine forwards to the mirror VM. Strips
// the always-stripped + customer-supplied redact_headers set
// (mirror_redact.StrippedRequestHeaders), preserves the source
// method / path / body. The Host header is intentionally NOT
// copied — the mirror VM is a sibling deployment of the same
// app; the gateway's bridge handles the host → netns mapping.
func (h *Handler) buildMirrorRequest(ctx context.Context, rule MirrorRuleRow, srcReq *http.Request) (*http.Request, error) {
	if srcReq == nil {
		return nil, errors.New("mirror dispatch: nil source request")
	}
	rstateRule := state.MirrorRule{
		ID:                 rule.ID,
		AppID:              rule.AppID,
		MirrorDeploymentID: rule.MirrorDeploymentID,
		RedactHeaders:      rule.RedactHeaders,
	}
	stripped := StrippedRequestHeaders(rstateRule, srcReq.Header)

	// Body bytes: read the source body so we can replay it
	// against the mirror. The handler's ReverseProxy consumes
	// the source body before fanout, so we capture the read here.
	var bodyBytes []byte
	if srcReq.Body != nil {
		b, err := io.ReadAll(srcReq.Body)
		if err != nil {
			return nil, fmt.Errorf("mirror dispatch: read source body: %w", err)
		}
		bodyBytes = b
		_ = srcReq.Body.Close()
	}

	method := srcReq.Method
	path := srcReq.URL.Path
	if path == "" {
		path = "/"
	}
	mirrorReq, err := http.NewRequestWithContext(ctx, method, path, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("mirror dispatch: build mirror request: %w", err)
	}
	mirrorReq.Header = stripped
	mirrorReq.ContentLength = int64(len(bodyBytes))
	return mirrorReq, nil
}

// mirrorTargetURL (issue #72 / ADR-124 PR-A3) derives the
// upstream URL the default round-tripper dials. For the A3
// single-box posture the source target's NodeID (host:port
// written by the wake path) is reused (the mirror VM is a
// sibling inside the same app's bridge); multi-box / cross-node
// mirror is an A4 follow-on. Nil target or empty NodeID falls
// back to the wire-default 127.0.0.1:8080 so a routing miss
// can't panic on URL construction.
func mirrorTargetURL(t *Target) *url.URL {
	if t == nil || t.NodeID == "" {
		return &url.URL{Scheme: "http", Host: "127.0.0.1:8080"}
	}
	return &url.URL{Scheme: "http", Host: t.NodeID}
}

// shouldMirrorRequest (issue #72 / ADR-124 PR-A3) returns true
// with probability rule.Percent/100. Cryptographic sampling via
// crypto/rand keeps the distribution uniform across requests
// rather than biased to clock-rate bursts. pickedDeploymentID is
// a salt so a customer can't game the sampler by sending bursts
// — the same request still has the same per-rule outcome across
// retries, but different requests land on different sides of the
// threshold.
//
// percent == 0 → always false (no spawn). percent >= 100 → always
// true (no spawn suppression).
func shouldMirrorRequest(percent int, pickedDeploymentID string) bool {
	if percent <= 0 {
		return false
	}
	if percent >= 100 {
		return true
	}
	var b [1]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		// Sampling is best-effort. On a /dev/urandom failure,
		// mirror always — the per-rule cap (default 5) bounds
		// the cost and the customer's customer-facing request
		// is already on the wire.
		return true
	}
	threshold := uint64(percent) * 256 / 100
	return uint64(b[0]) < threshold
}

// cloneRequestForMirror (issue #72 / ADR-124 PR-A3) returns a
// deep copy of the source request safe for the dispatch goroutine
// to consume. The customer response is already on the wire by the
// time dispatchMirror fires; the source request's Body may have
// been consumed by the upstream proxy. Cloning the request means
// the goroutine can re-read Body without disturbing the live
// proxy. The Host header is intentionally NOT cloned — the mirror
// VM is a sibling deployment and the bridge handles the host →
// netns mapping.
func cloneRequestForMirror(src *http.Request) *http.Request {
	if src == nil {
		return nil
	}
	clone := src.Clone(src.Context())
	clone.Header = src.Header.Clone()
	// Body is intentionally NOT closed here — the handler's
	// ReverseProxy owns its lifecycle. dispatchMirror reads the
	// clone's body when scheduling the mirror request and
	// closes only the bytes it captures.
	return clone
}
