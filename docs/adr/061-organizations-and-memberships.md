# ADR-061 · Organizations, memberships, and unpriced seats (IAM-6)

- **Status:** proposed
- **Date:** 2026-08-01
- **Decision:** Introduce an `orgs` table as the tenant for ownership, plan,
  quota, billing, and audit. Keep `accounts` as a human authentication
  identity that belongs to one or more orgs. Ship path-scoped APIs under
  `/v1/orgs/{org_slug}/...`, automatic personal-org compatibility for every
  account, and one existing plan/quota pool per org. Paid per-seat charging
  and SSO are deferred to follow-up issues.

- **Why:** Issue #190. Today `accounts.id` is the only principal: it owns
  every resource, carries the plan and provider identifiers, scopes gateway
  limits, and feeds scheduler / metering admission. A 5-engineer team has
  no way to share a single billing identity, role-gate access, or rotate a
  colleague's key without nuking the host session key. Sales stalls at
  the first team-sized customer. The change must not break §6.2 invariants
  and must land behind the existing personal-account flows so existing
  customers see no behaviour change during the cut-over.

  Two product decisions lock the shape of this ADR (locked with the
  product owner before drafting):

  1. Core teams first: organizations, memberships, invitations, RBAC,
     org-owned resources, and consolidated existing-plan billing. Paid
     per-seat charging and SSO are out of scope.
  2. APIs are path-scoped under `/v1/orgs/{org_slug}/...`. The session
     cookie never permanently binds an "active org" — clients pick the
     org on every request via the URL.

- **Definitions:**

  - *Account:* human authentication identity. Owns email, password,
    MFA, OAuth links, sessions, and login tokens. May belong to zero or
    more organizations. Always belongs to exactly one personal
    organization.
  - *Organization (org):* the shared tenant. Owns apps, projects,
    domains, builds, deployments, secrets, env, API keys, billing
    customer identifiers, dunning state, plan, and quotas. One personal
    organization per account is immutable: it cannot accept additional
    members, transfer ownership, or be deleted independently of the
    account.
  - *Personal org:* the 1-member org that holds an account's existing
    resources through the cut-over and any new resources until the
    customer creates a shared org.
  - *Shared (non-personal) org:* an org with at least one owner, zero or
    more additional members, and exactly one owner in this milestone.
    Membership is capped per plan (value authored in the financial
    model, encoded in `pkg/api/limits.go`); seats are not separately
    billed in this milestone.

- **Role vocabulary** (mirrors the §2 of issue #190 body; the RBAC
  engine keys all membership checks off this set):

  | Role | Capabilities |
  |---|---|
  | `owner` | org lifecycle, ownership transfer, role changes, invitations, resource writes, plan, billing |
  | `admin` | role changes, invitations, resource writes; cannot transfer ownership, change plan, or delete the org |
  | `developer` | application/deployment/configuration writes and operational reads |
  | `viewer` | read-only applications, deployments, usage, audit |
  | `billing` | billing, invoices, plan changes, usage reads; no application mutation |

  Exactly one `owner` exists per non-personal org. The last owner cannot
  self-remove, be removed, or be demoted; ownership transfer is the only
  way to vacate the role.

