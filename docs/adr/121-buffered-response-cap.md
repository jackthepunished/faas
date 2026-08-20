# ADR-121 · Buffered response body cap (`response_too_large` 413) + warn-on-approach telemetry

- **Status:** Proposed
- **Date:** 2026-08-20
- **Issue:** #995 (Edge protection gaps — code review of the
  platform's envelope protection surface)
- **Decision:** install a per-plan `MaxResponseBodyBytes` cap on the
  **buffered** reverse-proxy path (mirroring the streaming path's
  `capWriter`), emit a 413 `response_too_large` problem+json on
  over-cap, and surface `gateway_response_body_warn_total{app_id,
  bucket}` (with `bucket ∈ {near_threshold, exceeded}`) on the
  80% / 95% / 100% threshold crossings. The failure mode is
  customer-visible, so the prior silent connection-reset (and the
  aspirational `Server.MaxResponseBodyBytes` comment that was
  actually unenforced) is replaced with a stable RFC 7807 contract.

## Context

The audit that produced issue #995 surfaced four envelope-layer gaps.
Two of them cluster around a single architectural decision that
deserves its own ADR:

1. **The buffered reverse-proxy path has no response body cap.** Only
   the streaming path wraps the response writer with a `capWriter`
   that emits a 413 problem+json on over-cap. The buffered path
   relied on `http.Server.WriteTimeout` (bytes-by-elapsed-time) and
   an aspirational comment in `pkg/gateway/handler.go:5696-5699` that
   `Server.MaxResponseBodyBytes` enforces the cap. **That field does
   not exist on stdlib `http.Server`** — the cap was unenforced.
2. **No warn-on-approach telemetry.** The streaming `capWriter`
   emits a 413 on the boundary but does not flag the 80% / 95%
   near-cap region. Customers get no advance warning that a response
   is approaching the cap, and operators have no scrape-friendly
   signal to alert on.

The other two gaps (apid `http.Server` hardening, gatewayd-internal
`http.Server` hardening) are not architectural — they're
straightforward timeouts applied to listeners that already pass
request bodies through `http.MaxBytesReader`. They ship in the same
PR but don't need an ADR.

### Why the failure mode matters

The buffered path's failure mode is **customer-visible**. Today an
app that emits a 30 MiB response on a PlanFree (25 MiB cap) sees one
of:

- A stdlib-default 502 with a `Server.MaxBytesError` field (if
  `http.MaxBytesWriter` is on the path — it isn't on the buffered
  cap, only the streaming one).
- A silent connection reset mid-body (the `capWriter.disabled` path
  returns `http.ErrHandlerTimeout` from `Write`, which stdlib's
  chunked writer surfaces as a stream interruption with no
  customer-visible body).
- An `http.Server.WriteTimeout` reset on a long body (5 minutes
  by default — way longer than the cap should leave the customer
  waiting).

None of these surfaces the platform's stable 413 contract. The
streaming path already emits `413 streaming_not_available` on
over-cap; the buffered path should mirror it with `413
response_too_large`.

### Why a warn-on-approach counter

The plan-derived cap is a *hard* limit. The customer only learns
they hit it when the response is rejected. A counter that fires at
80% and 95% of the cap gives the customer's own dashboard an
"approaching cap" indicator, and gives the operator a scrape alert
on `gateway_response_body_warn_total{app_id=…,bucket="exceeded"}`
before customers report a regression.

## Decision

### 1. Buffered reverse-proxy cap

The buffered reverse-proxy dispatch site in `pkg/gateway/handler.go`
installs a `capWriter` around the response writer at the same
dispatch point where the streaming path installs one. The cap value
is the plan's `MaxResponseBodyBytes()` (Free 25 MiB, Hobby 100 MiB,
Pro 100 MiB, Scale 100 MiB — see `pkg/api/limits.go`).

On over-cap, the `capWriter` emits a 413 problem+json with the
new closed-set code `response_too_large`:

```
HTTP/1.1 413 Request Entity Too Large
Content-Type: application/problem+json

{
  "type": "about:blank",
  "title": "response_too_large",
  "status": 413,
  "detail": "app <id> on plan <plan> is capped at <cap> bytes per response"
}
```

The same shape is in `pkg/api/dto.go` alongside the existing
`streaming_not_available` code. `CodeResponseTooLarge` is the new
constant.

### 2. Architectural constraint on the buffered path

The buffered reverse-proxy is `httputil.ReverseProxy`: by the time
`capWriter.Write` is called, the upstream's `WriteHeader(200)` has
already been relayed to the client. Calling `api.WriteProblem`'s
`WriteHeader(413)` at that point is a silent no-op (stdlib logs
"superfluous header"). The contract is therefore:

- If the cap trips **before** the upstream emits headers (rare; the
  proxy reads headers from the upstream response, then immediately
  starts the body), the wrapper's `WriteHeader(413)` is the first
  header write and lands on the wire.
- If the cap trips **after** the upstream emits 200, the wrapper
  forces a connection reset / EOF at the body boundary. The
  customer's parser sees an incomplete stream, not a 200 with a
  truncated body — which is the same hardened behaviour the
  streaming cap uses.

The 413 problem+json shape is the stable contract; the network
behaviour depends on header ordering. This is documented in
`pkg/gateway/handler.go::setupBufferedCapWriter` and exercised in
`pkg/gateway/buffered_cap_test.go`.

### 3. Per-plan cap accessor

The plan-derived cap is read from `app.Plan.MaxResponseBodyBytes()`
(`pkg/api/limits.go:3530-3539`), the existing accessor. No new
constants are introduced for the cap value itself — the
table-driven test in `pkg/gateway/handler_test.go` covers the
plan-matrix AC.

### 4. Warn-on-approach telemetry

The streaming `capWriter` AND the new buffered `capWriter` emit an
`onWarn(bucket string)` callback at:

- **80%** of `cap` — `bucket="near_threshold"`.
- **95%** of `cap` — `bucket="near_threshold"` (a higher urgency
  signal, but the bucket label is the same so the dashboard can
  use a single threshold filter).
- **100%** (over-cap) — `bucket="exceeded"`.

The two thresholds are independent `atomic.Bool` CAS guards. A
single `Write` that crosses both 80% and 95% fires both. The
counter is:

```
gateway_response_body_warn_total{app_id="<id>", bucket="near_threshold"|"exceeded"}
```

The threshold pair is fixed at 80% / 95% — fixed thresholds match
the streaming-tier precedent (ADR-047) and avoid per-app knobs that
would invite customer-side drift.

#### Cardinality

The label set is `{app_id, bucket}`. `bucket` is closed-set (2
values). `app_id` is bounded by the per-account app count
(~100s) per ADR-093 precedent. `route_label` is intentionally NOT
admitted (would explode per the ADR-093 cap of 50 per app).

#### slog dedup

The Prometheus counter is the scrape-friendly signal. The slog
warn is the once-per-process log dedup — keyed on
`(app_id, bucket)`. The first hit emits a structured
`slog.Warn("gateway: response body approaching or exceeding
per-plan cap", {app, bucket, bytes, cap})`; subsequent requests
bump only the counter. Pattern mirrors the existing
`streamingWarned` (`pkg/gateway/handler.go:565-579`).

### 5. Apid loopback applies the same cap

The apid loopback proxy in `cmd/gatewayd-internal/proxy.go` is a
reverse proxy too — it inherits the same unbounded-response-body
failure mode. The capWriter is installed with the Free-tier
baseline (`api.MaxResponseBodyBytesDefault` — 25 MiB) as
defence-in-depth. Apid's control plane returns tiny JSON, so the
cap is rarely the active limit; it exists for the same reason the
gatewayd-internal public listener caps request headers.

### 6. Spec & limits sync

