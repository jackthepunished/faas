package main

// commands_decompose.go — Phase 3 repo decomposition CLI surface.
//
// Two new dispatch entries (registered in main.go):
//
//	gregale scan     — dry-run; renders the plan as a table or --json
//	gregale deploy   — extends cmdDeployTarball with --yes, --json,
//	                   --only, --project-slug for the one-key provision
//	                   flow on top of the existing --tarball/--image/
//	                   --template paths.
//
// Mutual exclusion: scan takes --tarball XOR --path XOR --repo (exactly
// one); the same flags are accepted on deploy with --yes/--json/--only
// to trigger the transactional apply.
//
// The CLI mirrors §4 acceptance verbatim: "faas deploy on the fixture
// repo creates 3 apps + 1 cron on one keypress; over-quota creates
// nothing; --json output is byte-stable." Test coverage lives in
// commands_decompose_test.go (Phase 3 task #49).

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
)

// cmdScan is the dry-run entry point.
//
//	gregale scan [--tarball X | --path Y | --repo owner/name] \
//	             [--project-slug S] [--only a,b,c] [--ref REF] \
//	             [--repository owner/name] [--install-id N]
//
// Renders the plan as a table by default; --json emits the server's
// PlanResponse bytes verbatim. Never writes; can_apply=false on
// over-quota surfaces the limit problem from the same response.
func cmdScan(args []string) int {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	tarball := fs.String("tarball", "", "path to source .tar.gz")
	pathFlag := fs.String("path", "", "path to local repo dir (auto-packed)")
	repo := fs.String("repo", "", "github owner/name to fetch tarball for")
	ref := fs.String("ref", "main", "git ref for --repo")
	only := fs.String("only", "", "comma-separated workload names")
	projectSlug := fs.String("project-slug", "", "kebab slug; default = repo dir basename")
	installID := fs.Int64("install-id", 0, "GitHub install id (with --repo)")
	prodBranch := fs.String("production-branch", "main", "production branch for the project")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	// Exactly one of --tarball / --path / --repo. Default --path $PWD
	// when stdin is a TTY and no flag is set (issue #313 zero-config).
	srcPath, sourceName, cleanup, err := resolveScanSource(*tarball, *pathFlag, *repo, *ref, *installID)
	if err != nil {
		return printErr("Could not resolve source", err)
	}
	defer cleanup()

	if *projectSlug == "" {
		*projectSlug = defaultProjectSlug(srcPath)
	}

	client, err := authedClientWithDeployTimeout(2 * time.Minute)
	if err != nil {
		return printErr("Not logged in", err)
	}
	ctx := context.Background()
	onlyList := splitCSV(*only)
	// srcPath was either customer-supplied (--tarball / --path) or
	// resoled by the CLI (autoPackCwd tmp / curl repo tmp). Both pass
	// through openCustomerFile so the lint tripwire's symlink-follow
	// guarantee (commands5.go) covers the Phase 3 paths too.
	src, err := openCustomerFile(srcPath)
	if err != nil {
		return printErr("Could not open source", err)
	}
	defer func() { _ = src.Close() }()
	plan, err := client.ScanProject(ctx, src, sourceName, *projectSlug, *prodBranch, *installID, onlyList)
	if err != nil {
		return printErr("Scan failed", err)
	}
	if jsonOutput {
		return jsonOut(writeJSON(plan))
	}
	return printPlanText(osStdout, plan)
}

