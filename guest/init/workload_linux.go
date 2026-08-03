//go:build linux

// Workload orchestration (issue #463 / ADR-069 / PR-B).
//
// Each deployment boots zero or more workloads under guest-init:
// one main workload (the customer app) plus 0..N sidecars declared
// in the deployment spec. The boot flow is:
//
//   1. Discover workloads. vmmd stamps /etc/faas/workloads.json on
//      drive1 (the main workload's drive) at wake time. The file
//      is the deployment-level roster: {Main, Sidecars[]} where
//      Main is the main workload's spec and Sidecars is the
//      per-sidecar array (or empty when there are no sidecars).
//
//   2. Run init sidecars sequentially. type=="init" workloads run
//      before main; non-zero exit fails the deploy with
//      failure_class: user_error (AC #1). The supervisor's Max=0
//      policy (no restarts for init — a failed init is a hard fail)
//      is enforced by newSupervisorFor's Max=0 branch.
//
//   3. Run main + long-running sidecars in parallel. type=="main"
//      and type=="sidecar" workloads run concurrently, each under
//      its own Supervisor. A non-essential sidecar crash does NOT
//      fail the deploy; an essential sidecar or main workload crash
//      restarts per the supervisor's Max policy (MaxRestarts).
//
//   4. Characterize the main workload only. The bind-detection
//      probe (characterize_linux.go) reads AppPID() from the MAIN
//      supervisor's *exec.Cmd — a sidecar's TCP listener would
//      mis-classify the boot class (e.g. an init sidecar that
//      binds :8080 would be observed as the main app's listener).
//
// Per-workload secrets/env:
//   - The MAIN workload reads /etc/faas/secrets.env and
//     /etc/faas/env.json from drive1 (the legacy paths). vmmd
//     writes these at wake time via StageSecretsEnv / StageAPIEnv.
//   - Sidecars don't read secrets.env / env.json — the customer's
//     per-sidecar env is baked into the sidecar's ext4 at build
//     time by imaged (PR-A's contract). The wake-time env wire
//     stays flat so the vmmd proto surface is unchanged.
//
// Per-workload cgroups (host side):
//   - vmmd creates nested cgroup scopes under the per-instance
//     scope (writeWorkloadCgroup). These are host-side
//     defense-in-depth scopes; the in-guest cgroup partition is
//     a separate concern and is intentionally NOT wired here
//     because the guest's cgroup namespace is isolated from the
//     host's cgroup hierarchy.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
)

// workloadSpec mirrors the on-disk shape of /etc/faas/workloads.json
// (issue #463 / ADR-069 / PR-B). Must stay in lockstep with
// pkg/fcvm/vmm.go::workloadManifest — vmmd writes the file and
// guest-init reads it; a field rename here requires a parallel
// rename in pkg/fcvm/vmm.go (and the proto wire if either end
// reshapes it).
//
// The build tag here is "linux" because guest-init only runs on
// Linux (the in-guest PID 1 of every microVM). The vmmd
// counterpart compiles on every platform but emits identical
// JSON because the field tags match exactly.
type workloadSpec struct {
	Name      string `json:"name"`
	Type      string `json:"type"` // "main" | "init" | "sidecar"
	RamMB     int    `json:"ram_mb"`
	Port      int    `json:"port"`
	Essential bool   `json:"essential"`
}

// workloadRosterPath is the deployment-level roster location
// (issue #463 / ADR-069 / PR-B). vmmd writes this file once on
// drive1 at wake time; guest-init reads it after assembleOverlay+
// pivot_root to discover the main workload's spec + the per-
// sidecar array. The same path on every workload's drive
// (because they share overlayfs) means a single read is
// sufficient; the merged-root sees drive1's copy.
//
// WorkloadSpecPath (single-workload envelope at /etc/faas/
// workload.json) is the per-drive stamp vmmd also writes for
// reverse-compat and operator visibility (debugging tools can
// `cat` it inside the VM). The orchestrator reads the roster,
// not the per-drive stamp.
const workloadRosterPath = "/etc/faas/workloads.json"

// workloadRoster mirrors the deployment-level roster shape.
// Main is the canonical main-workload spec; Sidecars is the
// per-sidecar array (nil/empty = legacy single-workload path).
type workloadRoster struct {
	Main     workloadSpec   `json:"main"`
	Sidecars []workloadSpec `json:"sidecars"`
}

