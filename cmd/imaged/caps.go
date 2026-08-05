// cmd/imaged/caps.go — DEPLOY-1 / ADR-075 cap declaration.
//
// BEFORE DEPLOY-1 imaged held cap_sys_admin via
// AmbientCapabilities=cap_sys_admin in
// deploy/systemd/faas-imaged.service, and issued
// unix.Mount(2) directly in pkg/imaged/mount_overlay_linux.go.
// That was the silent CLAUDE.md invariant violation: spec §11
// says vmmd is the only root component. PR-K added the
// syscall (replacing exec /bin/mount under NNP) and PR-K.2
// moved the staging dir to /dev/shm; both papers over the
// architectural rot. DEPLOY-1 erases the violation entirely:
// imaged's parent-ref overlay mount now flows through
// MountOverlayParent / UmountOverlayParent RPCs to vmmd.
//
// After DEPLOY-1 the capsDecl for imaged is the EMPTY
// declaration (no Allow, no Deny) — the canonical
// "unprivileged daemon" entry. imaged is User=faas-imaged
// + NoNewPrivileges=yes; its only filesystem ops are cp/mkdir
// under /dev/shm/faas-base-staging (which the daemon can write
// to as faas-imaged), /srv/fc/parent (loopback mount read by
// vmmd), and the parent staging tree it owns.
//
// The systemd unit's CapabilityBoundingSet= (the existing
// full set is unchanged — bbox shrinking is out of scope for
// DEPLOY-1) is fine; the runtimecheck validates the Allow list,
// not the Bnd set. A future hardening PR can shrink Bnd to the
// per-cap Allow list via pkg/capdecl's "expected Bnd subset"
// helper (not yet implemented — DEPLOY-3 / capdecl hardening).
//
// Removing the AmbientCapabilities=cap_sys_admin line from
// faas-imaged.service is a separate ship in DEPLOY-1's
// follow-up commit (the systemd unit edit lives in
// deploy/systemd/faas-imaged.service alongside this code change).
package main

import "github.com/onebox-faas/faas/pkg/capdecl"

// capsDecl for imaged is the empty declaration. imaged is an
// unprivileged daemon (User=faas-imaged + NoNewPrivileges=yes).
// All filesystem operations that previously needed cap_sys_admin
// (parent-ref overlay mount) now go through vmmd via gRPC.
//
// The runtimecheck validates this declaration against the
// live /proc/self/status on every imaged boot. If a future
// PR silently re-adds AmbientCapabilities=cap_sys_admin to
// the unit file, the boot fails fast with a *runtimecheck.Violation
// naming cap_sys_admin — a much narrower blast radius than the
// current "silently restart-loop in production" failure mode
// that drove DEPLOY-1.
var capsDecl = capdecl.Declaration{
	Allow: nil,
	Deny:  nil,
}
