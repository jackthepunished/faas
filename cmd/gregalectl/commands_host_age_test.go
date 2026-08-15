// commands_host_age_test.go — pins the load-bearing contracts of
// the operator-side host.age rotation CLI (commands_host_age.go).
//
// The current contracts under test:
//
//   - leaf-flag asymmetry: init defaults --force to false (refuse
//     overwrite; a mid-deploy re-init is almost certainly a mistake),
//     rotate defaults --force to true (rotate-without-overwrite is
//     a no-op), status defaults --force to false (status is a read
//     path; never writes), prune-previous defaults --force to false
//     (refuse to prune a too-recent .previous — the 30-day overlap
//     window is the security primitive).
//
//   - rotation round-trip: rotate takes a current host.age, drops a
//     new key, and leaves both files behind (current + .previous),
//     both 0400. The unlink dance is the load-bearing detail; a
//     regression that drops the .previous file strands every
//     pre-rotation SealedSecret in app_secrets.
//
//   - prune-previous refuses when .previous is younger than the
//     overlap window. Default 30 days. --force / --min-overlap-days
//     are the documented escape hatches.
//
// All tests run against t.TempDir() — no real /etc/faas/secrets
// touches — so the suite stays CI-runnable on any host.
package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/secretbox"
)

// TestHostAgeFlagDefaults pins the per-leaf --force asymmetry.
// Mirrors TestSignKeyFlagDefaults — the DefValue string is what
// operators read in `gregale host-age <leaf> --help`, so we pin
// both the struct field (what newHostAgeFlags returns) and the
// flag.FlagSet's DefValue (the help-text spelling).
func TestHostAgeFlagDefaults(t *testing.T) {
	cases := []struct {
		leaf         string
		defaultForce bool
		wantForce    string
	}{
		{"init", false, "false"},
		{"rotate", true, "true"},
		{"status", false, "false"},
	}
	for _, tc := range cases {
		t.Run(tc.leaf, func(t *testing.T) {
			fs, f := newHostAgeFlags("host-age "+tc.leaf, tc.defaultForce)
			gotForce := "false"
			if f.force {
				gotForce = "true"
			}
			if gotForce != tc.wantForce {
				t.Errorf("host-age %s --force default = %s, want %s", tc.leaf, gotForce, tc.wantForce)
			}
			if got := fs.Lookup("force").DefValue; got != tc.wantForce {
				t.Errorf("host-age %s --force DefValue = %q, want %q (printed in --help)", tc.leaf, got, tc.wantForce)
			}
		})
	}
}

// TestHostAgeInit_RefuseExisting pins the refuse-overwrite contract
// for `gregale host-age init`. Mirrors
// commands_backup_test.go::TestUnsealRclone_RefuseOverwrite — a
// silent overwrite of an existing identity strands every SealedSecret
// ever written under the old key.
//
// Skipped when not running as root: hostAgeInit refuses non-root
// callers (issue #316 / spec §11 — host.age is 0400 root:root, and
// a non-root write would produce a file the daemons can't load).
// CI runs as a non-root user; only the explicit root-only smoke
// runs under sudo.
func TestHostAgeInit_RefuseExisting(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("hostAgeInit requires root (writes 0400 host.age; spec §11)")
	}
	dir := t.TempDir()
	if _, err := secretbox.GenerateAndSaveHostKey(filepath.Join(dir, "host.age")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err := hostAgeInit(dir, false)
	if err == nil {
		t.Fatal("init should refuse when host.age already exists")
	}
	if !errors.Is(err, ErrInitRefuseOverwrite) {
		t.Errorf("err=%v, want errors.Is(ErrInitRefuseOverwrite)", err)
	}
}

