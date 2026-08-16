// commands_release.go — operator-side CLI for the cluster-shipped
// release bundle (issue #911 / ADR-110).
//
// `gregalectl release` is the operator surface that materialises a
// release bundle from a pre-built bin directory and installs it on
// the local box. The two subcommands map to the two halves of
// PR-3:
//
//   gregalectl release bundle --bin-dir PATH --git-sha SHA --manifest-hash HASH
//   gregalectl release install --git-sha SHA [--releases-root PATH] [--node NAME] [--role ROLE]
//
// Dispatcher shape mirrors commands_manifest.go:
// flag.Parse for the leaf's own flags, subcommand fan-out in
// cmdReleaseDispatch, --json / FAAS_JSON=1 honored.
//
// Materialise-from-git (`make build-sha256`, etc.) is intentionally
// OUT of scope: PR-3 only flips the symlink + writes the
// release_bundles row + stamps the first-write-wins applied_at.
// The operator runs the deterministic build target out-of-band and
// hands `gregalectl release bundle` the resulting bin directory.
//
// ADR-112 (role-image-collapse):
//   --role is a NEW flag on `release install`. First-boot flow:
//     `release install --role $FAAS_BOX_ROLE` (or the equivalent
//     implicit read from /etc/faas/first-boot.env when --role is
//     empty). PR-B (issue #935) extends --role to be the in-place
//     mutation flag: `release install --role compute-only` on an
//     already-running control-plane box transitions the subset.

package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onebox-faas/faas/pkg/releaseinstall"
	"github.com/onebox-faas/faas/pkg/roleTemplating"
	"github.com/onebox-faas/faas/pkg/state"
)

// release subcommands.
const (
	subReleaseBundle  = "bundle"
	subReleaseInstall = "install"
)

// cmdReleaseDispatch is the parent dispatcher.
func cmdReleaseDispatch(args []string) int {
	if len(args) == 0 {
		printReleaseUsage(os.Stderr)
		return 1
	}
	switch args[0] {
	case subReleaseBundle:
		return cmdReleaseBundle(args[1:])
	case subReleaseInstall:
		return cmdReleaseInstall(args[1:])
	case flagHelpShort, flagHelpLong:
		printReleaseUsage(os.Stderr)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "gregalectl release: unknown subcommand %q (expected: bundle | install)\n", args[0])
		return 1
	}
}

func printReleaseUsage(w io.Writer) {
	_, _ = fmt.Fprintf(w, `usage: gregalectl release <subcommand> [flags]

Subcommands:
  bundle    Materialise a release bundle from a pre-built bin directory.
            Writes <releases-root>/<git-sha>/release-manifest.json and
            INSERTs a row into release_bundles.
  install   Install a release on the local box (atomic symlink flip +
            release_bundles.applied_at first-write-wins stamp).

Flags (bundle):
  --bin-dir PATH        Path to the directory holding the daemon binaries
                        (one file per daemon in the manifest catalog).
  --git-sha SHA         40-char lowercase hex git SHA (required).
  --manifest-hash HASH  Manifest hash as 'sha256:<64hex>' (required).
  --releases-root PATH  Releases root (default: /opt/faas/releases).

Flags (install):
  --git-sha SHA         40-char lowercase hex git SHA to install (required).
  --releases-root PATH  Releases root (default: /opt/faas/releases).
  --node NAME           compute_nodes.name to stamp (default: hostname).

Exit codes:
  0  success
  1  usage error / invalid input
  3  platform/infra (file missing, DB unreachable, symlink target invalid)

Examples:
  gregalectl release bundle --bin-dir=out/bin --git-sha=$(git rev-parse HEAD) \
      --manifest-hash=sha256:$(sha256sum manifest.yaml | cut -d' ' -f1)
  gregalectl release install --git-sha=$(git rev-parse HEAD)
`)
}

