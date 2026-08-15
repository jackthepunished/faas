// Tests for the operator-side cosign keypair CLI
// (`gregale sign-keys init|rotate|status`). The dispatcher lives in
// commands_sign_keys.go; main_test.go pins its placement in the
// `run` switch. This file narrowly covers the leaves and the
// rotate-path safety rail (refuses overwrite without --force).
//
// Unlike commands_crons_update_test.go this surface is purely
// local — no httptest, no authedClient, no SDK round-trip —
// because operator-side keypair provisioning never hits apid. The
// only I/O the test exercises is the disk under t.TempDir() and
// the osStdout package seam.
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// swapIO captures writes to both osStdout (the package seam) and
// os.Stderr (which printErr + PrintUsage write to directly).
// os.Stdout and os.Stderr are *os.File in Go's stdlib — the
// stderr redirect uses a real temp file rather than a *bytes.Buffer.
// Returns the in-memory stdout buffer, a function that reads the
// stderr file into a string when called (deferred so the test body
// can run before the file is read), and a single restore func.
func swapIO(t *testing.T) (stdout *bytes.Buffer, readStderr func() string, restore func()) {
	t.Helper()
	var outBuf bytes.Buffer
	errFile, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatalf("create stderr temp: %v", err)
	}
	oldOut := osStdout
	oldErr := os.Stderr
	// Also swap the package var osStderr (commands3.go) so error-path
	// output routed through printErr's PrintWarn/renderAPIError flow
	// lands in the tempfile too. Same compatibility shim as
	// captureStderr in commands5_test.go (issue #744 / ADR-086).
	oldPkgErr := osStderr
	osStdout = &outBuf
	os.Stderr = errFile
	osStderr = errFile
	restore = func() {
		osStdout = oldOut
		os.Stderr = oldErr
		osStderr = oldPkgErr
		_ = errFile.Close()
	}
	readStderr = func() string {
		// Sync + close + read before the deferred restore runs.
		if err := errFile.Sync(); err != nil {
			t.Logf("stderr sync: %v", err)
		}
		path := errFile.Name()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Logf("stderr read: %v", err)
			return ""
		}
		return string(data)
	}
	return &outBuf, readStderr, restore
}

// swapStdout captures writes to osStdout only. Kept for tests that
// don't exercise the stderr path.
func swapStdout(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	var buf bytes.Buffer
	oldOut := osStdout
	osStdout = &buf
	return &buf, func() { osStdout = oldOut }
}

// --- parent dispatcher ------------------------------------------------------

func TestCmdSignKeys_NoArgs_PrintsUsage(t *testing.T) {
	_, readStderr, restore := swapIO(t)
	defer restore()

	if code := cmdSignKeys(nil); code != 1 {
		t.Errorf("cmdSignKeys(nil) = %d, want 1", code)
	}
	got := readStderr()
	if !strings.Contains(got, "usage: gregale sign-keys") {
		t.Errorf("stderr = %q, want usage line", got)
	}
	if !strings.Contains(got, "init|rotate|status") {
		t.Errorf("stderr = %q, want leaf enumeration", got)
	}
}

func TestCmdSignKeys_UnknownSubcommand(t *testing.T) {
	_, readStderr, restore := swapIO(t)
	defer restore()

	if code := cmdSignKeys([]string{"bogus"}); code != 1 {
		t.Errorf("cmdSignKeys(bogus) = %d, want 1", code)
	}
	if !strings.Contains(readStderr(), "unknown subcommand") {
		t.Errorf("stderr = %q, want 'unknown subcommand'", readStderr())
	}
	if !strings.Contains(readStderr(), "init, rotate, status") {
		t.Errorf("stderr = %q, want leaf enumeration", readStderr())
	}
}

// --- init -------------------------------------------------------------------

func TestCmdSignKeysInit_WritesBothFilesWithCanonicalModes(t *testing.T) {
	out, restore := swapStdout(t)
	defer restore()

	dir := t.TempDir()
	privPath := filepath.Join(dir, "sign.key")
	pubPath := filepath.Join(dir, "sign-pub.pem")

	if code := cmdSignKeysInit([]string{"--sign-key", privPath, "--verify-key", pubPath}); code != 0 {
		t.Fatalf("cmdSignKeysInit = %d, want 0", code)
	}

	privInfo, err := os.Stat(privPath)
	if err != nil {
		t.Fatalf("stat priv: %v", err)
	}
	if got := privInfo.Mode().Perm(); got != 0o440 {
		t.Errorf("priv mode = %#o, want 0o440", got)
	}
	pubInfo, err := os.Stat(pubPath)
	if err != nil {
		t.Fatalf("stat pub: %v", err)
	}
	if got := pubInfo.Mode().Perm(); got != 0o444 {
		t.Errorf("pub mode = %#o, want 0o444", got)
	}

	// And the loader accepts the freshly-written files. This is the
	// "operator can immediately restart gregale-imaged without any
	// post-fixup" guarantee — the v1 bootstrap.sh chown root:gregale
	// step (RETIRED 2026-08-15 by issue #911 / PR-1; v2 path is PR-X
	// `gregale secrets init`) only needs the file mode to be correct
	// on its own.
	privBytes, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatalf("read priv: %v", err)
	}
	if !strings.Contains(string(privBytes), "PRIVATE KEY") {
		t.Errorf("priv file missing PEM PRIVATE KEY block")
	}

	if !strings.Contains(out.String(), "Wrote") {
		t.Errorf("output = %q, want 'Wrote' status line", out.String())
	}
}

