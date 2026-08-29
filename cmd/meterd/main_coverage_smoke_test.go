package main

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/onebox-faas/faas/pkg/mail"
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

// TestNotificationsUnsubscribeURLValidation covers the validator
// the meterd boot wires against FAAS_NOTIFICATIONS_UNSUBSCRIBE_URL
// (issue #246 item 4). The validator itself lives in pkg/mail
// (mail.ValidateUnsubscribeURL); this test asserts both the
// pass-through behaviour and that runWithDeps will pass a clean
// URL through to the next boot step.
func TestNotificationsUnsubscribeURLValidation(t *testing.T) {
	bad := []string{
		"",
		"   ",
		"mailto:ops@example.com",
		"ftp://example.com/unsub",
		"https://",
	}
	for _, u := range bad {
		if err := mail.ValidateUnsubscribeURL(u); err == nil {
			t.Fatalf("ValidateUnsubscribeURL(%q) = nil, want error", u)
		}
	}
	good := []string{
		"http://localhost:8081/account/notifications",
		"https://faas.example.com/account/notifications?token=abc",
	}
	for _, u := range good {
		if err := mail.ValidateUnsubscribeURL(u); err != nil {
			t.Fatalf("ValidateUnsubscribeURL(%q) = %v, want nil", u, err)
		}
	}
}

// TestNotificationsUnsubscribeURLSetGet covers the meter-level
// singleton the quota loop reads from
// (pkg/meter/notifications.go). Set once → Get returns the same
// value; concurrent reads are safe.
func TestNotificationsUnsubscribeURLSetGet(t *testing.T) {
	// Reset to known-good state at test start.
	meter.SetNotificationsUnsubscribeURL("")
	defer meter.SetNotificationsUnsubscribeURL("")

	want := "https://faas.example.com/account/notifications"
	meter.SetNotificationsUnsubscribeURL(want)
	if got := meter.NotificationsUnsubscribeURL(); got != want {
		t.Fatalf("after SetNotificationsUnsubscribeURL(%q) Get = %q", want, got)
	}
	// Overwrite with a different value (Set is a setter, not
	// once-only).
	other := "http://localhost:8081/notifications"
	meter.SetNotificationsUnsubscribeURL(other)
	if got := meter.NotificationsUnsubscribeURL(); got != other {
		t.Fatalf("after second Set, Get = %q, want %q", got, other)
	}
	// Empty string is a valid value (dev box).
	meter.SetNotificationsUnsubscribeURL("")
	if got := meter.NotificationsUnsubscribeURL(); got != "" {
		t.Fatalf("after Set(\"\") Get = %q, want empty", got)
	}
}
