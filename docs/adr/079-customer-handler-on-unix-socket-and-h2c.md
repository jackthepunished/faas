# ADR-079 · Customer publicHandler on the gatewayd-internal unix socket + H2C on the public→internal hop

- **Status:** accepted
- **Superseded (in part, PR-E):** prose referred to the monolithic
  `cmd/gatewayd/` daemon split by ADR-070 into `gatewayd-public` (TLS-only
  edge) and `gatewayd-internal` (routing + wake + proxy). Body is preserved
  verbatim; readers should substitute "gatewayd-internal" for the
  routing/wake/proxy path and "gatewayd-public" for the certmagic/TLS path.
  `cmd/gatewayd/<file>.go` citations in this body are stale; see PR-E for
  the new file locations.
- **Date:** 2026-08-06
- **Decision:** Two coordinated changes to the Tier A7 edge
  (ADR-070):
  1. The unix-socket `/run/faas/gatewayd-internal.sock` that
     `gatewayd-internal` already serves shall mount the **customer
     `publicHandler`** alongside the existing `SynthServer` routes
     (`/v1/synthesize`, `/v1/invocations:dispatch`, `/healthz`).
     The two surfaces share one `http.ServeMux`; HTTP routing is by
     longest-prefix match.
  2. The public→internal hop negotiates **HTTP/2 cleartext (H2C)**
     using the Go 1.24+ stdlib `http.Server.Protocols` field on the
     server side (`SetUnencryptedHTTP2(true)` + `SetHTTP1(true)`)
     and `golang.org/x/net/http2.Transport{AllowHTTP:true,
     DialTLSContext:...}` on the client side. The Tier A7 production
     path now carries H2 frames end-to-end from Caddy → gatewayd-public
     → gatewayd-internal.

- **Why:** Two architectural facts collided:
  1. **The unix socket only served synth traffic.** Pre-#675
     `gatewayd-public` could dial `/run/faas/gatewayd-internal.sock`
     but only `SynthServer` routes were mounted. Customer traffic
     404'd on the unix socket. The HTTP listener that did serve
     customer traffic (`127.0.0.1:8080`) was disabled in production
     via `FAAS_GATEWAY_LISTEN=off`, so customer requests hit the
     socket, found no catch-all, and 404'd.
  2. **HTTP/2 was requested but couldn't reach the internal hop.**
     Caddy upstream terminates H2 over TLS into `gatewayd-public`,
     but the unix-socket hop was plaintext HTTP/1.1 — `http.Transport.
     ForceAttemptHTTP2` is a no-op over plaintext, and the spec's
     gRPC clients need H2 framing end-to-end.

  Resolving (2) without (1) is meaningless: an H2 hop that 404s
  on customer traffic is a worse production path than the existing
  H1 hop. The two changes land together so the H2C contract applies
  to a path that actually serves traffic.

- **Architecture after the change:**

```
customer → Caddy (TLS + H2 over :443)
       → 127.0.0.1:8080 (gatewayd-public, plaintext loopback)
       → /run/faas/gatewayd-internal.sock (HTTP/2 cleartext, NEW)
       → mux{ SynthServer{synthesize, dispatch, healthz},
              publicHandler{customer traffic} }
       → forwardproxy → guest :8080 (still HTTP/1.1 plaintext)
```

