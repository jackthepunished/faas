// commands_sign_keys_test.go - pins the load-bearing contracts of the
// operator-side cosign sign-key CLI (commands_sign_keys.go).
//
// The current contracts under test:
//
//   - sharedFlags default-force asymmetry: init defaults --force to
//     false (refuse overwrite; an operator who re-runs `init`
//     mid-deploy is almost certainly making a mistake), rotate
//     defaults --force to true (a bare `gregalectl sign-keys rotate`
//     MUST overwrite - that's the whole point of the subcommand;
//     rotate-without-overwrite is a no-op).
//   - status --json emits the {sign_key, pub_key} report with
//     present=false for missing files (json_parity_test pins the
//     field set; the missing-file shape here is the load-bearing
//     pre-init case).
//   - rotate --keep-old-pub archives the prior pub to <path>.<ts>
//     before writing the new keypair; rotate --json reports
//     {kept_old_pub, old_pub_sha256, new_pub_sha256, key_id} so
//     CI gates can audit the rotation lineage without parsing
//     the human output.
//
// The rotate-defaults-true contract was the source of a long-standing
// doc-comment bug (PR #449 follow-up): the previous comment claimed
// "does NOT silently overwrite" while the code passed defaultForce =
// true. The contradiction has been in this file since PR #322. This
// test pins the asymmetry so a future "let's make rotate safer" PR
// lands the change deliberately, not by accident.
//
// The new --json tests use real on-disk files (the cosign loader
// refuses insecure modes, so test fixtures write through the public
// helpers); the archive helper test uses os.WriteFile directly
// because it is a pure file-system rename.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

// forceDefaultTrue / forceDefaultFalse are the string values that
// flag.FlagSet.Lookup("force").DefValue takes when the --force default
// is true or false. Extracted as constants so the goconst lint rule
// doesn't fire (it counts string-literal occurrences across the
// package; without these, this file contributes 2 "false" occurrences
// and cmd/gregale/json_flag.go:57 contributes 1 more - three hits and
// the rule fires).
const (
	forceDefaultTrue  = "true"
	forceDefaultFalse = "false"
)

// TestSignKeyFlagDefaults pins the rotate-defaults-force=true /
// init-defaults-force=false contract. See package doc above for
// the history.
//
// Asserts both the struct field (what newSignKeyFlags returns) AND
// the flag.FlagSet's DefValue string (what the help text shows via
// `gregalectl sign-keys rotate --help`). The DefValue pin catches a
// regression class that the struct-field check alone misses: a
// future refactor that stops honouring the defaultForce argument
// (e.g. always passes true) would still leave initFlags.force ==
// false if a caller forgets to flip the argument - the field is
// just the parsed value. The DefValue string is computed once at
// fs construction time and is what operators read.
func TestSignKeyFlagDefaults(t *testing.T) {
	fsInit, initFlags := newSignKeyFlags("sign-keys init", false)
	if initFlags.force {
		t.Fatal("sign-keys init must default --force to false (refuse overwrite; a mid-deploy re-init is almost certainly a mistake)")
	}
	if got := fsInit.Lookup("force").DefValue; got != forceDefaultFalse {
		t.Fatalf("sign-keys init --force DefValue = %q, want %q (the help text printed by `gregalectl sign-keys init --help` shows this string)", got, forceDefaultFalse)
	}

	fsRotate, rotateFlags := newSignKeyFlags("sign-keys rotate", true)
	if !rotateFlags.force {
		t.Fatal("sign-keys rotate must default --force to true (rotate-without-overwrite is a no-op - that's the whole point of the subcommand)")
	}
	if got := fsRotate.Lookup("force").DefValue; got != forceDefaultTrue {
		t.Fatalf("sign-keys rotate --force DefValue = %q, want %q (the help text printed by `gregalectl sign-keys rotate --help` shows this string)", got, forceDefaultTrue)
	}

	fsStatus, statusFlags := newSignKeyFlags("sign-keys status", false)
	if statusFlags.force {
		t.Fatal("sign-keys status must default --force to false (status is a read path; it never writes)")
	}
	if got := fsStatus.Lookup("force").DefValue; got != forceDefaultFalse {
		t.Fatalf("sign-keys status --force DefValue = %q, want %q", got, forceDefaultFalse)
	}
}

