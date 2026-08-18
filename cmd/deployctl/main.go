// Command deployctl drives the systemd unit + daemons.json generator
// that powers DEPLOY-2 (issue #649). One Go source of truth —
// pkg/daemonunitspec — emits the 8 production daemon unit files into
// the three deploy trees + the cp-cp-only faas-cp.slice + the
// cd-controlplane workflow's daemons.json inventory.
//
// Subcommands:
//
//	generate [dirs...]   write regenerated units + daemons.json
//	check [dirs...]      regenerate to a tempdir, assert byte equality
//	                     against the committed files; exit 1 on drift
//	diff [dirs...]        like check, but prints the result to stdout
//	bundle-create <root> <release-id> <commit-sha> <target>
//	                      write and verify an immutable release manifest
//	bundle-check <root>   verify the manifest and every release file
//	migration-dry-run <release-id>
//	                      report host migration actions without mutating state
//	legacy-import <release-id> <commit-sha>
//	                      copy legacy binaries into a verified release baseline
//
// Invocation sites:
//
//	make generate        — `go run ./cmd/deployctl generate`
//	make generate-check  — `go run ./cmd/deployctl check`
//	make generate-diff   — `go run ./cmd/deployctl diff`
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/onebox-faas/faas/pkg/daemonunitspec"
	"github.com/onebox-faas/faas/pkg/deploycontroller"
	"github.com/onebox-faas/faas/pkg/releasebundle"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: deployctl <generate|check|diff> [dirs...]")
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	switch cmd {
	case "generate":
		if err := runGenerate(args); err != nil {
			fmt.Fprintln(os.Stderr, "deployctl:", err)
			os.Exit(1)
		}
	case "check":
		if err := runCheck(args, true); err != nil {
			fmt.Fprintln(os.Stderr, "deployctl check:", err)
			os.Exit(1)
		}
	case "diff":
		if err := runCheck(args, false); err != nil {
			fmt.Fprintln(os.Stderr, "deployctl diff:", err)
			os.Exit(1)
		}
	case "bundle-create":
		if err := runBundleCreate(args); err != nil {
			fmt.Fprintln(os.Stderr, "deployctl bundle-create:", err)
			os.Exit(1)
		}
	case "bundle-check":
		if err := runBundleCheck(args); err != nil {
			fmt.Fprintln(os.Stderr, "deployctl bundle-check:", err)
			os.Exit(1)
		}
	case "deploy":
		if err := runDeploy(args); err != nil {
			fmt.Fprintln(os.Stderr, "deployctl deploy:", err)
			os.Exit(1)
		}
	case "migration-dry-run":
		if err := runMigrationDryRun(args); err != nil {
			fmt.Fprintln(os.Stderr, "deployctl migration-dry-run:", err)
			os.Exit(1)
		}
	case "legacy-import":
		if err := runLegacyImport(args); err != nil {
			fmt.Fprintln(os.Stderr, "deployctl legacy-import:", err)
			os.Exit(1)
		}
	case "upgrade-node":
		// ADR-111 image rollout orchestrator. See upgrade.go for the
		// full drain → wait → cloud-rollout → Probe-gate → activate
		// flow. The function is exported as runUpgradeNode to keep the
		// switch statement flat.
		if err := runUpgradeNode(args); err != nil {
			fmt.Fprintln(os.Stderr, "deployctl upgrade-node:", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "unknown subcommand:", cmd)
		os.Exit(2)
	}
}

// target describes one place daemon unit files get emitted to.
type target struct {
	dir  string
	skip map[string]bool // daemon names to skip for this target
}

// cpcpIndex is the slot of the cp-cp target inside defaultTargets.
// The deploy workflow installs cp-cp on the EX44 box; cp-sys is the
// legacy + dev-VM tree; cp-ans is the ansible control_plane_service
// role drop-in. cp-cp is index 0 by convention — it ships the most
// daemons (all 8) plus faas-cp.slice, and `make generate` walks the
// slice in order so it emits first.
const cpcpIndex = 0

