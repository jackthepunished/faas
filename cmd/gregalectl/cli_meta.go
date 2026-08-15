// cli_meta.go — operator-side CLI manifest (issue #911 / ADR-110 PR-6.5).
//
// Hand-curated manifest of every top-level gregalectl command, used as
// the single source of truth for `gregalectl completion {bash|zsh|fish|powershell}`
// and `gregalectl man [command]`. Mirrors the dispatch switch in
// main.go (cmd/gregalectl/main.go::run); the manifest-drift test
// (commands_completion_test.go::TestCompletion_ManifestDrift) walks
// main.go and asserts every `case "<name>":` arm has a matching
// cliCommand entry, and vice versa.
//
// This is the operator half of the original cmd/gregale/cli_meta.go.
// PR-6.5 atomic split: customer commands stay in cmd/gregale/cli_meta.go,
// operator commands live here. New operator commands add a 4-line entry
// here at the same time as the `case "<name>":` in main.go — the
// code-review gate fires when either side is missing.
//
// PR-7 (cutover runbook) may extract a shared `cmd/internal/cliutil/`
// package; for PR-6.5 the type definitions below are duplicated into
// cmd/gregale/cli_meta.go (identical shapes) and the operator binary
// is self-contained.

package main

// cliCommand is one top-level gregalectl command. Mirrors
// cmd/gregale/cli_meta.go::cliCommand byte-for-byte (PR-6.5).
type cliCommand struct {
	Name        string
	DocSlug     string
	Short       string
	Subcommands []cliSub
	Flags       []cliFlag
	Positionals []string
	ClosedSet   []string
}

func (c cliCommand) hasSlugFirst() bool {
	return len(c.Positionals) > 0 && c.Positionals[0] == "<slug>"
}

// cliSub is one verb under a cliCommand. Mirrors cmd/gregale/cli_meta.go.
type cliSub struct {
	Name  string
	Short string
	Flags []cliFlag
}

// cliFlag is one CLI flag. Mirrors cmd/gregale/cli_meta.go.
type cliFlag struct {
	Name      string
	Short     string
	Req       bool
	ClosedSet []string
}

