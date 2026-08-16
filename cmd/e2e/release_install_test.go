// release_install_test.go — non-metal CI-safe acceptance for the
// `gregale release bundle|install` flow (PR-3 / ADR-110).
//
// This test is the e2e surface for PR-3. It exercises:
//
//   - `gregale release bundle --bin-dir ... --git-sha ... --manifest-hash ...`
//     builds + writes the manifest and INSERTs into release_bundles.
//   - `gregale release install --git-sha ...` flips the
//     /opt/faas/current symlink atomically and stamps applied_at
//     first-write-wins.
//   - A second install is a no-op (the first-write-wins UPDATE
//     returns false).
//   - The on-disk manifest verifies via releaseinstall.Verify
//     (every daemon binary hashes to the recorded value).
//
// Skipped automatically when FAAS_PG_DSN is unset (no Postgres in
// the harness). Uses a temp /opt/faas/releases-root + a fresh
// compute_nodes row + a temp FAAS_PG_DSN.
package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onebox-faas/faas/pkg/manifest"
	"github.com/onebox-faas/faas/pkg/releaseinstall"
)

func TestReleaseInstall_RoundTrip(t *testing.T) {
	dsn := os.Getenv("FAAS_PG_DSN")
	if dsn == "" {
		t.Skip("FAAS_PG_DSN not set; skipping release install e2e")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	// Build a fake bin dir with one binary per catalog daemon.
	workdir := t.TempDir()
	binDir := filepath.Join(workdir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	for _, name := range manifest.SortedHostKeys() {
		body := []byte("fake-" + name)
		if err := os.WriteFile(filepath.Join(binDir, name), body, 0o755); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	// Lay out a temp /opt/faas tree (releases-root + sibling current).
	optRoot := filepath.Join(workdir, "opt", "faas")
	releasesRoot := filepath.Join(optRoot, "releases")
	if err := os.MkdirAll(releasesRoot, 0o755); err != nil {
		t.Fatalf("mkdir releases: %v", err)
	}

	gitSHA := "0123456789abcdef0123456789abcdef01234567"
	manifestHash := "sha256:" + strings.Repeat("a", 64)

	// Drive the CLI as a subprocess so we exercise the public
	// surface (not the in-process Go functions). This catches
	// CLI-only regressions (flag.Parse wiring, exit codes).
	gregalectlBin := buildGregaleCtl(t)
	runCmd := func(args ...string) (int, string, string) {
		cmd := exec.Command(gregalectlBin, args...)
		cmd.Env = append(os.Environ(), "FAAS_PG_DSN="+dsn)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		code := 0
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			} else {
				t.Fatalf("exec: %v", err)
			}
		}
		return code, stdout.String(), stderr.String()
	}

	// 1. bundle
	code, stdout, stderr := runCmd(
		"release", "bundle",
		"--bin-dir="+binDir,
		"--git-sha="+gitSHA,
		"--manifest-hash="+manifestHash,
		"--releases-root="+releasesRoot,
	)
	if code != 0 {
		t.Fatalf("release bundle: exit=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, gitSHA) {
		t.Errorf("release bundle stdout = %q; missing %s", stdout, gitSHA)
	}
	// Manifest must exist on disk.
	mPath := filepath.Join(releasesRoot, gitSHA, releaseinstall.ManifestName)
	if _, err := os.Stat(mPath); err != nil {
		t.Fatalf("manifest not written: %v", err)
	}
	// Verify the manifest against the staged binaries.
	m, err := releaseinstall.Read(releasesRoot, gitSHA)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if err := releaseinstall.Verify(releasesRoot, m); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	// 2. install (first time)
	code, _, stderr = runCmd(
		"release", "install",
		"--git-sha="+gitSHA,
		"--releases-root="+releasesRoot,
		"--node=test-node-release-install",
	)
	if code != 0 {
		t.Fatalf("release install (first): exit=%d stderr=%q", code, stderr)
	}
	// Symlink must point at the git_sha.
	link := filepath.Join(filepath.Dir(releasesRoot), "current")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink %s: %v", link, err)
	}
	if target != gitSHA {
		t.Errorf("symlink target = %q, want %q", target, gitSHA)
	}

	// 3. install (second time) — must be a no-op on the symlink
	// (AtomicFlip is idempotent) and report first_applied=false.
	code, stdout, _ = runCmd(
		"release", "install",
		"--git-sha="+gitSHA,
		"--releases-root="+releasesRoot,
		"--node=test-node-release-install",
		"--json",
	)
	if code != 0 {
		t.Fatalf("release install (second): exit=%d", code)
	}
	var rep releaseInstallReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("decode --json: %v (stdout=%q)", err, stdout)
	}
	if rep.FirstApplied {
		t.Errorf("second install first_applied = true, want false (idempotent)")
	}
	if rep.ComputeNodeID == "" {
		t.Errorf("second install compute_node_id = empty; want uuid from gen_random_uuid() (PR-6 UPSERT)")
	}
	if rep.ComputeNodeError != "" {
		t.Errorf("second install compute_node_error = %q, want empty (UPSERT succeeded)", rep.ComputeNodeError)
	}

	// 4. release_bundles row exists with applied_at non-null.
	store := releaseinstall.NewStore(pool)
	row, err := store.GetByGitSHA(ctx, gitSHA)
	if err != nil {
		t.Fatalf("GetByGitSHA: %v", err)
	}
	if row.AppliedAt == nil {
		t.Errorf("AppliedAt = nil; want non-null after install")
	}
	if row.ID == "" {
		t.Errorf("ID = empty; want uuid from gen_random_uuid()")
	}
	if len(row.DaemonHashes) != len(manifest.SortedHostKeys()) {
		t.Errorf("DaemonHashes len = %d, want %d", len(row.DaemonHashes), len(manifest.SortedHostKeys()))
	}

	// 5. (PR-6) compute_nodes row reflects the install: the
	// per-node release membership must match git_sha + manifest_hash
	// so doctor (PR-4) can compare across the cluster.
	cn, err := store.GetComputeNode(ctx, "test-node-release-install")
	if err != nil {
		t.Fatalf("GetComputeNode: %v", err)
	}
	if cn.ReleaseID != gitSHA {
		t.Errorf("compute_nodes.release_id = %q, want %q", cn.ReleaseID, gitSHA)
	}
	if cn.ManifestHash != manifestHash {
		t.Errorf("compute_nodes.manifest_hash = %q, want %q", cn.ManifestHash, manifestHash)
	}
	if cn.ID == "" {
		t.Errorf("compute_nodes.id = empty; want uuid from gen_random_uuid()")
	}
	// Idempotent re-install: a third install onto the same node
	// must keep the same row id and overwrite with the same values
	// (release_id, manifest_hash unchanged).
	thirdCode, thirdOut, thirdErr := runCmd(
		"release", "install",
		"--git-sha="+gitSHA,
		"--releases-root="+releasesRoot,
		"--node=test-node-release-install",
		"--json",
	)
	if thirdCode != 0 {
		t.Fatalf("release install (third): exit=%d stderr=%q", thirdCode, thirdErr)
	}
	var rep3 releaseInstallReport
	if err := json.Unmarshal([]byte(thirdOut), &rep3); err != nil {
		t.Fatalf("decode --json (third install): %v (stdout=%q)", err, thirdOut)
	}
	if rep3.ComputeNodeID != cn.ID {
		t.Errorf("third install compute_node_id = %q, want %q (idempotent UPSERT)", rep3.ComputeNodeID, cn.ID)
	}
	// 6. (PR-6) Per-name UPSERT: a fresh install onto a DIFFERENT
	// node name must create a SECOND compute_nodes row, not collide
	// on the first. Proves the UPSERT is keyed by name.
	altNode := "test-node-release-install-third"
	code, _, stderr = runCmd(
		"release", "install",
		"--git-sha="+gitSHA,
		"--releases-root="+releasesRoot,
		"--node="+altNode,
		"--json",
	)
	if code != 0 {
		t.Fatalf("release install (alt-node): exit=%d stderr=%q", code, stderr)
	}
	cn2, err := store.GetComputeNode(ctx, altNode)
	if err != nil {
		t.Fatalf("GetComputeNode (alt node): %v", err)
	}
	if cn2.ID == cn.ID {
		t.Errorf("alt-node compute_nodes.id = %q, want different from first node's %q (UPSERT is per-name)",
			cn2.ID, cn.ID)
	}
	if cn2.ReleaseID != gitSHA || cn2.ManifestHash != manifestHash {
		t.Errorf("alt-node release_id=%q manifest_hash=%q, want %q / %q",
			cn2.ReleaseID, cn2.ManifestHash, gitSHA, manifestHash)
	}

	// Cleanup: delete the release_bundles row + both compute_nodes
	// rows so reruns are deterministic.
	if _, err := pool.Exec(ctx, `delete from release_bundles where git_sha = $1`, gitSHA); err != nil {
		t.Logf("cleanup: delete release_bundles: %v", err)
	}
	if _, err := pool.Exec(ctx, `delete from compute_nodes where name in ($1, $2)`,
		"test-node-release-install", altNode); err != nil {
		t.Logf("cleanup: delete compute_nodes: %v", err)
	}
}

// buildGregaleCtl builds ./cmd/gregalectl (the operator-side CLI,
// PR-6.5) into a temp binary. Kept as a per-test build rather than
// a TestMain cache to dodge the pre-existing harness's tight
// coupling to the apid/vmmd pair.
func buildGregaleCtl(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "gregalectl")
	wd, _ := os.Getwd()
	root := wd
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		root = filepath.Dir(root)
	}
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/gregalectl")
	cmd.Dir = root
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build gregalectl: %v\n%s", err, out.String())
	}
	return bin
}

// releaseInstallReport mirrors cmd/gregale/commands_release.go
// without importing it (cmd/gregale is a main package; cmd/e2e is
// a test package). The shape is the same.
//
// PR-6 added ComputeNodeID + ComputeNodeError fields for the
// per-node release membership UPSERT (issue #911 / ADR-110).
type releaseInstallReport struct {
	GitSHA           string `json:"git_sha"`
	FirstApplied     bool   `json:"first_applied"`
	Node             string `json:"node"`
	ComputeNodeID    string `json:"compute_node_id"`
	ComputeNodeError string `json:"compute_node_error"`
}
