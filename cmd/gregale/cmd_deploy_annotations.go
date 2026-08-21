// cmd/gregale/cmd_deploy_annotations.go — closed-set validation +
// auto-capture helpers for the deployment annotations surface
// (issue #977 / ADR-116). Lives in its own file so cmdDeployTarball
// stays focused on its main path and so the helpers can be unit-
// tested without spinning up the flag machinery.
//
// Three pieces:
//
//  1. isValidDeploymentAnnotationTag — mirrors the DB CHECK
//     (closed-set enum) so a CLI typo never ships a half-formed
//     annotation row.
//
//  2. resolveDeployedBy — when --deployed-by is unset on the
//     cmdDeployTarball path, attempts to auto-capture `git config
//     user.name` from cwd. Mirrors the zero-config path in
//     cmd_deploy_zero_config.go:69-72 — swallows ErrNoGitConfigKey
//     to "" so a customer who never ran `git config --global
//     user.name "..."` is not blocked.
//
//  3. DeploymentAnnotationTags — exported vocabulary list, used
//     by tests + the auto-generated help text we emit in the
//     --tag validation error.

package main

import (
	"os"
)

// DeploymentAnnotationTags is the canonical closed-set vocabulary
// for the --tag flag (issue #977 / ADR-116). The DB CHECK
// (migrations/00346_deployments_annotation.sql) is the source of
// truth; this list mirrors it byte-for-byte. Any change here must
// land as a new migration that widens the CHECK (and ship with a
// new ADR).
var DeploymentAnnotationTags = []string{
	"incident_recovery",
	"hotfix",
	"scheduled_maintenance",
	"compliance_hold",
	"partner_request",
}

// isValidDeploymentAnnotationTag returns true iff s is in
// DeploymentAnnotationTags. Empty string is allowed (== no tag).
func isValidDeploymentAnnotationTag(s string) bool {
	if s == "" {
		return true
	}
	for _, t := range DeploymentAnnotationTags {
		if s == t {
			return true
		}
	}
	return false
}

// resolveDeployedBy picks the effective deployed_by for the
// cmdDeployTarball path. Precedence:
//
//  1. explicit — when the operator passed --deployed-by, that wins
//     verbatim (no auto-capture lookup). Lets CI override the git
//     config (e.g. when running as a service account).
//  2. cwd-in-git-repo — auto-capture `git config user.name` from
//     cwd. The same helper cmd_deploy_zero_config.go uses, so a
//     customer who deploys the same project via either path gets
//     the same stamp.
//  3. non-git or no-config — empty string; the column is nullable.
//
// Errors from the helper (non-ErrNoGitConfigKey git errors) are
// silently swallowed to "" so a transient git hiccup never blocks
// a deploy. The audit row will simply lack a deployed_by and the
// dashboard renders nothing.
func resolveDeployedBy(explicit string) string {
	if explicit != "" {
		return explicit
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	root, err := gitRootFromCwd(cwd)
	if err != nil {
		// Not in a git repo → no auto-capture possible.
		return ""
	}
	name, err := gitUserName(root)
	if err != nil {
		// ErrNoGitConfigKey already swallowed inside gitUserName;
		// any other error (config-file parse, permission denied)
		// falls through to "" rather than blocking the deploy.
		return ""
	}
	return name
}
