// commands_compute_nodes.go — gregalectl compute-nodes subcommand
// dispatcher (PR #929 review-fix).
//
// Used by the image-rollout orchestrator at cmd/deployctl/upgrade.go:
// the upgrade-node flow calls `gregalectl compute-nodes drain --node X`,
// `gregalectl compute-nodes drain-status --node X`, and
// `gregalectl compute-nodes activate --node X`. Without this dispatcher
// the upgrade orchestrator was dead on arrival — the CLI fell into the
// default case (cmd/gregalectl/main.go:155-157) and exited 1.
//
// Wire shape: every subcommand takes --node=<fqdn>; the state package
// owns the canonical SQL UPDATE (pkg/state.MarkComputeNodeInactive for
// drain, pkg/state.SetComputeNodeActive(ctx, id, true) for activate).
// drain-status queries pkg/state.ListInstancesByNodeID and counts rows
// in {WAKING, COLD_BOOTING, RUNNING}; > 0 means the upgrade orchestrator
// blocks until the operator runs force-drain.

package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onebox-faas/faas/pkg/state"
)

// dispatchComputeNodes is wired into cmd/gregalectl/main.go:switch
// alongside the other dispatch* consts.
const dispatchComputeNodes = "compute-nodes"

// cmdComputeNodesDispatch fans to drain / drain-status / activate /
// force-drain. Matches the (args []string) int signature every other
// dispatch* arm uses (see commands_release.go:cmdReleaseDispatch).
func cmdComputeNodesDispatch(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "gregalectl compute-nodes: missing subcommand; want drain|drain-status|activate|force-drain")
		return 2
	}
	switch args[0] {
	case "drain":
		return cmdComputeNodesDrain(args[1:])
	case "drain-status":
		return cmdComputeNodesDrainStatus(args[1:])
	case "activate":
		return cmdComputeNodesActivate(args[1:])
	case "force-drain":
		return cmdComputeNodesForceDrain(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "gregalectl compute-nodes: unknown subcommand %q\n", args[0])
		return 2
	}
}

// openComputeNodesStore wires a state.Store from FAAS_PG_DSN via the
// existing openPgPoolFromEnv helper (commands_release.go:344). The
// pool is returned alongside the store so the caller can close it.
func openComputeNodesStore() (state.Store, *pgxpool.Pool, error) {
	pool, err := openPgPoolFromEnv()
	if err != nil {
		return nil, nil, fmt.Errorf("gregalectl compute-nodes: %w", err)
	}
	return state.NewPgStore(pool), pool, nil
}

// cmdComputeNodesDrain runs `UPDATE compute_nodes SET active=false
// WHERE id=<fqdn>` via state.Store.MarkComputeNodeInactive.
func cmdComputeNodesDrain(args []string) int {
	fs := flag.NewFlagSet("drain", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	node := fs.String("node", "", "fqdn of the node to drain")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *node == "" {
		fmt.Fprintln(os.Stderr, "gregalectl compute-nodes drain: --node required")
		return 2
	}
	st, pool, err := openComputeNodesStore()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer pool.Close()

	if err := st.MarkComputeNodeInactive(context.Background(), *node); err != nil {
		fmt.Fprintln(os.Stderr, "gregalectl compute-nodes drain:", err)
		return 1
	}
	fmt.Fprintf(os.Stdout, "drained %s\n", *node)
	return 0
}

// cmdComputeNodesDrainStatus reports whether live instances remain on
// the node. Exit 0 if drain is safe (no live instances); exit 1 if
// instances still pinned (upgrade orchestrator surfaces this as
// "instances still on node" and the operator runs force-drain).
//
// Per CLAUDE.md invariants, an instance is "live" iff it's in
// {WAKING, COLD_BOOTING, RUNNING}. The state package exposes
// ListInstancesByNodeID; we filter to the live subset here so the
// caller doesn't have to walk the instance lifecycle.
func cmdComputeNodesDrainStatus(args []string) int {
	fs := flag.NewFlagSet("drain-status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	node := fs.String("node", "", "fqdn of the node to check")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *node == "" {
		fmt.Fprintln(os.Stderr, "gregalectl compute-nodes drain-status: --node required")
		return 2
	}
	st, pool, err := openComputeNodesStore()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer pool.Close()

	ctx := context.Background()
	insts, err := st.ListInstancesByNodeID(ctx, *node)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gregalectl compute-nodes drain-status:", err)
		return 1
	}
	live := 0
	for _, inst := range insts {
		switch inst.State {
		case "WAKING", "COLD_BOOTING", "RUNNING":
			live++
		}
	}
	if live > 0 {
		fmt.Fprintf(os.Stdout, "instances still on %s: %d\n", *node, live)
		return 1
	}
	fmt.Fprintf(os.Stdout, "drain-safe: %s has 0 live instances\n", *node)
	return 0
}

// cmdComputeNodesActivate runs the inverse — flips active=true. Only
// invoked by the upgrade orchestrator AFTER every Lifecycle.Probe on
// every Registry entry reports ready (cmd/deployctl/upgrade.go:waitForReady).
func cmdComputeNodesActivate(args []string) int {
	fs := flag.NewFlagSet("activate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	node := fs.String("node", "", "fqdn of the node to activate")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *node == "" {
		fmt.Fprintln(os.Stderr, "gregalectl compute-nodes activate: --node required")
		return 2
	}
	st, pool, err := openComputeNodesStore()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer pool.Close()

	if err := st.SetComputeNodeActive(context.Background(), *node, true); err != nil {
		fmt.Fprintln(os.Stderr, "gregalectl compute-nodes activate:", err)
		return 1
	}
	fmt.Fprintf(os.Stdout, "activated %s\n", *node)
	return 0
}

// cmdComputeNodesForceDrain is the operator's escape hatch when an
// upgrade can't move because live instances are pinned. NOT called by
// the upgrade orchestrator (the operator runs this manually after
// acknowledging the loud warning). Same SQL as drain but explicitly
// named so the operator's intent is auditable.
func cmdComputeNodesForceDrain(args []string) int {
	fs := flag.NewFlagSet("force-drain", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	node := fs.String("node", "", "fqdn of the node to force-drain")
	ack := fs.Bool("yes", false, "acknowledge that live instances may be cold-evicted")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *node == "" {
		fmt.Fprintln(os.Stderr, "gregalectl compute-nodes force-drain: --node required")
		return 2
	}
	if !*ack {
		fmt.Fprintln(os.Stderr, "gregalectl compute-nodes force-drain: --yes required (live instances may be cold-evicted)")
		return 2
	}
	st, pool, err := openComputeNodesStore()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer pool.Close()

	if err := st.MarkComputeNodeInactive(context.Background(), *node); err != nil {
		fmt.Fprintln(os.Stderr, "gregalectl compute-nodes force-drain:", err)
		return 1
	}
	fmt.Fprintf(os.Stdout, "force-drained %s\n", *node)
	return 0
}