// discoverRoster reads the workload roster from the merged
// root. Returns the parsed roster or an error. A missing file
// is the legacy single-workload path — boot() in main_linux.go
// routes to runAppWithEnv unchanged.
//
// The fs.FS parameter lets the unit test drive discoverRoster
// with testing/fstest.MapFS instead of touching the real root.
// On the live boot path, callers pass os.DirFS("/").
func discoverRoster(fsys fs.FS) (workloadRoster, error) {
	var zero workloadRoster
	data, err := fs.ReadFile(fsys, strings.TrimPrefix(workloadRosterPath, "/"))
	if err != nil {
		return zero, err // caller treats absent as legacy path
	}
	var roster workloadRoster
	if err := json.Unmarshal(data, &roster); err != nil {
		return zero, fmt.Errorf("workload roster: parse %q: %w", workloadRosterPath, err)
	}
	return roster, nil
}

// runWorkloads (issue #463 / ADR-069 / PR-B) is the boot-side
// orchestrator. The dispatch order:
//
//  1. Run init sidecars sequentially (each blocking). A non-zero
//     init exit fails the deploy immediately — no main workload
//     starts. (AC #1.)
//  2. Run main + type="sidecar" workloads in parallel, each under
//     its own Supervisor. A main workload crash restarts per the
//     supervisor's Max policy; an essential sidecar crash has the
//     same policy; a non-essential sidecar crash is logged and
//     the other workloads continue. (AC #2 / AC #4.)
//  3. Returns when every supervisor has exited (clean or
//     exhausted its restart budget). The main workload's exit
//     code is the deploy's exit code; non-essential sidecar
//     exits are logged but ignored.
//
// The legacy single-workload path (no roster) is the caller's
// responsibility — boot() in main_linux.go owns that fallback.
// runWorkloads is called ONLY when at least one workload
// roster was discovered.
//
// mainManifest is the legacy api.AppManifest for the main
// workload (passed in from boot's earlier os.Open +
// ReadManifest). The orchestrator uses it for the main
// workload's entrypoint + env. Sidecars' entrypoints live in
// their baked ext4 images at the canonical
// /usr/local/bin/start.sh (or whatever the customer image
// provides) — guest-init exec's them verbatim.
func runWorkloads(mainManifest api.AppManifest, roster workloadRoster, secrets, apiEnv map[string]string, log *slog.Logger, sidecarProxy *sidecarEventsProxy) error {
	if log == nil {
		log = slog.Default()
	}

	// Defensive cap-2 enforcement (issue #463 / ADR-069 / PR-B
	// review finding #2). The migrations 00118 + 00119 cap the
	// per-deployment sidecar count at 2 server-side; guest-init
	// re-asserts the same limit so a malformed /etc/faas/
	// workloads.json (e.g. one stamped by an older vmmd, or a
	// hand-crafted fixture in a metal test) can't trick the
	// orchestrator into supervising more than 2 sidecars.
	// Matches SidecarCapMax in pkg/api/limits.go — if the cap
	// is ever raised, raise both constants together and update
	// the unit test guest/init/workload_linux_test.go.
	if len(roster.Sidecars) > 2 {
		return fmt.Errorf("workload roster: deployment has %d sidecars; cap is 2 (ADR-069 §Decision 1)", len(roster.Sidecars))
	}

	// Step 1: run init sidecars sequentially (AC #1).
	for i := range roster.Sidecars {
		sc := roster.Sidecars[i]
		if sc.Type != "init" {
			continue
		}
		log.Info("runWorkloads: init sidecar starting",
			"name", sc.Name, "essential", sc.Essential)
		// Issue #463 / ADR-069 / ADR-071 / PR-C §3: stamp the
		// wall-clock start so the sidecar_init_exit envelope
		// (init_ok / init_failed) carries a meaningful
		// duration_ms. The start is captured INSIDE the loop
		// (not above it) so each init sidecar's duration is
		// per-sidecar, not cumulative across the roster.
		startedAt := time.Now()
		sup := newSupervisorFor(sc, secrets, apiEnv, log, sidecarProxy)
		runErr := sup.Run()
		elapsedMs := time.Since(startedAt).Milliseconds()
		// Translate the supervisor's terminal error into the
		// status the audit needs. The supervisor's Run returns
		// nil on a clean exit; a non-nil error wraps the
		// sidecar's exit or restart-budget exhaustion (AC #1's
		// hard fail). We attempt to surface the underlying
		// exec.ExitError code for the audit so operators see
		// the real shell exit rather than the supervisor's
		// "crash-looped after N restart(s)" wrapper. A non
		//-ExitError (e.g. supervisor-internal panic-recovered)
		// falls back to -1 and gets recorded as such.
		exitCode := 0
		status := "init_ok"
		if runErr != nil {
			status = "init_failed"
			var ee *exec.ExitError
			if errors.As(runErr, &ee) {
				exitCode = ee.ExitCode()
			} else {
				exitCode = -1
			}
		}
		// Send the sidecar_init_exit envelope AFTER the
		// supervisor returns, so the audit captures the
		// terminal state. A send error is logged + ignored
		// (the supervisor's terminal state remains the
		// source of truth for "did the deploy succeed"); we
		// never silently fail a deploy because the audit
		// signal didn't make it home.
		if sendErr := sidecarProxy.SendInitExit(sc.Name, status, exitCode, elapsedMs); sendErr != nil {
			log.Warn("runWorkloads: sidecar init_exit send failed",
				"name", sc.Name, "status", status, "err", sendErr)
		}
		if runErr != nil {
			// AC #1: init non-zero exit → user_error.
			log.Error("runWorkloads: init sidecar failed",
				"name", sc.Name, "essential", sc.Essential,
				"exit_code", exitCode, "duration_ms", elapsedMs, "err", runErr)
			return fmt.Errorf("init sidecar %q failed: %w", sc.Name, runErr)
		}
		log.Info("runWorkloads: init sidecar ok",
			"name", sc.Name, "duration_ms", elapsedMs)
	}

	// Step 2: spawn main + type="sidecar" workloads in parallel.
	mainSup := newSupervisorForMain(roster.Main, mainManifest, secrets, apiEnv, log)
	supervisors := []*Supervisor{mainSup}
	for i := range roster.Sidecars {
		sc := roster.Sidecars[i]
		if sc.Type != "sidecar" {
			continue
		}
		supervisors = append(supervisors, newSupervisorFor(sc, secrets, apiEnv, log, sidecarProxy))
	}

	// ADR-051 Phase 4 (Slice A PR-B / issue #463 / ADR-069):
	// characterize the main workload only. A sidecar's TCP listener
	// would mis-classify the boot class (e.g. an init sidecar that
	// binds :8080 would be observed as the main app's listener). The
	// probe (characterize_linux.go) reads AppPID() / WaitForExit /
	// RingBufferTail from the main supervisor; sidecar supervisors
	// carry their own atomic state but the characterize probe never
	// reads them. The probe goroutine races the supervisor goroutines
	// so the bind-walk finds the customer's listener without blocking
	// the boot.
	go runCharacterizationForSup(mainSup, mainManifest)

	// Step 3: run every long-running supervisor in its own
	// goroutine and wait for all of them to exit. A
	// non-essential sidecar crash is contained by its
	// supervisor (Max=0 policy); an essential sidecar or
	// main workload crash triggers the supervisor's restart
	// policy and eventually a non-zero Run() return.
	//
	// Panic-safety (PR-B review finding #4): `defer wg.Done()`
	// is the FIRST defer so it runs even if a later recover()
	// re-panics. A bare recover turns a supervisor-panic into
	// a non-fatal log line so one bad sidecar doesn't take
	// down WaitGroup.Wait() — the rest of the workloads keep
	// running. mainSup is intentionally NOT recovered: a
	// panic in the main supervisor is the deploy's terminal
	// failure and must propagate to the orchestrator's
	// return value.
	var wg sync.WaitGroup
	wg.Add(len(supervisors))
	for i, sup := range supervisors {
		sup := sup
		isMain := i == 0 // supervisors[0] is mainSup (see Step 2)
		go func() {
			defer wg.Done()
			if isMain {
				_ = sup.Run()
				return
			}
			defer func() {
				if r := recover(); r != nil {
					log.Error("runWorkloads: sidecar supervisor panicked",
						"index", i, "recover", fmt.Sprintf("%v", r))
				}
			}()
			_ = sup.Run()
		}()
	}
	wg.Wait()

	// The main workload's exit code is the deploy's exit
	// code; non-essential sidecar exits are logged but
	// ignored. The supervisor's lastErr() surfaces the
	// terminal error from Run().
	if lastErr := mainSup.lastErr(); lastErr != nil {
		return lastErr
	}
	return nil
}

