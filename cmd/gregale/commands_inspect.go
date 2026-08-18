// gregale inspect — read-only operator surface for inspecting
// per-app state (issue #952). First leaf: `--upstreams`
// (ADR-098 §9.A captured-upstream table). Future leaves
// (--env, --crons, --instances, …) add a flag here and a
// commands_inspect_<noun>.go dispatcher file — keeps the verb
// itself a single dispatcher and the per-leaf logic isolated.
//
// Why this isn't a sub-dispatch table (a la crons / domains):
// every leaf has its own flag set + validation, and a future
// `--env` and `--crons` will share the slug but NOT the
// filters. Putting the leaf's flag set on cmdInspect itself
// lets the man-page manifest see every flag at one stop and
// keeps the per-leaf renderer in commands_inspect_<noun>.go
// narrow (≤50 lines per the handlers convention).
//
// UX shape (the v1 leaf):
//
//	$ gregale inspect <slug> --upstreams
//	$ gregale inspect <slug> --upstreams --scope <scope>
//	$ gregale inspect <slug> --upstreams --json
//
// Future leaves reuse the slug positional; their flag set is
// added below the `--upstreams` arm.
package main

import (
	"flag"
	"fmt"
	"os"
)

// cmdInspect is the verb-level dispatcher for
// `gregale inspect <slug> [flags]`. v1 recognises only one leaf
// (`--upstreams`); unknown leaves print the same usage hint
// with no server call (the validator runs before authedClient).
//
// Why --upstreams is a flag and not a positional subcommand:
// the issue wording is authoritative — `gregale inspect <slug>
// --upstreams` — and the verb-shape is symmetric with the
// upcoming `gregale inspect <slug> --env` / `--crons` /
// `--instances` leaves (each one is a switch on the leaf set
// the customer wants to see).
//
// inspectUsage is the single source of truth for the verb's
// usage line. The dispatcher prints it on every bad-args path
// (missing slug, trailing positional, missing leaf flag) so
// drift across the three call sites is impossible — the test
// file pins this exact wording.
const inspectUsage = "usage: gregale inspect <slug> [--upstreams] [--scope <scope>] [--json]"

func cmdInspect(args []string) int {
	if len(args) == 0 {
		PrintUsage(os.Stderr, inspectUsage, "inspect")
		return 1
	}
	// The slug is always args[0]; flags follow. Mirrors the
	// cmdCronsUpdate shape (commands2.go) — stdlib flag.Parse
	// stops parsing flags at the first positional, so the
	// slug-shaped args[0] would otherwise eat the --upstreams
	// flag. We pass args[1:] to the FlagSet so the leaf's flags
	// are visible.
	slug := args[0]
	if !validCLISlug(slug) {
		fmt.Fprintf(os.Stderr, "invalid slug %q (3..40 chars, lowercase alnum + dash, no leading/trailing dash)\n", slug)
		return 1
	}
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	upstreams := fs.Bool("upstreams", false, "list data upstreams captured for this app (ADR-098 §9.A)")
	scope := fs.String("scope", "", "filter upstreams by scope (forwarded as ?scope=<scope>)")
	if err := fs.Parse(args[1:]); err != nil {
		return 1
	}
	if fs.NArg() != 0 {
		PrintUsage(os.Stderr, inspectUsage, "inspect")
		return 1
	}
	// At least one leaf flag is required for v1 — the verb without
	// any leaf would be ambiguous (future leaves will add their
	// own gates). A bare `gregale inspect myapp` exits 1 with
	// the same usage line, no server call.
	if !*upstreams {
		PrintUsage(os.Stderr, inspectUsage, "inspect")
		return 1
	}
	return cmdInspectUpstreams(slug, *scope)
}
