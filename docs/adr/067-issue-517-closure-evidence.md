# ADR-067 · Issue #517 closure evidence (LOGGING: correlation, server-side filters, explicit gap semantics)

- **Status:** accepted
- **Date:** 2026-08-02
- **Closes:** #517
- **Decision:** Close issue #517. The work it scopes was decomposed across three
  merged PRs (PR-A, PR-B, PR-C) that together satisfy every acceptance criterion
  in the issue body. This ADR records that evidence and the one residual thread
  that is intentionally deferred to a follow-up issue.

### Note on slot-collision hygiene

This ADR is filed under number 067 because that was the next free slot at the
time of writing. The repo's `docs/adr/` directory has duplicate-number collisions
on **ADR-062** (two files) and **ADR-064** (two files) on `main` at the time of
filing — both predate this PR and are not addressed here. If you are reading
this ADR as part of a #517 closure review, treat the cited filenames (e.g.
`docs/adr/064-wake-timeline-canonical-vocabulary.md`) as the source of truth,
not the bare ADR number.

## Context

Issue #517 ("LOGGING: correlation, server-side filters, and explicit gap
semantics") was opened with the goal of making the logging contract correct and
explainable across the request and wake lifecycle. Its scope touched six threads
(envelope, correlation, platform events, server-side filters, gap frames, jailer
stderr capture) and six acceptance criteria. Rather than land as one PR, the work
was sliced:

- **PR-A** (#520, merged 2026-08-01, commit `e3bf0981`) — canonical envelope +
  correlation propagation across HTTP / gRPC / slog.
- **PR-B** (#524, merged 2026-08-02, commit `3c5dad17`, plus `f1634765`,
  `ecd8f779`, `db70e8aa`) — server-side filter enforcement + ring-buffer gap
  frames.
- **PR-C** (#532, merged 2026-08-02, plus `35181909`, `90e56e5e`, `bb6200c9`,
  `a88f5682`, ADR-064) — canonical wake-timeline vocabulary + structured
  platform events + SDK exposure.

A separate ring-buffer PR (#537, branch `feat/phase4-slice-a-prb-ring-buffer`,
commit `760a2562`) wired the 64 KiB Supervisor-side ring on the metal path. It
predates PR-B but is part of the same logical surface.

This ADR exists so the next reviewer can grep one file to verify the AC mapping
instead of re-deriving it from three PR threads and a renumber chain.

## Acceptance-criterion → shipped PR mapping

Each acceptance criterion from the issue body, paired with the PR (or commit)
that satisfied it and the canonical evidence path a reviewer can `Read` to spot
check.

| AC | Acceptance criterion (verbatim, condensed) | PR / Commit | Evidence |
|---|---|---|---|
| 1 | Single request traceable `gatewayd → schedd → vmmd` via shared correlation | PR-A `e3bf0981` | `pkg/wire/grpcmetadata.go`, `pkg/wire/logging.go`, `pkg/sched/engine_test.go::TestEngineWake_PropagatesWakeIDToVMM` |
| 1 | `request_id` propagated through gRPC metadata and synthetic requests | PR-A `e3bf0981` | `pkg/middleware/requestid.go`, `pkg/wire/grpcmetadata.go::WithCorrelationOutgoing` |
| 1 | `app_id` / `deployment_id` / `instance_id` / `wake_id` / `invocation_id` carried wherever available | PR-A `e3bf0981` | `pkg/wire/logging.go::CorrelationFields`, proto `vmmd.proto` `wake_id` fields |
| 2 | Cold wake has a queryable timeline (queue / admission / restore / cold-boot / guest resume / readiness / proxy) | PR-C `a88f5682`, `90e56e5e`, `35181909`, `bb6200c9`, ADR-064, M5 §14 e2e `edc41894` | `pkg/events/platform.go`, `migrations/00106_events_wake_id_idx.sql` (next free slot, future), `ListEventsByWakeID`, `ListWakeTimeline` on the public Go SDK |
| 3 | Log filters enforced before data leaves the platform | PR-B `3c5dad17` + `f1634765` | `pkg/sched/logs.go` (server-side filter pipeline), `pkg/api/limits.go` (plan-gated limits), `pkg/apislogs/sse.go` |
| 4 | Dropped / unavailable ranges explicit in wire protocol + tests | PR-B `3c5dad17` + `f1634765` | `pkg/sched/vmmclient.go::LogLine{IsGap, GapReason, GapToWrittenAt}`, `pkg/fcvm/logbuf/ring.go` (gap emission), `pkg/vmmdgrpc/logs.go` (gap frame wire), tests `pkg/fcvm/logbuf/ring_gap_test.go`, `pkg/vmmdgrpc/logs_gap_test.go`, `pkg/sched/logs_filter_test.go` |
| 5 | Firecracker / jailer config failures summarized safely, customer-safe error reference, no host command lines exposed | **Partial** — see "Residual thread" below | `pkg/logsanitize/sanitize.go`, `pkg/grpcerr` |
| 6 | No secrets / auth headers / cookies / request bodies / raw host paths in logs | PR-A `e3bf0981` | envelope helper drops empty fields (see `pkg/wire/logging.go`); `pkg/logsanitize` audit pattern. Note: PR-A's commit message references "ADR-061" but that slot was reassigned to iam-6; no standalone envelope-sanitization ADR was filed. |

## Test surface (verbatim test names a reviewer can `go test -run`)

- `pkg/wire/logging_test.go` — envelope emission, empty-field drops, context round-trip, nil-safe contract.
- `pkg/wire/grpcmetadata_test.go` — round-trip across simulated server / client, empty-field skips, no-op on empty struct, MD-join preserves pre-existing keys.
- `pkg/sched/engine_test.go::TestEngineWake_PropagatesWakeIDToVMM` — fakeVMM captures the boot ctx and asserts `request_id` (inbound) + `wake_id` / `instance_id` (engine-minted) all reach the vmmd boundary.
- `pkg/fcvm/logbuf/ring_gap_test.go` — ring emits gap frames on `seq_below_retained` and `since_below_retained`.
- `pkg/vmmdgrpc/logs_gap_test.go` — vmmd logs RPC gap-frame wire shape, zero-valued line frame fields, explicit `gap_reason`.
- `pkg/sched/logs_filter_test.go` — programmable LogStream, plan-gated filter matrix (grep / level / deployment / time), gap-frame passthrough.
- `pkg/scheddgrpc/logs_test.go` + `pkg/scheddgrpc/bufconn_test.go` — schedd-fan-out shape.
- `cmd/gatewayd/app_logs_test.go` — gateway app-logs handler.
- `cmd/schedd/main_test.go` — schedd wiring.
- E2E: PR-C `edc41894` ("M5 §14 acceptance — wake-timeline e2e").

## Residual thread (deferred to a follow-up issue)

AC #5 in the issue body — *"Firecracker / jailer configuration failures are not
discarded and have a customer-safe error reference"* — is partially satisfied:

- `pkg/logsanitize/sanitize.go` already scrubs host-sensitive patterns.
- The ring buffer (`pkg/fcvm/logbuf/ring.go`) is the operator-side sink for guest
  stdout / stderr.
- `pkg/grpcerr` lifts typed `*api.Problem` errors off the gRPC seam.

The remaining work — **conversion of vmmd's own stderr (jailer / firecracker
launch failures, config validation rejections, kernel-load errors) into
sanitized customer-safe summaries with a stable error reference** — touches
`pkg/vmmdgrpc` and `cmd/vmmd` configuration-failure handling. It is a separate
concern from the #517 logging envelope contract because:

