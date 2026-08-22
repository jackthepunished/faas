// commands_sign_keys.go — operator-side CLI for the cosign sign-key
// pair (ADR-038 §Phase 3 / Tier 3). This is the surface PR #322
// (dbf89d1) deferred to a follow-up. It is the OPERATOR surface, not
// the customer surface: there is no `authedClient()` call, no SDK,
// no API call — every leaf is a local file-system operation
// against the canonical /etc/faas/secrets/ paths (or the
// caller-supplied --sign-key / --verify-key flags).
//
// The namespace `gregale keys` is already taken by the customer-facing
// API-key manager (cmdKeys in commands2.go:725-780 — every leaf
// calls authedClient() and hits apid). Operator-side provisioning
// has no business in that namespace; this is a separate top-level
// command `gregalectl sign-keys` with three leaves:
//   - init   — write a fresh keypair (refuses overwrite)
//   - rotate — write a fresh keypair with --force (overwrite allowed;
//     --keep-old-pub archives the prior pub file at
//     sign-pub.pem.<unix-ts> so verifiers rolling back
//     can re-pin the old public key mid-rotation)
//   - status — print mode + fingerprint + paths for both files
//     (--json emits a structured report for CI gates and
//     the json_parity_test discipline)
//
// All three leaves share the same flag surface (--sign-key,
// --verify-key, --force). The default paths are the cosign
// package's DefaultSignKeyPath / DefaultSignPubPath, which match
// what cmd/imaged and cmd/schedd load at startup. A reviewer
// changing one of those constants must also update this file's
// --help.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/onebox-faas/faas/pkg/cosign"
)

const dispatchSignKeys = "sign-keys"

// subInit / subStatus are the leaf names. subRotate is declared in
// commands2.go (shared across every resource's `… rotate …` literal
// so goconst stops flagging the cli_meta.go manifest).
const (
	subInit   = "init"
	subStatus = "status"
)

// cmdSignKeys is the parent dispatcher. With zero args it prints
// usage; with init/rotate/status it fans to the matching helper.
// Unknown subcommands return 1 with a usage hint — same contract
// as cmdBuild / cmdSecrets / cmdKeys.
func cmdSignKeys(args []string) int {
	parent, _ := lookupCliCommand("sign-keys")
	if len(args) == 0 {
		PrintUsage(os.Stderr, "usage: gregalectl sign-keys <init|rotate|status> [flags]", "sign-keys")
		return 1
	}
	switch args[0] {
	case subInit:
		return cmdSignKeysInit(args[1:])
	case subRotate:
		return cmdSignKeysRotate(args[1:])
	case subStatus:
		return cmdSignKeysStatus(args[1:])
	default:
		sug, _ := suggestSubcommand(args[0], parent)
		fmt.Fprintf(os.Stderr, "gregalectl sign-keys: unknown subcommand %q (known: init, rotate, status)\n", args[0])
		maybeSuggestSub(sug)
		return 1
	}
}

// sharedFlags builds the common --sign-key / --verify-key flag set.
// Both init and rotate use the same surface; status only needs the
// paths (no --force).
//
// force defaults: init=false (refuse overwrite by default; an operator
// who re-runs `init` mid-deploy is almost certainly making a mistake),
// rotate=true (a bare `gregalectl sign-keys rotate` MUST overwrite —
// that's the whole point of the subcommand; running rotate without
// overwrite is a no-op, see cmdSignKeysRotate body for the rotation
// flow).
//
// The rotate-true default was the source of a long-standing doc bug
// (PR #449 follow-up): the previous comment claimed "does NOT
// silently overwrite" while the code passed defaultForce = true. The
// contradiction has been in this file since PR #322. The asymmetry
// is load-bearing — TestSignKeyFlagDefaults pins it.
type signKeyFlags struct {
	signKey    string
	verify     string
	force      bool
	keepOldPub bool
	jsonOut    bool
}

