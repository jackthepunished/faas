# ADR-080 · Per-app async task queue (issue #668)

- **Status:** proposed
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
- A cron-fired synthetic request (`schedd` → gatewayd).
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
    payload_cipher  bytea,                       -- when sealed=true
    attempt         int NOT NULL DEFAULT 0,
    max_attempts    int NOT NULL,
    status          text NOT NULL DEFAULT 'queued'
                    CHECK (status IN ('queued','running','succeeded','failed','dead')),
    enqueued_at     timestamptz NOT NULL DEFAULT now(),
    scheduled_for   timestamptz NOT NULL DEFAULT now(),
    last_attempt_at timestamptz,
    last_error      text,
    audit_id        bigint REFERENCES public.audit_events(id),
    UNIQUE (app_id, idempotency_key)             -- 24h idempotency window enforced via TTL reaper
);
CREATE INDEX tasks_queue_ready_idx
    ON public.tasks (app_id, scheduled_for)
    WHERE status = 'queued';
```

Why Postgres, not a new store (Redis Streams, NATS, Kafka):

- The control plane already runs Postgres with `pg_notify`; adding
  Redis Streams means a new deployment unit, a new backup story,
  a new failure mode. The financial model does not budget for a
  second control-plane datastore.
- FIFO is enforced by `enqueued_at` (the existing `tasks_queue_ready_idx`
  is a partial index on `status='queued'` rows only — the schedd
  tick does `SELECT ... WHERE status='queued' AND scheduled_for <= now()
  ORDER BY enqueued_at LIMIT N FOR UPDATE SKIP LOCKED`, the same
  pattern `schedd` uses for the cron sweep).
- The 256 KB body cap means a single row stays under Postgres's
  TOAST threshold for the common case; large rows TOAST transparently.

### 2. Delivery is a synthetic wake through gatewayd

`schedd` grows `pkg/sched/loop.go::dispatchTasksTick` that, on
`LISTEN tasks_queued`, polls the per-app queue and calls
`schedd.EnsureInstance` (the same function a cron fires), then
hands the payload to `gatewayd-internal` via the synthetic-request
path. The synthetic request carries an internal Bearer token
(`Bearer faas_task_<signed>`) signed with the vmmd-issued host
key (the same X25519 key pair ADR-020 established for `AppSecret`
sealing) — the customer's handler validates via
`pkg/auth.Middleware.RequireTaskToken`, which:

- Decodes the token's app_id + task_id + attempt + exp claims.
- Verifies the X25519 signature against the platform's published
  public half.
- Confirms `task_id` matches a `public.tasks.status='running'` row
  for the matching `app_id`.

The path is `apps.task_handler_path` (default `/_tasks/<task_id>`,
configurable, ≤ 256 chars). The platform reserves the path
alongside the §4.1.1 reservation list — `/_tasks/...` is
platform-owned, the customer's `task_handler_path` is the
landing.

Why this reuses the cron path: the cron path already exercises
the full wake lifecycle (cold-boot / hot-wake, rate-limit gates,
framework_ready DGRAM, snapshot_and_park). The task delivery
should not bypass any of those gates — if a wake is over its
concurrency cap, the task waits; if the wake is parked, the
task cold-boots.

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

`apid` enforces `TaskQueueDepthPerPlan` at insert (returns 429
`task_queue_full`), `TaskBodyMaxBytes` (returns 413), and the
`idempotency_key` UNIQUE constraint (returns 200 with the original
`task_id`). `schedd` enforces `TaskMaxAttemptsPerPlan` at retry
(after exhaustion, status flips to `dead` and a `task.dead_letter`
audit row is written). `gatewayd-internal` enforces
`TaskDeliveryDeadlineSec` on the synthetic-wake side (a task that
exceeds the deadline is the same as a 5xx response from the
customer — retry with backoff).

Why a per-plan delivery deadline: the wake's
`snapshot_and_park` watchdog is 5 s, but the customer's
`task_handler_path` handler may be doing real work (the task IS
the work). The deadline here is per-plan, not the global 5 s
snapshot_andPark watchdog, because the task's wake is allowed to
run longer than a request-driven wake. The 5 s watchdog still
fires if the handler hangs past it — the deadline is a *softer*
timer that produces a retry, not a force-park.

### 4. Retry semantics: exponential backoff on 5xx, dead on 4xx

- **2xx** → `status='succeeded'`, emit `task.succeeded` audit row.
- **5xx, timeout, OOM, or 3xx** → increment `attempt`,
  `status='queued'`, reschedule with `scheduled_for = now() +
  min(TaskBackoffBaseSeconds * 2^(attempt-1), TaskBackoffMaxSeconds)`.
- **4xx** → `status='failed'`, emit `task.failed` audit row, do NOT
  retry. The customer's handler explicitly rejected the task.
- **`attempt >= max_attempts`** → `status='dead'`, emit
  `task.dead_letter` audit row. Visible in the dashboard;
  re-enqueueable via `POST /v1/apps/{slug}/tasks/{id}/requeue`
  (resets `attempt=0`, `status='queued'`).

Why 4xx → failed, not retried: a 4xx is the customer's handler
saying "this payload is bad". Retrying would just produce the same
4xx. The customer opts in to retry semantics by returning 5xx.

### 5. Sealed payloads reuse ADR-020

When `sealed: true` is set on enqueue, the payload is sealed
server-side in `apid` with the same X25519 + ChaCha20-Poly1305
pattern ADR-020 established for `AppSecret`. `apid` writes
`payload_cipher` (not `payload`); on delivery, the schedd-side
delivery path unseals the payload, then sends the plaintext JSON
over the platform's internal synthetic-wake HTTP. The TLS-on-edge
already protects the in-flight payload; the seal is at-rest only.

Why seal at all: customers storing API keys, PII, or auth tokens
inside task payloads want the same `AppSecret`-grade guarantee that
the bytes in Postgres are not readable without the platform key.
It mirrors the existing secret story; no new crypto primitive.

### 6. Synthetic-wake auth: internal Bearer, not the customer's API key

The synthetic-wake Bearer is `Bearer faas_task_<base64(X25519-signed
{app_id, task_id, attempt, exp})>`, signed with the platform's
private half of the host key pair. The customer handler validates
via `pkg/auth.Middleware.RequireTaskToken` — NOT via the customer's
existing `Authorization: Bearer fp_live_...` API key middleware.

Why: the task payload comes from the customer's own handler (or
their own CLI / API call) — they shouldn't have to share an API
key with themselves. The internal Bearer is the platform asserting
"this is a task delivery, not a user request"; the customer
validates the signature against the platform's published
`/etc/faas/secrets/host.age.pub` (already exposed by ADR-020).

### 7. Free plan disabled

Free plan returns 402 `tasks_not_allowed` on `POST /v1/apps/{slug}/tasks`.
Aligns with the cron limit shape (`cron_limit_per_app` per plan) and
the `waitUntil` Free 5 s ceiling — Free plan is the "does this fit
in a single synchronous request?" tier.

## Consequences

### Files added (anchors)

- `migrations/00NN_tasks.sql` (goose, append-only) — `public.tasks`
  table + `tasks_queue_ready_idx`.
- `docs/adr/080-per-app-async-task-queue.md` — this ADR.
- `pkg/api/limits.go` — `TaskQueueDepthPerPlan`, `TaskMaxAttemptsPerPlan`,
  `TaskBodyMaxBytes`, `TaskIdempotencyTTL`, `TaskBackoffBaseSeconds`,
  `TaskBackoffMaxSeconds`, `TaskDeliveryDeadlineSec`.
- `pkg/state/tasks.go` — sqlc-generated CRUD on `public.tasks`
  (Enqueue, Claim, MarkRunning, MarkSucceeded, MarkFailed, MarkDead,
  Requeue).
- `pkg/auth/task_token.go` — `RequireTaskToken` middleware + the
  X25519-signed Bearer encoder/decoder.
- `pkg/sched/dispatch_tasks.go` — `dispatchTasksTick`, `LISTEN
  tasks_queued`, the per-app queue sweep.
- `pkg/gatewaydinternal/task_route.go` — `/_tasks/<id>` reserved
  path, routes to `apps.task_handler_path`.
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
  `tasks_dead_letter_total{app,plan}`.
- `sdk/go/client.go` — `EnqueueTask`, `ListTasks`, `GetTask`,
  `RequeueTask`.
- `cmd/faas/cmd/tasks.go` — `faas tasks enqueue / list / get /
  requeue` (mirrors the `faas crons` shape).
- `pkg/e2e/tasks_e2e_test.go` — full lifecycle (enqueue, deliver,
  succeed, retry, dead-letter, requeue).
- `pkg/api/spec/openapi.yaml` — `Task` schema, four endpoints.
- `pkg/api/client.go` — typed Go client.
- `docs/STATUS.md` M7.x entry — pins the e2e test to the wire
  surface.

### Files modified

- `pkg/api/limits.go` — add the 5 plan-bound caps.
- `pkg/sched/loop.go` — register `dispatchTasksTick` alongside the
  cron tick.
- `cmd/gatewayd-internal/main.go` — wire the `/_tasks/<id>`
  reservation list.
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

### Acceptance (mirrors #668 §"Acceptance")

- [ ] `POST /v1/apps/{slug}/tasks` returns `{task_id, status: "queued", scheduled_for}`
      within 100 ms p50 (warm Postgres).
- [ ] `schedd` delivers a queued task within 1 s of enqueue (idle
      instance) or via the existing cold-boot wake path.
- [ ] 2xx marks `succeeded`; 5xx retries with exponential backoff up
      to `TaskMaxAttemptsPerPlan`; 4xx marks `failed`.
- [ ] `task_dead_letter_total` increments on exhaustion.
- [ ] Per-app queue-depth cap returns 429 `task_queue_full`.
- [ ] `idempotency_key` deduplicates within 24 h.
- [ ] Free plan returns 402 `tasks_not_allowed`.
- [ ] Body > 256 KB returns 413 `task_body_too_large`.
- [ ] `cmd/faas tasks enqueue / list / get / requeue` works
      end-to-end.
- [ ] `pkg/e2e/tasks_e2e_test.go` exercises the full lifecycle.
- [ ] Dashboard renders `task.dead_letter` rows.
- [ ] Sealed payloads round-trip through Postgres with
      `payload_cipher` non-null and `payload` null.

### Risk register

1. **Schedd-tick contention with the cron tick.** Both sweeps
   compete for the same `schedd` goroutine + the same `EnsureInstance`
   path. Mitigation: the cron tick already does `FOR UPDATE SKIP LOCKED`
   to avoid contention; the task tick uses the same primitive on
   a different table, no shared row locks. The schedd's per-app
   fairness scheduler (`pkg/sched/fairness.go`) treats a task
   delivery as one `EnsureInstance` slot, identical to a cron.

2. **Postgres storage growth.** A 256 KB × 10,000 tasks/app on
   Scale = 2.5 GB per Scale app. Mitigation: the
   `tasks_queue_ready_idx` partial index only indexes `status='queued'`
   rows; succeeded / failed / dead rows are not in the hot path. A
   `pkg/retention/tasks_gc.go` weekly job deletes `status IN
   ('succeeded','failed','dead') AND last_attempt_at < now() -
   interval '30 days'`. Retention is tunable per-account.

3. **Internal Bearer replay.** A leaked `Bearer faas_task_<...>`
   token is valid until `exp` (default 5 minutes). Mitigation:
   `exp` is short (5 min — the delivery must happen in the
   scheduled window or the task is retried anyway); the token's
   `task_id` is single-use (`public.tasks.status='running'` is
   the lock — once the task finishes, replay returns 401). The
   customer's `task_handler_path` middleware enforces this check.

4. **Cold-boot for every task.** A Scale customer enqueuing
   10,000 tasks on a parked app → 10,000 cold-boots → wake-storm.
   Mitigation: the schedd's existing wake rate-limiter
   (`pkg/sched/rate_limit.go::EnsureInstanceRateLimit`) bounds the
   cold-boot rate to `plan_concurrency_per_app` per minute. Tasks
   queue beyond the rate window; the `task_queue_depth{status="queued"}`
   gauge surfaces the backlog. Documented in ADR-080
   §"Consequences — Cold-boot behavior".

5. **ADR-020 dependency.** Task sealing uses the same X25519 key
   pair as `AppSecret`. If ADR-020's key-rotation story changes
   (currently manual — see `pkg/secrets/rotate.go` TODO), this
   ADR inherits the change.

## Out of scope (separate issues)

- `waitUntil` post-response tail — #667 (shipped via PR #683 + #700).
- Durable execution wrapper over crons — #669.
- Persistent state across wakes — rejected per #668 §"Out of scope".
- Cross-app task routing — single-app queue only.
- Per-task timeout (beyond `TaskDeliveryDeadlineSec`) — handled by
  the wake's existing reaper; not a task-queue concern.
- Webhook delivery as a task — handled by ADR-076 webhook
  delivery, which already exists and is its own primitive.