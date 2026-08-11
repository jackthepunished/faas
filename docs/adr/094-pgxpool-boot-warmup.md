# ADR-094 · pgxpool boot warm-up barrier

- **Status:** accepted v1.0 (2026-08-11)
- **Date:** 2026-08-11 (proposed + accepted)
- **Decision:** Pin a two-part boot-time contract on every daemons that uses `pkg/db.Open`:
  1. **Warm-up barrier.** Immediately after `db.Open` returns and before any background goroutine launches (`bgBefore`), call `db.WarmUp(ctx, pool, want, deadline)` to acquire (and release) `want` connections. The helper is **fail-CLOSED by default**: if `want` connections can't be acquired within `deadline`, the daemon refuses to start. `apid` is the first caller (4 conns / 5s); future daemons can call the same helper with their own `want` and `deadline`.
  2. **Pool lifetime decoupling.** `defer pool.Close()` no longer fires at the top of the daemon's `run()` — it fires **after the listener bind succeeds**, gated by a `sync.OnceFunc` helper (`closePoolOnce`) that survives every early-return in the bind path. The helper closes the pool exactly once, regardless of which early-return site fires.
- **Why:** PR #823 (issue #678 PR-0) hit a deterministic e2e flake in `TestRekeyRunnerPg` across five consecutive retriggers (CI runs 31475571835, 31477156489, 31478096496, 31478907581, 31480046338), every time on e2e shard 3, every time at `secrets_rotate_box_e2e_test.go:120` with `127.0.0.1:<port> did not accept within 10s`. The flake chain:
  1. Phase-2 apid boots, enters `runWithDeps`, runs through `deps.bgBefore(...)` BEFORE the listener bind at line 1068.
  2. `bgBefore` launches 8+ concurrent goroutines, each calling `pool.Acquire()` on a `MaxConns=8` pool (`pkg/db/db.go:60`). The pool is exhausted before any goroutine finishes its first batch.
  3. The audit subscriber's fail-fast initial `db.SubscribeWithReconnect` returns `db: acquire listener: failed to connect to user=faas database=faas: 127.0.0.1:5432 (localhost): failed SASL auth: context canceled`.
  4. Other bgBefore goroutines that were mid-Acquire return `rekey: list batch: closed pool` because the defer-fired cleanup in `run()` closes the pool underneath them (the pre-fix `defer pool.Close()` was at line 388, BEFORE the listener bind).
  5. `runWithDeps` never reaches `srv.Serve(l)`; the `waitTCP(addr, 10s)` in `pkg/e2etest/harness.go:566` fires after 10s.

  `db.Open`'s implicit `pool.Ping(ctx)` (5s timeout, `pkg/db/db.go:74`) only proves **one** connection works; it does not prove that **N parallel** connections are acquirable before the daemon's goroutines launch. The warm-up barrier closes that gap with a deterministic pre-flight.

  The pool-lifetime decoupling is the second half of the fix: the `defer pool.Close()` shape meant an early-return in `runWithDeps` (e.g. on `cfg.LoadGithubdTLS()` error) tore down the pool while the bgBefore goroutines still owned LISTEN sessions and in-flight queries. The new shape is "pool lives until post-bind; close once, exactly once, via `closePoolOnce`".

