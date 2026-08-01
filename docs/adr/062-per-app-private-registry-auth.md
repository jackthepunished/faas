# ADR-062 — Per-app private-registry Basic Auth (issue #461)

- **Status:** proposed
- **Date:** 2026-08-01
- **Issue:** #461
- **Decision:** customers can store Basic Auth credentials per
  `(app_id, registry_host)` on Hobby+ plans; imaged threads the
  credential through `pkg/oci.RegistryClient` for pulls of that
  registry only. Free plans cannot store credentials
  (`Limits.RegistryCredentialMax = 0`). The existing
  `apps.egress_allowlist` per-app CIDR list remains authoritative;
  adding a credential does NOT bypass `pkg/oci.EgressDialContext`,
  and `oci_egress_deny_total` still increments on denied dials.

## Why

Today, `pkg/oci.RegistryClient.fetchToken` (`pkg/oci/registry.go:519`)
hard-codes `nil` for `*BasicAuth`. `pkg/oci/auth.go::FetchToken` /
`RefreshToken` already accept an optional `*BasicAuth` (nil = anonymous),
but no caller threads it. imaged pulls the app's manifest, config, and
layers anonymously. A Hobby+ customer who hosts the app on
`registry.gregale.dev` (or any private registry) gets a 401 at the
first `/v2/...` request and `POST /v1/apps/{slug}/deployments` fails
at the imaged step. There is no per-app way to store Basic Auth.

The acceptance criteria from issue #461:
- Customer can store Basic Auth for `registry.gregale.dev` per app;
  retrieve omits the password; delete works.
- `POST /v1/apps/{slug}/deployments` with
  `image: registry.gregale.dev/me/app@sha256:...` succeeds where it
  would otherwise return 401.
- No password material in any slog log line; no password in any
  GET response.
- Migration renumbered to avoid collisions (per
  `docs/adr/041-migration-slot-reservation.md`).
- `oci_egress_deny_total` shows the dial when the registry is not in
  the egress allowlist.

## Decision

**1. Per-app credential store keyed `(app_id, registry_host)`**,
account-scoped via `account_id` for ownership and `ON DELETE CASCADE`
on both `accounts(id)` and `apps(id)` so deletion cleans up. Storage
table `app_registry_credentials` (migration `00083_app_registry_credentials.sql`):
PK `(app_id, registry)` via `UNIQUE` constraint, `id` is a
`gen_random_uuid()` surrogate (audit/foreign-key friendliness).

**2. Registry host normalized at the API layer** (lowercase, no
scheme, no path, no trailing slash, port preserved). Storage stores
the normalized form. The same normalization runs at PUT and at
imaged's lookup; `ref.Host()` on the OCI reference parser returns
the same shape. Storing the normalized form means there is no
ambiguity between `ghcr.io`, `ghcr.io:443`, and `https://ghcr.io`.

**3. Password sealed at rest via `secretbox.SealBytes`**
(`pkg/secretbox/seal.go:183-214`) with `namespace="registry_creds"`.
Username stays in plaintext (metadata, mirrors the `AppSecret`
precedent where each value is sealed independently and metadata
isn't). `secretbox.OpenBytes` returns the namespace on open;
imaged verifies the namespace before accepting the unseal result
so a blob from a different namespace (e.g. a customer secret blob
landed in the wrong column) fails closed. The host age recipient
is the seal target (no DEK/KEK) — same threat model as
`AppSecret`.

**4. imaged unseals transiently in the pull path**;
plaintext lives only in the `buildImageLayer` call frame and is GC'd
on return. NEVER attached to dep, audit payload, log line, or error.
The mark-used call (`MarkAppRegistryCredentialUsed`) happens after a
successful authenticated pull; failure to update `last_used_at` is
non-fatal (warned but not fatal) so a successful deployment is not
derailed by a timestamping hiccup.

**5. `oci.Puller` interface stays untouched.** New additive
`oci.AuthPuller` interface (`PullDigestWithAuth`,
`PullImageConfigWithAuth`, `PullLayersWithAuth`). `RegistryClient`
implements both — old methods (`PullDigest`, etc.) delegate to the
new methods with `auth=nil`, preserving the existing anonymous
path. `DefaultPuller` (test/no-op impl) implements `AuthPuller` by
ignoring auth, preserving offline tests. imaged type-asserts
`oci.AuthPuller`; falls back to the anonymous methods when the
puller doesn't satisfy the interface (so existing test fakes that
only implement `oci.Puller` keep working without changes).