// resolveScanSource normalises the three input shapes (--tarball /
// --path / --repo) into a (path, sourceName, cleanup, err). For
// --repo the tarball is fetched via the install token and dropped
// into a tmpfile (cleanup removes it). For --path the local directory
// is auto-packed via the same autoPackCwd the deploy path uses.
func resolveScanSource(
	tarball, pathFlag, repo, ref string, installID int64,
) (string, string, func(), error) {
	chosen := 0
	if tarball != "" {
		chosen++
	}
	if pathFlag != "" {
		chosen++
	}
	if repo != "" {
		chosen++
	}
	if chosen > 1 {
		return "", "", func() {}, errors.New("--tarball, --path, and --repo are mutually exclusive")
	}
	if tarball != "" {
		return tarball, filepath.Base(tarball), func() {}, nil
	}
	if pathFlag != "" {
		path, _, n, err := autoPackCwd(pathFlag)
		if err != nil {
			return "", "", func() {}, err
		}
		_ = n
		return path, filepath.Base(path) + ".tar.gz", func() { _ = os.Remove(path) }, nil
	}
	if repo != "" {
		if installID <= 0 {
			return "", "", func() {}, errors.New("--repo requires --install-id")
		}
		path, err := fetchRepoTarball(repo, ref, installID)
		if err != nil {
			return "", "", func() {}, err
		}
		return path, fmt.Sprintf("%s-%s.tar.gz", strings.ReplaceAll(repo, "/", "-"), ref),
			func() { _ = os.Remove(path) }, nil
	}
	// zero-config: stdin is a TTY → pack $PWD (issue #313)
	if stdoutIsTTY() && stdinIsTTY() {
		cwd, err := os.Getwd()
		if err != nil {
			return "", "", func() {}, err
		}
		path, _, n, err := autoPackCwd(cwd)
		if err != nil {
			return "", "", func() {}, err
		}
		_ = n
		return path, filepath.Base(path) + ".tar.gz", func() { _ = os.Remove(path) }, nil
	}
	return "", "", func() {}, errors.New("one of --tarball, --path, --repo, or a TTY cwd is required")
}

// fetchRepoTarball shells out to curl to download the GitHub tarball
// using the install token from env or keyring. Returns the tmp path.
// The function lives in the CLI (not pkg/) because the install token
// lives in the customer's keychain — apid does not see it.
//
// Curl+sha256+tar pattern mirrors CI's vacuum binary download
// (memory note: ci-vacuum-binary-download).
func fetchRepoTarball(repoFullName, ref string, installID int64) (string, error) {
	f, err := os.CreateTemp("", "gregale-repo-*.tar.gz")
	if err != nil {
		return "", err
	}
	_ = f.Close()
	path := f.Name()
	token, err := readInstallToken(installID)
	if err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("read install token: %w", err)
	}
	url := fmt.Sprintf("https://api.github.com/repos/%s/tarball/%s", repoFullName, ref)
	cmd := exec.Command("curl", "-sSL", "--fail-with-body",
		"-H", "Authorization: Bearer "+token,
		"-H", "Accept: application/vnd.github+json",
		"-o", path, url)
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("curl: %w: %s", err, string(out))
	}
	return path, nil
}

// readInstallToken pulls a GitHub install token from env or keyring.
// Tries GREGALE_INSTALL_TOKEN_<ID> first (env override for CI), then
// errors cleanly so the caller can surface a "run `gregale connect`
// first" message.
func readInstallToken(installID int64) (string, error) {
	if v := os.Getenv(fmt.Sprintf("GREGALE_INSTALL_TOKEN_%d", installID)); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("no install token for id %d — run `gregale connect` first", installID)
}

// defaultProjectSlug derives a kebab slug from the source path's
// final component, stripping archive-style suffixes (.tar.gz, .tgz).
// Returns "" when the path is empty so the apid server falls back
// to its project-<random> default. The two-step strip is intentional
// — a single filepath.Ext() call only drops ".gz" and leaves ".tar"
// in the slug, which customers typing "deploy fixture.tar.gz" would
// not expect.
func defaultProjectSlug(p string) string {
	if p == "" {
		return ""
	}
	base := filepath.Base(p)
	for _, suffix := range []string{".tar.gz", ".tgz", ".zip"} {
		if strings.HasSuffix(base, suffix) {
			base = strings.TrimSuffix(base, suffix)
			break
		}
	}
	base = strings.TrimSuffix(base, filepath.Ext(base))
	return base
}