- `pkg/api/limits.go::MaxResponseBodyBytesDefault` — the existing
  accessor. Comment updated to point to `setupBufferedCapWriter`
  and `setupStreamingWriter` as the two enforcement points.
- `pkg/gateway/handler.go:5696-5699` — the aspirational comment
  about `Server.MaxResponseBodyBytes` is replaced with the truthful
  one pointing to the upstream `io.LimitReader` + downstream
  `capWriter` pair.
- `docs/faas_implementation_spec.md` §4.1 — the "25 MB either
  direction" bullet is updated to call out the buffered-vs-
  streaming distinction and the 413 failure mode, with a §17
  reference to the warn-on-approach counter.

## Why not per-route overrides

The plan-derived cap is the right default. Per-route overrides
would let the customer defeat the cap on a single hot path — but
the cap is a financial-model constraint (§4.7 — billing uses plan
RAM + 8 MB per running second, not sampled RSS), not a customer
control. A per-route override would also re-litigate the
ADR-093 cardinality precedent, because override values would
necessarily live on the `EdgeRule` shape and the per-rule label
cardinality is bounded at 50 per app.

A per-route cap is a reasonable follow-up — but it lands after
the per-plan cap is observed.

## Risks (numbered)

- **R1. Stdlib's `http.MaxBytesWriter` interaction.** The streaming
  path already uses `capWriter` (not `http.MaxBytesWriter`) because
  the stdlib type writes a 502 directly to the underlying writer
  with no opportunity to inject the 413 problem+json shape. The
  buffered path likewise uses `capWriter` — the same precedent.
- **R2. `httptest.ResponseRecorder` interaction.** The new
  `capWriter.WriteHeader` emits a "superfluous header" warning
  on the recorder when the upstream already wrote 200. The test
  in `pkg/gateway/buffered_cap_test.go` accepts either outcome
  (413 problem+json OR connection reset / EOF) per the architectural
  contract documented in `setupBufferedCapWriter`.
- **R3. Threshold drift under chunked writes.** A streaming
  customer that emits 64-byte chunks might "tickle" the 80%
  threshold many times. The per-request `atomic.Bool` guards
  prevent double-fire, so the counter is still increments-per-
  request (1 per threshold, 2 max). The slog dedup is keyed
  `(app_id, bucket)` so log lines are per-process, not per-
  request.
- **R4. counter cardinality creep.** The metric is labelled on
  `app_id`. ADR-093 bounds the active app count at ~50 per
  account × 100 accounts = 5,000 series. Two bucket values =
  10,000 series ceiling. Well within the project's Prometheus
  budget.
- **R5. Plan-free apps on Hobby cap.** A customer who upgrades
  from Free to Hobby sees the cap jump from 25 MiB to 100 MiB
  in-place. Historical counter values are stable across the
  upgrade (the counter is per-app, not per-plan), so the
  customer's near-cap history survives the upgrade.
- **R6. Race in the per-request disabled CAS.** The existing
  `capWriter.Write` disabled CAS guards the `onCap` and `onWarn`
  fires correctly under concurrent writes. The new `onWarn` call
  is added inside the same critical section as `onCap` — the
  race detector covers this in `go test -race ./pkg/gateway/`.

## Cross-references

- ADR-009 (identical inner network world — the snapshot-reuse
  invariant that makes a per-plan cap viable).
- ADR-047 PR-B / PR-D (streaming cap on the SSE path — the
  precedent for `capWriter` and the 413 emission pattern).
- ADR-070 (split public listener — gatewayd-public is the edge
  that enforces the timeouts; this ADR is about the gatewayd-
  internal proxy path's cap, not the edge).
- ADR-093 (per-app label cardinality — the precedent for
  bounding `app_id` series).
- ADR-104 (per-consumer throttle decisions — the precedent for
  closed-set label pairs).
- Issue #995 (this ADR's mega-issue).
- Issue #975 (mega-Foundation that owns the streaming cap).
- Spec §4.1, §4.7, §17.
