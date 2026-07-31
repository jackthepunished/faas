// commands_sign_keys_test.go — pins the load-bearing contracts of the
// operator-side cosign sign-key CLI (commands_sign_keys.go).
//
// The current contracts under test:
//
//   - sharedFlags default-force asymmetry: init defaults --force to
//     false (refuse overwrite; an operator who re-runs `init`
//     mid-deploy is almost certainly making a mistake), rotate
//     defaults --force to true (a bare `gregale sign-keys rotate`
//     MUST overwrite — that's the whole point of the subcommand;
//     rotate-without-overwrite is a no-op).
//
// The rotate-defaults-true contract was the source of a long-standing
// doc-comment bug (PR #449 follow-up): the previous comment claimed
// "does NOT silently overwrite" while the code passed defaultForce =
// true. The contradiction has been in the file since PR #322. This
// test pins the asymmetry so a future "let's make rotate safer" PR
// lands the change deliberately, not by accident.
//
// No file-system or cosign-package touches here — `newSignKeyFlags`
// only constructs a flag.FlagSet with defaults; we don't even call
// fs.Parse, so no key files are needed.
package main

import "testing"

// TestSignKeyFlagDefaults pins the rotate-defaults-force=true /
// init-defaults-force=false contract. See package doc above for
// the history.
func TestSignKeyFlagDefaults(t *testing.T) {
	_, initFlags := newSignKeyFlags("sign-keys init", false)
	if initFlags.force {
		t.Fatal("sign-keys init must default --force to false (refuse overwrite; a mid-deploy re-init is almost certainly a mistake)")
	}
	_, rotateFlags := newSignKeyFlags("sign-keys rotate", true)
	if !rotateFlags.force {
		t.Fatal("sign-keys rotate must default --force to true (rotate-without-overwrite is a no-op — that's the whole point of the subcommand)")
	}
	_, statusFlags := newSignKeyFlags("sign-keys status", false)
	if statusFlags.force {
		t.Fatal("sign-keys status must default --force to false (status is a read path; it never writes)")
	}
}