// defaultTargets — the trees systemd unit files live in across the
// platform. Index 0 is cp-cp (see cpcpIndex); index 1 is the legacy
// tree + dev VMs; index 2+ are the per-box ansible role drop-ins
// (Gate-B PR-2: split into control_plane_service, githubd_service,
// compute_only_service so each role ships only its own daemons).
//
// PR-1 (issue #911 / ADR-110): index 0 still targets the v1 cp-cp
// tree (deploy/controlplane/systemd/) so the `make generate` /
// `make generate-check` gate continues to function. The cd-controlplane
// workflow (CD pipeline) no longer reads from this tree — it walks the
// per-role files/ paths (defaultTargets[2..]). Phase 2 (after PR-X
// `gregale secrets init` lands) deletes the v1 tree; the cpcpIndex slot
// will then rebind to a v2 path. The tombstone RETIRED.md in
// deploy/controlplane/ explains the v1 → v2 mapping for operators.
//
// Mega-PR-C: index 0's `skip: cpcpSkipOnlyBuilderd` ensures the new
// builderd registry entry does NOT emit into the v1 tombstone. The
// tombstone is a v1 snapshot — adding a post-v1 daemon to it would
// (a) leave an untracked faas-builderd.service after every
// `make generate`, breaking `make generate-check`, and (b) confuse
// the v1→v2 operator narrative.
var defaultTargets = []target{
	{dir: "deploy/controlplane/systemd", skip: cpcpSkipOnlyBuilderd()},
	{dir: "deploy/systemd", skip: legacySkips()},
	{dir: "deploy/ansible/roles/control_plane_service/files", skip: ansibleRoleSkips()},
	{dir: "deploy/ansible/roles/githubd_service/files", skip: githubdOnlySkips()},
	{dir: "deploy/ansible/roles/compute_only_service/files", skip: computeOnlySkips()},
}

// cpcpSkipOnlyBuilderd: the v1 cp-cp tombstone (deploy/controlplane/systemd)
// is a frozen snapshot of the EX44-era daemon set. Daemons that
// joined AFTER the tombstone was retired (Mega-PR-C: builderd) get
// skipped so they emit only into the modern trees. The tombstone's
// 8 v1 unit files stay byte-identical under `make generate-check`.
func cpcpSkipOnlyBuilderd() map[string]bool {
	return map[string]bool{
		"builderd": true,
	}
}

// ansibleRoleSkips: the ansible control_plane_service role only ships
// 3 of the 8 daemons today (apid, meterd, schedd). imaged moved to
// compute_only_service in Gate-B PR-2; vmmd + gatewayd-internal +
// gatewayd-public + githubd + builderd are NOT shipped by this role
// (builderd lives on fsn-2 via builderd_service). Widening the role
// to all 8 is a separate ops change.
func ansibleRoleSkips() map[string]bool {
	return map[string]bool{
		"githubd":           true,
		"vmmd":              true,
		"builderd":          true,
		"imaged":            true,
		"gatewayd-public":   true,
		"gatewayd-internal": true,
	}
}

// githubdOnlySkips: githubd_service is single-daemon (Gate-B PR-2).
// Every daemon other than githubd is skipped so the role's files/
// tree only carries faas-githubd.service + githubd.toml.example.
// builderd lives on fsn-2 via its own role, so it's skipped here too.
func githubdOnlySkips() map[string]bool {
	return map[string]bool{
		"apid":              true,
		"schedd":            true,
		"meterd":            true,
		"imaged":            true,
		"vmmd":              true,
		"builderd":          true,
		"gatewayd-public":   true,
		"gatewayd-internal": true,
	}
}

// computeOnlySkips: compute_only_service ships imaged only (Gate-B
// PR-2). vmmd + gatewayd-internal + builderd have their own ansible
// roles (Mega-PR-C added builderd_service); the registry adds their
// units so the deployctl generator skips them in this tree's emit.
func computeOnlySkips() map[string]bool {
	return map[string]bool{
		"apid":              true,
		"schedd":            true,
		"meterd":            true,
		"githubd":           true,
		"vmmd":              true,
		"builderd":          true,
		"gatewayd-public":   true,
		"gatewayd-internal": true,
	}
}

// legacySkips: deploy/systemd/ exists for legacy + dev VMs; doesn't
// ship githubd or meterd (those only exist on cp-cp today).
func legacySkips() map[string]bool {
	return map[string]bool{
		"githubd": true,
		"meterd":  true,
	}
}

