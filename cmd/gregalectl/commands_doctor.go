// commands_doctor.go — operator-side cluster-shipped release
// diagnostic (issue #911 / ADR-110 PR-4).
//
// `gregalectl doctor` is the load-bearing read-only diagnostic that
// verifies the cluster-shipped release bundle is consistent across
// three surfaces: the on-disk release tree at /opt/faas/releases,
// the release_bundles table, and the compute_nodes table.
//
// It NEVER writes anything. No symlink flip, no UPSERT, no DB
// mutation. The output is a structured report (text or --json) plus
// a process exit code:
//
//	0  healthy (no error findings, or only warnings below fail-on)
//	1  usage error (bad flag, mutually-exclusive flag combo)
//	3  drift detected (per UX §3.2 platform/infra)
//
// The six checks run in order:
//
//	symlink         /opt/faas/current read; missing / broken / stale
//	bundle          manifest on disk + Verify against bin/
//	lockstep        manifest.daemon_hashes == catalog daemon count
//	nodes           per-node release_id / manifest_hash drift
//	bundle-orphans  unapplied release_bundles rows whose bin/ is gone
//	node-hashes     --deep only: per-node re-hash against the bundle
//
// The DB is optional for checks 1-3 (omitted FAAS_PG_DSN emits a
// warn finding and skips checks 4-5). --deep requires the DB.

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/onebox-faas/faas/pkg/releaseinstall"
)

// doctorFindingsCap caps the number of findings emitted per run.
// Anything beyond is dropped and replaced with a single error
// finding telling the operator to narrow the filter. Loose
// upper bound — a fleet of 50 boxes with 6 checks each produces
// well under 100, but a misconfigured PR-3 install could
// trivially fan out 10× that.
const doctorFindingsCap = 1000

// Check name constants. The same name appears as the JSON field
// value and as the label in the per-check roll-up, so each one
// is a single const that's referenced everywhere. Matches the
// patterns in commands_release.go (subReleaseBundle etc.) and
// keeps goconst quiet.
const (
	doctorCheckSymlink       = "symlink"
	doctorCheckBundle        = "bundle"
	doctorCheckLockstep      = "lockstep"
	doctorCheckNodes         = "nodes"
	doctorCheckBundleOrphans = "bundle-orphans"
	doctorCheckNodeHashes    = "node-hashes"
	doctorCheckDB            = "db"
	doctorCheckTruncate      = "truncate"
)

// doctor severity constants. Match the wire shape so JSON consumers
// don't have to translate.
const (
	doctorSeverityOK    = "ok"
	doctorSeverityWarn  = "warn"
	doctorSeverityError = "error"
)

// doctor check status constants. Distinct from severity because
// "skipped" (DB down + --deep) is neither ok nor error.
const (
	doctorStatusOK      = "ok"
	doctorStatusWarn    = "warn"
	doctorStatusError   = "error"
	doctorStatusSkipped = "skipped"
)

// fail-on threshold constants. UX §3.2 + ADR-110 §63 use the
// canonical "warn" / "error" labels.
const (
	doctorFailOnWarn  = "warn"
	doctorFailOnError = "error"
)

// validGitSHAPattern lives in pkg/releaseinstall as ValidGitSHA. The
// doctor delegates to it so the per-node release_id check stays in
// one place (mirrors the DB CHECK constraint from migration 00272).

// doctorFinding is one row in the JSON report. The check name ties
// the row to a check; severity is from the {ok, warn, error} closed
// set. target is the per-finding object (node name, git_sha, etc.)
// and is omitted when the finding is a global one (e.g. "DB down").
type doctorFinding struct {
	Check    string `json:"check"`
	Severity string `json:"severity"`
	Target   string `json:"target,omitempty"`
	Message  string `json:"message"`
	Detail   string `json:"detail,omitempty"`
}

// doctorCounts is the headline summary. Total = OK + Warn + Error.
type doctorCounts struct {
	OK    int `json:"ok"`
	Warn  int `json:"warn"`
	Error int `json:"error"`
	Total int `json:"total"`
}

