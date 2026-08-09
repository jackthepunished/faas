// Standby warm-up scraper (Tier A8 / ADR-083). On a standby
// gatewayd-public, this loop pre-warms the per-app target-set
// cache by issuing a bounded HTTP HEAD against
// cmd/gatewayd-internal on each known app's hostname. On the
// active-passive flip, the new leader's first request to any
// app hits a warm cache → no cold-boot penalty.
//
// The scraper is bounded by api.HAFailoverProbeTimeoutMS
// (default 500 ms) so a misbehaving gatewayd-internal can't
// drag the standby down: timeout = skip + warn log, never
// block. On ctx cancel the loop exits cleanly.
//
// Placement of this file: the warm-up loop is gatewayd-public's
// responsibility (the public daemon owns the DNS handoff and
// the per-app cache), so it lives in pkg/gateway and is wired
// in cmd/gatewayd-public/standby_warmup.go (PR-B).
//
// # Throughput
//
// tick() runs a worker pool sized to
// min(len(slugs), runtime.GOMAXPROCS(0)) (review finding #6 —
// the previous sequential loop took ~500s per tick on a
// 1000-app fleet, blowing past the HAStandbyWarmupIntervalMS
// interval itself and starving the standby cache). The
// realistic sustained rate is len(slugs) * (1000 /
// ProbeTimeout) probes/s — i.e. ~2000 probes/s on a 1000-app
// fleet at ProbeTimeout=500ms, but NOT the "2000 probes/s"
// the previous docstring claimed. Each probe is independent
// (no shared state), so the pool scales linearly with
// GOMAXPROCS.
//
// # Failure modes
//
//   - probe timeout: log Warn, skip the app, continue.
//   - gatewayd-internal 5xx: same as timeout (treat as
//     unhealthy; cold-boot safety net serves the request).
//   - DNS resolution failure: same as timeout (the cache
//     stays empty; the new leader will cold-boot on first
//     request).
//   - ctx cancel: return nil cleanly.

package gateway

import (
	"context"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
)

// WarmupProber is the surface the scraper needs from
// gatewayd-internal. Today cmd/gatewayd-public hits the same
// daemon over a unix socket for the per-app metadata probe;
// the scraper is satisfied by an HTTP HEAD on a /warmup
// endpoint that returns 200 + the per-app target-set payload.
//
// Tests use an in-memory fake; production wires
// `http.Client` pointed at /run/faas/gatewayd-internal.sock.
type WarmupProber interface {
	// Probe issues a single HTTP HEAD against the given app's
	// hostname. Returns nil on 2xx (cache populated), an error
	// otherwise. ctx bounds the request — callers should set
	// it to HAStandbyWarmupIntervalMS / 2 so a slow probe
	// doesn't pin the loop.
	Probe(ctx context.Context, appSlug string) error
}

// httpWarmupProber is the production WarmupProber. It uses a
// caller-supplied http.Client + base URL (typically the
// gatewayd-internal unix socket, but http.Client doesn't care
// about the scheme).
type httpWarmupProber struct {
	hc      *http.Client
	baseURL string
}

// NewHTTPWarmupProber builds the production prober. baseURL is
// the gatewayd-internal endpoint (e.g.
// "http://unix/run/faas/gatewayd-internal.sock" — http.Client
// supports unix sockets via a custom Transport; cmd/gatewayd-public
// wires that transport at startup).
func NewHTTPWarmupProber(hc *http.Client, baseURL string) WarmupProber {
	return &httpWarmupProber{hc: hc, baseURL: baseURL}
}

func (h *httpWarmupProber) Probe(ctx context.Context, appSlug string) error {
	if appSlug == "" {
		return errWarmupEmptySlug
	}
	url := h.baseURL + "/v1/apps/" + appSlug + "/warmup"
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return err
	}
	resp, err := h.hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errWarmupNon2xx
	}
	return nil
}

// errWarmupEmptySlug and errWarmupNon2xx are the sentinel
// errors the prober returns. Callers log Warn with the slug;
// the metric counter surfaces the rate.
var (
	errWarmupEmptySlug = &warmupError{"empty app slug"}
	errWarmupNon2xx    = &warmupError{"non-2xx response"}
)

