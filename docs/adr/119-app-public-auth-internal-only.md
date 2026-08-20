# ADR-119 · Per-app ingress "Internal Gregale services only" (`apps.public_auth_mode = 'internal_only'`)

- **Status:** Proposed
- **Date:** 2026-08-20
- **Decision:** Extend the closed `apps.public_auth_mode` enum
  (ADR-079, ADR-118) with a fifth value `internal_only`. When
  set, every public request to the app's hostname must carry an
  `Authorization: Bearer` JWT with `aud='gregale.internal'`
  signed by a Gregale daemon's Ed25519 key. The gateway holds
  a per-service public-key allowlist and 403s anything else.
  The gate runs in `pkg/gateway/handler.go::applyIngressInternalSvc`
  immediately after `applyIngressIPAllowlist` (ADR-118) and
  before `applyEdgeRuleIP`, so a token failure short-circuits
  before any Firecracker wake — same invariant as the kind=ip
  gate. **First caller is schedd**, firing a cron into a
  customer app whose `public_auth_mode='internal_only'`.

## Context

The four canonical ingress postures a customer expects from a
modern PaaS (table lifted from [[118-app-public-auth-ip-allowlist]]
§Context):

| Posture                              | Today                          |
|--------------------------------------|--------------------------------|
| Public (open)                        | ✅ `public_auth_mode=open` (ADR-079) |
| Organization members only            | ✅ IAM-6 cookie/org session (ADR-061) |
| Selected IP ranges                   | ✅ `public_auth_mode=ip_allowlist` (PR #999, ADR-118) |
| Internal Gregale services only       | ❌ **missing** — ADR-118 line 239-243 carved this out as "separate ADR" |

PR #999 closed the third bullet. This ADR closes the fourth.
Today there is **no** customer-facing surface that lets a
Gregale-internal daemon (schedd, meterd, imaged, builderd) call
into a customer app without going through the same public
ingress as everyone else — a customer who wants their app
reachable only by Gregale cron firings has no posture. ADR-118
reserved `internal_only` as the future enum value and called
out the prerequisites:

> *"a Gregale service-token signing key
> (`FAAS_INTERNAL_SVC_KEY`), a separate `aud='gregale.internal'`
> mint path, and a per-service allowlist."*

All three prerequisites are greenfield — this ADR designs them.

The closest precedents (which this ADR explicitly distinguishes
itself from):

- **Unix-socket auth v1.0** ([[015-unix-socket-auth-v1]]): DAC
  on `/run/faas/*.sock`, group `faas`, mode 0660. Auth model
  is filesystem perms only; no peer-cred check (`SO_PEERCRED`).
  This is the v1.0 control-plane trust model and continues to
  work for daemon-to-daemon gRPC. **Out of scope** for this ADR —
  `internal_only` is a *customer*-facing ingress gate, not a
  control-plane socket auth.
- **Control-plane mTLS** ([[052-control-plane-mtls-and-handler-peer-binding]]):
  reserved for the multi-host control plane (Gate-A). Out of
  scope here; the JWT-on-Authorization-header is the v1.0 trust
  model for internal-only ingress.
- **External JWT** ([[091-connection-aware-execution]] D21, the
  `kind='jwt'` edge rule): customer-issued JWT verified against
  the customer's JWKS endpoint. **Different trust model**: customer
  controls the key. For `internal_only`, Gregale controls the key
  (one keypair per daemon) and the verifier holds only the
  PUBLIC half. We cannot reuse `pkg/edgejwks/verifier.go`
  because that resolver expects an externally-hosted JWKS.

## Decision

### Closed enum widening (no new column)

```sql
ALTER TABLE apps DROP CONSTRAINT IF EXISTS apps_public_auth_mode_chk;
ALTER TABLE apps ADD CONSTRAINT apps_public_auth_mode_chk
  CHECK (public_auth_mode IN ('open','bearer','basic','ip_allowlist','internal_only'));
```

The token lives on the **request**, not on the **app row**.
The app row pins the policy; the policy references a class of
caller (Gregale daemons), not a list of specific callers.
Adding a per-app allowlist of specific daemon names would
defeat the simplicity of the mode (and the customer would
have to know every Gregale daemon's name).

### Asymmetric JWT, per-daemon keypair (Ed25519)

```text
header:   { "alg": "EdDSA", "kid": <sha256(pubkey)[:16]> }
payload:  { "iss": "gregale",
            "sub": <svcName>,             // e.g. "schedd"
            "aud": "gregale.internal",
            "exp": now+ttl,               // ≤30s in v1.0
            "iat": now,
            "nbf": now,
            "jti": <uuidv4>,
            "app_id": <optional uuid> }   // for cross-check at the gate
```

- **Why Ed25519 over RSA/ECDSA**: small keys (32 bytes private,
  32 bytes public), fast verify, deterministic signing, no
  padding-oracle surface. `golang.org/x/crypto/ed25519` is in
  stdlib-adjacent territory; `github.com/golang-jwt/jwt/v5`
  has first-class support.
- **Why per-daemon keypair, not one shared HMAC secret**: shared
  HMAC means every daemon can mint tokens for any other daemon
  (a compromised schedd can forge `sub="meterd"`); per-daemon
  keypairs mean a compromised schedd can only mint `sub="schedd"`
  tokens and the gateway's per-service allowlist will reject
  any other `sub`.
- **Why a per-service allowlist at the gateway**: the verifier
  holds **only public keys**. The gateway operator declares
  "I trust schedd, meterd, imaged, builderd" via
  `FAAS_INTERNAL_SVC_PUBKEYS` (JSON map `{"schedd":"<PEM>","meterd":"<PEM>",...}`).
  Adding/removing a trusted daemon = updating the env, no key
  rotation of existing daemons.

### Plan ladder: NONE

`internal_only` ships **for all plans** — Free/Hobby/Pro/Scale.

Justification: the mode is operator-side, not human-side. Gregale
daemons authenticate as services, not as humans. A Hobby customer
who wants a webhook-receiver app reachable only by Gregale cron
firing should not be blocked from doing so by a plan gate.
This is **different** from `ip_allowlist` (ADR-118) which is a
human-facing network policy and is appropriately gated to paid
plans.

### Gate order

Inserted at `pkg/gateway/handler.go:4435`, between
`applyIngressIPAllowlist` (line 4432, ADR-118) and
`applyEdgeRuleIP` (line 4438):

```text
applyAppsMaintenanceMode
applyEdgeRuleMaintenance
matchAndApplyRedirect
matchAndApplyRewrite
applyEdgeRuleHeaders
applyEdgeRuleCORS
applyEdgeRuleJWT
applyIngressIPAllowlist       ← ADR-118
applyIngressInternalSvc       ← THIS ADR (new)
applyEdgeRuleIP
applyEdgeRuleGeo
applyEdgeRuleLimit
applyEdgeRuleThrottle
applyEdgeRuleValidate
applyEdgeRuleBudget
enforceRequireAuthn
enforcePublicAuth
sidecar
wake
```

An IP-blocked request short-circuits first (ADR-118 invariant).
A token-blocked request short-circuits second (this ADR). An
edge-rule-blocked request short-circuits third. A `require_authn`
or `public_auth` failure short-circuits after all of those.
A `wake` only fires if every gate above is a pass-through.

### Trust chain: gatewayd-public MUST strip inbound Authorization

Today, `pkg/gateway/internal_proxy.go:348-354` strips the
inbound `X-Forwarded-For` chain (the customer can forge any
IP) and re-adds only the public daemon's `RemoteAddr`. This
ADR adds `outReq.Header.Del("Authorization")` next to it.

**Without this strip**: a customer sending `Authorization: Bearer foo`
on a TLS-terminated request reaches the internal gate. The
verifier will 403 them anyway (signature fails), but the audit
log shows their request reached an internal surface that they
should never have seen. Stripping is the correct posture.

**Only daemons that dial gatewayd-internal directly via
`/run/faas/gatewayd-internal.sock` reach the gate with an
Authorization header intact** — the unix-socket DAC model
(ADR-015) keeps the customer out of that path.

### pkg/internalsvc (NEW PACKAGE)

```
pkg/internalsvc/
  internalsvc.go          Mint + Verify + Audience
  internalsvc_test.go     6 unit tests (round-trip, wrong-aud,
                          expired, unknown-svc, tampered-sig,
                          missing-kid)
```

Public surface:

```go
package internalsvc

const Audience = "gregale.internal"

// Mint signs a JWT with aud=Audience, sub=svcName, ttl=ttl.
// Claims is merged into the payload (used for app_id context).
func Mint(svcName string, ttl time.Duration, claims map[string]any,
    priv ed25519.PrivateKey, kid string) (string, error)

// Verify checks signature, aud, exp, nbf (with 30s leeway),
// and that the issuer's svcName is in the allowlist.
// Returns the verified svcName on success.
func Verify(token string, allowedSvc map[string]ed25519.PublicKey) (svcName string, err error)
```

The allowlist is `map[string]ed25519.PublicKey` keyed by the
canonical service name (`"schedd"`, `"meterd"`, `"imaged"`,
`"builderd"`). The key in the JWT's `sub` claim is matched
against this map; a token whose `sub` is not in the map fails
verification. This is the per-service allowlist.

### Audit + metric

Audit codes (emitted via the existing `h.emitAuthnAudit` helper
in `pkg/gateway/handler.go`, mirroring the bearer/basic pattern
at L3686-3691):

- `instances.public_auth_internal_missing` — no Authorization
  header on an `internal_only` app
- `instances.public_auth_internal_invalid` — token present but
  verify failed (signature, aud, exp, nbf, or unknown svcName)
- (no `internal_matched` audit — pass-throughs don't need their
  own event, mirrors the geo path)

**Audit redaction invariant**: the JWT string itself MUST NEVER
appear in the audit payload. Only `reason` (the verify error
code, e.g. `"aud_mismatch"`), `app_id`, `from_host`. Pin via
`TestPublicAuthInternalOnly_AuditDoesNotEchoToken` that scans
raw audit JSON for a known JWT substring.

Metric (new counter family, NOT a re-use of `edgeRuleMatch` —
this is a true auth event, not an edge-rule decision):

```text
gateway_internal_auth_match_total{outcome="matched|blocked"}     counter
```

Pre-instantiated in `pkg/gateway/metrics.go` near the existing
`edgeRuleMatch` pre-instantiation (L964-968). Closed outcome set:
`{matched, blocked}` — extend the slice when adding new outcomes
(documented at the call site, mirrors the `edgeRuleMatch`
pre-instantiation comment).

### Key loading

- **Mint side** (e.g. schedd): `cmd/schedd/main.go` loads the
  Ed25519 private key from `FAAS_INTERNAL_SVC_KEY_PATH`
  (default `/etc/faas/secrets/internal-svc/schedd.ed25519`) at
  boot. If `FAAS_INTERNAL_SVC_KEY_SEALED=1`, the file is
  sealed under host.age in the `GREGALE_INTERNAL_SVC` namespace
  (mirrors `APP_BASIC_AUTH` at `pkg/secretbox/seal.go:52-78`).
- **Verify side** (`gatewayd-internal`): `cmd/gatewayd-internal/run.go`
  loads `FAAS_INTERNAL_SVC_PUBKEYS` (JSON map) at boot,
  passes into a new bridge file
  `cmd/gatewayd-internal/internal_svc_verifier.go` (mirrors
  `cmd/gatewayd-internal/public_auth_unsealer.go`).

### First caller: schedd → gatewayd-internal via cron

When schedd fires a cron to a customer app whose
`apps.public_auth_mode='internal_only'`:

1. Look up the app's mode (existing `schedd.AppForCron` helper).
2. If `internal_only`, mint a JWT via
   `internalsvc.Mint("schedd", 30*time.Second, claims, scheddPrivKey, scheddKID)`.
3. Attach `Authorization: Bearer <token>` to the outbound HTTP
   request to gatewayd-internal's HTTP reverse-proxy path.

The HTTP client uses a unix-socket-only dialer
(`/run/faas/gatewayd-internal.sock`), already wired at
`cmd/schedd/cron.go`. This PR adds the conditional header.

The 30s TTL bounds replay risk without a `jti` denylist: a
captured token is unusable after 30 seconds. Future work may
add a denylist (see "Future work" below).

## Schema materialization

`schema.sql` inline CHECK on `public_auth_mode` is widened to
the new closed set. Mirrors the migration verbatim.

## Verification (test coverage)

| Surface                       | File                                                  | Tests                                    |
|-------------------------------|-------------------------------------------------------|------------------------------------------|
| JWT round-trip + edge cases   | `pkg/internalsvc/internalsvc_test.go`                 | 6                                        |
| Gateway gate                  | `pkg/gateway/public_auth_internal_only_test.go`       | 7 (valid, missing, invalid-sig, wrong-aud, expired, other-mode, gatewayd-public-strips-auth) |
| Audit redaction               | same                                                  | + 1 (token substring scan)               |
| Schedd mint + cron header     | `cmd/schedd/cron_internal_only_test.go`               | 3                                        |
| Migration static + apply      | `migrations/00333_apps_public_auth_internal_only_test.go` | 5 (ApplyThrough, RoundTrip, Accepts, Rejects, DownGrade) |
| Drift guard                   | `pkg/api/public_auth_constants_test.go` + `pkg/gateway/handler_public_auth_constants_test.go` | 1 each |
| End-to-end                    | `cmd/e2e/public_auth_internal_only_e2e_test.go`       | 1 (full stack, three requests)            |

Run in this order (mirrors the PR #999 verification protocol
captured in `[[pr-999-public-auth-ip-allowlist-shipped-2026-08-20]]`):

1. `make test`
2. `make lint`
3. `make metal-lima` (skipped — PR does not touch `pkg/fcvm`,
   `pkg/netns`, `vmmd`, `builderd`)
4. `make leakcheck` (skipped, same reason)
5. Migration smoke: `go test ./migrations -tags no_pg -run
   TestMigrations_00333`
6. Slot precheck (cross-PR fence collision):
   `PR_NUMBER=<n> BASE_REF=main GITHUB_REPOSITORY=poyrazK/faas
   ./scripts/ci/check_migration_slots.sh`
7. Drift guard (3-place constant alignment):
   `go test ./pkg/api ./pkg/gateway -run 'TestPublicAuth.*ConstantsAgree'`

## Future work (deliberately out of scope for this ADR)

- **Key rotation**: first PR ships the static env-load path.
  Rotation (publish new pubkey, remove old) needs a new pg_notify
  channel `internal_svc_keys`, mirroring the `NotifyAppChanged`
  pattern. ADR-120 candidate.
- **Replay protection**: short TTL (≤30s) is sufficient for v1.0.
  `jti` is generated but not denylisted. Future ADR may add a
  `revoked_jti` Postgres table for explicit revocation.
- **Multi-host**: when the multi-host control plane lands
  (ADR-052), the allowlist becomes per-host (one pubkey per
  daemon per host). The JWT is per-daemon, not per-host.
- **`members_only` mode** (the bullet-2 unification): still
  covered by the IAM-6 cookie layer; out of scope here.
- **Per-deployment `internal_only` override**: keep both. Customers
  may want per-(host,path) ingress policy (via edge rules) in
  addition to per-app. Same posture as ADR-118.
- **mTLS at the unix-socket layer**: explicitly deferred per
  ADR-052. JWT-on-Authorization is v1.0.

## Deployment requirements (operator-side)

The `internal_only` mode requires explicit key wiring on the
gatewayd-internal side. Without it, the gate 500s every request
(operator_error) — the loud posture is deliberate, but the
deploy step is mandatory.

**Required env vars**:

- `FAAS_INTERNAL_SVC_PUBKEYS` (gatewayd-internal): JSON map
  `{"<svcName>": "<pem-encoded Ed25519 public key>"}`. e.g.
  ```json
  {"schedd": "-----BEGIN PUBLIC KEY-----\nMCowBQYDK2VwAyEA...\n-----END PUBLIC KEY-----"}
  ```
  The verifier is constructed at boot from this map. If the
  env is unset, the verifier is nil and the gate returns 500
  with `instances.public_auth_internal_invalid` audit
  (reason="empty_allowlist" once the wire surfaces that).
- `FAAS_INTERNAL_SVC_KEY_PATH` (schedd, only): PKCS#8 PEM
  file path to the schedd's Ed25519 private key. Default
  `/etc/faas/secrets/internal-svc/schedd.ed25519`. If the
  file is missing, schedd auto-generates a fresh keypair
  with a loud WARN ("schedd: internal-svc keypair generated
  at runtime — operator MUST provision + publish the public
  key into FAAS_INTERNAL_SVC_PUBKEYS"). The auto-generated
  path is dev-only; production deploys MUST provision
  deterministic keys.

**First-boot deployment procedure**:

1. Generate the schedd keypair:
   ```bash
   openssl genpkey -algorithm Ed25519 -out /etc/faas/secrets/internal-svc/schedd.ed25519
   ```
2. Extract the matching public key:
   ```bash
   openssl pkey -in /etc/faas/secrets/internal-svc/schedd.ed25519 -pubout \
     -out /etc/faas/secrets/internal-svc/schedd.pub
   ```
3. Persist the public key on every gatewayd-internal node
   (via Ansible or systemd `EnvironmentFile=`):
   ```
   FAAS_INTERNAL_SVC_PUBKEYS={"schedd":"$(cat /etc/faas/secrets/internal-svc/schedd.pub)"}
   ```
4. Restart both daemons. The verifier is constructed at
   boot; rotation is a follow-up (see Future work above).

**Future hardening (ADR-120 candidate)**: replace the static
env-load path with a pg_notify-driven pubkey registry so a
schedd key-rotation only requires adding the new pubkey to
the registry, not editing every gatewayd-internal's
EnvironmentFile.

## Round-2 review follow-ups (2026-08-21)

Closed by an immediate patch on the same PR. Each finding
+ resolution:

### F1 — `/v1/invocations:dispatch` bypass

Peer review surfaced that schedd's cron path calls
`l.gateway.Invoke` (`pkg/sched/loop.go:2335`) BEFORE the
fallback `SynthesizeRequest`, and `Invoke` posts to
`/v1/invocations:dispatch` which has no gate. A forged
schedd could invoke an internal_only app via this surface.

**Fix**: extended `SynthServer.applyIngressInternalSvc` to
also gate `handleInvocationDispatch` (mirrors the
`handleSynthesize` coverage). The single-share `SynthServer`
already has the per-app `appPublicAuthMode` + the
`InternalSvcVerifier`; only the call site is new.

### F2 — `/v1/invocations:dispatch_batch` bypass

Symmetric to F1: the trigger batch dispatch tick
(`pkg/sched/dispatch_triggers.go:784`) posts to
`/v1/invocations:dispatch_batch` with no JWT, and the
handler calls `dispatcher.Invoke` per record with no gate.

**Fix**: extended `applyIngressInternalSvc` to also gate
`handleInvocationDispatchBatch`. The batch envelope carries
one appID, so the gate is per-batch (not per-record) — much
cheaper than per-record Verify.

### F3 — `/v1/invocations:dispatch` + dispatch_batch outbound
from schedd

The first PR attached the JWT only to `SynthesizeRequest`.
`Invoke` (cron + drain) and `postBatch` (trigger batch)
also need it.

**Fix**: extracted the per-app auth attachment into the same
inline pattern across all three call sites. The Loop
(`pkg/sched/loop.go`) gained two new fields
(`appPublicAuthModeLookup`, `mintInternalSvcToken`) and the
matching `WithAppPublicAuthModeLookup` + `WithMintInternalSvcToken`
setters. `cmd/schedd/main.go` wires them once.

### F4 — fail-open posture on lookup error

`PublicAuthModeFromStore` returned `""` on any DB error, and
both the schedd-side attachment AND the gateway-side gate
treat `""` as "open". A transient Postgres outage during a
cron tick to an internal_only app would omit the JWT, and
the gateway would also return `open` on the same error —
invoke succeeds without auth.

**Fix**: changed the lookup signature to
`func(ctx context.Context, appID string) (PublicAuthModeLookupResult, error)`.
On error, callers now treat the app as `internal_only` and
attach the JWT (fail-closed). The gateway-side gate still
checks the app's actual mode (via its per-app cache), so a
genuinely-open app doesn't get a JWT for nothing — the
JWT is harmless in that case.

The exported sentinel `ErrAuthModeLookup` lets a future
hardening pass abort the request entirely on lookup failure
without re-plumbing the closure.

### F5 — caller-ctx ignored

`PublicAuthModeFromStore` used `context.Background()` for
the store round-trip, ignoring the caller's cancellation
(shutdown signal, tick deadline, etc.).

**Fix**: changed the closure signature to take
`ctx context.Context` and threaded it through
`PublicAuthModeLookupFunc`. All three call sites (SynthesizeRequest,
Invoke, postBatch) now pass their own ctx.

### "from" tag split

The audit row's `from` field was hardcoded to `"synth"`.
With three gated surfaces (and the HTTP-front-door side
using `"http"`), the value is now part of the dashboard
split. The new tags:

- `"synth"` — `handleSynthesize` (legacy wake-only)
- `"synth_dispatch"` — `handleInvocationDispatch` (move-1 single)
- `"synth_batch"` — `handleInvocationDispatchBatch` (trigger batch)
- `"http"` — `Handler.applyIngressInternalSvc` (HTTP-front-door)

## References

- [[118-app-public-auth-ip-allowlist]] — the sibling ADR for the
  third bullet (`ip_allowlist`); same drift-guard surface, same
  gate-chain placement, same audit redaction invariant.
- [[079-per-app-public-auth]] — origin of the closed
  `public_auth_mode` enum; reserved `internal_only` as a future
  extension slot.
- [[015-unix-socket-auth-v1]] — control-plane socket auth;
  **distinguished** from `internal_only` in §Context.
- [[052-control-plane-mtls-and-handler-peer-binding]] — multi-host
  mTLS; **distinguished** in §Context.
- [[091-connection-aware-execution]] D21 — `kind='jwt'` edge
  rule (customer-issued); **distinguished** in §Context.
- [[031-app-egress-allowlist]] / [[033-app-egress-allowlist-v6]]
  — schema+trigger precedent (referenced from ADR-118).
- [[089-metrics-convention]] — Prometheus naming.
- `[[pr-999-public-auth-ip-allowlist-shipped-2026-08-20]]` —
  verification protocol + drift-guard test precedent.

## Cited precedents (file paths)

- ADR-079 enum rationale + future reserved values:
  `docs/adr/079-per-app-public-auth.md` L52-69, L177-180.
- PR #999 gate chain hook:
  `pkg/gateway/handler.go:4432` (`applyIngressIPAllowlist`).
- PR #999 drift guard:
  `pkg/api/public_auth_constants_test.go:42-87`,
  `pkg/gateway/handler_public_auth_constants_test.go:34-76`.
- PR #999 metric pre-instantiation:
  `pkg/gateway/metrics.go:964-968`, `1043-1055`.
- Bridge file precedent (sealed-at-rest):
  `cmd/gatewayd-internal/public_auth_unsealer.go`,
  `cmd/gatewayd-internal/run.go:1138-1147`.
- host.age sealed secret pattern:
  `pkg/secretbox/seal.go:52-78`,
  `cmd/githubd/main.go:475-499` (`FAAS_HOST_AGE_IDENTITY_PATH`).
