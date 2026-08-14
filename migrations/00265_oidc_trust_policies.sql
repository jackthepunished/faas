-- +goose Up
-- +goose StatementBegin

-- 00265_oidc_trust_policies.sql — ADR-101 / issue #270 / PR-A.
--
-- Per-(account_id, issuer_url) OIDC / keyless deploy auth policy.
-- One row per (account, issuer) pair the customer trusts. The
-- composite PK is the customer's authoritative answer to "which
-- CI runner can I trust to deploy my app?" — issuer_url is the
-- IdP's stable identifier (e.g. https://token.actions.githubusercontent.com
-- for GitHub Actions), and account_id is the platform-side binding.
--
-- Auto-create on first exchange (PR-A) means customers do not have
-- to touch the dashboard before their first CI deploy; the row
-- appears on the first POST /v1/auth/oidc/exchange call. The
-- `audit_login='auto'` column stamps the provenance so the
-- dashboard's "Refine" CTA can distinguish "you set this" from
-- "system defaulted this" (PR-C).
--
-- Columns:
--   account_id        uuid        NOT NULL FK accounts(id) ON DELETE CASCADE
--                 — the owning platform account. GDPR §17 G2 path
--                   deletes the row when the account goes away.
--   issuer_url        text        NOT NULL
--                 — the OIDC issuer URL (the IdP's stable identifier).
--                   The first-use auto-create reads this from the JWT
--                   envelope's `iss` claim on the first exchange for
--                   this account.
--   jwks_url          text        NOT NULL
--                 — the OIDC issuer's JWKS URL. HTTPS-only by
--                   policy (the edge-jwt posture trusts the same
--                   shape; pkg/edgejwks.ValidJWKSURL enforces the
--                   https:// scheme at Verify time).
--   audience          text[]      NOT NULL
--                 — the aud claims the customer pinned. Empty =
--                   permissive (the auto-create shape); populated
--                   by the dashboard's Refine form (PR-C).
--   subject_pattern   text        NULL
--                 — optional regex match on the JWT `sub` claim
--                   (e.g. `repo:OWNER/NAME:ref:refs/heads/main`).
--                   NULL/empty = no check (the auto-create shape).
--                   The regex is compiled at the edgejwks layer
--                   (pkg/edgejwks.VerifierRule.RequiredClaimPatterns)
--                   so this column stays stringy.
--   algorithms        text[]      NOT NULL
--                 — the closed alg set the customer is willing to
--                   accept (RS256/RS384/RS512/ES256/ES384/ES512).
--                   The auto-create shape pins [RS256] (the GitHub
--                   Actions default); the dashboard refine form
--                   lets the customer add ES*/PS* if their IdP issues
--                   them.
--   required_claims   jsonb       NOT NULL DEFAULT '{}'::jsonb
--                 — strict-equality gate on JWT claims. The
--                   edge-jwt loop (pkg/edgejwks/verifier.go:193+)
--                   walks this map; a missing key skips the check.
--                   The regex variant is RequiredClaimPatterns
--                   (added in PR-A, distinct from this column).
--   created_at        timestamptz NOT NULL DEFAULT now()
--                 — first-use auto-create stamps it; the
--                   dashboard refine UPDATE preserves it.
--   updated_at        timestamptz NOT NULL DEFAULT now()
--                 — every UPSERT bumps this column.
--   audit_login       text        NOT NULL
--                 — 'auto' on first-use auto-create; the
--                   customer's account_id later if the operator
--                   refines. Mirrors the github_installations
--                   column (PR-C compatibility).
--
-- Replay-safety: every DDL uses IF NOT EXISTS so a second
-- MigrateUp (e.g. after a partial-apply where the goose row was
-- lost) is idempotent. Same convention as 00059
-- (github_installations) and 00264 (deployments_secret_findings).
--
-- Slot history: 00265 was the next free real slot at PR-A's
-- authoring time (post-PR #896 tenant-surfaces PR-A, post-PR #895
-- jobs PR-A). No prior reservation existed at this slot.

create table if not exists oidc_trust_policies (
    account_id       uuid        not null
                                  references accounts(id) on delete cascade,
    issuer_url       text        not null,
    jwks_url         text        not null,
    audience         text[]      not null,
    subject_pattern  text        null,
    algorithms       text[]      not null,
    required_claims  jsonb       not null default '{}'::jsonb,
    created_at       timestamptz not null default now(),
    updated_at       timestamptz not null default now(),
    audit_login      text        not null,
    primary key (account_id, issuer_url)
);

-- The composite PK is the lookup path (the hot path on every
-- exchange). The (account_id, ...) sort matches the per-account
-- dashboard list scan (PR-C) without an extra index — packs
-- the heat into the same B-tree.

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
drop table if exists oidc_trust_policies;
-- +goose StatementEnd
