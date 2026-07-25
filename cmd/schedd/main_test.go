// Tests for the schedd daemon entrypoint. The real VM path needs KVM; here we
// exercise run()'s orchestration through the dependency-injection seam. Paths
// that need a live pool (LISTEN, migrate, seed) use pkg/db/pgtest, which skips
// when Postgres is unreachable; the pure config/open-failure paths run anywhere.

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
	"github.com/onebox-faas/faas/pkg/sched"
	"github.com/onebox-faas/faas/pkg/sched/instancestats"
	"github.com/onebox-faas/faas/pkg/state"
	"github.com/onebox-faas/faas/pkg/wire"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// shortDir keeps unix socket paths under macOS's ~104-char sun_path limit.
func shortDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	short := "/tmp/fsst-schedd-" + t.Name()
	if err := os.Symlink(root, short); err != nil {
		return root
	}
	t.Cleanup(func() { _ = os.Remove(short) })
	return short
}

// migratedPool returns a pgtest pool with the schema migrated, or skips.
func migratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := pgtest.Open(t)
	if err := db.MigrateUp(context.Background(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

func TestRun_BadConfigPath(t *testing.T) {
	deps := runDeps{configPath: t.TempDir()} // a directory fails the non-ENOENT read
	if err := runWithDeps(context.Background(), discardLog(), deps); err == nil {
		t.Fatal("expected error from directory-as-config-path")
	}
}

func TestRun_OpenDBFailurePropagates(t *testing.T) {
	wantErr := errors.New("db down")
	deps := runDeps{
		configPath: filepath.Join(t.TempDir(), "absent.toml"), // ENOENT => defaults
		openDB:     func(context.Context, string) (*pgxpool.Pool, error) { return nil, wantErr },
	}
	if err := runWithDeps(context.Background(), discardLog(), deps); !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want wraps %v", err, wantErr)
	}
}

// TestRun_PartialVMMDFailsAtBoot validates that a config with a partial
// vmmd_tls_* cluster (only one of the three paths set) is rejected
// during startup rather than surfacing the error on the first dial.
// PR #113 made the dial lazy (router dials on first Wake), so the
// eager-boot-time "dial failure" path this test used to exercise
// no longer exists; the partial-TLS-cluster check is the equivalent
// loud-at-boot guarantee under the lazy model.
func TestRun_PartialVMMDFailsAtBoot(t *testing.T) {
	pool := migratedPool(t)
	dir := shortDir(t)
	cfgPath := filepath.Join(dir, "schedd.toml")
	// Exactly one of the three vmmd_tls_* keys is set — wire.LoadClientTLSConfigWithPrefix
	// rejects this with an error naming the missing fields.
	cfg := `socket_path = "` + filepath.Join(dir, "schedd.sock") + `"
vmmd_tls_cert_path = "/some/cert"
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := runDeps{
		configPath: cfgPath,
		openDB:     func(context.Context, string) (*pgxpool.Pool, error) { return pool, nil },
		migrate:    func(context.Context, *pgxpool.Pool) error { return nil },
		detectFC:   func(context.Context) (string, error) { return "1.10.0", nil },
		dialVMM:    stubDialVMM,
	}
	if err := runWithDeps(context.Background(), discardLog(), deps); err == nil {
		t.Fatal("expected partial vmmd_tls_* cluster to fail at boot")
	}
}

func TestRun_ListenFailurePropagates(t *testing.T) {
	pool := migratedPool(t)
	wantErr := errors.New("listen broken")
	deps := runDeps{
		configPath: filepath.Join(t.TempDir(), "absent.toml"),
		openDB:     func(context.Context, string) (*pgxpool.Pool, error) { return pool, nil },
		migrate:    func(context.Context, *pgxpool.Pool) error { return nil },
		detectFC:   func(context.Context) (string, error) { return "1.10.0", nil },
		dialVMM:    stubDialVMM,
		listen:     func(context.Context, string, *tls.Config, string) (net.Listener, error) { return nil, wantErr },
	}
	if err := runWithDeps(context.Background(), discardLog(), deps); !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want wraps %v", err, wantErr)
	}
}

// TestMain_AttachesInstanceStats pins the production wiring
// contract for the per-instance metrics poller (issue #170 / PR-A).
//
// runWithDeps is a one-shot function — it constructs the Loop,
// runs it, and tears down on cancel — so we can't observe the
// constructed Loop directly. Instead, this test mirrors the
// production wiring inline (a) by exercising the same code paths
// the production main.go uses (NewPoller, the dial adapter, and
// Loop.WithInstanceStats) and (b) by asserting the constructed
// poller has the right shape (non-nil Reader, non-nil Dialer,
// non-nil Metrics, Interval == DefaultStatsInterval). The reader
// is the canonical seam #171 / #169 will read from — its presence
// in the test verifies the production wiring would carry it
// through.
//
// The Loop's runInstanceStats helper is unexported, so we can't
// drive it from cmd/schedd (package main, not pkg/sched_test).
// The deeper integration is covered by
// pkg/sched/loop_instancestats_test.go. Here we pin the
// production-shape wiring only.
func TestMain_AttachesInstanceStats(t *testing.T) {
	log := discardLog()
	ops := wire.NewOpsMetrics("schedd")
	reader := instancestats.NewReader()
	dialer := instancestats.DialerFunc(stubDialVMM)
	poller := instancestats.NewPoller(state.NewMemStore(), dialer, nil, reader, ops, log)

	// Reader is non-nil + empty (no Replace yet).
	if reader == nil {
		t.Fatal("reader is nil after NewReader()")
	}
	if got := reader.SnapshotAll(); got != nil {
		t.Errorf("SnapshotAll on fresh Reader = %v, want nil", got)
	}

	// Poller wired correctly: non-nil, default cadence.
	if poller == nil {
		t.Fatal("poller is nil after NewPoller()")
	}
	if got := poller.TickInterval(); got != instancestats.DefaultStatsInterval {
		t.Errorf("TickInterval = %v, want %v", got, instancestats.DefaultStatsInterval)
	}

	// WithInstanceStats accepts the poller (compile-time guarantee
	// that *instancestats.Poller satisfies sched.InstanceStatsPoller;
	// this is the load-bearing part of the wiring contract).
	chain := sched.NewLoop(nil, nil, log).
		WithWatchdog(nil).
		WithRetention(nil).
		WithHeartbeat(nil).
		WithInstanceStats(poller)
	if chain == nil {
		t.Fatal("WithInstanceStats returned nil chain")
	}
}

// TestRun_DrainsOnCancel exercises the happy early-lifecycle path end to end:
// config, migrated pool, injected FC + vmm, a real unix listener + LISTEN loop,
// then cancel → clean nil return.
func TestRun_DrainsOnCancel(t *testing.T) {
	pool := migratedPool(t)
	dir := shortDir(t)
	cfgPath := filepath.Join(dir, "schedd.toml")
	cfg := "socket_path = \"" + filepath.Join(dir, "schedd.sock") + "\"\nowner_user = \"root\"\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := runDeps{
		configPath: cfgPath,
		openDB:     func(context.Context, string) (*pgxpool.Pool, error) { return pool, nil },
		migrate:    func(context.Context, *pgxpool.Pool) error { return nil },
		detectFC:   func(context.Context) (string, error) { return "1.10.0", nil },
		dialVMM:    stubDialVMM,
		listen: func(_ context.Context, target string, _ *tls.Config, _ string) (net.Listener, error) {
			t2, err := wire.ParseTarget(target)
			if err != nil {
				return nil, err
			}
			return net.Listen("unix", t2.Address)
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runWithDeps(ctx, discardLog(), deps) }()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run returned %v, want nil on clean drain", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run did not return within 3s of cancel")
	}
}

// stubDialVMM is the canonical no-op dialer for the wiring tests
// (no VM is booted, no net dial happens). Every runDeps in this
// file uses it so the runDeps literal stays one line per knob;
// production main.go passes deps.dialVMM, which is the real
// overlay.Dial.
func stubDialVMM(context.Context, string, *tls.Config) (sched.VMM, error) {
	return stubVMM{}, nil
}

// stubVMM is a no-op sched.VMM for the wiring tests (no VM is booted).
type stubVMM struct{}

func (stubVMM) CreateColdBoot(context.Context, string, sched.AppSpec) (*sched.WakeOutcome, error) {
	return &sched.WakeOutcome{}, nil
}
func (stubVMM) CreateFromSnapshot(context.Context, string, sched.AppSpec, sched.SnapshotRef) (*sched.WakeOutcome, error) {
	return &sched.WakeOutcome{}, nil
}
func (stubVMM) PauseAndSnapshot(context.Context, string, string, string, string) (sched.SnapshotBytes, error) {
	return sched.SnapshotBytes{}, nil
}
func (stubVMM) Destroy(context.Context, string) error { return nil }
func (stubVMM) Ping(context.Context) (*sched.PingOutcome, error) {
	return &sched.PingOutcome{FcVersion: "1.10.0"}, nil
}

// Stats implements sched.VMM (issue #170 / PR-A). Wiring tests
// don't observe instance metrics; return empty snapshot.
func (stubVMM) Stats(context.Context) (*sched.StatsSnapshot, error) {
	return &sched.StatsSnapshot{}, nil
}
func (stubVMM) Close() error { return nil }

// UpdateEgressAllowlist (tier-2 PR-B) — wiring tests don't drive
// the egress drift path. Returns nil so the VMM contract is
// satisfied when schedd's deps.subscribeEgressDrift is left
// nil (the production subscriber is not started in these tests).
func (stubVMM) UpdateEgressAllowlist(context.Context, string, []netip.Prefix) error {
	return nil
}
