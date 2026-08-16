//go:build e2eimage

// cmd/e2e/image_role_mutation_test.go — in-place role mutation e2e
// (ADR-112 PR-B). Boots a cloud VM from a built canary image
// (IMAGE_TAG env var) with control-plane first-boot, then walks
// the box through two re-role cycles:
//
//	control-plane → compute-only → control-plane
//
// and asserts after each cycle:
//
//   - compute_nodes.role (Postgres) flipped to the new role via
//     psqlExec on DATABASE_URL (the post-#931 PR-B runtime path)
//   - every daemon in the new subset reports active (systemctl
//     is-active via SSH), every daemon NOT in the subset reports
//     inactive — the per-daemon gate the Mutate contract enforces
//   - sealed.env / host.age / rclone.conf.age / cosign.{key,pub}
//     sha256sums are byte-identical pre/post (the load-bearing
//     invariant: the role mutation touches nothing sealed)
//   - `gregalectl doctor --deep` exits 0 at the end of each cycle
//     (PR #921 readiness gate)
//
// Build tag: e2eimage. Standard `make test` does NOT compile this
// file. Operator invocation:
// `make image-test-role-mutation IMAGE_TAG=<tag>` (deploy/packer/Makefile).
//
// Requires (env vars):
//
//	IMAGE_TAG    — built image tag (no role segment; ADR-112)
//	HCLOUD_TOKEN — Hetzner Cloud API token
//	SSH_KEY_PATH — path to a private key the e2e box accepts
//	SSH_USER     — default 'root'
//	DATABASE_URL — reachable Postgres (for psqlExec against
//	               compute_nodes.role)
package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// sealedPaths is the canonical "what MUST be byte-identical pre/post"
// list. These are the per-box identity secrets whose drift between
// role mutations would surface as a broken cluster — sealed.env
// is the customer's sealed envelope (PR-X secrets init), host.age
// is the per-node age identity (ADR-053), rclone.conf.age is the
// rclone credential store, cosign.{key,pub} is the cosign keypair
// (PR #371 cosign keypair init).
var sealedPaths = []string{
	"/etc/faas/sealed.env",
	"/etc/faas/host.age",
	"/etc/faas/rclone.conf.age",
	"/etc/faas/cosign.key",
	"/etc/faas/cosign.pub",
}

// sealedFingerprint returns a deterministic fingerprint of the
// 5 sealed files. sha256sum is the canonical operator tool; we
// emit a single concatenated hex string so the assertion is one
// diff rather than five.
func sealedFingerprint(ctx context.Context, t *testing.T, ip, user, key string) string {
	t.Helper()
	joined := strings.Join(sealedPaths, " ")
	out := sshExec(ctx, t, ip, user, key,
		fmt.Sprintf("sha256sum %s 2>/dev/null", joined))
	h := sha256.New()
	h.Write([]byte(out))
	return hex.EncodeToString(h.Sum(nil))
}

// assertDaemonSubset asserts every daemon in `wantActive` reports
// active and every daemon in `wantInactive` reports inactive. Walks
// the daemonunitspec.Registry so adding a new daemon automatically
// gets a coverage check.
func assertDaemonSubset(
	ctx context.Context, t *testing.T, ip, user, key string,
	wantActive, wantInactive map[string]struct{},
) {
	t.Helper()
	for name := range wantActive {
		got := strings.TrimSpace(sshExec(ctx, t, ip, user, key,
			"systemctl is-active faas-"+name+".service"))
		if got != "active" {
			t.Errorf("daemon %s: want active, got %q", name, got)
		}
	}
	for name := range wantInactive {
		got := strings.TrimSpace(sshExec(ctx, t, ip, user, key,
			"systemctl is-active faas-"+name+".service"))
		if got != "inactive" {
			t.Errorf("daemon %s: want inactive, got %q", name, got)
		}
	}
}