func TestCmdSignKeysInit_RefusesOverwrite(t *testing.T) {
	_, readStderr, restore := swapIO(t)
	defer restore()

	dir := t.TempDir()
	privPath := filepath.Join(dir, "sign.key")
	pubPath := filepath.Join(dir, "sign-pub.pem")

	// First init: succeeds.
	if code := cmdSignKeysInit([]string{"--sign-key", privPath, "--verify-key", pubPath}); code != 0 {
		t.Fatalf("first init: %d, want 0", code)
	}
	privBefore, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatalf("read priv before: %v", err)
	}

	// Second init without --force: refuses.
	if code := cmdSignKeysInit([]string{"--sign-key", privPath, "--verify-key", pubPath}); code != 1 {
		t.Errorf("second init without force: code = %d, want 1", code)
	}
	privAfter, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatalf("read priv after: %v", err)
	}
	if string(privBefore) != string(privAfter) {
		t.Error("priv file mutated despite refusal")
	}
	if !strings.Contains(readStderr(), "init failed") {
		t.Errorf("stderr = %q, want 'init failed' error line", readStderr())
	}
}

func TestCmdSignKeysInit_ExtraArgsRejected(t *testing.T) {
	_, readStderr, restore := swapIO(t)
	defer restore()

	dir := t.TempDir()
	privPath := filepath.Join(dir, "sign.key")
	pubPath := filepath.Join(dir, "sign-pub.pem")

	// Two positional args (init only accepts flags).
	if code := cmdSignKeysInit([]string{"--sign-key", privPath, "--verify-key", pubPath, "extra", "args"}); code != 1 {
		t.Errorf("code = %d, want 1 for extra positional args", code)
	}
	if !strings.Contains(readStderr(), "usage: gregale sign-keys init") {
		t.Errorf("stderr = %q, want usage line", readStderr())
	}
}

// --- rotate -----------------------------------------------------------------

func TestCmdSignKeysRotate_ForceRotates(t *testing.T) {
	out, restore := swapStdout(t)
	defer restore()

	dir := t.TempDir()
	privPath := filepath.Join(dir, "sign.key")
	pubPath := filepath.Join(dir, "sign-pub.pem")

	if code := cmdSignKeysInit([]string{"--sign-key", privPath, "--verify-key", pubPath}); code != 0 {
		t.Fatalf("seed init: %d, want 0", code)
	}
	privBefore, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatalf("read priv before: %v", err)
	}

	// rotate defaults to force=true; even bare `sign-keys rotate`
	// with explicit --sign-key/--verify-key should succeed and
	// replace the bytes.
	if code := cmdSignKeysRotate([]string{"--sign-key", privPath, "--verify-key", pubPath}); code != 0 {
		t.Fatalf("rotate: %d, want 0", code)
	}
	privAfter, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatalf("read priv after: %v", err)
	}
	if string(privBefore) == string(privAfter) {
		t.Error("rotate did not change priv bytes; force=true should have replaced")
	}

	// Mode survived the rotate.
	info, err := os.Stat(privPath)
	if err != nil {
		t.Fatalf("stat priv after rotate: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o440 {
		t.Errorf("priv mode after rotate = %#o, want 0o440", got)
	}

	if !strings.Contains(out.String(), "Rotated") {
		t.Errorf("output = %q, want 'Rotated' status line", out.String())
	}
}