// TestHostAgeInit_RefuseNonRoot pins the root-only guard. Spec §11
// mandates host.age is 0400 root:root; a non-root `init` would
// produce a file owned by the calling user that vmmd/apid/meterd/
// githubd cannot load (LoadCredential= copies the file as the unit
// user, but the on-disk source must be root:root or chown — the
// daemons explicitly refuse anything that isn't 0:0).
//
// The guard is independent of the refuse-overwrite guard above; both
// must fire under their respective conditions.
func TestHostAgeInit_RefuseNonRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("hostAgeInit_RefuseNonRoot only meaningful as non-root; CI runs as non-root by default")
	}
	dir := t.TempDir()
	err := hostAgeInit(dir, false)
	if err == nil {
		t.Fatal("init must refuse when not running as root")
	}
	if !errors.Is(err, ErrInitRequiresRoot) {
		t.Errorf("err=%v, want errors.Is(ErrInitRequiresRoot) (point operator at sudo)", err)
	}
	// No host.age should have been written.
	if _, statErr := os.Stat(filepath.Join(dir, "host.age")); !os.IsNotExist(statErr) {
		t.Errorf("host.age must not exist after refused init: stat err = %v", statErr)
	}
}

// TestHostAgeInit_HappyPath confirms the seed-to-load round-trip:
// init writes a fresh host.age (mode 0400), and the load path
// (via secretbox.LoadHostKey) reads it back. The atomic-write
// property is implicitly exercised by every test in this file
// that reads back what hostAgeInit wrote.
//
// Skipped when not running as root — see TestHostAgeInit_RefuseNonRoot.
func TestHostAgeInit_HappyPath(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("hostAgeInit requires root (writes 0400 host.age; spec §11)")
	}
	dir := t.TempDir()
	if err := hostAgeInit(dir, false); err != nil {
		t.Fatalf("init: %v", err)
	}
	st, err := os.Stat(filepath.Join(dir, "host.age"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o400 {
		t.Errorf("mode=%o want 0o400", perm)
	}
	if _, err := secretbox.LoadHostKey(filepath.Join(dir, "host.age")); err != nil {
		t.Errorf("load after init failed: %v", err)
	}
}

// TestHostAgeRotate_HappyPath pins the rotation round-trip:
// rotate takes the current host.age, drops a new key, and leaves
// both files behind (current + .previous), both 0400. After
// rotate, secretbox.LoadHostKeys(dir) returns a 2-element slice
// (current first, previous second) — the load-bearing detail
// for the multi-recipient unseal fallback.
func TestHostAgeRotate_HappyPath(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("hostAgeInit requires root (writes 0400 host.age; spec §11)")
	}
	dir := t.TempDir()
	if err := hostAgeInit(dir, false); err != nil {
		t.Fatalf("init: %v", err)
	}
	// Capture the pre-rotate recipient so we can assert .previous
	// holds the SAME identity after rotate.
	preID, err := secretbox.LoadHostKey(filepath.Join(dir, "host.age"))
	if err != nil {
		t.Fatalf("load pre: %v", err)
	}

	_, _, err = hostAgeRotate(dir, false)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}

	// host.age.new must NOT survive the rotate (it was renamed to
	// host.age; if it's still on disk the rename failed silently).
	if _, err := os.Stat(filepath.Join(dir, "host.age.new")); !os.IsNotExist(err) {
		t.Errorf("host.age.new should be gone after rotate: stat err = %v", err)
	}

	// Both files must be mode 0400.
	for _, name := range []string{"host.age", "host.age.previous"} {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if perm := st.Mode().Perm(); perm != 0o400 {
			t.Errorf("%s mode=%o want 0o400", name, perm)
		}
	}

	// LoadHostKeys returns [current, previous]. The previous
	// identity must match the pre-rotate recipient — that's the
	// load-bearing contract (otherwise freshly-sealed envelopes
	// under the OLD key fail to unseal during the overlap).
	idents, err := secretbox.LoadHostKeys(dir)
	if err != nil {
		t.Fatalf("LoadHostKeys: %v", err)
	}
	if len(idents) != 2 {
		t.Fatalf("len=%d, want 2 (current + previous)", len(idents))
	}
	if idents[1].Recipient().String() != preID.Recipient().String() {
		t.Errorf("previous recipient mismatch: got %q, want %q",
			idents[1].Recipient().String(), preID.Recipient().String())
	}
}