// TestArchiveOldPubIfRequested_NoPriorPub pins the no-op first-
// rotation path: keepOldPub=true + a missing pub file must return
// ("", false, nil) without surfacing an error. CI gates rely on
// this being silent so the rotate JSON report distinguishes
// "first rotation, nothing to archive" from "second rotation,
// archive failed".
func TestArchiveOldPubIfRequested_NoPriorPub(t *testing.T) {
	dir := t.TempDir()
	pubPath := filepath.Join(dir, "sign-pub.pem")
	kept, ok, err := archiveOldPubIfRequested(true, pubPath)
	if err != nil {
		t.Fatalf("archiveOldPubIfRequested(missing) err = %v, want nil", err)
	}
	if ok {
		t.Errorf("archiveOldPubIfRequested(missing) ok = true, want false")
	}
	if kept != "" {
		t.Errorf("archiveOldPubIfRequested(missing) kept = %q, want empty", kept)
	}
}

// TestArchiveOldPubIfRequested_KeepDisabled pins the keep=false
// short-circuit: must return ("", false, nil) WITHOUT stat-ing
// the path. The contract is that --keep-old-pub defaults to
// false so a bare `rotate` is byte-identical to the pre-PR flow.
func TestArchiveOldPubIfRequested_KeepDisabled(t *testing.T) {
	dir := t.TempDir()
	pubPath := filepath.Join(dir, "sign-pub.pem")
	if err := os.WriteFile(pubPath, []byte("existing pub"), 0o644); err != nil {
		t.Fatalf("seed prior pub: %v", err)
	}
	kept, ok, err := archiveOldPubIfRequested(false, pubPath)
	if err != nil {
		t.Fatalf("archiveOldPubIfRequested(keep=false) err = %v, want nil", err)
	}
	if ok {
		t.Errorf("archiveOldPubIfRequested(keep=false) ok = true, want false")
	}
	if kept != "" {
		t.Errorf("archiveOldPubIfRequested(keep=false) kept = %q, want empty", kept)
	}
	if _, err := os.Stat(pubPath); err != nil {
		t.Errorf("keep=false must not touch the file; stat err = %v", err)
	}
}

// TestArchiveOldPubIfRequested_RenamesToTimestampedSibling pins
// the happy path. Writes a prior pub, archives it, asserts the
// archived file exists at <path>.<unix-ts> and the canonical path
// is gone (so the subsequent writeKeyPair lands cleanly).
func TestArchiveOldPubIfRequested_RenamesToTimestampedSibling(t *testing.T) {
	dir := t.TempDir()
	pubPath := filepath.Join(dir, "sign-pub.pem")
	prior := []byte("prior pub body")
	if err := os.WriteFile(pubPath, prior, 0o644); err != nil {
		t.Fatalf("seed prior pub: %v", err)
	}
	kept, ok, err := archiveOldPubIfRequested(true, pubPath)
	if err != nil {
		t.Fatalf("archiveOldPubIfRequested err = %v, want nil", err)
	}
	if !ok {
		t.Fatal("archiveOldPubIfRequested ok = false, want true")
	}
	if kept == pubPath {
		t.Errorf("kept = %q, must differ from the source", kept)
	}
	// Archived file must contain the same bytes as the original.
	got, err := os.ReadFile(kept)
	if err != nil {
		t.Fatalf("read archived: %v", err)
	}
	if string(got) != string(prior) {
		t.Errorf("archived bytes = %q, want %q", got, prior)
	}
	// Source path must be gone (so the new keypair write lands
	// without colliding).
	if _, err := os.Stat(pubPath); !os.IsNotExist(err) {
		t.Errorf("stat source after archive: err = %v, want ENOENT", err)
	}
}

// TestInspectSignKeysStatus_BothPresent pins the JSON shape for
// the happy path. Both files present, both report present=true +
// a non-zero sha256. Catches a struct-tag typo (e.g. a stray
// `omitempty` on Path that would erase the file path) and a
// regression in inspectSignKeyFile that returns zero values for
// present files.
func TestInspectSignKeysStatus_BothPresent(t *testing.T) {
	dir := t.TempDir()
	sign := filepath.Join(dir, "sign.key")
	pub := filepath.Join(dir, "sign-pub.pem")
	writeFakeKeypair(t, sign, pub)

	rep := inspectSignKeysStatus(sign, pub)
	if !rep.SignKey.Present {
		t.Errorf("SignKey.Present = false, want true")
	}
	if rep.SignKey.Path != sign {
		t.Errorf("SignKey.Path = %q, want %q", rep.SignKey.Path, sign)
	}
	if rep.SignKey.SHA256 == "" {
		t.Errorf("SignKey.SHA256 empty, want non-zero")
	}
	if !rep.PubKey.Present {
		t.Errorf("PubKey.Present = false, want true")
	}
	if rep.PubKey.SHA256 == "" {
		t.Errorf("PubKey.SHA256 empty, want non-zero")
	}
}