// doctorCheckSum is one per-check roll-up. Notes is the number of
// findings (of any severity) attributed to that check.
type doctorCheckSum struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	DurationMS int64  `json:"duration_ms"`
	Notes      int    `json:"notes"`
}

// doctorReport is the full JSON wire shape. Designed to be round-
// trippable by apid admin observers (PR-X) and downstream tooling.
type doctorReport struct {
	ReleasesRoot  string           `json:"releases_root"`
	NodeFilter    string           `json:"node_filter,omitempty"`
	ReleaseFilter string           `json:"release_filter,omitempty"`
	StartedAt     time.Time        `json:"started_at"`
	FinishedAt    time.Time        `json:"finished_at"`
	Healthy       bool             `json:"healthy"`
	Counts        doctorCounts     `json:"counts"`
	Findings      []doctorFinding  `json:"findings"`
	Checks        []doctorCheckSum `json:"checks"`
}

// doctorDeps is the cross-check bundle. checkSymlink writes
// currentGitSHA so the downstream checks can compare against the
// active release without re-reading the symlink. bundlesBySHA is
// populated once by runDoctorChecks when the store is wired, so
// the DB-touching checks (checkNodes + checkNodeHashes) share a
// single SELECT instead of issuing one per node.
type doctorDeps struct {
	releasesRoot  string
	nodeFilter    string
	releaseFilter string
	deep          bool
	store         releaseinstall.Store
	// currentGitSHA is set by checkSymlink (the first check) and
	// read by checkBundle. nil-safe via the doctor.medianScoped
	// pattern — checks that don't need it just ignore it.
	currentGitSHA string
	// bundlesBySHA is keyed by release_bundles.git_sha. Populated
	// lazily by the first DB check that needs it; nil means
	// "not yet loaded". The map's value carries the row's
	// manifest_hash so the deep check can do a single map
	// lookup per node (no N+1 SELECT).
	bundlesBySHA map[string]releaseinstall.BundleRow
}

// cmdDoctorDispatch is the entry point for `gregalectl doctor`.
// Mirrors commands_release.go's dispatcher shape: empty args
// prints the usage block and returns 1; flag.Parse handles the
// remainder.
func cmdDoctorDispatch(args []string) int {
	if len(args) > 0 && (args[0] == flagHelpLong || args[0] == flagHelpShort) {
		printDoctorUsage(os.Stderr)
		return 0
	}
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	nodeFilter := fs.String("node", "", "compute_nodes.name filter (default: all)")
	releaseFilter := fs.String("release", "", "release_bundles.git_sha filter (default: all)")
	releasesRoot := fs.String("releases-root", "/opt/faas/releases", "releases root directory")
	deep := fs.Bool("deep", false, "re-hash on-disk daemons per-node (slow on large fleets)")
	failOn := fs.String("fail-on", doctorFailOnError, "exit non-zero threshold: warn | error (default error)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	switch *failOn {
	case doctorFailOnWarn, doctorFailOnError:
		// ok
	default:
		_, _ = fmt.Fprintf(os.Stderr, "gregalectl doctor: --fail-on=%q is not warn|error\n", *failOn)
		return 1
	}

	deps := &doctorDeps{
		releasesRoot:  *releasesRoot,
		nodeFilter:    *nodeFilter,
		releaseFilter: *releaseFilter,
		deep:          *deep,
	}

	// Open the DB pool lazily so checks 1-3 can run without one.
	// Doctor must work on a fresh box with no DB reachable.
	pool, dbErr := openPgPoolFromEnv()
	if dbErr != nil {
		if *deep {
			// Deep requires DB (check 6 walks ListComputeNodes).
			report := doctorReport{
				ReleasesRoot: deps.releasesRoot,
				NodeFilter:   deps.nodeFilter,
				StartedAt:    time.Now(),
				Healthy:      false,
				Counts:       doctorCounts{},
				Findings: []doctorFinding{{
					Check:    doctorCheckDB,
					Severity: doctorSeverityError,
					Message:  "FAAS_PG_DSN not set; --deep requires the DB",
					Detail:   dbErr.Error(),
				}},
				Checks: []doctorCheckSum{},
			}
			report.FinishedAt = time.Now()
			report.Counts.Error = 1
			report.Counts.Total = 1
			emitDoctorReport(os.Stdout, report)
			return 3
		}
		// Without DB, the store is nil and the DB checks short-circuit.
	} else {
		defer pool.Close()
		deps.store = releaseinstall.NewStore(pool)
		// Pre-load the release_bundles table once so the
		// DB-touching checks (nodes + bundle-orphans + node-hashes)
		// share a single SELECT instead of issuing one per node.
		// Loaded here (not in each check) so all three reads
		// stay consistent — a concurrent INSERT between
		// ListAllBundles and ListComputeNodes would otherwise
		// produce a false-positive orphan-release_id finding.
		bundles, err := deps.store.ListAllBundles(context.Background())
		if err == nil {
			deps.bundlesBySHA = make(map[string]releaseinstall.BundleRow, len(bundles))
			for _, b := range bundles {
				deps.bundlesBySHA[b.GitSHA] = b
			}
		}
	}

	report := runDoctorChecks(context.Background(), deps)
	emitDoctorReport(os.Stdout, report)

	switch *failOn {
	case doctorFailOnWarn:
		if report.Counts.Error+report.Counts.Warn > 0 {
			return 3
		}
	case doctorFailOnError:
		if report.Counts.Error > 0 {
			return 3
		}
	}
	return 0
}