// cliCommands is the operator-side manifest. One entry per top-level
// command in main.go's run() switch.
//
// Operator-side surface (PR-6.5):
//   - manifest  (validate | render)
//   - release   (bundle | install)
//   - host-age  (init | rotate | status | prune-previous)
//   - pki       (init | status | rotate)
//   - sign-keys (init | rotate | status)
//   - node-key  (init | rotate | status)
//   - backup    (unseal-rclone | unseal-archive-creds)
//   - trusted-publishers (add | remove | list)
//   - version   (internal)
//   - completion (bash | zsh | fish | powershell) (internal)
//   - man       (<command>) (internal)
//
// Customer-side commands (rollback, secrets, registry, deploy, init,
// apps, deploy, etc.) stay in cmd/gregale/cli_meta.go. The drift
// test in each binary's commands_completion_test.go pins the
// boundary — adding a customer command to gregalectl or an operator
// command to gregale fails CI immediately.
var cliCommands = []cliCommand{
	{
		Name:    "backup",
		DocSlug: "backup",
		Short:   "Operator rclone config unseal (backup unseal-rclone | unseal-archive-creds)",
		Subcommands: []cliSub{
			{Name: "unseal-rclone", Short: "Unseal the rclone config"},
			{Name: "unseal-archive-creds", Short: "Unseal the log-archive credentials"},
			{Name: "init", Short: "Initialise the on-disk backup credentials store"},
		},
	},
	{
		Name:    dispatchHostAge,
		DocSlug: "host-age",
		Short:   "Operator host.age rotation (host-age init|rotate|status|prune-previous)",
		Subcommands: []cliSub{
			{Name: subHostAgeInit, Short: "Initialise host.age"},
			{Name: subHostAgeRotate, Short: "Rotate host.age"},
			{Name: subHostAgeStatus, Short: "Show host.age status"},
			{Name: subHostAgePrunePrevious, Short: "Prune the previous host.age key"},
		},
	},
	{
		Name:    "manifest",
		DocSlug: "manifest",
		Short:   "Operator split-box deployment manifest (manifest validate|render; issue #911 / ADR-110)",
		Subcommands: []cliSub{
			{
				Name:  subValidate,
				Short: "Validate a manifest YAML file (canonical path: pkg/manifest.Validate)",
				Flags: []cliFlag{{Name: "file", Short: "path to the manifest YAML file (required)"}},
			},
			{
				Name:  subRender,
				Short: "Render a validated manifest to /etc/faas/*.toml + systemd units + cgroup subtree_control + PKI leaves (canonical path: pkg/renderer.Render)",
				Flags: []cliFlag{
					{Name: "manifest-file", Short: "path to the manifest YAML file (required)", Req: true},
					{Name: "host", Short: "host in the manifest to render (default: first host)"},
					{Name: "releases-root", Short: "releases root (default /opt/faas/releases)"},
					{Name: "etc-faas-dir", Short: "TOML root (default /etc/faas)"},
					{Name: "systemd-dir", Short: "systemd unit tree (default /etc/systemd/system)"},
					{Name: "pki-root-dir", Short: "PKI root (default /etc/faas/tls)"},
					{Name: "cgroup-root", Short: "cgroup v2 mount root (default /sys/fs/cgroup)"},
					{Name: "host-san-file", Short: "optional JSON file with per-host SANs"},
					{Name: "dry-run", Short: "compute outputs but do not write"},
				},
			},
		},
	},
	{
		Name:    "release",
		DocSlug: "release",
		Short:   "Cluster-shipped release bundle (release bundle|install --git-sha SHA; PR-3 / ADR-110)",
		Subcommands: []cliSub{
			{
				Name:  subReleaseBundle,
				Short: "Materialise a release bundle from a pre-built bin directory and INSERT into release_bundles",
				Flags: []cliFlag{
					{Name: "bin-dir", Short: "path to daemon binaries directory (required)", Req: true},
					{Name: "git-sha", Short: "40-char lowercase hex git SHA (required)", Req: true},
					{Name: "manifest-hash", Short: "manifest hash as 'sha256:<64hex>' (required)", Req: true},
					{Name: "releases-root", Short: "releases root (default /opt/faas/releases)"},
				},
			},
			{
				Name:  subReleaseInstall,
				Short: "Install a release on the local box (atomic symlink flip + applied_at first-write-wins stamp + compute_nodes UPSERT)",
				Flags: []cliFlag{
					{Name: "git-sha", Short: "40-char lowercase hex git SHA to install (required)", Req: true},
					{Name: "releases-root", Short: "releases root (default /opt/faas/releases)"},
					{Name: "node", Short: "compute_nodes.name to stamp (default: hostname)"},
				},
			},
		},
	},
	{
		Name:    dispatchPKI,
		DocSlug: "pki",
		Short:   "Operator local-dev PKI bootstrap (pki init|status|rotate)",
		Subcommands: []cliSub{
			{Name: subPKIInit, Short: "Initialise the local PKI"},
			{Name: subPKIStatus, Short: "Show PKI status"},
			{Name: subPKIRotate, Short: "Rotate the PKI"},
		},
	},
	{
		Name:    dispatchSignKeys,
		DocSlug: "sign-keys",
		Short:   "Provision the cosign sign keypair (operator; --sign-key / --verify-key)",
		Subcommands: []cliSub{
			{Name: subInit, Short: "Initialise the cosign keypair"},
			{Name: subRotate, Short: "Rotate the cosign keypair"},
			{Name: subStatus, Short: "Show keypair status"},
		},
		Flags: []cliFlag{
			{Name: "sign-key", Short: "path to the sign key"},
			{Name: "verify-key", Short: "path to the verify key"},
		},
	},
	{
		Name:    dispatchNodeKey,
		DocSlug: "node-key",
		Short:   "Provision the per-node CapacityReport signing keypair (operator; ADR-053)",
		Subcommands: []cliSub{
			{Name: subNodeInit, Short: "Initialise the node signing keypair"},
			{Name: subNodeRotate, Short: "Rotate the node signing keypair"},
			{Name: subNodeStatus, Short: "Show node keypair status"},
		},
		Flags: []cliFlag{
			{Name: "node-key", Short: "path to the node signing private key"},
			{Name: "node-key-pub", Short: "path to the node signing public key"},
		},
	},
	// Internal surface — version, completion, man.
	{
		Name:    "version",
		DocSlug: "version",
		Short:   "Print the CLI version",
	},
	{
		Name:    "completion",
		DocSlug: "completion",
		Short:   "Print a shell completion script (bash|zsh|fish|powershell)",
		Subcommands: []cliSub{
			{Name: "bash", Short: "Print the bash completion script"},
			{Name: "zsh", Short: "Print the zsh completion script"},
			{Name: "fish", Short: "Print the fish completion script"},
			{Name: "powershell", Short: "Print the powershell completion snippet"},
		},
	},
	{
		Name:        "man",
		DocSlug:     "man",
		Short:       "Print the gregalectl(1) man page (or gregalectl-<command>(1) with one arg)",
		Positionals: []string{"<command>"},
	},
}