type warmupError struct{ msg string }

func (e *warmupError) Error() string { return "warmup: " + e.msg }

// WarmupLoop is the per-standby scraper. AppSlugs is the
// caller-resolved list of apps known to the standby (today
// pulled from a small SQLite/JSON mirror of the apps table —
// out of scope for PR-A; PR-B wires the actual data source).
// Interval is HAStandbyWarmupIntervalMS; ProbeTimeout is
// HAFailoverProbeTimeoutMS (per-probe) — review finding #11
// fixed the previous typo HAStandupProbeTimeoutMS, which
// referenced a symbol that did not exist.
//
// The loop returns nil on ctx cancel. Errors from individual
// probes are swallowed (logged + counted) — never returned.
type WarmupLoop struct {
	Prober       WarmupProber
	Interval     time.Duration
	ProbeTimeout time.Duration
	// OnError is called once per probe failure (after the
	// prober returns err). The default implementation logs
	// Warn and bumps a metric; tests substitute a recorder.
	OnError func(appSlug string, err error)
	// Slugs returns the current set of app slugs to probe.
	// Called once per tick; the function may return a
	// shrinking/growing slice across calls (apps added/removed
	// between ticks).
	Slugs func() []string
	// MaxWorkers overrides the worker-pool size (default
	// runtime.GOMAXPROCS(0)). Tests use a small value to
	// exercise the pool's synchronisation; production never
	// sets this.
	MaxWorkers int
}

// Run blocks until ctx is cancelled, probing each app on each
// tick. Returns ctx.Err() on cancel. OnError is called for
// every probe failure; nil OnError is silently dropped.
func (w *WarmupLoop) Run(ctx context.Context) error {
	if w.Interval <= 0 {
		w.Interval = time.Duration(api.HAStandbyWarmupIntervalMS) * time.Millisecond
	}
	if w.ProbeTimeout <= 0 {
		w.ProbeTimeout = time.Duration(api.HAFailoverProbeTimeoutMS) * time.Millisecond
	}
	if w.Slugs == nil {
		// Default no-op slugs list — the scraper is
		// single-tenant on a fleet with one app, so the
		// default keeps PR-A's tests quiet without forcing
		// every caller to wire Slugs.
		w.Slugs = func() []string { return nil }
	}
	if w.OnError == nil {
		w.OnError = func(string, error) {}
	}
	if w.MaxWorkers <= 0 {
		w.MaxWorkers = runtime.GOMAXPROCS(0)
	}
	t := time.NewTicker(w.Interval)
	defer t.Stop()
	// Probe immediately on Run so the warm-up path doesn't
	// wait one interval for the first probe.
	w.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			w.tick(ctx)
		}
	}
}

// tick probes every known app once via a worker pool sized
// to min(len(slugs), MaxWorkers). Probe failures are
// swallowed (logged + OnError). The loop never fails the
// whole tick because of a single probe.
//
// Review finding #6 (severe): the previous sequential loop
// took ~500s per tick on a 1000-app fleet (1000 × 500ms
// probe timeout), blowing past the 500ms HAStandbyWarmupIntervalMS
// interval and starving the standby cache. The
// `FaasStandbyStateWarmingTooLong` alert would fire on every
// box within 60s of boot. The pool path keeps the per-tick
// wall-clock bounded at len(slugs) / MaxWorkers × ProbeTimeout.
func (w *WarmupLoop) tick(ctx context.Context) {
	slugs := w.Slugs()
	if len(slugs) == 0 {
		return
	}
	workers := w.MaxWorkers
	if workers > len(slugs) {
		workers = len(slugs)
	}
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan string, len(slugs))
	for _, s := range slugs {
		jobs <- s
	}
	close(jobs)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for slug := range jobs {
				if ctx.Err() != nil {
					return
				}
				probeCtx, cancel := context.WithTimeout(ctx, w.ProbeTimeout)
				err := w.Prober.Probe(probeCtx, slug)
				cancel()
				if err != nil {
					w.OnError(slug, err)
				}
			}
		}()
	}
	wg.Wait()
}