func printDoctorUsage(w io.Writer) {
	_, _ = fmt.Fprintf(w, `usage: gregalectl doctor [flags]

Reads the cluster-shipped release bundle's three surfaces and
reports drift. NEVER writes; safe to run on production.

Surfaces:
  on-disk       /opt/faas/releases/<git-sha>/bin + manifest
  release_bundles  the per-release INSERT row + applied_at
  compute_nodes    per-node release_id + manifest_hash

Flags:
  --node NAME           compute_nodes.name filter (default: all)
  --release SHA         release_bundles.git_sha filter (default: all)
  --releases-root PATH  releases root (default /opt/faas/releases)
  --deep                re-hash on-disk daemons per-node (slow)
  --fail-on MODE        exit non-zero threshold: warn | error (default error)

Exit codes:
  0  no drift (or only warnings below --fail-on)
  1  usage error
  3  drift detected (per UX §3.2 platform/infra)

Examples:
  gregalectl doctor
  gregalectl doctor --node=test-node
  gregalectl doctor --release=0123456789abcdef0123456789abcdef01234567
  gregalectl doctor --deep --json
`)
	_, _ = fmt.Fprintf(w, "  Docs: %sdoctor\n", docsURLBase)
}

// runDoctorChecks drives the six checks in order and gathers
// findings + per-check summaries. Order matters: checkSymlink
// populates deps.currentGitSHA for checkBundle.
func runDoctorChecks(ctx context.Context, deps *doctorDeps) doctorReport {
	startedAt := time.Now()
	report := doctorReport{
		ReleasesRoot:  deps.releasesRoot,
		NodeFilter:    deps.nodeFilter,
		ReleaseFilter: deps.releaseFilter,
		StartedAt:     startedAt,
		Counts:        doctorCounts{},
		Findings:      []doctorFinding{},
		Checks:        []doctorCheckSum{},
	}

	runCheck := func(name string, fn func() ([]doctorFinding, error)) {
		t := time.Now()
		findings, err := fn()
		dur := time.Since(t)
		if err != nil {
			// Synthesize a single error finding before the
			// roll-up runs so Counts stays consistent with
			// len(Findings). The check summary's status is
			// pinned to error since the runner only emits a
			// finding when the check itself failed.
			findings = append(findings, doctorFinding{
				Check:    name,
				Severity: doctorSeverityError,
				Message:  "check failed",
				Detail:   err.Error(),
			})
		}
		sum := doctorCheckSum{
			Name:       name,
			Status:     maxSeverity(findings),
			DurationMS: dur.Milliseconds(),
			Notes:      len(findings),
		}
		report.Counts.OK += rollupOK(findings)
		report.Counts.Warn += rollupWarn(findings)
		report.Counts.Error += rollupError(findings)
		report.Findings = append(report.Findings, findings...)
		report.Checks = append(report.Checks, sum)
	}

	runCheck(doctorCheckSymlink, func() ([]doctorFinding, error) {
		return checkSymlink(deps)
	})
	runCheck(doctorCheckBundle, func() ([]doctorFinding, error) {
		return checkBundle(deps)
	})
	runCheck(doctorCheckLockstep, func() ([]doctorFinding, error) {
		return checkLockstep(deps)
	})
	runCheck(doctorCheckNodes, func() ([]doctorFinding, error) {
		return checkNodes(ctx, deps)
	})
	runCheck(doctorCheckBundleOrphans, func() ([]doctorFinding, error) {
		return checkBundleOrphans(ctx, deps)
	})
	// node-hashes is --deep-only. When the flag is unset, don't
	// append the per-check summary at all — the JSON / text
	// output should clearly show the check was not run. apid
	// admin observers (PR-X) read the checks array as the
	// authoritative source for "what did we run", so a skipped
	// check must be absent, not present-with-zero-findings.
	if deps.deep {
		runCheck(doctorCheckNodeHashes, func() ([]doctorFinding, error) {
			return checkNodeHashes(ctx, deps)
		})
	}

	// Truncate findings if exceeded the cap. Counts is then
	// RE-DERIVED from the truncated slice so the wire shape
	// stays consistent: len(Findings) == sum(Counts).
	if len(report.Findings) > doctorFindingsCap {
		dropped := len(report.Findings) - doctorFindingsCap
		report.Findings = report.Findings[:doctorFindingsCap]
		report.Findings = append(report.Findings, doctorFinding{
			Check:    doctorCheckTruncate,
			Severity: doctorSeverityError,
			Message:  fmt.Sprintf("findings truncated at %d; use --node / --release to narrow", doctorFindingsCap),
			Detail:   fmt.Sprintf("dropped %d additional findings", dropped),
		})
	}
	report.Counts.OK = 0
	report.Counts.Warn = 0
	report.Counts.Error = 0
	for _, f := range report.Findings {
		switch f.Severity {
		case doctorSeverityOK:
			report.Counts.OK++
		case doctorSeverityWarn:
			report.Counts.Warn++
		case doctorSeverityError:
			report.Counts.Error++
		}
	}
	report.Counts.Total = report.Counts.OK + report.Counts.Warn + report.Counts.Error
	report.FinishedAt = time.Now()
	report.Healthy = report.Counts.Error == 0
	return report
}

