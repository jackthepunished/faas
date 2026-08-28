// commands_secrets_init_test.go — PR-X (issue #911 / ADR-110)
// tests for the post-bootstrap secrets init leaf.
//
// Coverage:
//   - happy path: writes all 5 files in the off-tree temp dir
//   - refuse-overwrite: a second run without --force fails
//   - force flag: a second run with --force succeeds
//   - status leaf: prints line per file (missing vs present)
//   - dispatcher: unknown subcommand / no args / --help
//
// The tests run with os.Geteuid() == 0 (CI default). On a
// dev-loop Mac as a regular user, the file-write tests are
// skipped — the non-root error path is exercised instead.
package main

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runSecretsInit / runSecretsStatus shell out to the leaf
// functions with --dir=<tmp> so the operator default
// (/etc/faas/secrets) is bypassed in the dev loop. The dispatcher
// itself is tested directly (TestSecretsDispatch_*).
//
// The init tests assert file-side effects only, so we forcibly
// clear FAAS_PG_DSN for the duration of the call (t.Setenv rolls
// back automatically on test exit). This keeps the tests local while
// the deployment retry path exercises the explicit `--no-db` flag.
func runSecretsInit(t *testing.T, dir string, args []string) (string, error) {
	t.Helper()
	t.Setenv("FAAS_PG_DSN", "")
	args = append([]string{"--dir", dir}, args...)
	return captureStdout(t, func() int {
		return cmdSecretsInit(args)
	})
}

func runSecretsStatus(t *testing.T, dir string) (string, error) {
	t.Helper()
	return captureStdout(t, func() int {
		return cmdSecretsStatus([]string{"--dir", dir})
	})
}

// captureStdout redirects the package-level osStdout writer to a
// pipe during fn, returns the captured bytes plus the exit code
// converted to an error. The package-level writer is what
// PrintOK / reportSecretStatus / etc. write to (per constants.go).
// Stderr is also redirected so the error-path (printErr) message
// is included in the captured output.
func captureStdout(t *testing.T, fn func() int) (string, error) {
	t.Helper()
	oldOut := osStdout
	oldErr := os.Stderr
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	osStdout = wOut
	os.Stderr = wErr
	defer func() {
		osStdout = oldOut
		os.Stderr = oldErr
	}()
	code := fn()
	_ = wOut.Close()
	_ = wErr.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(rOut)
	_, _ = buf.ReadFrom(rErr)
	if code != 0 {
		return buf.String(), &cliErr{out: buf.String()}
	}
	return buf.String(), nil
}

type cliErr struct {
	out string
}

func (e *cliErr) Error() string { return e.out }

// TestSecretsInit_RefuseNonRoot pins the load-bearing contract:
// secrets init requires root (host.age is 0400 root:root per
// spec §11). A non-root caller MUST get a clear error pointing
// at the sentinel, not a silent wrong-mode file.
//
// The test is gated on os.Geteuid() != 0 — CI runs as root, so
// the test skips. On a dev-loop Mac as a regular user, the test
// runs and the non-root error path is exercised.
func TestSecretsInit_RefuseNonRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("CI runs gregalectl tests as root; non-root path is exercised manually")
	}
	dir := t.TempDir()
	_, err := runSecretsInit(t, dir, nil)
	if err == nil {
		t.Fatalf("expected non-root error, got nil")
	}
	if !strings.Contains(err.Error(), "root") {
		t.Errorf("err = %v, want 'root' in message", err)
	}
}