// newSupervisorForMain builds the main workload's supervisor
// (issue #463 / ADR-069 / PR-B). The customer's app spec is
// the legacy api.AppManifest; the workload spec carries the
// per-workload policy (port, ram_mb, essential). The
// supervisor's Start closure runs runAppWithEnv (the legacy
// entrypoint that exec's the manifest's entrypoint with
// the merged env).
func newSupervisorForMain(spec workloadSpec, manifest api.AppManifest, secrets, apiEnv map[string]string, log *slog.Logger) *Supervisor {
	supRef := &Supervisor{Max: MaxRestarts}
	supRef.Start = func() error { return runAppWithEnv(manifest, secrets, apiEnv, supRef) }
	supRef.OnCrash = func(attempt int, err error) {
		fmt.Fprintf(os.Stderr, "guest-init: main crashed (restart %d/%d): %v\n", attempt, MaxRestarts, err)
	}
	_ = spec // reserved for PR-C per-workload policy; ignored today
	return supRef
}

// newSupervisorFor builds a sidecar supervisor
// (issue #463 / ADR-069 / PR-B + PR-C §4). The sidecar's
// entrypoint is the customer's image default — guest-init
// exec's the sidecar's baked /usr/local/bin/start.sh (or
// whatever the image provides). The essential flag drives
// the restart policy: non-essential = Max=0 (no restart,
// log-and-continue); essential = Max=MaxRestarts (restart
// per the platform contract). PR-C §4 wires the supervisor's
// OnCrash hook to call SendRestart on the proxy so the host
// can increment schedd_sidecar_restart_total{app, sidecar}.
// A nil sidecarProxy (no-signal contract when bind fails)
// keeps the OnCrash hook log-only.
func newSupervisorFor(spec workloadSpec, secrets, apiEnv map[string]string, log *slog.Logger, sidecarProxy *sidecarEventsProxy) *Supervisor {
	maxRestarts := MaxRestarts
	if !spec.Essential {
		maxRestarts = 0 // non-essential sidecar: log crash, do not restart
	}
	supRef := &Supervisor{Max: maxRestarts}
	supRef.Start = func() error { return runSidecar(spec, secrets, apiEnv, supRef) }
	supRef.OnCrash = func(attempt int, err error) {
		fmt.Fprintf(os.Stderr, "guest-init: sidecar %s crashed (restart %d/%d): %v\n",
			spec.Name, attempt, maxRestarts, err)
		// PR-C §4: ship the sidecar_restart envelope so vmmd
		// can increment <daemon>_sidecar_restart_total AND
		// emit events.SidecarRestart. A send error is
		// best-effort (logged + ignored); the supervisor's
		// restart policy remains the source of truth for
		// "did the sidecar actually come back".
		if sidecarProxy != nil {
			if sErr := sidecarProxy.SendRestart(spec.Name, attempt); sErr != nil {
				log.Warn("sidecar restart emit failed",
					"sidecar", spec.Name, "attempt", attempt, "err", sErr)
			}
		}
	}
	return supRef
}