// checkSymlink reads /opt/faas/current. Missing → warn (fresh box
// with no install yet); broken → error; valid → pop deps.currentGitSHA
// and emit one OK finding.
func checkSymlink(deps *doctorDeps) ([]doctorFinding, error) {
	gitSHA, err := releaseinstall.CurrentGitSHA(deps.releasesRoot)
	if err != nil {
		//nolint:nilerr // err is converted to a finding; runner does not need to see it.
		return []doctorFinding{{
			Check:    doctorCheckSymlink,
			Severity: doctorSeverityError,
			Message:  "cannot read /opt/faas/current",
			Detail:   err.Error(),
		}}, nil
	}
	if gitSHA == "" {
		// Fresh box; not a drift but worth flagging.
		return []doctorFinding{{
			Check:    doctorCheckSymlink,
			Severity: doctorSeverityWarn,
			Message:  "no active release; /opt/faas/current is missing",
		}}, nil
	}
	deps.currentGitSHA = gitSHA
	return []doctorFinding{{
		Check:    doctorCheckSymlink,
		Severity: doctorSeverityOK,
		Target:   gitSHA,
		Message:  "active release symlink points at " + gitSHA,
	}}, nil
}

// checkBundle reads + verifies the release manifest on disk. The
// currentGitSHA from checkSymlink is the load-bearing anchor —
// without an active release there is nothing to verify.
func checkBundle(deps *doctorDeps) ([]doctorFinding, error) {
	if deps.currentGitSHA == "" {
		return []doctorFinding{{
			Check:    doctorCheckBundle,
			Severity: doctorSeverityWarn,
			Message:  "skipped: no active release symlink",
		}}, nil
	}
	m, err := releaseinstall.Read(deps.releasesRoot, deps.currentGitSHA)
	if err != nil {
		//nolint:nilerr // err is converted to a finding; runner does not need to see it.
		return []doctorFinding{{
			Check:    doctorCheckBundle,
			Severity: doctorSeverityError,
			Target:   deps.currentGitSHA,
			Message:  "manifest read failed",
			Detail:   err.Error(),
		}}, nil
	}
	if err := releaseinstall.Verify(deps.releasesRoot, m); err != nil {
		//nolint:nilerr // err is converted to a finding; runner does not need to see it.
		return []doctorFinding{{
			Check:    doctorCheckBundle,
			Severity: doctorSeverityError,
			Target:   deps.currentGitSHA,
			Message:  "manifest verify failed",
			Detail:   err.Error(),
		}}, nil
	}
	return []doctorFinding{{
		Check:    doctorCheckBundle,
		Severity: doctorSeverityOK,
		Target:   deps.currentGitSHA,
		Message:  "manifest read + verify OK",
	}}, nil
}