func newSignKeyFlags(name string, defaultForce bool) (*flag.FlagSet, *signKeyFlags) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	f := &signKeyFlags{}
	fs.StringVar(&f.signKey, "sign-key", cosign.DefaultSignKeyPath,
		"path to the private key (mode 0440 root:gregale on the canonical install)")
	fs.StringVar(&f.verify, "verify-key", cosign.DefaultSignPubPath,
		"path to the public key (mode 0444, world-readable)")
	fs.BoolVar(&f.force, "force", defaultForce,
		"overwrite an existing keypair (rotate only)")
	fs.BoolVar(&f.keepOldPub, "keep-old-pub", false,
		"on rotate, archive the existing sign-pub.pem to <path>.<unix-ts> before writing the new keypair (lets verifiers re-pin the old pub mid-rotation)")
	fs.BoolVar(&f.jsonOut, "json", false,
		"emit structured JSON to stdout")
	return fs, f
}

// writeKeyPair is the shared write path. Status is the only leaf
// that doesn't call this. Both init and rotate converge here so
// any future change to the writer (e.g. switching
// WriteKeyPairForGroup → a KMS-backed writer) only has to land in
// one place. The error is annotated with the operator-facing hint
// because cmd/imaged and cmd/schedd both reference this same
// surface from their startup-error messages.
func writeKeyPair(force bool, privPath, pubPath string) error {
	privPEM, pubPEM, err := cosign.GenerateKeyPair()
	if err != nil {
		return fmt.Errorf("generate keypair: %w", err)
	}
	if err := cosign.WriteKeyPairForGroup(privPath, privPEM, pubPath, pubPEM, force); err != nil {
		return err
	}
	return nil
}

// cmdSignKeysInit writes a fresh keypair. Refuses to overwrite
// existing files. The caller is expected to be the bootstrap or
// an ansible task running as root (so the post-write chown to
// root:gregale succeeds). `gregalectl sign-keys init --force` is allowed
// for emergency re-init but the operator should normally use
// `gregalectl sign-keys rotate` for that flow — `init --force` skips
// the rename of the existing pub file that rotate performs in a
// future patch (out of scope here).
func cmdSignKeysInit(args []string) int {
	fs, f := newSignKeyFlags("sign-keys init", false)
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() != 0 {
		PrintUsage(os.Stderr, "usage: gregalectl sign-keys init [flags]", "sign-keys")
		return 1
	}
	if err := writeKeyPair(f.force, f.signKey, f.verify); err != nil {
		return printErr("init failed", err)
	}
	PrintOK(osStdout, "Wrote %s (0440) and %s (0444)\n  Next: chown root:gregale %s && chmod 0440 %s\n  Next: chown root:root %s && chmod 0444 %s",
		f.signKey, f.verify,
		f.signKey, f.signKey,
		f.verify, f.verify)
	return 0
}

// cmdSignKeysRotate is the documented operator flow for replacing
// the keypair (compromise, scheduled rotation). Default force=true
// because rotate without overwrite is a no-op.
//
// --keep-old-pub archives the existing sign-pub.pem to
// <path>.<unix-ts> BEFORE the new keypair is generated. The
// archived copy lets a verifier mid-rotation re-pin the old pub
// without re-running rotate; without it, once schedd / imaged
// load the new pub, old signatures won't verify (verifier side
// has no rollback path). The flag is a no-op when the pub file
// does not exist (first rotation - nothing to archive).
//
// --json emits the {old, kept, new} report so CI gates can
// reason about the rotation lineage without parsing the human
// output. The kept_old_pub boolean is the audit trail signal:
// true iff --keep-old-pub was set AND the rename actually
// landed (a missing prior pub returns kept_old_pub=false, not an
// error - first-rotation is the common case).
func cmdSignKeysRotate(args []string) int {
	fs, f := newSignKeyFlags("sign-keys rotate", true)
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() != 0 {
		PrintUsage(os.Stderr, "usage: gregalectl sign-keys rotate [flags]", "sign-keys")
		return 1
	}
	keptPath, keptOldPub, keepErr := archiveOldPubIfRequested(f.keepOldPub, f.verify)
	if keepErr != nil {
		return printErr("rotate failed", keepErr)
	}
	if err := writeKeyPair(f.force, f.signKey, f.verify); err != nil {
		return printErr("rotate failed", err)
	}
	if f.jsonOut {
		jsonEmit(osStdout, inspectSignKeysRotateReport(f.signKey, f.verify, keptPath, keptOldPub))
		return 0
	}
	if keptOldPub {
		PrintOK(osStdout, "Rotated %s and %s (force=%t)\n  Archived prior pub: %s\n  Restart: systemctl restart gregale-imaged gregale-schedd",
			f.signKey, f.verify, f.force, keptPath)
		return 0
	}
	PrintOK(os.Stdout, "Rotated %s and %s (force=%t)\n  Restart: systemctl restart gregale-imaged gregale-schedd",
		f.signKey, f.verify, f.force)
	return 0
}