- **Consequences:**

  - `pkg/gateway/synth.go` — `SynthServer` exposes `SetHandler(h)`
    and `Mux() *http.ServeMux` so the unified mux in
    `cmd/gatewayd-internal/run.go` can mount the customer
    `publicHandler` as the catch-all and the three synth routes as
    sub-handlers. The handler swap is a write to `s.srv.Handler`; the
    `Protocols` field set in `NewSynthServer` is preserved.
  - `cmd/gatewayd-internal/run.go` — `runWithDeps` builds the unified
    mux when `deps.synth != nil`. The mux registers `Handle("/",
    publicHandler)` first, then the more-specific `/v1/synthesize`,
    `/v1/invocations:dispatch`, `/healthz` mounts — Go's
    `http.ServeMux` longest-prefix match means the catch-all does
    NOT shadow the more-specific routes. Registration order is
    load-bearing only for the reader; comment in `run.go` makes
    this explicit.
  - `pkg/gateway/internal_proxy.go` — `NewInternalReverseProxy`
    takes a `useH2C bool`. When true, the proxy uses
    `newInternalProxyH2CTransport` (`http2.Transport{AllowHTTP:true,
    DialTLSContext:...}`); when false, the existing
    `newInternalProxyTransport` (`http.Transport{ForceAttemptHTTP2:
    false}`). `DialTLSContext` propagates the request ctx into the
    dialer so cancellation works (golangci-lint v2.4 `contextcheck`
    would flag a plain `DialTLS`).
  - `cmd/gatewayd-public/main.go` — `FAAS_INTERNAL_H2C` env knob
    (default `true`) controls the transport on the public→internal
    hop. `false` reverts to the legacy HTTP/1.1 transport via daemon
    restart; no redeploy needed.
  - `pkg/gateway/synth.go` — `NewSynthServer` enables H2C + HTTP/1.1
    on the listener via `srv.Protocols.SetUnencryptedHTTP2(true)` +
    `SetHTTP1(true)`. This is the Go 1.24+ replacement for the
    deprecated `golang.org/x/net/http2/h2c.NewHandler` wrapper.
  - `pkg/gateway/gate.go` — `WakeGate` doc comment confirms H2
    multiplexing preserves single-flight coalescing: the gate keys
    on `appID` alone (not on connection), so N concurrent H2
    streams on one unix-socket connection still coalesce to one
    wake. Verified by `TestConcurrentColdRequestsCoalesceToOneWake`
    in `handler_test.go:412` (pre-existing) and
    `TestInternalProxy_H2CMultiplexesConcurrentStreams` in
    `internal_proxy_h2_test.go` (new — confirms the H2C transport
    fans out, not coalesces, at the proxy layer).
  - `pkg/daemonunitspec/gatewayd_internal.go` — wipe comment block
    updated to reflect the unified-mux shape; references this ADR.

- **Out of scope (filed as separate NETWORK issues):**

  - **Issue #686** — H2C on the gatewayd-internal → vmmd → guest
    `:8080` leg requires rewriting the streaming bridge in Go (it
    hard-codes `HTTP/1.1` in shell at `pkg/vmmdgrpc/forward.go:464`).
    Keeps ADR-009 (strict-netns) but adds a per-instance unix-socket
    or a Go rewrite of the streaming RPC.
  - **Issue #687** — `pkg/gateway/internal_proxy.go` latent bugs
    activated by H2C multiplexing: no `http.Flusher.Flush` on body
    copy (line ~275), body-copy goroutine lifetime after ctx cancel
    (line ~311-320), `DialTimeout` wrapping the full `RoundTrip`
    context (line ~240-244). The third is now a regression risk
    for cold-wake streaming under H2C.

- **Verified:**

  - `go test -race ./pkg/gateway/... ./cmd/gatewayd-public/...
    ./cmd/gatewayd-internal/` — all pass.
  - `TestInternalProxy_NegotiatesH2C` (TCP loopback),
    `TestInternalProxy_NegotiatesH2C_OverUnixSocket` (production
    unix-socket wire), `TestInternalProxy_HTTP11Fallback` (legacy
    rollback), `TestInternalProxy_H2CMultiplexesConcurrentStreams`
    (H2 fan-out assertion). All green under `-race -count=1`.
  - `golangci-lint run --timeout=4m` (v2.4.0, CI's exact version)
    — 0 issues.
  - Full CI: `lint + build`, all 4 unit-test shards, all 4 e2e
    shards, CodeQL, grype, migrations, sqlc-check, daemonunit-check,
    spec-check, load, image-scan, sdk-go/node/python — all
    `SUCCESS` on the branch at PR #685.

- **Migration / rollback:**

  - Default: H2C enabled (`FAAS_INTERNAL_H2C` unset → `true`).
  - Rollback: set `FAAS_INTERNAL_H2C=false` on `gatewayd-public`
    and restart. The proxy swaps to the legacy HTTP/1.1 transport
    with no schema change and no in-flight state.
  - The unified-mux mount is unconditional — disabling it would
    require reverting the entire change set. Operationally
    unnecessary: the unified-mux path is the production path; the
    legacy listener at `:8080` (`FAAS_GATEWAY_LISTEN=!off`) keeps
    the customer listener available for migrations.

- **References:**

  - ADR-070 — Tier A7 edge split (parent decision).
  - ADR-009 — strict-netns model (no extra host networking
    surface; unix-socket path stays in-cgroup).
  - ADR-015 — unix-socket DAC auth (mode 0660, group `faas`).
  - Issue #675 — the originating request.
  - PR #685 — implementation.
  - Issues #686, #687 — out-of-scope follow-ups.