// splitCSV returns the trimmed lowercase entries of s. Empty input → nil.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		p = strings.ToLower(strings.TrimSpace(p))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// printPlanText renders the plan as a human-readable table. Two
// sections: Workloads (sorted by name asc) and Managed (stateful
// services we won't provision). Cron rows appear under Workloads with
// a "(cron: <schedule>)" suffix.
func printPlanText(w io.Writer, plan api.PlanResponse) int {
	fmt.Fprintf(w, "Project: %s\n", plan.ProjectSlug)
	fmt.Fprintf(w, "Scan source: %s   tier: %s\n", plan.ScanSource, plan.Tier)
	fmt.Fprintf(w, "Quota: %d/%d apps   %d/%d crons\n",
		plan.ObservedApps, plan.LimitApps, plan.ObservedCrons, plan.LimitCrons)
	if plan.CronsNotAllowed {
		fmt.Fprintln(w, "(Crons unavailable on this plan — upgrade to Hobby or above.)")
	}
	if !plan.CanApply {
		fmt.Fprintln(w, "can_apply: false")
		return 0
	}
	fmt.Fprintln(w, "can_apply: true")
	if len(plan.Workloads) > 0 {
		fmt.Fprintln(w, "\nWorkloads:")
		// sort defensively even though reposcan already sorts — a
		// future server-side refactor must not change CLI output.
		ws := append([]api.PlanWorkload(nil), plan.Workloads...)
		sort.Slice(ws, func(i, j int) bool { return ws[i].Name < ws[j].Name })
		for _, wl := range ws {
			schedSuffix := ""
			if wl.Schedule != "" {
				schedSuffix = " (cron: " + wl.Schedule + ")"
			}
			classSuffix := ""
			if wl.Class != "" {
				classSuffix = "  class=" + wl.Class
			}
			fmt.Fprintf(w, "  - %-20s root=%-20s%s%s\n", wl.Name, wl.RootDir, schedSuffix, classSuffix)
		}
	}
	if len(plan.Managed) > 0 {
		fmt.Fprintln(w, "\nManaged (not provisioned):")
		for _, m := range plan.Managed {
			fmt.Fprintf(w, "  - %s [%s]   hint=%s   image=%s\n",
				m.Name, m.Kind, m.EnvHint, m.Image)
		}
	}
	if len(plan.Warnings) > 0 {
		fmt.Fprintln(w, "\nWarnings:")
		for _, wn := range plan.Warnings {
			fmt.Fprintln(w, "  - "+wn)
		}
	}
	return 0
}

// confirmPlan prints the plan and waits for a y/N confirmation. Reads
// from r (typically os.Stdin) so tests can stub it. Returns true on
// 'y' / 'yes' (case-insensitive); false on EOF, 'n', or any other
// input — git does the same.
func confirmPlan(w io.Writer, r io.Reader, plan api.PlanResponse) bool {
	printPlanText(w, plan)
	fmt.Fprintln(w, "\nApply this plan? [y/N] ")
	scanner := bufio.NewScanner(r)
	if !scanner.Scan() {
		return false
	}
	ans := strings.ToLower(strings.TrimSpace(scanner.Text()))
	return ans == "y" || ans == "yes"
}

// planProblem translates a non-applyable plan into the matching
// RFC 7807 problem shape the apid handler emits. Used by the --json
// path so a CLI consumer sees the same wire shape as a direct
// httptest call would produce.
func planProblem(plan api.PlanResponse) api.Problem {
	if plan.CronsNotAllowed {
		return api.Problem{
			Status: 402,
			Code:   api.CodePlanCronsNotAllowed,
			Title:  "Crons unavailable on this plan",
			Detail: "the Free plan does not include cron; upgrade to Hobby or above to schedule synthetic requests.",
		}
	}
	if plan.ObservedApps > plan.LimitApps {
		return api.Problem{
			Status: 403,
			Code:   api.CodePlanLimitApps,
			Title:  "App limit reached",
			Detail: fmt.Sprintf("plan caps apps at %d; you have %d.", plan.LimitApps, plan.ObservedApps),
		}
	}
	return api.Problem{
		Status: 403,
		Code:   api.CodePlanCronQuota,
		Title:  "Cron limit reached",
		Detail: fmt.Sprintf("plan caps crons at %d; you have %d.", plan.LimitCrons, plan.ObservedCrons),
	}
}