// targetFor returns the target spec for a known source dir. Unknown
// paths get an empty skip set (every daemon emitted). The skip-set
// drives which daemons the generator writes; it's the source-dir's
// identity, not the destination's.
func targetFor(p string) target {
	cleaned := filepath.Clean(p)
	for _, t := range defaultTargets {
		if filepath.Clean(t.dir) == cleaned {
			return t
		}
	}
	return target{dir: p}
}

// targetsFor returns the target spec for each dir in `dirs`, in order.
// Used by runCheck so the per-source-dir skip-set travels to the
// regenerated tmpdir rather than getting lost when the destination
// path is `tmp/tree-N` instead of the canonical source dir.
func targetsFor(dirs []string) []target {
	out := make([]target, len(dirs))
	for i, d := range dirs {
		out[i] = targetFor(d)
	}
	return out
}

// isCPCP reports whether t is the cp-cp target. Comparing resolved
// target structs (rather than dir path strings) survives the runCheck
// remap where `d` is `tmp/tree-0` and t.dir is the source-dir string.
func isCPCP(t target) bool {
	return filepath.Clean(t.dir) == filepath.Clean(defaultTargets[cpcpIndex].dir)
}

// runGenerate writes units + daemons.json to the named target dirs.
// If no dirs are given, all three targets + daemons.json.
func runGenerate(args []string) error {
	if len(args) == 0 {
		args = targetDirs()
	}
	return generateTo(targetsFor(args), args, "deploy/etc/daemons.json")
}

func targetDirs() []string {
	dirs := make([]string, len(defaultTargets))
	for i, t := range defaultTargets {
		dirs[i] = t.dir
	}
	return dirs
}

func runBundleCreate(args []string) error {
	if len(args) != 4 {
		return fmt.Errorf("usage: deployctl bundle-create <root> <release-id> <commit-sha> <target>")
	}
	manifest, err := releasebundle.Build(args[0], args[1], args[2], args[3], time.Now().UTC())
	if err != nil {
		return err
	}
	if err := releasebundle.Write(args[0], manifest); err != nil {
		return err
	}
	if err := releasebundle.Verify(args[0], manifest); err != nil {
		return err
	}
	fmt.Printf("release bundle %s verified (%d files)\n", manifest.ReleaseID, len(manifest.Files))
	return nil
}

func runBundleCheck(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: deployctl bundle-check <root>")
	}
	manifest, err := releasebundle.Read(args[0])
	if err != nil {
		return err
	}
	if err := releasebundle.Verify(args[0], manifest); err != nil {
		return err
	}
	fmt.Printf("release bundle %s verified (%d files)\n", manifest.ReleaseID, len(manifest.Files))
	return nil
}

func runMigrationDryRun(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: deployctl migration-dry-run <release-id>")
	}
	report, err := deploycontroller.DryRun(deploycontroller.Config{
		ReleasesRoot: "/opt/faas/releases",
		CurrentPath:  "/opt/faas/current",
		LockPath:     "/run/lock/faas-deploy.lock",
	}, args[0])
	if err != nil {
		return err
	}
	fmt.Printf("release: %s\ncurrent: %s\nrollback available: %t\nlegacy binaries: %t\nlegacy source: %t\n", report.ReleaseID, report.CurrentTarget, report.HasPreviousRelease, report.LegacyBinDir, report.LegacySourceDir)
	for _, check := range report.RequiredPaths {
		fmt.Printf("path %s: exists=%t (%s)\n", check.Path, check.Exists, check.Reason)
	}
	for _, path := range report.StaleScratchFiles {
		fmt.Printf("stale scratch candidate: %s\n", path)
	}
	for _, warning := range report.Warnings {
		fmt.Printf("warning: %s\n", warning)
	}
	for _, action := range report.Actions {
		fmt.Printf("action: %s\n", action)
	}
	return nil
}

func runLegacyImport(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: deployctl legacy-import <release-id> <commit-sha>")
	}
	manifest, err := deploycontroller.ImportLegacyBin("/opt/faas/bin", "/opt/faas/releases", args[0], args[1], time.Now().UTC())
	if err != nil {
		return err
	}
	fmt.Printf("legacy release %s imported and verified (%d files); current pointer unchanged\n", manifest.ReleaseID, len(manifest.Files))
	return nil
}

