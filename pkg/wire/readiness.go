// Package wire — readiness.go owns the daemon-level readiness helpers
// (issue #571). It is the daemon-side counterpart of
// pkg/gateway/readiness.go, which serves the Tier A7 edge daemons.
//
// Why duplicated: pkg/gateway already imports pkg/wire (the
// dependency direction is gateway -> wire, not the reverse). Lifting
// the helpers into pkg/wire makes them available to every cmd/<daemon>/
// main.go without creating a cycle. The two implementations stay in
// lockstep via the ADR-129 footnote on daemon-level /readyz; a future
// PR can extract pkg/readiness if a third caller appears. For now the
// surface is small (~120 LOC) and stable, so the duplication cost is
// bounded.
//
// What lives here:
//
//   - ReadySignal: one component's "I'm ready" bit. Operators add
//     signals with ReadyzProbe.Register; the /readyz handler flips to
//     503 when ANY registered signal reports false. Signals are
//     independent — no priority order, no AND-of-children logic.
//   - ReadyzProbe: fan-in point. Daemons Register one ReadySignal
//     per component at construction; the /readyz handler calls All().
//     Zero state (no signals registered) returns ready=true — the
//     pre-split behaviour, so an early-boot scrape does not see a
//     spurious 503.
//   - NewStalenessSignal: signal that flips false after `stale`
//     elapses since the last Touch. Used by meterd's loop.Health
//     adapter (cmd/meterd/readiness.go).
//   - NewPGPingSignal: signal that tracks pgxpool.Pool liveness.
//     Used by apid, schedd, githubd (cmd/<daemon>/readiness.go).
//   - ControlMuxLite: registers /healthz (200 ok) and /readyz
//     (200 ready / 503 not-ready:<reason>) on an existing
//     http.ServeMux. Lighter than gateway.ControlMux (no drain
//     wrapping, no separate metrics registry).
//
// RunAndShutdown lives in runandshutdown.go (commit 11) — the drain
// helper is logically separate from the probe-fan-in shape and
// merited its own file.
package wire

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// ReadySignal is one component's "I'm ready" bit. Operators add
// signals with ReadyzProbe.Register; the /readyz handler flips to
// 503 when ANY registered signal reports false. Signals are
// independent — there is no priority order, no AND-of-children
// logic. If two signals report the same fact, the second one is
// authoritative (it overwrites the first).
//
// Every method is safe for concurrent use.
//
// Mirrors pkg/gateway/readiness.go::ReadySignal — see the ADR-129
// footnote in docs/adr/0129-deploy-observability.md on daemon-level
// /readyz for the rationale on duplication.
type ReadySignal struct {
	mu     sync.RWMutex
	ready  atomic.Bool // primary flag — read on the /readyz hot path
	reason string      // last reason string for the /readyz body
}

// Set flips the signal's ready bit. reason is optional — pass ""
// for "no human-readable reason needed". The /readyz handler
// surfaces the most recent reason across all registered signals
// when /readyz returns 503.
func (s *ReadySignal) Set(ready bool, reason string) {
	s.ready.Store(ready)
	s.mu.Lock()
	s.reason = reason
	s.mu.Unlock()
}

// Report returns the current bit and the most recent reason.
func (s *ReadySignal) Report() (ready bool, reason string) {
	return s.ready.Load(), s.lastReason()
}

func (s *ReadySignal) lastReason() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.reason
}

// ReadyzProbe is the fan-in point. Daemons Register one ReadySignal
// per component at construction; the /readyz handler calls All().
//
// All returns true iff every registered signal is ready. If no
// signals are registered (the zero state), All returns true — the
// pre-split behaviour, preserved so an early-boot scrape does not
// see a spurious 503.
//
// stoppers collects one stopper func per helper-backed signal
// (NewPGPingSignal, NewStalenessSignal, and per-daemon helpers
// like vmmdDialSignal / buildsDirSignal / writableSignal).
// Drain() fires every stopper BEFORE flipping signals to false
// so a helper goroutine can't re-flip a signal after the drain
// has set it. Manual signals (Register, RegisterSignal with nil
// stopper) don't have a stopper entry; Drain() ignores them.
//
// Mirrors pkg/gateway/readiness.go::ReadyzProbe.
type ReadyzProbe struct {
	mu       sync.RWMutex
	signals  []*ReadySignal
	stoppers []func()
}

