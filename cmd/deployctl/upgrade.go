// cmd/deployctl/upgrade.go — image-rollout orchestrator (ADR-111).
//
// `make upgrade-node IMAGE_TAG=<tag>` flow:
//   1. gregalectl compute-nodes drain --node <fqdn>
//      → UPDATE compute_nodes SET active=false WHERE name=<fqdn>
//   2. wait MigrateLiveLeaseSeconds (90s, per pkg/api/limits.go) + 5s
//      grace for live instances to land on peers
//   3. signal the cloud-specific image-rollout mechanism (hcloud /
//      amazon-ebs / bare-metal — each is its own .sh wrapper)
//   4. wait for the new VM to come up
//   5. poll every Lifecycle.Probe / ProbeTarget in
//      pkg/daemonunitspec.Registry IN ORDER; fail-closed if any probe
//      reports not-ready past readyTimeout
//   6. UPDATE compute_nodes SET active=true on the node ONLY after
//      every probe passes
//
// The orchestrator reuses the existing waitPath / waitTCP /
// waitSystemdActive helpers from cmd/deployctl/runtime.go:295-299 — no
// new probe code; the gate IS the per-daemon probe.
//
// Per ADR-066 / §14 M9: live-migration is out of scope. The drain-
// then-rollout path is the only supported upgrade mechanism.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/daemonunitspec"
)

// upgradeArgs parses argv (everything after `deployctl upgrade-node`).
// Operator invocation:
//
//   deployctl upgrade-node --image-tag=gregale-compute-control-plane-...
//                          --node=fsn-1
//                          [--cloud=hcloud|amazon-ebs|bare-metal]
//                          [--drain-timeout=120s]
//                          [--ready-timeout=300s]
//                          [--cloud-rollout=/path/to/cloud-specific.sh]
type upgradeArgs struct {
	imageTag     string
	node         string
	cloud        string
	drainTimeout time.Duration
	readyTimeout time.Duration
	cloudRollout string
}

func parseUpgradeArgs(stdout io.Writer, args []string) (*upgradeArgs, error) {
	a := &upgradeArgs{
		drainTimeout: time.Duration(api.MigrateLiveLeaseSeconds)*time.Second + 5*time.Second,
		readyTimeout: 5 * time.Minute,
		cloud:        "hcloud",
	}

	fs := flag.NewFlagSet("upgrade-node", flag.ContinueOnError)
	fs.SetOutput(stdout)
	fs.StringVar(&a.imageTag, "image-tag", "", "image tag to roll out (gregale-compute-{role}-{fc_release}-{kernel_version}-{git_sha})")
	fs.StringVar(&a.node, "node", "", "fqdn of the node being upgraded")
	fs.StringVar(&a.cloud, "cloud", a.cloud, "cloud provider: hcloud|amazon-ebs|bare-metal")
	fs.DurationVar(&a.drainTimeout, "drain-timeout", a.drainTimeout, "time to wait for live instances to land on peers (MigrateLiveLeaseSeconds + 5s grace)")
	fs.DurationVar(&a.readyTimeout, "ready-timeout", a.readyTimeout, "time to wait for every Lifecycle.Probe on every Registry entry to pass")
	fs.StringVar(&a.cloudRollout, "cloud-rollout", "", "path to the cloud-specific rollout shell wrapper (defaults to deploy/packer/cloud-rollout/<cloud>.sh)")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	if a.imageTag == "" {
		return nil, errors.New("--image-tag required (gregale-compute-{role}-...)")
	}
	if a.node == "" {
		return nil, errors.New("--node required (fqdn of the target box)")
	}
	if !strings.HasPrefix(a.imageTag, "gregale-compute-") {
		return nil, fmt.Errorf("--image-tag must satisfy the ADR-111 contract; got %q", a.imageTag)
	}

	return a, nil
}

// runUpgradeNode is the entry point invoked from main.go's switch.
func runUpgradeNode(args []string) error {
	a, err := parseUpgradeArgs(io.Discard, args)
	if err != nil {
		return err
	}

	logger := slog.Default()
	ctx := context.Background()

	logger.Info("upgrade-node: starting",
		"node", a.node,
		"image_tag", a.imageTag,
		"cloud", a.cloud,
	)

	// 1. Drain — UPDATE compute_nodes SET active=false WHERE name=<fqdn>.
	// Delegates to gregalectl compute-nodes drain (PR #914), the operator-
	// facing CLI on the same wire path as `make bootstrap`.
	if err := runDrain(ctx, a); err != nil {
		return fmt.Errorf("drain %s: %w", a.node, err)
	}
	logger.Info("upgrade-node: drained", "node", a.node)

	// 2. Wait MigrateLiveLeaseSeconds + 5s grace for live instances to
	// land on peers. The schedd rebalances naturally once the box is
	// drained (the placement algorithm skips inactive rows). If anything
	// is still on the box after the wait, exit 1 with a loud warning.
	if err := waitForDrain(ctx, a); err != nil {
		return fmt.Errorf("drain wait %s: %w", a.node, err)
	}
	logger.Info("upgrade-node: drain wait complete", "node", a.node)

	// 3. Cloud-specific rollout — hand off to the per-cloud wrapper.
	if err := runCloudRollout(ctx, a); err != nil {
		return fmt.Errorf("cloud rollout %s: %w", a.cloud, err)
	}
	logger.Info("upgrade-node: cloud rollout signal sent", "node", a.node, "cloud", a.cloud)

	// 4+5. Wait for the new VM to come up + poll every Lifecycle.Probe.
	if err := waitForReady(ctx, a); err != nil {
		return fmt.Errorf("ready gate %s: %w", a.node, err)
	}
	logger.Info("upgrade-node: ready gate passed", "node", a.node)

	// 6. Flip active=true — only after every probe passes.
	if err := runActivate(ctx, a); err != nil {
		return fmt.Errorf("activate %s: %w", a.node, err)
	}
	logger.Info("upgrade-node: activated", "node", a.node)
	return nil
}

