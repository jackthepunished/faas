// Tests for cmd/gregale/commands_release.go. The dispatch surface
// (usage, unknown subcommand, --help) is unit-testable; the bundle
// and install paths require a real Postgres (FAAS_PG_DSN) and are
// covered by cmd/e2e/release_install_test.go.
//
// The drift test (commands_completion_test.go:137) enforces that
// main.go's `case "release":` and cli_meta.go's cliCommand{Name: "release"}
// are both present or both absent — those are not covered here.

package main

import (
	"strings"
	"testing"
)

func TestCmdReleaseDispatch_NoArgs(t *testing.T) {
	if code := cmdReleaseDispatch(nil); code != 1 {
		t.Errorf("cmdReleaseDispatch(nil) = %d, want 1", code)
	}
}

func TestCmdReleaseDispatch_Unknown(t *testing.T) {
	if code := cmdReleaseDispatch([]string{"wat"}); code != 1 {
		t.Errorf("cmdReleaseDispatch(wat) = %d, want 1", code)
	}
}

func TestCmdReleaseDispatch_Help(t *testing.T) {
	for _, h := range []string{"-h", "--help"} {
		if code := cmdReleaseDispatch([]string{h}); code != 0 {
			t.Errorf("cmdReleaseDispatch(%s) = %d, want 0", h, code)
		}
	}
}

func TestCmdReleaseBundle_Help(t *testing.T) {
	for _, h := range []string{"-h", "--help"} {
		if code := cmdReleaseBundle([]string{h}); code != 0 {
			t.Errorf("cmdReleaseBundle(%s) = %d, want 0", h, code)
		}
	}
}

func TestCmdReleaseBundle_MissingFlags(t *testing.T) {
	if code := cmdReleaseBundle(nil); code != 1 {
		t.Errorf("cmdReleaseBundle(nil) = %d, want 1", code)
	}
	if code := cmdReleaseBundle([]string{"--bin-dir=/tmp/x"}); code != 1 {
		t.Errorf("cmdReleaseBundle(missing git-sha) = %d, want 1", code)
	}
	if code := cmdReleaseBundle([]string{"--bin-dir=/tmp/x", "--git-sha=abc"}); code != 1 {
		t.Errorf("cmdReleaseBundle(missing manifest-hash) = %d, want 1", code)
	}
}

func TestCmdReleaseBundle_BadGitSHA(t *testing.T) {
	// 40 hex chars, but uppercase — releaseinstall rejects non-lowercase.
	if code := cmdReleaseBundle([]string{
		"--bin-dir=/tmp/x",
		"--git-sha=0123456789ABCDEF0123456789ABCDEF01234567",
		"--manifest-hash=sha256:" + strings.Repeat("a", 64),
	}); code != 1 {
		t.Errorf("cmdReleaseBundle(uppercase sha) = %d, want 1 (releaseinstall rejects)", code)
	}
}

func TestCmdReleaseInstall_Help(t *testing.T) {
	for _, h := range []string{"-h", "--help"} {
		if code := cmdReleaseInstall([]string{h}); code != 0 {
			t.Errorf("cmdReleaseInstall(%s) = %d, want 0", h, code)
		}
	}
}

func TestCmdReleaseInstall_MissingFlags(t *testing.T) {
	if code := cmdReleaseInstall(nil); code != 1 {
		t.Errorf("cmdReleaseInstall(nil) = %d, want 1", code)
	}
}