// cmdSignKeysStatus reports the mode + fingerprint for both files.
// Used by ansible stat-asserts at deploy time and by the operator
// during incident response. Output is line-oriented by default;
// --json emits a structured report for CI gates. Missing files
// print an explicit "missing" line and the leaf returns 0 - the
// operator should see both paths even if one is absent, so they
// can run `init` once.
func cmdSignKeysStatus(args []string) int {
	fs, f := newSignKeyFlags("sign-keys status", false)
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() != 0 {
		PrintUsage(os.Stderr, "usage: gregalectl sign-keys status [--json]", "sign-keys")
		return 1
	}
	if f.jsonOut {
		jsonEmit(osStdout, inspectSignKeysStatus(f.signKey, f.verify))
		return 0
	}
	for _, p := range []struct {
		path  string
		label string
	}{
		{f.signKey, "sign.key    "},
		{f.verify, "sign-pub.pem"},
	} {
		reportSignKeyStatus(osStdout, p.label, p.path)
	}
	return 0
}

// reportSignKeyStatus prints one line per file: <label>  <mode>
// <sha256[:12] of bytes>  <path>. Missing files print "<label>
// missing: <err>" so the operator can copy/paste the path straight
// into the next command. The mode is read with os.Stat; the
// fingerprint is read with os.ReadFile (not LoadPrivateKeyFile /
// LoadPublicKeyFile, because the loader refuses insecure modes and
// a misconfigured file should still report what it has).
func reportSignKeyStatus(w io.Writer, label, path string) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			_, _ = fmt.Fprintf(w, "%s  missing  %s\n", label, path)
			return
		}
		_, _ = fmt.Fprintf(w, "%s  stat error: %v  %s\n", label, err, path)
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		_, _ = fmt.Fprintf(w, "%s  mode %#o  read error: %v  %s\n", label, info.Mode().Perm(), err, path)
		return
	}
	sum := sha256.Sum256(data)
	_, _ = fmt.Fprintf(w, "%s  %#o  sha256:%s  %s\n", label, info.Mode().Perm(), hex.EncodeToString(sum[:6]), path)
}

// archiveOldPubIfRequested renames pubPath to <pubPath>.<unix-ts>
// when keep is true AND the file exists. Returns the archived
// path (empty if not archived) and the boolean that lands in
// the JSON report. First rotation (no prior pub) is a no-op:
// returns ("", false, nil) so the rotate path stays unchanged
// for the common case.
func archiveOldPubIfRequested(keep bool, pubPath string) (string, bool, error) {
	if !keep {
		return "", false, nil
	}
	info, err := os.Stat(pubPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("stat prior pub %s: %w", pubPath, err)
	}
	if !info.Mode().IsRegular() {
		return "", false, fmt.Errorf("prior pub %s is not a regular file (mode %s); refusing to archive", pubPath, info.Mode())
	}
	archived := pubPath + "." + strconv.FormatInt(time.Now().Unix(), 10)
	if err := os.Rename(pubPath, archived); err != nil {
		return "", false, fmt.Errorf("archive prior pub %s -> %s: %w", pubPath, archived, err)
	}
	return archived, true, nil
}