// cmdReleaseBundle materialises a cluster-shipped release bundle:
// hashes every daemon binary in --bin-dir, writes the manifest, and
// INSERTs a row into release_bundles.
//
// This is the operator-side CLI for PR-3; it does NOT build the
// binaries (that is `make build-sha256`'s job, run out-of-band).
// The operator hands the CLI the already-built bin directory plus
// the git_sha + manifest_hash that go with it.
func cmdReleaseBundle(args []string) int {
	if len(args) > 0 && (args[0] == flagHelpLong || args[0] == flagHelpShort) {
		PrintUsage(os.Stderr, "usage: gregalectl release bundle --bin-dir PATH --git-sha SHA --manifest-hash HASH", "release")
		return 0
	}
	fs := flag.NewFlagSet("release bundle", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	binDir := fs.String("bin-dir", "", "path to the directory holding daemon binaries (required)")
	gitSHA := fs.String("git-sha", "", "40-char lowercase hex git SHA (required)")
	manifestHash := fs.String("manifest-hash", "", "manifest hash as 'sha256:<64hex>' (required)")
	releasesRoot := fs.String("releases-root", "/opt/faas/releases", "releases root directory")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *binDir == "" || *gitSHA == "" || *manifestHash == "" {
		_, _ = fmt.Fprintln(os.Stderr, "gregalectl release bundle: --bin-dir, --git-sha, and --manifest-hash are required")
		return 1
	}
	// Validate git_sha / manifest_hash shape BEFORE touching the
	// filesystem so a malformed CLI argument surfaces as a usage
	// error (exit 1), not as a platform/infra error (exit 3).
	if err := releaseinstall.ValidateManifest(releaseinstall.Manifest{
		FormatVersion: releaseinstall.FormatVersion,
		GitSHA:        *gitSHA,
		ManifestHash:  *manifestHash,
	}); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "gregalectl release bundle: %v\n", err)
		return 1
	}

	// Resolve absolute bin dir so the manifest's per-release
	// directory walks the same paths the verifier (PR-4) will.
	absBin, err := filepath.Abs(*binDir)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "gregalectl release bundle: resolve bin-dir: %v\n", err)
		return 3
	}
	if _, err := os.Stat(absBin); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "gregalectl release bundle: bin-dir: %v\n", err)
		return 3
	}
	if err := copyBinIntoRelease(*releasesRoot, *gitSHA, absBin); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "gregalectl release bundle: stage bin: %v\n", err)
		return 3
	}
	now := time.Now().UTC()
	m, err := releaseinstall.Build(*releasesRoot, *gitSHA, *manifestHash, now)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "gregalectl release bundle: build manifest: %v\n", err)
		return 1
	}
	if err := releaseinstall.Write(*releasesRoot, m); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "gregalectl release bundle: write manifest: %v\n", err)
		return 3
	}
	// INSERT into release_bundles. If the DB is unreachable, we
	// still have the on-disk manifest — the operator can retry
	// the INSERT (release_bundles has no UNIQUE on git_sha so
	// retries would collide; the CLI surfaces that as a conflict).
	pool, dbErr := openPgPoolFromEnv()
	if dbErr != nil {
		if jsonEnabled() {
			jsonEmit(os.Stdout, releaseBundleReport{
				GitSHA:       *gitSHA,
				ManifestHash: *manifestHash,
				ManifestPath: filepath.Join(releaseinstall.BundleRoot(*releasesRoot, *gitSHA), releaseinstall.ManifestName),
				DBError:      dbErr.Error(),
			})
		} else {
			_, _ = fmt.Fprintf(os.Stdout, "wrote %s for %s (DB unreachable: %v)\n",
				filepath.Join(releaseinstall.BundleRoot(*releasesRoot, *gitSHA), releaseinstall.ManifestName),
				*gitSHA, dbErr)
		}
		return 3
	}
	defer pool.Close()
	store := releaseinstall.NewStore(pool)
	id, err := store.Insert(context.Background(), releaseinstall.FromManifest(m))
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "gregalectl release bundle: insert release_bundles: %v\n", err)
		return 3
	}
	if jsonEnabled() {
		jsonEmit(os.Stdout, releaseBundleReport{
			GitSHA:       *gitSHA,
			ManifestHash: *manifestHash,
			ManifestPath: filepath.Join(releaseinstall.BundleRoot(*releasesRoot, *gitSHA), releaseinstall.ManifestName),
			ID:           id,
		})
	} else {
		_, _ = fmt.Fprintf(os.Stdout, "wrote %s for %s (id=%s)\n",
			filepath.Join(releaseinstall.BundleRoot(*releasesRoot, *gitSHA), releaseinstall.ManifestName),
			*gitSHA, id)
	}
	return 0
}

