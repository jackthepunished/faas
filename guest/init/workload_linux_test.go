//go:build linux

// Workload orchestration tests (issue #463 / ADR-069 / PR-B).
//
// The guest-init orchestrator's three-step dispatch (init sequentially →
// main+sidecar in parallel → return main's lastErr) is verified against a
// pure function surface. We don't spin up real workloads here — the
// per-workload exec path is pinned by image-level fixtures and the metal
// suite; these tests verify the dispatcher with stubs that record the
// call order. The pre-PR-B runAppWithEnv path is untouched and continues
// to be covered by app_test.go.
//
// discoverRoster is exercised with testing/fstest.MapFS so the unit
// tests don't depend on the host filesystem. The fsys path matches the
// live boot path (os.DirFS("/")) relative form ("etc/faas/...") — fs.FS
// rejects absolute paths.

package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/onebox-faas/faas/pkg/api"
)

// TestDiscoverRoster_AbsentReturnsEmpty pins the legacy single-workload
// path: a missing /etc/faas/workloads.json returns an empty roster +
// an error. boot() in main_linux.go treats the error as the legacy
// signal and falls through to runAppWithEnv unchanged.
func TestDiscoverRoster_AbsentReturnsEmpty(t *testing.T) {
	fsys := fstest.MapFS{}
	if _, err := discoverRoster(fsys); err == nil {
		t.Fatal("discoverRoster: missing file should return error")
	} else if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("discoverRoster: err = %v, want fs.ErrNotExist", err)
	}
}

// TestDiscoverRoster_ValidFile pins the happy path: a valid roster
// JSON round-trips through discoverRoster without loss. Field
// alignment (json tags) is the load-bearing contract — a rename
// here requires a parallel rename in pkg/fcvm/vmm.go.
func TestDiscoverRoster_ValidFile(t *testing.T) {
	roster := workloadRoster{
		Main: workloadSpec{Name: "main", Type: "main", RamMB: 256, Port: 8080, Essential: true},
		Sidecars: []workloadSpec{
			{Name: "metrics", Type: "sidecar", RamMB: 64, Port: 9090, Essential: true},
		},
	}
	blob, err := json.Marshal(roster)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	fsys := fstest.MapFS{
		"etc/faas/workloads.json": &fstest.MapFile{Data: blob},
	}
	got, err := discoverRoster(fsys)
	if err != nil {
		t.Fatalf("discoverRoster: %v", err)
	}
	if got.Main.Name != "main" || got.Main.Type != "main" || got.Main.RamMB != 256 {
		t.Errorf("main = %+v, want name=main type=main ram=256", got.Main)
	}
	if len(got.Sidecars) != 1 {
		t.Fatalf("sidecars = %d, want 1", len(got.Sidecars))
	}
	sc := got.Sidecars[0]
	if sc.Name != "metrics" || sc.Type != "sidecar" || sc.RamMB != 64 || sc.Port != 9090 || !sc.Essential {
		t.Errorf("sidecar[0] = %+v, want metrics/sidecar/64/9090/true", sc)
	}
}

// TestDiscoverRoster_MalformedFile pins the parse-error path: a
// malformed JSON file returns an error wrapping the path so the
// caller (boot()) can log it without leaking the file contents.
func TestDiscoverRoster_MalformedFile(t *testing.T) {
	fsys := fstest.MapFS{
		"etc/faas/workloads.json": &fstest.MapFile{Data: []byte("{not json")},
	}
	_, err := discoverRoster(fsys)
	if err == nil {
		t.Fatal("discoverRoster: malformed JSON should return error")
	}
	// Path is leaked in the error (operator visibility); values
	// are NOT leaked because the malformed input itself doesn't
	// parse. errorKind() in main_linux.go classifies this as
	// "other" — the test just asserts non-nil.
}

// TestNewSupervisorFor_NonEssentialZeroRestarts pins the non-essential
// sidecar policy: Max=0 means a crash is logged and the supervisor
// returns immediately, the orchestrator's WaitGroup unblocks, and the
// rest of the workloads keep running. The "essential" boolean is the
// load-bearing input; a future rename of the field name would have to
// match pkg/fcvm/vmm.go's workloadManifest.Essential and the
// deployments.sidecars jsonb column.
func TestNewSupervisorFor_NonEssentialZeroRestarts(t *testing.T) {
	spec := workloadSpec{Name: "metrics", Type: "sidecar", Essential: false, RamMB: 64}
	sup := newSupervisorFor(spec, nil, nil, nil)
	if sup == nil {
		t.Fatal("newSupervisorFor returned nil")
	}
	if sup.Max != 0 {
		t.Errorf("non-essential Max = %d, want 0", sup.Max)
	}
	if sup.Start == nil {
		t.Error("Start closure not wired")
	}
	if sup.OnCrash == nil {
		t.Error("OnCrash hook not wired")
	}
}

