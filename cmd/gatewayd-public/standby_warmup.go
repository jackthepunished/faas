// StandbyWarmup wiring for cmd/gatewayd-public (Tier A8 / ADR-083 /
// code-review fix #6).
//
// Production wires the WarmupLoop's Slugs and OnError hooks so
// the standby box actually probes something AND surfaces
// probe failures on a Prometheus counter. Default-zero
// WarmupLoop{} probes zero apps and silently swallows errors
// — observability-dead per the audit.
//
// Slugs is fed by a small in-process list. The list comes from
// a static operator-managed file (default
// /etc/faas/standby_warmup_slugs.json) so this daemon — which
// is plain-HTTP and doesn't speak the apid unix socket — has
// a hot-path-free source of truth. The file is read once at
// boot; reload is a future PR (SIGHUP precedent in
// pkg/wire/daemon.go).
//
// OnError bumps a Prometheus counter on the shared OpsMetrics
// so a sustained probe-failure rate alerts via the §12 panel
// (the previous zero-op swallowed failures silently).

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/gateway"
	"github.com/onebox-faas/faas/pkg/wire"
)

// defaultWarmupSlugsPath is the operator-managed list of
// standby-warm app slugs. JSON array shape matches the
// manifest pattern in pkg/storage for consistency with other
// operator-facing files.
const defaultWarmupSlugsPath = "/etc/faas/standby_warmup_slugs.json"

// slugsLoader is the goroutine-safe loader the WarmupLoop
// calls once per tick.
type slugsLoader struct {
	mu    sync.RWMutex
	slugs []string
}

// load reads the JSON array at path. Errors are logged and
// the previous list is retained — a transient read error
// must NOT blank out the standby's warmup.
func (l *slugsLoader) load(path string, log *slog.Logger) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var slugs []string
	if err := json.Unmarshal(data, &slugs); err != nil {
		return err
	}
	l.mu.Lock()
	l.slugs = slugs
	l.mu.Unlock()
	log.Info("gatewayd-public: standby warmup slugs loaded", "path", path, "count", len(slugs))
	return nil
}

// Slugs returns the current list. Called by WarmupLoop.tick.
func (l *slugsLoader) Slugs() []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]string, len(l.slugs))
	copy(out, l.slugs)
	return out
}

// runStandbyWarmup blocks until ctx is cancelled, probing
// every slug in slugs on each tick. Errors bump the
// gatewayd_public_warmup_errors_total counter on the shared
// OpsMetrics.
//
// Skip this entirely if FAAS_STANDBY_WARMUP_ENABLED is set to
// "0" / "false" / "no" (single-box dev path — a box that is
// ALSO the leader has nothing to warm up).
func runStandbyWarmup(
	ctx context.Context,
	log *slog.Logger,
	ops *wire.OpsMetrics,
) error {
	if !envBoolOr("FAAS_STANDBY_WARMUP_ENABLED", true) {
		log.Info("gatewayd-public: standby warmup disabled by env")
		<-ctx.Done()
		return nil
	}
	loader := &slugsLoader{}
	slugsPath := envOr("FAAS_STANDBY_WARMUP_SLUGS_PATH", defaultWarmupSlugsPath)
	if err := loader.load(slugsPath, log); err != nil {
		log.Warn("gatewayd-public: standby warmup slugs load failed at boot; warmup is no-op until next reload",
			"path", slugsPath, "err", err.Error())
	}
	warmupErrCount := ops.WarmupErrors
	// The prober targets the gatewayd-public's own public
	// listener on loopback so the warmup probes what the
	// customer would see (the unix-socket-to-internal hop
	// hides errors that the public listener surfaces).
	prober := gateway.NewHTTPWarmupProber(
		&http.Client{Timeout: time.Duration(api.HAFailoverProbeTimeoutMS) * time.Millisecond},
		envOr("FAAS_PUBLIC_LISTEN_ADDR", defaultListenAddr),
	)
	loop := &gateway.WarmupLoop{
		Prober:       prober,
		Interval:     time.Duration(envIntOr("FAAS_STANDBY_WARMUP_INTERVAL_MS", api.HAStandbyWarmupIntervalMS)) * time.Millisecond,
		ProbeTimeout: time.Duration(api.HAFailoverProbeTimeoutMS) * time.Millisecond,
		OnError: func(appSlug string, err error) {
			warmupErrCount(appSlug).Inc()
			log.Warn("gatewayd-public: standby warmup probe failed",
				"slug", appSlug, "err", err.Error())
		},
		Slugs: loader.Slugs,
	}
	log.Info("gatewayd-public: standby warmup loop starting",
		"interval_ms", api.HAStandbyWarmupIntervalMS,
		"probe_timeout_ms", api.HAFailoverProbeTimeoutMS,
		"slugs", len(loader.Slugs()),
	)
	return loop.Run(ctx)
}

// envIntOr parses an env var as an int; missing/empty/invalid
// returns def. Same shape as envBoolOr.
func envIntOr(envKey string, def int) int {
	v := os.Getenv(envKey)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
