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
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onebox-faas/faas/pkg/releaseinstall"
	"github.com/onebox-faas/faas/pkg/roleTemplating"
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
	// cloud-init user-data), adopt it. This makes first-boot work
	// without passing --role explicitly on the runbook step.
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
	// ADR-112: after the symlink flip (the load-bearing step),
	// apply role templating. The drop-ins + daemon-reload are
	// what materially makes FAAS_BOX_ROLE take effect on the box.
	// Failure here aborts the install with exit 4 — the symlink is
	// flipped but the daemons aren't role-correct yet. Doctor
	// (PR #921) will flag this; the operator re-runs with the
	// correct --role to recover.
	if *roleFlag != "" {
		role := roleTemplating.Role(*roleFlag)
		if err := roleTemplating.ApplyFilesystem(role, ""); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "gregalectl release install: apply role %s: %v\n", *roleFlag, err)
			return 4
		}
		// Start the role-appropriate subset. PR-A runs Apply on a
		// blank box (first-boot); idempotency is preserved
		// because systemctl start on an already-active unit is
		// a no-op.
		daemons, _ := roleTemplating.Subset(role)
		for _, d := range daemons {
			cmd := exec.Command("systemctl", "start", fmt.Sprintf("faas-%s.service", d))
			if out, err := cmd.CombinedOutput(); err != nil {
				_, _ = fmt.Fprintf(os.Stderr,
					"gregalectl release install: start faas-%s.service: %v (%s)\n",
					d, err, string(out))
				return 4
			}
		}
	}
	// DB writes — best effort. The on-disk symlink flip is the
	// load-bearing side; the DB row records the audit trail and
	// first-write-wins mark.
	pool, dbErr := openPgPoolFromEnv()
	if dbErr != nil {
		if jsonEnabled() {
			jsonEmit(os.Stdout, releaseInstallReport{
				GitSHA:      *gitSHA,
				DBError:     dbErr.Error(),
				SymlinkOnly: true,
			})
		} else {
			_, _ = fmt.Fprintf(os.Stdout, "flipped current -> %s (DB unreachable: %v)\n", *gitSHA, dbErr)
		}
		return 3
	}
	defer pool.Close()
	store := releaseinstall.NewStore(pool)
	first, err := store.MarkApplied(context.Background(), *gitSHA)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "gregalectl release install: mark applied: %v\n", err)
		return 3
	}
	node := *nodeName
	if node == "" {
		var herr error
		node, herr = os.Hostname()
		if herr != nil {
			node = "unknown"
		}
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
// returns the FAAS_BOX_ROLE value. Returns ok=false if the file is
// missing or FAAS_BOX_ROLE is unset.
//
// The sentinel `__SET_BY_OPERATOR_AT_LAUNCH__` is the explicit
// "operator did NOT override FAAS_BOX_ROLE in user-data" marker; the
// cloud-init first-boot runcmd's assert-first-boot-env.sh detects it
// and fails loud BEFORE this code runs. We treat the sentinel
// identically to "absent" (returns ok=false), so the cmdReleaseInstall
// caller can decide what to do — typically, surface a clearer error.
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
		if value == "" ||
			value == "__SET_BY_OPERATOR_AT_LAUNCH__" ||
			value == "control-plane" ||
			value == "compute-only" {
			if value == "control-plane" || value == "compute-only" {
				return value, true
			}
			return "", false
		}
		// Unknown role value — surface to caller as if absent;
		// cmdReleaseInstall will exit 2 with the validation error.
		return "", false
	}
	return "", false
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