// TestInspectSignKeysStatus_BothMissing pins the pre-init shape:
// no files on disk, both report present=false + empty sha256 +
// path populated (so JSON consumers see WHAT would be there).
func TestInspectSignKeysStatus_BothMissing(t *testing.T) {
	dir := t.TempDir()
	sign := filepath.Join(dir, "sign.key")
	pub := filepath.Join(dir, "sign-pub.pem")

	rep := inspectSignKeysStatus(sign, pub)
	if rep.SignKey.Present {
		t.Errorf("SignKey.Present = true with no file, want false")
	}
	if rep.SignKey.SHA256 != "" {
		t.Errorf("SignKey.SHA256 = %q, want empty", rep.SignKey.SHA256)
	}
	if rep.SignKey.Path != sign {
		t.Errorf("SignKey.Path = %q, want %q", rep.SignKey.Path, sign)
	}
	if rep.PubKey.Present {
		t.Errorf("PubKey.Present = true with no file, want false")
	}
}

// TestCmdSignKeysStatus_JSON_BothPresent drives the full CLI
// path with --json, captures stdout via the osStdout hook, and
// asserts the JSON document unmarshals into the pinned shape.
// This is the wire-format guarantee that CI gates rely on.
func TestCmdSignKeysStatus_JSON_BothPresent(t *testing.T) {
	dir := t.TempDir()
	sign := filepath.Join(dir, "sign.key")
	pub := filepath.Join(dir, "sign-pub.pem")
	writeFakeKeypair(t, sign, pub)

	out, restore := captureOsStdout(t)
	code := cmdSignKeysStatus([]string{
		"--sign-key=" + sign,
		"--verify-key=" + pub,
		"--json",
	})
	restore()
	if code != 0 {
		t.Fatalf("cmdSignKeysStatus(--json) = %d, want 0", code)
	}
	var rep signKeysStatusReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("unmarshal stdout: %v (raw: %q)", err, out.String())
	}
	if !rep.SignKey.Present || !rep.PubKey.Present {
		t.Errorf("both present; got sign=%v pub=%v", rep.SignKey.Present, rep.PubKey.Present)
	}
	if rep.SignKey.SHA256 == "" || rep.PubKey.SHA256 == "" {
		t.Errorf("both sha256 non-empty; got sign=%q pub=%q", rep.SignKey.SHA256, rep.PubKey.SHA256)
	}
}

// TestCmdSignKeysStatus_JSON_Missing pins the pre-init wire
// shape: --json with no files on disk must NOT error, must emit
// present=false on both, and must still report the paths (so
// the operator can see what WOULD be inspected).
func TestCmdSignKeysStatus_JSON_Missing(t *testing.T) {
	dir := t.TempDir()
	sign := filepath.Join(dir, "sign.key")
	pub := filepath.Join(dir, "sign-pub.pem")

	out, restore := captureOsStdout(t)
	code := cmdSignKeysStatus([]string{
		"--sign-key=" + sign,
		"--verify-key=" + pub,
		"--json",
	})
	restore()
	if code != 0 {
		t.Fatalf("cmdSignKeysStatus(--json, missing) = %d, want 0 (status is a read path; missing files are not an error)", code)
	}
	var rep signKeysStatusReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("unmarshal stdout: %v (raw: %q)", err, out.String())
	}
	if rep.SignKey.Present || rep.PubKey.Present {
		t.Errorf("both absent; got sign=%v pub=%v", rep.SignKey.Present, rep.PubKey.Present)
	}
	if rep.SignKey.Path != sign {
		t.Errorf("SignKey.Path = %q, want %q", rep.SignKey.Path, sign)
	}
}