**6. Per-plan quota matrix: Free=0, Hobby=2, Pro=5, Scale=20.**
Free cannot store credentials. Hobby+ opt in. The matrix is in
`Limits.RegistryCredentialMax` (`pkg/api/limits.go`), the single
source of truth per CLAUDE.md. Updates to an existing
`(app_id, registry)` row do not consume quota — the upsert treats
the existing row as a replacement, so a customer can rotate their
password without bumping the count past the cap.

**7. Egress allowlist unchanged.** `apps.egress_allowlist`
(`pkg/state`) and `pkg/oci.EgressDialContext` (`pkg/oci`) remain
authoritative. Credentials do NOT bypass the dial; a registry host
that is not in the allowlist fails with `oci_egress_deny_total`
incrementing. This pins acceptance criterion #4.

**8. `LastUsedAt` updated only after a successful authenticated
pull.** The `MarkAppRegistryCredentialUsed` call returns
`ErrNotFound` if the row is gone (cascade-cleaned between pull
start and mark) — that's not an error for the deployment. Network
or DB failure on the mark call is warned but not fatal.

**9. HTTP DELETE uses query param `?registry=...`** (URL-encoded).
Registry hosts contain `:port`; the path segment form
(`/v1/apps/{slug}/registry-credentials/{registry}`) doesn't escape
cleanly. The query-param form mirrors `?limit=...&before=...` on
`ListAppSecretsForAccount` and similar list endpoints.

**10. IAM-4 audit events: `registry_credential.set` and
`registry_credential.deleted`.** Audit payloads carry `app_id`,
normalized `registry`, and `username` only. NEVER the password,
the ciphertext, or a base64-decoded `Authorization` header.
Logging uses `logsanitize.Field` for `registry` and `username`
(the same way `handlers_secrets.go` handles secret keys) and
NEVER the password value.

## Failure modes

| Scenario | Behaviour |
|---|---|
| Customer PUTs to Free plan | `ErrRegistryCredentialsNotAllowedOnPlan` 403 at apid; row never written. |
| Customer exceeds Hobby+ quota | `ErrRegistryCredentialsQuotaExceeded` 413 at apid with the cap and observed count. |
| Update of existing `(app_id, registry)` | Quota check passes (existing row acts as replacement); upsert overwrites username + ciphertext. |
| Malformed registry host (`http://...`, `/path`, control chars) | `ErrInvalidRegistryHost` 400 at apid; nothing written. |
| `secretbox.OpenBytes` returns wrong namespace | imaged rejects the unseal; pull fails with redacted error. |
| Host not in `apps.egress_allowlist` | `EgressDialContext` rejects the dial BEFORE the credential lookup runs; `oci_egress_deny_total` increments; deployment fails with the existing egress error. |
| apid boot without age recipient loaded | `ErrCapacity` 503 at PUT (recipient not loaded → refusing to seal). Same posture as `setSecret`. |
| imaged boot without identity | `ErrCapacity` 503 surfaced as imaged-pulled-failure; deployment fails closed. |
| MarkUsed race with account/app delete | Returns `ErrNotFound`; warned and ignored (deployment already succeeded). |
| Slot collision with sibling PR | Drop `00083_reserve_slot.sql` on rebase if the sibling PR landed first; bump to 84+ (ADR-041). |
| pgtest/test-metal skip | pgtest auto-skips when Postgres isn't reachable (existing helper); the unit suite is the defensive layer. |
| Slot 83 already taken on rebase | `make test` will catch it; renumber per ADR-041 with `git mv migrations/00083_*.{sql,_test.go} migrations/00084_*.{sql,_test.go}`. |

## Security

- §11 unchanged — no new cgroup / uid / netns surface; imaged is
  non-root; egress enforcement unchanged.
