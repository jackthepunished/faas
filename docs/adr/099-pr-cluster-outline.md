# ADR-099 PR-cluster outline

Six reviewable PRs (PR-0 + PR-A through PR-F), each ships behind
default-OFF semantics where it touches customer-facing surface, each
independently reviewable in ~10 min. Mirrors the cluster discipline from
ADR-090 (named envs), ADR-091 (edge rules), ADR-092 (scoped secrets),
and ADR-098 (connection-aware execution). The rate-limiter primitive
(PR-0) ships first because it is load-bearing for both ADR-099 and
ADR-080 (per memory `pr-823-issue-678-pr0-shipped`).

| PR | Files touched | Behavior change | Review budget |
|---|---|---|---|
| **PR-0 rate-limit** | `pkg/sched/rate_limit.go` + test, `pkg/api/limits.go` (2 fields) + test, `pkg/sched/engine.go` (1 gate) | Token-bucket per app + per account. Per-plan burst: Free 1, Hobby 5, Pro 20, Scale 100 wakes/min (per-app); 1/10/30/150 per-account. **MERGED** as #886 (2026-08-13). | ~10 min |
| **PR-A schema** | `migrations/00255_jobs.sql` (3 tables), `migrations/00256_instances_kind_job.sql` (instances widening), `migrations/00257_usage_minutes_app_nullable.sql` (metering widening), `migrations/00258-00263_reserve_slot.sql` (6 fences), `migrations/00244_reserve_slot.sql` (contiguity filler), `docs/adr/099-pr-cluster-outline.md` (this file) | None. Pure DDL; no code path reads the new tables yet. Default backfill (`kind='wake'`, `meter_kind='app'`) preserves the existing wake/build paths. | ~10 min |
| **PR-B state** | `pkg/state/jobs.go` (sqlc-generated), `pkg/state/jobs_test.go`, `pkg/state/queries.sql` (10+ queries), `pkg/state/pgstore.go::WithJobStore` setter, `cmd/apid/main.go` wire | sqlc regen + 10 new state methods (CreateJob, GetJob, ListJobs, DeleteJob, UpdateJob, CreateRun, ClaimTasks, MarkTaskSucceeded/Failed/Timeout/Cancelled/OOM, RecomputeRunStatus). No apid routes yet. | ~15 min |
| **PR-C dispatch** | `pkg/sched/dispatch_jobs.go` (1s tick), `pkg/sched/engine.go::WakeJob` (~150 LOC mirroring `Wake`), `pkg/sched/jobs.go` (AllocateJobTask, MarkJobTaskTerminal), `pkg/sched/reaper.go` (kind filter), `pkg/sched/loop.go::Loop` (register runJobsTick), `cmd/schedd/main.go`, `guest/init/job_supervisor.go`, `pkg/fcvm/vsock.go` (port 1026/3), 4 new tests | The architectural PR. Job-task VM path with watchdog, exit-code capture, env merge. Default OFF (`FAAS_JOBS_DISPATCH=0`) until PR-D. | ~25 min |
| **PR-D apid** | `cmd/apid/handlers/jobs.go` (11 routes), `pkg/api/limits.go` (7 plan caps), `pkg/api/dto.go` (Job/JobRun/JobTask DTOs), `pkg/api/spec/openapi.yaml`, `pkg/apid/openapi.yaml` (regen), `pkg/api/client.go` (typed methods), `pkg/api/errors.go` (5 codes), 3 SDKs (regen), 2 tests | Customer-facing surface. Hobby+ opt-in flag (`FAAS_JOBS_Hobby=true` first 2 weeks per task #18). Free plan 402. | ~20 min |
| **PR-E CLI+e2e** | `cmd/gregale/cmd/jobs.go` (7 subcommands), `cmd/gregale/cmd/jobs_test.go`, `pkg/e2e/jobs_e2e_test.go` (14 acceptance items from ADR-099), `pkg/state/jobs_e2e_helpers.go`, `dashboard/jobs.html`, `dashboard/job_run.html` | Full lifecycle coverage: create, fan-out, retry, OOM, cancel, dead-letter, log tail, RAM cap, Free 402, meterd roll-up. | ~25 min |
| **PR-F docs** | `docs/faas_implementation_spec.md` §4.7.4 (new), §6 (widen "wake or build" → "wake, build, or job_task"), `docs/STATUS.md` M7.x, `docs/runbooks/JobsBacklog.md`, `docs/runbooks/JobTaskOOMStorm.md` | Docs only. | ~10 min |

## Cross-PR gates (every PR satisfies)

Per CLAUDE.md + repo memory (each rule has a memory entry — name in
backticks):

1. `make lint` + `make test` + `make spec-check` (`ci-three-job-split`).
2. `make metal-lima` for any touch to `pkg/fcvm` / `pkg/netns` /
   `vmmd` / `builderd`. PR-C touches `pkg/sched` + `guest/init`;
   `make test-metal` + `make leakcheck` required per CLAUDE.md.
3. Every new quota in `pkg/api/limits.go` (`pkg/api/limits_test.go`
   table-driven coverage). No inline numbers. PR-D adds 7 plan caps
   for jobs (`JobMaxPerAccount`, `JobMaxPerApp`, `JobMaxParallelismPerRun`,
   `JobMaxTasksPerRun`, `JobMaxTimeoutSec`, `JobMaxRetries`,
   `JobMaxImageMB`).
4. No direct cross-component call. apid writes PG → meterd reads →
   schedd learns via `pg_notify` (`job_tasks_queued` channel
   introduced in 00255). No apid→schedd gRPC.
5. `Engine.WakeJob` mirrors `Engine.Wake` — same single-flight
   discipline, same appMu lock narrowing pattern, same watchdog.
6. §11 secret rule: env overrides pass through secretbox sealing
   (no plaintext at rest); `pkg/api/errors.go::StatusForCode` covers
   the 5 new codes (`CodeJobsNotAllowed`, `CodeJobQuotaExceeded`,
   `CodeJobTaskNotFound`, `CodeJobRunCancelled`, `CodeJobDeadlineExceeded`).
7. OpenAPI 3.1 + SDK regen (`pr-819-openapi-nullable-3-1`,
   `sdk-coverage-walks-pkg-api`). Three SDKs regenerated in PR-D.
8. Slot fence discipline (`cross-pr-slot-gate-reservation-fence-pattern`,
   `pr-849-adr-092-pr-a-slot-chase-cluster`). 00258-00263 carry the
   PR-B/C/D/E/F coordination headroom; 00244 is a co-fence with
   PR #887 (edge-rules throttle; if #887 lands first it overwrites
   this fence with its real `00244_edge_rules_kind_throttle.sql`).
