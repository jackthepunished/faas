// cmd/gregale/git_local_test.go — unit tests for the zero-config
// `gregale deploy` git helpers (issue #961 / Mega-A PR-1). The URL
// parser is the highest-risk surface (every GitHub remote variant
// has to land on a clean (owner, repo) pair), so it gets the
// broadest matrix. The walk-up + HEAD + dirty helpers are
// integration-flavored (they shell out to `git`) so they use
// per-test tmpdir repos.

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseGitRemoteURL(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		// Accepted forms.
		{"ssh with .git", "git@github.com:o/r.git", "o", "r", false},
		{"ssh without .git", "git@github.com:o/r", "o", "r", false},
		{"https with .git", "https://github.com/o/r.git", "o", "r", false},
		{"https without .git", "https://github.com/o/r", "o", "r", false},
		{"http form", "http://github.com/o/r.git", "o", "r", false},
		{"uppercase normalized", "git@github.com:OWNER/REPO.git", "owner", "repo", false},
		{"with whitespace", "  git@github.com:o/r.git  ", "o", "r", false},

		// Rejected forms (v1 is GitHub-only).
		{"empty", "", "", "", true},
		{"whitespace-only", "   ", "", "", true},
		{"gitlab ssh", "git@gitlab.com:o/r.git", "", "", true},
		{"gitlab https", "https://gitlab.com/o/r.git", "", "", true},
		{"bitbucket https", "https://bitbucket.org/o/r.git", "", "", true},
		{"custom host ssh", "git@git.example.com:o/r.git", "", "", true},
		{"custom host https", "https://git.example.com/o/r.git", "", "", true},
		{"ssh scheme variant", "ssh://git@github.com/o/r.git", "", "", true},
		{"missing repo", "git@github.com:o", "", "", true},
		{"missing owner", "git@github.com:/r", "", "", true},
		{"missing both", "git@github.com:", "", "", true},
		{"too many slashes", "git@github.com:o/r/extra", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner, repo, err := parseGitRemoteURL(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseGitRemoteURL(%q) = (%q, %q, nil); want error", tc.input, owner, repo)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseGitRemoteURL(%q): unexpected error: %v", tc.input, err)
			}
			if owner != tc.wantOwner || repo != tc.wantRepo {
				t.Fatalf("parseGitRemoteURL(%q) = (%q, %q); want (%q, %q)",
					tc.input, owner, repo, tc.wantOwner, tc.wantRepo)
			}
		})
	}
}

func TestSplitOwnerRepo(t *testing.T) {
	cases := []struct {
		in        string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		{"o/r", "o", "r", false},
		{"o/r.git", "o", "r", false},
		{"O/R.git", "o", "r", false}, // lowercased
		{"/r", "", "", true},
		{"o/", "", "", true},
		{"", "", "", true},
		{"o", "", "", true},
		{"o/r/extra", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			owner, repo, err := splitOwnerRepo(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("splitOwnerRepo(%q) = (%q, %q, nil); want error", tc.in, owner, repo)
				}
				return
			}
			if err != nil {
				t.Fatalf("splitOwnerRepo(%q): unexpected error: %v", tc.in, err)
			}
			if owner != tc.wantOwner || repo != tc.wantRepo {
				t.Fatalf("splitOwnerRepo(%q) = (%q, %q); want (%q, %q)",
					tc.in, owner, repo, tc.wantOwner, tc.wantRepo)
			}
		})
	}
}

func TestGitRootFromCwd(t *testing.T) {
	// Inside a fresh repo with no remote, the helper resolves to
	// the repo root regardless of how deep we are nested.
	t.Run("empty_start_walks_up", func(t *testing.T) {
		root := initTestRepo(t)
		// Save and restore cwd so we don't leak state.
		orig, err := os.Getwd()
		if err != nil {
			t.Fatalf("os.Getwd: %v", err)
		}
		t.Cleanup(func() { _ = os.Chdir(orig) })

		nested := filepath.Join(root, "deep", "nest")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.Chdir(nested); err != nil {
			t.Fatalf("Chdir: %v", err)
		}
		got, err := gitRootFromCwd("")
		if err != nil {
			t.Fatalf("gitRootFromCwd(\"\"): %v", err)
		}
		// On macOS, $TMPDIR is a symlink (/var/folders → /private/var/folders);
		// filepath.Abs preserves the symlink form, so canonicalize both
		// paths before comparing.
		absRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			t.Fatalf("EvalSymlinks(root): %v", err)
		}
		absGot, err := filepath.EvalSymlinks(got)
		if err != nil {
			t.Fatalf("EvalSymlinks(got): %v", err)
		}
		if absGot != absRoot {
			t.Fatalf("got %q, want %q", absGot, absRoot)
		}
	})

	// /tmp is never a git repo (well — under normal CI it isn't; if
	// your /tmp happens to be a git repo, the assertion is wrong
	// but the helper is still correct).
	t.Run("not_in_git_repo", func(t *testing.T) {
		tmp := t.TempDir()
		_, err := gitRootFromCwd(tmp)
		if !errors.Is(err, ErrNotInGitRepo) {
			t.Fatalf("gitRootFromCwd(%q): got %v; want ErrNotInGitRepo", tmp, err)
		}
	})

	// Inside a real git repo, the helper returns the repo root and
	// walks UP through nested subdirs.
	t.Run("walks_up_to_repo_root", func(t *testing.T) {
		root := initTestRepo(t)
		nested := filepath.Join(root, "a", "b", "c")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		got, err := gitRootFromCwd(nested)
		if err != nil {
			t.Fatalf("gitRootFromCwd: %v", err)
		}
		absRoot, _ := filepath.Abs(root)
		if got != absRoot {
			t.Fatalf("got %q, want %q", got, absRoot)
		}
	})
}

