# ADR-046 — OAuth sign-in boot validation (issue #419)

## Status

Accepted — 2026-07-29. Closes #419.

## Context

Issue #419 is tier-1 ship-blocking: `GET /v1/auth/google` and
`GET /v1/auth/github` return `500 *_oauth_misconfigured` on the EX44
production box because `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET` /
`GITHUB_CLIENT_ID` / `GITHUB_CLIENT_SECRET` are unset in
`/etc/faas/sealed.env` and apid never validates them at startup.
The handlers read `os.Getenv(...)` per request (cmd/apid/handlers_google.go
+ handlers_github.go), so a fresh box with the env vars missing would
boot clean (`/healthz` returns `{"status":"ok"}`) and only surface
the misconfiguration as a 500 when a customer clicked the OAuth
button.

The systemd unit (`deploy/systemd/faas-apid.service`), the ansible
copy, and `deploy/digitalocean/sealed.env.example` never list the
variables, so an operator who hasn't set them doesn't notice.

Worse: **passwordless accounts (those who only ever signed up via
OAuth) cannot sign in** today. The §11 anti-enumeration contract
(`pkg/auth.DummyPHC` + `verifyPasswordOrPad` in
`cmd/apid/handlers_auth_login.go`) means `/login` returns 401
`invalid_credentials` for them by design. With both consent routes
dead, those accounts have no entry point at all.

## Decision

Hybrid boot validation: the daemon refuses to start if either
provider's `(ID, SECRET)` pair is *half-set*; both-unset is
permitted and disables the provider. Specifically:

1. **Both ID and SECRET present per provider** →
   `SignInProviderConfigured`. The handler builds a consent-redirect
   URL the same way it does today.
2. **Both unset per provider** → `SignInProviderDisabled`. The
   handler returns 503 `oauth_provider_unavailable` and the
   dashboard's login template hides the button.
3. **Exactly one of ID / SECRET set per provider** →
   `pkg/auth.LoadSignInConfigFromEnv` returns a wrapped error and
   `cmd/apid/main.go::runWithDeps` fails to start the daemon. The
   error message names the diverging variables so the operator can
   act.

The same closed set powers the new authenticated `GET
/v1/auth/capabilities` endpoint, which the dashboard reads on
`/login` to decide whether to render the buttons. The shape is
`{"providers":{"google":{"enabled":true},"github":{"enabled":false}}}`
(pinned as `pkg/api.AuthCapabilities`).

The disabled-provider branch increments
`apid_oauth_disabled_total{provider=google|github}` (a
pre-instantiated Prometheus CounterVec, single-registry pattern at
`pkg/wire/metrics.go`). This is the tripwire signal for "operators
running with OAuth off" — a sustained increment means the dashboard
is hitting 503s, which is fine if intentional (single-box dev) but
a regression on a production box that should have OAuth enabled.

## Consequences

### Positive

- Production boxes that should have OAuth configured can no longer
  boot without it — the misconfiguration surfaces at apid startup
  rather than at the first customer click.
- The dashboard does not render dead buttons that lead to 500.
  Customers see a single unmissable "OAuth Provider Unavailable"
  on click.
- The pre-instantiated `apid_oauth_disabled_total` metric gives
  operators a fast signal that a box is running with OAuth off
  (intentional on dev, a regression on prod).

### Negative

- Half-set configs that previously "worked" (consent redirect
  succeeded against an unfilled client_id, callback then 500'd on
  the missing secret) now refuse to boot. This is the desired
  outcome, but it does mean an operator who set only one variable
  must fix their config before the daemon will start.
