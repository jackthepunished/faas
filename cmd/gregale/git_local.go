// cmd/gregale/git_local.go — pure helpers for zero-config `gregale deploy`
// (issue #961 / Mega-A PR-1). The CLI walks up from cwd to find the
// enclosing git repo, parses the `origin` remote URL into an
// (owner, repo) pair, resolves HEAD to its 40-char SHA, and warns if
// the working tree is dirty.
//
// Trust model: see docs/adr/0XX-local-tarball-deploy-trust-root.md.
// The CLI is the trust root for the local-tarball upload path; these
// helpers do NOT make network calls and do NOT shell out beyond
// `git(1)` invocations. Failures here surface to the operator as
// plain-text errors before any HTTP traffic.

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrNotInGitRepo is returned by gitRootFromCwd when no .git/ ancestor
// exists. The CLI maps this to a "not in a git repo" error and exits 1.
var ErrNotInGitRepo = errors.New("not in a git repo")

// ErrNoGitRemote is returned when the repo has no `origin` remote set.
// Zero-config deploy requires the remote to derive the (owner, repo)
// pair; without one we cannot construct the deployment's source_url.
var ErrNoGitRemote = errors.New("no git remote 'origin'")

// gitRootFromCwd walks up from start to find the first ancestor that
// contains a `.git/` directory (or `.git` file, for worktrees). Returns
// the absolute path of that ancestor, or ErrNotInGitRepo if none.
// Symbolic links are resolved at every step so a symlinked cwd does
// not loop infinitely.
func gitRootFromCwd(start string) (string, error) {
	if start == "" {
		start = "."
	}
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("gitRootFromCwd: abs: %w", err)
	}
	cur := abs
	for {
		// filepath.Dir of "/" returns "/"; stop there.
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			return cur, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", ErrNotInGitRepo
		}
		cur = parent
	}
}

// parseGitRemoteURL accepts the URL forms GitHub ships today and
// returns (owner, repo). v1 only supports GitHub hostnames — non-GitHub
// remotes are rejected with a clear error so the operator knows to
// pass `--repo OWNER/NAME --ref SHA` explicitly or install the Gregale
// GitHub App.
//
// Accepted:
//   - git@github.com:OWNER/REPO.git
//   - git@github.com:OWNER/REPO
//   - https://github.com/OWNER/REPO.git
//   - https://github.com/OWNER/REPO
//
// Rejected:
//   - empty string
//   - ssh://git@github.com/OWNER/REPO.git (not used by `git clone`)
//   - any non-github.com host (gitlab, bitbucket, custom)
//   - malformed (missing owner or repo)
func parseGitRemoteURL(remoteURL string) (owner, repo string, err error) {
	remoteURL = strings.TrimSpace(remoteURL)
	if remoteURL == "" {
		return "", "", errors.New("parseGitRemoteURL: empty remote URL")
	}

	// SSH form: git@github.com:OWNER/REPO(.git)?
	if strings.HasPrefix(remoteURL, "git@github.com:") {
		rest := strings.TrimPrefix(remoteURL, "git@github.com:")
		owner, repo, err = splitOwnerRepo(rest)
		if err != nil {
			return "", "", fmt.Errorf("parseGitRemoteURL: ssh form: %w", err)
		}
		return owner, repo, nil
	}

	// HTTPS form: https://github.com/OWNER/REPO(.git)?
	if strings.HasPrefix(remoteURL, "https://github.com/") {
		rest := strings.TrimPrefix(remoteURL, "https://github.com/")
		owner, repo, err = splitOwnerRepo(rest)
		if err != nil {
			return "", "", fmt.Errorf("parseGitRemoteURL: https form: %w", err)
		}
		return owner, repo, nil
	}

	// HTTP form (rarely used; CLI tools sometimes normalize to https).
	if strings.HasPrefix(remoteURL, "http://github.com/") {
		rest := strings.TrimPrefix(remoteURL, "http://github.com/")
		owner, repo, err = splitOwnerRepo(rest)
		if err != nil {
			return "", "", fmt.Errorf("parseGitRemoteURL: http form: %w", err)
		}
		return owner, repo, nil
	}

	return "", "", fmt.Errorf("parseGitRemoteURL: only github.com remotes are supported in v1; got %q", remoteURL)
}

// splitOwnerRepo splits "OWNER/REPO" or "OWNER/REPO.git" into
// (OWNER, REPO), lowercasing both. Returns an error if either segment
// is empty, contains a '/', or is otherwise malformed.
func splitOwnerRepo(s string) (owner, repo string, err error) {
	s = strings.TrimSuffix(s, ".git")
	slash := strings.IndexByte(s, '/')
	if slash <= 0 || slash == len(s)-1 {
		return "", "", fmt.Errorf("expected OWNER/REPO, got %q", s)
	}
	owner = s[:slash]
	repo = s[slash+1:]
	if strings.ContainsAny(owner, "/ \t\n") || strings.ContainsAny(repo, "/ \t\n") {
		return "", "", fmt.Errorf("invalid OWNER or REPO in %q", s)
	}
	return strings.ToLower(owner), strings.ToLower(repo), nil
}

