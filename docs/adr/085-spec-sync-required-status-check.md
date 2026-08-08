# ADR-085 · Make spec-sync a required pre-merge status check (issue #745 / DEPLOY-PROV-9)

- **Status:** accepted
- **Date:** 2026-08-08
- **Issue:** #745 / DEPLOY-PROV-9
- **Supersedes:** none
- **Related:** ADR-038 (sdk-go public SDK surface); `Makefile:514-529` (`spec-sync` / `spec-check`); `.github/workflows/ci.yml:426-481` (`spec-check` job); ruleset `19061133` (main branch protection); memory [[spec-sync-stale-embed-on-openapi-change]]; memory [[golangci-lint-v2-4-0-handler-checklist]] (pre-PR aggregator precedent)

## Context

Two copies of the OpenAPI spec coexist in the tree:

- `api/openapi.yaml` — the hand-authored source of truth.
- `pkg/apid/openapi.yaml` — a checked-in copy consumed by `//go:embed` in `pkg/apid/openapi_handler.go:28-94` and served at `GET /v1/openapi.{yaml,json}` to SDK generators and `curl`.

A PR that edits `api/openapi.yaml` without re-running `make spec-sync` leaves the served copy out of date. Generated SDKs (`sdk/node`, `sdk/python`) and the dashboard's OpenAPI-driven forms silently disagree with the live API, producing "the API returned X but the SDK expected Y" bug reports. Memory [[spec-sync-stale-embed-on-openapi-change]] records this as a recurring footgun. The most recent hit was PR #725 (2026-08-07), caught only because a follow-up PR (`aaf8e0c8`) re-ran the full spec-check job.

### Audit of the existing gate

The drift gate already exists in three layers:

1. `Makefile:514-517` `spec-sync` — idempotent `cp api/openapi.yaml pkg/apid/openapi.yaml`.
2. `Makefile:523-529` `spec-check` — chains `spec-install → spec-lint → spec-sync → denylist-md`, runs the AST parity suite `TestSpecCompliance` (`cmd/apid/spec_compliance_test.go:167`), and asserts `git diff --exit-code -- $(SPEC) $(SPEC_EMBED) ...`.
3. `.github/workflows/ci.yml:426-481` `spec-check` job — runs `make spec-check` on every PR (no path scoping on `pull_request:`).

So drift detection is already in CI. The gap is that the gate is **advisory, not blocking**. The repo's branch protection is a GitHub ruleset (ID `19061133`) that only enforces `deletion` + `non_fast_forward` — no `required_status_checks`. A red spec-check does not grey the GitHub UI merge button; the merge contract relies on reviewer discipline.

## Decision

1. **Required status check (load-bearing).** Add a `required_status_checks` rule to ruleset `19061133` requiring `spec-check (OpenAPI lint + AST parity)` to pass before merge to `main`. Strict mode **off** initially — the gate applies to commits pushed after the ruleset is in place, so currently-open PRs are not blocked. Bypass actors remain empty (`current_user_can_bypass: "never"` matches the existing semantics).

2. **Pre-PR aggregator (DX).** Add `make pre-pr` — a local one-liner that runs `spec-check` → `proto-check` → `sqlc-check` → `egress-check` → `sdk-gen`. Each sub-target already asserts its own clean diff; the aggregator is the "I am about to push" shortcut. Does NOT cover CI jobs that require Postgres service containers (lint+build, unit-tests, e2e).

3. **Pre-push hint (DX).** Add `.githooks/pre-push` — a non-blocking (`exit 0`) warning printed when `api/openapi.yaml` is in the push range and `pkg/apid/openapi.yaml` is stale. Enable once per clone via `git config core.hooksPath .githooks`.

4. **Job-name registry.** Maintain `docs/ci-required-checks.md` as the source-of-truth table for which CI job protects which file family. Future job renames that don't update the table will silently stop matching the ruleset — the table is the tripwire.

5. **Out of scope for this ADR.** Tightening to require `lint + build`, the unit-test shards, `CodeQL`, etc. The 2026-08-08 audit of open PRs (#763, #762, #761, #754, #753) showed several are red on those checks; requiring them now would block merges. They are listed in `docs/ci-required-checks.md` as "next up" so the path is documented.

### Rationale

- **Why ruleset not workflow-only?** A GitHub Actions workflow that fails is informational; a ruleset `required_status_checks` rule is the only thing that greys the merge button. Without the ruleset layer, the `aaf8e0c8`-style manual fix-up commit is load-bearing human process — the very thing the issue wants to eliminate.
- **Why spec-check only?** It is the only job that catches the issue's specific footgun, and at the time of writing all 6 evaluable open PRs (#762, #761, #753, #752, #664, #630) are green on it. Tightening to more jobs is a follow-up that can land when the open PR backlog clears.
- **Why strict mode off?** Open PRs created before this ruleset change do not have a green spec-check on their latest commit; strict mode would block them. Off = the gate applies to NEW pushes only. The user can flip strict to `true` later via the same `gh api PUT` shape.
- **Why no banner comment in `pkg/apid/openapi.yaml`?** `make spec-sync` is `cp api/openapi.yaml pkg/apid/openapi.yaml` — a banner would be clobbered on every sync. The five other signals (ruleset gate, CI job, `make pre-pr`, `.githooks/pre-push`, memory) cover the discoverability gap.

## Consequences

- A PR that edits `api/openapi.yaml` without re-running `make spec-sync` cannot be merged via the GitHub UI.
- The ruleset now depends on the exact job name `spec-check (OpenAPI lint + AST parity)`. A rename in `.github/workflows/ci.yml` will silently disable the gate; `docs/ci-required-checks.md` is the tripwire.
- Local devs who run `git push` without setting `core.hooksPath` lose the pre-push hint; document in CONTRIBUTING.
- `make pre-pr` is a NEW target — devs who run `make` (no args) won't see it in the `help` listing unless we add it to the index. Add a one-line entry near the top-level convenience targets if a follow-up wants discoverability.

## Rollback

Revert via the same `gh api PUT` shape, removing the `required_status_checks` rule from `rules[]`. The change is repo-settings only — no code revert needed; no PR revert needed.

## Verification

The load-bearing acceptance gate is a **throwaway drift PR**: open a PR that intentionally drifts `api/openapi.yaml` (one-line edit), leave `pkg/apid/openapi.yaml` untouched, and confirm:

1. `spec-check (OpenAPI lint + AST parity)` job goes red.
2. The GitHub UI Merge button is greyed; the "Show all checks" tooltip references the failed check.

Push the matching `make spec-sync` regeneration as a follow-up commit. Confirm spec-check flips green and the merge button re-enables. Close the throwaway PR without merging.
