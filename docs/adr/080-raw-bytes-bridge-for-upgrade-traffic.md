# ADR-080 · Raw-bytes bridge for Upgrade traffic over the gatewayd-internal → vmmd → guest path

- **Status:** accepted
- **Superseded (in part, PR-E):** prose referred to the monolithic
  `cmd/gatewayd/` daemon split by ADR-070 into `gatewayd-public` (TLS-only
  edge) and `gatewayd-internal` (routing + wake + proxy). Body is preserved
  verbatim; readers should substitute "gatewayd-internal" for the
  routing/wake/proxy path and "gatewayd-public" for the certmagic/TLS path.
  `cmd/gatewayd/<file>.go` citations in this body are stale; see PR-E for
  the new file locations.
- **Date:** 2026-08-07
- **Decision:** Two coordinated changes that close issue #676:

  1. **New gRPC bidi on vmmd.** Add
     `Vmmd.ForwardRawStream(stream ForwardRawRequest) returns (stream ForwardRawResponse)`
     to `api/proto/onebox/faas/vmmd/v1/vmmd.proto` (the legacy
     `ForwardHTTPStream` stays the path for plain HTTP / SSE / chunked
     and is not modified by this ADR). The server handler lives at
     `pkg/vmmdgrpc/forward.go:519` and spawns a new Go binary
     `cmd/vmmd-raw-bridge/main.go` (273 lines) per stream. The bridge
     calls `golang.org/x/sys/unix.Setns` to enter the guest's netns,
     `net.Dial("tcp", guestIP:port)`, runs two `io.Copy` goroutines
     (stdin ↔ conn, conn ↔ stdout), and emits a parsed status-line +
     headers head on stdout followed by raw bytes — the same head
     format `pkg/vmmdgrpc/forward.go:1043` `parseBridgeOutput` splits
     on `\n\n`. The wire is bytes-in / bytes-out: no framing
     metadata beyond a single init envelope on each side; no
     discriminator byte shares the existing
     `ForwardHTTPStreamRequest` body channel.

  2. **gatewayd-internal Upgrade detection + raw forwarder.**
     `pkg/gateway/handler.go` inserts a three-input gate —
     `isUpgradeRequest(r) && app.WebSocketEnabled && h.rawByNode != nil`
     — between `h.backend.Pick(app.ID)` and `h.proxyByNode(target).ServeHTTP`.
     When the gate fires, the new `rawStreamOnceWithEvents` opens the
     `ForwardRawStream` RPC on the wake-node's vmmd client (local OR
     remote — `NodeClientCache` is the resolution seam), sends the
     customer's bytes verbatim, and pumps the guest's response back
     through the inbound writer. `pkg/gateway/internal_proxy.go:228`
     adds a 3-line guard that skips `stripHopByHopInPlace` when the
     request is an upgrade, so the public→internal H2C hop
     (ADR-079) preserves the handshake too. `hopByHopHeaders`
     (`pkg/gateway/forwardproxy.go:66-75`) is unchanged — the upgrade
     path bypasses the strip entirely.

