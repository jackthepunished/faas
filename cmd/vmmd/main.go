// Command vmmd — microVM supervisor: firecracker + jailer, the only root
// component (spec §4.4). vmmd owns everything that touches
// /usr/bin/firecracker and the jailer. It is the sole root-privileged daemon;
// per-VM work drops to the jailer immediately. Do not add a path that lets
// another component touch firecracker directly (spec §Component ownership).
//
// M1 wires the gRPC control surface (CreateFromSnapshot, CreateColdBoot,
// Pause+Snapshot, Destroy, Stats) per ADR-013..016. The control-plane TCP
// port is gated by the metrics_addr config field; the only required listen
// is the unix-domain socket at /run/faas/vmmd.sock.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"filippo.io/age"
	"github.com/jackc/pgx/v5/pgxpool"
	vmmdpb "github.com/onebox-faas/faas/api/proto/onebox/faas/vmmd/v1"
	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/fcvm"
	"github.com/onebox-faas/faas/pkg/fcvm/cpustats"
	"github.com/onebox-faas/faas/pkg/fcvm/netstats"
	"github.com/onebox-faas/faas/pkg/netns"
	"github.com/onebox-faas/faas/pkg/sched"
	"github.com/onebox-faas/faas/pkg/secretbox"
	"github.com/onebox-faas/faas/pkg/state"
	"github.com/onebox-faas/faas/pkg/storage"
	"github.com/onebox-faas/faas/pkg/vmmdgrpc"
	"github.com/onebox-faas/faas/pkg/wire"
	"google.golang.org/grpc"
)

// ellipticP256 returns the P-256 curve. Mirrors
// sched.ecdsaP256 without importing the unexported name from
// pkg/sched — we just need a stable pointer for the curve
// equality check inside loadNodeSigningKey.
func ellipticP256() elliptic.Curve { return elliptic.P256() }

// defaultNodeKeyPath is the canonical location for the slice-3
// node signing key (ADR-053). Mirrors DefaultSignKeyPath from
// pkg/cosign but lives under the vmmd-specific secrets dir so a
// future signer split (e.g. per-daemon keys) doesn't collide.
// Mode 0400 root:root on the install (PR-#237 stat-assert).
const defaultNodeKeyPath = "/etc/faas/secrets/vmmd/node.key"

// errNodeKeyInsecure is returned by loadNodeSigningKey when the
// node.key file's mode permits any group/other access. Inserting
// a node key whose file is readable by the faas-imaged or faas-
// schedd user is a §11 G2 violation: the canonical install is
// 0400 root:root (vmmd is the only root daemon, so the file is
// only readable because vmmd runs as root). Anything looser
// (group read, world read, any write/exec/setuid) is a PKI
// tamper signal and the daemon refuses to start.
var errNodeKeyInsecure = errors.New("vmmd: node.key mode permits group/other access")

