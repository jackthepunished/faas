# ADR-034 · Scoped API keys

- **Status:** accepted
- **Date:** 2026-07-25
- **Decision:** Every API key carries an explicit `scopes text[]` set; the
  apid auth middleware checks the requested scope on every authenticated
  route. The vocabulary is `admin | read | write`. `admin` is the legacy
  full-access scope; `read` covers GETs; `write` covers POST/PUT/PATCH/DELETE.
  Per-resource fine-grained scopes (`deploy:write`, `secrets:read`, etc.)
  are deferred to IAM-2.

- **Why:** Today every API key is an admin token for the account that owns
  it. A customer's CI deploy key can read secrets, delete apps, change the
  plan, and call `/v1/compute-nodes` (when the account email is in
  `FAAS_ADMIN_EMAILS`). A leaked CI variable or compromised developer
  laptop exfiltrates the entire account — the deployed surface is
  indistinguishable from the control surface. The fix is a coarse
  permission split that lets a customer mint a deploy-only key without
  hand-rolling a second account.

- **Consequences:**

  - `api_keys` gains a `scopes text[] NOT NULL DEFAULT '{admin}'` column
    (migration 00035). The default backfills existing rows to admin so
    the migration is zero-downtime — every key minted before this ADR
    keeps the same effective authority it had yesterday.
  - `state.APIKey` gains a `Scopes []string` field. `Store.CreateAPIKey`
    takes an explicit `scopes` argument. `Store.AuthenticateKey` is the
    canonical account+key lookup, returning both in one call.
  - `apid` middleware composes as `auth → requireScope → handler`. The
    principal is stashed in `r.Context()` so the 38 existing
    `accountHandler` bodies are untouched; the per-route scope check
    lives in the route table.
  - `POST /v1/keys` validates requested scopes against the closed
    vocabulary and rejects unknown scopes with `400 code:validation`.
    Omitting `scopes` defaults to `["admin"]` so existing SDK/CLI callers
    keep full access.
  - The existing `adminAllows` email-allowlist gate on `/v1/compute-nodes`
    is preserved. Scope is layered on top: a non-admin key never reaches
    the admin email check.
  - Session-cookie auth (dashboard) is implicitly admin. A human at the
    keyboard gets full access; an API key holder is the only principal
    that can be scoped down.
  - OpenAPI spec, SDK `CreateKey`, and DTOs gain a `scopes []string`
    field. `make spec-check` enforces parity.
  - The 401 → 403 path is now distinguishable: `code: unauthorized`
    means "log in", `code: insufficient_scope` means "your key does
    not have permission for this endpoint".

- **Rejected alternatives:**

  1. **Fine-grained scopes from day one** (`deploy:write`, `secrets:read`,
     `usage:read`, `apps:read`, etc.). Rejected: locks in a vocabulary
     that may not match how customers actually want to split permissions,
     and a 6-route × 6-scope matrix is the kind of spec that grows
     without ever shrinking. The two-scope language (`read`/`write`)
     covers the 80% case; IAM-2 layers fine-grained scopes on top
     without breaking the existing vocabulary.
  2. **Per-resource scopes** ("this key can only deploy `app-foo`").
     Rejected: requires a resource-binding primitive (RBAC, ABAC, or
     signed JWTs) that the platform does not yet have. Defer to a
     later IAM phase.
  3. **RBAC roles** (`admin`, `developer`, `viewer`). Rejected: keys
     are minted by an account owner for their own use; the
     multi-user-account model is not on the v1 roadmap. Scopes on a
     key are simpler and cover the same threat model.
  4. **Multiple distinct API-key types** (`admin_key`, `deploy_key`).
     Rejected: a `type` column is a single-value categorical where
     scopes are a flexible set. A `type` field would need to be
     extended for every new permission split; scopes compose.
  5. **Bootstrap every key as admin and let customers opt down** vs.
     the inverse (no scopes → 403). Rejected the inverse: the
     /v1/keys CLI/SDK integration is already shipped in customer
     tooling (the `fp_live_…` mint path), and a deny-by-default
     migration would break production traffic. The "default
     admin" semantics preserve the pre-1.0 contract; customers
     who want least-privilege opt in explicitly.
  6. **Per-key IP allowlists, key expiry, automated rotation**.
     Rejected: separate IAM-3 phase. Permitting them now would
     inflate the surface; the scope primitive is the load-bearing
     one.
  7. **Using the existing `Label` column instead of a new column**.
     Rejected: `Label` is a free-form description rendered in the
     dashboard ("ci-deploy", "laptop"). Coupling authorization to
     a string the customer types at mint time is a typo-driven
     privilege escalation waiting to happen.