- The dashboard's `/login` template now branches on `{{if
  .Auth.GoogleEnabled}}` instead of unconditionally rendering both
  buttons — a future template that fails to populate
  `dashboard.Page.Auth` will render neither button. The wrapper
  `{{if .Auth}}…{{end}}` guards against the nil-pointer panic in
  pkg/dashboard (dashboard_test.go's
  `TestRender_LoginBody` regression caught this).
- `pkg/auth.SignInConfig.Enabled()` now also returns false when
  `ClientID` or `ClientSecret` is empty even with `Status ==
  Configured`. This is defence in depth — boot validation refuses
  half-set configs, but a future direct constructor call or test
  helper must not bypass the guard.

### Rejected alternatives

- **Pure fail-fast (any unset env = refuse to boot).** Rejected
  because single-box dev environments reasonably run apid without
  OAuth — they authenticate via the password form and don't need
  the OAuth flow. Forcing OAuth on every install would block the
  dev loop.
- **Pure warn-and-continue (any env unset = log warn, run
  anyway).** Rejected because that's the status quo and it's the
  bug. Half-set configs that produced silent-fallback 500s would
  still ship.

## References

- Issue #419 — the production OAuth 500 / passwordless-account
  lockout.
- ADR-032 — codifies the sign-in OAuth flow
  (`/v1/auth/{google,github}` + `/v1/auth/{google,github}/callback`).
- ADR-032-paddle-billing-provider (PR #137) — fail-fast env
  loading precedent: `FAAS_BILLING_PROVIDER=paddle` without a
  matching catalog file refuses to boot. The pattern is the same
  shape: validate at startup, name the diverging variables in the
  error.
- pkg/auth/oauth.go — `SignInConfig`, `LoadSignInConfigFromEnv`,
  `SignInProviderStatus`.
- pkg/auth/oauth_test.go — 7 cases covering both-set, both-unset,
  half-set (each half), and mixed-configured-half-set.
- pkg/api/dto.go — `AuthCapabilities`, `AuthProviders`,
  `OAuthProviderCapability`.
- pkg/wire/metrics.go — `apid_oauth_disabled_total{provider}`,
  `ObserveOAuthDisabled`.
- cmd/apid/server.go — `WithOAuthConfig` setter pattern (matches
  `WithBillingProvider`, `WithOpsMetrics`).
- cmd/apid/handlers_oauth_capabilities.go — `renderAuthCapabilities`.
- cmd/apid/handlers_google.go + handlers_github.go — the disabled-
  provider 503 branch + the `s.oauthConfig.{Google,GitHub}` reads
  that replace the legacy `os.Getenv(...)` calls.
- pkg/dashboard/templates/login.html — `{{if .Auth.GoogleEnabled}}…{{end}}`
  + `{{if .Auth.GitHubEnabled}}…{{end}}` guards.
- deploy/digitalocean/README.md#oauth-sign-in-optional — operator
  doc for the env vars + the three-mode outcome table.
- deploy/digitalocean/sealed.env.example — commented OAuth lines.
- deploy/systemd/faas-apid.service + ansible copy — note in the
  `EnvironmentFile=` comment block.

## Future work (out of scope here)

- **GitHub App OAuth callback at `/oauth/code-callback`** reads
  `FAAS_GITHUB_APP_CLIENT_ID`, which is a different env / lifecycle
  (the install-bind flow, not the dashboard login flow). The same
  half-set fail-fast pattern applies, but it is a separate concern
  with a separate `pkg/auth` package and is left for a future PR.
- **Per-route rate-limit budget sharing with disabled-provider
  503s.** Intentional: the dashboard's AuthLimit counts the
  disabled-provider 503 against the §11 10/min/IP bucket. This means
  a misconfigured box (buttons disabled, but customer typing the URL
  directly) burns the same bucket as a brute-force attempt. A
  future change could split the disabled-provider path into its own
  no-counter bucket, but the current shape is the conservative one.
- **Dashboard-side redirect on 503.** Currently the
  `/v1/auth/capabilities` shape gates the buttons, but a customer
  who knows the URL could still hit `/v1/auth/google` directly. The
  handler's 503 + log + metric is the production response. A future
  PR could add a customer-facing JS-side redirect to `/login?oauth=unavailable`
  on 503, but the dashboard surface is server-rendered and that
  redirect belongs on a future HTMX layer.