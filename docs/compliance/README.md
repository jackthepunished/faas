# Compliance

This directory holds the evidence, policies, and procedures that back Gregale's
security and privacy attestations (SOC 2 Type 1, ISO 27001, GDPR DPA).

## Index

| Doc | Purpose | Owner | Status |
|---|---|---|---|
| [soc2-control-mapping.md](soc2-control-mapping.md) | Maps every SOC 2 Trust Services Criteria (TSC) control to the artifact in the codebase that satisfies it. | Platform | Draft |
| [iso27001-statement-of-applicability.md](iso27001-statement-of-applicability.md) | ISO/IEC 27001:2022 Annex A — Applicable / Not-applicable per control + rationale. | Platform | Open |
| [subprocessors.md](subprocessors.md) | Public sub-processor list with category, data, region, DPA reference. | Platform + Legal | Open |
| [subprocessors.json](subprocessors.json) | Source-of-truth JSON for the sub-processor list; `subprocessor-check` CI gate renders `subprocessors.md` from this. | Platform | Open |
| [subprocessor-archive.json](subprocessor-archive.json) | Removed sub-processors with effective date + removal reason. | Platform | Open |
| [responsible-disclosure.md](responsible-disclosure.md) | Public security disclosure policy + 24/72/7-day SLAs + PGP. | Security | Open |
| [../../SECURITY.md](../../SECURITY.md) | Repo-root mirror of `responsible-disclosure.md` (GitHub convention). | Security | Open |
| [vendor-risk-management.md](vendor-risk-management.md) | Tier classification (critical / important / general) + per-tier assessment depth + re-assessment cadence. | Security | Open |
| [vendor-assessments/](vendor-assessments/) | One file per critical-tier vendor with questionnaire + DPA + decision. | Security | Open |
| [access-review.sql](access-review.sql) | Quarterly SQL template joining `accounts`, `keys`, `sessions`, `invitations`, `events`, `usage_monthly`. | Security | Open |
| [access-review.md](access-review.md) | Runbook for the quarterly review — when / who / what to do with findings. | Security | Open |

## Companion documents outside `docs/compliance/`

- `docs/DPA.md` — the in-repo Data Processing Addendum template. Production binding lives at `/etc/faas/dpa.md` (rendered from a template, single source of truth).
- `docs/faas_implementation_spec.md` §5.1 (audit event taxonomy), §11 (security hardening checklist), §12 (observability SLOs), §17 (known gaps register — GDPR G6 lives there).
- `docs/adr/020-customer-secrets.md` (sealed secrets), `039-server-side-session-revocation.md` (sessions), `042-webhook-replay-protection.md` (replay dedupe), `054-...` (code signing), `077-step-up-mfa.md`, `079-liveness-probe-restart-wedged-vm.md`.

## Tracking

The issue that drives this directory is
[#755 — Compliance attestations: SOC 2 Type 1 + ISO 27001 + GDPR DPA](https://github.com/poyrazK/faas/issues/755).
A full implementation plan lives at `$CLAUDE_JOB_DIR/tmp/755-plan/plan.md` and
as a comment on that issue.

## Conventions

- Every page that touches customer data must link back to one of the GDPR / DPA
  artifacts here or in `docs/DPA.md`.
- Every page that names a control (SOC 2 / ISO 27001) must cite the
  cross-reference it relies on (spec §, ADR, code path) so the auditor can
  re-derive the evidence from a fresh checkout.
- All sub-processor changes go through `subprocessor-check` (see PR-3 plan in
  the issue). The 30-day notice is enforced by the CI gate, not by humans.