// copyBinIntoRelease copies every regular file in srcDir into
// <releasesRoot>/<gitSHA>/bin/<basename>. Symlinks and directories
// are skipped — the bundle is a flat list of daemon binaries.
func copyBinIntoRelease(releasesRoot, gitSHA, srcDir string) error {
	bin := releaseinstall.BinDir(releasesRoot, gitSHA)
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", bin, err)
	}
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return fmt.Errorf("read %s: %w", srcDir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		src := filepath.Join(srcDir, e.Name())
		dst := filepath.Join(bin, e.Name())
		body, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("read %s: %w", src, err)
		}
		if err := os.WriteFile(dst, body, 0o755); err != nil {
			return fmt.Errorf("write %s: %w", dst, err)
		}
	}
	return nil
}

// cmdReleaseInstall installs a release on the local box: flips
// /opt/faas/current to <git-sha>, UPSERTs compute_nodes.release_id
// + manifest_hash (PR-6), and runs the release_bundles.applied_at
// first-write-wins UPDATE.
//
// DB writes go through the releaseinstall.Store abstraction so
// tests can inject a fake; the production code path uses pgxpool
// directly to avoid spurious pgstore regen for the per-PR release
// table.
func cmdReleaseInstall(args []string) int {
	if len(args) > 0 && (args[0] == flagHelpLong || args[0] == flagHelpShort) {
		PrintUsage(os.Stderr, "usage: gregalectl release install --git-sha SHA [--role ROLE]", "release")
		return 0
	}
	fs := flag.NewFlagSet("release install", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	gitSHA := fs.String("git-sha", "", "40-char lowercase hex git SHA to install (required)")
	releasesRoot := fs.String("releases-root", "/opt/faas/releases", "releases root directory")
	nodeName := fs.String("node", "", "compute_nodes.name to stamp (default: hostname)")
	// ADR-112: --role is the role-templating trigger. Empty means
	// "do nothing here" (legacy callers); when set, the binary
	// applies roleTemplating.ApplyFilesystem(role) after the
	// symlink flip.
	roleFlag := fs.String("role", "", "box role: control-plane|compute-only (ADR-112). Empty = no role templating.")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *gitSHA == "" {
		_, _ = fmt.Fprintln(os.Stderr, "gregalectl release install: --git-sha is required")
		return 1
	}
	// ADR-112: if --role is empty AND /etc/faas/first-boot.env has
	// FAAS_BOX_ROLE set (the operator-supplied sentinel from
	// cloud-init user-data), adopt the raw value and let Validate
	// catch unknown / typo'd strings (post-#930 review Fix 5).
	// Unknown values used to be silently dropped here, leaving the
	// install to exit 0 with no role templating — a footgun.
	if *roleFlag == "" {
		if envRole, ok := readFirstBootRole(); ok {
			*roleFlag = envRole
		}
	}
	// Validate --role BEFORE any side-effects (no symlink flip if
	// role is bogus). Per the [[gregalectl-dispatch-manifest-
	// completeness]] lesson: stable exit codes; usage errors exit 2,
	// runtime errors exit ≥3.
	if *roleFlag != "" {
		if err := roleTemplating.Validate(roleTemplating.Role(*roleFlag)); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "gregalectl release install: %v\n", err)
			return 2
		}
	}
	// Verify the bundle on disk before flipping the symlink —
	// PR-4 doctor will do this same check; PR-3's install path
	// is just as strict, so a corrupted bundle never becomes the
	// active release.
	m, err := releaseinstall.Read(*releasesRoot, *gitSHA)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "gregalectl release install: read manifest: %v\n", err)
		return 3
	}
	if err := releaseinstall.Verify(*releasesRoot, m); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "gregalectl release install: verify manifest: %v\n", err)
		return 3
	}
	if err := releaseinstall.AtomicFlip(*releasesRoot, *gitSHA); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "gregalectl release install: flip symlink: %v\n", err)
		return 3
	}
	// Open the DB pool BEFORE the role branch so PR-B's drain gate
	// and compute_nodes.role read can use it. The DB can fail
	// (no DSN, no row) — that's the legacy first-boot path; we
	// fall through to the env fallback. node is the canonical
	// compute_nodes.name for this box.
	openPool, dbErr := openPgPoolFromEnv()
	if dbErr != nil {
		openPool = nil // legacy / --no-db mode
	} else {
		defer openPool.Close()
	}
	node := *nodeName
	if node == "" {
		var herr error
		node, herr = os.Hostname()
		if herr != nil {
			node = "unknown"
		}
	}
	// ADR-112: after the symlink flip (the load-bearing step),
	// apply role templating. The drop-ins + daemon-reload are
	// what materially makes FAAS_BOX_ROLE take effect on the box.
	//
	// PR-B (issue #935) extends `--role` to be the in-place role
	// mutation flag. On a running box with a different existing
	// role, the flow is: read current role from DB (or env fallback),
	// short-circuit on same-role, drain-gate on live instances,
	// then Mutate(stop old subset, start new subset) instead of
	// the blank-box Apply path. sealed.env / host.age / rclone.conf
	// / cosign keys / TLS leaves are NOT touched — the mutation is
	// purely a "what daemons run here" change.
	if *roleFlag != "" {
		target := roleTemplating.Role(*roleFlag)
		// Read the current role. PR-B prefers the DB (compute_nodes.role
		// by id) over the env fallback. The DB can fail (no DSN, no row)
		// — that's the legacy first-boot path; we fall through to env
		// or to the blank-box Apply.
		cnID, current := readCurrentRole(context.Background(), openPool, node)
		if current == target {
			// Idempotent re-run. No drop-in re-templating, no
			// daemon restarts, NO compute_nodes UPDATE — the
			// latter matters because every UPDATE fires the
			// compute_nodes trigger + pg_notify storm across the
			// cluster for no reason. Print a clear no-op line so
			// the operator sees their intent was honored.
			_, _ = fmt.Fprintf(os.Stderr,
				"gregalectl release install: already role=%s; no-op (sealed-env and daemons untouched, no DB write)\n",
				current)
		} else {
			// Drain gate. Hard block on live instances.
			if err := assertDrainStatus(context.Background(), openPool, node); err != nil {
				_, _ = fmt.Fprintf(os.Stderr,
					"gregalectl release install: %v\n"+
						"  run: gregalectl compute-nodes drain-status --node %s\n"+
						"  then: gregalectl compute-nodes force-drain --node %s --yes (operator override)\n",
					err, node, node)
				return 4
			}
			// Re-template drop-ins for the new role. Idempotent
			// write of identical bytes (the only thing that
			// differs is the FAAS_BOX_ROLE / FAAS_<DAEMON>_ROLE
			// values).
			if err := roleTemplating.ApplyFilesystem(target); err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "gregalectl release install: apply role %s: %v\n", target, err)
				return 4
			}
			// Run the Mutate contract: stop the (from \ to) subset
			// in reverse dependency order with gatewayd-public last,
			// start the (to \ from) subset in forward dependency order.
			// Empty from (blank-box first-boot) or from == target
			// (idempotent) means no systemctl calls.
			stopped, started, err := roleTemplating.Mutate(current, target, systemctlExec)
			if err != nil {
				_, _ = fmt.Fprintf(os.Stderr,
					"gregalectl release install: mutate role %s -> %s: %v\n",
					current, target, err)
				return 4
			}
			_, _ = fmt.Fprintf(os.Stderr,
				"gregalectl release install: re-rolled %s -> %s (stopped %v, started %v)\n",
				current, target, stopped, started)
			// PR-B (issue #935): stamp the post-mutation role on
			// compute_nodes.role keyed by id. Done HERE (inside the
			// else branch) so idempotent re-runs (current == target
			// above) do NOT fire a pg_notify storm across the cluster.
			// The runtime allow-list is narrower than the renderer's
			// (see pkg/state.pgstore docstring); a `single-box`
			// renderer write is fine but a runtime re-role to that
			// value is rejected — by design, post-#930.
			if cnID != "" {
				pgstore := state.NewPgStore(openPool)
				if err := pgstore.SetComputeNodeRole(context.Background(), cnID, string(target)); err != nil {
					_, _ = fmt.Fprintf(os.Stderr, "gregalectl release install: set compute_nodes.role: %v\n", err)
					// Don't fail the install; the on-disk state is
					// the source of truth. Doctor will report the drift.
				}
			}
		}
	}
	// DB writes — best effort. The on-disk symlink flip is the
	// load-bearing side; the DB row records the audit trail and
	// first-write-wins mark. Reuse openPool (already opened above
	// for the PR-B role branch).
	if openPool == nil {
		if jsonEnabled() {
			jsonEmit(os.Stdout, releaseInstallReport{
				GitSHA:      *gitSHA,
				DBError:     "FAAS_PG_DSN not set",
				SymlinkOnly: true,
			})
		} else {
			_, _ = fmt.Fprintf(os.Stdout, "flipped current -> %s (DB unreachable: FAAS_PG_DSN not set)\n", *gitSHA)
		}
		return 3
	}
	store := releaseinstall.NewStore(openPool)
	first, err := store.MarkApplied(context.Background(), *gitSHA)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "gregalectl release install: mark applied: %v\n", err)
		return 3
	}
	// PR-6 (issue #911 / ADR-110): stamp the per-node release
	// membership on compute_nodes. The on-disk symlink flip is the
	// load-bearing side; the UPSERT is best-effort like the existing
	// MarkApplied write. Doctor (PR-4) reads release_id / manifest_hash
	// off this row to detect per-node drift across the cluster.
	cnID, cnErr := store.UpsertComputeNode(context.Background(), node, *gitSHA, m.ManifestHash)
	if cnErr != nil {
		if jsonEnabled() {
			jsonEmit(os.Stdout, releaseInstallReport{
				GitSHA:           *gitSHA,
				FirstApplied:     first,
				Node:             node,
				SymlinkOnly:      true,
				ComputeNodeError: cnErr.Error(),
			})
		} else {
			_, _ = fmt.Fprintf(os.Stderr, "gregalectl release install: upsert compute_nodes: %v\n", cnErr)
		}
		return 3
	}
	// PR-B (issue #935): the post-mutation role write happens
	// INSIDE the role-branch else-block above (so idempotent re-runs
	// do NOT fire the compute_nodes UPDATE trigger + pg_notify storm).
	// Do not duplicate the write here.
	if jsonEnabled() {
		jsonEmit(os.Stdout, releaseInstallReport{
			GitSHA:        *gitSHA,
			FirstApplied:  first,
			Node:          node,
			ComputeNodeID: cnID,
		})
	} else {
		_, _ = fmt.Fprintf(os.Stdout, "flipped current -> %s (first_applied=%v, node=%s, compute_node=%s)\n",
			*gitSHA, first, node, cnID)
	}
	return 0
}

