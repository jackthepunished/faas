// Package cgroupstats reads per-VM cgroup v2 counters (cpu.stat
// usage_usec, memory.current) for the vmmd Stats handler that feeds
// schedd's per-instance metrics poller (issue #170).
//
// It is a sibling of pkg/fcvm/leakcheck: that package's only job is
// post-mortem RAM leak accounting at end-of-test, while cgroupstats is
// the runtime feed for live signal collection. Keeping them separate
// preserves the leakcheck contract and lets cgroupstats evolve
// independently (delta math, freshness windows, non-Linux fallthrough).
//
// Paths follow the lockstep convention in pkg/fcvm/cgroup.go and
// pkg/fcvm/config.go: cgroupRoot + faas-tenant.slice/<instance>/, where
// <instance> is the bare Lease.Instance (no '.scope' suffix — jailer
// v1.7 rejects '.' in --id, see pkg/fcvm/cgroup.go:24).
//
// Non-Linux hosts return ok=false; the Stats handler translates that
// into resident_bytes=null / cpu_pct=null on the wire so the schedd
// poller can mark the row Unknown and exclude it from the
// {app,node}-rolled-up Prometheus series.
package cgroupstats