- **Wire / authorization contract:**

  - All org-scoped endpoints resolve the active org via URL slug.
  - Cross-org access returns `404 not_found` (IDOR convention used by
    `pkg/auth/middleware.LoadApp`; never leaks existence).
  - A known member with an insufficient role gets `403 forbidden`.
  - Plan caps surface as dedicated RFC 7807 codes so dashboards and the
    CLI can branch on them without parsing prose (see §"Stable error
    codes" below).
  - Session cookies remain bound to the human account and live `sid`
    only. Selecting an org is a per-request decision and never mutates
    the session envelope.
  - API keys bind to one organization in addition to their existing
    scopes. Effective permission is the intersection of key scopes and
    the creator's current membership role. Removing the creator's
    membership invalidates the key's use immediately.

- **Migration strategy: expansion / backfill / cut-over / contraction**

  The migration is additive over two releases and respects the §17
  invariants. Concretely:

  1. **Expansion** — add nullable `org_id` columns and the new tables
     (`orgs`, `org_memberships`, `org_invitations`). No code reads them
     yet.
  2. **Backfill** — every account gets one personal org and one owner
     membership; the account's plan, status, dunning fields, and provider
     identifiers are copied to the org; every tenant-root row is stamped
     with `org_id`. Slug generation is deterministic and collision-safe
     (a short hash suffix is appended when the slug shape collides).
  3. **Dual-write** — every account creation / plan change / status
     transition / provider write also updates the personal org inside
     the same transaction. Reads continue to use account fields for now.
     Shadow mismatch counters must read zero for one full metering
     cycle before the cut-over proceeds.
  4. **Cut-over** — readers switch to org fields; `account.{plan,status,…}`
     become a compatibility mirror of the personal org. Old flat routes
     keep returning the personal-org data only.
  5. **Contraction** — only after two clean releases: set `org_id` to
     `NOT NULL` on the tenant-root tables and add composite integrity
     constraints. Old account columns may stay one more release for
     rollback, then are removed in an append-only migration.

  `apid` remains the sole writer to customer-intent tables. `schedd`
  remains the sole writer to `instances`. `meterd` continues to walk a
  per-tenant list — that list becomes `orgs` once the cut-over begins.

- **Account deletion and GDPR (ADR-021 compatibility):**

  - Deleting a non-owner member removes their memberships, sessions,
    OAuth / password data, and API keys they created. Shared-org
    resources survive.
  - Deleting the sole owner of a shared org is refused with a
    dedicated 409; ownership transfer must happen first.
  - Deleting an account whose only org is the personal org uses the
    existing 30-day export → restore → hard-delete flow (ADR-021) and
    cascades the personal org with it.
  - Deleting a shared org is owner-only, uses its own
    `deleted_pending → hard-delete` lifecycle, and does **not** delete
    member accounts.
  - Audit events keep actor attribution even after membership is
    removed; `org_invitations.invited_by` / actor FK columns use
    `ON DELETE SET NULL` for that reason.

- **Stable error codes** (added to `pkg/api/errors.go`):

  | Code | HTTP | Meaning |
  |---|---|---|
  | `org_not_found` | 404 | slug does not resolve to an org the principal can see |
  | `org_slug_invalid` | 422 | slug fails the regex or shape check |
  | `org_slug_taken` | 409 | slug already in use |
  | `org_member_cap_exceeded` | 403 | org has reached `OrgMembersMax` for its plan |
  | `org_invitation_cap_exceeded` | 403 | org has reached `OrgPendingInvitationsMax` |
  | `org_role_forbidden` | 403 | authenticated member lacks the role for this action |
  | `org_already_member` | 409 | accepting account already has a membership in the org |
  | `org_invitation_invalid` | 410 | token unknown, consumed, or revoked |
  | `org_invitation_expired` | 410 | token past its `expires_at` |
  | `org_last_owner` | 409 | removing or demoting the only owner is refused |
  | `org_personal_immutable` | 409 | personal org cannot accept members or be deleted standalone |
  | `org_api_key_requires_org` | 409 | legacy API key needs to be re-bound to an org |

- **Plan / quota table changes (PR 1 placeholder):**

  `pkg/api/limits.go` gains two fields that PR 2 will populate from the
  financial model:

  - `OrgMembersMax` — maximum active members per non-personal org
  - `OrgPendingInvitationsMax` — maximum pending invitations per
    non-personal org

  These fields are added in PR 1 with explicit per-plan `0` rows and a
  `TestOrgMembersLimits_ZeroUntilAuthorised` test that pins the
  fail-closed contract. Authoritative values come from
  `ex44_faas_financial_model.xlsx` and **must** be added there before
  PR 2 lands. PR 1 does not invent numbers.

- **Rejected alternatives:**

  - *Header / session-selected active org.* Adding an `X-Active-Org`
    header or persisting an active org in the session envelope was
    rejected because cookies would permanently bind to one org even
    when a user belongs to many; selecting via URL keeps authorization,
    idempotency, logs, caches, and SDK calls explicit and verifiable.
  - *Permanent legacy account fallback.* Keeping account-owned
    resources indefinitely for existing customers would force every
    handler to branch on `account.OrgID IS NULL` forever. The
    automatic personal-org backfill removes that branch after one
    release.
  - *Pricing seats in this milestone.* Adding per-seat prices requires
    financial-model updates (Plan × Seats matrix, proration rules,
    provider subscription-item quantities) that are explicitly out of
    scope. PR 1 ships the limit fields but leaves the values at 0 until
    the workbook is updated.
  - *Merge IAM-1 / IAM-2 / IAM-3 work.* Scoped API keys, TOTP MFA,
    and session revocation were shipped ahead of orgs and remain
    forward-compatible; folding them into IAM-6 would force a
    redesign of every IAM primitive.

- **Out of scope (separate issues):**

  - Paid per-seat charging, proration, subscription quantities.
  - SAML / OIDC / Google Workspace / Okta / GitHub org SSO; email-
    domain auto-join.
  - Custom roles and user-defined permissions.
  - Multiple simultaneous owners (this milestone uses explicit
    ownership transfer).
  - Frontend org switcher / invitation / membership UI (handled in
    the extracted frontend repository).
  - Cross-org aggregate enterprise billing or consolidated invoices
    above a single org.

- **Compatibility notes:**

  - All existing APIs continue to work during the dual window. Flat
    routes resolve to the caller's personal org only; they never guess
    among shared orgs.
  - The new path-scoped APIs are additive; SDKs grow typed methods
    (`pkg/api/client.go`) and the SDK coverage gate passes.
  - `apid` is still the sole writer to customer-intent tables;
    `schedd` still owns `instances`; `vmmd` still owns Firecracker.
    These ownership rules are not changed.

- **Verification gates:** replay-safety on every migration; `make test`,
  `make lint`, `make spec-check`, `make sdk-check`, `make sqlc-check`,
  `make proto-check`, and the Postgres-backed e2e matrix listed in the
  implementation plan at `/Users/poyrazk/.claude/plans/lexical-roaming-candle.md`.
  VM-lifecycle code is unchanged; no metal re-run is required for this
  PR.