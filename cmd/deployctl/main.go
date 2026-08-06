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
//
// Invocation sites:
//
//	make generate        — `go run ./cmd/deployctl generate`
//	make generate-check  — `go run ./cmd/deployctl check`
//	make generate-diff   — `go run ./cmd/deployctl diff`
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/onebox-faas/faas/pkg/daemonunitspec"
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
	default:
		fmt.Fprintln(os.Stderr, "unknown subcommand:", cmd)
		os.Exit(2)
	}
}

// target describes one place daemon unit files get emitted to.
type target struct {
	dir   string
	label string
	skip  map[string]bool // daemon names to skip for this target
}

// defaultTargets — the three trees systemd unit files live in across
// the platform. `cp-cp` is what the deploy workflow installs on the
// EX44 box; `cp-sys` is the legacy tree + dev VMs; `cp-ans` is the
// ansible `control_plane_service` role drop-in.
var defaultTargets = []target{
	{dir: "deploy/controlplane/systemd", label: "cp-cp (controlplane/systemd)"},
	{dir: "deploy/systemd", label: "cp-sys (legacy/dev)", skip: legacySkips()},
	{dir: "deploy/ansible/roles/control_plane_service/files", label: "cp-ans (ansible role)", skip: ansibleRoleSkips()},
}

// ansibleRoleSkips: the ansible control_plane_service role only ships
// 4 of the 8 daemons today (apid, imaged, meterd, schedd). vmmd +
// gatewayd-internal + gatewayd-public + githubd are NOT shipped by
// this role. Widening the role to all 8 is a separate ops change.
func ansibleRoleSkips() map[string]bool {
	return map[string]bool{
		"gatewayd-public":   true,
		"gatewayd-internal": true,
		"githubd":           true,
		"vmmd":              true,
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
	switch filepath.Clean(p) {
	case filepath.Clean(defaultTargets[0].dir):
		return defaultTargets[0]
	case filepath.Clean(defaultTargets[1].dir):
		return defaultTargets[1]
	case filepath.Clean(defaultTargets[2].dir):
		return defaultTargets[2]
	default:
		return target{dir: p}
	}
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
	return filepath.Clean(t.dir) == filepath.Clean(defaultTargets[0].dir)
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
	return []string{defaultTargets[0].dir, defaultTargets[1].dir, defaultTargets[2].dir}
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
	defer os.RemoveAll(tmp)

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
//   - `-` only in committed: committed file not generated ⇒ NOT a failure
//     (legacy artefacts like faas-gatewayd.service, README.md,
//     pg-basebackup-*, *.toml.example, faas.conf are preserved)
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
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// keep strings import used (target.label strings used in non-Printf paths
// when callers want a label). We reference strings here so future
// expansion of target.label doesn't trip "imported and not used".
var _ = strings.HasPrefix
