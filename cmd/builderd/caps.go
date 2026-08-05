// cmd/builderd/caps.go — DEPLOY-1 / ADR-075 cap declaration.
//
// builderd is the build orchestrator (ADR-003). It does NOT
// itself run the build — that happens inside ephemeral
// builder microVMs owned by vmmd. builderd just queues jobs,
// waits for vmmd's gRPC events, and tracks build state. It
// needs NO caps.
//
// A common confusion: builderd sounds like it should be a
// "privileged build host" but it's the orchestrator, not the
// runtime. The build VM (Firecracker, ephemeral, snapshotted
// in pkg/fcvm) is owned by vmmd and the actual compilation
// runs inside the VM. builderd is a control-plane daemon with
// no filesystem ops, no privileged ports, no network setup.
//
// The empty declaration matches the unit file (no
// AmbientCapabilities). Adding a cap here is a structural
// regression — file an ADR.
package main

import "github.com/onebox-faas/faas/pkg/capdecl"

var capsDecl = capdecl.Declaration{
	Allow: nil,
	Deny:  nil,
}