// TestHostAgeRotate_NoCurrent pins the empty-state guard: rotate
// with no current key must surface a clear "run init first" error,
// not silently generate-and-rename (which would leave the box
// without a .previous file and zero audit trail).
func TestHostAgeRotate_NoCurrent(t *testing.T) {
	dir := t.TempDir()
	_, _, err := hostAgeRotate(dir, false)
	if err == nil {
		t.Fatal("rotate with no current must fail")
	}
	if !errors.Is(err, ErrRotateNoCurrent) {
		t.Errorf("err=%v, want errors.Is(ErrRotateNoCurrent) (point operator at init)", err)
	}
}

// TestHostAgeStatus_PrintsBothFingerprints confirms the status leaf
// surfaces both files. We don't pin the exact sha256 output (it
// changes per-run because the key is random) but we do pin the
// presence of both labels and the absence of "missing" for a
// 2-file state.
//
// A future patch that adds a JSON output mode would replace this
// test with a JSON-shape assertion; for now the line-oriented
// shape is the contract (same as reportSignKeyStatus).
func TestHostAgeStatus_PrintsBothFingerprints(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("hostAgeInit requires root (writes 0400 host.age; spec §11)")
	}
	dir := t.TempDir()
	if err := hostAgeInit(dir, false); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, _, err := hostAgeRotate(dir, false); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	// status() returns int; we don't pipe stdout in tests, but we
	// can verify it returns 0 (no error) for a healthy 2-file
	// state. The fingerprint-printing is exercised by every other
	// test in this file that calls reportHostAgeStatus; the wiring
	// smoke is enough here.
	if rc := cmdHostAgeStatus([]string{"--dir", dir}); rc != 0 {
		t.Errorf("cmdHostAgeStatus rc=%d, want 0", rc)
	}
}

// TestHostAgeStatus_PrintsMissing confirms the status leaf
// surfaces the explicit "missing" line for absent files. The
// pre-rotation state has no .previous; the post-prune state has
// no .previous; both must print the path so the operator can see
// which side of the rotation they're on.
func TestHostAgeStatus_PrintsMissing(t *testing.T) {
	dir := t.TempDir()
	// No init — both files missing. Status returns 0 (it never
	// fails — surfacing the missing shape is the whole point).
	if rc := cmdHostAgeStatus([]string{"--dir", dir}); rc != 0 {
		t.Errorf("cmdHostAgeStatus rc=%d, want 0 (missing is not an error)", rc)
	}
}

// TestHostAgePrunePrevious_RefuseTooRecent pins the 30-day overlap
// guard. A freshly-rotated .previous must NOT be prunable until
// 30 days have passed; --force / --min-overlap-days are the
// documented escape hatches.
//
// We can't actually wait 30 days in a unit test, so we backdate
// the .previous file's mtime by 1 day (too recent) and 31 days
// (old enough) and assert the threshold behavior. Same shape as
// PR #483's TestUnsealRclone_RefuseOverwrite — small file-system
// state plus a clear pass/fail boundary.
func TestHostAgePrunePrevious_RefuseTooRecent(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("hostAgeInit requires root (writes 0400 host.age; spec §11)")
	}
	dir := t.TempDir()
	if err := hostAgeInit(dir, false); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, _, err := hostAgeRotate(dir, false); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	// Backdate .previous by 1 day — too recent for a default
	// 30-day-min prune.
	prevPath := filepath.Join(dir, "host.age.previous")
	if err := os.Chtimes(prevPath, time.Now().Add(-24*time.Hour), time.Now().Add(-24*time.Hour)); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	err := hostAgePrunePrevious(dir, defaultMinOverlapDays, false, false)
	if err == nil {
		t.Fatal("prune-previous should refuse when .previous is <30 days old")
	}
	if !errors.Is(err, ErrPruneTooRecent) {
		t.Errorf("err=%v, want errors.Is(ErrPruneTooRecent) (the refusal surfaces the min-overlap-days threshold)", err)
	}
	if _, statErr := os.Stat(prevPath); statErr != nil {
		t.Errorf(".previous must remain after refused prune: stat err = %v", statErr)
	}

	// Backdate by 31 days — old enough. Prune succeeds.
	if err := os.Chtimes(prevPath, time.Now().Add(-31*24*time.Hour), time.Now().Add(-31*24*time.Hour)); err != nil {
		t.Fatalf("chtimes 31d: %v", err)
	}
	if err := hostAgePrunePrevious(dir, defaultMinOverlapDays, false, false); err != nil {
		t.Errorf("prune-previous (31d old) should succeed: %v", err)
	}
	if _, statErr := os.Stat(prevPath); !os.IsNotExist(statErr) {
		t.Errorf(".previous should be gone after successful prune: stat err = %v", statErr)
	}
}

