# ADR-046 — `pkg/auth` extraction (Move 4 / issue #254)

## Status

Accepted, 2026-07-29. PR-1 of the Move 4 follow-up. The companion
implementation lands in this PR; PR-2 (gatewayd AppLogsHandler) follows
once PR-1 merges.

- **Superseded (in part, PR-E):** prose referred to the monolithic
  `cmd/gatewayd/` daemon split by ADR-070 into `gatewayd-public` (TLS-only
  edge) and `gatewayd-internal` (routing + wake + proxy). Body is preserved
  verbatim; readers should substitute "gatewayd-internal" for the
  routing/wake/proxy path and "gatewayd-public" for the certmagic/TLS path.
  `cmd/gatewayd/<file>.go` citations in this body are stale; see PR-E for
  the new file locations.

## Context

PR #412 (issue #254 / Move 4 partial) shipped the transport-neutral
client adapter (`pkg/scheddgrpc.Client.StreamAppLogs`) and the
receive-pump fix in `cmd/apid`. It deliberately left the production
wiring undone because `.golangci.yml`'s `apid-control-plane-only`
depguard rule forbids `cmd/apid` from importing `pkg/scheddgrpc`
(CLAUDE.md §Component ownership). The follow-up architectural decision
recorded in `move-4-architectural-decision-gateway-streaming` was:
**stream via gatewayd**, which already imports `pkg/scheddgrpc` per
ADR-018 and which already dials schedd at `/run/faas/schedd.sock`.

The naive plan ("gatewayd owns the route and applies auth inline")
collides with CLAUDE.md's component-ownership rule: every `/v1/*`
route in `cmd/apid` is wrapped in a five-piece auth chain

```
auth → authLimited → requireMFA → requireScope → loadApp → handler
```

and replicating that chain in `cmd/gatewayd` would (a) duplicate five
load-bearing functions and (b) create a parallel evolution path that
future auth changes would have to apply in two files. The memory
`gatewayd-isapidpath-pr180-gap` shows the same class of bug from
PR #180: a forgotten parallel entry on the gatewayd side became a
silent routing bug.

## Decision

Lift the auth chain into a new library package so both daemons
depend on the same surface.

### Package layout

- `pkg/auth/` — pre-existing primitives: Argon2id encoder/verifier,
  HMAC, password helpers, TOTP. Untouched. Import name: `auth`.
- `pkg/auth/middleware/` — **new in this PR.** Lifts
  `cmd/apid/server.go::auth` (requireSession), `cmd/apid/mfa_middleware.go::requireMFA`,
  `cmd/apid/server.go::requireScope`, `cmd/apid/server.go::authLimited`,
  and `cmd/apid/server.go::loadApp` into `Middleware`. Import name:
  `middleware` (the package is rooted at `pkg/auth/middleware/`; the
  short name keeps facade call sites readable).
- `pkg/authcode/` — **new in this PR.** Holds the recovery-code
  primitives that used to live in `pkg/auth/totp.go` (`NewRecoveryCodes`,
  `HashRecoveryCode`, `RecoveryCodeCount`). Lives outside `pkg/auth`
  on purpose: `pkg/auth/middleware` now imports `pkg/state` (it
  threads `state.Account` through the typed `AccountHandler`), and
  `pkg/state`'s `*_test.go` files need `authcode` for the
  `MFARecoveryCodesHash` column tests. Pulling `authcode` into
  `pkg/state` itself would close the same cycle the other way.
- `cmd/apid/auth_facade.go` — **new.** Thin facade: `s.auth`,
  `s.authLimited`, `s.requireMFA`, `s.requireScope`, `s.loadApp`,
  `s.clearSessionCookie`, `sessionFrom`. Every existing route
  registration at `cmd/apid/server.go:561-815` continues to call
  these method names; the body inside each is a one-line delegate
  to `s.authMw`.
- `cmd/apid/auth_adapters.go` — **new.** The bridge between
  `cmd/apid`'s concrete types (`state.Store`, `*auditor`) and the
  three `pkg/auth/middleware` interfaces (`Authenticator`,
  `SessionLookup`, `Auditor`). All three are free pass-throughs —
  the concrete types already have the methods with matching
  signatures.

### ctx-key ownership