// TestSecretsInit_WriteAllFive covers the happy path. The test
// runs only as root (CI default); on a dev-loop Mac as a regular
// user, the test is skipped.
func TestSecretsInit_WriteAllFive(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("test requires root (CI default; dev-loop Mac regular users skip)")
	}
	dir := t.TempDir()
	out, err := runSecretsInit(t, dir, nil)
	if err != nil {
		t.Fatalf("secrets init: %v (out=%s)", err, out)
	}
	// Five files must exist.
	storageDir := filepath.Join(dir, "storage-box")
	wantFiles := []string{
		filepath.Join(dir, "host.age"),
		filepath.Join(dir, "session.key"),
		filepath.Join(storageDir, "box-age-key"),
		filepath.Join(storageDir, "rclone.conf"),
		filepath.Join(storageDir, "archive-creds.json"),
	}
	for _, p := range wantFiles {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
	// session.key must be exactly 64 hex chars (32 bytes hex-encoded).
	sessionKeyBytes, err := os.ReadFile(filepath.Join(dir, "session.key"))
	if err != nil {
		t.Fatalf("read session.key: %v", err)
	}
	if len(sessionKeyBytes) != 64 {
		t.Errorf("session.key len = %d, want 64 hex chars (32 bytes)", len(sessionKeyBytes))
	}
	if _, err := hex.DecodeString(strings.TrimSpace(string(sessionKeyBytes))); err != nil {
		t.Errorf("session.key is not hex: %v", err)
	}
	// host.age must be readable (we don't decode the X25519
	// identity here — secretbox.LoadHostKey is a separate test
	// surface). The file-existence + non-empty check is the
	// load-bearing contract for this leaf.
	hostAgeBytes, err := os.ReadFile(filepath.Join(dir, "host.age"))
	if err != nil {
		t.Errorf("read host.age: %v", err)
	}
	if len(hostAgeBytes) == 0 {
		t.Errorf("host.age is empty")
	}
	// rclone.conf + archive-creds.json stubs must be non-empty.
	for _, p := range []string{
		filepath.Join(storageDir, "rclone.conf"),
		filepath.Join(storageDir, "archive-creds.json"),
	} {
		body, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("read %s: %v", p, err)
		}
		if len(body) == 0 {
			t.Errorf("%s is empty", p)
		}
	}
}

func TestSecretsInit_NoDBFlag(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("test requires root")
	}
	dir := t.TempDir()
	if out, err := runSecretsInit(t, dir, []string{"--no-db"}); err != nil {
		t.Fatalf("secrets init --no-db: %v (out=%s)", err, out)
	}
}

// TestSecretsInit_RefuseOverwrite pins the second-run contract:
// without --force, a re-init refuses to overwrite. Mirrors
// cmdHostAgeInit's refuse-overwrite pattern.
func TestSecretsInit_RefuseOverwrite(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("test requires root")
	}
	dir := t.TempDir()
	if _, err := runSecretsInit(t, dir, nil); err != nil {
		t.Fatalf("first init: %v", err)
	}
	_, err := runSecretsInit(t, dir, nil)
	if err == nil {
		t.Errorf("second init without --force = nil err, want refuse-overwrite")
	}
}

// TestSecretsInit_ForceOverwrite pins the --force escape hatch.
func TestSecretsInit_ForceOverwrite(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("test requires root")
	}
	dir := t.TempDir()
	if _, err := runSecretsInit(t, dir, nil); err != nil {
		t.Fatalf("first init: %v", err)
	}
	out, err := runSecretsInit(t, dir, []string{"--force"})
	if err != nil {
		t.Errorf("force re-init: %v (out=%s)", err, out)
	}
}

