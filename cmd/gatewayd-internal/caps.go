// cmd/gatewayd-internal/caps.go — DEPLOY-1 / ADR-075 cap
// declaration.
//
// gatewayd-internal is the routing + wake + proxy daemon
// that listens on a unix socket inside the box
// (deploy/systemd/faas-gatewayd-internal.service, Tier A7
// split). It needs NO caps — every filesystem / network op
// goes through vmmd / schedd / apid. The empty declaration
// matches the unit file (no AmbientCapabilities).
//
// gatewayd-internal's wire socket is a unix-domain socket
// in /run/faas/ — no privileged port binding required, so
// no cap_net_bind_service. The TLS edge listener moved to
// gatewayd-public (PR #633 / ADR-070).
//
// Adding a cap here is a load-bearing event: it means the
// gateway is doing something itself instead of routing
// through the privileged components. File an ADR.
package main

import "github.com/onebox-faas/faas/pkg/capdecl"

var capsDecl = capdecl.Declaration{
	Allow: nil,
	Deny:  nil,
}
