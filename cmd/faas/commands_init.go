// commands_init.go — Wave 0 PR-B customer-facing scaffolder for the
// stateless contract. UX spec §8 promises four reference templates
// (`s3-uploader`, `slack-bot`, `rest-api-postgres`, `cron-worker`) that
// each name the managed service to plug in and fail clearly if the
// customer forgot to `faas secrets set` the relevant env var. This file
// is the CLI half of that promise — `docs/storage.md` is the docs half.
//
// Command shape:
//
//	faas init --template <name> --path <dir> [--deploy] [--name <slug>]
//
// `--template` is required and validated against templates.Exists (the
// authoritative list in cmd/faas/templates/embed.go). `--path` is the
// destination directory; created if missing, refused if non-empty. The
// fail-fast contract lives in the templates themselves (each handler
// exits / 500s with the exact `faas secrets set` hint), not here.
//
// `--deploy` chains into cmdDeployTarball with --template + --name
// passed through. cmdDeployTarball's --template branch (commands2.go:291)
// re-materializes the template into its own tmpdir and tar.gz's it; the
// customer's `--path` stays as their working copy. We never chdir —
// the chain runs in the caller's cwd, the deployment is independent.
//
// What this command does NOT do (UX spec §8 doesn't promise it):
//   - list templates (`--list` is implicit; customers run with a bad
//     name and see "unknown template foo (known: ...)")
//   - update templates (`faas init` is a one-time scaffolder)
//   - validate `faas secrets set` calls (that's a server-side check;
//     the template README is the source of truth)
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/onebox-faas/faas/cmd/faas/templates"
)

// initCmdUsage is the top-of-failure-line shown for `faas init` errors.
// Mirrors PrintUsage's docs URL convention (output.go:144) so the line
// carries the stable docs site pointer.
const initCmdUsage = "usage: faas init --template <name> --path <dir> [--deploy] [--name <slug>]"

// initCmdDocsTopic is the docs topic slug appended to docsURLBase when
// PrintUsage emits the trailing "Docs:" row. Keeps the CLI's help line
// stable across template additions.
const initCmdDocsTopic = "init"

// cmdInit is the dispatcher for `faas init`. Parses flags, validates,
// materializes, and (when --deploy) chains into cmdDeployTarball.
// Returns 0 on success, 1 on user error (per UX spec §3.2).
func cmdInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	tpl := fs.String("template", "", "template name (run with a bad value to see available names)")
	dest := fs.String("path", "", "destination directory (created if missing; refused if non-empty)")
	deploy := fs.Bool("deploy", false, "after materializing, chain into `faas deploy --template <name> --name <slug>`")
	name := fs.String("name", "", "app slug to pass to --deploy (default: derive from --path basename)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() != 0 {
		PrintUsage(os.Stderr, initCmdUsage, initCmdDocsTopic)
		return 1
	}
	if *tpl == "" {
		PrintUsage(os.Stderr, initCmdUsage+"\n  error: --template is required", initCmdDocsTopic)
		return 1
	}
	if *dest == "" {
		PrintUsage(os.Stderr, initCmdUsage+"\n  error: --path is required", initCmdDocsTopic)
		return 1
	}
	return runCmdInit(*tpl, *dest, *deploy, *name, osStdout, os.Stderr)
}