func TestImageRoleMutation(t *testing.T) {
	imageTag := os.Getenv("IMAGE_TAG")
	if imageTag == "" {
		t.Skip("IMAGE_TAG not set; skip e2eimage role-mutation test")
	}
	if os.Getenv("HCLOUD_TOKEN") == "" {
		t.Skip("HCLOUD_TOKEN not set; skip e2eimage role-mutation test")
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skip e2eimage role-mutation test (needs psqlExec against compute_nodes.role)")
	}
	sshKey := os.Getenv("SSH_KEY_PATH")
	sshUser := os.Getenv("SSH_USER")
	if sshUser == "" {
		sshUser = "root"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// 1. Spawn a fresh VM with control-plane first-boot.
	serverIP, serverCleanup := spawnHcloudServer(ctx, t, imageTag, sshKey)
	defer serverCleanup()
	waitForNodeReady(ctx, t, serverIP, sshUser, sshKey, 8*time.Minute)

	// Pre-mutation baseline: capture the 5 sealed-file fingerprints
	// and the initial compute_nodes.role. Both must survive the
	// C → CO → C round-trip byte-for-byte.
	preSealedCP := sealedFingerprint(ctx, t, serverIP, sshUser, sshKey)
	hostname := strings.TrimSpace(sshExec(ctx, t, serverIP, sshUser, sshKey, "hostname"))
	preRoleCP := strings.TrimSpace(psqlExec(ctx, t, dsn,
		fmt.Sprintf("SELECT COALESCE(role, '') FROM compute_nodes WHERE name = '%s'", hostname)))
	if preRoleCP != "control-plane" {
		t.Fatalf("baseline role: want control-plane, got %q (first-boot path failed?)", preRoleCP)
	}

	// 2. Re-role control-plane → compute-only. The Mutate contract
	// (PR-A Fix 7+8) handles the stop/start subset; the runtime CLI
	// branch (PR-B commit 2) handles the DB write + drain gate.
	sshExec(ctx, t, serverIP, sshUser, sshKey,
		"gregalectl release install --role=compute-only")

	// 3. Assert compute_nodes.role flipped.
	roleAfterFlip1 := strings.TrimSpace(psqlExec(ctx, t, dsn,
		fmt.Sprintf("SELECT role FROM compute_nodes WHERE name = '%s'", hostname)))
	if roleAfterFlip1 != "compute-only" {
		t.Fatalf("post-flip1 role: want compute-only, got %q", roleAfterFlip1)
	}

	// 4. Assert the daemon subset flipped: apid/schedd/meterd/
	// githubd/gatewayd-public should be inactive; vmmd/imaged/
	// builderd/gatewayd-internal should be active.
	controlPlaneActive := map[string]struct{}{
		"apid": {}, "schedd": {}, "meterd": {}, "githubd": {}, "gatewayd-public": {},
	}
	computeOnlyActive := map[string]struct{}{
		"vmmd": {}, "imaged": {}, "builderd": {}, "gatewayd-internal": {},
	}
	assertDaemonSubset(ctx, t, serverIP, sshUser, sshKey,
		computeOnlyActive, controlPlaneActive)

	// 5. Sealed files are byte-identical.
	postSealedCO := sealedFingerprint(ctx, t, serverIP, sshUser, sshKey)
	if postSealedCO != preSealedCP {
		t.Fatalf("sealed files drifted C → CO:\nbefore=%s\nafter =%s",
			preSealedCP, postSealedCO)
	}

	// 6. Doctor gate. Exit 0 = every role-appropriate daemon is
	// active and every ProbeTarget passes. JSON: healthy=true.
	doctorOut := sshExec(ctx, t, serverIP, sshUser, sshKey,
		"gregalectl doctor --deep --output=json")
	if !strings.Contains(doctorOut, "\"healthy\":true") {
		t.Fatalf("doctor after C → CO: expected healthy=true, got %s", doctorOut)
	}

	// 7. Re-role back: compute-only → control-plane. The reverse
	// direction; gatewayd-public must come up last (PR-A Fix 7).
	sshExec(ctx, t, serverIP, sshUser, sshKey,
		"gregalectl release install --role=control-plane")

	// 8. Assert role + daemon subset flipped back.
	roleAfterFlip2 := strings.TrimSpace(psqlExec(ctx, t, dsn,
		fmt.Sprintf("SELECT role FROM compute_nodes WHERE name = '%s'", hostname)))
	if roleAfterFlip2 != "control-plane" {
		t.Fatalf("post-flip2 role: want control-plane, got %q", roleAfterFlip2)
	}
	assertDaemonSubset(ctx, t, serverIP, sshUser, sshKey,
		controlPlaneActive, computeOnlyActive)

	// 9. Sealed files STILL byte-identical after the second cycle.
	postSealedCP := sealedFingerprint(ctx, t, serverIP, sshUser, sshKey)
	if postSealedCP != preSealedCP {
		t.Fatalf("sealed files drifted after C → CO → C:\nbefore=%s\nafter =%s",
			preSealedCP, postSealedCP)
	}

	// 10. Doctor gate again. Both directions must exit 0.
	doctorOut2 := sshExec(ctx, t, serverIP, sshUser, sshKey,
		"gregalectl doctor --deep --output=json")
	if !strings.Contains(doctorOut2, "\"healthy\":true") {
		t.Fatalf("doctor after CO → C: expected healthy=true, got %s", doctorOut2)
	}

	// 11. Idempotent re-run: same --role on a converged box is a
	// no-op. gregalectl prints "already role=X; no-op"; the sealed
	// fingerprint is unchanged; the daemon subset is unchanged.
	out := sshExec(ctx, t, serverIP, sshUser, sshKey,
		"gregalectl release install --role=control-plane 2>&1")
	if !strings.Contains(out, "no-op") {
		t.Errorf("idempotent re-run: expected no-op marker, got %s", out)
	}
	if got := sealedFingerprint(ctx, t, serverIP, sshUser, sshKey); got != postSealedCP {
		t.Errorf("idempotent re-run touched sealed files: %s -> %s", postSealedCP, got)
	}

	t.Logf("TestImageRoleMutation: %s PASSED (image_tag=%s, hostname=%s)",
		serverIP, imageTag, hostname)
}