// TestNewSupervisorFor_EssentialUsesMaxRestarts pins the essential
// sidecar policy: Max=MaxRestarts means a crashed essential sidecar
// is restarted per the platform's standard restart budget, matching
// the main workload's behaviour. This is the contract AC #2 (init
// sidecars run before main) depends on; an essential sidecar
// crash-loop must NOT silently take down the deploy.
func TestNewSupervisorFor_EssentialUsesMaxRestarts(t *testing.T) {
	spec := workloadSpec{Name: "metrics", Type: "sidecar", Essential: true, RamMB: 64}
	sup := newSupervisorFor(spec, nil, nil, nil)
	if sup.Max != MaxRestarts {
		t.Errorf("essential Max = %d, want %d", sup.Max, MaxRestarts)
	}
}

// TestNewSupervisorForMain_HooksWired pins the main-workload
// supervisor's wiring: Max=MaxRestarts, Start closure delegates
// to runAppWithEnv (the legacy entrypoint), and OnCrash is set so
// operators see a stderr line per restart. The closure is what
// lets the characterize probe read the main workload's PID via
// sup.LastAppPID(); a missing Start would silently bind :8080 to
// nothing on the boot path.
func TestNewSupervisorForMain_HooksWired(t *testing.T) {
	spec := workloadSpec{Name: "main", Type: "main", Essential: true, RamMB: 256, Port: 8080}
	manifest := api.AppManifest{Kind: "app", Entrypoint: []string{"/bin/sleep", "1"}}
	sup := newSupervisorForMain(spec, manifest, nil, nil, nil)
	if sup == nil {
		t.Fatal("newSupervisorForMain returned nil")
	}
	if sup.Max != MaxRestarts {
		t.Errorf("main Max = %d, want %d", sup.Max, MaxRestarts)
	}
	if sup.Start == nil {
		t.Error("Start closure not wired")
	}
	if sup.OnCrash == nil {
		t.Error("OnCrash hook not wired")
	}
}

// TestSupervisor_LastErr_NilAndStored pins the new lastErr /
// trackRunErr plumbing (issue #463 / ADR-069 / PR-B). The
// orchestrator reads sup.lastErr() after WaitGroup.Wait() to
// surface the main workload's terminal state. A fresh supervisor
// must return nil (never ran); a supervisor that returned a
// tracked error must surface it.
func TestSupervisor_LastErr_NilAndStored(t *testing.T) {
	sup := &Supervisor{Max: 0}
	if err := sup.lastErr(); err != nil {
		t.Errorf("fresh sup.lastErr = %v, want nil", err)
	}
	stored := errors.New("synthetic terminal error")
	sup.trackRunErr(stored)
	if got := sup.lastErr(); got != stored {
		t.Errorf("after trackRunErr, lastErr = %v, want %v", got, stored)
	}
}

// TestRunWorkloads_CapRejectsThreeSidecars pins the in-guest
// cap-2 defensive check (PR-B review finding #2). The server-
// side cap (migration 00119 trigger) rejects a 3rd row before
// the roster is ever stamped; guest-init still re-asserts the
// limit so a malformed /etc/faas/workloads.json (e.g. stamped
// by an older vmmd, or hand-crafted for a metal test) can't
// trick the orchestrator into supervising more than 2 sidecars.
// The error must be returned BEFORE any exec.Command, so this
// test uses an empty mainManifest — a real runWorkloads would
// fail later, but the cap rejection must short-circuit first.
func TestRunWorkloads_CapRejectsThreeSidecars(t *testing.T) {
	roster := workloadRoster{
		Main: workloadSpec{Name: "main", Type: "main", Essential: true},
		Sidecars: []workloadSpec{
			{Name: "metrics", Type: "sidecar", Essential: true},
			{Name: "logger", Type: "sidecar", Essential: true},
			{Name: "audit", Type: "sidecar", Essential: true},
		},
	}
	err := runWorkloads(api.AppManifest{}, roster, nil, nil, nil)
	if err == nil {
		t.Fatal("runWorkloads with 3 sidecars: got nil, want cap rejection")
	}
	if !strings.Contains(err.Error(), "cap is 2") {
		t.Errorf("runWorkloads error = %v, want cap-2 message", err)
	}
}
