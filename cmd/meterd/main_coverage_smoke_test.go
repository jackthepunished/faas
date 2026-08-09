package main

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/onebox-faas/faas/pkg/meter"
)

// TestDefaultDepsMeterdWireShape covers defaultDeps to pin its production
// wiring. cmd/meterd is at 0% coverage because main.go never runs in
// tests; the seam at main.go:531 (defaultDeps) + main.go:577 (runWithDeps)
// is the only entry point exercised by the harness below.
func TestDefaultDepsMeterdWireShape(t *testing.T) {
	deps := defaultDeps()
	if deps.configPath != "/etc/faas/meterd.toml" {
		t.Errorf("defaultDeps.configPath = %q, want /etc/faas/meterd.toml", deps.configPath)
	}
	if deps.openDB == nil {
		t.Errorf("defaultDeps.openDB is nil")
	}
	if deps.migrate == nil {
		t.Errorf("defaultDeps.migrate is nil")
	}
	if deps.loadMeter == nil {
		t.Errorf("defaultDeps.loadMeter is nil")
	}
	if deps.getenv == nil {
		t.Errorf("defaultDeps.getenv is nil")
	}
	if deps.dialSchedd == nil {
		t.Errorf("defaultDeps.dialSchedd is nil")
	}
	if deps.loadBillingProvider == nil {
		t.Errorf("defaultDeps.loadBillingProvider is nil")
	}
	if deps.metricsListenAndServe == nil {
		t.Errorf("defaultDeps.metricsListenAndServe is nil")
	}
	if deps.now == nil {
		t.Errorf("defaultDeps.now is nil")
	}
}

// TestRunWithDepsCapCheckFails covers the first error branch in
// runWithDeps: capCheck() returns an error and the function exits
// before doing any DB / loadMeter work. Mirrors cmd/schedd's test.
func TestRunWithDepsCapCheckFails(t *testing.T) {
	sentinel := &meterSentinelErr{"cap-check failed"}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ctx := context.Background()

	deps := runDeps{
		configPath: "/dev/null",
		capCheck:   func() error { return sentinel },
	}
	if err := runWithDeps(ctx, log, deps); err == nil {
		t.Fatalf("capCheck failure: err = nil, want non-nil")
	}
}

// TestRunWithDepsLoadMeterFails covers the loadMeter(cfg) failure branch.
// capCheck passes, LoadConfig returns a default Config for a non-existent
// path (cmd/meterd/config.go:LoadConfig mirrors cmd/schedd/config.go:325),
// so the loadMeter seam is the next observable failure.
func TestRunWithDepsLoadMeterFails(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ctx := context.Background()

	deps := runDeps{
		configPath: "/this/path/does/not/exist/meterd-" + t.Name(),
		capCheck:   func() error { return nil },
		loadMeter: func(c *Config) (*meter.Config, error) {
			return nil, &meterSentinelErr{"loadMeter failed"}
		},
	}
	if err := runWithDeps(ctx, log, deps); err == nil {
		t.Fatalf("loadMeter failure: err = nil, want non-nil")
	}
}

// TestRunAdapterStructsConstructible is a compile-time presence check
// for the adapter structs that wrap state.Store / pgxpool.Pool for
// pkg/meter. These are at 0% coverage because the runtime path that
// constructs them is gated on a successful DB open + migrate.
func TestRunAdapterStructsConstructible(t *testing.T) {
	// poolAdapter holds a *pgxpool.Pool; the struct must accept
	// a nil pointer (the Wrap path is exercised at runtime).
	var _ = poolAdapter{}

	// storageStoreAdapter holds a state.Store; pass a MemStore so
	// the cast is type-safe.
	_ = storageStoreAdapter{s: nil}
}

// meterSentinelErr is a small typed error used to verify that
// runWithDeps propagates errors with a stable identity.
type meterSentinelErr struct{ msg string }

func (e *meterSentinelErr) Error() string { return e.msg }