`pkg/auth/middleware` owns the principal, session-row, and
mfa-pending ctx keys. `cmd/apid`'s pre-existing `principalFrom` /
`withPrincipal` (cmd/apid/server.go) become thin bridges:
`withPrincipal` calls `middleware.WithPrincipal`, and `principalFrom`
reads via `middleware.AccountFromContext`. This keeps
`cmd/apid/observeWrap` working without a parallel-ctx-key refactor
and lets `middleware.RequireScope` see the principal that
`cmd/apid`'s cookie branch stamps.

### What stays in cmd/apid (NOT lifted to pkg/auth)

- `s.adminAllows` — per-daemon operator-email allowlist
  (compute_nodes.go). PR-2 doesn't need it; keep the seam
  per-daemon.
- `mfaAllowlist` / `isMFAAllowlisted` — referenced by
  `pkg/auth/middleware.RequireMFA`'s path-prefix check. The
  cmd/apid copy stays so the operator surface remains
  observable in whitebox tests.

### What lands in the follow-up slice (NOT this PR)

- `s.auth`'s `requireSessionCookie` + `touchDebounce` +
  `sessionTouch` machinery. These touch `session.AEAD` +
  `state.Store` cross-check; lifting them requires `pkg/auth/middleware`
  to grow a `Sessions` interface method surface
  (`GetSession`, `TouchSessionLastSeen`) that doesn't exist today
  and isn't needed by PR-2's AppLogsHandler. Lands in the slice
  that follows PR-2.

## Why a typed `AccountHandler` instead of `http.Handler`

The chain threads the resolved `state.Account` through three
layers (`RequireSession → RequireMFA → RequireScope → handler`).
An `http.Handler` boundary forces the Account lookup to be
re-derived from the context inside every handler; the existing
cmd/apid wiring avoids this by passing the value directly.
`pkg/auth/middleware.AccountHandler` exposes the same shape so the
facade in `cmd/apid/auth_facade.go` is a one-line pass-through.

## Why pointer mutation (`*r = *r.WithContext(...)`)

Same rationale as PR #332 (issue #278): `cmd/apid`'s outermost
middleware is `observeWrap`, which reads the principal via
`principalFrom(r)` to feed `apid_ops_total{account_id=...}`.
A non-mutating rebind would be invisible to `observeWrap` and
every authenticated call would land in the "anonymous" bucket.
The pointer-mutation contract is documented in
`pkg/auth/middleware/middleware.go` so a future refactor doesn't
accidentally revert it.

## Depguard

No change. `pkg/auth/middleware` and `pkg/authcode` are new
packages; nothing in the existing `apid-control-plane-only` or
`firecracker-jailer-vmmd-owned` rules rejects them. `cmd/gatewayd`
gains allowed imports for both, which is the load-bearing
prerequisite for PR-2.

## Verification

- `go test ./...` — full unit suite green; `pkg/auth` +
  `pkg/auth/middleware` + `pkg/authcode` + `cmd/apid` all
  pass without modifying existing assertions.
- `gofmt -l pkg/auth pkg/authcode pkg/auth/middleware cmd/apid`
  — empty.
- `go vet ./...` — empty.
- `cmd/apid`'s 60+ route registrations in
  `cmd/apid/server.go:561-815` are unchanged at the call-site
  level (only the implementation behind `s.requireMFA` and
  `s.requireScope` changed); every route's existing tests in
  `cmd/apid/handlers_*_test.go` pass.

## How to apply

When a new component (apid, gatewayd, schedd, vmmd, imaged,
builderd, meterd) needs to authenticate a request, depend on
`pkg/auth/middleware.Middleware` — never re-implement the bearer /
session / MFA chain inline. New scope vocabulary entries go in
`pkg/api` (existing `ScopesReadSurface` etc.) and are wired into
handlers via `middleware.RequireScope(api.ScopesReadSurface...)`.

## Related

- PR #412 — Move 4 partial (client adapter + receive-pump fix).
- `move-4-architectural-decision-gateway-streaming` — why the
  app-logs route moves to gatewayd.
- ADR-043 — app-logs stream architecture (records the
  cmd/gatewayd-inline decision this PR makes load-bearing).
- ADR-039 — IAM-3 session revocation (the live-row cross-check
  `pkg/auth/middleware.RequireSession` consumes).
- ADR-034 rev2 — IAM-1 scope vocabulary (`pkg/auth/middleware.RequireScope`
  is the load-bearing enforcer).
- Memory `gatewayd-isapidpath-pr180-gap` — the bug class that
  motivates extraction over duplication.