- **Consequences:**
  - **New helper `pkg/db/warmup.go::WarmUp`.** Sequentially acquires and releases `want` connections within `deadline`. Returns `ErrWarmUpTimeout` (a typed sentinel — distinct from `context.DeadlineExceeded`) on partial acquire. Fail-CLOSED by default — opt-in fail-open (`WarmUpSlow`) is documented but not implemented.
  - **Architecture test `pkg/db/warmup_architecture_test.go::TestArch_ApidWarmsPoolBeforeBgBefore`.** AST-walker that parses `cmd/apid/main.go` and asserts (a) `db.WarmUp(...)` is called inside `run` / `runWithDeps`, (b) every `db.WarmUp` call precedes every `deps.bgBefore` and every `deps.listen` call, (c) the `closePool` helper literal exists in the source so a future refactor that drops the close-once gate fails the test. This is the grep-gate (reviewable in ~30 seconds); the unit tests on `pkg/db` pin the helper's behaviour.
  - **Unit tests `pkg/db/warmup_test.go`.** Table-driven cover: nil pool, want=0 / want<0 / deadline<=0, closed pool, ctx-canceled ctx, healthy pool (live pgxpool), `want > MaxConns` with held conn (asserts `ErrWarmUpTimeout` and bounded elapsed time).
  - **e2e pin `TestRekeyRunnerPg_Phase2Isolated`.** Sibling to `TestRekeyRunnerPg` that runs the same phase-1 → phase-2 lifecycle in stripped-down form (no secrets, no progress polling) so it can run as a CI smoke test multiple times. The test asserts only that `h1.Stop()` + `StartWithEnv(APID, …)` succeeds deterministically — if the L2 fix ever regresses, this sibling flakes the same way the original did.
  - **Exported `pkg/e2etest.Harness.Stop()`.** Thin wrapper around the unexported `stop()`. Required so tests in other packages (e.g. `cmd/e2e`) can release daemon subprocess resources between phases. Idempotent (the inner loop short-circuits on `p.ProcessState != nil`).
  - **`cmd/apid/main.go` rewrite.** `run()` declares `closePool` after `db.Open`; `runWithDeps` wires it via `runDeps.closePool` (a new field, nil-tolerant for tests). Every early-return in the pre-bind range is covered by the `defer closePoolOnce()` fallback; explicit `closePool()` calls before each `return err` suppress the deferred close so the pool is freed promptly (the Once guards against double-close). The `srv.Serve` goroutine runs against a live pool; the pool is closed via the deferred fallback when `runWithDeps` returns.

- **Reused primitives (no change):**
  - **CLAUDE.md "Components talk via Postgres rows + pg_notify"** — every goroutine in `bgBefore` already acquires its own connection; the warm-up barrier is a pre-flight, not a connection-sharing change.
  - **`pkg/db/wait_for.go::WaitForNotification`** — same `pool.Acquire` + `defer conn.Release` pattern; `WarmUp` mirrors the structure with a loop instead of a long-poll.
  - **`pkg/db.notify.go::SubscribeWithReconnect`** — fail-fast initial Subscribe is unchanged; the warm-up barrier does not affect its error semantics, only prevents the "closed pool" race downstream.
  - **`pkg/db.Open`'s implicit Ping** — kept as-is. Ping proves one socket works; WarmUp proves N sockets are acquirable in parallel. They solve different problems and must stay distinct.

- **Migration:** None — no schema change, no new env var, no new wire-level config. The new helper is opt-in (only `apid` calls it).

- **Risks + mitigations:**
  - **WarmUp blocks legitimate slow-cluster startups.** Fail-CLOSED by default matches apid's "control-plane daemon refuses to start under misconfigured boot" stance (see `pkg/role.Require`, the recovery-HMAC loader's no-zero-key fallback). A future ops hook can add `WarmUpSlow` (fail-open with longer deadline) — not in scope for this PR.
  - **Defer-move forgets an early-return site.** The `defer closePoolOnce()` fallback at the top of `runWithDeps` is the defence-in-depth: any path that forgets an explicit `closePool()` call still closes the pool on return. The `sync.OnceFunc` guards against double-close when both paths fire.
  - **Other daemons (`schedd` / `vmmd` / `builderd` / `imaged` / `meterd`) need the same fix.** Scoped to apid because apid is the only daemon with 4+ concurrent `pool.Acquire()` callers in `bgBefore`. If a future daemon picks up a fifth concurrent goroutine at boot, the architecture test's `TestArch_ApidWarmsPoolBeforeBgBefore` would still pass (it only pins apid's wiring) — a parallel test per daemon is the next lever, deferred.
  - **`h1.Stop()` accidentally closes the test pool.** Verified by reading `pkg/e2etest/harness.go:846-875` — the inner `stop()` only signals daemons; it never touches `h.Pool`. The pool is closed by `pgtest.Open`'s cleanup (`pkg/db/pgtest/pgtest.go:79-94`) at test exit.
  - **`h1.Stop()` double-cleanup at test return.** The first loop at `:847-852` does `_ = p.Process.Signal(SIGTERM)` (no-op on a reaped process); the second loop at `:853-867` skips procs with non-nil `ProcessState`. The first `Stop` is reentrant-safe. The new exported wrapper is idempotent because it calls `stop()` once.
