// cmd/apid/caps.go — DEPLOY-1 / ADR-075 cap declaration.
//
// apid is the customer-facing API daemon. It needs
// cap_net_bind_service for the public HTTPS listener
// (deploy/systemd/faas-apid.service: AmbientCapabilities=CAP_NET_BIND_SERVICE)
// and nothing else. The Allow list is the single cap.
//
// apid is NOT a root component — it runs as User=faas-apid
// with NoNewPrivileges=yes. The bounding set in the unit file
// can stay broad (no shrinking in DEPLOY-1); the runtimecheck
// validates the Allow list against /proc/self/status.
//
// A future PR that adds a real privilege requirement to apid
// (e.g. snapshot-aware request routing) MUST extend this
// declaration AND the systemd unit's AmbientCapabilities=
// line AND cite the ADR. The lint rule blocks pkg/vmmdgrpc
// imports outside cmd/vmmd/ + pkg/vmmd/ so a misuse attempt
// trips at CI time.
package main

import "github.com/onebox-faas/faas/pkg/capdecl"

var capsDecl = capdecl.Declaration{
	Allow: []string{
		"cap_net_bind_service",
	},
	Deny: nil,
}