// checkLockstep confirms manifest.daemon_hashes has one entry per
// catalog daemon. Mirrors ValidateManifest's invariant; surfacing
// it separately so a bundle built with a stale renderer PR is
// flagged distinctly from a bin/ mismatch.
func checkLockstep(deps *doctorDeps) ([]doctorFinding, error) {
	if deps.currentGitSHA == "" {
		return []doctorFinding{{
			Check:    doctorCheckLockstep,
			Severity: doctorSeverityWarn,
			Message:  "skipped: no active release symlink",
		}}, nil
	}
	m, err := releaseinstall.Read(deps.releasesRoot, deps.currentGitSHA)
	if err != nil {
		//nolint:nilerr // err is converted to a finding; runner does not need to see it.
		return []doctorFinding{{
			Check:    doctorCheckLockstep,
			Severity: doctorSeverityError,
			Target:   deps.currentGitSHA,
			Message:  "manifest read failed",
			Detail:   err.Error(),
		}}, nil
	}
	catalog := len(catalogHostKeys())
	if got := len(m.DaemonHashes); got != catalog {
		return []doctorFinding{{
			Check:    doctorCheckLockstep,
			Severity: doctorSeverityError,
			Target:   deps.currentGitSHA,
			Message:  fmt.Sprintf("manifest.daemon_hashes has %d entries, want %d", got, catalog),
		}}, nil
	}
	return []doctorFinding{{
		Check:    doctorCheckLockstep,
		Severity: doctorSeverityOK,
		Target:   deps.currentGitSHA,
		Message:  fmt.Sprintf("manifest covers all %d catalog daemons", catalog),
	}}, nil
}

