# ADR-080 · Per-app async task queue (issue #668)

- **Status:** proposed
- **Superseded (in part, PR-E):** prose referred to the monolithic
  `cmd/gatewayd/` daemon split by ADR-070 into `gatewayd-public` (TLS-only
  edge) and `gatewayd-internal` (routing + wake + proxy). Body is preserved
  verbatim; readers should substitute "gatewayd-internal" for the
  routing/wake/proxy path and "gatewayd-public" for the certmagic/TLS path.
  `cmd/gatewayd/<file>.go` citations in this body are stale; see PR-E for
  the new file locations.
- **Date:** 2026-08-06
- **Closes:** #668
- **Decision:** Add a per-app durable task queue. `POST /v1/apps/{slug}/tasks`
  enqueues a JSONB payload + optional `delay_seconds` + `idempotency_key`,
  the platform schedules a synthetic wake via the existing cron path,
  and delivers the payload to the customer's `apps.task_handler_path`
  (default `/_tasks/<task_id>`) with an internal Bearer token the
  customer's handler validates via a new `pkg/auth.RequireTaskToken`
  middleware. Tasks are stored in Postgres (`public.tasks` table, FIFO
  via `enqueued_at`), retry with exponential backoff on 5xx up to
  `TaskMaxAttemptsPerPlan` (Hobby 3 / Pro 5 / Scale 10; Free 0), and
  land in a `dead_letter` status after exhaustion. Queue depth is
  plan-capped (Free 0 / Hobby 100 / Pro 1,000 / Scale 10,000); body
  size is capped at 256 KB. Free plan is disabled.