func TestGitRemoteOrigin(t *testing.T) {
	t.Run("happy_path", func(t *testing.T) {
		root := initTestRepo(t)
		// `git init` does not set a remote; add one explicitly.
		mustGit(t, root, "remote", "add", "origin", "git@github.com:o/r.git")
		got, err := gitRemoteOrigin(root)
		if err != nil {
			t.Fatalf("gitRemoteOrigin: %v", err)
		}
		if got != "git@github.com:o/r.git" {
			t.Fatalf("gitRemoteOrigin = %q; want %q", got, "git@github.com:o/r.git")
		}
	})

	t.Run("no_origin", func(t *testing.T) {
		root := initTestRepo(t)
		_, err := gitRemoteOrigin(root)
		if !errors.Is(err, ErrNoGitRemote) {
			t.Fatalf("gitRemoteOrigin (no origin): got %v; want ErrNoGitRemote", err)
		}
	})
}

func TestResolveHEAD(t *testing.T) {
	t.Run("happy_path_returns_sha", func(t *testing.T) {
		root := initTestRepo(t)
		// initTestRepo commits a file so HEAD resolves.
		sha, err := resolveHEAD(root)
		if err != nil {
			t.Fatalf("resolveHEAD: %v", err)
		}
		if len(sha) != 40 {
			t.Fatalf("resolveHEAD: len(sha) = %d; want 40", len(sha))
		}
	})

	t.Run("zero_commits", func(t *testing.T) {
		root := t.TempDir()
		// Plain `git init` with no commit.
		mustGit(t, root, "init", "-q")
		_, err := resolveHEAD(root)
		if err == nil {
			t.Fatalf("resolveHEAD on zero-commit repo: got nil error; want one")
		}
		if !strings.Contains(err.Error(), "no commits yet") {
			t.Fatalf("resolveHEAD on zero-commit repo: error = %v; want 'no commits yet'", err)
		}
	})
}

func TestIsDirtyWorkdir(t *testing.T) {
	t.Run("clean", func(t *testing.T) {
		root := initTestRepo(t)
		dirty, err := isDirtyWorkdir(root)
		if err != nil {
			t.Fatalf("isDirtyWorkdir: %v", err)
		}
		if dirty {
			t.Fatalf("isDirtyWorkdir = true on clean repo")
		}
	})

	t.Run("modified_file", func(t *testing.T) {
		root := initTestRepo(t)
		// Modify the existing tracked file.
		if err := os.WriteFile(filepath.Join(root, "README.md"),
			[]byte("modified contents\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		dirty, err := isDirtyWorkdir(root)
		if err != nil {
			t.Fatalf("isDirtyWorkdir: %v", err)
		}
		if !dirty {
			t.Fatalf("isDirtyWorkdir = false after modifying tracked file")
		}
	})
}

// initTestRepo creates a tempdir with a fresh `git init`, configures
// a committer identity (required for `git commit` to succeed in CI
// environments without one pre-set), commits a README, and returns
// the tempdir path. The repo has no remote — tests that need one add
// it explicitly.
func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustGit(t, dir, "init", "-q")
	mustGit(t, dir, "config", "user.email", "test@example.com")
	mustGit(t, dir, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(dir, "README.md"),
		[]byte("hello\n"), 0o644); err != nil {
		t.Fatalf("WriteFile README: %v", err)
	}
	mustGit(t, dir, "add", "README.md")
	mustGit(t, dir, "commit", "-q", "-m", "initial commit")
	return dir
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_PAGER=cat",
		// belt-and-suspenders in case the runner has no global config
		"GIT_AUTHOR_NAME=Test User",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test User",
		"GIT_COMMITTER_EMAIL=test@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, string(out))
	}
}