// openPgPoolFromEnv returns a pgxpool.Pool wired from FAAS_PG_DSN
// (the convention used by cmd/gregale for other DB-touching
// subcommands). Returns an error if the env var is unset.
func openPgPoolFromEnv() (*pgxpool.Pool, error) {
	dsn := os.Getenv("FAAS_PG_DSN")
	if dsn == "" {
		return nil, fmt.Errorf("FAAS_PG_DSN not set")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse FAAS_PG_DSN: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return nil, fmt.Errorf("open pgxpool: %w", err)
	}
	return pool, nil
}

// readFirstBootRole parses /etc/faas/first-boot.env (the file the
// cloud-init user-data wrote at server-create time, ADR-112) and
// returns the FAAS_BOX_ROLE value (raw, unvalidated). Returns
// ok=false ONLY when the file is missing OR FAAS_BOX_ROLE is unset
// OR its value is the operator-override sentinel
// `__SET_BY_OPERATOR_AT_LAUNCH__` (the marker the cloud-init
// runcmd's assert-first-boot-env.sh detects and fails loud on).
//
// UNKNOWN values (typos like "control-plan") are returned with
// ok=true so cmdReleaseInstall's Validate() surfaces the error.
// Pre-Fix-5 the function returned ("", false) for unknowns, leaving
// `*roleFlag == ""` and silently skipping role templating — the
// install exited 0 with no daemons templated, which is the worst
// possible failure mode. The post-Fix-5 contract: be loud about
// unknowns, fall back to legacy-only on absent/sentinel.
func readFirstBootRole() (string, bool) {
	const path = "/etc/faas/first-boot.env"
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "FAAS_BOX_ROLE=") {
			continue
		}
		value := strings.TrimPrefix(line, "FAAS_BOX_ROLE=")
		value = strings.Trim(value, "\"'")
		if value == "" || value == "__SET_BY_OPERATOR_AT_LAUNCH__" {
			return "", false
		}
		return value, true
	}
	return "", false
}