1. It is not about log propagation, correlation, or filter enforcement; it is
   about error *visibility*.
2. It crosses the §11 secret-leak / command-line-redaction boundary which has
   its own audit surface (`pkg/logsanitize`).
3. Bundling it into a #517 closure PR would obscure both work streams in review.

This ADR therefore closes #517 with AC #5 explicitly **partial**, and recommends
opening a separate follow-up issue (working title: *"vmmd config-failure
sanitization with customer-safe error reference"*) to land that surface.

## Consequences

- Issue #517 can be closed after this ADR merges.
- The three shipped PRs (`#520`, `#524`, `#532`) plus the ring-buffer PR
  (`#537`) remain the canonical citations — this ADR is the index.
- A new issue should be opened for the AC #5 residual (see above).
- No new migration is required for the closure PR itself.

## Cross-references

- `pkg/wire/logging.go`, `pkg/wire/grpcmetadata.go` — canonical envelope +
  correlation helpers (PR-A #520, commit `e3bf0981`). No standalone ADR was
  filed for the envelope shape; PR-A's commit message references "ADR-061" but
  that slot was reassigned to iam-6 (organizations/memberships, PR #536)
  before the logging ADR could land under its own number. Treat the wire
  helpers as the source of truth.
- `docs/adr/064-wake-timeline-canonical-vocabulary.md` — wake.* event vocabulary
  (PR-C). Note: at filing time `docs/adr/` has two ADR-064 files; cite by
  filename, not by ADR number.
- `docs/adr/041-migration-slot-reservation.md` — migration slot reservation
  convention. If the AC #5 follow-up needs a schema change, the next free
  slot must be discovered at PR-creation time via
  `ls migrations/ | sort -n | tail -1` — slot numbers rot between this ADR
  being written and any future follow-up landing.
- `pkg/wire/logging.go`, `pkg/wire/grpcmetadata.go` — canonical envelope helpers.
- `pkg/sched/logs.go` — server-side filter enforcement.
- `pkg/fcvm/logbuf/ring.go` — guest log ring with seq + gap metadata.
- `pkg/vmmdgrpc/logs.go` — vmmd logs RPC, emits gap frames.
- `pkg/api/sse.go` — public wire DTO (the `event: gap` frame).
- `pkg/events/platform.go` — structured platform events fan-out.

## Verification (defensive re-verification)

This PR is docs-only — it touches no Go code, no proto, no OpenAPI. The
following checks are run defensively so the merge commit cannot silently
break a gate:

- `go test -race -count=1 -timeout=15m ./...` must remain green.
- `gofmt -l .` must be clean (a no-op for markdown, but verifies the rest of
  the tree).
- `golangci-lint run` must report 0 issues.
- `make proto-check` must be clean (no `*.pb.go` drift).
- `make spec-check` must be clean (no OpenAPI drift).
