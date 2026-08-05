// cmd/meterd/caps.go — DEPLOY-1 / ADR-075 cap declaration.
//
// meterd is the metering daemon — it reads per-instance
// usage counters from the runtime DB, computes the billing
// rollup, and emits Prometheus + Stripe events. It needs
// NO caps. Every operation is a DB read or a Prometheus
// scrape; no privileged ports, no privileged filesystem
// ops, no network setup. The empty declaration matches the
// unit file (no AmbientCapabilities).
//
// Adding a cap here would be a structural regression —
// meterd's whole design is "read-only observer of state".
// File an ADR if you think you need one.
package main

import "github.com/onebox-faas/faas/pkg/capdecl"

var capsDecl = capdecl.Declaration{
	Allow: nil,
	Deny:  nil,
}