// runDrain invokes `gregalectl compute-nodes drain --node <fqdn>`.
// PR #914's CLI emits the same UPDATE that pkg/state.PgStore.
// MarkComputeNodeInactive does (pkg/state/pgstore.go:8720).
func runDrain(ctx context.Context, a *upgradeArgs) error {
	cmd := exec.CommandContext(ctx, "/opt/faas/current/bin/gregalectl",
		"compute-nodes", "drain", "--node", a.node)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run()
}

// waitForDrain sleeps for MigrateLiveLeaseSeconds + 5s grace, then
// re-checks via gregalectl compute-nodes drain-status. The schedd's
// rebalance is asynchronous (every 30s per the heartbeat tick); the
// wait absorbs a full tick + a margin.
func waitForDrain(ctx context.Context, a *upgradeArgs) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(a.drainTimeout):
	}
	cmd := exec.CommandContext(ctx, "/opt/faas/current/bin/gregalectl",
		"compute-nodes", "drain-status", "--node", a.node)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return errors.New("instances still on node — run 'gregalectl compute-nodes force-drain' to override")
	}
	return nil
}

// runCloudRollout invokes the cloud-specific wrapper. The wrapper's
// contract: it takes (node, image_tag), performs the cloud-specific
// image swap (Hetzner rebuild, AWS AMI-rotate, PXE-boot), and exits 0
// on success.
func runCloudRollout(ctx context.Context, a *upgradeArgs) error {
	rolloutScript := a.cloudRollout
	if rolloutScript == "" {
		rolloutScript = fmt.Sprintf("deploy/packer/cloud-rollout/%s.sh", a.cloud)
	}
	cmd := exec.CommandContext(ctx, "bash", rolloutScript, a.node, a.imageTag)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run()
}

// waitForReady polls every Lifecycle.Probe / ProbeTarget in
// pkg/daemonunitspec.Registry IN REGISTRATION ORDER until every entry
// reports ready. Reuses runtime.go:295-299's probe primitives — the
// same waitPath / waitTCP / waitSystemdActive that `deployctl deploy`
// uses to gate per-service readiness.
func waitForReady(ctx context.Context, a *upgradeArgs) error {
	deadline := time.Now().Add(a.readyTimeout)
	for _, entry := range daemonunitspec.Registry {
		if time.Now().After(deadline) {
			return fmt.Errorf("ready gate: deadline exceeded before %s", entry.Name)
		}
		if err := waitOneReady(ctx, entry, time.Until(deadline)); err != nil {
			return fmt.Errorf("ready gate: %s not ready: %w", entry.Name, err)
		}
	}
	return nil
}

// waitOneReady maps daemonunitspec.Lifecycle.Probe to the existing
// runtime probe helpers (waitPath / waitTCP / waitSystemdActive).
// The switch is byte-identical to runtime.go:295-299.
func waitOneReady(ctx context.Context, entry daemonunitspec.Entry, timeout time.Duration) error {
	if timeout < 0 {
		timeout = 0
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	switch entry.Lifecycle.Probe {
	case daemonunitspec.ProbeUnix:
		return waitPath(rctx, entry.Lifecycle.ProbeTarget, timeout)
	case daemonunitspec.ProbeTCP:
		return waitTCP(rctx, entry.Lifecycle.ProbeTarget, timeout)
	case daemonunitspec.ProbeSystemd:
		return waitSystemdActive(rctx, "faas-"+entry.Name+".service", timeout)
	default:
		return fmt.Errorf("unknown readiness probe for %s", entry.Name)
	}
}

// runActivate flips compute_nodes.active=true via gregalectl
// compute-nodes activate — same wire path as drain.
func runActivate(ctx context.Context, a *upgradeArgs) error {
	cmd := exec.CommandContext(ctx, "/opt/faas/current/bin/gregalectl",
		"compute-nodes", "activate", "--node", a.node)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run()
}