// Register adds a new ReadySignal to the probe and returns it so
// the caller can Set bits on it later. New signals default to
// "not ready" with the reason "not yet ready" so /readyz surfaces a
// human-readable string during the first half-second of boot. The
// daemon flips each signal to ready (typically with Set(true, ""))
// as components come up. This is the deliberate behaviour — every
// component must opt IN to ready, never opt OUT.
//
// If a daemon wants the pre-split "always ready" behaviour during
// boot, the caller calls Set(true, "") immediately after Register.
func (p *ReadyzProbe) Register() *ReadySignal {
	s := &ReadySignal{}
	s.ready.Store(false) // explicit; atomic.Bool zero value is false
	s.reason = "not yet ready"
	p.mu.Lock()
	p.signals = append(p.signals, s)
	p.mu.Unlock()
	return s
}

// RegisterSignal adds a pre-constructed *ReadySignal to the probe
// plus an optional stopper func for the helper goroutine that
// drives the signal (NewPGPingSignal, NewStalenessSignal,
// vmmdDialSignal, buildsDirSignal, writableSignal, meterd's
// loop.Health adapter). Drain() fires every registered stopper
// synchronously BEFORE flipping signals to false; this prevents
// the helper goroutine from re-flipping a signal after the drain
// has set it (Finding 4 from PR #1091 review).
//
// stopper may be nil for signals that have no helper goroutine
// (vmmd's kvmOpenableSignal, fcBinarySignal, grpcBoundSignal,
// githubd's credsLoadedSignal / secretWiredSignal — manual flips).
// A nil stopper is silently dropped from the stoppers slice.
//
// Order in All() is the order of the Register / RegisterSignal
// calls, which is preserved for the /readyz reason concat.
func (p *ReadyzProbe) RegisterSignal(s *ReadySignal, stopper func()) {
	if s == nil {
		return
	}
	p.mu.Lock()
	p.signals = append(p.signals, s)
	if stopper != nil {
		p.stoppers = append(p.stoppers, stopper)
	}
	p.mu.Unlock()
}

// All returns true iff every registered signal is ready. The
// ready reason returned is the OR of all "not ready" reasons —
// the operator can see at a glance which component is not yet
// up. Reasons are concatenated with "; " for parseability.
func (p *ReadyzProbe) All() (ready bool, reason string) {
	p.mu.RLock()
	signals := make([]*ReadySignal, len(p.signals))
	copy(signals, p.signals)
	p.mu.RUnlock()
	if len(signals) == 0 {
		return true, ""
	}
	ready = true
	var reasons []string
	for _, s := range signals {
		r, why := s.Report()
		if !r {
			ready = false
			if why != "" {
				reasons = append(reasons, why)
			}
		}
	}
	if len(reasons) == 0 {
		return ready, ""
	}
	// Concatenate reasons. Cap at 5 to keep the body small.
	if len(reasons) > 5 {
		reasons = reasons[:5]
	}
	return ready, joinWireReasons(reasons)
}

// ReadyFunc returns a ReadyFunc suitable for ControlMuxLite (or
// any handler that takes a func() bool). The returned func is
// safe for concurrent use; the underlying All() is RLock-guarded.
func (p *ReadyzProbe) ReadyFunc() ReadyFunc {
	return func() bool {
		ok, _ := p.All()
		return ok
	}
}

// ReasonFunc returns a func that yields the current readiness
// reason string ("draining", "pg ping failed: ...", etc.) suitable
// for the /readyz body. Returns "" when ready.
func (p *ReadyzProbe) ReasonFunc() func() string {
	return func() string {
		_, reason := p.All()
		return reason
	}
}

