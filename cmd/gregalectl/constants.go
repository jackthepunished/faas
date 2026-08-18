// constants.go — operator-side CLI constants (issue #911 / ADR-110 PR-6.5).
//
// This file holds the constants and package-level vars that are
// shared across the operator dispatcher files (manifest, release,
// host-age, pki, sign-keys, node-key, backup, trusted-publishers).
// The customer-side binary `gregale` has its own `commands3.go:37`
// for osStdout + `commands2.go` for subRotate, etc. After PR-6.5
// each binary is self-contained.
//
// Duplication is intentional (two binaries, two main packages, no
// shared internal package at this PR — see PR-7 follow-up).
package main

import "os"
import "io"

// Flag help aliases — duplicated from cmd/gregale/commands_billing.go.
// The operator dispatcher files (commands_manifest.go, commands_release.go,
// commands_node_key.go, etc.) reference these for `--help` / `-h` handling.
const (
	flagHelpShort = "-h"
	flagHelpLong  = "--help"
)

// subRotate is the literal "rotate" — the operator dispatcher files
// (commands_pki.go, commands_host_age.go, commands_sign_keys.go) use
// this for the `rotate` verb under their respective dispatch
// commands. The customer-side binary's `commands2.go:41` defines the
// same const; PR-6.5 duplicates it here to keep both binaries
// self-contained.
const subRotate = "rotate"

// osStdout is the operator-side package-level stdout writer. Mirrors
// cmd/gregale/commands3.go:37. Tests in cmd/gregalectl swap this
// pointer (analogous to the existing cli_test.go pattern) to capture
// output; production code uses it directly via fmt.Fprintln / PrintOK.
//
// Declared here (not in commands3.go, which is customer-side) so the
// operator package compiles in isolation. Same shape as the
// customer-side declaration.
var osStdout io.Writer = os.Stdout

// osStderr is the operator-side stderr seam. Mirrors
// cmd/gregale/commands3.go:42 — tests use it to capture error-path
// output without touching the real os.Stderr file descriptor.
// Declared here so the operator package compiles in isolation.
var osStderr io.Writer = os.Stderr
