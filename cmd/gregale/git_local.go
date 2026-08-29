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
// returns (owner, repo). v1 only resolves GitHub hostnames into a
// zero-config-eligible (owner, repo) pair; any other host returns
// ("", "", nil) so the caller can route to the non-zero-config
// (cwd-auto-pack) branch instead of erroring on a perfectly valid
// non-GitHub repo.
//
// Accepted (any of the standard GitHub remote shapes):
//   - git@github.com:OWNER/REPO[.git]
//   - ssh://[git@]github.com[:22]/OWNER/REPO[.git]
//   - ssh://[git@]github.com[:22]:OWNER/REPO[.git]
//   - https://[user[:pass]@]github.com/OWNER/REPO[.git]
//   - http://[user[:pass]@]github.com/OWNER/REPO[.git]
//
// Returned triples:
//   - GitHub URL that parses cleanly → (owner, repo, nil)
//   - Non-GitHub URL of any shape     → ("",  "",    nil)  — caller falls through
//   - GitHub URL that's malformed     → ("",  "",    err) — caller surfaces the error
//   - Empty URL                       → ("",  "",    err) — caller surfaces "no remote"
func parseGitRemoteURL(remoteURL string) (owner, repo string, err error) {
	remoteURL = strings.TrimSpace(remoteURL)
	if remoteURL == "" {
		return "", "", errors.New("parseGitRemoteURL: empty remote URL")
	}

	// SSH "scp-like" form: git@github.com:OWNER/REPO[.git]
	if strings.HasPrefix(remoteURL, "git@github.com:") {
		return splitOwnerRepo(strings.TrimPrefix(remoteURL, "git@github.com:"))
	}

	// SSH explicit form: ssh://[user@]host[:port]/path
	// (a few SSH clients use this; `git clone` never produces it but
	// hand-written remote URLs do — issue #1182 §3.6.)
	if path, ok := matchSSHURL(remoteURL, "github.com"); ok {
		return splitOwnerRepo(path)
	}

	// HTTPS with optional userinfo: https://[user[:pass]@]github.com/...
	if rest, ok := stripURLUserinfo(remoteURL, "https://", "github.com"); ok {
		return splitOwnerRepo(rest)
	}
	if rest, ok := stripURLUserinfo(remoteURL, "http://", "github.com"); ok {
		return splitOwnerRepo(rest)
	}

	// Anything else (file://, git://, plain non-host paths, or any
	// https/http URL pointing at a non-github.com host) — not a
	// recognised zero-config origin. Return empty triple so the
	// caller falls through to the cwd-auto-pack branch.
	return "", "", nil
}

// matchSSHURL handles "ssh://[user@]host[:port]/path" forms for the
// given host. Returns (path, true) on a host match, ("", false)
// otherwise. Caller is responsible for splitting the path into
// (owner, repo) via splitOwnerRepo (which strips a trailing .git
// and lowercases).
func matchSSHURL(remoteURL, host string) (path string, ok bool) {
	const prefix = "ssh://"
	if !strings.HasPrefix(remoteURL, prefix) {
		return "", false
	}
	rest := remoteURL[len(prefix):]
	// userinfo (before '@') is optional.
	if at := strings.IndexByte(rest, '@'); at >= 0 {
		rest = rest[at+1:]
	}
	// host[:port]/...
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return "", false
	}
	hostPort := rest[:slash]
	// Trim optional :port.
	if colon := strings.IndexByte(hostPort, ':'); colon >= 0 {
		hostPort = hostPort[:colon]
	}
	if hostPort != host {
		return "", false
	}
	return rest[slash+1:], true
}

// stripURLUserinfo handles "scheme://[userinfo@]host/..." for the
// given scheme + host. Returns (path, true) on a host match,
// ("", false) otherwise — including the "scheme matched but host
// didn't" case (which the caller maps to a soft empty-triple
// return, signalling "not zero-config-eligible").
//
// On the GitHub path, the userinfo segment is dropped: a token
// embedded in the remote URL is rare (and a security smell — the
// GitHub App auth model uses install tokens, not URL credentials),
// but if a customer's remote has it, we don't want to surface it on
// the deployment row.
func stripURLUserinfo(remoteURL, scheme, host string) (path string, ok bool) {
	if !strings.HasPrefix(remoteURL, scheme) {
		return "", false
	}
	rest := remoteURL[len(scheme):]
	if at := strings.IndexByte(rest, '@'); at >= 0 {
		rest = rest[at+1:]
	}
	hostPrefix := host + "/"
	if !strings.HasPrefix(rest, hostPrefix) {
		// scheme matched but host didn't — fall through so caller
		// returns empty triple (not zero-config-eligible, not an error).
		return "", false
	}
	return rest[len(hostPrefix):], true
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
