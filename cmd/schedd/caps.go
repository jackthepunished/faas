// cmd/schedd/caps.go — DEPLOY-1 / ADR-075 cap declaration.
//
// schedd is the engine: it owns the instance state machine
// (spec §6) and routes wake/park/cron events to vmmd / imaged.
// It needs NO caps — every privileged operation it performs
// (network setup, VM lifecycle) goes through vmmd's gRPC.
//
// The empty declaration is intentional. A future PR that
// adds a privileged operation to schedd MUST route through
// vmmd instead; the lint rule (pkg/vmmdgrpc / pkg/vmmdmount
// only-importable-from-vmmd) is the structural enforcement.
//
// schedd's systemd unit has no AmbientCapabilities line —
// the empty declaration matches the unit file's lack of
// privilege escalation.
package main

import "github.com/onebox-faas/faas/pkg/capdecl"

var capsDecl = capdecl.Declaration{
	Allow: nil,
	Deny:  nil,
}