// checkNodes walks ListComputeNodes and flags per-node drift:
// release_id missing / malformed / pointing at a release_bundles
// row that doesn't exist; manifest_hash drift on the active release.
// Honors --node and --release filters.
func checkNodes(ctx context.Context, deps *doctorDeps) ([]doctorFinding, error) {
	if deps.store == nil {
		// DB down — surface one warn and skip. The deep/--fail-on
		// escalation is handled by cmdDoctorDispatch.
		return []doctorFinding{{
			Check:    doctorCheckDB,
			Severity: doctorSeverityWarn,
			Message:  "FAAS_PG_DSN not set; skipping nodes + bundle-orphans",
		}}, nil
	}

	// Use the shared pre-load from cmdDoctorDispatch. A nil map
	// means the pre-load failed (deps.store.ListAllBundles err'd);
	// in that case we surface the failure as a hard error so
	// the operator sees the DB read failure rather than a
	// silent skip.
	if deps.bundlesBySHA == nil {
		return nil, fmt.Errorf("release_bundles pre-load missing")
	}

	nodes, err := deps.store.ListComputeNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list compute nodes: %w", err)
	}

	var findings []doctorFinding
	for _, n := range nodes {
		if deps.nodeFilter != "" && n.Name != deps.nodeFilter {
			continue
		}
		// Validity runs BEFORE the --release filter: empty /
		// malformed / orphan release_ids are exactly the drift
		// the check exists to surface, and an operator
		// triaging a specific SHA must see those defects.
		// release_id sanity.
		if n.ReleaseID == "" {
			findings = append(findings, doctorFinding{
				Check:    doctorCheckNodes,
				Severity: doctorSeverityError,
				Target:   n.Name,
				Message:  "compute_nodes row has empty release_id",
			})
			continue
		}
		if !releaseinstall.ValidGitSHA(n.ReleaseID) {
			findings = append(findings, doctorFinding{
				Check:    doctorCheckNodes,
				Severity: doctorSeverityError,
				Target:   n.Name,
				Message:  "compute_nodes.release_id is not 40-char lowercase hex",
				Detail:   n.ReleaseID,
			})
			continue
		}
		// Orphan release_id: points at a row that doesn't exist.
		if _, ok := deps.bundlesBySHA[n.ReleaseID]; !ok {
			findings = append(findings, doctorFinding{
				Check:    doctorCheckNodes,
				Severity: doctorSeverityError,
				Target:   n.Name,
				Message:  "orphan release_id: no release_bundles row for " + n.ReleaseID,
			})
			continue
		}
		// --release filter is applied AFTER the validity
		// checks above. The remaining comparison (manifest_hash
		// vs. active release) only matters for nodes on the
		// filtered release.
		if deps.releaseFilter != "" && n.ReleaseID != deps.releaseFilter {
			continue
		}
		// manifest_hash drift: the row's manifest_hash must match
		// the active release's manifest_hash (when active).
		if deps.currentGitSHA != "" && n.ReleaseID == deps.currentGitSHA && n.ManifestHash != "" {
			bundle, ok := deps.bundlesBySHA[n.ReleaseID]
			if ok && bundle.ManifestHash != n.ManifestHash {
				findings = append(findings, doctorFinding{
					Check:    doctorCheckNodes,
					Severity: doctorSeverityError,
					Target:   n.Name,
					Message:  "compute_nodes.manifest_hash drift",
					Detail:   fmt.Sprintf("got %s, want %s", n.ManifestHash, bundle.ManifestHash),
				})
				continue
			}
		}
	}

	if len(findings) == 0 {
		// Synthesize one OK finding so the JSON report shows the
		// check ran successfully.
		return []doctorFinding{{
			Check:    doctorCheckNodes,
			Severity: doctorSeverityOK,
			Message:  fmt.Sprintf("scanned %d compute_nodes rows", len(nodes)),
		}}, nil
	}
	return findings, nil
}

