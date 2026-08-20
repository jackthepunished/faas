package daemonunitspec

import "github.com/onebox-faas/faas/pkg/daemonunit"

// UnitSchedd is the canonical unit for faas-schedd — scheduler +
// instance state machine owner (spec §4.3, §6).
//
// Wipe-comments-load-bearing rationale:
//
//   - schedd is the SOLE writer to the `instances` table
//     (CLAUDE.md Component ownership).
//   - `PrivateTmp=` MUST be `no` for the same reason as vmmd: with
//     `=yes`, schedd.sock lands inside schedd's per-mount-namespace tmpfs
//     and imaged (which dials from its own mount ns) gets "no such file
//     or directory" even though `ss -lnx` shows the LISTEN entry
//     (run 30839233808). schedd does NOT declare RuntimeDirectory=faas;
//     it inherits the bind-mount from vmmd's declaration via
//     ReadWritePaths=/run/faas.
//
// See ADR-078 for the migration that wiped this from the unit body.
func UnitSchedd() daemonunit.Unit {
	return daemonunit.Unit{
		Description: "onebox-faas schedd — scheduler + lifecycle owner",
		After:       []string{"network.target", "faas-cp.slice", "faas-brokerq.slice"},
		Wants:       []string{"faas-cp.slice", "faas-brokerq.slice"},

		Type:  "simple",
		User:  "faas-schedd",
		Group: "faas",
		ExecStartPre: []string{
			`/usr/bin/install -d -o faas-schedd -g faas -m 0770 /var/lib/faas/oci-tmp`,
			// Apply the broker-egress tc qdisc on the brokerq
			// host interface before schedd starts polling. The
			// actual command is synthesised by
			// pkg/sched.BrokerTcCommands at first-boot from
			// FAAS_BROKER_EGRESS_MBIT — when EgressMbit == 0
			// the slice + ExecStartPre line are no-ops, matching
			// the Hobby / no-quota plan shape (ADR-118 §9).
			`/opt/faas/current/bin/schedd-brokerq-apply`,
		},
		ExecStart:  `/opt/faas/current/bin/schedd --config /etc/faas/schedd.toml`,
		Restart:    "on-failure",
		RestartSec: "2s",

		Slice:     "faas-cp.slice",
		MemoryMax: "256M",

		EnvironmentFile: "/etc/faas/sealed.env",
		Environment: []daemonunit.KV{
			{Key: "TMPDIR", Value: "/var/lib/faas/oci-tmp"},
		},

		NoNewPrivileges:       true,
		ProtectSystem:         "strict",
		ProtectHome:           true,
		PrivateTmp:            daemonunit.BoolPtr(false), // inherits /run/faas via vmmd's bind-mount
		ProtectKernelTunables: true,
		ProtectKernelModules:  true,
		ProtectControlGroups:  true,

		ReadOnlyPaths:  []string{"/etc/faas"},
		ReadWritePaths: []string{"/run/faas", "/var/lib/faas", "/var/log/faas"},

		WantedBy: "multi-user.target",
	}
}
