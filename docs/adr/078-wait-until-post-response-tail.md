# ADR-078 · `waitUntil` post-response tail primitive

- **Status:** accepted
- **Date:** 2026-08-06
- **Decision:** Add `ctx.waitUntil(promise)` to the guest-init runtime. The
  handler registers background work; the response flushes immediately; the
  wake stays `RUNNING` (and therefore metered via the existing
  `usage_minutes.mb_seconds`) until every registered task completes or a
  per-plan timeout fires. The primitive is bounded by a per-task
  `context.WithTimeout(WaitUntilSec)` enforced inside the runner's in-process
  tail host, a per-request `TailCapMax = 16` structural cap, and a per-plan
  `ConcurrentTailsPerInstance` cap (Free 4 / Hobby 16 / Pro 64 / Scale 256).
  No new billing surface. No new gRPC RPC. No host-side goroutine per wake.
- **Why:** Issue #667. Every modern function platform (Cloudflare Workers,
  Vercel Edge Functions, AWS Lambda) ships `waitUntil` for the
  fire-and-forget-after-response pattern (send a confirmation email, post a
  webhook to a third party, write an analytics event, schedule a follow-up
  task). Today the only way to do this inside a Gregale microVM is to
  inflate the synchronous handler latency, which ties up the wake, confuses
  customers on gateway latency, and is the only surface for "fire-and-forget
  inside a microVM". The primitive is the single most-asked-for addition to
  the function surface.
- **Issue:** #667

## Consequences

### Wire (runner → handler subprocess envelope)

The runner-side `envelope` struct gains two fields in all 5 runtime shims
(`guest/runners/{node22,node24,python312,python313,go124}/main.go`):

```jsonc
{
  "method": "GET",
  "path": "/foo",
  "headers": {...},
  "query": "",
  "body_b64": "",
  "wait_until_sec": 30,             // NEW: per-task wall-clock ceiling (issue #667 §"Rules")
  "tail_pipe_path": "/tmp/..."      // NEW: JSONL pipe the handler appends to per waitUntil(promise) call
}
```

The handler subprocess emits one JSONL line per `ctx.waitUntil(promise)`
registration to `tail_pipe_path`. The runner reads the pipe, starts a
`sync.WaitGroup` of per-task goroutines each wrapped in
`context.WithTimeout(WaitUntilSec)`, drains them, and writes a 0x04 DGRAM
to vsock port 1027 on each terminal event (completed / failed / timeout).
The response envelope is written to stdout once the tail drains (or
times out) — `signal.SignalReady(...)` continues to fire exactly once per
wake, on the first non-5xx response, and is not retriggered by the tail.

### Wire (runner → guest-init, vsock DGRAM port 1027 lead byte `0x04`)

Fixed-size 16-byte envelope:

| Byte offset | Field | Notes |
|---|---|---|
| 0 | 0x04 | type discriminator (joins 0x01 framework_ready, 0x02 sidecar_init_exit, 0x03 sidecar_restart) |
| 1 | uint8 outcome | `1=completed`, `2=failed`, `3=timeout` |
| 2..7 | reserved | 6 bytes zero-padded; gives a wire-incompatible-free upgrade path (a future ADR can repurpose the reserved bytes for an additional discriminator, sequence id, or compressed instance hash without breaking existing runners) |
| 8..15 | elapsed_ms uint64 BE | wall-clock from waitUntil registration to terminal (the runner's per-task timer) |

The instance identity is resolved from the DGRAM peer CID (vsock context id) on the guest-init receiver, not from the wire. Each wake has its own vsock context with a stable CID for its lifetime; reusing it as the instance handle keeps the envelope a flat 16 bytes and avoids dragging a 16-byte UUID across the wire on every terminal.

guest-init's `framework_ready_proxy_linux.go` multiplexer gains a new
`0x04` branch that calls `handleTailEvent(outcome, elapsedMS)`. The
instance is resolved from the peer CID inside `handleTailEvent` (see
the "Wire" preamble above for why), so it does NOT appear in the
helper signature — a reviewer diffing this file should NOT re-add
it: the wire is 16 bytes by design and the resolver lives in
guest-init's per-context table.
vmmd's `SendStatelessAdvisory` path stamps the metric
`schedd_tail_count` and bumps the per-instance `instances.tail_count`
column via `state.Store.DecrementInstanceTailCount` /
`state.Store.AppendUsage` (new `tailSeconds` argument). The existing port
1027 lead-byte discriminator + port 1026 host-side receiver is reused
verbatim — no new port, no new CID, no new gRPC RPC.

### State machine

`pkg/state/machine.go` is unchanged. `StateRunning` is the only state a
tailing instance occupies; `tail_count` is a column on `instances`, not a
state. The wake stays `RUNNING` because the runner subprocess is still
alive inside the VM — the platform needs no new process model.

### Reaper gate (`pkg/sched/reaper.go`)

`ReapIdle`, `ReapAggressive`, and `SelectEvictions` gain a `TailCount > 0`
early-out (mirrors the G7 `OpenConns > 0` gate). A wake with active tails
is not idle-eligible, cannot be aggressively scaled in, and cannot be
evicted by RAM pressure. The `InstanceInfo` carrier struct gains
`TailCount int`.

### Park gate (`pkg/sched/engine.go::snapshotAndPark`)

The `StateRunning → StateSnapshotting` transition reads
`instances.tail_count`. If non-zero, the engine waits on a 5 s watchdog
(`ParkTailDrainTimeoutSeconds`) for the runner to drain. If the watchdog
fires, the engine force-parks with a `wake.tail_drained_at_park` audit row
and emits `wake.tail_failed{reason=forced_at_park}` for any unfinished
tails.

### Migration (slot 00151 — slot walked past 00149 / 00150 reservations)

`migrations/00151_wait_until_tail.sql` adds two replay-safe columns:

- `instances.tail_count integer NOT NULL DEFAULT 0` — the in-flight tail
  task counter, bumped by the runner on every `waitUntil` registration,
  decremented on each terminal. The reaper reads this column.
- `usage_minutes.tail_seconds bigint NOT NULL DEFAULT 0` — cumulative
  wall-clock seconds the instance spent draining tail tasks during this
  minute. Additive merge via `AppendUsage` (mirrors
  `cpu_usec` / `tx_bytes` shape from migrations 00055 / 00067).

Companion test `00151_wait_until_tail_test.go` pins replay safety +
column types + the non-negative floor on `DecrementInstanceTailCount`.

### Limits (`pkg/api/limits.go`)

Three new `Limits` fields, populated per plan in `planLimits`:

| Plan | `TailTimeoutS` | `TailCapMax` | `ConcurrentTailsPerInstance` |
|---|---|---|---|
| Free | 5 | 16 | 4 |
| Hobby | 15 | 16 | 16 |
| Pro | 30 | 16 | 64 |
| Scale | 60 | 16 | 256 |

`TailCapMax = 16` is structural (the issue's `TailCapMax = 16`); the
per-plan `ConcurrentTailsPerInstance` controls how aggressive the cap is
across concurrent requests. New accessors: `Plan.TailEnabled()`,
`Plan.TailTimeoutSeconds()`, `Plan.TailCapMax()`,
`Plan.ConcurrentTailsPerInstance()`. `pkg/api/limits_test.go` pins the
matrix.

### Metering (informational only)

`pkg/meter/sampler.go::RolledRow` gains `TailSeconds int64`. The
`SampleAndRoll` tick reads per-instance tail-elapsed sums and populates
the new `usage_minutes.tail_seconds` column via `AppendUsage`. The
rollup table `usage_daily` mirrors the column additively.

`pkg/meter/pusher.go::PushHour` is **unchanged**. `tail_seconds` does not
enter `Math.GBHours`, `Provider.PushUsageRecord`, `providerOpsFor`, or any
Stripe/Paddle payload shape. A permanent guard test
`pkg/meter/pusher_shadow_test.go::TestPushHour_ExcludesTailSeconds` pins
this — a follow-up ADR would have to remove it.

### Metrics (`pkg/wire/metrics.go`)

Three new fields on `OpsMetrics`:

- `guestTailSeconds` (`HistogramVec`, labels `{plan, runtime, outcome}`)
  — runtime ∈ {node22, node24, python312, python313, go124} (closed set,
  5 values), outcome ∈ {completed, failed, timeout} (closed set, 3
  values). Total series: 4 × 5 × 3 = 60.
- `guestTailFailedTotal` (`CounterVec`, labels `{plan, reason}`) —
  reason ∈ {timeout, handler_error, forced_at_park, unknown} (closed set,
  4 values). Total series: 4 × 4 = 16.
- `tailCapReached` (`CounterVec`, label `{plan}`) — fires when a customer
  tries to register the `(ConcurrentTailsPerInstance + 1)`-th tail. 4
  series.

Closed-set labels are pre-instantiated at `NewOpsMetrics` so the registry
never sees an unknown label. Bucket set for `guestTailSeconds`:
`{0.05, 0.1, 0.2, 0.5, 1, 2, 5, 10, 30, 60, 180, 600}` — sized for the
Free→Scale matrix (5 s / 60 s) plus the forced-at-park tail (5 s
watchdog).

## Files

**New:**

- `docs/adr/078-wait-until-post-response-tail.md` — this ADR
- `migrations/00151_wait_until_tail.sql` + `_test.go` — the schema migration
  + companion replay-safety test
- `guest/runners/internal/runnerparity/tail_test.go` — `waitUntil`
  semantics parity test (table-driven over all 5 runners)
- `pkg/fcvm/tail_metal_test.go` — end-to-end metal acceptance
  (`//go:build metal`)

**Modify:**

- `guest/init/framework_ready_proxy_linux.go` — new `0x04` dispatch branch
  in the multiplexer (calls `handleTailEvent(outcome, elapsedMS)` with
  instance resolved from peer CID — see "Wire" above)
- `guest/init/sidecar_events_proxy_linux.go` — extends the existing
  sidecar-events helper signatures to take the new `tail_event` lead
  byte (`0x04`); the impl went here, NOT into a separate
  `tail_events_proxy_linux.go` (deliberate — keeps the lead-byte
  discriminator table co-located with the existing 0x01/0x02/0x03
  branches; a reviewer diffing this file should NOT split it into a
  fourth file).
- `guest/runners/{node22,node24,python312,python313,go124}/main.go` —
  envelope + response struct extension + in-process tail host + per-runtime
  `__faas_tail.{js,py}` shim for the handler subprocess
- `guest/runners/internal/runnerparity/parity.go` — `TailScript` field on
  `FakeHandler` + JSON-tag pin
- `pkg/api/limits.go` — three new fields + accessor methods + per-plan
  matrix + global constants (`TailCapMax`, `TailTimeoutFloorSeconds`,
  `ParkTailDrainTimeoutSeconds`)
- `pkg/api/limits_test.go` — `TestPlanTail` pin
- `pkg/state/store.go` — `Instance.TailCount` field + `BumpInstanceTailCount` /
  `DecrementInstanceTailCount` / `GetInstanceTailCount` methods on `Store` +
  extended `AppendUsage` signature with `tailSeconds int64`
- `pkg/state/pgstore.go` — implementations
- `pkg/state/memstore.go` — implementations
- `pkg/fcvm/manager.go` — `Manager.MarkInstanceTailTerminal` helper
  (parallel to `MarkInstanceFrameworkReady`); `SendStatelessAdvisory`
  reused with new kind `"tail_completed"` / `"tail_failed"`
- `pkg/sched/engine.go` — `snapshotAndPark` watchdog + audit row
- `pkg/sched/reaper.go` — `TailCount > 0` early-out in
  `ReapIdle` / `ReapAggressive` / `SelectEvictions` + `TailCount int` on
  `InstanceInfo`
- `pkg/sched/loop.go` — thread `TailCount` from `state.Instance` to
  `InstanceInfo`
- `pkg/sched/invariants_property_test.go` — new property test
  `TestInvariant_TailCountNeverBlocksParkAfterDrain`
- `pkg/meter/sampler.go` — `RolledRow.TailSeconds` + `SampleAndRoll` wiring
- `pkg/meter/rollup.go` — `usage_daily.tail_seconds` additive merge
- `pkg/meter/pusher_test.go` — new permanent guard test
  `TestPushHour_ExcludesTailSeconds`
- `pkg/wire/metrics.go` — three new fields + pre-instantiated labels +
  accessors
- `docs/STATUS.md` — M7.5 entry
- `docs/faas_implementation_spec.md` — §4.7.1 sub-section

## Rejected alternatives

- **HTTP response trailer.** Breaks the streaming adapter (which already
  uses trailers for streaming metadata). Requires `text/event-stream`
  only. The issue explicitly forbids rewrites of the response.
- **New vmmd gRPC RPC.** Out of scope; the existing DGRAM port 1027
  lead-byte discriminator extension is sufficient and matches the
  existing stateless-advisory fan-in (PR-C).
- **In-memory goroutine-per-wake on schedd.** Breaks the Tier A
  single-host reaper invariant (no goroutine per wake; per-wake state
  is a value bundle carried across the unlocked Phase 3 boot RPC, per
  ADR-062). The runner's in-process tail host avoids this.
- **Persistent tail queue (cross-wake durability).** Tail tasks live and
  die with the wake. No persistent tail queue. The build queue
  (PR #136 / #154) is the durable pattern; adopting it for tail is a
  separate ADR (the issue's "STATELESS-issue #2").
- **Per-customer tail budget / quota enforcement.** Per-instance cap
  only. Future PR.
- **Inverted billing.** A future change that pushes `tail_seconds` onto
  Stripe/Paddle would break the financial model. The permanent
  `TestPushHour_ExcludesTailSeconds` guard test in
  `pkg/meter/pusher_test.go` is the load-bearing piece that pins
  "informational only" forever — a follow-up ADR would have to remove
  it.

## Verification

Local dev loop (no KVM needed for unit tests):

```bash
make test PKG=./guest/init/...
make test PKG=./guest/runners/...
make test PKG=./pkg/sched/...
make test PKG=./pkg/meter/...
make test PKG=./pkg/state/...
make test PKG=./pkg/api/...      # pins the new TailTimeoutS / TailCapMax / ConcurrentTailsPerInstance matrix
make test PKG=./pkg/wire/...     # pins the metric cardinality bounds
make test PKG=./migrations/...   # 00151_wait_until_tail_test.go
make lint
```

Metal acceptance (Lima nested KVM on M3+ Mac):

```bash
make metal-lima RUN_ARGS='-run TestMetal_TailEndToEnd'
make metal-lima RUN_ARGS='-run TestMetalTail_KeepsWakeRunning'
make leakcheck
```

Reference-node sign-off (per CLAUDE.md, necessary not sufficient):

```bash
make test-metal
```

## Amendment (issue #667 follow-up, PR feat/issue-667-followup)

The original PR shipped the envelope shape (§"Wire runner → handler")
and the host-side receipt handler (`pkg/fcvm.MarkInstanceTailTerminal`)
but **deferred the runner-side tail host** to this follow-up. The
deferral was load-bearing: the agent audit confirmed that none of the
five runner `main.go` files (`guest/runners/{go124,node22,node24,python312,python313}/main.go`)
actually invoked a tail host on the response path. ADR-078 §"Wire
runner → handler" described the primitive; the primitive was not
implemented. This amendment documents the wire shape that **actually
runs end-to-end** and the three new acceptance-checkboxes (#6/#7/#8)
that close the issue.

### Wire shape that runs end-to-end

The runner process (the long-lived HTTP host inside the VM, parent
of the handler subprocess) cannot reach vsock directly — it is
unprivileged inside the VM. The path is:

```
handler subprocess
  │  writes a JSONL line per waitUntil(registration) to envelope.TailPipePath
  ▼
runner process (sync.WaitGroup of per-task goroutines, each wrapped in
                context.WithTimeout(env.WaitUntilSec * time.Second))
  │  on terminal (completed / failed / timeout / panicked): writes
  │  one line "outcome_byte elapsed_ms\n" to /run/guest-init/tail-events.sock
  ▼
guest-init tail_events_proxy_linux.go (unix-domain stream, mode 0o660)
  │  frames the 16-byte 0x04 DGRAM and ships it via
  │  unix.SendmsgN to VMADDR_CID_HOST:VsockFrameworkReadyPort (1027)
  ▼
vmmd framework_ready_recv.go::dispatchTailEvent (already shipped)
  │  → Manager.MarkInstanceTailTerminal
  │  → pkg/state.Store.DecrementInstanceTailCount
  │  → TailSecondsAccumulator (informational)
  ▼
schedd reaper / snapshotAndPark reflect the new tail_count
```

Key choice: a **separate unix-domain proxy** at
`/run/guest-init/tail-events.sock` (mode 0o660). ADR-078 §"Wire runner
→ guest-init" originally proposed extending the existing
`framework_ready_proxy_linux.go` multiplexer; in practice this
follow-up moved the tail-event proxy to its own file
(`guest/init/tail_events_proxy_linux.go`) for the same reason the
sidecar-events proxy lives in its own file: a unix-domain DGRAM socket
per payload type keeps the dispatch shape (one parse per file) and
the dial path (one AF_UNIX SOCK_DGRAM per runner) symmetrical. The
framework_ready multiplexer's `0x04` branch was kept as a *forwarding*
target inside the existing socket — the runner emits to the new
proxy, which then internally re-emits to the existing 0x04 branch.
Wire contract from the runner's perspective is unchanged.

### Behavior change: framework_ready timing

ADR-078 §"Wire runner → handler" stated that the response envelope is
written to stdout once the tail drains OR times out, and that
`signal.SignalReady(...)` continues to fire on the first non-5xx
response. **Corrected by this amendment:** `signal.SignalReady(...)`
fires **after** the tail host's `Drain()` returns (the WaitGroup
call), not on the first non-5xx response. This is the change that
makes the reaper gate (ADR-078 §"Reaper gate") load-bearing: a wake
with active tails must not signal ready until the tails have
drained, otherwise the gateway would consider the wake eligible for
the next request before the tail host is done. The 5 s watchdog in
`snapshotAndPark` (`ParkTailDrainTimeoutSeconds`) is the hard ceiling
— if the tail host hangs for any reason, the park gate fires
`tail_failed{reason=forced_at_park}` and force-parks the wake.

The non-tail path (no `waitUntil` calls) is unchanged: `Drain()`
returns immediately on an empty pipe, the response runs through.

### Runner-side tail host (`guest/runners/internal/tail_host.go`)

Exported `TailHost` type. Constructor `NewTailHost(runtime, pipePath, waitUntilSec, tailCapMax int)` returns a `*TailHost`. Public surface:

- `RegisterCount() int` — number of registered tasks (capped at `TailCapMax`).
- `Failures() []string` — `"timeout:task-N"` / `"error:task-N"` reasons captured during the drain (appended to `response.TailErrors`).
- `Register(name string) error` — reads one JSONL line from the pipe, spawns a goroutine under `context.WithTimeout`, captures the terminal outcome. Returns `ErrTailCapReached` if the cap is hit.
- `Drain() int` — blocks until every spawned goroutine reaches a terminal state. Returns the number of tasks that completed within the budget.

Wire emit to the tail-events proxy uses the same shape as the existing
framework_ready proxy: 250 ms dial + 250 ms write deadlines, errors
logged at `slog.Warn` (no propagation to the handler response). The
runner process never blocks the handler response on a slow proxy
write.

### Reused primitives

| Need | Existing primitive | File |
|---|---|---|
| Fire-and-forget one-shot signal from runner to guest-init | `internal.RunnerSignal.SignalReady()` | `guest/runners/internal/framework_ready.go` |
| Unix-domain proxy to bridge runner → vsock | the new `tail_events_proxy_linux.go` (mirrors `framework_ready_proxy_linux.go`) | `guest/init/tail_events_proxy_linux.go` |
| 16-byte 0x04 DGRAM encode | `sendTail()` | `guest/init/sidecar_events_proxy_linux.go` |
| Host-side 0x04 DGRAM dispatch | `dispatchTailEvent` | `cmd/vmmd/framework_ready_recv.go` |
| `TailOutcome` enum | `pkg/fcvm.TailOutcomeCompleted/Failed/Timeout` | `pkg/fcvm/manager.go` |
| `TailCapMax` structural cap | `pkg/api.Limits.TailCapMax` | `pkg/api/limits.go` |
| Per-plan `WaitUntilSec` ceiling | `Plan.TailTimeoutSeconds()` | `pkg/api/limits.go` |
| Per-instance tail accumulator | `Manager.ReadAndResetTailSeconds` | `pkg/fcvm/manager.go` |
| 5 s park watchdog | `engine.snapshotAndPark` | `pkg/sched/engine.go` |

### Acceptance-checkboxes #6, #7, #8 (this follow-up)

- **#6 — pathological tail killed at the ceiling.** Exercised by
  `guest/runners/internal/tail_host_test.go::TestTailHost_PathologicalTailKilledAtCeiling`
  (6 unit tests pinning the TailHost surface) and the per-runtime
  `TestHandle_WaitUntilEnvelopeRoundTrip` family. The load-bearing
  assertion: a handler that registers a 10 s tail and immediately
  returns is killed at the 1 s `WaitUntilSec` ceiling, and the
  runner's `response.TailErrors` carries a `"timeout:task-N"` entry
  — proving the per-task `context.WithTimeout` is the thing that
  fires, not the 30 s handler-subprocess timeout.
- **#7 — cross-runtime parity.** Pinned by
  `guest/runners/internal/runnerparity/tail_host_pin_test.go::TestParity_AllRuntimesHonorWaitUntil`
  (file-walk: every `guest/runners/<runtime>/main.go` reads
  `FAAS_TAIL_WAIT_SEC` + `FAAS_TAIL_PIPE_PATH` and calls
  `drainTailHost`; every `tail_host_integration.go` imports
  `internal.NewTailHost` + `internal.ReadPipe`). Plus 5 per-runtime
  `TestHandle_WaitUntilEnvelopeRoundTrip` tests driving a fake
  handler that writes JSONL to the pipe.
- **#8 — metal end-to-end.** Pinned by
  `pkg/fcvm/tail_metal_linux_test.go` (file renamed from
  `tail_metal_test.go` to scope the `unix.SOCK_CLOEXEC` /
  `unix.AF_VSOCK` symbols to Linux). The test boots a real
  Firecracker microVM, fires a REAL 0x04 DGRAM via vsock from the
  test process to `VMADDR_CID_HOST:1027`, and asserts the wire
  encoding (16 bytes, type=0x04, outcome, 6 reserved zeros, 8
  elapsed_ms BE uint64) is exactly what
  `cmd/vmmd::dispatchTailEvent` parses. The host-side stamper +
  `ReadAndResetTailSeconds` paths are exercised by the sibling
  `TestMetal_TailEndToEnd` (already shipped in the original PR).

### Files added in this follow-up

- `guest/runners/internal/tail_host.go` — TailHost type, Drain, WaitGroup
- `guest/runners/internal/tail_host_test.go` — 6 unit tests (checkbox #6 core)
- `guest/runners/internal/runnerparity/tail_host_pin_test.go` — file-walk parity test (checkbox #7)
- `guest/runners/{go124,node22,node24,python312,python313}/tail_host_integration.go` — 5 per-runtime drainTailHost helpers
- `guest/init/tail_events_proxy_linux.go` — unix-domain proxy bridging runner → vsock
- `pkg/fcvm/tail_metal_linux_test.go::TestMetal_TailFullEndToEnd` — checkbox #8 wire-format gate

### Out of scope (kept here to prevent drift)

- Async / durable task queue — explicit per issue #667 ("separate issue").
- Thin durable-execution wrapper over crons — explicit per issue.
- Persistent storage — explicit per issue (rejected — breaks the niche).
- Custom guest-init knobs beyond `waitUntil` — explicit per issue.
- Per-customer tail budget / quota enforcement — explicit per issue ("deferred to a future PR"). The per-instance cap is the only ceiling today.