- Basic Auth sent over HTTPS only — the `bearerForChallenge` /
  `validateBearerRealm` gate at `pkg/storage/oci.go:1298-1346`
  already enforces HTTPS-only realm + same-host-or-single-subdomain
  host match when `o.user != ""`. Mirror as a helper in `pkg/oci`
  for the auth path; the anonymous path keeps the existing
  behaviour.
- Password never appears in any `slog` field, audit payload, error
  string, or HTTP response. Pinned by capture-based tests in
  `handlers_registry_auth_test.go` and `handler_auth_test.go`.
- Scrub: `scrubAuthFromError` strips any
  `Authorization: Basic <base64>` substring from registry-returned
  errors. Defence in depth — `FetchToken` never echoes the header,
  but registry-returned error bodies may.
- Ciphertext quota per app = `Limits.RegistryCredentialMax`, not a
  separate byte cap. The byte cap on the sealed payload itself is
  the existing `secretbox.SealBytes(..., maxBytes)` parameter
  (4096 bytes is more than enough for any Basic Auth password).
- Account-scoped ownership: the `account_id` column is the audit +
  cascade lever; cross-account access returns `state.ErrNotFound`
  (mirror of `AppSecret`).

## Rejected alternatives

- **Embedding the password in `pg_notify` payload.** pg_notify is
  plaintext in WAL/replication. Even when the WAL is encrypted at
  rest, decrypting it for replication requires re-sealing. A
  credential reference (id) that imaged then resolves via
  `Store.GetAppRegistryCredential` is the safer shape; the
  reference carries no secret.
- **Sealing username+password as one envelope.** Couples two
  fields unnecessarily; mirrors `AppSecret` precedent where each
  value is sealed independently. Username is metadata (logged,
  audited) and password is the secret.
- **Adding `*BasicAuth` to existing `oci.Puller` methods.** Breaks
  every test double (`cmd/e2e/fakevmm` etc.) and the production
  `RegistryClient` keeps working either way; additive is strictly
  safer.
- **Per-account credentials (shared across apps).** Violates the
  app ownership model; secrets are per-app. Customers with multiple
  apps on the same registry should set credentials per app (each
  row has its own last_used_at and rotation timeline).
- **DEK/KEK encryption at rest.** Over-engineered for a single
  box; the host age recipient IS the seal target. KEK rotation
  requires re-sealing every row; ADR-057 (host-age rotation)
  already handles recipient rotation via a one-shot migration.
- **Token exchange (`/token?service=...&scope=...` returns a bearer
  exchange token).** Works against registries that require it, but
  requires server-side caching of the exchanged token keyed by
  `(username, scope)`. Out of scope for v1: Basic Auth covers ~95%
  of self-hosted private registries; registries that mandate token
  exchange can be a follow-up.

## Consequences

- New table `app_registry_credentials` (migration `00083`).
- New state domain type `state.AppRegistryCredential`; six new
  `Store` methods (Upsert/Get/List/Delete/Count/MarkUsed).
- New DTOs (`pkg/api/dto.go`): `PutAppRegistryCredentialRequest`,
  `AppRegistryCredentialResponse` (no Password field by design),
  `AppRegistryCredentialListResponse`.
- New `Limits.RegistryCredentialMax` field on every plan.
- New scope `ScopeRegistryCredentialsWrite` +
  `ScopesRegistryCredentialsWriteSurface`.
- New problem constructors in `pkg/api/errors.go` (5).
- New additive `oci.AuthPuller` interface; `RegistryClient` and
  `DefaultPuller` implement it.
- imaged `buildImageLayer` and `aboveBaseLayers` thread auth
  per-pull (app manifest/blob = app auth; base manifest/blob = nil).
- apid handlers: `setRegistryCredential`, `listRegistryCredentials`,
  `deleteRegistryCredential`. Routes registered with the same
  middleware chain as the secrets handler
  (`authLimited → requireMFA → requireScope`).
- E2E `cmd/e2e/registry_auth_e2e_test.go` exercises the apid-only
  surface end-to-end against an `httptest` fake registry.
- No schema change to existing tables.
- No SDK surface change (registry creds are server-side only).
- No proto / OpenAPI change.

## Downstream

- Closes issue #461.
- No follow-up ADR proposed. ADR-061 remains reserved for the
  decimal-vs-binary GB-h consolidation that ADR-060 §Decision 8
  promises.