- **Why:** Five forcing facts:

  1. **The existing shell bridge destroys the WS handshake.** The
     pre-PR-1 `pkg/vmmdgrpc/forward.go:454-511` bridge hard-codes
     `Transfer-Encoding: chunked` on the outbound side (forward.go:469)
     and rewrites `Host:` to the inner-side IP (forward.go:467-468).
     A WebSocket `Connection: Upgrade` + `Upgrade: websocket`
     handshake cannot survive either transformation — the chunked
     framing would corrupt the first bytes of the inbound upgrade
     response, and the `Host:` rewrite would strip the customer's
     virtual-host header before the guest's `Sec-WebSocket-*` check
     sees it.

  2. **Three places strip or mangle Upgrade headers today.**
     `pkg/gateway/forwardproxy.go:66-75` `hopByHopHeaders` (the
     plain-HTTP path strips `Connection` + `Upgrade`),
     `pkg/gateway/internal_proxy.go:228` `stripHopByHopInPlace`
     (the public→internal hop strips the same headers), and the
     vmmd shell bridge. None can pass `Connection: Upgrade` +
     `Upgrade: websocket` end-to-end without a guarded bypass.

  3. **Go-level TCP control is what carries Upgrade bytes.** Lifting
     the bridge from bash to Go (`cmd/vmmd-raw-bridge/main.go`,
     273 lines: `unix.Setns` → `net.Dial("tcp", guestIP:port)` →
     goroutine A pipes stdin → conn → goroutine B pipes conn → stdout)
     gives us per-direction deadlines, body-cap enforcement via
     `io.LimitReader`, and clean goroutine teardown on stream
     cancel. The new RPC `ForwardRawStream` is the wire that
     surfaces this Go-level pump; its `parseBridgeOutput` head-split
     on `\n\n` matches the existing shell-bridge contract, so the
     upgrade response is bytes-identical to a plain-HTTP response.

  4. **A separate RPC keeps each contract narrow (ADR-016 additive).**
     Mixing raw-byte mode into `ForwardHTTPStreamRequest` would force
     a discriminator byte and a per-frame switch — ugly and a future
     wire-bump hazard. A separate bidi leaves the legacy RPC's
     contract alone and gives the new path its own envelope
     (`ForwardRawRequestInit{instance, port, max_request_bytes}`
     for setup; `bytes body_chunk` for the body stream).

  5. **H2C is the inbound transport after ADR-079; H2 DATA frames are
     raw bytes once `WriteHeader(101)` lands.** The
     gatewayd-public → gatewayd-internal hop is HTTP/2 cleartext
     (ADR-079); after `WriteHeader(101)` the inbound writer is in
     raw-data mode and `http.NewResponseController(w).Flush()` emits
     raw DATA frames. The customer's stack sees the WS handshake on
     top of the same TCP/TLS connection — no `http.Hijacker.Hijack()`
     shim (which would be a no-op on H2 anyway), no special framing.

- **Architecture after the change:**

  ```
  customer (TLS+H2, WS-aware)
    ↓
  Caddy (TLS termination, H2 → H1/WS to upstream)
    ↓
  gatewayd-public:8080 (H2C client)
    ↓  IF Connection: Upgrade + Upgrade: <token> → skip hop-by-hop strip
    ↓  ELSE → existing strip (plain HTTP path, unchanged)
    ↓
  gatewayd-internal (/run/faas/gatewayd-internal.sock, H2C server)
    ├── plain HTTP / SSE / chunked → proxyByNode → ForwardingReverseProxy → ForwardHTTPStream → vmmd shell bridge (unchanged)
    └── Connection: Upgrade + Upgrade: <token> → rawStreamReverseProxy → ForwardRawStream → vmmd Go-bridge → guest netns TCP
                                  ↑
                                  └─ NodeClientCache picks the right vmmd (local or remote; same channel as ForwardHTTPStream)
  ```

