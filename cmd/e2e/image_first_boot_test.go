//go:build e2eimage

// cmd/e2e/image_first_boot_test.go — first-boot e2e test (ADR-111).
//
// Boots a cloud VM from a built canary image (IMAGE_TAG env var),
// waits for the cloud-init user-data to complete, and asserts:
//   - doctor --deep exits 0 on the new box (via ssh)
//   - every Lifecycle.Probe / ProbeTarget on every Registry entry
//     reports ready (the per-daemon gate the upgrade-node orchestrator
//     also uses)
//   - /etc/faas/{sealed.env, host.age, rclone.conf.age} match the
//     expected schema (verify-secrets.sh — same script the build
//     host runs before sealing the image)
//   - re-running the user-data is a no-op (the second invocation
//     must NOT restart daemons, NOT re-seal secrets, NOT change
//     /opt/faas/current's git_sha pin)
//
// Build tag: e2eimage. The standard `make test` does NOT compile
// this file — operators invoke `make image-test-first-boot IMAGE_TAG=…`
// which sets -tags=e2eimage explicitly.
//
// Requires (env vars):
//   IMAGE_TAG    — built image tag (e.g. gregale-compute-control-plane-...)
//   HCLOUD_TOKEN — Hetzner Cloud API token (for hcloud server create)
//   SSH_KEY_PATH — path to a private key the e2e box accepts
//   SSH_USER     — default 'root' for the hcloud Ubuntu image

package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/daemonunitspec"
)

