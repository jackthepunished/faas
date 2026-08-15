// constants.go — customer-side CLI constants (issue #911 / ADR-110 PR-6.5).
//
// After the CLI split, dispatch consts that are referenced from the
// customer's `case "..."` arms in main.go but whose commands were
// moved to `cmd/gregalectl/` are still needed here as plain string
// literals — Go's `switch case` requires a constant. Each const below
// is the literal `case` label that previously lived in the now-moved
// operator file; duplicating it here keeps main.go readable.
//
// Duplication is intentional (two binaries, two main packages, no
// shared internal package at this PR — see PR-7 follow-up).
package main

const (
	dispatchHostAge  = "host-age"
	dispatchPKI      = "pki"
	dispatchSignKeys = "sign-keys"
	dispatchNodeKey  = "node-key"
	dispatchBackup   = "backup"
	dispatchManifest = "manifest"
	dispatchRelease  = "release"

	subNodeInit   = "init"
	subNodeRotate = "rotate"
	subNodeStatus = "status"
)

// Note: dispatchTrustedPublishers is declared in
// commands_trusted_publishers.go (the file's own const), which
// stayed in cmd/gregale/ after PR-6.5 (it hits the admin API via
// authedClient — see plan §"Deviation").