func joinWireReasons(reasons []string) string {
	out := ""
	for i, r := range reasons {
		if i > 0 {
			out += "; "
		}
		out += r
	}
	return out
}

// NewStalenessSignal returns a ReadySignal whose bit flips false
// after `stale` elapses since the last Touch. The caller calls
// Touch() on every successful receive (e.g. every pg_notify
// delivery); the helper goroutine flips the bit on staleness.
//
// The helper goroutine lives for the lifetime of the signal;
// callers should typically construct the signal at boot and let
// it run forever. A shutdown hook that flips the signal false on
// SIGTERM prevents a late-arriving Touch from re-enabling a
// draining daemon — see cmd/gatewayd-public/drain.go for the
// canonical wiring, mirrored here for daemon-level /readyz.
//
// stale must be positive; pass api.CertSyncIntervalSeconds or
// the warm-hint publish cadence at the call site.
//
// The helper runs at the staleness check cadence (1s by default)
// regardless of Touch rate, so a /readyz scrape sees the flip
// within ~1s of staleness.
//
// Concurrency note: touch() BOTH writes the timestamp AND flips
// the signal ready. The helper goroutine only writes false (on
// staleness). Without the optimistic ready-set on touch, a
// /readyz scrape that lands AFTER the previous tick flipped
// stale but BEFORE the next tick would observe the stale bit —
// even though the touch was fresh. The goroutine catches
// staleness; the touch path is the hot recovery path readers
// (LB probes, ops dashboards) observe.
//
// Mirrors pkg/gateway/readiness.go::NewStalenessSignal.
func NewStalenessSignal(stale time.Duration) (signal *ReadySignal, touch func(), stopper func()) {
	s := &ReadySignal{}
	s.ready.Store(false)
	var lastTouch atomic.Int64 // unix nanos; 0 = "never touched"
	stop := make(chan struct{})
	done := make(chan struct{})
	// Cadence: half the staleness window so a /readyz scrape sees
	// the staleness flip within ≤stale/2 of the actual timeout. Cap
	// the cadence at 1 s — going faster on long windows (the common
	// 30 s CertSyncInterval case) would burn CPU for no signal gain.
	cadence := stale / 2
	if cadence > time.Second {
		cadence = time.Second
	}
	if cadence < 10*time.Millisecond {
		cadence = 10 * time.Millisecond
	}
	touchFn := func() {
		lastTouch.Store(time.Now().UnixNano())
		s.Set(true, "")
	}
	go func() {
		defer close(done)
		t := time.NewTicker(cadence)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				touched := lastTouch.Load()
				if touched == 0 {
					// No touch yet — keep signalling not ready.
					s.Set(false, "no touch yet")
					continue
				}
				age := time.Since(time.Unix(0, touched))
				if age > stale {
					s.Set(false, "stale")
					continue
				}
				// Fresh — re-flip ready so the signal is invariant
				// under tick/touch interleaving. touch() also writes
				// ready, but a tick that arrives just after a stale
				// flip and before the next touch would observe stale
				// state from the prior tick without this re-set.
				s.Set(true, "")
			}
		}
	}()
	stopperFn := func() {
		close(stop)
		<-done
		s.Set(false, "shutting down")
	}
	// Pre-arm the signal — the first touch flips it ready.
	s.Set(true, "")
	return s, touchFn, stopperFn
}