- **Why:** Every real app has "I just want a background job" — the
  email send, the webhook callback, the analytics flush. Today the
  customer must run their own queue (Upstash QStash, SQS) or
  long-poll inside a wake. The first poll cost a wake on every cron
  tick; the second burns the wake's idle budget. Gregale has the
  control plane (Postgres + pg_notify) and the synthetic-request
  plumbing (cron → schedd → gatewayd) already — a per-app task queue
  is the natural third STATELESS primitive after `waitUntil`
  (ADR-078) and the durable-execution wrapper over crons
  (#669). It's also the most-asked-for integration request in the
  post-waitUntil customer feedback.
- **Issue:** #668

## Context

Gregale's wake model is **request-driven** today. A wake fires on:
- An inbound HTTP request (`gatewayd` → `schedd.EnsureInstance`).
- A cron-fired synthetic request (`schedd` → `GatewaySynth` RPC →
  `gatewayd-internal` → wake).
- A webhook-out fired from inside a request handler.

There is no first-class way to enqueue a unit of work that runs
later, asynchronously, without the customer managing a queue
themselves. The workarounds are documented in #668 §"Context" — they
all involve either the customer paying for a polling cron (every
empty poll is a wake) or wiring a queue on the upstream side. Both
are friction.

The proposal in #668 is roughly what **Upstash QStash** is, but
built into the platform: enqueue, durable storage, synthetic-wake
delivery, retry-with-backoff, dead-letter, dashboard visibility.

### Why now, not later

`waitUntil` (ADR-078, merged as PR #683 + #700) closed the
fire-and-forget-after-response gap. The async-task queue closes the
**durable-across-park** gap — work that must run even if the wake
parks before the promise resolves. They are complementary primitives:
`waitUntil` is "stay alive long enough for these promises to
resolve"; the task queue is "schedule this work even if the wake is
already parked or has never woken". The same primitive set that
Cloudflare / Vercel / AWS Lambda all ship.

### Non-goals (explicit per #668 §"Out of scope")

- Cross-app task routing (single-app queue only).
- Persistent state across wakes (rejected — breaks the niche).
- Per-task timeout (a single wake serves the task; if the wake
  reaper parks mid-task, the task is retried on the next wake).
- Replacing the durable-execution wrapper over crons (#669) — that
  is the workflow primitive; the task queue is the single-job
  primitive.

## Decisions

### 1. The queue lives in Postgres

A new `public.tasks` table:

```sql
CREATE TABLE public.tasks (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    app_id          bigint NOT NULL REFERENCES public.apps(id) ON DELETE CASCADE,
    idempotency_key text,
    payload         jsonb NOT NULL,
    sealed          boolean NOT NULL DEFAULT false,
    payload_cipher  bytea,                       -- when sealed=true; mutually exclusive with payload
    attempt         int NOT NULL DEFAULT 0,
    max_attempts    int NOT NULL,
    status          text NOT NULL DEFAULT 'queued'
                    CHECK (status IN ('queued','running','succeeded','failed','dead')),
    enqueued_at     timestamptz NOT NULL DEFAULT now(),
    scheduled_for   timestamptz NOT NULL DEFAULT now(),
    last_attempt_at timestamptz,
    last_error      text,
    audit_id        bigint REFERENCES public.audit_events(id),
    CHECK (NOT (sealed AND payload IS NOT NULL))                 -- exactly one of payload / payload_cipher
);
-- 24 h idempotency window: partial unique index, plus a TTL reaper
-- that nulls idempotency_key once the row is older than
-- TaskIdempotencyTTL. The partial index is the load-bearing piece —
-- a bare UNIQUE constraint would forbid re-enqueueing the same key
-- across an app's lifetime (defeating the "24 h window" contract).
CREATE UNIQUE INDEX tasks_idempotency_key_uniq_idx
    ON public.tasks (app_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
CREATE INDEX tasks_queue_ready_idx
    ON public.tasks (app_id, scheduled_for)
    WHERE status = 'queued';
```

The TTL reaper runs hourly in `pkg/retention/tasks_gc.go` (same
package as the existing 30-day retention reaper mentioned in
Risk #2): `UPDATE public.tasks SET idempotency_key = NULL WHERE
idempotency_key IS NOT NULL AND enqueued_at < now() - interval
'24 hours'`.

Why Postgres, not a new store (Redis Streams, NATS, Kafka):

- The control plane already runs Postgres with `pg_notify`; adding
  Redis Streams means a new deployment unit, a new backup story,
  a new failure mode. The financial model does not budget for a
  second control-plane datastore.
- FIFO is enforced by `enqueued_at` (the partial index
  `tasks_queue_ready_idx` filters `status='queued'` rows only; the
  schedd tick does `SELECT ... WHERE status='queued' AND
  scheduled_for <= now() ORDER BY enqueued_at LIMIT N FOR UPDATE
  SKIP LOCKED`, the same pattern the cron sweep uses in
  `pkg/sched/loop.go::runCronTick`).
- The 256 KB body cap means a single row stays under Postgres's
  TOAST threshold for the common case; large rows TOAST transparently.

### 2. Delivery is a synthetic wake through gatewayd

`schedd` grows `pkg/sched/dispatch_tasks.go::runTasksTick`, a
sibling to `runCronTick` (line 1515 of `pkg/sched/loop.go`) that
on `LISTEN tasks_queued` polls the per-app queue and calls
`schedd.EnsureInstance`, then hands the payload to
`gatewayd-internal` via the same `GatewaySynth` RPC interface the
cron path uses (`pkg/sched/loop.go:1302` — the cron path crosses
the schedd→gatewayd boundary through this RPC; reusing it is the
whole point of §Decision 2).

The synthetic request carries an internal Bearer token
(`Bearer faas_task_<signed>`) signed with the platform's private
X25519 half — see §Decision 6 for the full token shape and the
**server-side single-use lock** that prevents replay.

The path is `apps.task_handler_path` (default `/_tasks/<task_id>`,
configurable, ≤ 256 chars). `gatewayd-internal` reserves
`/_tasks/...` (alongside the §4.1.1 reservation list) and routes
to the customer's configured `apps.task_handler_path` with a
synthetic internal Bearer prepended.

Why this reuses the cron path: the cron path already exercises the
full wake lifecycle (cold-boot / hot-wake, rate-limit gates,
framework_ready DGRAM, snapshot_and_park). The task delivery
should not bypass any of those gates — if a wake is over its
concurrency cap, the task waits; if the wake is parked, the task
cold-boots.

### 3. Plan-bound caps, single source of truth

```go
// pkg/api/limits.go
const (
    TaskQueueDepthPerPlan     = [4]int{0, 100, 1000, 10000}     // Free 0 / Hobby / Pro / Scale
    TaskMaxAttemptsPerPlan    = [4]int{0, 3, 5, 10}
    TaskBodyMaxBytes          = 256 * 1024
    TaskIdempotencyTTL        = 24 * time.Hour
    TaskBackoffBaseSeconds    = 5                                // attempt N backoff = base * 2^(N-1)
    TaskBackoffMaxSeconds     = 5 * 60
    TaskDeliveryDeadlineSec   = [4]int{0, 30, 60, 120}           // per-plan delivery-wake timeout (matches #667 waitUntil pattern)
)
```

The enforcer per cap (single source of truth):

- `TaskQueueDepthPerPlan` → `apid` at insert (returns 429
  `task_queue_full`).
- `TaskBodyMaxBytes` → `apid` at insert (returns 413
  `task_body_too_large`).
- `idempotency_key` partial unique index → `apid` returns 200 with
  the original `task_id` on UNIQUE-violation (the row already
  exists). The TTL reaper (§Decision 1) clears the key after 24 h.
- `TaskMaxAttemptsPerPlan` → `schedd` at retry; after exhaustion,
  status flips to `dead` and a `task.dead_letter` audit row is
  written.
- `TaskDeliveryDeadlineSec` → `gatewayd-internal` on the outbound
  synthetic-wake HTTP hop. `gatewayd-internal` sets a per-request
  `context.WithDeadline(planDeadline)` on the outbound HTTP to the
  guest's `apps.task_handler_path`; a deadline expiry is converted
  to the same 5xx-equivalent as a customer-supplied 5xx (retry with
  backoff). This is distinct from — and complementary to — the
  global 5 s `snapshot_and_park` watchdog on the wake, which fires
  if the handler hangs past it.

Why a per-plan delivery deadline (vs. the global 5 s
`snapshot_and_park` watchdog): the customer's `task_handler_path`
handler may be doing real work (the task IS the work). The deadline
here is per-plan, not the global 5 s watchdog, because the task's
wake is allowed to run longer than a request-driven wake. The 5 s
watchdog still fires if the handler hangs past it — `gatewayd`'s
per-request deadline is a *softer* timer that produces a retry, not
a force-park.

### 4. State machine for `public.tasks.status`

Five states, eight transitions. The implementation must encode
this exactly:

| From       | Trigger                                    | To          | Side effects                                                                                          |
|------------|--------------------------------------------|-------------|--------------------------------------------------------------------------------------------------------|
| (insert)   | `apid` POST `/_tasks`                      | `queued`    | `enqueued_at=now()`, `scheduled_for=now()` (or `now()+delay_seconds`), `pg_notify('tasks_queued', …)` |
| `queued`   | `schedd` claims row (`FOR UPDATE SKIP LOCKED`) | `running`   | `attempt++`, `last_attempt_at=now()`, `audit.task.delivered` row                                      |
| `running`  | handler returns 2xx                         | `succeeded` | `audit.task.succeeded` row                                                                             |
| `running`  | handler returns 5xx / timeout / OOM / 3xx  | `queued`    | `scheduled_for = now() + min(base * 2^(attempt-1), max)` (per-plan backoff); `last_error` populated   |
| `running`  | handler returns 4xx                         | `failed`    | `audit.task.failed` row; no retry                                                                      |
| `running`  | `schedd` reaper: `attempt >= max_attempts` | `dead`      | `audit.task.dead_letter` row; visible in dashboard                                                    |
| `failed`   | customer `POST /_tasks/{id}/requeue`       | `queued`    | `attempt=0`, `scheduled_for=now()`                                                                    |
| `dead`     | customer `POST /_tasks/{id}/requeue`       | `queued`    | `attempt=0`, `scheduled_for=now()`, `last_error` cleared                                              |

The 4xx→`failed` (no retry) decision: a 4xx is the customer's
handler saying "this payload is bad". Retrying would produce the
same 4xx. The customer opts in to retry semantics by returning 5xx.
This matches the §4.7 cron-failure conventions in the spec.

The `running` row is the single-use lock that prevents replay
(combined with §Decision 6's server-side check).

### 5. Sealed payloads reuse ADR-020; unseal-on-delivery placement is `schedd`

When `sealed: true` is set on enqueue, `apid` server-side seals
the payload with the same X25519 + ChaCha20-Poly1305 secretbox
pattern ADR-020 established for `AppSecret`. `apid` writes
`payload_cipher` (not `payload`); on delivery, the
`schedd`-side delivery path **unseals** the payload **before** the
synthetic-wake HTTP hop, then sends plaintext JSON to the guest's
`apps.task_handler_path`. The TLS-on-edge between
`gatewayd-public` and the customer's TLS terminator (already
deployed) protects the in-flight payload; the seal is at-rest only.

#### Host-key placement (the ADR-020 reuse question)

ADR-020 generates the host key in `vmmd` (§D1 of ADR-020). That
placement is correct for `AppSecret` (the *unseal-at-wake* surface
is vmmd's drive1 staging). It is **wrong** for task payload seal,
which has to unseal at request rate (one unseal per `schedd` task
dispatch) inside a process that is not vmmd. Resolution:

- **Public half** (recipient, mode 0444) — loaded by `apid` at
  startup, identical to the existing `AppSecret` seal flow.
  `apid` is the trust boundary for enqueue-time sealing.
- **Private half** (mode 0400, `/etc/faas/secrets/host.age`) —
  loaded by `schedd` at startup, alongside vmmd's load. `schedd`
  unseals on the dispatch hop. The plaintext is held in
  `schedd`'s process memory for the lifetime of one synthetic-wake
  HTTP call (≤ `TaskDeliveryDeadlineSec` per plan); it is never
  logged, never written outside the GPU-process heap, never sent
  to disk.
- **Why not vmmd**: ADR-020's vmmd placement is for *boot-time*
  unseal of `AppSecret`. Task unsealing is *request-time*, in a
  different process (schedd). The key is the same X25519 identity;
  the process that holds it for unseal is `schedd`, not `vmmd`.
  This does not change CLAUDE.md's "vmmd is the only root
  component" rule — `schedd` already runs as root in the control
  plane.

This adds `schedd.hostIdentity` (the X25519 private identity) as a
new field on the existing `Loop` struct, mirroring
`vmmd.Manager.hostIdentity`. The two processes read the same
`/etc/faas/secrets/host.age` file; rotation under ADR-020 future
work (multi-recipient seal) propagates to both without changes
here.

Why seal at all: customers storing API keys, PII, or auth tokens
inside task payloads want the same `AppSecret`-grade guarantee that
the bytes in Postgres are not readable without the platform key.
Mirrors the existing secret story; no new crypto primitive.

### 6. Synthetic-wake auth: internal Bearer, single-use server-side lock

The synthetic-wake Bearer is `Bearer faas_task_<base64(X25519-signed
{app_id, task_id, attempt, exp})>`, signed with `schedd`'s
private half. The token expires after the configured delivery
window (default 5 minutes, bound by `TaskDeliveryDeadlineSec`).

The replay-protection load-bearing check is **server-side** —
NOT the customer's handler:

- `schedd::runTasksTick::claim()` (the row state transition
  `queued`→`running`) writes `last_attempt_at=now()` under the
  same `FOR UPDATE SKIP LOCKED` row lock that selects the row.
- `gatewayd-internal` validates the Bearer signature (X25519
  verify, `exp` check). If valid, it pins the request to the
  customer handler with the matching `X-Faas-Task-Id` header.
  The customer's `pkg/auth.RequireTaskToken` middleware is
  **advisory** — it can verify the signature and reject obvious
  forgeries, but it cannot be the load-bearing replay guard.
- A replayed token after the task row's status has flipped away
  from `running` (e.g. customer returned 5xx, row returned to
  `queued` for retry) is rejected by `gatewayd-internal`
  **before** it reaches the guest: the gateway holds the only
  authoritative view of the row's status and serves a 401
  on `attempt` mismatch.

The bearer is **NOT** the customer's `Authorization: Bearer
fp_live_...` API key. The task payload originates from the
customer's own handler (or their CLI/API call); the internal
Bearer is the platform asserting "this is a task delivery, not a
user request". The customer's middleware is recommended but
optional — task delivery works even if the customer's handler
ignores the Bearer entirely (the platform's replay check still
holds).

### 7. Free plan disabled

Free plan returns 402 `tasks_not_allowed` on `POST /v1/apps/{slug}/tasks`.
Aligns with the cron limit shape (`cron_limit_per_app` per plan) and
the `waitUntil` Free 5 s ceiling — Free plan is the "does this fit
in a single synchronous request?" tier.

## Consequences

### Files added (anchors)

- `migrations/00NN_tasks.sql` (goose, append-only) — `public.tasks`
  table + `tasks_idempotency_key_uniq_idx` partial unique + `tasks_queue_ready_idx`.
- `docs/adr/080-per-app-async-task-queue.md` — this ADR.
- `pkg/api/limits.go` — `TaskQueueDepthPerPlan`, `TaskMaxAttemptsPerPlan`,
  `TaskBodyMaxBytes`, `TaskIdempotencyTTL`, `TaskBackoffBaseSeconds`,
  `TaskBackoffMaxSeconds`, `TaskDeliveryDeadlineSec`.
- `pkg/state/tasks.go` — sqlc-generated CRUD on `public.tasks`
  (Enqueue, Claim, MarkRunning, MarkSucceeded, MarkFailedRetry,
  MarkFailed, MarkDead, Requeue, IdempotencyFind).
- `pkg/auth/task_token.go` — `RequireTaskToken` advisory middleware +
  the X25519-signed Bearer encoder/decoder.
- `pkg/sched/hostkey.go` — `schedd` host identity loader (mirrors
  `pkg/secretbox/hostkey.go` from ADR-020).
- `pkg/sched/dispatch_tasks.go` — `runTasksTick`, `LISTEN
  tasks_queued`, the per-app queue sweep, the unseal-on-claim
  step (calls into the host identity loaded by `hostkey.go`).
- `pkg/sched/loop.go` — registers `runTasksTick` alongside
  `runCronTick`.
- `pkg/gatewaydinternal/task_route.go` — `/_tasks/<id>` reserved
  path, routes to `apps.task_handler_path`, **server-side replay
  check** (locks against `public.tasks.status='running'`).
- `pkg/retention/tasks_idempotency_gc.go` — hourly reaper that
  nulls `idempotency_key` after 24 h.
- `cmd/apid/handlers/tasks.go` — `POST /v1/apps/{slug}/tasks`,
  `POST /v1/apps/{slug}/tasks/{id}/requeue`, `GET
  /v1/apps/{slug}/tasks`, `GET /v1/apps/{slug}/tasks/{id}`.
- `pkg/events/task.go` — event kinds `task.enqueued`,
  `task.delivered`, `task.succeeded`, `task.failed`,
  `task.dead_letter`.
- `pkg/wire/metrics/tasks.go` — `tasks_enqueued_total{app,plan}`,
  `tasks_delivered_total{app,plan,outcome}`,
  `tasks_retry_total{app,plan,attempt}`,
  `task_delivery_seconds{app,plan}` histogram,
  `tasks_dead_letter_total{app,plan}`,
  `tasks_queue_depth{app,plan,status}` gauge.
- `sdk/go/client.go` — `EnqueueTask`, `ListTasks`, `GetTask`,
  `RequeueTask`.
- `cmd/faas/cmd/tasks.go` — `faas tasks enqueue / list / get /
  requeue` (mirrors the `faas crons` shape).
- `pkg/e2e/tasks_e2e_test.go` — full lifecycle (enqueue, deliver,
  succeed, retry, dead-letter, requeue, replay-rejection).
- `pkg/api/spec/openapi.yaml` — `Task` schema, four endpoints.
- `pkg/api/client.go` — typed Go client.
- `docs/STATUS.md` M7.x entry — pins the e2e test to the wire
  surface.

### Files modified

- `pkg/api/limits.go` — add the 5 plan-bound caps.
- `pkg/sched/loop.go::Loop` struct — add `hostIdentity
  *secretbox.Identity` field; `WithHostKey(...)` setter.
- `cmd/schedd/main.go` — load host key on startup alongside the
  existing PG and `GatewaySynth` setup.
- `cmd/gatewayd-internal/main.go` — wire the `/_tasks/<id>`
  reservation list, the server-side replay check, and the
  per-plan delivery deadline.
- `pkg/wire/metrics/metrics.go` — register the 5 new counter /
  histogram families.
- `docs/faas_implementation_spec.md` §4.7.3 — new paragraph on
  the per-app task queue.

### Operational signals

- `tasks_queue_depth{app,plan,status}` gauge — operator-facing,
  same shape as `pkg/wire/metrics.instances_gauge`.
- `task_dead_letter_total{app,plan}` — alert at > 0 in 5 min
  window (a customer whose handler is misbehaving and 4xx-ing
  every task).
- `task_delivery_seconds` p99 — the **primary SLO metric** for
  the queue, target p99 < 1 s for idle-instance delivery,
  < 5 s for cold-boot delivery.
- `task_replay_rejected_total{app,plan}` — operational signal
  for any token replay attempt; alert at > 0 in any 5 min window
  (indicates a leaked token or a bug in schedd's row pinning).

### Acceptance (mirrors #668 §"Acceptance")

- [ ] `POST /v1/apps/{slug}/tasks` returns `{task_id, status: "queued", scheduled_for}`
      within 100 ms p50 (warm Postgres).
- [ ] `schedd` delivers a queued task within 1 s of enqueue (idle
      instance) or via the existing cold-boot wake path.
- [ ] 2xx marks `succeeded`; 5xx retries with exponential backoff up
      to `TaskMaxAttemptsPerPlan`; 4xx marks `failed`.
- [ ] `task_dead_letter_total` increments on exhaustion.
- [ ] Per-app queue-depth cap returns 429 `task_queue_full`.
- [ ] `idempotency_key` deduplicates within 24 h (partial unique
      index + hourly reaper, both tested).
- [ ] Free plan returns 402 `tasks_not_allowed`.
- [ ] Body > 256 KB returns 413 `task_body_too_large`.
- [ ] `cmd/faas tasks enqueue / list / get / requeue` works
      end-to-end.
- [ ] `pkg/e2e/tasks_e2e_test.go` exercises the full lifecycle.
- [ ] Dashboard renders `task.dead_letter` rows.
- [ ] Sealed payloads round-trip through Postgres with
      `payload_cipher` non-null and `payload` null.
- [ ] `task_replay_rejected_total` increments when a Bearer is
      replayed after the row's status has flipped away from
      `running`.
- [ ] Customer's `task_handler_path` handler can be a stub that
      ignores the Bearer entirely — task delivery still works
      (proves the server-side replay check is load-bearing, not
      the customer's middleware).
- [ ] `pkg/retention/tasks_idempotency_gc_test.go` — reaper nulls
      `idempotency_key` for rows older than `TaskIdempotencyTTL`,
      leaves younger rows untouched.
- [ ] State machine (`§Decision 4`) — all 8 transitions covered by
      unit tests on `pkg/state/tasks.go`.

### Risk register

1. **Cold-boot wake-storm on enqueue burst** (re-ordered: this
   is the biggest operational risk). A Scale customer enqueuing
   10,000 tasks on a parked app → 10,000 cold-boots. **Today the
   codebase has no per-app wake rate-limit primitive** (a
   `pkg/sched/rate_limit.go::EnsureInstanceRateLimit` does not
   exist — see `pkg/sched/*.go` for the actual file list). This
   ADR **depends on a new primitive** the schedd-side task tick
   requires. Two acceptable resolutions:
   - (a) **Add the primitive in a small pre-PR**:
     `pkg/sched/rate_limit.go::EnsureInstanceRateLimit(appID,
     burstTo int)` — a token-bucket-per-app sliding-window with a
     per-plan burst ceiling (Free 1 / Hobby 5 / Pro 20 / Scale
     100 wakes/min). The task tick issues one wake per task and
     respects the ceiling; tasks beyond the ceiling stay in
     `queued` and the `tasks_queue_depth{status="queued"}` gauge
     surfaces the backlog.
   - (b) **Cap task dispatches per cron tick** as a coarse
     workaround (e.g. ≤ 100 dispatches per 1 s tick) and defer
     the per-app rate-limiter to a follow-up ADR. Less clean but
     ships faster.
   The implementation PR chain must pin one of these in PR #1
   (migration / limits). Without it, a single enqueue burst can
   OOM the control-plane box.

2. **Schedd-tick contention with the cron tick.** Both sweeps
   compete for the same `schedd` goroutine + the same `EnsureInstance`
   path. Mitigation: the cron tick already does `FOR UPDATE SKIP LOCKED`
   to avoid contention; the task tick uses the same primitive on
   a different table, no shared row locks. The schedd's per-app
   fairness scheduler (`pkg/sched/fairness.go`) treats a task
   delivery as one `EnsureInstance` slot, identical to a cron.

3. **Postgres storage growth.** A 256 KB × 10,000 tasks/app on
   Scale = 2.5 GB per Scale app. Mitigation: the
   `tasks_queue_ready_idx` partial index only indexes `status='queued'`
   rows; succeeded / failed / dead rows are not in the hot path. A
   `pkg/retention/tasks_gc.go` weekly job deletes `status IN
   ('succeeded','failed','dead') AND last_attempt_at < now() -
   interval '30 days'`. Retention is tunable per-account.

4. **Internal Bearer replay.** Mitigated by **server-side lock**
   (§Decision 6): the row's `status='running'` is held under the
   same `FOR UPDATE SKIP LOCKED` row lock that selected it for
   delivery. A replay after the row's status flips away from
   `running` is rejected by `gatewayd-internal` (401). The
   customer's middleware is advisory. `exp` is short (5 min,
   bound by `TaskDeliveryDeadlineSec`); a stale-exp token is also
   rejected. `task_replay_rejected_total{app,plan}` is the
   operator-visible signal for any replay attempt — alert at > 0
   in 5 min window.

5. **ADR-020 dependency for host key.** Task unsealing shares
   the X25519 identity with `AppSecret`. If ADR-020's key-rotation
   story changes (currently manual — see ADR-020 §"Future work
   #1", multi-recipient seal), this ADR inherits the change.
   Two processes (`vmmd` for AppSecret, `schedd` for tasks) will
   both load the same file; rotation must propagate to both.

6. **`TaskDeliveryDeadlineSec` interaction with snapshot_and_park.**
   `gatewayd-internal`'s per-request deadline (30/60/120 s by plan)
   is a *softer* retry trigger than the wake's 5 s
   `snapshot_and_park` watchdog. The watchdog fires first if the
   handler genuinely hangs; both lead to a retry via the same
   `queued` transition. Documented in §Decision 3 so the
   implementation does not double-enforce.

## Out of scope (separate issues)

- `waitUntil` post-response tail — #667 (shipped via PR #683 + #700).
- Durable execution wrapper over crons — #669.
- Persistent state across wakes — rejected per #668 §"Out of scope".
- Cross-app task routing — single-app queue only.
- Per-task timeout (beyond `TaskDeliveryDeadlineSec`) — handled by
  the wake's existing reaper; not a task-queue concern.
- Webhook delivery as a task — handled by ADR-076 webhook
  delivery, which already exists and is its own primitive.
- Per-app wake rate-limit primitive — Risk #1; either lands as a
  pre-PR or as part of the migration PR.