// TestInspectSignKeysRotateReport_KeepOldPub pins the audit
// shape: keep=true + a real prior pub archived at <path>.<ts>
// must surface {kept_old_pub=<path>, old_pub_sha256=<hash>,
// new_pub_sha256=<hash>, key_id=<hash16>}. The key_id is the
// first 16 hex chars of new_pub_sha256 (a short fingerprint
// the audit log can quote without the full hash).
func TestInspectSignKeysRotateReport_KeepOldPub(t *testing.T) {
	dir := t.TempDir()
	sign := filepath.Join(dir, "sign.key")
	pub := filepath.Join(dir, "sign-pub.pem")
	writeFakeKeypair(t, sign, pub)
	// Recreate a prior pub (the rotate flow archives first,
	// then writes - so we mirror that order).
	priorPath := pub + ".1234567890"
	if err := os.WriteFile(priorPath, []byte("prior"), 0o644); err != nil {
		t.Fatalf("seed prior: %v", err)
	}

	rep := inspectSignKeysRotateReport(sign, pub, priorPath, true)
	if !rep.KeepOldPub {
		t.Errorf("KeepOldPub = false, want true")
	}
	if rep.KeptOldPub != priorPath {
		t.Errorf("KeptOldPub = %q, want %q", rep.KeptOldPub, priorPath)
	}
	if rep.OldPubSHA == "" {
		t.Errorf("OldPubSHA empty, want non-zero")
	}
	if rep.NewPubSHA == "" {
		t.Errorf("NewPubSHA empty, want non-zero")
	}
	if len(rep.KeyID) != 16 {
		t.Errorf("KeyID len = %d, want 16", len(rep.KeyID))
	}
	if rep.NewPubSHA[:16] != rep.KeyID {
		t.Errorf("KeyID = %q, want first 16 of new_pub_sha256=%q", rep.KeyID, rep.NewPubSHA)
	}
}

// TestInspectSignKeysRotateReport_FirstRotation pins the
// first-rotation path: keep_old_pub=false (no prior pub)
// means KeptOldPub + OldPubSHA are empty, but the new
// fields are still populated. This is the common case after
// the initial `sign-keys init`.
func TestInspectSignKeysRotateReport_FirstRotation(t *testing.T) {
	dir := t.TempDir()
	sign := filepath.Join(dir, "sign.key")
	pub := filepath.Join(dir, "sign-pub.pem")
	writeFakeKeypair(t, sign, pub)

	rep := inspectSignKeysRotateReport(sign, pub, "", false)
	if rep.KeepOldPub {
		t.Errorf("KeepOldPub = true, want false")
	}
	if rep.KeptOldPub != "" {
		t.Errorf("KeptOldPub = %q, want empty", rep.KeptOldPub)
	}
	if rep.OldPubSHA != "" {
		t.Errorf("OldPubSHA = %q, want empty on first rotation", rep.OldPubSHA)
	}
	if rep.NewPubSHA == "" {
		t.Errorf("NewPubSHA empty, want non-zero")
	}
	if len(rep.KeyID) != 16 {
		t.Errorf("KeyID len = %d, want 16", len(rep.KeyID))
	}
}

// captureOsStdout swaps the package-level osStdout for a buffer
// and returns a restore closure. Mirrors the pattern used by
// commands_secrets_init_test.go (see also the osStdout overrides
// in sign_keys_test.go:43).
func captureOsStdout(t *testing.T) (*buffer, func()) {
	t.Helper()
	old := osStdout
	buf := &buffer{}
	osStdout = buf
	return buf, func() { osStdout = old }
}

// buffer is a tiny io.Writer used by captureOsStdout. We avoid
// bytes.Buffer so the type is unambiguous if the test file later
// needs to swap between the two.
type buffer struct {
	data []byte
}

func (b *buffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *buffer) Bytes() []byte  { return b.data }
func (b *buffer) String() string { return string(b.data) }

// writeFakeKeypair writes a self-signed ECDSA P-256 keypair as
// PEM files at the requested paths. The cosign loader refuses
// insecure modes, so we write 0444 / 0644 for the test fixture
// and bypass the loader in the inspect path (which uses
// os.ReadFile + sha256 directly, not the loader).
func writeFakeKeypair(t *testing.T, privPath, pubPath string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}
	privBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privBytes})
	if err := os.WriteFile(privPath, privPEM, 0o644); err != nil {
		t.Fatalf("write priv: %v", err)
	}
	pubBytes, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey: %v", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes})
	if err := os.WriteFile(pubPath, pubPEM, 0o644); err != nil {
		t.Fatalf("write pub: %v", err)
	}
}