// NewPGPingSignal returns a ReadySignal whose bit tracks the liveness
// of a pgxpool.Pool. The helper goroutine pings the pool every
// `every` (default 5 s if zero); on success the bit flips true, on
// failure the bit flips false with the most recent error message as
// the reason.
//
// Pings are bounded: the helper cancels any in-flight ping when the
// next tick fires, so a wedged connection cannot stall the readiness
// loop. The returned stopper should be called on daemon shutdown so
// the bit flips false before the process exits — same pattern as
// NewStalenessSignal's stopper.
//
// every must be positive; pass 5 s at the call site for the
// post-split daemon shape. The P50 pgxpool.Ping on EX44 is
// sub-millisecond when the pool has warm connections (per ADR-040
// bench); under pool exhaustion the ping will block on Connect for
// up to pgx's DialTimeout (default 5 s).
//
// Mirrors pkg/gateway/readiness.go::NewPGPingSignal.
func NewPGPingSignal(ctx context.Context, pool pinger, every time.Duration) (*ReadySignal, func()) {
	if every <= 0 {
		every = 5 * time.Second
	}
	s := &ReadySignal{}
	s.ready.Store(false)
	s.Set(false, "pg ping not yet attempted")
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		// First tick is half-every so the daemon has a "we tried" bit
		// in /readyz within ~half a probe interval of boot.
		t := time.NewTicker(every / 2)
		defer t.Stop()
		ping := func() {
			// Bound the ping to half-every; if the pool is wedged
			// (pool.Ping blocks), the next tick cancels this one.
			pctx, cancel := context.WithTimeout(ctx, every/2)
			defer cancel()
			if err := pool.Ping(pctx); err != nil {
				s.Set(false, "pg ping failed: "+err.Error())
				return
			}
			s.Set(true, "")
		}
		// Kick one ping immediately so the bit flips to "ready" the
		// instant the daemon can reach Postgres, instead of after
		// `every/2` of idle time.
		ping()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				s.Set(false, "pg ctx cancelled")
				return
			case <-t.C:
				ping()
			}
		}
	}()
	var stopOnce sync.Once
	stopper := func() {
		stopOnce.Do(func() {
			close(stop)
			<-done
			s.Set(false, "pg ping stopped")
		})
	}
	return s, stopper
}

// pinger is the subset of *pgxpool.Pool we need for NewPGPingSignal.
// Defining it locally avoids dragging pgxpool into every test
// import; the production wiring passes *pgxpool.Pool directly.
//
// Same shape as pkg/gateway/readiness.go::pinger.
type pinger interface {
	Ping(context.Context) error
}

// ReadyFunc is the contract ControlMuxLite / any handler accepts:
// a goroutine-safe predicate returning the daemon's readiness.
// Returns true ⇒ /readyz returns 200; false ⇒ /readyz returns 503
// with the reason surfaced via ReasonFunc.
type ReadyFunc func() bool

// ControlMuxLite registers /healthz and /readyz on an existing
// http.ServeMux — the daemon's metrics mux, typically (so the LB
// already scraping /metrics gets /healthz + /readyz on the same
// URL pattern). Lighter than pkg/gateway.ControlMux: no drain
// tracker wrapping, no separate metrics registry. Drains are
// handled separately by wire.RunAndShutdown (commit 11) flipping
// the probe's signals to false.
//
// Body shape:
//
//	GET /healthz → 200 "ok"
//	GET /readyz  → 200 "ready" if ready() is true
//	               503 "not-ready:<reason>" otherwise
//
// The bodies are short ASCII to keep log-shipping cheap. Operators
// triage via the response body plus the daemon_ready Prometheus
// gauge (issue #586 / ADR-129).
//
// mux is required; readyFunc may be nil — passing nil degrades to
// the pre-split "always ready" behaviour (always 200), preserved
// so a partial-boot daemon that hasn't wired its probe yet does
// not brick the LB scrape path. Once the daemon constructs its
// probe, the caller replaces the registration with one driven by
// the probe (or uses RunAndShutdown's single-source-of-truth
// pattern in commit 11).
func ControlMuxLite(mux *http.ServeMux, readyFunc ReadyFunc, reasonFunc func() string) {
	if mux == nil {
		return
	}
	if readyFunc == nil {
		readyFunc = func() bool { return true }
	}
	if reasonFunc == nil {
		reasonFunc = func() string { return "" }
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if readyFunc() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ready"))
			return
		}
		body := "not-ready:" + reasonFunc()
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(body))
	})
}
