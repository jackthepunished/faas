# ADR-093 · `faas-deploy-action` customer-facing GitHub Action (issue #270)

- **Status:** accepted
- **Date:** 2026-08-11
- **Issue:** #270 (`SDK: Publish an official GitHub deploy action`)
- **Labels:** `tier-2-public-launch`, `customer-facing`, `dx`
- **Supersedes:** none (no first-party Action was published before this slice)
- **Related:** ADR-092 (headless source-ref deploy — the wire contract the
  Action wraps), ADR-012 (githubd install-token model — the server-side
  plumbing the Action depends on), ADR-020 (sealed env-var secrets;
  `GREGALE_API_KEY` is the bearer token the Action forwards), ADR-014
  (push-webhook bind flow — complementary, not replaced), spec §14
  milestone M5 (DEPLOY-PROV), docs/source-ref.md (`## GitHub Actions`
  section is the customer-facing entry point).

## Context

PR-A (#828) + PR-B (#832) + PR-D (#838) shipped the **headless**
source-ref deploy path end-to-end: `gregale deploy --repo OWNER/NAME
--ref <sha>` posts to `POST /v1/apps/{slug}/deployments/source-ref`,
and the webhook push-to-deploy loop is wired with per-tenant secrets
and the Checks API writer. But the explicit-CI shape — a team that
wants a workflow run on every push, not a push listener — has no
first-party story. Customers hand-roll a `gregale deploy --repo`
step in their `.github/workflows/`, with all the boilerplate that
implies: unpinned CLI install via `curl | bash`, no token-redacted
error annotations, no fixture-tested pattern, no per-step outputs.

Issue #270 ships the customer-facing Action: a separate
`poyrazK/faas-deploy-action` repo with a `composite` `action.yml`
that wraps the existing `POST /v1/apps/{slug}/deployments/source-ref`
endpoint (no server change required), pins a single `gregale` CLI
version per release, and surfaces RFC 7807 errors as redacted
GitHub annotations. The companion `gregale deploy --github` flag
in this repo emits a copy-paste-ready workflow snippet so a customer
can go from `gregale connect` to a working Actions workflow in one
command.

## Three alternatives

| Axis | Option 1 (composite + vendored CLI) | Option 2 (external userland script) | Option 3 (JavaScript Action) |
|------|--------------------------------------|--------------------------------------|------------------------------|
| Runtime dependency | None. Vendored binary committed to the repo per release. | `curl` + `sha256sum` + `tar` against GitHub Releases. | Node.js runtime in the Actions runner. |
| Determinism | Best. `cli-version` is the literal vendored binary. Bad input → literal vendored binary. | Worst. Re-downloads on every run; supply-chain surface is GitHub Releases. | Mixed. The Action JS pins a `@actions/*` version, but the wire shape is whatever the API has at request time. |
| Repo size | Heavier: ~15 MB binary in `bin/`. `git clone --depth 1` keeps day-to-day work fast. | Tiny. ~50 KB. | ~200 KB. |
| CI smoke surface | `actionlint` + `shellcheck` + the bash exit codes. | `actionlint` + a small bash wrapper. | node:test + a `node` runner. |
| Crash visibility | Reproducible. The vendored binary is the one shipped. | Variable. The customer's runner is one cached download away. | Variable. Node version drift. |
| Maintainer onboarding | The CLI is the contract. No new runtime. | New release-pipeline surface (SHAs, signing). | New npm/RFC 7807 / fetch implementation. |
| Customer trust story | "The vendored binary is the contract; here's the SHA." | "The release pipeline is signed; we trust the release commit." | "The npm package is published under the gregale org." |
| Spec compliance | Clean. Reuses poyrazK/faas LDFLAGS (`-X github.com/onebox-faas/faas/pkg/wire.Version=...`). | Same. | Reimplements the wire shape in TypeScript — drift risk. |

## Decision

Choose **option 1 (composite + vendored CLI)**.

The Action is a separate repo at `poyrazK/faas-deploy-action`. Layout:

```
faas-deploy-action/
├── action.yml                  # composite, runs ./bin/gregale
├── src/run.sh                  # bash wrapper: set -euo pipefail, export outputs
├── src/annotate.sh             # RFC 7807 → ::error, redacted
├── src/version.txt             # bundled CLI version (written by release.yml)
├── bin/gregale                 # linux-amd64 binary, vendored at release time
├── examples/basic.yml
├── test/fixture-workflow.yml
├── README.md
├── LICENSE
└── .github/workflows/
    ├── ci.yml                  # actionlint + shellcheck + fixture e2e
    ├── release.yml             # cross-build + SHA256SUMS + softprops/action-gh-release
    └── codeql.yml
```

The Action's release workflow runs `make build` for the linux-amd64
target with the same `LDFLAGS` (`pkg/wire.Version` stamping) that
`poyrazK/faas`'s `Makefile:15-17` already uses, generates a
`SHA256SUMS` file, and attaches both to a GitHub Release via
`softprops/action-gh-release@v2.6.2` (the same SHA pinned at
`poyrazK/faas/.github/workflows/cd-controlplane.yml:99-110`).

The companion `gregale deploy --github` flag in `poyrazK/faas`:

- New file `cmd/gregale/cmd_deploy_github.go` (snippet generator).
- New flag --github in `cmdDeployTarball` (`commands2.go:744-764`).
- Updated `cli_meta.go:280` Short description.
- New `cmd/gregale/cmd_deploy_github_test.go` (table-driven: bare,
  runner env, pinned SHA, secret redaction).
- New `TestCmdDeployTarball_GithubFlag` in `commands2_test.go`
  (wire-in coverage).

The snippet uses `${{ github.repository }}` / `${{ github.sha }}`
placeholders by default; when run inside an Actions runner
(`GITHUB_REPOSITORY` + `GITHUB_SHA` env vars are set), the snippet
hard-codes those values. The Action reference is pinned to
`@v1` (the user-confirmed shape from the plan) and a `# pin:`
comment line surfaces the immutable SHA for customers who want
reproducibility.

## Why no new server endpoint

The Action reuses the existing `POST /v1/apps/{slug}/deployments/source-ref`
endpoint (server-side handler at `cmd/apid/handlers_source_ref.go:1-156`,
SDK binding at `pkg/api/client.go:611-628` `DeployFromSourceRef`).
The wire shape is `{repo, ref, format}` — the Action sends
`format: "tarball"` exactly like the CLI does today. Server-side
resolves the install token from `github_installations` keyed by
`account_id` (not `repo_full_name`), so the Action has no
install-token plumbing of its own.

A new endpoint would have been duplicate code with a single
discriminator (the deployment kind, which already exists). The
audit row (`deploy.source_ref`) gains two new JSON fields
(`cli_version`, `action_version`) but no schema change — the
existing `audit_log` JSON column absorbs them.

## Why no OIDC in this slice

The first action uses the existing bearer-token contract
(`secrets.GREGALE_API_KEY`). OIDC / keyless requires a new
`pkg/auth/<provider>/` design and an ADR for the token-exchange
semantics. The issue body explicitly defers OIDC to a follow-up
proposal; the plan builds this in as a future ADR-094-track.

## Why no replacement of the webhook push-to-deploy path

PR-D's webhook loop is the right shape for teams that want
push-to-deploy. The Action is the right shape for teams that
want explicit-CI. Both stamp `DeploymentKind = "github"` on the
deployment row and the audit row distinguishes them by
`action_version` (the Action sets this; the webhook path sets
`null`). Customers pick one or the other — both are first-class.

## Consequences

- **Vendored binary = 15 MB repo.** Acceptable; `git clone
  --depth 1` keeps day-to-day work fast. The CI release workflow
  never re-downloads; the binary is the commit message.
- **No signed releases in v1.** cosign signing is deferred to
  v2 (per the plan R5 mitigation). The release notes call this
  out, and the SHA256SUMS file is the deterministic verification
  surface for v1.
- **The snippet generator is a CLI-only path.** Adding
  `gregale deploy --github` to a non-CI customer is a no-op
  (the snippet is documentation). The wiring is `cmdDeployTarball`
  → `cmdDeployGithubSnippet` in `cmd/gregale/cmd_deploy_github.go`.
- **No migration needed.** The audit-row fields live in
  `audit_log` JSON column (no schema change). Plan R6 documents
  this explicitly so the migration-slot gate doesn't flag a
  non-existent migration.

## Rollback

- The `gregale deploy --github` flag can be removed in a single
  revert; the Action repo is unaffected. Customers who copied
  the snippet still have a working install.
- The Action repo (poyrazK/faas-deploy-action) can be archived
  with no effect on existing `@v1` pin customers; no new
  customers can adopt, but already-pinned workflows continue.
- The ADR has no downstream ADRs to retire.