// loadNodeSigningKey loads the per-node ECDSA P-256 signing key
// vmmd uses to stamp every CapacityReport with node_signature
// (ADR-053).
//
// Returns (nil, "", nil) when the file is missing — single-box
// dev default falls through to pre-slice-3 mode (unsigned
// reports). The wire field is additive per ADR-016, so legacy
// schedd silently accepts the empty signature.
//
// On a non-empty file: the file must be mode 0400 root:root
// (mode 0440 owner+group read is NOT accepted here — vmmd is
// the only root daemon, so the canonical install is owner-only
// to keep the post-restart file-mode stat-assert simple). The
// PEM type must be PRIVATE KEY (PKCS#8) and the curve must be
// P-256. The key_id is computed once as SHA-256(SPKI) hex so the
// hot path stays cheap and the schedd-side registry can bind
// signatures to the leaf's identity without re-running the PEM
// parse on every report.
//
// The matching public key is registered in schedd's
// compute_node_keys table by an out-of-band install step; the
// registry listens for `compute_node_changed` pg_notify and
// picks up the row within its next refresh tick (migration
// 00075).
func loadNodeSigningKey() (*ecdsa.PrivateKey, string, error) {
	path := envOr("FAAS_VMMD_NODE_KEY_PATH", defaultNodeKeyPath)
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Pre-slice-3 mode: no node key on disk. The
			// publisher emits unsigned reports; legacy
			// schedd accepts, slice-3 schedd (with a
			// configured registry) rejects the stream.
			return nil, "", nil
		}
		return nil, "", fmt.Errorf("vmmd: stat node key %q: %w", path, err)
	}
	// Mode 0400 strict. vmmd is root, so group/other bits are
	// an unambiguous tamper signal. Anything looser →
	// errNodeKeyInsecure (fail-loud, not fail-open).
	if perm := info.Mode().Perm(); perm != 0o400 {
		return nil, "", fmt.Errorf("vmmd: node.key %q mode %#o: %w",
			path, perm, errNodeKeyInsecure)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("vmmd: read node key %q: %w", path, err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, "", fmt.Errorf("vmmd: node key %q is not PEM-encoded", path)
	}
	// PKCS#8 form. The image builder (cmd/faas-pki) emits
	// "PRIVATE KEY" (PKCS#8), not "EC PRIVATE KEY" (SEC1),
	// so the matching PEM type is required.
	if block.Type != "PRIVATE KEY" {
		return nil, "", fmt.Errorf("vmmd: node key %q PEM type %q, want PRIVATE KEY",
			path, block.Type)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, "", fmt.Errorf("vmmd: parse node key %q: %w", path, err)
	}
	priv, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, "", fmt.Errorf("vmmd: node key %q is not ECDSA (got %T)", path, key)
	}
	if priv.Curve != ellipticP256() {
		return nil, "", fmt.Errorf("vmmd: node key %q curve %s, want P-256",
			path, priv.Curve.Params().Name)
	}
	keyID, err := sched.KeyIDForPublicKey(&priv.PublicKey)
	if err != nil {
		return nil, "", fmt.Errorf("vmmd: compute key_id for %q: %w", path, err)
	}
	return priv, keyID, nil
}

const metricsPath = "/metrics"

func main() {
	wire.Daemon("vmmd", run)
}

// runDeps is the dependency-injection seam for testing. Production code
// uses the defaults; tests can swap individual fields to drive `run` without
// needing KVM, root, or a real /etc/faas/vmmd.toml.
type runDeps struct {
	configPath string                                                                                                // defaults to /etc/faas/vmmd.toml
	detectFC   func(context.Context) (string, error)                                                                 // defaults to fcvm.DetectFirecrackerVersion
	listen     func(ctx context.Context, target string, tlsCfg *tls.Config, daemonUser string) (net.Listener, error) // defaults to wire.ListenAs (issue #95 / ADR-025)
	// openDB / openStore: only invoked when [compute_node].name is set;
	// the legacy default-local path skips the DB entirely (no upsert).
	openDB    func(context.Context, string) (*pgxpool.Pool, error)
	openStore func(*pgxpool.Pool) *state.PgStore
	// detectOverlayIP — best-effort, default shelles out to
	// `tailscale ip -4`. nil means "skip overlay detection"
	// (WireGuard-mode operators set [compute_node].overlay_ip
	// explicitly and don't need this hook).
	detectOverlayIP func(context.Context) (string, error)
	// hostKey plumbing — function-typed so tests can drive first-boot
	// (LoadHostKey returns ErrHostKeyNotFound → GenerateAndSaveHostKey)
	// and restart (LoadHostKey returns id) paths without touching disk.
	loadHostKey    func(path string) (*age.X25519Identity, error)
	genAndSaveKey  func(path string) (*age.X25519Identity, error)
	writeRecipient func(path string, id *age.X25519Identity) error
	// popCounters: PR-E egress-deny poll seam. nil → netns.PopCounters
	// (metal) / netns.PopCounters non-metal stub (unit tests on dev box).
	// Tests inject a stub map-returning func to drive runEgressPoll
	// without shelling out to nft.
	popCounters popCountersFunc
	// egressPollInterval: PR-E override for runEgressPoll's cadence.
	// nil → EgressPollInterval (15s). Tests inject a 1ms cadence so the
	// loop ticks fast enough to be observable in a unit test.
	egressPollInterval *time.Duration
	// startEgressPoll: PR-E seam. nil → start the production goroutine
	// bound to ctx + ops + popCounters + log. Tests inject a no-op to
	// skip the loop entirely, or a callback to observe the seam args.
	startEgressPoll func(ctx context.Context, ops *wire.OpsMetrics, pop popCountersFunc, interval time.Duration, log *slog.Logger)
	// scheddTarget: ADR-025 axis 5 — vmmd's outbound gRPC target for
	// the capacity publisher. Empty disables the publisher entirely
	// (single-box dev default). Tests inject a fake target to drive
	// the seam without a real schedd.
	scheddTarget string
	// scheddClientTLS: ADR-052 — mTLS config the capacity publisher
	// uses to dial schedd. nil → no TLS (single-box unix socket);
	// loaded from cfg.ScheddClientTLS in main.go and passed through.
	// Tests inject a stub to assert the seam forwards the right
	// *tls.Config to wire.DialContext.
	scheddClientTLS *tls.Config
	// capacityInterval: ADR-025 axis 5 — override for the publisher's
	// tick cadence. nil → CapacityInterval (1 s). Tests inject
	// sub-second cadence so the loop has observable ticks in a unit
	// test.
	capacityInterval *time.Duration
	// residentFn: ADR-025 axis 5 — leakcheck seam. nil → leakcheck.ResidentBytes.
	// Tests inject a stub returning a fixed map.
	residentFn func() (map[string]int64, bool)
	// startCapacityPublish: ADR-025 axis 5 — seam for the publisher
	// goroutine. nil → start the production loop rooted at runCapacityPublish.
	// Tests inject a no-op to skip the loop or a callback to drive
	// the seam args.
	//
	// `counts` is a countReader (PR-1 review) rather than a concrete
	// *fcvm.Manager so the production wiring still passes `mgr` (which
	// satisfies the interface) and tests can inject a stub without
	// booting a real Manager.
	startCapacityPublish func(ctx context.Context, counts countReader, nodeID string, cfg ComputeNodeConfig, scheddTarget string, scheddClientTLS *tls.Config, tick time.Duration, resident func() (map[string]int64, bool), nodeKey *ecdsa.PrivateKey, nodeKeyID string, log *slog.Logger)
}