- **Consequences:**

  - **vmmd wire surface:** `Vmmd.ForwardRawStream` added to
    `api/proto/onebox/faas/vmmd/v1/vmmd.proto`. The handler at
    `pkg/vmmdgrpc/forward.go:519` (`ForwardRawStream`) is the entry
    point; it spawns `cmd/vmmd-raw-bridge` via
    `exec.CommandContext(stream.Context())` with argv
    `netnsName guestIP port headTimeoutMs` and pipes per-frame
    `body_chunk` reads into the bridge's stdin. The shell bridge
    (`pkg/vmmdgrpc/forward.go:454-511`) is unchanged on the plain
    HTTP path.

  - **gatewayd-internal detector:** `pkg/gateway/handler.go` inserts
    the upgrade detector between `target, ok := h.backend.Pick(app.ID)`
    and `h.proxyByNode(target).ServeHTTP`. Three-input gate:
    `isUpgradeRequest(r) && app.WebSocketEnabled && h.rawByNode != nil`.
    `pkg/gateway/upgrade.go` holds the shared `isUpgradeRequest(r)`
    helper (case-insensitive on both `Connection` token parsing AND
    the `Upgrade` header check, per RFC 7230 §3.2). The detector
    accepts ANY Upgrade value (websocket, h2c, mqtt, ...) — the raw
    RPC carries bytes verbatim, so a non-WS token simply flows to
    the guest unchanged. The detector stamps `r.Header.Set(
    "x-faas-upgrade", "true")` for observability (matches the
    ADR-064 wake-timeline vocabulary).

  - **gatewayd-internal hop-by-hop preservation:**
    `pkg/gateway/internal_proxy.go:228` adds a 3-line guard that
    skips `stripHopByHopInPlace` when the request is an upgrade,
    so the public→internal H2C hop (ADR-079) preserves the upgrade
    handshake. The response-side strip at `internal_proxy.go:267-274`
    is unchanged — the bridge already echoes Connection/Upgrade on
    the response, and the public hop's response strip does not
    affect the upgrade flow.

  - **`hopByHopHeaders` unchanged:** `pkg/gateway/forwardproxy.go:66-75`
    keeps stripping `Connection` + `Upgrade` on the plain-HTTP path
    to vmmd. The upgrade path bypasses the strip entirely — bytes
    flow raw through `rawStreamOnceWithEvents`, never touching the
    `hopByHopHeaders` mutator.

  - **Per-app + per-plan gating:** new column `apps.websocket_enabled`
    (default false) added by migration `00155_apps_websocket_enabled.sql`
    (`ALTER TABLE apps ADD COLUMN websocket_enabled BOOLEAN NOT NULL
    DEFAULT FALSE`). `pkg/api/limits.go` adds
    `Plan.WebSocketEnabled()` (Free false; Hobby/Pro/Scale true) and
    `Plan.WebSocketResponseAllowed()` (Free false; Hobby+ true). The
    per-plan default is applied at CreateApp time in
    `cmd/apid/handlers.go::buildApp`; an existing app may still flip
    the flag via PATCH (gated by `WebSocketResponseAllowed` so Free
    stays off even when an admin backfills the column). The Free
    fail-closed contract surfaces `403 plan_websocket_not_allowed`
    (new code in `pkg/api/errors.go:471`).

  - **Reused infra:** `NodeClientCache`
    (`pkg/gateway/forwardproxy.go:497-626`) is local-or-remote
    transparent — the same gRPC channel serves `ForwardHTTPStream`
    and `ForwardRawStream`. No dialer change; no cache change; the
    new RPC inherits the cache's eviction discipline on
    `compute_node_changed`. `WakeGate` (`pkg/gateway/gate.go:16-21`)
    keys on `appID` alone and is transport-agnostic — the upgrade
    path inherits coalescing. `pkg/wire.DialContext`
    (`pkg/wire/grpc.go:165-234`) is the existing one-shot dialer
    used by the production NodeClientCache.

  - **Body cap:** `api.RawStreamMaxRequestBytes = 100 MiB`
    (`pkg/vmmdgrpc/forward.go:424-426` →
    `pkg/api/limits.go:1426`). The gateway stamps it on the init
    frame's `MaxRequestBytes` field; vmmd's bridge clamps DOWN
    with `io.LimitReader` so a 100 MiB+ body never OOMs the
    gateway (the gateway streams in 8 KiB chunks via the
    `ctxReader` pattern at `pkg/gateway/forwardproxy.go:335`).
    This matches `ForwardStreamMaxBodyBytes` so the upgrade path
    has the same inbound budget as the streaming path.

  - **Rollout gate:** the feature ships enabled at the merge point.
    Per-app gating via `PATCH /v1/apps/{id} websocket_enabled=false`
    is the operator escape hatch — see Migration / rollback below.
    (An `FAAS_GATEWAY_RAW_STREAM_ENABLED` feature flag is a
    follow-up; the merged PR-3 wiring does not include one yet —
    filed as a follow-up to this ADR.)

  - **Observability:** `x-faas-upgrade: true` observability header
    stamped on the inbound request (matches the ADR-064 wake-timeline
    vocabulary). `evts.Platform` events emitted on the first
    downstream byte via `evts.ProxyFirstByte` — same seam the
    streaming forwarder uses (ADR-064 precedent). (Four
    `gateway_ws_*` Prometheus series — `gateway_ws_upgrade_total`,
    `gateway_ws_active_sessions`,
    `gateway_ws_session_duration_seconds`,
    `gateway_ws_session_bytes_total{direction}` — are documented
    as the metering seam for the per-session byte cap follow-up
    PR; they are NOT defined in `pkg/gateway/metrics.go` at this
    point and the merged raw forwarder emits only the events seam
    above. Filed as a follow-up to this ADR.)

  - **Cross-references:** ADR-016 (additive wire change), ADR-028
    (default-local wake; the new RPC inherits local-or-remote
    transparency), ADR-047 (streaming response cap-lift;
    `WebSocketEnabled` follows the same Free-denied pattern),
    ADR-052 (control-plane mTLS — the new RPC inherits the same
    auth, so #678's mTLS work covers it), ADR-070 (Tier A7 edge
    split; the 3-line guard in `internal_proxy.go` is the
    public→internal fix), ADR-079 (H2C public→internal hop;
    orthogonal transport — this ADR preserves the upgrade bytes
    through the same transport).