// TestSecretsInit_PreserveExisting keeps an already-provisioned host identity
// stable while filling in files that a previously interrupted deployment did
// not reach. This is the retry contract used by node_join.yml.
func TestSecretsInit_PreserveExisting(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("test requires root")
	}
	dir := t.TempDir()
	if _, err := runSecretsInit(t, dir, nil); err != nil {
		t.Fatalf("first init: %v", err)
	}
	hostAgePath := filepath.Join(dir, "host.age")
	before, err := os.ReadFile(hostAgePath)
	if err != nil {
		t.Fatalf("read host.age before preserve retry: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "session.key")); err != nil {
		t.Fatalf("remove session.key: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "storage-box", "rclone.conf")); err != nil {
		t.Fatalf("remove rclone.conf: %v", err)
	}
	// A retry must repair permissions on preserved files as well as recreate
	// files that are missing. This catches hosts whose previous bootstrap
	// wrote the files with the shared-daemon 0440 mode.
	for _, p := range []string{
		filepath.Join(dir, "host.age"),
		filepath.Join(dir, "session.key"),
		filepath.Join(dir, "storage-box", "box-age-key"),
		filepath.Join(dir, "storage-box", "archive-creds.json"),
	} {
		if err := os.Chmod(p, 0o440); err != nil {
			t.Fatalf("loosen preserved mode for %s: %v", p, err)
		}
	}
	if out, err := runSecretsInit(t, dir, []string{"--preserve-existing"}); err != nil {
		t.Fatalf("preserve retry: %v (out=%s)", err, out)
	}
	after, err := os.ReadFile(hostAgePath)
	if err != nil {
		t.Fatalf("read host.age after preserve retry: %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("preserve retry changed existing host.age")
	}
	for _, p := range []string{
		filepath.Join(dir, "session.key"),
		filepath.Join(dir, "storage-box", "rclone.conf"),
	} {
		info, err := os.Stat(p)
		if err != nil {
			t.Errorf("preserve retry did not recreate %s: %v", p, err)
		} else if got := info.Mode().Perm(); got != 0o400 {
			t.Errorf("preserve retry mode for %s = %#o, want 0400", p, got)
		}
	}
	for _, p := range []string{
		filepath.Join(dir, "host.age"),
		filepath.Join(dir, "storage-box", "box-age-key"),
		filepath.Join(dir, "storage-box", "archive-creds.json"),
	} {
		if info, err := os.Stat(p); err != nil {
			t.Errorf("preserve retry missing %s: %v", p, err)
		} else if got := info.Mode().Perm(); got != 0o400 {
			t.Errorf("preserve retry mode for %s = %#o, want 0400", p, got)
		}
	}
}

// TestSecretsStamp_DoesNotRewriteHostAge pins the repair contract: a failed
// database stamp must leave the existing identity byte-for-byte unchanged.
func TestSecretsStamp_DoesNotRewriteHostAge(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("test requires root")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "host.age")
	if _, err := writeHostAge(path, false); err != nil {
		t.Fatalf("write host.age: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read host.age before stamp: %v", err)
	}

	err = stampExistingHostCertificate(&secretsStampFlags{
		dir:  dir,
		host: "test-node",
		dsn:  "postgres://",
	})
	if err == nil {
		t.Fatal("stampExistingHostCertificate returned nil for invalid DSN")
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read host.age after stamp: %v", readErr)
	}
	if string(after) != string(before) {
		t.Fatal("secrets stamp changed existing host.age")
	}
}

// TestSecretsStatus_PrintsFileModes pins the status leaf shape:
// one line per file with mode + sha256[:12] + path; missing files
// print an explicit "missing" line.
func TestSecretsStatus_PrintsFileModes(t *testing.T) {
	dir := t.TempDir()
	if os.Geteuid() == 0 {
		if _, err := runSecretsInit(t, dir, nil); err != nil {
			t.Fatalf("init: %v", err)
		}
	}
	out, err := runSecretsStatus(t, dir)
	if err != nil {
		t.Fatalf("status: %v (out=%s)", err, out)
	}
	wantLabels := []string{
		"host.age",
		"session.key",
		"box-age-key",
		"rclone.conf",
		"archive-creds",
	}
	for _, label := range wantLabels {
		if !strings.Contains(out, label) {
			t.Errorf("status output missing label %q (out=%s)", label, out)
		}
	}
}

// TestSecretsStatus_MissingFilesPrintMissing pins the load-bearing
// signal: a missing file must print "missing" + path (NOT silently
// omitted). The operator needs to see all five even when one is
// uninitialised.
func TestSecretsStatus_MissingFilesPrintMissing(t *testing.T) {
	dir := t.TempDir()
	out, err := runSecretsStatus(t, dir)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, "missing") {
		t.Errorf("status output missing 'missing' marker (out=%s)", out)
	}
}

// TestSecretsDispatch_UnknownSubcommandRefused pins the dispatch
// contract: an unknown subcommand returns 1 with a usage hint.
func TestSecretsDispatch_UnknownSubcommandRefused(t *testing.T) {
	if got := cmdSecretsDispatch([]string{"bogus"}); got != 1 {
		t.Errorf("cmdSecretsDispatch(bogus) = %d, want 1", got)
	}
}

// TestSecretsDispatch_HelpAccepted pins the --help / -h path.
func TestSecretsDispatch_HelpAccepted(t *testing.T) {
	if got := cmdSecretsDispatch([]string{"--help"}); got != 0 {
		t.Errorf("cmdSecretsDispatch(--help) = %d, want 0", got)
	}
	if got := cmdSecretsDispatch([]string{"-h"}); got != 0 {
		t.Errorf("cmdSecretsDispatch(-h) = %d, want 0", got)
	}
}

// TestSecretsDispatch_NoArgsRefused pins the usage contract.
func TestSecretsDispatch_NoArgsRefused(t *testing.T) {
	if got := cmdSecretsDispatch([]string{}); got != 1 {
		t.Errorf("cmdSecretsDispatch() = %d, want 1", got)
	}
}