func defaultDeps() runDeps {
	return runDeps{
		configPath:         envOr("FAAS_VMMD_CONFIG", "/etc/faas/vmmd.toml"),
		detectFC:           fcvm.DetectFirecrackerVersion,
		listen:             wire.ListenAs,
		openDB:             db.Open,
		openStore:          state.NewPgStore,
		detectOverlayIP:    defaultDetectOverlayIP,
		loadHostKey:        secretbox.LoadHostKey,
		genAndSaveKey:      secretbox.GenerateAndSaveHostKey,
		writeRecipient:     secretbox.WriteRecipientFile,
		popCounters:        netns.PopCounters,
		egressPollInterval: durationPtr(EgressPollInterval),
		startEgressPoll:    nil, // defaultDeps() leaves nil so the runtime branch can detect "use production"
		scheddTarget:       envOr("FAAS_VMMD_SCHEDD_TARGET", "unix:///run/faas/schedd.sock"),
		capacityInterval:   durationPtr(CapacityInterval),
		// residentFn left nil; runWithDeps fills it with
		// leakcheck.ResidentBytes once the resolver runs.
		// startCapacityPublish left nil; the runtime branch
		// detects "use production" and calls runCapacityPublish.
	}
}

func durationPtr(d time.Duration) *time.Duration { return &d }

// envOr returns the value of env key, or fallback when unset/empty.
// Named envOr to avoid colliding with any same-named helper in
// cmd/<other-daemon> if these are ever linked into the same binary.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func run(ctx context.Context, log *slog.Logger) error {
	return runWithDeps(ctx, log, defaultDeps())
}