// gitRemoteOrigin returns the `origin` remote URL for the repo at
// gitDir, or ErrNoGitRemote if no `origin` is configured.
//
// MED-3 fix: previously this helper detected the "no remote" case
// by substring-matching stderr for "exit status 1", which broke the
// moment git changed its error wording (and would misclassify a
// config-file parse error as "no remote"). The proper gate is
// exec.ExitError.ExitCode() — `git config --get` returns 1
// specifically when the key is absent; any other non-zero exit is
// a real error to propagate.
func gitRemoteOrigin(gitDir string) (string, error) {
	out, err := runGitCmd(gitDir, "config", "--get", "remote.origin.url")
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return "", ErrNoGitRemote
		}
		return "", fmt.Errorf("gitRemoteOrigin: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// resolveHEAD runs `git rev-parse HEAD` in gitDir and returns the
// 40-char SHA. On a repo with zero commits `git rev-parse HEAD`
// fails with "ambiguous HEAD" — the caller maps that to a clean
// "no commits yet" error and exits 1 (the operator probably just
// `git init`-ed and never committed).
func resolveHEAD(gitDir string) (string, error) {
	out, err := runGitCmd(gitDir, "rev-parse", "HEAD")
	if err != nil {
		if strings.Contains(err.Error(), "ambiguous") {
			return "", errors.New("no commits yet (git rev-parse HEAD: ambiguous)")
		}
		return "", fmt.Errorf("resolveHEAD: %w", err)
	}
	sha := strings.TrimSpace(out)
	if len(sha) != 40 {
		return "", fmt.Errorf("resolveHEAD: expected 40-char SHA, got %q", sha)
	}
	return sha, nil
}

// isDirtyWorkdir runs `git status --porcelain` in gitDir and returns
// true iff there is any output (modified, untracked, staged, etc.).
// The CLI surfaces a non-fatal warning to the operator so they know
// the deploy does NOT include any uncommitted work.
func isDirtyWorkdir(gitDir string) (bool, error) {
	out, err := runGitCmd(gitDir, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("isDirtyWorkdir: %w", err)
	}
	return strings.TrimSpace(out) != "", nil
}

// runGitCmd runs `git <args...>` with -C gitDir and returns combined
// stdout+stderr trimmed. Errors include stderr so the operator sees
// the underlying git message.
func runGitCmd(gitDir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = gitDir
	// Redirect git's progress / pager noise away from the terminal.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return string(out), nil
}

// gitArchiveHEAD runs `git archive HEAD --format=tar.gz -o <outPath>` in
// gitDir, materialising the committed tree of HEAD as a gzipped tar
// archive. Used by the refactored zero-config deploy path (issue
// #1182) so the customer sees a faithful "deploying HEAD" semantic
// instead of the cwd packer, which silently includes uncommitted /
// untracked files.
//
// Errors:
//
//   - empty repo (no commits yet): rev-parse --verify HEAD^{commit}
//     fails with "unknown revision"; we surface this directly so the
//     caller can render a clean "no commits yet, commit something and
//     try again" message without parsing git's exit message.
//   - not in a repo: rev-parse fails with "not a git repository";
//     bubbled up with stderr for the operator.
//   - archive write failure (perm denied, disk full): bubbled up
//     with stderr.
//
// Caller owns outPath. On success the file exists and is a valid
// gzipped tar; the caller is expected to defer os.Remove. The
// function does not open the file — `git archive -o` writes
// directly, so no fd leak on this helper.
func gitArchiveHEAD(gitDir, outPath string) error {
	// Empty-repo guard. `git archive HEAD` would itself error with
	// "unknown revision 'HEAD'" on a fresh `git init` (exit 128) but
	// the rev-parse form gives a stable, parseable signal that we
	// can wrap without relying on git's exact stderr string.
	if _, err := runGitCmd(gitDir, "rev-parse", "--verify", "HEAD^{commit}"); err != nil {
		return fmt.Errorf("gitArchiveHEAD: %w", err)
	}
	if _, err := runGitCmd(gitDir, "archive", "HEAD", "--format=tar.gz", "-o", outPath); err != nil {
		return fmt.Errorf("gitArchiveHEAD: archive HEAD failed: %w", err)
	}
	return nil
}

// ErrNoGitConfigKey is returned by gitConfigUser when `git config --get`
// exits with code 1 (the key is not set). Mirrors the MED-3 detection
// pattern in gitRemoteOrigin (line 147): exit code 1 is the well-defined
// "key absent" sentinel; any other non-zero exit is a real error to
// propagate (config-file parse error, permission denied, etc.).
var ErrNoGitConfigKey = errors.New("git config key not set")

// gitConfigUser returns the trimmed value of `git config --get <key>`
// from the repo at gitDir. Returns ErrNoGitConfigKey when the key is
// not set; any other non-zero exit is wrapped with stderr so the
// operator sees the underlying git message.
//
// Issue #977 / ADR-116: auto-capture of `user.name` so the CLI's
// zero-config `gregale deploy` path can stamp `deployed_by` on the
// deployment row without requiring a --deployed-by flag.
func gitConfigUser(gitDir, key string) (string, error) {
	out, err := runGitCmd(gitDir, "config", "--get", key)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return "", ErrNoGitConfigKey
		}
		return "", fmt.Errorf("gitConfigUser(%s): %w", key, err)
	}
	return strings.TrimSpace(out), nil
}

// gitUserName returns the value of `git config --get user.name` in
// gitDir, or "" when the key is unset (ErrNoGitConfigKey is swallowed
// to match the "operator did not configure git" path — a CLI auto-
// capture failure must not block the deploy; the operator can always
// supply --deployed-by). Any other non-zero exit is propagated.
//
// Note: a global-only user.name (in ~/.gitconfig but not in the repo
// or its includes) IS returned; `git config --get` walks the
// include chain. This is the right behavior — most operators set
// `git config --global user.name` once and never touch the repo
// config.
func gitUserName(gitDir string) (string, error) {
	name, err := gitConfigUser(gitDir, "user.name")
	if errors.Is(err, ErrNoGitConfigKey) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return name, nil
}