// runSidecar exec's a sidecar workload (issue #463 /
// ADR-069 / PR-B). The entrypoint is the customer's image
// default (no manifest drives it). Secrets/env come from
// the sidecar's baked ext4 — the wake-time env wire stays
// flat so this layer doesn't read secrets.env / env.json.
//
// spec.Name and spec.Port are reserved for the wire-stable
// log surface (PR-C extends to plumb through supervisor
// restart events). The cmd field will land in PR-C; today
// the sidecar's entrypoint is /usr/local/bin/start.sh by
// convention.
func runSidecar(spec workloadSpec, secrets, apiEnv map[string]string, sup *Supervisor) error {
	// The sidecar's cmd is the customer-image default.
	// PR-C will read this from a per-workload field; today
	// every sidecar image ships /usr/local/bin/start.sh
	// (imaged's stamp during buildSidecarLayer).
	cmd := exec.Command("/usr/local/bin/start.sh")
	cmd.Env = os.Environ()
	// Sidecar env can layer on top of the customer's baked
	// env (imaged wrote the per-sidecar env into the ext4 at
	// build time). The secrets/apiEnv args from wake are
	// shared with the main workload's legacy readers; we
	// pass them through but they only take effect when the
	// sidecar's image was built without a baked env.
	if len(secrets) > 0 || len(apiEnv) > 0 {
		cmd.Env = BuildEnvWithSecrets(cmd.Env, api.AppManifest{}, secrets, apiEnv)
	}
	// Pipe stdout/stderr into the supervisor's ring buffer
	// (Slice A PR-B contract).
	if sup != nil {
		mw := io.MultiWriter(os.Stdout, sup.LogBuffer())
		cmd.Stdout, cmd.Stderr = mw, mw
	} else {
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	}
	// ADR-051 Phase 4: expose the forked cmd to the
	// supervisor so runCharacterizationForSup can read the
	// PID via LastAppPID(). The characterize probe filters
	// by workload name, so a sidecar's PID is invisible to
	// the main workload's classify.
	if sup != nil {
		sup.TrackCommand(cmd)
	}
	// Run the sidecar. exec.Command blocks until the sidecar
	// exits; the supervisor's Run() loop captures the exit
	// code via trackExit and decides whether to restart.
	_ = spec.Port
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run sidecar %s: %w", spec.Name, err)
	}
	return nil
}
