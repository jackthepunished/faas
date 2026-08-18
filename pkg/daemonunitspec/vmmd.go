package daemonunitspec

import "github.com/onebox-faas/faas/pkg/daemonunit"

// UnitVmmd is the canonical unit for faas-vmmd — microVM supervisor.
// vmmd is the ONLY root component (spec §4.4, CLAUDE.md component
// ownership). It owns firecracker + the jailer + the overlay mount
// RPC handler (DEPLOY-1).
//
// Wipe-comments-load-bearing rationale that USED to live in the unit
// file body, now preserved here:
//
//   - /run/faas is a host-shared tmpfs directory provisioned by tmpfiles.d
//     and reasserted by vmmd's host-side install/chown fixups. vmmd must
//     not declare RuntimeDirectory=faas: systemd's per-unit runtime mount
//     can remove or hide gatewayd/schedd sockets during a vmmd restart.
//     schedd + gatewayd-internal + imaged write into the shared host path
//     via ReadWritePaths=/run/faas.
//   - PrivateTmp MUST be `=no` — `=yes` makes /run/faas land inside
//     vmmd's per-mount-namespace tmpfs and become invisible from the
//     host, breaking every other daemon's dial of /run/faas/vmmd.sock
//     (run 30839233808).
//   - vmmd has CAP_NET_BIND_SERVICE so it can bind the /metrics low
//     TCP port if MetricsAddr is set in TOML.
//
// See ADR-078 for the migration that wiped these from the unit body.
func UnitVmmd() daemonunit.Unit {
	return daemonunit.Unit{
		Description:   "onebox-faas vmmd — microVM supervisor (the only root component, spec §4.4)",
		Documentation: "https://docs.gregale.dev/ops/vmmd",
		After:         []string{"faas-tenant.slice", "faas-cp.slice"},
		Wants:         []string{"faas-tenant.slice", "faas-cp.slice"},

		Type: "simple",
		// No User=/Group=: vmmd is root by design.
		ExecStart: `/opt/faas/current/bin/vmmd --config /etc/faas/vmmd.toml`,
		ExecStartPre: []string{
			`/usr/bin/install -d -o root -g faas -m 0775 /run/faas`,
			`/usr/bin/chmod 0775 /run/faas`,
		},
		// Re-assert the shared host-directory ownership after startup.
		// This keeps a hand-edited or manually repaired /run tree safe for
		// gatewayd and schedd after a vmmd restart.
		ExecStartPost: []string{
			`/usr/bin/chown root:faas /run/faas`,
			`/usr/bin/chmod 0775 /run/faas`,
		},
		Restart:    "on-failure",
		RestartSec: "2s",

		Slice:     "faas-cp.slice",
		MemoryMax: "256M",

		AmbientCapabilities: []string{"CAP_NET_BIND_SERVICE"},

		NoNewPrivileges:       true,
		ProtectSystem:         "strict",
		ProtectHome:           true,
		PrivateTmp:            daemonunit.BoolPtr(false), // /run/faas is host-shared.
		ProtectKernelTunables: true,
		ProtectKernelModules:  true,
		ProtectControlGroups:  true,

		ReadWritePaths: []string{"/etc/faas/secrets", "/run/faas", "/srv/fc", "/var/log/faas"},

		WantedBy: "multi-user.target",
	}
}