func runDeploy(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: deployctl deploy <release-id>")
	}
	runtime := defaultHostRuntime()
	controller, err := deploycontroller.New(deploycontroller.Config{
		ReleasesRoot: "/opt/faas/releases",
		CurrentPath:  "/opt/faas/current",
		LockPath:     "/run/lock/faas-deploy.lock",
	}, runtime)
	if err != nil {
		return err
	}
	return controller.Deploy(context.Background(), args[0])
}

// generateTo is the core: write unit files + slice + JSON to named
// dirs + daemonsPath. Each entry in `ts` is the resolved target
// (with its skip-set) for the corresponding dir in `dirs`. The cp-cp
// slice is emitted into whichever dir is the cp-cp target.
func generateTo(ts []target, dirs []string, daemonsPath string) error {
	cpcpDir := ""
	for i, d := range dirs {
		t := ts[i]
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
		for _, entry := range daemonunitspec.Registry {
			if t.skip[entry.Name] {
				continue
			}
			path := filepath.Join(d, "faas-"+entry.Name+".service")
			if err := os.WriteFile(path, entry.Unit().Render(), 0o644); err != nil {
				return fmt.Errorf("write %s: %w", path, err)
			}
		}
		if isCPCP(t) {
			cpcpDir = d
		}
	}
	// faas-cp.slice lives outside the Registry (it's the wrapper,
	// not a member). Only cp-cp ships it.
	if err := writeCPSlice(cpcpDir); err != nil {
		return err
	}
	return writeDaemonsJSON(daemonsPath)
}