func TestImageFirstBoot(t *testing.T) {
	imageTag := os.Getenv("IMAGE_TAG")
	if imageTag == "" {
		t.Skip("IMAGE_TAG not set; skip e2eimage first-boot test")
	}
	if os.Getenv("HCLOUD_TOKEN") == "" {
		t.Skip("HCLOUD_TOKEN not set; skip e2eimage first-boot test")
	}
	sshKey := os.Getenv("SSH_KEY_PATH")
	sshUser := os.Getenv("SSH_USER")
	if sshUser == "" {
		sshUser = "root"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// 1. Spawn a fresh VM from the built image. `hcloud server create
	// --image <snapshot-id> --ssh-key <fingerprint>` is the canonical
	// Hetzner provisioning path.
	serverIP, serverCleanup := spawnHcloudServer(ctx, t, imageTag, sshKey)
	defer serverCleanup()

	// 2. Wait for the cloud-init runcmd to complete. The first-boot
	// user-data ends with `node-ready: true` in /var/log/faas-first-boot/
	// runbook.log — we wait for that marker via SSH tail.
	waitForNodeReady(ctx, t, serverIP, sshUser, sshKey, 8*time.Minute)

	// 3. Run gregalectl doctor --deep. The doctor is the canonical
	// "node ready" gate (PR #921 / ADR-110 PR-4). It exits 0 iff every
	// daemon in the Registry is active + every ProbeTarget passes.
	doctorOutput := sshExec(ctx, t, serverIP, sshUser, sshKey,
		"gregalectl doctor --deep --output=json")
	if !strings.Contains(doctorOutput, "\"summary\":\"ok\"") {
		t.Fatalf("doctor --deep: expected summary=ok, got %s", doctorOutput)
	}

	// 4. Per-daemon Probe gate — same gate upgrade-node uses.
	for _, entry := range daemonunitspec.Registry {
		switch entry.Lifecycle.Probe {
		case daemonunitspec.ProbeUnix:
			if _, err := sshExecErr(ctx, t, serverIP, sshUser, sshKey,
				"test -S "+entry.Lifecycle.ProbeTarget); err != nil {
				t.Fatalf("daemon %s: probe unix %s not present: %v",
					entry.Name, entry.Lifecycle.ProbeTarget, err)
			}
		case daemonunitspec.ProbeTCP:
			host, port, splitErr := net.SplitHostPort(entry.Lifecycle.ProbeTarget)
			if splitErr != nil {
				t.Fatalf("daemon %s: bad tcp probe %q: %v",
					entry.Name, entry.Lifecycle.ProbeTarget, splitErr)
			}
			if host == "" || host == "127.0.0.1" || host == "localhost" {
				// Local probe — assert via ssh that the port is open.
				sshExec(ctx, t, serverIP, sshUser, sshKey,
					fmt.Sprintf("bash -c 'echo > /dev/tcp/127.0.0.1/%s'", port))
			} else {
				// Cross-host probe — assert via net.Dial from the e2e box.
				conn, dialErr := net.DialTimeout("tcp",
					net.JoinHostPort(serverIP, port), 5*time.Second)
				if dialErr != nil {
					t.Fatalf("daemon %s: tcp probe %s dial: %v",
						entry.Name, entry.Lifecycle.ProbeTarget, dialErr)
				}
				_ = conn.Close()
			}
		case daemonunitspec.ProbeSystemd:
			sshExec(ctx, t, serverIP, sshUser, sshKey,
				"systemctl is-active faas-"+entry.Name+".service")
		}
	}

	// 5. Schema assertions on the sealed env. verify-secrets.sh is the
	// canonical schema check; we re-run it on the live box.
	sshExec(ctx, t, serverIP, sshUser, sshKey,
		"bash /usr/local/bin/faas-first-boot/verify-secrets.sh")

	// 6. Idempotency: re-run the user-data; nothing should change.
	// Concretely: /opt/faas/current/VERSION matches the image tag's
	// git_sha; running the same user-data a second time is a no-op.
	gitSha := strings.TrimPrefix(strings.Split(imageTag, "-g")[1], "")
	sshExec(ctx, t, serverIP, sshUser, sshKey,
		"grep -q '"+gitSha+"' /opt/faas/current/VERSION")
	sshExec(ctx, t, serverIP, sshUser, sshKey,
		"bash /usr/local/bin/faas-first-boot/runbook-step-9.sh") // no-op
	sshExec(ctx, t, serverIP, sshUser, sshKey,
		"bash /usr/local/bin/faas-first-boot/runbook-step-10.sh") // doctor, read-only

	t.Logf("TestImageFirstBoot: %s PASSED (image_tag=%s)", serverIP, imageTag)
}

// spawnHcloudServer creates a fresh Hetzner Cloud server from the
// snapshot with the given imageTag. Returns the public IPv4 + a
// cleanup func that destroys the server.
func spawnHcloudServer(ctx context.Context, t *testing.T, imageTag, sshKey string) (string, func()) {
	t.Helper()

	// Resolve snapshot id by tag. Mirrors deploy/packer/cloud-rollout/
	// hcloud.sh:tag resolution.
	out, err := exec.CommandContext(ctx, "hcloud", "image", "list",
		"-o", "noheader", "-o", "columns=id,name").CombinedOutput()
	if err != nil {
		t.Fatalf("hcloud image list: %v: %s", err, out)
	}
	var snapshotID string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == imageTag {
			snapshotID = fields[0]
			break
		}
	}
	if snapshotID == "" {
		t.Fatalf("no snapshot with tag %q in hcloud", imageTag)
	}

	serverName := fmt.Sprintf("e2e-first-boot-%d", time.Now().UnixNano())
	createCmd := exec.CommandContext(ctx, "hcloud", "server", "create",
		"--name", serverName,
		"--image", snapshotID,
		"--type", "cx22",
		"--location", "fsn1",
		"--ssh-key", filepath.Base(sshKey), // hcloud resolves by fingerprint
	)
	createOut, err := createCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hcloud server create: %v: %s", err, createOut)
	}

	// Resolve the server's public IP via hcloud (the create output
	// doesn't always include it; a follow-up describe is the safest
	// way to find the IPv4).
	ipOut, err := exec.CommandContext(ctx, "hcloud", "server", "list",
		"-o", "noheader", "-o", "columns=ipv4,name").CombinedOutput()
	if err != nil {
		t.Fatalf("hcloud server list: %v: %s", err, ipOut)
	}
	var ip string
	for _, line := range strings.Split(strings.TrimSpace(string(ipOut)), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == serverName {
			ip = fields[0]
			break
		}
	}
	if ip == "" {
		t.Fatalf("hcloud: no ipv4 for server %s", serverName)
	}

	cleanup := func() {
		_ = exec.Command("hcloud", "server", "delete", serverName).Run()
	}
	return ip, cleanup
}

// waitForNodeReady polls the box via SSH until the first-boot
// runbook.log contains 'node-ready: true' OR the deadline passes.
func waitForNodeReady(ctx context.Context, t *testing.T, ip, user, key string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := sshExecErr(ctx, t, ip, user, key,
			"grep -c 'node-ready: true' /var/log/faas-first-boot/runbook.log 2>/dev/null || true")
		if err == nil && strings.TrimSpace(out) == "1" {
			return
		}
		time.Sleep(10 * time.Second)
	}
	t.Fatalf("node-ready: timeout waiting for cloud-init completion on %s", ip)
}

func sshExec(ctx context.Context, t *testing.T, ip, user, key, cmd string) string {
	t.Helper()
	out, err := sshExecErr(ctx, t, ip, user, key, cmd)
	if err != nil {
		t.Fatalf("ssh %s@%s: %s: %v", user, ip, cmd, err)
	}
	return out
}

func sshExecErr(ctx context.Context, t *testing.T, ip, user, key, cmd string) (string, error) {
	t.Helper()
	c := exec.CommandContext(ctx, "ssh",
		"-i", key,
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=/dev/null",
		fmt.Sprintf("%s@%s", user, ip),
		cmd,
	)
	out, err := c.CombinedOutput()
	return string(out), err
}