- **Out of scope (filed as separate NETWORK issues):**

  - **Issue #686** — H2C on the gatewayd-internal → vmmd → guest
    `:8080` leg requires rewriting the streaming bridge in Go
    (it hard-codes `HTTP/1.1` in shell at
    `pkg/vmmdgrpc/forward.go:464`). ADR-080's raw RPC is the
    durable Upgrade path; #686 deletes the chunked shell bridge
    for PLAIN traffic but leaves `ForwardRawStream` intact. The
    two are orthogonal — plain traffic does not need the raw RPC
    because the shell bridge already handles
    `Transfer-Encoding: chunked` correctly. ADR-080 closes the
    upgrade half of the gap; #686 closes the plain-traffic half.

  - **Issue #687** — `pkg/gateway/internal_proxy.go` latent bugs
    activated by H2C multiplexing: no `http.Flusher.Flush` on body
    copy (~line 275), body-copy goroutine lifetime after ctx cancel
    (~line 311-320), `DialTimeout` wrapping the full `RoundTrip`
    context (~line 240-244). The raw upgrade path inherits the
    same latent issues; the fix is in the inner-leg rewrite scope
    (#686), not here.

  - **WS over H2 multiplexing** — one WS = one H2 DATA stream in
    v1.0. A future PR opens N concurrent DATA streams on the same
    inbound connection to amortize the per-stream TLS overhead.
    ADR-080's wire accommodates this — `RawStreamSessionDeadline`
    and `MaxRequestBytes` are per-stream, not per-connection.

  - **Per-session byte cap at the platform layer** — the metering
    seam is the `evts.Platform` events surface (ADR-064) plus a
    follow-up `gateway_ws_session_bytes_total{direction}`
    Prometheus counter (not yet defined). The actual
    cap-and-throttle enforcement is a follow-up metering PR
    (cite ADR-048 meterd-gbh-floor as the model). v1.0 has no
    customer-visible cap; the vmmd-side 100 MiB per-request cap
    is the load-bearing ceiling.

  - **HTTP/3 / QUIC** — issue #680.

- **Verified:**

  - `go test -race ./pkg/gateway/... ./pkg/vmmdgrpc/... ./pkg/api/...
    ./cmd/gatewayd-internal/... ./cmd/apid/...
    ./cmd/vmmd-raw-bridge/...` — all pass.

  - **Raw forwarder round-trip:**
    `TestRawStreamReverseProxy_RoundTrip`,
    `TestRawStreamReverseProxy_RemoteWakeNode`,
    `TestRawStreamReverseProxy_StatusRecorderFlushes`,
    `TestRawStreamReverseProxy_ClientCancel_TearsDownStream`,
    `TestRawStreamReverseProxy_InitError_Populated` in
    `pkg/gateway/forwardproxy_test.go` — all green under
    `-race -count=1`.

  - **vmmd-side handler:**
    `TestForwardRawStream_HappyPath_UpgradeEcho`,
    `TestForwardRawStream_GuestRefusesConnection`,
    `TestForwardRawStream_UnknownInstance`,
    `TestForwardRawStream_BodyCapEnforced`,
    `TestForwardRawStream_ContextCancelClosesGuest`,
    `TestForwardRawStream_LargePayload` in
    `pkg/vmmdgrpc/forward_internal_test.go` — all green.

  - **Detector:**
    `TestServeHTTP_UpgradeHeader_BypassesProxyByNode`,
    `TestServeHTTP_NonUpgradeHeader_TakesProxyByNode`,
    `TestServeHTTP_FreePlan_WebSocketNotAllowed`,
    `TestServeHTTP_WebSocketEnabled_DispatchesToRawStream` in
    `pkg/gateway/forwardproxy_handler_test.go` — all green.

  - **Public-hop preservation:**
    `TestInternalProxy_PreservesUpgradeHeaders`,
    `TestInternalProxy_StripsNonUpgradeHopByHop`,
    `TestInternalProxy_StripsCaseInsensitiveUpgrade` in
    `pkg/gateway/internal_proxy_test.go` — all green.

  - **e2e (gorilla/websocket):**
    `TestRawStreamReverseProxy_E2E_WithGorillaWS`,
    `TestRawStreamReverseProxy_LongLived`,
    `TestRawStreamReverseProxy_ClientDisconnectMidFrame` in
    `pkg/gateway/forwardproxy_handler_test.go` — all green;
    16 KiB binary-frame exchange round-trips bit-for-bit.

  - **`golangci-lint run --timeout=4m`** (v2.4.0, CI's exact
    version) — 0 issues.

  - **Full CI on the merge-point branch:** `lint + build`, all 4
    unit-test shards, all 4 e2e shards, CodeQL, grype,
    migrations, sqlc-check, daemonunit-check, spec-check, load,
    image-scan, sdk-go/node/python — all `SUCCESS`.

  - **PR #694** — PR-1 (ForwardRawStream wire + vmmd-side handler
    + bridge binary), merged 2026-08-06.
  - **PR #702** — PR-3 (gateway detector + raw forwarder + e2e,
    bundling the original PR-3 + PR-4 per user decision), merged
    2026-08-07.