// writeCPSlice writes faas-cp.slice into the named dir. Empty dir is
// a no-op (no cp-cp target in `dirs`); generator callers pass the
// actual cp-cp path (committed or tmpdir), not the registry literal.
func writeCPSlice(cpcpDir string) error {
	if cpcpDir == "" {
		return nil
	}
	body := "[Unit]\n" +
		"Description=onebox-faas control plane (DO dev deployment)\n" +
		"\n" +
		"[Slice]\n" +
		"MemoryMax=3G\n"
	path := filepath.Join(cpcpDir, "faas-cp.slice")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// DaemonsJSON is the shape emitted to deploy/etc/daemons.json.
type DaemonsJSON struct {
	Critical   []string `json:"critical"`
	BestEffort []string `json:"best_effort"`
}

func writeDaemonsJSON(path string) error {
	dj := DaemonsJSON{}
	for _, entry := range daemonunitspec.Registry {
		if entry.Critical {
			dj.Critical = append(dj.Critical, entry.Name)
		} else {
			dj.BestEffort = append(dj.BestEffort, entry.Name)
		}
	}
	sort.Strings(dj.Critical)
	sort.Strings(dj.BestEffort)
	body, err := json.MarshalIndent(dj, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o644)
}

// runCheck regenerates to a tempdir and compares against committed
// files. `quiet` true ⇒ exit 1 on drift without printing; `quiet` false
// (the `diff` subcommand) prints a focused diff message before exiting.
func runCheck(args []string, quiet bool) error {
	dirs := args
	if len(dirs) == 0 {
		dirs = targetDirs()
	}

	tmp, err := os.MkdirTemp("", "deployctl-check-")
	if err != nil {
		return err
	}
	defer func() {
		// Best-effort cleanup; on Windows the RemoveAll can race with
		// lingering read handles from filepath.Walk, but the OS
		// eventually reaps the dir. Failure here is non-fatal — the
		// tmp dir name is uniq and the OS clears /tmp on reboot.
		_ = os.RemoveAll(tmp)
	}()

	// Resolve skip-sets BEFORE remapping to tmpdirs — the source-dir
	// identity is what drives which daemons each tree ships, and the
	// tmpdir path no longer encodes it.
	ts := targetsFor(dirs)

	// Per-target tmpdir suffix: two of the three default targets
	// end in `systemd` (cp-cp + cp-sys), so `filepath.Base(d)` would
	// collide and the cp-cp regeneration would clobber cp-sys. The
	// suffix must be unique per `dir` regardless of trailing name.
	tmpDirs := make([]string, len(dirs))
	for i := range dirs {
		tmpDirs[i] = filepath.Join(tmp, fmt.Sprintf("tree-%d", i))
	}
	tmpJSON := filepath.Join(tmp, "daemons.json")
	if err := generateTo(ts, tmpDirs, tmpJSON); err != nil {
		return err
	}

	drift := 0
	for i, d := range dirs {
		if err := compareTrees(d, tmpDirs[i], quiet); err != nil {
			drift++
			if !quiet {
				fmt.Fprintln(os.Stderr, err)
			}
		}
	}
	committedJSON := "deploy/etc/daemons.json"
	if _, err := os.Stat(committedJSON); err == nil {
		if err := compareFiles(committedJSON, tmpJSON, quiet); err != nil {
			drift++
			if !quiet {
				fmt.Fprintln(os.Stderr, err)
			}
		}
	}

	if drift > 0 {
		if quiet {
			return fmt.Errorf("%d drifted paths; run 'make generate' and commit the result", drift)
		}
		return fmt.Errorf("%d drifted paths", drift)
	}
	if !quiet {
		fmt.Println("deployctl diff: no drift")
	}
	return nil
}

// compareTrees walks d (committed) and td (regenerated), comparing each
// file by name + bytes. The gate's policy:
//
//   - `~` drifted: generated file's bytes differ from committed ⇒ FAIL
//   - `+` only in regenerated: generated file missing from committed ⇒ FAIL
//   - `-` only in committed: committed file not generated ⇒ NOT a failure.
//     Legacy artefacts (README.md, pg-basebackup-*, *.toml.example,
//     faas.conf) are preserved on purpose — removing them is a separate ops
//     change, not a generator regression. Preserved artefacts do NOT trip
//     the gate.
//
// PR-1 (issue #911 / ADR-110): the v1 cp-cp tree
// (deploy/controlplane/systemd/) is now a tombstone; the CD pipeline no
// longer reads from it. Phase 2 (after PR-X) deletes it. Until then,
// `make generate-check` keeps comparing the regenerated tmpdir against
// the committed cp-cp tree so a daemonunitspec change cannot silently
// drift.
//
// Reports the names that drift; `quiet` controls print/no-print.
func compareTrees(committed, regenerated string, quiet bool) error {
	committedFiles, err := readFiles(committed)
	if err != nil {
		return fmt.Errorf("walk %s: %w", committed, err)
	}
	regeneratedFiles, err := readFiles(regenerated)
	if err != nil {
		return fmt.Errorf("walk %s: %w", regenerated, err)
	}

	var changed bool
	for name, ab := range committedFiles {
		bb, ok := regeneratedFiles[name]
		if !ok {
			// Preserved legacy artefact — not a failure.
			if !quiet {
				fmt.Printf("- %s (preserved)\n", filepath.Join(committed, name))
			}
			continue
		}
		if !bytesEqual(ab, bb) {
			changed = true
			if !quiet {
				fmt.Printf("~ %s (drifted)\n", filepath.Join(committed, name))
			}
		}
	}
	for name := range regeneratedFiles {
		if _, ok := committedFiles[name]; !ok {
			changed = true
			if !quiet {
				fmt.Printf("+ %s (only in regenerated)\n", filepath.Join(committed, name))
			}
		}
	}
	if changed {
		return fmt.Errorf("drift under %s", committed)
	}
	return nil
}

// compareFiles compares a single flat file pair.
func compareFiles(committed, regenerated string, quiet bool) error {
	a, err := os.ReadFile(committed)
	if err != nil {
		return fmt.Errorf("read %s: %w", committed, err)
	}
	b, err := os.ReadFile(regenerated)
	if err != nil {
		return fmt.Errorf("read %s: %w", regenerated, err)
	}
	if !bytesEqual(a, b) {
		if !quiet {
			fmt.Printf("~ %s (drifted)\n", committed)
		}
		return fmt.Errorf("daemons.json drifted")
	}
	return nil
}

func readFiles(root string) (map[string][]byte, error) {
	out := map[string][]byte{}
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		out[rel] = b
		return nil
	})
	return out, err
}

func bytesEqual(a, b []byte) bool {
	return bytes.Equal(a, b)
}
