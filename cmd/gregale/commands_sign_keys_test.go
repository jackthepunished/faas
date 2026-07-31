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

// forceDefaultTrue / forceDefaultFalse are the string values that
// flag.FlagSet.Lookup("force").DefValue takes when the --force default
// is true or false. Extracted as constants so the goconst lint rule
// doesn't fire (it counts string-literal occurrences across the
// package; without these, this file contributes 2 "false" occurrences
// and cmd/gregale/json_flag.go:57 contributes 1 more — three hits and
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
// `gregale sign-keys rotate --help`). The DefValue pin catches a
// regression class that the struct-field check alone misses: a
// future refactor that stops honouring the defaultForce argument
// (e.g. always passes true) would still leave initFlags.force ==
// false if a caller forgets to flip the argument — the field is
// just the parsed value. The DefValue string is computed once at
// fs construction time and is what operators read.
func TestSignKeyFlagDefaults(t *testing.T) {
	fsInit, initFlags := newSignKeyFlags("sign-keys init", false)
	if initFlags.force {
		t.Fatal("sign-keys init must default --force to false (refuse overwrite; a mid-deploy re-init is almost certainly a mistake)")
	}
	if got := fsInit.Lookup("force").DefValue; got != forceDefaultFalse {
		t.Fatalf("sign-keys init --force DefValue = %q, want %q (the help text printed by `gregale sign-keys init --help` shows this string)", got, forceDefaultFalse)
	}

	fsRotate, rotateFlags := newSignKeyFlags("sign-keys rotate", true)
	if !rotateFlags.force {
		t.Fatal("sign-keys rotate must default --force to true (rotate-without-overwrite is a no-op — that's the whole point of the subcommand)")
	}
	if got := fsRotate.Lookup("force").DefValue; got != forceDefaultTrue {
		t.Fatalf("sign-keys rotate --force DefValue = %q, want %q (the help text printed by `gregale sign-keys rotate --help` shows this string)", got, forceDefaultTrue)
	}

	fsStatus, statusFlags := newSignKeyFlags("sign-keys status", false)
	if statusFlags.force {
		t.Fatal("sign-keys status must default --force to false (status is a read path; it never writes)")
	}
	if got := fsStatus.Lookup("force").DefValue; got != forceDefaultFalse {
		t.Fatalf("sign-keys status --force DefValue = %q, want %q", got, forceDefaultFalse)
	}
}