// signKeysRotateReport is the JSON shape for
// `sign-keys rotate --json`. The fields are pinned by
// json_parity_test so CI gates can rely on the schema. The
// old/new sha256s are the post-rotation values; the old pub
// sha256 is empty when kept_old_pub=false (no prior pub to
// fingerprint).
type signKeysRotateReport struct {
	SignKey    string `json:"sign_key"`
	VerifyKey  string `json:"verify_key"`
	KeepOldPub bool   `json:"keep_old_pub"`
	KeptOldPub string `json:"kept_old_pub,omitempty"` // path to archived prior pub (empty when not kept)
	OldPubSHA  string `json:"old_pub_sha256,omitempty"`
	NewPubSHA  string `json:"new_pub_sha256"`
	KeyID      string `json:"key_id"` // sha256[:16] of the new pub bytes; short fingerprint for audit logs
}

// inspectSignKeysRotateReport hashes the post-rotation files
// and (if kept) the archived prior pub, then composes the
// report struct. The function reads file bytes (not the
// cosign loader) so the same path-status logic as status /
// reportSignKeyStatus applies - a misconfigured mode still
// surfaces a hash, not a hard failure.
//
// sha256Hex is the package-shared helper (commands_secrets_init.go)
// which returns []byte; we convert to string at the assignment
// sites so the JSON struct fields stay typed.
func inspectSignKeysRotateReport(signKey, verifyKey, keptOldPubPath string, keptOldPub bool) signKeysRotateReport {
	rep := signKeysRotateReport{
		SignKey:    signKey,
		VerifyKey:  verifyKey,
		KeepOldPub: keptOldPub,
		KeptOldPub: keptOldPubPath,
	}
	if keptOldPub {
		if data, err := os.ReadFile(keptOldPubPath); err == nil {
			rep.OldPubSHA = string(sha256Hex(data))
		}
	}
	if data, err := os.ReadFile(verifyKey); err == nil {
		sum := string(sha256Hex(data))
		rep.NewPubSHA = sum
		rep.KeyID = sum[:16]
	}
	return rep
}

// signKeyFileStatus is the per-file inspection shape used by
// both renderers (text + JSON). present=false means the file
// does not exist (pre-init / post-rotate before next init);
// mode + sha256 are zero-valued in the absent case so JSON
// consumers can rely on `present` as the existence gate.
type signKeyFileStatus struct {
	Present bool   `json:"present"`
	Path    string `json:"path"`
	Mode    string `json:"mode,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
}

// signKeysStatusReport is the JSON shape for
// `sign-keys status --json`. Field set is pinned by
// json_parity_test so CI gates can rely on the schema.
type signKeysStatusReport struct {
	SignKey signKeyFileStatus `json:"sign_key"`
	PubKey  signKeyFileStatus `json:"pub_key"`
}

// inspectSignKeysStatus reads mode + sha256 for both files.
// Mirrors reportSignKeyStatus byte-for-byte so the text + JSON
// renderers never disagree on what's on disk.
func inspectSignKeysStatus(signKey, verifyKey string) signKeysStatusReport {
	return signKeysStatusReport{
		SignKey: inspectSignKeyFile(signKey),
		PubKey:  inspectSignKeyFile(verifyKey),
	}
}

// inspectSignKeyFile reads mode + sha256 for one file. Missing
// files return present=false (other fields zero).
func inspectSignKeyFile(path string) signKeyFileStatus {
	info, err := os.Stat(path)
	if err != nil {
		return signKeyFileStatus{Path: path}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return signKeyFileStatus{Path: path, Mode: fmt.Sprintf("%#o", info.Mode().Perm())}
	}
	return signKeyFileStatus{
		Present: true,
		Path:    path,
		Mode:    fmt.Sprintf("%#o", info.Mode().Perm()),
		SHA256:  string(sha256Hex(data)),
	}
}
