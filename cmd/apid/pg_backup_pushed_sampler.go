// Sampler goroutine for the <prefix>_pg_backup_last_pushed_seconds
// gauge (issue #250).
//
// One instance per apid process. Stamps the gauge with the age of
// the newest tarball in /var/lib/pgsql/basebackup/, in seconds.
// The gauge is queried by the PgBackupStale alert rule
// (deploy/ansible/roles/prometheus/files/pg_backup.rules.yml); a
// value > 86400 (24h) fires the page.
//
// Why 60s:
//   The basebackup timer fires at 03:00 UTC and the push timer at
//   03:30 UTC. A 60s tick keeps the alert latency under a minute
//   without burning IO on a stat() per second. On a busy box the
//   stat is cheap (single directory, cgroup-cached); on an idle
//   box it's a no-op.
//
// Why apid (and not meterd or a separate sampler daemon):
//   The gauge is cluster-wide (one CP host, one basebackup root).
//   apid is the per-cluster OpsMetrics owner for unrelated
//   cluster-wide gauges (alertEvaluatorEnabled, topTenantRPS); the
//   pgBackupLastPushed gauge slots into the same registration site
//   (pkg/wire/metrics.go).
//
// Lifecycle:
//   Started by cmd/apid/main.go's bgBefore hook after the server
//   is constructed; runs until ctx is cancelled. Stops cleanly
//   because the only mutable state is the gauge (Prometheus
//   gauges have no per-tick bookkeeping; emitting 0 simply
//   updates the existing series).

package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/onebox-faas/faas/pkg/wire"
)

// pgBackupPushedInterval is the gauge-emission cadence (issue #250).
// 60s = 1 min, bounded so the PgBackupStale alert's `for: 5m`
// window always sees at least 5 fresh ticks.
const pgBackupPushedInterval = 60 * time.Second

// pgBackupPushedRoot is the directory the sampler stats each tick.
// Mirrors deploy/systemd/faas-pg-basebackup.service
// Environment=PG_BB_ROOT. Single source of truth would be a future
// refactor (issue #250 out-of-scope).
//
// var (not const) so the unit test can swap to a t.TempDir path;
// the production wiring never reassigns it.
//
// TODO(F9-followup): source this from a FAAS_PG_BASEBACKUP_ROOT env
// var matching the systemd unit's Environment=PG_BB_ROOT, with the
// hardcoded path as the default. Today the hardcoded path is fine
// (canonical install) but a Lima/metal test running against a
// non-canonical root would have to swap the package-level var in a
// test-only file. See review F9 + issue #250 follow-up.
var pgBackupPushedRoot = "/var/lib/pgsql/basebackup"

// pgBackupPushedSampler stamps the pg_backup_last_pushed_seconds
// gauge with the age (in seconds) of the newest tarball under
// /var/lib/pgsql/basebackup/. One instance per apid process;
// constructed once at server boot, runs as a background goroutine
// for the daemon's lifetime.
//
// Concurrency: one sampler goroutine per apid process. The sampler
// is the ONLY writer to the gauge (same precedent as
// topNSampler for apid_top_tenant_rps — see cmd/apid/topn.go).
type pgBackupPushedSampler struct {
	ops *wire.OpsMetrics
	log *slog.Logger
}

// newPgBackupPushedSampler constructs a sampler; the caller owns the
// goroutine lifecycle (start it after construction, cancel the ctx
// to stop it).
func newPgBackupPushedSampler(ops *wire.OpsMetrics, log *slog.Logger) *pgBackupPushedSampler {
	return &pgBackupPushedSampler{ops: ops, log: log}
}

// run drives the sampler loop until ctx is cancelled. Returns
// cleanly on ctx.Done(). Errors are logged but non-fatal — a
// transient stat() failure (e.g. the directory being recreated
// mid-tick) recovers on the next tick; the gauge simply stays at
// its previous value.
//
// On each tick:
//
//  1. List the pgBackupPushedRoot directory.
//  2. Find the newest entry by mtime.
//  3. Stamp the gauge with time.Since(newest).Seconds(), or 0 if
//     the directory is empty (matches the gauge's pre-instantiated
//     default so a fresh box doesn't look like a stale-push).
func (s *pgBackupPushedSampler) run(ctx context.Context) {
	if s.ops == nil {
		// Defensive: the sampler is started from bgBefore AFTER
		// srv.WithOpsMetrics, so this should never trigger. The
		// guard exists so unit tests can construct + Run without
		// first calling WithOpsMetrics.
		s.log.Warn("pgBackupPushedSampler started with nil ops; exiting")
		return
	}
	t := time.NewTicker(pgBackupPushedInterval)
	defer t.Stop()
	// Drive one tick immediately so the gauge isn't empty for the
	// first 60s after boot. Matches the pre-instantiated gauge
	// pattern from pkg/wire/metrics.go: every Gauge emits from the
	// moment the daemon boots, not only after the first observation.
	s.tick()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.tick()
		}
	}
}

// tick drives one sampler iteration. Extracted so the boot-time
// immediate emit (run) and the recurring tick share the same path.
//
// TODO(F8-followup): the unit test for tick() currently constructs a
// nil-ops sampler (returning the empty gauge) and asserts the path
// through newestEntryMtime; the real wire.PgBackupLastPushed() path
// is uncovered. Future work: add a test that constructs an OpsMetrics
// via wire.NewOpsMetrics(), swaps pgBackupPushedRoot to a t.TempDir,
// stamps a fake mtime, and asserts the gauge reads time.Since(stamp).
// See review F8 + issue #250 follow-up.
func (s *pgBackupPushedSampler) tick() {
	gauge := s.ops.PgBackupLastPushed()
	if gauge == nil {
		return
	}
	newest, ok := newestEntryMtime(pgBackupPushedRoot)
	if !ok {
		// Empty dir or stat failure — keep the gauge at 0 so the
		// alert doesn't false-fire on a fresh install.
		gauge.Set(0)
		return
	}
	ageSeconds := time.Since(newest).Seconds()
	if ageSeconds < 0 {
		// Clock skew between stat + time.Now — clamp to 0 rather
		// than emit a negative value. Prometheus rates would
		// otherwise refuse to compute `time() - negative`.
		ageSeconds = 0
	}
	gauge.Set(ageSeconds)
}

// newestEntryMtime returns the maximum mtime of entries directly
// under root. Returns ok=false when root is missing, unreadable,
// or empty.
//
// Why max mtime (not entry count, not sum of bytes): the alert
// rule cares about "the most recent backup we have"; a stale
// + fresh pair should still report fresh. The newest-tarball
// mtime is the cheapest signal that captures both freshness and
// recency-of-failure (a stuck push leaves the local dir with the
// last successful tarball's mtime, which climbs past 24h).
//
// Why not recursive Walk: the basebackup root holds tarballs
// directly (no nested dirs), and Walk would stat every file
// instead of every directory entry.
func newestEntryMtime(root string) (time.Time, bool) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return time.Time{}, false
	}
	var newest time.Time
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	if newest.IsZero() {
		return time.Time{}, false
	}
	return newest, true
}