// readCurrentRole resolves the box's current role (ADR-112 PR-B).
// Returns the compute_nodes.id alongside the role so the caller
// can re-use it for the post-mutation SetComputeNodeRole write
// (pkg/state keys by id, not by name — the renderer side keys by
// name, but the runtime path is id-keyed). The id is the same row
// that the existing UpsertComputeNode returns later in
// cmdReleaseInstall, so callers can pass it through to
// SetComputeNodeRole directly and skip the duplicate lookup.
//
// Source-of-truth ordering:
//
//  1. compute_nodes.role via pgxpool (DB truth).
//  2. FAAS_BOX_ROLE from /etc/faas/first-boot.env (legacy
//     first-boot path, no DB row yet).
//  3. Empty Role, empty id — "blank-box first-boot" (no current
//     role to compare against). The caller runs the blank-box
//     Apply path (PR-A behaviour, unchanged).
//
// All failure modes are silent-by-design: PR-B is best-effort.
// The DB read errors are treated as "no row" — the env fallback
// kicks in. The env fallback returning "" is the blank-box path.
func readCurrentRole(ctx context.Context, pool *pgxpool.Pool, nodeName string) (cnID string, current roleTemplating.Role) {
	if pool != nil && nodeName != "" {
		var id, role *string
		err := pool.QueryRow(ctx,
			`SELECT id, role FROM compute_nodes WHERE name = $1`, nodeName).Scan(&id, &role)
		if err == nil && id != nil {
			cnID = *id
			if role != nil && *role != "" {
				return cnID, roleTemplating.Role(*role)
			}
			return cnID, "" // row exists, role unset — env fallback below
		}
	}
	if envRole, ok := readFirstBootRole(); ok && envRole != "" {
		return "", roleTemplating.Role(envRole)
	}
	return "", ""
}

