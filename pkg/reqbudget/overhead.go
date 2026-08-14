// Package reqbudget: overhead.go — per-hop reservation constants.
//
// Each downstream hop reserves a slice of the parent's remaining
// budget before doing any work. The reservation is an absolute cost,
// not a measurement: it ensures hop B starts with at most
// (parentRemaining - cost) wall clock even before B's own work
// begins. Defaults are sized for the local-control-plane hop: same
// PG, same node, same loopback gRPC. Customers on bigger hops
// (cross-region DB, remote upstream) may need larger reservations —
// but the load-bearing constraint is "we never silently widen the
// budget"; never drop below these defaults.
//
// Source of truth lives in pkg/api/limits.go. This file re-exports
// the same constants under the reqbudget package name so call-sites
// can `reqbudget.DefaultOverheadDB` without importing pkg/api.
package reqbudget

import "time"

// Per-hop reservation defaults. Values match pkg/api/limits.go
// DefaultOverheadDB / GRPC / HTTP / Stream / Queue.
const (
	DefaultOverheadDB     = 10 * time.Millisecond
	DefaultOverheadGRPC   = 5 * time.Millisecond
	DefaultOverheadHTTP   = 20 * time.Millisecond
	DefaultOverheadStream = 50 * time.Millisecond
	DefaultOverheadQueue  = 5 * time.Millisecond
)

// Per-plan defaults for the per-request budget itself. Mirrors
// pkg/api/limits.go RequestBudgetDefault / RequestBudgetMax /
// RequestBudgetApidDefault. apid's default is higher than the
// gateway's because apid serves dashboards + admin + sync-invoke
// long-polls (already capped at 910 s upstream), and a 3 s apid
// budget would cut admin operations that legitimately take longer.
const (
	DefaultBudget     = 3 * time.Second
	DefaultBudgetMax  = 30 * time.Second
	DefaultBudgetApid = 5 * time.Second
)
