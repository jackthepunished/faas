# ADR-047 · Stateless-runtime advisory (Wave 0 PR-C / G13)

- **Status:** accepted
- **Date:** 2026-07-29
- **Decision:** Wire a guest-init `fanotify` advisory that observes
  writes to a closed set of state-shaped paths and ships one
  debounced batch over AF_VSOCK DGRAM to the host. vmmd forwards
  the batch to apid over a new `/run/faas/apid.sock` gRPC seam
  (`api/proto/onebox/faas/apid/v1/advisory.proto`). apid writes
  one `stateless.advisory` audit row per batch via
  `pkg/audit.Auditor.Emit`. The advisory is **observation only**,
  never blocking — spec §17 G13 explicitly forbids EROFS / remount
  for Wave 0.
- **Why:** PR-A (PR #413) gated persistence at deploy-accept with
  a 422 `stateless_only_violation`. PR-B (PR #417) added
  `faas init` storage templates + `docs/storage.md` to teach the
  managed-service pattern. Both share one blind spot: a customer
  who passes the deploy-time scan (no `VOLUME`, no top-level
  `data/`, no stateful base image) but then writes to `/data/foo`
  at request time sees no signal that the data is gone after the
  next park. PR-C closes that loop. The third bullet of
  `pkg/api/errors.go:357-358` already reserves this behaviour but
  the referenced `guest/init/stateless_advisory_linux.go` file
  did not exist before this PR. The customer experience is
  "scale-to-zero forgets" — the audit row makes that contract
  observable rather than silent.
- **Consequences:**
  - New `guest/init/stateless_advisory_linux.go`
    (`//go:build linux && amd64`): `unix.FanotifyInit` with
    `FAN_CLASS_NOTIF` (observe-only), `FAN_MARK_MOUNT` over the
    closed path set (`/data`, `/db`, `/var/lib/postgresql`,
    `/var/lib/redis`, `/var/lib/mysql`, `/var/lib/mongodb`,
    `/var/lib/mongo`, `/var/lib/cockroach`, `/var/lib/cassandra`,
    `/var/lib/clickhouse`), debounce 1s per
    `(path, mask-set)`, ship over AF_VSOCK DGRAM
    (`VsockStatelessAdvisoryPort=1025`, distinct from
    `VsockResumePort=1024`; `VsockStatelessAdvisoryMsgType=2`,
    distinct from `VsockResumeMsgType=1`).
  - New `api/proto/onebox/faas/apid/v1/advisory.proto`: unary
    `ForwardStatelessAdvisory` RPC. Wire is one proto per
    batch; the audit row's `data` JSON mirrors the proto.
  - New `pkg/vmmdgrpc/advisory_client.go`: vmmd-side dialer
    (first vmmd-issued gRPC client in the repo). Lazy dial,
    200 ms dial timeout, 1 retry on `codes.Unavailable`. ADR-035
    best-effort: a dropped advisory is silent (log Warn +
    counter increment).
  - New `cmd/apid/advisory_receiver.go`: apid-side gRPC server
    bound on `/run/faas/apid.sock`. Resolves `app.AccountID` from
    `state.Store.AppByID(appID)`, emits one audit row with
    `subject=accountID`. Missing app row → `codes.NotFound`
    (lets vmmd's retry logic distinguish "app genuinely gone"
    from "transient DB blip"); non-NotFound store error → emit
    with `subject=NULL` so the row isn't dropped.
  - `pkg/fcvm/manager.go` extended: new `VMM.SendStatelessAdvisory`
    (Wave-0 pass-through stub), `Manager.ForwardStatelessAdvisory`
    public seam, `AdvisoryForwarder` interface field wired via
    `SetAdvisoryClient`.
  - New pg_notify channel `db.NotifyStatelessAdvisory =
    "stateless_advisory"`. Wired into BOTH
    `cmd/apid/handlers_events.go::eventsChannels` and
    `cmd/apid/sse_fanin.go::sseChannels` (lock-step — see the
    `// Mirrored in cmd/apid/sse_fanin.go::sseChannels` comment).
    Payload is a SMALL summary (`{app_id, instance, n,
    sample_path}`); detail surface is the audit row at
    `/v1/audit-events?kind_prefix=stateless.advisory`.
  - `cmd/apid/handlers_audit.go` extended to accept
    `?include_anonymous=true` (default false) so operators can
    surface subject=NULL rows (the defensive case where the app
    row was deleted between wake and advisory). Default false
    keeps customer dashboards clean.
  - `pkg/api/client.go::ListAuditEvents` gains a trailing
    `includeAnonymous bool` parameter. OpenAPI spec mirrors the
    same. SDK regen via `make sdk-gen-node` is additive — old
    SDKs that don't pass the new arg default to false.
  - New `cmd/faas/commands_audit_events.go` (subcommand
    `faas audit-events`) — surfaces `kind_prefix`, `since`,
    `limit`, `include_anonymous` on the CLI. The existing
    `faas tail` is extended with `--include-stateless` for the
    SSE live view (default off).
  - `pkg/dashboard/templates/app_detail.html` extended with a
    "Stateless advisories" section that links to
    `/dashboard/audit-events?kind_prefix=stateless.advisory&app_id=…`.
    The `apps_list.html` "warnings detected: N" badge was
    deferred to a Wave 1 follow-up (requires a backend count
    endpoint not in the Wave 0 plan).
  - `deploy/systemd/faas-apid.service` line 21 extended with
    `--advisory-sock=/run/faas/apid.sock`.
  - `pkg/imaged/handler_test.go::TestHandleDeployment_PullDigestSentinel_
    PersistsErrorCode` gains a 5th row pinning the
    `ErrStatelessOnlyViolation → CodeStatelessOnlyViolation`
    mapping end-to-end.
  - **vmmd becomes a gRPC client for the first time.** Default-
    local vmmd (no Postgres, no apid.sock to dial) is preserved:
    `Manager.advisoryClient == nil` short-circuits
    `ForwardStatelessAdvisory` to a no-op. depguard's
    `apid-control-plane-only` table is unchanged — apid never
    imports vmmdgrpc, only the reverse.
- **Rejected alternatives:**
  - **Option B — vmmd writes events directly via
    `state.Store.AppendEvent`.** Forces vmmd to construct a
    `*state.PgStore` and a `pgxpool.Pool`. Breaks the
    "default-local vmmd is DB-less" invariant; doubles vmmd's
    surface for what is a one-row write. Rejected.
  - **Option C — new `stateless_advisories` table.** Adds a
    migration, a third writer to a new table, and an extra
    customer-facing endpoint. The audit table already exists
    and is the right home for observational data per ADR-035.
    Rejected.
  - **vsock STREAM vs DGRAM.** STREAM needs a host-side bind +
    accept loop + retry-with-backoff. DGRAM is fire-and-forget;
    a dropped advisory is silent, matching ADR-035's
    "observation, not source of truth" semantics. STREAM is a
    Wave 1 switch if the advisory ever becomes safety-critical.
  - **EROFS / read-only remount on state-shaped paths.** Explicitly
    forbidden by spec §17 G13 for Wave 0 ("advisory only, no
    EROFS"). The audit row is the customer-facing signal; if
    telemetry later shows misuse, Wave 1 can lift to a hard
    block.
  - **Generic dynamic path discovery at runtime** (e.g. scanning
    `/var/lib/*` on every wake). Adds startup cost and
    unbounded fanotify marks. Closed-set (10 entries) matches
    PR-A's `statefulTopLevelDirs` shape. Wave 1 follow-up if
    audit telemetry shows a different daemon data dir.
  - **Make `pkg/vmmdgrpc` importable from apid for the reverse
    direction.** depguard's `apid-control-plane-only` deny list
    (`.golangci.yml:50-78`) explicitly forbids it — apid never
    dials vmmd. The new `apid.sock` keeps the boundary clean:
    vmmd pushes to apid, apid never pushes to vmmd.

## Cross-references

- Spec §17 G13 (`docs/faas_implementation_spec.md:682`), §6
  (state machine), §11 (security), §14 (acceptance gates).
- ADR-035 (audit taxonomy + dropped-rows-are-silent semantics).
- ADR-005 (snapshots are cache, not truth — applies here: a
  dropped advisory doesn't break wake).
- PR-A (PR #413 — stateless_only_violation at deploy-accept).
- PR-B (PR #417 — storage templates + dashboard CTA).
- Issue #266 (Move 4 / Tier 1 ship-blocker family).
- Memories: `apid-park-wake-not-a-vmmd-call`,
  `cross-renderer-invariant-pattern`,
  `golangci-lint-v2-4-0-handler-checklist`,
  `move-4-architectural-decision-gateway-streaming`,
  `pr-c-cross-pr-slot-56-collision-pr369` (slot reservation
  pattern).