// assertDrainStatus fails PR-B's drain gate. Returns nil if the
// node has no live instances (drain-safe), or a loud error pointing
// at the operator-override path otherwise.
//
// Walks the same SQL as cmdComputeNodesDrainStatus (the dedicated
// compute-nodes subcommand) for parity, but inlined here so the
// gate is a single function call without forking the CLI. The
// Postgres query counts rows in (WAKING, COLD_BOOTING, RUNNING)
// keyed by the node_id column on the instances table.
//
// Three terminal states, all disambiguated explicitly:
//
//  1. compute_nodes row MISSING for this name — return an explicit
//     error. The previous shape used
//     `WHERE node_id = (SELECT id ...)` which evaluates the
//     subquery to NULL and the predicate to UNKNOWN (not FALSE),
//     silently treating "no row" as "zero live instances" and
//     bypassing the gate. Fail-closed is the right default: a
//     half-mutated box whose row was deleted out-of-band must NOT
//     re-role without operator acknowledgement.
//  2. DB error reading the row or the count — fail-closed (block
//     the re-role).
//  3. live > 0 — return a loud error pointing at force-drain.
//
// The legacy first-boot path (no DB / empty node name) keeps its
// pass-through behaviour because there is literally no compute_nodes
// row to disambiguate against.
func assertDrainStatus(ctx context.Context, pool *pgxpool.Pool, nodeName string) error {
	if pool == nil || nodeName == "" {
		return nil // no DB / no node: skip the gate (legacy first-boot)
	}
	// 1. Resolve the row by name. ErrNoRows is treated as an explicit
	// gate failure, not as "zero live instances".
	var cnID string
	err := pool.QueryRow(ctx,
		`SELECT id FROM compute_nodes WHERE name = $1`, nodeName).Scan(&cnID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("drain gate: compute_nodes row missing for name %q; cannot verify live instances (refusing to re-role)", nodeName)
		}
		return fmt.Errorf("drain gate: cannot read compute_nodes row: %w", err)
	}
	// 2. Count live instances keyed by id (the safe join shape — no
	// NULL coercion of the subquery).
	var live int
	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM instances
		 WHERE node_id = $1
		   AND state IN ('WAKING', 'COLD_BOOTING', 'RUNNING')
	`, cnID).Scan(&live)
	if err != nil {
		return fmt.Errorf("drain gate: cannot read live-instance count: %w", err)
	}
	if live > 0 {
		return fmt.Errorf("node %q has %d live instances; re-role would kill them mid-request", nodeName, live)
	}
	return nil
}

// systemctlExec is the production execCommand adapter for
// roleTemplating.Mutate. Returns the combined stdout+stderr string
// (so Mutate's error path can include systemctl's diagnostic
// output) or an error. Mutate swallows the output on the success
// path; the error path's string is what the operator sees.
func systemctlExec(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// releaseBundleReport is the JSON wire shape for `gregalectl release
// bundle --json`.
type releaseBundleReport struct {
	GitSHA       string `json:"git_sha"`
	ManifestHash string `json:"manifest_hash"`
	ManifestPath string `json:"manifest_path,omitempty"`
	ID           string `json:"id,omitempty"`
	DBError      string `json:"db_error,omitempty"`
}

// releaseInstallReport is the JSON wire shape for `gregalectl release
// install --json`.
type releaseInstallReport struct {
	GitSHA       string `json:"git_sha"`
	FirstApplied bool   `json:"first_applied"`
	Node         string `json:"node,omitempty"`
	SymlinkOnly  bool   `json:"symlink_only,omitempty"`
	DBError      string `json:"db_error,omitempty"`
	// PR-6: compute_nodes UPSERT result. ComputeNodeID is the row id
	// from gen_random_uuid(); ComputeNodeError surfaces best-effort
	// UPSERT failures without dropping the load-bearing symlink flip.
	ComputeNodeID    string `json:"compute_node_id,omitempty"`
	ComputeNodeError string `json:"compute_node_error,omitempty"`
}

// _ keeps bytes imported even if the future JSON marshaller is
// reformatted; avoids the "imported and not used" lint when
// callers swap to streaming JSON.
var _ = bytes.NewBuffer
