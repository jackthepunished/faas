// cmd/gatewayd-public/caps.go — DEPLOY-1 / ADR-075 cap
// declaration.
//
// gatewayd-public is the public TLS edge listener (Tier A7
// split, ADR-070). It needs cap_net_bind_service for the
// :443 / :80 listener and nothing else. PR-J (issue #641)
// moved the control listener to :9092 — the
// AmbientCapabilities= line in
// deploy/systemd/faas-gatewayd-public.service is empty
// today, which is intentional: cap_net_bind_service is in
// CapabilityBoundingSet but NOT in AmbientCapabilities,
// and the daemon doesn't actually need to bind privileged
// ports (the public TLS edge is on 443, granted via the
// systemd socket unit pattern).
//
// The empty declaration matches the unit file. A future PR
// that switches gatewayd-public to a privileged-port bind
// must add cap_net_bind_service to both this file AND the
// systemd unit's AmbientCapabilities= line.
package main

import "github.com/onebox-faas/faas/pkg/capdecl"

var capsDecl = capdecl.Declaration{
	Allow: nil,
	Deny:  nil,
}
