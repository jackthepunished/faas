package sched

import (
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace/noop"
)

// TestCoverageSliceEngineSetters drives the 0%-coverage With* setters
// on Engine. The setters are pure delegation: they return the same
// engine pointer for chaining, so the test is a compile-time
// presence check + return-value assertion.
func TestCoverageSliceEngineSetters(t *testing.T) {
	e := &Engine{}

	if got := e.WithRebalanceConfig(30, 5); got == nil {
		t.Errorf("WithRebalanceConfig returned nil")
	}
	if got := e.WithMigrateLiveLeaseSeconds(45); got == nil {
		t.Errorf("WithMigrateLiveLeaseSeconds returned nil")
	}
	if got := e.WithMigratingWatchdogIntervalSeconds(2); got == nil {
		t.Errorf("WithMigratingWatchdogIntervalSeconds returned nil")
	}
	if got := e.WithTracer(noop.NewTracerProvider().Tracer("")); got == nil {
		t.Errorf("WithTracer returned nil")
	}
	if got := e.WithNodeKeyRegistry(nil); got == nil {
		t.Errorf("WithNodeKeyRegistry returned nil")
	}

	// CapacityTable + CapacitySink are getters; nil is a valid result
	// on a fresh engine (they're initialized lazily). The test just
	// exercises the getter path.
	_ = e.CapacityTable()
	_ = e.CapacitySink()

	// NodeKeyRegistry: nil is fine, the test exercises the getter path.
	if e.NodeKeyRegistry() != nil {
		t.Error("NodeKeyRegistry() != nil on fresh engine, want nil")
	}
}

// TestCoverageSliceWithClock pins the disk_drift.go WithClock setter.
func TestCoverageSliceWithClock(t *testing.T) {
	d := &DiskDrift{}
	if got := d.WithClock(func() time.Time { return time.Now() }); got == nil {
		t.Errorf("DiskDrift.WithClock returned nil")
	}
	// nil clock must not panic.
	if got := d.WithClock(nil); got == nil {
		t.Errorf("DiskDrift.WithClock(nil) returned nil")
	}
}

// TestCoverageSliceScheduleRaw pins the cron.go:52 Raw() getter.
func TestCoverageSliceScheduleRaw(t *testing.T) {
	s := &Schedule{raw: "*/5 * * * *"}
	if got := s.Raw(); got != "*/5 * * * *" {
		t.Errorf("Schedule.Raw() = %q, want %q", got, "*/5 * * * *")
	}
}