func runWithDeps(ctx context.Context, log *slog.Logger, deps runDeps) error {
	cfg, err := LoadConfig(deps.configPath)
	if err != nil {
		return err
	}
	listenTarget := cfg.ResolveListenTarget()
	log.Info("config", "listen_addr", listenTarget, "socket", cfg.SocketPath, "kernel_key", cfg.KernelKey,
		"kernel_path_legacy", cfg.KernelPath,
		"metrics_addr", cfg.MetricsAddr)

	// Fill in host-key defaults if a test passed a zero-value runDeps
	// without these. The other deps (configPath, detectFC, listen) are
	// not defaulted here — they're test seams where nil is meaningful
	// (e.g. TestRun_BadConfigPath passes configPath = a directory).
	if deps.loadHostKey == nil {
		deps.loadHostKey = secretbox.LoadHostKey
	}
	if deps.genAndSaveKey == nil {
		deps.genAndSaveKey = secretbox.GenerateAndSaveHostKey
	}
	if deps.writeRecipient == nil {
		deps.writeRecipient = secretbox.WriteRecipientFile
	}

	// Snapshots are pinned to the running Firecracker version (ADR-005);
	// detect it so restore only loads compatible snapshots and everything
	// else cold boots.
	fcVersion, err := deps.detectFC(ctx)
	if err != nil {
		log.Warn("could not detect firecracker version; treating all snapshots as stale", "err", err)
	}
	// Issue #96 / ADR-025 axis 2 (PR #116): derive the canonical
	// StorageBackend key for the kernel artifact from the detected
	// FC version. Operators may pin a specific key via vmmd.toml
	// (cfg.KernelKey); when unset we fall back to the version-keyed
	// form sched.KernelKey(fcVersion). The deprecated cfg.KernelPath
	// still flows into the log line so an operator can spot drift
	// between the two during the migration window.
	//
	// When fcVersion is empty (the FC-detect-failure warning path
	// pinned by TestRun_FCDetectFailureIsWarning), we leave cfg.KernelKey
	// empty and let the rest of startup proceed — every snapshot will
	// be marked stale and every wake will cold-boot, which is the
	// correct cold-boot-always-works behaviour (ADR-005).
	if cfg.KernelKey == "" && fcVersion != "" {
		cfg.KernelKey = sched.KernelKey(fcVersion)
	}

	// Host-key lifecycle (ADR-020 / spec §11 G2). Without this, the
	// Manager refuses to wake any app that PUT a secret (Manager.Wake
	// returns ErrNoHostKey). vmmd is the only writer to the on-disk
	// key — apid reads the public recipient to seal, builderd reads
	// it to seal build-time env, and the wake path inside vmmd unseals
	// with the private identity. The first-boot branch generates a
	// fresh X25519 identity; the restart branch loads the existing
	// one and re-emits the public recipient file (idempotent).
	hostID, keyPath, pubPath, err := loadOrGenerateHostIdentity(deps,
		envOr("FAAS_HOST_KEY_PATH", secretbox.DefaultHostKeyPath),
		envOr("FAAS_HOST_AGE_RECIPIENT_PATH", secretbox.DefaultHostAgeRecipientPath),
	)
	if err != nil {
		return err
	}

	// Issue #98 / ADR-028: vmmd self-registers in compute_nodes
	// before the gRPC listener binds. Fail-closed: if the upsert
	// fails (Postgres down, schema drift), vmmd exits rather than
	// serving traffic with no identity. The legacy default-local
	// path (NodeName empty) skips the DB entirely — no migration
	// is required on a fresh single-box dev install beyond what
	// already exists.
	var nodeID string
	if cfg.ComputeNode.NodeName != "" {
		dbURL := cfg.DBURL
		if dbURL == "" {
			dbURL = envOr("FAAS_VMMD_DBURL", "")
		}
		if dbURL == "" {
			return errors.New("vmmd: [compute_node].name set but [db_url] (or FAAS_VMMD_DBURL) is empty")
		}
		pool, err := deps.openDB(ctx, dbURL)
		if err != nil {
			return fmt.Errorf("vmmd: open db for self-registration: %w", err)
		}
		defer pool.Close()
		store := deps.openStore(pool)
		cn, err := registerComputeNode(ctx, store, cfg.ComputeNode, listenTarget, deps.detectOverlayIP, log)
		if err != nil {
			return err
		}
		nodeID = cn.ID
	}

	cbm := fcvm.NewColdBootMetrics()
	// #96 / ADR-025 axis 2: vmmd publishes the mem blob via the configured
	// StorageBackend after a successful Snapshot, and resolves it back
	// from the key on Restore. The env-driven fork (FAAS_STORAGE_BACKEND)
	// routes the same call sites through a remote OCI distribution-spec
	// backend when the operator sets one up.
	storageBackend, err := storage.BackendFromEnv()
	if err != nil {
		return fmt.Errorf("vmmd: %w", err)
	}
	if envOr("FAAS_STORAGE_BACKEND", "local") == "oci" {
		log.Info("vmmd: storage backend = oci", "registry", envOr("FAAS_OCI_REGISTRY", ""))
	} else {
		log.Info("vmmd: storage backend = local", "fc_root", envOr("FAAS_STORAGE_ROOT", "/srv/fc"))
	}
	mgr := fcvm.NewManager(
		wire.ExecRunner{},
		fcvm.NewJailerVMM(fcvm.JailChrootBase, 30*time.Second).WithStorage(storageBackend),
		fcvm.Paths{Kernel: cfg.KernelKey},
		fcVersion,
		log,
		cbm,
	)
	mgr.SetHostIdentity(hostID)
	// issue #299: wire the artifact backend the Manager uses to
	// read Grype scan sidecars at boot time. Mirrors the VMM's
	// own WithStorage wiring at line 223 above; the VMM uses
	// storage to materialize snapshot blobs while the Manager
	// uses it to fetch the per-runtime scan sidecar. Both share
	// the same backend (the production PrefixRouter rooted at
	// /srv/fc).
	mgr.WithStorage(storageBackend)

	// Ops + listener. Resolve the listen target (issue #95): unix://
	// default, tcp/dns optional; tcp targets require a complete mTLS
	// cluster and the loader rejects partial configs.
	//
	// Hoisted above NewAdvisoryClient (Mega-PR B): the advisory
	// client increments vmmd_stateless_advisory_batches_emitted_total
	// on every Forward outcome, so OpsMetrics must exist before the
	// client constructor captures it. Same single-registry pattern
	// (memory wire-opsmetrics-single-registry) as every other
	// vmmd-side metric.
	ops := wire.NewOpsMetrics("vmmd")
	// issue #299: wire the OpsMetrics the Manager's scan check
	// feeds per-severity finding counts into (vmmd_trivy_image_vulns_total{image, severity}).
	// The counter is pre-instantiated at boot on every daemon's
	// single-registry OpsMetrics (memory note wire/OpsMetrics),
	// so this call is the vmmd-side producer wiring only — no new
	// registration, no new listener.
	mgr.SetImageScanMetrics(ops)

	// Wave 0 PR-C / ADR-047: vmmd becomes a gRPC client for the
	// first time. The AdvisoryClient dials /run/faas/apid.sock to
	// forward guest-init fanotify batches. Empty FAAS_APID_ADVISORY_SOCK
	// disables (matches apid's explicit-empty pattern); nil client
	// short-circuits Manager.ForwardStatelessAdvisory to a no-op.
	//
	// Mega-PR B: pass `ops` so the AdvisoryClient can increment
	// stateless_advisory_batches_emitted_total{result} on every
	// Forward outcome. The accessor is nil-receiver safe, so a
	// nil ops is also a clean no-op (kept for symmetry / unit
	// tests that don't wire metrics).
	advisoryTarget := envOr("FAAS_APID_ADVISORY_SOCK", "unix:///run/faas/apid.sock")
	var advisoryCli *vmmdgrpc.AdvisoryClient
	if advisoryTarget != "" {
		advisoryCli = vmmdgrpc.NewAdvisoryClient(advisoryTarget, log, ops)
		mgr.SetAdvisoryClient(advisoryCli)
		log.Info("vmmd: stateless advisory client wired", "target", advisoryTarget)
	}
	log.Info("vmmd ready", "fc_version", fcVersion, "max_slots", fcvm.MaxSlots,
		"uid_lo", fcvm.JailUIDBase, "uid_hi", fcvm.JailUIDMax,
		"host_key_path", keyPath, "recipient_path", pubPath,
		"recipient", hostID.Recipient().String())
	serverTLS, err := cfg.LoadServerTLS()
	if err != nil {
		return fmt.Errorf("vmmd: load server TLS: %w", err)
	}
	scheddClientTLS, err := cfg.LoadScheddClientTLS()
	if err != nil {
		return fmt.Errorf("vmmd: load schedd client TLS: %w", err)
	}
	deps.scheddClientTLS = scheddClientTLS
	lis, err := deps.listen(ctx, listenTarget, serverTLS, cfg.OwnerUser)
	if err != nil {
		return fmt.Errorf("vmmd: listen %s: %w", listenTarget, err)
	}
	// CPU cache: a per-instance rate + accumulator over cgroup
	// usage_usec, fed by runCPUSampleLoop below and consumed by
	// vmmdgrpc.Server.Stats. issue #279 / PR-B. nil-safe so
	// tests that don't care about CPU can pass a fresh
	// cpustats.NewWithDefaults() and skip the sample loop
	// entirely via runCPUSampleInterval=0.
	cpuCache := cpustats.NewWithDefaults()
	// Netstats cache: per-instance byte-counter over root-side
	// vethHost.rx_bytes, fed by runNetworkEgressPoll below and
	// consumed by vmmdgrpc.Server.Stats as the net_tx_bytes
	// wire field. ADR-046 (step 7). nil-safe so tests can pass
	// nil to vmmdgrpc.NewWithCPUAndNet and skip the sample
	// loop entirely.
	netCache := netstats.NewWithDefaults()
	gsrv := grpc.NewServer(wire.ServerCredsOrEmpty(serverTLS)...)
	impl := vmmdgrpc.NewWithCPUAndNet(mgr, ops, fcVersion, log, cpuCache, netCache)
	impl.Register(gsrv)

	// Optional /metrics endpoint.
	var httpSrv *http.Server
	if cfg.MetricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle(metricsPath, ops.Handler())
		// Cold-boot fallback counter has its own registry (one writer,
		// one reader). Mount at /metrics/fallback so a scrape that only
		// wants the ops series stays clean.
		mux.Handle(metricsPath+"/fallback", cbm.Handler())
		httpSrv = &http.Server{
			Addr:              cfg.MetricsAddr,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second, // match schedd; guards the metrics endpoint against Slowloris
		}
		go func() {
			if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("metrics http", "err", err)
			}
		}()
		log.Info("metrics listening", "addr", cfg.MetricsAddr)
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("grpc listening", "addr", listenTarget, "service", vmmdpb.Vmmd_ServiceDesc.ServiceName)
		serveErr <- gsrv.Serve(lis)
	}()

	// PR-E egress-deny counter poll adapter. Reads `nft list counters`
	// every EgressPollInterval (15 s by default) and emits the per-CIDR
	// delta as <daemon>_egress_deny_total{cidr,family}. Tests inject
	// startEgressPoll to skip the loop or capture the seam args; nil
	// means "start the production goroutine". The interval is
	// parameterised so a unit test can drive the loop at sub-second
	// cadence (see cmd/vmmd/poller_test.go::TestRunEgressPoll_DeltaOnSecondTick).
	interval := EgressPollInterval
	if deps.egressPollInterval != nil {
		interval = *deps.egressPollInterval
	}
	pop := deps.popCounters
	if pop == nil {
		pop = netns.PopCounters
	}
	if deps.startEgressPoll != nil {
		deps.startEgressPoll(ctx, ops, pop, interval, log)
	} else {
		go runEgressPoll(ctx, ops, pop, interval, log)
	}

	// CPU sample loop (issue #279 / PR-B): drives the cpustats
	// cache at 250 ms cadence — half the schedd poller's
	// 200 ms so a fresh rate is always ready when schedd dials
	// Stats. 250 ms matches the spike-capture window the
	// cgroupstats metal test was written against
	// (pkg/sched/instancestats/poller_metal_test.go:153). On
	// non-Linux hosts cgroupstats.Sample returns ok=false; the
	// loop is a no-op there, leaving cpuCache cold.
	go runCPUSampleLoop(ctx, cpuCache, log)
	// Network egress poll loop (ADR-046, step 7): reads
	// /sys/class/net/<vethHost>/statistics/rx_bytes for every
	// live instance on a 250 ms tick and feeds netstats.Cache.
	// The schedd poller pulls the value via Stats at its own
	// 200 ms cadence; meterd's sampler appends to
	// usage_minutes.net_tx_bytes additively per minute.
	go runNetworkEgressPoll(ctx, mgr, netCache, ops, nil, nil, nil, 0, log)

	// ADR-025 axis 5: vmmd publishes live capacity (live_count,
	// leased_count, used_mb, ram_headroom_mb, vcpu_busy) to
	// schedd on a 1 s cadence. The publisher only runs on the
	// multi-node path (NodeName set, nodeID non-empty). The
	// single-box default-local vmmd skips the loop entirely,
	// preserving backward compatibility (ADR-005 cold-boot).
	//
	// residentFn is the leakcheck seam — leakcheck.ResidentBytes
	// on Linux, nil on non-Linux dev boxes (the chooser then
	// falls back to the store sum).
	if nodeID != "" && deps.scheddTarget != "" {
		interval := CapacityInterval
		if deps.capacityInterval != nil {
			interval = *deps.capacityInterval
		}
		resident := deps.residentFn
		if resident == nil {
			resident = leakcheckResidentBytes
		}
		// Slice-3 (ADR-053): load the node signing key from
		// disk. The key file is at /etc/faas/secrets/vmmd/node.key
		// (mode 0400, mirror of the mTLS leaf's posture). When
		// the file is missing (single-box / pre-slice-3 dev
		// default), we fall through to nil + "" and the
		// publisher emits unsigned reports — additive, so
		// pre-slice-3 schedd still accepts. The key_id is the
		// SHA-256 hex of the leaf's SPKI; we compute it once
		// at startup so the hot path stays cheap.
		nodeKey, nodeKeyID, keyErr := loadNodeSigningKey()
		if keyErr != nil {
			return fmt.Errorf("vmmd: load node signing key: %w", keyErr)
		}
		if nodeKey != nil {
			log.Info("vmmd: capacity reports will be signed", "key_id", nodeKeyID)
		} else {
			log.Info("vmmd: capacity reports unsigned (no node.key); pre-slice-3 mode")
		}
		if deps.startCapacityPublish != nil {
			deps.startCapacityPublish(ctx, mgr, nodeID, cfg.ComputeNode, deps.scheddTarget, deps.scheddClientTLS, interval, resident, nodeKey, nodeKeyID, log)
		} else {
			go runCapacityPublish(ctx, mgr, nodeID, cfg.ComputeNode, deps.scheddTarget, deps.scheddClientTLS, interval, resident, nodeKey, nodeKeyID, log)
		}
		log.Info("vmmd: capacity publisher wired", "node_id", nodeID, "target", deps.scheddTarget, "interval", interval.String())
	}

	// Heartbeat retains the §6.2 leak signal (live + leased must be 0 when idle).
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
heartbeat:
	for {
		select {
		case <-ctx.Done():
			log.Info("draining", "live", mgr.LiveCount())
			break heartbeat
		case <-tick.C:
			log.Debug("heartbeat", "live", mgr.LiveCount(), "leased", mgr.LeasedCount())
		case err := <-serveErr:
			if err != nil {
				return err
			}
		}
	}

	// Graceful shutdown — 5s deadline; M2 schedd may be holding a Connect
	// we don't want to drop before its replacement lease is acquired.
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gsrv.GracefulStop()
	if httpSrv != nil {
		//nolint:contextcheck // shutdown context must outlive caller ctx (which is already Done); detached from caller per gRPC + net/http contract.
		_ = httpSrv.Shutdown(stopCtx)
	}
	_ = lis.Close()
	// Advisory gRPC client holds the dial to /run/faas/apid.sock
	// open for ~30s of keepalive if we don't close it explicitly.
	// Idempotent at the gRPC layer (pkg/vmmdgrpc uses sync.Once).
	if advisoryCli != nil {
		_ = advisoryCli.Close()
	}
	return nil
}

// loadOrGenerateHostIdentity implements the G2 host-key lifecycle:
//
//  1. Try LoadHostKey(path).
//  2. On ErrHostKeyNotFound (first boot) → GenerateAndSaveHostKey(path).
//  3. Always WriteRecipientFile(pubPath, id) so apid / builderd have
//     a fresh public recipient to seal against on every startup.
//
// Returns the identity plus the resolved paths so the caller can log
// them. Extracted so tests can drive the lifecycle without booting
// the full gRPC + listener stack.
func loadOrGenerateHostIdentity(deps runDeps, keyPath, pubPath string) (*age.X25519Identity, string, string, error) {
	id, err := deps.loadHostKey(keyPath)
	if errors.Is(err, secretbox.ErrHostKeyNotFound) {
		id, err = deps.genAndSaveKey(keyPath)
	}
	if err != nil {
		return nil, keyPath, pubPath, fmt.Errorf("vmmd: host key (%s): %w", keyPath, err)
	}
	if err := deps.writeRecipient(pubPath, id); err != nil {
		return nil, keyPath, pubPath, fmt.Errorf("vmmd: write recipient (%s): %w", pubPath, err)
	}
	return id, keyPath, pubPath, nil
}
