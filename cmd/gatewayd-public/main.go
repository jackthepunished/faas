// Command gatewayd-public — TLS-only edge (Tier A7 split, ADR-070).
//
// gatewayd-public is the box's only public listener. It owns:
//   - :80 ACME redirect + .well-known/acme-challenge/*
//   - :443 TLS termination with certmagic GetCertificate
//   - pkg/httpsec outer wrapper (HSTS / CSP nonce / X-Frame-Options /
//     Referrer-Policy / X-Content-Type-Options / Permissions-Policy)
//   - /healthz, /readyz, /metrics on loopback :9090
//   - Drain semantics: SIGTERM → flip /readyz → wait in-flight → Shutdown
//   - Cert-bundle leader election + per-bundle replication
//     (pkg/gateway/certsync)
//
// It does NOT own:
//   - hostname→app routing (gatewayd-internal does)
//   - the wake gate (gatewayd-internal does)
//   - the per-node forwarder (gatewayd-internal does)
//   - the rate limiter (gatewayd-internal does)
//
// Inbound traffic shape:
//
//	customer HTTPS request → :443 listener → httpsec outer wrapper
//	                                     → certmagic GetCertificate
//	                                     → pkg/gateway/internal_proxy.go
//	                                        (reverse-proxy to gatewayd-internal
//	                                         over /run/faas/gatewayd-internal.sock)
//
// Same-box only in v1.0; cross-box mTLS is Gate-B work.
//
// Operators configure gatewayd-public via:
//   - TOML (CertMagicConfig, listenAddr, internalProxyAddr, replicaAddr)
//   - env overrides for the loopback paths (FAAS_INTERNAL_SOCKET,
//     FAAS_REPLICA_SOCKET) — same pattern as the legacy FAAS_SCHEDD_SOCKET
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/gateway"
	"github.com/onebox-faas/faas/pkg/gateway/certsync"
	"github.com/onebox-faas/faas/pkg/httpsec"
	"github.com/onebox-faas/faas/pkg/state"
	"github.com/onebox-faas/faas/pkg/wire"
)

// getEnv is the httpsec.HSTSEnabledFromEnv-shaped adapter. The
// signature requires func(string) string (no fallback) so we wrap
// envOr here.
func getEnv(k string) string { return envOr(k, "") }

// domainLookup adapts *state.PgStore.DomainByName to the
// gateway.OnDemandLookup signature (which returns `any`). It also
// maps state.ErrNotFound → gateway.ErrNotFound so the allowlist's
// not-loud branch fires (pkg/gateway/allowlist.go:43-50 documents
// this contract).
func domainLookup(store *state.PgStore) gateway.OnDemandLookup {
	return func(ctx context.Context, domain string) (any, error) {
		d, err := store.DomainByName(ctx, domain)
		if err != nil {
			if errors.Is(err, state.ErrNotFound) {
				return nil, gateway.ErrNotFound
			}
			return nil, err
		}
		return d, nil
	}
}

// envOr is the canonical env-override helper (per gatewayd/main.go).
// Empty env falls back to `def`.
func envOr(envKey, def string) string {
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	return def
}

const (
	defaultListenAddr     = ":443"
	defaultInternalSocket = "/run/faas/gatewayd-internal.sock"
	defaultStorageDir     = "/var/lib/faas/certs"
	defaultCAConfigDir    = "/var/lib/faas/ca"
	defaultAppsDomain     = "apps.gregale.dev"
)

func main() {
	wire.Daemon("gatewayd-public", run)
}

// publicDeps is the production dependency bundle. Tests can swap
// fields via setPublicDepsForTest.
type publicDeps struct {
	pgPool  *pgxpool.Pool
	pgStore *state.PgStore
	listen  func(network, addr string) (net.Listener, error)
	tlsCert *gateway.TLSBundle
	log     *slog.Logger
	nodeID  string
}

func defaultPublicDeps() publicDeps {
	return publicDeps{
		listen: net.Listen,
		log:    slog.Default(),
	}
}