// checkBundleOrphans walks ListAllBundles and flags any
// applied_at IS NULL bundle whose on-disk tree is gone. A
// recoverable warning — the install path is incomplete but the
// operator can re-run `gregalectl release install` against the SHA.
func checkBundleOrphans(ctx context.Context, deps *doctorDeps) ([]doctorFinding, error) {
	if deps.store == nil {
		return []doctorFinding{{
			Check:    doctorCheckDB,
			Severity: doctorSeverityWarn,
			Message:  "FAAS_PG_DSN not set; skipping bundle-orphans",
		}}, nil
	}
	if deps.bundlesBySHA == nil {
		return nil, fmt.Errorf("release_bundles pre-load missing")
	}
	var findings []doctorFinding
	for _, b := range deps.bundlesBySHA {
		if deps.releaseFilter != "" && b.GitSHA != deps.releaseFilter {
			continue
		}
		// Only orphan unapplied rows that have been swept.
		if b.AppliedAt != nil {
			continue
		}
		ok, err := releaseinstall.IsBundleOnDisk(deps.releasesRoot, b.GitSHA)
		if err != nil {
			findings = append(findings, doctorFinding{
				Check:    doctorCheckBundleOrphans,
				Severity: doctorSeverityWarn,
				Target:   b.GitSHA,
				Message:  "could not stat bundle on disk",
				Detail:   err.Error(),
			})
			continue
		}
		if !ok {
			findings = append(findings, doctorFinding{
				Check:    doctorCheckBundleOrphans,
				Severity: doctorSeverityWarn,
				Target:   b.GitSHA,
				Message:  "unapplied release_bundles row; on-disk tree missing",
			})
		}
	}
	if len(findings) == 0 {
		return []doctorFinding{{
			Check:    doctorCheckBundleOrphans,
			Severity: doctorSeverityOK,
			Message:  fmt.Sprintf("scanned %d release_bundles rows", len(deps.bundlesBySHA)),
		}}, nil
	}
	return findings, nil
}

// checkNodeHashes is the --deep check. Loads each node's
// release_id + manifest_hash, then re-Verify()s the on-disk
// bundle against the current release_bundles row hashes. A
// mismatch means the on-disk binary was tampered with after the
// install — a real drift signal.
func checkNodeHashes(ctx context.Context, deps *doctorDeps) ([]doctorFinding, error) {
	if deps.store == nil {
		// cmdDoctorDispatch blocks --deep + no-DB at the entry
		// gate; defensive guard here keeps the check symmetric.
		return []doctorFinding{{
			Check:    doctorCheckNodeHashes,
			Severity: doctorSeverityError,
			Message:  "FAAS_PG_DSN not set; --deep requires DB",
		}}, nil
	}
	nodes, err := deps.store.ListComputeNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list compute nodes: %w", err)
	}
	// Bundle cache: one per release_id so the heavy re-hash runs
	// at most once per release.
	type bundleVer struct {
		manifest  releaseinstall.Manifest
		verifyErr error
	}
	cache := make(map[string]bundleVer, len(nodes))
	var findings []doctorFinding
	for _, n := range nodes {
		if deps.nodeFilter != "" && n.Name != deps.nodeFilter {
			continue
		}
		if deps.releaseFilter != "" && n.ReleaseID != deps.releaseFilter {
			continue
		}
		if _, ok := cache[n.ReleaseID]; !ok {
			m, err := releaseinstall.Read(deps.releasesRoot, n.ReleaseID)
			if err != nil {
				cache[n.ReleaseID] = bundleVer{verifyErr: err}
			} else {
				verr := releaseinstall.Verify(deps.releasesRoot, m)
				cache[n.ReleaseID] = bundleVer{manifest: m, verifyErr: verr}
			}
		}
		bd := cache[n.ReleaseID]
		if bd.verifyErr != nil {
			findings = append(findings, doctorFinding{
				Check:    doctorCheckNodeHashes,
				Severity: doctorSeverityError,
				Target:   n.Name,
				Message:  "bundle verify failed",
				Detail:   bd.verifyErr.Error(),
			})
			continue
		}
		// Cross-check against the DB row's manifest_hash too.
		// Uses the shared pre-load from cmdDoctorDispatch (one
		// SELECT for the whole fleet) — no per-node GetByGitSHA.
		bundle, ok := deps.bundlesBySHA[n.ReleaseID]
		if !ok {
			findings = append(findings, doctorFinding{
				Check:    doctorCheckNodeHashes,
				Severity: doctorSeverityError,
				Target:   n.Name,
				Message:  "compute_nodes.release_id missing from release_bundles pre-load",
				Detail:   n.ReleaseID,
			})
			continue
		}
		if bundle.ManifestHash != n.ManifestHash {
			findings = append(findings, doctorFinding{
				Check:    doctorCheckNodeHashes,
				Severity: doctorSeverityError,
				Target:   n.Name,
				Message:  "manifest_hash drift",
				Detail:   fmt.Sprintf("compute_nodes=%s release_bundles=%s", n.ManifestHash, bundle.ManifestHash),
			})
		}
	}
	if len(findings) == 0 {
		return []doctorFinding{{
			Check:    doctorCheckNodeHashes,
			Severity: doctorSeverityOK,
			Message:  fmt.Sprintf("deep hash verified for %d nodes", len(nodes)),
		}}, nil
	}
	return findings, nil
}