// runCmdInit is the pure-logic entry point. Pulled out so the test in
// commands_init_test.go can drive it directly without rebuilding the
// flag.FlagSet. osStdout / osStderr are parameters (not globals) so
// tests can pipe capture without touching the package-level seam.
//
// The function is total — every error path prints a UX-spec §3.2 line
// and returns an int exit code; no panics, no os.Exit inside.
func runCmdInit(tpl, dest string, deploy bool, name string, stdout, stderr io.Writer) int {
	// Step 1: validate the template name against the embedded list.
	// We use Exists (which wraps NameIsValid + Names membership) so a
	// bad flag like "--template ../../etc/passwd" is rejected before we
	// touch the embed FS.
	if !templates.Exists(tpl) {
		PrintFail(stderr, "unknown --template %q (known: %s)",
			tpl, strings.Join(templates.Names, ", "))
		return 1
	}

	// Step 2: resolve --path to an absolute path. Customers pass
	// relative paths in 95% of cases; absolute resolution here makes
	// the rest of the function simpler (no need to chdir to check
	// emptiness).
	absDest, err := filepath.Abs(dest)
	if err != nil {
		return printErr("Could not resolve --path", err)
	}

	// Step 3: refuse non-empty destinations. Materialize over an
	// existing repo would silently overwrite customer work — that's
	// never the intent of `faas init`. PrintFail + 1 follows the §3.2
	// user-error contract.
	if err := checkDestEmpty(absDest); err != nil {
		return printErr("Refusing to write into "+absDest, err)
	}

	// Step 4: create the destination directory. MkdirAll covers the
	// "single-level new dir" case (./uploader) and the "nested new
	// dirs" case (./tmp/foo/uploader) — the same shape mkdir(1) takes.
	if err := os.MkdirAll(absDest, 0o755); err != nil {
		return printErr("Could not create "+absDest, err)
	}

	// Step 5: materialize the embedded template into the destination.
	// templates.Materialize is os.CopyFS under the hood (embed.go:79);
	// no chdir, no intermediate tmp.
	if err := templates.Materialize(tpl, absDest); err != nil {
		return printErr("Could not write template into "+absDest, err)
	}

	// Step 6: surface the customer's next steps. Per-template
	// `faas secrets set` hints live in the template README, but we
	// always print the storage docs link so the customer lands on the
	// canonical external-storage page (UX spec §8) regardless of which
	// template they chose.
	PrintProgress(stdout, "Wrote %s template to %s", tpl, absDest)
	PrintProgress(stdout, "Next:")
	for _, line := range nextStepsFor(tpl) {
		_, _ = fmt.Fprintf(stdout, "  %s\n", line)
	}
	PrintProgress(stdout, "Docs: https://docs.DOMAIN/storage")

	// Step 7: optional deploy chain. When --deploy is set, we hand off
	// to cmdDeployTarball with --template + --name; cmdDeployTarball
	// re-materializes the template into its own tmpdir, tar.gz's it,
	// and runs the upload + log-stream flow. The customer's --path
	// stays untouched (their working copy).
	if !deploy {
		return 0
	}
	slug := name
	if slug == "" {
		slug = sanitizeSlug(filepath.Base(absDest))
	}
	PrintProgress(stdout, "Deploying %s as %s", tpl, slug)
	return cmdDeployTarball([]string{
		"--template", tpl,
		"--name", slug,
	})
}

// checkDestEmpty returns an error if path exists and is non-empty.
// A missing path is fine (the caller will MkdirAll); an empty
// existing path is also fine. The function is split out so the test
// can exercise the edge cases without spinning up a real FS tree.
func checkDestEmpty(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("directory is not empty (found %d entr%s); pick a fresh path or `rm -rf` first",
			len(entries), pluralY(len(entries)))
	}
	return nil
}

func pluralY(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// nextStepsFor returns the customer-facing "next:" lines printed after
// a successful materialization. The list is template-specific so the
// `faas secrets set` command lines up with the README — and so the
// common-deploy pattern (`cd && faas deploy`) appears for every
// template. The CLI always emits the docs link afterwards, so this
// helper omits it; see the print loop in runCmdInit.
//
// Adding a template: append a case below AND add a secrets-set line
// in the template's README so the README and the CLI hint stay in
// lockstep. The init_test.go TestCmdInit_NextStepsFor test pins both.
func nextStepsFor(tpl string) []string {
	switch tpl {
	case "s3-uploader":
		return []string{
			"Set the S3 / R2 / B2 secrets:",
			"  faas secrets set --app <slug> S3_BUCKET=... S3_REGION=... S3_ACCESS_KEY_ID=... S3_SECRET_ACCESS_KEY=...",
			"  (optionally: S3_ENDPOINT=https://<account>.r2.cloudflarestorage.com for R2/B2)",
			"Deploy from the new directory:",
			"  cd <dest> && faas deploy",
		}
	case "slack-bot":
		return []string{
			"Set the Slack signing secret + bot token:",
			"  faas secrets set --app <slug> SLACK_SIGNING_SECRET=... SLACK_BOT_TOKEN=xoxb-...",
			"Deploy from the new directory:",
			"  cd <dest> && faas deploy",
		}
	case "rest-api-postgres":
		return []string{
			"Set the database URL (Neon / Supabase / PlanetScale / CockroachDB Cloud):",
			"  faas secrets set --app <slug> DATABASE_URL=postgres://user:pass@host/db?sslmode=require",
			"Deploy from the new directory:",
			"  cd <dest> && faas deploy",
		}
	case "cron-worker":
		return []string{
			"Set the Upstash QStash + Redis credentials:",
			"  faas secrets set --app <slug> QSTASH_TOKEN=... UPSTASH_REDIS_REST_URL=... UPSTASH_REDIS_REST_TOKEN=...",
			"Wire QStash to invoke the function (curl or the QStash dashboard):",
			"  curl -X POST https://qstash.upstash.io/v2/publish/<slug> -H \"Authorization: Bearer $QSTASH_TOKEN\"",
			"Deploy from the new directory:",
			"  cd <dest> && faas deploy",
		}
	default:
		// The seven pre-existing templates don't need secrets; print
		// the bare deploy hint. Keeps the existing
		// `faas deploy --template=hello-node` workflow discoverable
		// from `faas init --template=hello-node` too.
		return []string{
			"Deploy from the new directory:",
			"  cd <dest> && faas deploy",
		}
	}
}