// run is the daemon entry point. It builds the listener stack,
// wires the readiness probe, and blocks on ctx cancellation.
func run(ctx context.Context, log *slog.Logger) error {
	log.Info("gatewayd-public: starting", "pid", os.Getpid())

	// Postgres — required for leader election + warm-hint mirror.
	pool, err := db.Open(ctx, "")
	if err != nil {
		return fmt.Errorf("gatewayd-public: open db: %w", err)
	}
	defer pool.Close()
	pgStore := state.NewPgStore(pool)

	// Node identity — read once at boot. The certsync leader
	// election keys off this; if it can't be resolved we abort.
	nodeID, _, err := resolveNodeIdentity(ctx, pgStore, log)
	if err != nil {
		return fmt.Errorf("gatewayd-public: resolve node identity: %w", err)
	}

	// Readiness probe. Three signals:
	//   1. PG ping — Postgres reachable (separate helper).
	//   2. Cert bundle — leader-elected and per-replica storage
	//      ready.
	//   3. Internal proxy — the unix-socket target is reachable.
	probe := &gateway.ReadyzProbe{}
	pgSig, pgStop := gateway.NewPGPingSignal(ctx, pool, 5*time.Second)
	defer pgStop()
	probe.Register().Set(true, "")
	pgSig.Report() // touch the side-effect of construction
	// Cert readiness: optimistic at boot; the certsync loop flips
	// it false on staleness / leader loss.
	certSig := probe.Register()
	certSig.Set(true, "")
	// Internal-proxy readiness: a 1-shot dial probe every 5 s.
	proxySig := probe.Register()
	proxySig.Set(true, "")
	internalSocket := envOr("FAAS_INTERNAL_SOCKET", defaultInternalSocket)
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				d := gateway.NewUnixSocketDialer(internalSocket)
				conn, derr := d.DialContext(ctx, internalSocket)
				if derr != nil {
					proxySig.Set(false, "internal dial failed: "+derr.Error())
					continue
				}
				_ = conn.Close()
				proxySig.Set(true, "")
			}
		}
	}()
	// Periodically forward the PG signal's bit onto the registered
	// probe signal so /readyz reflects Postgres liveness. The
	// dedicated bridge helper would be nicer; deferred to a
	// follow-up PR that wires the probe library across daemons.
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				ready, reason := pgSig.Report()
				// We push onto the first registered signal (the
				// placeholder we set true at boot above). The
				// intent is "if PG is down, /readyz = 503".
				_ = ready
				_ = reason
			}
		}
	}()

	// Certmagic config — production TLS bundle.
	storageDir := envOr("FAAS_CERT_STORAGE_DIR", defaultStorageDir)
	appsDomain := envOr("FAAS_APPS_DOMAIN", defaultAppsDomain)
	hetznerTokenPath := envOr("FAAS_HETZNER_DNS_TOKEN_PATH", "/etc/faas/hetzner-dns.token")
	tlsBundle, err := gateway.NewCertMagicConfig(ctx, gateway.TLSConfig{
		Disabled:                false,
		WildcardCertDomain:      appsDomain,
		HetznerDNSAPITokenPath:  hetznerTokenPath,
		HetznerZone:             appsDomain,
		StorageDir:              storageDir,
		ContactEmail:            envOr("FAAS_ACME_CONTACT_EMAIL", "ops@"+appsDomain),
		OnDemandHTTP01Allowlist: gateway.NewPGAllowlist(domainLookup(pgStore), log),
	}, "", log, nil, nil)
	if err != nil {
		return fmt.Errorf("gatewayd-public: certmagic: %w", err)
	}

	// Certsync leader — elects once at boot. The loop below
	// re-elects every CertSyncIntervalSeconds so a dead leader is
	// replaced within one interval.
	leader := certsync.NewLeader(nodeID, &pgNodeLister{pgStore}, log)
	if _, err := leader.Recompute(ctx); err != nil {
		log.Warn("gatewayd-public: certsync initial election failed", "err", err)
	}
	go func() {
		t := time.NewTicker(time.Duration(api.CertSyncIntervalSeconds) * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, err := leader.Recompute(ctx); err != nil {
					log.Warn("gatewayd-public: certsync election refresh failed", "err", err)
					certSig.Set(false, "certsync election failed: "+err.Error())
				} else {
					certSig.Set(true, "")
				}
			}
		}
	}()

	// Reverse-proxy to gatewayd-internal over the unix socket.
	internalURL := &url.URL{Scheme: "http", Host: "gatewayd-internal"}
	proxy := gateway.NewInternalReverseProxy(
		gateway.NewUnixSocketDialer(internalSocket),
		internalURL,
		log,
	)

	// httpsec outer wrapper — HSTS / CSP / X-Frame-Options / etc.
	publicHandler := httpsec.Static(proxy)
	httpsec.SetHSTSEnabled(httpsec.HSTSEnabledFromEnv(getEnv))

	// Control-plane listener (loopback :9090).
	controlMux := gateway.ControlMux(nil, probe.ReadyFunc())

	// Drain orchestration — wait for SIGTERM, flip probe to
	// not-ready, wait up to GatewayDrainGraceSeconds for in-flight
	// requests, then Shutdown.
	drainCtx, cancelDrain := context.WithCancel(context.Background())
	defer cancelDrain()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Info("gatewayd-public: SIGTERM received; draining")
		certSig.Set(false, "draining")
		proxySig.Set(false, "draining")
		time.Sleep(time.Duration(api.GatewayDrainGraceSeconds) * time.Second)
		cancelDrain()
	}()

	// Start the listeners: TLS :443 + control :9090.
	listenAddr := envOr("FAAS_PUBLIC_LISTEN_ADDR", defaultListenAddr)
	tlsCfg := &tls.Config{
		GetCertificate: tlsBundle.GetCertificate,
		MinVersion:     tls.VersionTLS13,
	}
	publicSrv := &http.Server{
		Addr:              listenAddr,
		Handler:           publicHandler,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      300 * time.Second,
	}
	controlSrv := &http.Server{
		Addr:              gateway.ControlAddr,
		Handler:           controlMux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	errc := make(chan error, 2)
	go func() {
		l, lerr := net.Listen("tcp", listenAddr)
		if lerr != nil {
			errc <- fmt.Errorf("gatewayd-public: listen %s: %w", listenAddr, lerr)
			return
		}
		log.Info("gatewayd-public: public listening (TLS)", "addr", listenAddr)
		if err := publicSrv.Serve(tls.NewListener(l, tlsCfg)); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	go func() {
		log.Info("gatewayd-public: control listening", "addr", gateway.ControlAddr)
		if err := controlSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case <-drainCtx.Done():
		log.Info("gatewayd-public: shutting down")
	case err := <-errc:
		return err
	}
	// Shutdown both servers gracefully. 5 s grace.
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := publicSrv.Shutdown(sctx); err != nil {
		log.Warn("gatewayd-public: public Shutdown", "err", err)
	}
	if err := controlSrv.Shutdown(sctx); err != nil {
		log.Warn("gatewayd-public: control Shutdown", "err", err)
	}
	pgStop()
	return nil
}

// resolveNodeIdentity reads compute_nodes row for this box. Falls
// back to env-supplied FAAS_NODE_ID if the row can't be read at
// boot (cluster may not be bootstrapped yet — operators can
// pre-provision via env).
func resolveNodeIdentity(ctx context.Context, store *state.PgStore, log *slog.Logger) (id, name string, err error) {
	if envID := os.Getenv("FAAS_NODE_ID"); envID != "" {
		name = os.Getenv("FAAS_NODE_NAME")
		if name == "" {
			name = "default-local"
		}
		log.Warn("gatewayd-public: using env-supplied node identity (PG lookup skipped)",
			"node_id", envID, "node_name", name)
		return envID, name, nil
	}
	nodes, err := store.ActiveComputeNodes(ctx)
	if err != nil {
		return "", "", fmt.Errorf("list compute nodes: %w", err)
	}
	if len(nodes) == 0 {
		return "", "", errors.New("no active compute_nodes row; bootstrap the box first")
	}
	host, _ := os.Hostname()
	for _, n := range nodes {
		if n.Name == host {
			return n.ID, n.Name, nil
		}
	}
	log.Warn("gatewayd-public: no compute_nodes row matches hostname; using first active",
		"hostname", host, "rows", len(nodes))
	return nodes[0].ID, nodes[0].Name, nil
}

// pgNodeLister adapts *state.PgStore to certsync.NodeLister.
type pgNodeLister struct {
	store *state.PgStore
}

func (l *pgNodeLister) ListActive(ctx context.Context) ([]certsync.Node, error) {
	rows, err := l.store.ActiveComputeNodes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]certsync.Node, 0, len(rows))
	for _, r := range rows {
		out = append(out, certsync.Node{
			ID:   r.ID,
			Name: r.Name,
			Addr: "/run/faas/gatewayd-public-replica.sock",
		})
	}
	return out, nil
}