9. `pkg/apid/openapi.yaml` mirror via `make spec-sync`
   (`spec-sync-stale-embed-on-openapi-change`).
10. `golangci-lint v2.4.0` 173 pre-existing alerts are not
    regressions (`golangci-lint-v2-4-0-handler-checklist`).
11. CI-protoc flake fix landed in PR #873; the cluster never
    re-introduces `arduino/setup-protoc@v3`
    (`ci-arduino-protoc-rate-limit-flake`).

## Slot allocation (PR-A ranges)

`git ls-tree origin/main migrations/` at PR-A push time had 00243 as
the highest merged slot. PR #887 (edge-rules throttle) originally
co-fenced with the ADR-099 cluster at 00244; PR #864 (edge-rules
budget) renumbered off 00244 to 00245 first (see #864's
`00245_edge_rules_kind_budget.sql`), which forced the ADR-099
cluster to skip 00244-00254 as well to preserve the
cross-pr-slot-gate-reservation-fence-pattern invariant (open
sibling PRs may still take any unallocated slot below this PR's
base).

**Final allocation: 00255-00263 (3 real + 6 fences).** 00244 holds a
co-fence that survives alongside #887's real 00244 file — the
second merger replaces the fence (same file path, same no-op body,
harmless overwrite).

| Slot | File | PR owner |
|---|---|---|
| 00244 | `00244_reserve_slot.sql` | co-fence with #887 (one will overwrite, see header) |
| 00255 | `00255_jobs.sql` | PR-A (this PR) |
| 00256 | `00256_instances_kind_job.sql` | PR-A |
| 00257 | `00257_usage_minutes_app_nullable.sql` | PR-A |
| 00258 | `00258_reserve_slot.sql` | PR-B (sqlc regen, no real migration; fence still useful for cross-PR coordination) |
| 00259 | `00259_reserve_slot.sql` | PR-C (1-2 migrations: dispatch tick + Watchdog exit-code DGRAM port) |
| 00260 | `00260_reserve_slot.sql` | PR-D (no migration; OpenAPI regen only) |
| 00261 | `00261_reserve_slot.sql` | PR-E (no migration; CLI + e2e + dashboard) |
| 00262 | `00262_reserve_slot.sql` | PR-F (no migration; docs only) |
| 00263 | `00263_reserve_slot.sql` | spare buffer for hot-fix or future amendment |

## Renumber history

PR-A originally targeted 00245-00253 + 00244 fence. At push time
the CI migration-slot precheck (per memory
`cross-pr-slot-gate-races-with-active-pr` +
`cross-pr-slot-precheck-pr-867-collision-2026-08-13`) caught a
collision with PR #864 (which had recently taken 00245 from its
original 00238-00244 fence range). The pattern resolution
(loser-renumbers past winner) bumped PR-A's base from 00245 to
00255; the 00244 fence was kept as a coordination buffer with
#887's real 00244 file.

## Rollout gate (cluster-as-a-whole)

1. Hobby plan opt-in flag (`FAAS_JOBS_Hobby=true`) for the first 2
   weeks post-merge (per the imp plan's non-code task table).
2. Default-OFF in schedd until PR-D lands (PR-C ships behind
   `FAAS_JOBS_DISPATCH=0`).
3. meterd smoke test in PR-A's cluster: roll-up queries on
   `usage_minutes WHERE meter_kind='job'` must return 0 rows
   post-apply (no job-metered rows exist yet) — the pre-existing
   `meter_kind='app'` rows pass the pair CHECK unchanged.
4. PR-A acceptance: `migrations/apply_walk_test.go` green
   (contiguity from the embedded set); `migrations/embed_test.go`
   green (filename-vs-embedded agreement).