func TestCmdSignKeysRotate_NoForce_Refuses(t *testing.T) {
	_, readStderr, restore := swapIO(t)
	defer restore()

	dir := t.TempDir()
	privPath := filepath.Join(dir, "sign.key")
	pubPath := filepath.Join(dir, "sign-pub.pem")

	if code := cmdSignKeysInit([]string{"--sign-key", privPath, "--verify-key", pubPath}); code != 0 {
		t.Fatalf("seed init: %d, want 0", code)
	}
	privBefore, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatalf("read priv before: %v", err)
	}

	// --force=false explicitly turns off the default. The expected
	// behaviour is refusal.
	if code := cmdSignKeysRotate([]string{"--sign-key", privPath, "--verify-key", pubPath, "--force=false"}); code != 1 {
		t.Errorf("rotate --force=false: code = %d, want 1", code)
	}
	privAfter, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatalf("read priv after: %v", err)
	}
	if string(privBefore) != string(privAfter) {
		t.Error("priv file mutated despite --force=false")
	}
	if !strings.Contains(readStderr(), "rotate failed") {
		t.Errorf("stderr = %q, want 'rotate failed' error line", readStderr())
	}
}

// --- status -----------------------------------------------------------------

func TestCmdSignKeysStatus_PresentFiles(t *testing.T) {
	out, restore := swapStdout(t)
	defer restore()

	dir := t.TempDir()
	privPath := filepath.Join(dir, "sign.key")
	pubPath := filepath.Join(dir, "sign-pub.pem")

	if code := cmdSignKeysInit([]string{"--sign-key", privPath, "--verify-key", pubPath}); code != 0 {
		t.Fatalf("seed init: %d, want 0", code)
	}
	out.Reset() // drop the init output so status assertions are clean

	if code := cmdSignKeysStatus([]string{"--sign-key", privPath, "--verify-key", pubPath}); code != 0 {
		t.Errorf("status = %d, want 0", code)
	}

	got := out.String()
	if !strings.Contains(got, "sign.key") {
		t.Errorf("status output missing sign.key label:\n%s", got)
	}
	// Mode renders as bare octal (e.g. "0440"), not Go's "0o440"
	// form — %o with no leading 0o. Verify the actual substring
	// produced by `fmt.Fprintf("%#o", 0o440)` is the rendered text
	// we ship.
	if !strings.Contains(got, "0440") {
		t.Errorf("status output missing priv mode 0440:\n%s", got)
	}
	if !strings.Contains(got, "sign-pub.pem") {
		t.Errorf("status output missing sign-pub.pem label:\n%s", got)
	}
	if !strings.Contains(got, "0444") {
		t.Errorf("status output missing pub mode 0444:\n%s", got)
	}
	if !strings.Contains(got, "sha256:") {
		t.Errorf("status output missing sha256 fingerprint:\n%s", got)
	}
}

func TestCmdSignKeysStatus_MissingFile_ReportsMissing(t *testing.T) {
	out, restore := swapStdout(t)
	defer restore()

	dir := t.TempDir()
	privPath := filepath.Join(dir, "sign.key") // never written
	pubPath := filepath.Join(dir, "sign-pub.pem")

	if code := cmdSignKeysStatus([]string{"--sign-key", privPath, "--verify-key", pubPath}); code != 0 {
		t.Errorf("status = %d, want 0 even with missing files (operator must see both rows)", code)
	}

	got := out.String()
	if !strings.Contains(got, "missing") {
		t.Errorf("status output missing 'missing' label:\n%s", got)
	}
	if !strings.Contains(got, privPath) {
		t.Errorf("status output missing priv path:\n%s", got)
	}
}

// --- run() dispatch routing -------------------------------------------------

// TestRun_DispatchSignKeys asserts the main run() switch routes
// `sign-keys init` into cmdSignKeysInit rather than falling through
// to the unknown-command branch. Uses tmpdir paths so no global
// state is touched.
func TestRun_DispatchSignKeys(t *testing.T) {
	dir := t.TempDir()
	privPath := filepath.Join(dir, "sign.key")
	pubPath := filepath.Join(dir, "sign-pub.pem")

	// Mirror the commands_crons_update_test.go setup pattern:
	// HOME + XDG_CONFIG_HOME redirect so the authedClient path
	// doesn't trip on a real home dir if any subcommand
	// accidentally calls it.
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	if code := run([]string{"sign-keys", "init", "--sign-key", privPath, "--verify-key", pubPath}); code != 0 {
		t.Errorf("run sign-keys init = %d, want 0", code)
	}
	// Sanity: the dispatch actually wrote the files.
	if _, err := os.Stat(privPath); err != nil {
		t.Errorf("dispatch did not route to cmdSignKeysInit: priv stat: %v", err)
	}
	if _, err := os.Stat(pubPath); err != nil {
		t.Errorf("dispatch did not route to cmdSignKeysInit: pub stat: %v", err)
	}
}

func TestRun_DispatchSignKeys_UnknownSubcommand(t *testing.T) {
	if code := run([]string{"sign-keys", "bogus"}); code != 1 {
		t.Errorf("run sign-keys bogus = %d, want 1", code)
	}
}