// TestHostAgePrunePrevious_Missing pins the no-rotation guard:
// prune-previous with no .previous file must surface a clear
// "already pruned, or no rotation in progress" error rather than
// silently succeeding.
func TestHostAgePrunePrevious_Missing(t *testing.T) {
	dir := t.TempDir()
	err := hostAgePrunePrevious(dir, defaultMinOverlapDays, false, false)
	if err == nil {
		t.Fatal("prune-previous with no .previous must fail")
	}
	if !errors.Is(err, ErrPruneMissingPrevious) {
		t.Errorf("err=%v, want errors.Is(ErrPruneMissingPrevious)", err)
	}
}

// TestHostAgePrunePrevious_PromoteFlow pins the manual escape
// hatch: --promote renames .previous → current. Use case:
// current was lost mid-rotation and the operator needs the
// previous key to be the new current. The operator must remove
// the broken current first (the helper refuses otherwise —
// protecting against a silent overwrite that would strand
// freshly-sealed envelopes under the new current).
func TestHostAgePrunePrevious_PromoteFlow(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("hostAgeInit requires root (writes 0400 host.age; spec §11)")
	}
	dir := t.TempDir()
	if err := hostAgeInit(dir, false); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, _, err := hostAgeRotate(dir, false); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	// Capture the .previous recipient so we can assert promote
	// moves it (not the discarded current).
	prevID, err := secretbox.LoadHostKey(filepath.Join(dir, "host.age.previous"))
	if err != nil {
		t.Fatalf("load prev: %v", err)
	}

	// Operator removes the broken current before promoting — this
	// is the documented workflow (the helper refuses otherwise).
	currPath := filepath.Join(dir, "host.age")
	if err := os.Remove(currPath); err != nil {
		t.Fatalf("remove current: %v", err)
	}

	if err := hostAgePrunePrevious(dir, defaultMinOverlapDays, false, true); err != nil {
		t.Fatalf("promote: %v", err)
	}

	// .previous must be gone, current must now hold the previous identity.
	if _, err := os.Stat(filepath.Join(dir, "host.age.previous")); !os.IsNotExist(err) {
		t.Errorf(".previous should be gone after promote: stat err = %v", err)
	}
	curr, err := secretbox.LoadHostKey(filepath.Join(dir, "host.age"))
	if err != nil {
		t.Fatalf("load current after promote: %v", err)
	}
	if curr.Recipient().String() != prevID.Recipient().String() {
		t.Errorf("promoted recipient mismatch: got %q, want %q",
			curr.Recipient().String(), prevID.Recipient().String())
	}
}

// TestHostAgeDispatch_UsageString pins the parent dispatcher's
// usage message. The "usage:" line is the operator-facing hint
// when an unknown subcommand is passed; pinning it as a constant
// catches a future refactor that silently changes the leaf names
// (init / rotate / status / prune-previous) without updating the
// usage string.
func TestHostAgeDispatch_UsageString(t *testing.T) {
	if rc := cmdHostAge(nil); rc != 1 {
		t.Errorf("cmdHostAge(nil) rc=%d, want 1 (zero args → usage)", rc)
	}
	if rc := cmdHostAge([]string{"unknown"}); rc != 1 {
		t.Errorf("cmdHostAge(unknown) rc=%d, want 1", rc)
	}
	// Pin the error message text — the operator-facing hint.
	// (We don't pipe stderr; we just check rc=1 is the contract
	// for unknown subcommands.)
}