- **Migration / rollback:**

  - **Default:** the feature ships enabled at the merge point.
    There is no daemon-level kill switch in PR-3 (the
    `FAAS_GATEWAY_RAW_STREAM_ENABLED` env var is filed as a
    follow-up to this ADR — the merged PR-3 wiring installs
    `WithRawForwarding` unconditionally when `deps.nodeCache != nil`,
    so the only post-merge operator control is per-app PATCH
    below).

  - **Per-app rollback:** `PATCH /v1/apps/{id}` with
    `websocket_enabled=false` flips an individual app off at the
    three-input detector (handler.go: `isUpgradeRequest(r) &&
    app.WebSocketEnabled && h.rawByNode != nil`) — the request
    falls through to `proxyByNode` and the plain-HTTP strip
    removes Connection + Upgrade as hop-by-hop, returning 502
    from the upstream (the standard "no-WS-handshake-here"
    failure path; not a deterministic 501 — a deterministic 501
    path is the follow-up env var above). The Free plan's
    per-plan `WebSocketResponseAllowed() == false` makes the
    PATCH path fail-closed for free customers even if an admin
    backfills the column (the `403 plan_websocket_not_allowed`
    response gates the PATCH before the column write lands).

  - **Schema rollback:** `ALTER TABLE apps DROP COLUMN
    websocket_enabled;` is non-blocking (no FK, no NOT NULL
    elsewhere, no index). The apid layer stops reading the
    column at next deploy; per-app flag disappears cleanly.

  - **Wire rollback:** `ForwardRawStream` is additive; the legacy
    `ForwardHTTPStream` RPC is untouched. A future RPC removal
    follows the gRPC deprecation convention
    (`google.api.HttpRule` annotation + 90-day notice); the
    bridge binary can be deleted only after every gatewayd has
    rolled back off the new RPC.

- **References:**

  - ADR-009 — strict-netns model (the bridge binary joins the
    guest's netns; no extra host networking surface).
  - ADR-015 — unix-socket DAC auth (mode 0660, group `faas`) —
    the local-wake case uses the same `unix:///run/faas/vmmd.sock`
    channel as `ForwardHTTPStream`; no auth change.
  - ADR-016 — additive wire changes (the new RPC follows naming
    convention; no mode flag on `ForwardHTTPStream`).
  - ADR-028 — default-local wake (the new RPC inherits
    local-or-remote transparency via `NodeClientCache`).
  - ADR-047 — streaming response cap-lift (`WebSocketEnabled`
    follows the same Free-denied pattern).
  - ADR-048 — meterd-gbh-floor (model for the follow-up metering
    PR that consumes the WS-events surface and ships the
    `gateway_ws_session_bytes_total` counter).
  - ADR-052 — control-plane mTLS + handler peer binding (the new
    RPC inherits the same auth).
  - ADR-064 — wake-timeline vocabulary (`x-faas-upgrade: true`
    header matches the same observability pattern).
  - ADR-070 — Tier A7 edge split (parent decision; the 3-line
    guard in `internal_proxy.go` is the public→internal fix).
  - ADR-079 — H2C public→internal hop (orthogonal transport; this
    ADR preserves the upgrade bytes through the same transport).
  - Issue #676 — the originating request (WebSocket /
    long-poll / MQTT-over-WS support).
  - Issue #678 — control-plane mTLS (covers the new RPC's auth).
  - Issue #680 — HTTP/3 / QUIC (out of scope here).
  - Issue #686 — H2C inner-leg rewrite (out of scope here;
    closes the plain-traffic half).
  - Issue #687 — `internal_proxy.go` latent bugs (out of scope
    here; closes the H2C-multiplexing bugs).
  - PR #694 — PR-1 (ForwardRawStream wire + vmmd-side handler +
    bridge binary), merged 2026-08-06.
  - PR #702 — PR-3 (gateway detector + raw forwarder + e2e,
    bundling the original PR-3 + PR-4 per user decision), merged
    2026-08-07.