// emitDoctorReport writes the report to w. JSON mode → indented
// JSON; text mode → human-readable table.
func emitDoctorReport(w io.Writer, r doctorReport) {
	if jsonEnabled() {
		jsonEmit(w, r)
		return
	}
	// Plain text: per-check summary then findings.
	_, _ = fmt.Fprintf(w, "gregalectl doctor: %s\n", releasesRootLabel(r))
	if r.Healthy {
		_, _ = fmt.Fprintf(w, "  → healthy: %d ok, %d warn, %d error\n",
			r.Counts.OK, r.Counts.Warn, r.Counts.Error)
	} else {
		_, _ = fmt.Fprintf(w, "  → drift: %d ok, %d warn, %d error\n",
			r.Counts.OK, r.Counts.Warn, r.Counts.Error)
	}
	for _, c := range r.Checks {
		_, _ = fmt.Fprintf(w, "  - %-18s %-7s %dms (%d findings)\n",
			c.Name, c.Status, c.DurationMS, c.Notes)
	}
	if len(r.Findings) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w, "  findings:")
	for _, f := range r.Findings {
		target := ""
		if f.Target != "" {
			target = " [" + f.Target + "]"
		}
		_, _ = fmt.Fprintf(w, "    %s %s%s: %s\n",
			severityGlyph(f.Severity), f.Check, target, f.Message)
		if f.Detail != "" {
			_, _ = fmt.Fprintf(w, "      %s\n", f.Detail)
		}
	}
}

func releasesRootLabel(r doctorReport) string {
	if r.NodeFilter != "" || r.ReleaseFilter != "" {
		return r.ReleasesRoot + " (filters applied)"
	}
	return r.ReleasesRoot
}

func severityGlyph(s string) string {
	switch s {
	case doctorSeverityOK:
		return "OK"
	case doctorSeverityWarn:
		return "WARN"
	case doctorSeverityError:
		return "ERROR"
	}
	return s
}

// maxSeverity returns the roll-up status for a check. ok < warn <
// error. Empty findings → ok.
func maxSeverity(findings []doctorFinding) string {
	roll := doctorStatusOK
	for _, f := range findings {
		switch f.Severity {
		case doctorSeverityError:
			return doctorStatusError
		case doctorSeverityWarn:
			roll = doctorStatusWarn
		}
	}
	return roll
}

func rollupOK(findings []doctorFinding) int {
	n := 0
	for _, f := range findings {
		if f.Severity == doctorSeverityOK {
			n++
		}
	}
	return n
}

func rollupWarn(findings []doctorFinding) int {
	n := 0
	for _, f := range findings {
		if f.Severity == doctorSeverityWarn {
			n++
		}
	}
	return n
}

func rollupError(findings []doctorFinding) int {
	n := 0
	for _, f := range findings {
		if f.Severity == doctorSeverityError {
			n++
		}
	}
	return n
}

// dispatchDoctor is the const name referenced by main.go +
// commands_completion_test.go. Kept here (not in commands2.go or
// constants.go) so the doctor surface is a single drop-in file.
const dispatchDoctor = "doctor"
