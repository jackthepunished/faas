package api

// trigger.go — wire shape for the unified Trigger primitive
// (issue #757 / ADR-0NN).
//
// Six kinds live on one Trigger resource (kind discriminator):
//
//	cron         existing robfig/cron/v3 path; this file mirrors it
//	             onto the new shape (Schedule + Path) without removing
//	             the crons table (commit #2 keeps crons and pairs a
//	             triggers row via cron_id).
//	kafka        Kafka consumer group
//	nats         NATS JetStream durable consumer
//	redis_streams Redis Streams XReadGroup
//	sqs_compat   Long-poll on the in-platform SQS-compatible queue
//	queue        In-platform queue / delayed-task unification
//
// Five of the six share the same batch envelope (size + window +
// max_attempts). Only `cron` keeps its existing fields because the
// robfig 5-field scheduler needs (schedule, path) rather than (slug,
// config). On-disk: kind=cron rows always have cron_id IS NOT NULL;
// the other five kinds always have cron_id IS NULL (SQL CHECK at
// migrations/00267_triggers.sql). Kind is immutable after create.
//
// Wire shape decisions (pinned):
//
//   - Config is json.RawMessage so a per-kind config struct is decoded
//     lazily; the SDK round-trip preserves unknown fields.
//   - Slug is required for non-cron kinds (DNS-safe at the manifest
//     validator, see pkg/gregalemanifest/manifest.go) and absent for
//     cron (its identity is schedule+path).
//   - Schedule + Path are cron-only and serialize as omitempty so
//     non-cron rows don't carry empty cron placeholders.
//   - CronID + Source surface the legacy crons linkage: cron_id is
//     set on kind=cron rows, source is the "queue" / "delayed_task"
//     tag on the kind=queue rows that fan out from the unified
//     `invocations` table fan-in.
//
// This file is consumed by:
//   - cmd/apid/handlers_triggers.go (commit #6, HTTP routes)
//   - pkg/state/pgstore.go projections (commit #5)
//   - sdk-go (regenerated from OpenAPI in commit #19)
//   - pkg/sched/dispatch_triggers.go (commit #14, runtime dispatch)

import (
	"encoding/json"
	"time"
)

// TriggerKind is the closed-vocabulary discriminator. The set is
// pinned by the SQL CHECK on triggers.kind and by the per-kind
// validator in pkg/gregalemanifest.validateKindConfig. Adding a new
// kind requires:
//  1. New constant here.
//  2. New case in pkg/gregalemanifest.validateKindConfig.
//  3. New SQL CHECK membership in 00267_triggers.sql
//     (forced via a new migration; the existing CHECK is hard-pinned).
//  4. New poller file in pkg/sched/ (see commit #8..12 cluster).
//  5. New per-plan accessor + counter in pkg/api/limits.go (PR-B).
type TriggerKind string

const (
	TriggerKindCron         TriggerKind = "cron"
	TriggerKindKafka        TriggerKind = "kafka"
	TriggerKindNATS         TriggerKind = "nats"
	TriggerKindRedisStreams TriggerKind = "redis_streams"
	TriggerKindSQSCompat    TriggerKind = "sqs_compat"
	TriggerKindQueue        TriggerKind = "queue"
)

// BrokerPoisonStrategy is the closed-vocabulary carrier for the
// audit #10 column added in migration 00292_triggers_poison_strategy.sql.
// Pinned by the SQL CHECK on triggers.broker_poison_strategy.
//
// Literal-string constants (NOT a Go enum) because pkg/api cannot
// import pkg/state (per pkg-api-cannot-import-pkg-state). The
// string values are the exact SQL CHECK membership tokens, so
// passing one straight through to a sqlc parameter round-trips
// without a translation hop.
const (
	// BrokerPoisonStrategyCommit (default) — the dispatcher
	// dead-letters the record AND the kafka poller commits the
	// broker offset. The two sides are permanently out of sync
	// for that offset; operator retry works via the dashboard's
	// "re-drive from DLQ" action which mints a fresh
	// trigger_records row from the same item_id.
	BrokerPoisonStrategyCommit = "commit"
	// BrokerPoisonStrategySeekToOffset — the dispatcher
	// dead-letters the record AND the kafka poller rewinds the
	// consumer-group offset via SetOffset. The next Poll
	// re-fetches the same message; operator retry combines a
	// trigger re-enable with a dashboard "reset offset" action
	// that re-fetches the dead-lettered payload.
	BrokerPoisonStrategySeekToOffset = "seek-to-offset"
)

// Trigger is the wire shape returned by GET/POST/PATCH on
// /v1/triggers. Mirrors the triggers table (commits #2/#3). The
// Config blob is opaque at the wire level — every kind's
// per-shape config struct decodes from `Config`. The SDK round-trip
// preserves the raw JSON so unknown fields don't get dropped by a
// older SDK.
type Trigger struct {
	ID            string          `json:"id"`
	AccountID     string          `json:"account_id"`
	AppID         string          `json:"app_id"`
	Kind          TriggerKind     `json:"kind"`
	Slug          string          `json:"slug,omitempty"`
	Enabled       bool            `json:"enabled"`
	Config        json.RawMessage `json:"config"`
	BatchSizeMax  int             `json:"batch_size_max"`
	BatchWindowMs int             `json:"batch_window_ms"`
	MaxAttempts   int             `json:"max_attempts"`

	// PayloadMaxBytes (migration 00274) bounds the per-record
	// broker payload size. Records above this cap are DLQ'd at
	// insert time with reason='payload_too_large' rather than
	// silently truncated. Default 6291456 (6 MiB); the SQL CHECK
	// admits [1024, 67108864].
	PayloadMaxBytes int `json:"payload_max_bytes"`

	// BrokerPoisonStrategy (migration 00275) controls how the
	// kafka poller reconciles its broker offset with a
	// dead-lettered record. Default "commit" preserves the
	// previous behaviour (broker offset advances; DB marks
	// dead_letter). "seek-to-offset" rewinds the consumer-group
	// offset via SetOffset so the next Poll re-fetches the same
	// message — operator retry then re-drives from the
	// dashboard's "replay poison" action. Closed vocab pinned by
	// the SQL CHECK.
	BrokerPoisonStrategy string `json:"broker_poison_strategy"`

	// Cron-only: kind=cron rows mirror a crons row via cron_id.
	// Mutually exclusive with the non-cron fields below (enforced
	// by SQL CHECK).
	Schedule string `json:"schedule,omitempty"`
	Path     string `json:"path,omitempty"`
	CronID   string `json:"cron_id,omitempty"`

	// Source only applies to kind=queue rows that fan in from the
	// `invocations` table — `queue` for per-app FIFO, `delayed_task`
	// for delayed-task rows (commits #8 + #16). Empty for every
	// other kind.
	Source *string `json:"source,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CreateTriggerRequest creates a new trigger. Kind is immutable
// after create; subsequent PATCH cannot change kind.
//
// Per-kind field gating mirrors pkg/gregalemanifest.validateKindConfig:
//   - kind=cron requires schedule + path (slug ignored).
//   - non-cron kinds require slug + config (schedule, path ignored).
//
// The apid handler enforces the same checks server-side as a defence
// in depth (the manifest path uses the package validator; the HTTP
// path uses the inlined rules below to keep the handler free of a
// gregalemanifest dependency cycle).
type CreateTriggerRequest struct {
	AppID         string          `json:"app_id"`
	Kind          TriggerKind     `json:"kind"`
	Slug          string          `json:"slug,omitempty"`
	Enabled       *bool           `json:"enabled,omitempty"`
	Config        json.RawMessage `json:"config,omitempty"`
	BatchSizeMax  *int            `json:"batch_size_max,omitempty"`
	BatchWindowMs *int            `json:"batch_window_ms,omitempty"`
	MaxAttempts   *int            `json:"max_attempts,omitempty"`
	// PayloadMaxBytes is the broker-payload byte cap per record.
	// nil → default 6291456 (6 MiB); the SQL CHECK rejects values
	// outside [1024, 67108864].
	PayloadMaxBytes *int `json:"payload_max_bytes,omitempty"`
	// BrokerPoisonStrategy is the kafka-only poison-record
	// handling strategy. nil → default "commit" (the previous
	// hardcoded behaviour; broker offset advances on poison).
	// Only meaningful for kind='kafka' triggers; the apid
	// handler ignores it for every other kind.
	BrokerPoisonStrategy *string `json:"broker_poison_strategy,omitempty"`

	// Cron-only fields.
	Schedule string `json:"schedule,omitempty"`
	Path     string `json:"path,omitempty"`
}

// UpdateTriggerRequest is a partial update. nil means "leave
// unchanged" — same semantics as UpdateCronRequest. Kind is
// NOT in this request — it is immutable (changing kind = creating
// a new resource + deleting the old one, which the customer has
// to do explicitly).
type UpdateTriggerRequest struct {
	Enabled              *bool           `json:"enabled,omitempty"`
	Config               json.RawMessage `json:"config,omitempty"`
	BatchSizeMax         *int            `json:"batch_size_max,omitempty"`
	BatchWindowMs        *int            `json:"batch_window_ms,omitempty"`
	MaxAttempts          *int            `json:"max_attempts,omitempty"`
	PayloadMaxBytes      *int            `json:"payload_max_bytes,omitempty"`
	BrokerPoisonStrategy *string         `json:"broker_poison_strategy,omitempty"`

	// Cron-only patches. Cron kinds accept schedule+path patches
	// (e.g. updating an existing cron); non-cron kinds reject
	// these (immutable slug field is the dedupe key for them).
	Schedule *string `json:"schedule,omitempty"`
	Path     *string `json:"path,omitempty"`
}

// TriggerRecord is the read-only audit row for one record passing
// through a trigger. Surfaced on GET /v1/triggers/{id}/records so the
// customer can answer "did my last N wake-ups succeed?". The wire
// shape includes the broker provenance (item_identifier, headers,
// metadata) so debugging is possible without direct DB access.
//
// State semantics (mirrors the SQL CHECK envelope at
// migrations/00267_triggers.sql):
//
//	"pending"     broker delivered, schedd has not picked it up
//	"claimed"     schedd FOR UPDATE SKIP LOCKED grabbed the row
//	"succeeded"   function returned 2xx; broker Acked
//	"retry"       function returned partial failure OR non-2xx;
//	              next_fire_at is in the future
//	"dead_letter" attempts == max_attempts OR poisoned; row in
//	              trigger_dead_letter
type TriggerRecord struct {
	ID               string     `json:"id"`
	TriggerID        string     `json:"trigger_id"`
	ItemIdentifier   string     `json:"item_identifier"`
	Payload          string     `json:"payload"`  // raw JSON, decoded lazily by the dashboard
	Headers          string     `json:"headers"`  // raw JSON
	Metadata         string     `json:"metadata"` // raw JSON
	State            string     `json:"state"`
	Attempts         int        `json:"attempts"`
	NextFireAt       time.Time  `json:"next_fire_at"`
	ReceivedAt       time.Time  `json:"received_at"`
	LastError        *string    `json:"last_error,omitempty"`
	LastDispatchedAt *time.Time `json:"last_dispatched_at,omitempty"`
}

// TriggerRecordRetryRequest (POST /v1/triggers/{id}/records/{rid}/retry)
// forces a record out of `retry` / `dead_letter` back into `pending`
// with attempts reset to 0. The dashboard surfaces this as a
// "Re-drive from DLQ" action. Operator-only scope is enforced at
// the route layer (operator scope, not deploy-write).
type TriggerRecordRetryRequest struct{}

// ListTriggerDeadLetterResponse answers GET /v1/triggers/{id}/dlq
// with the most recent rows from trigger_dead_letter (sort by
// created_at DESC, limit configurable via ?limit=, default 50).
type ListTriggerDeadLetterResponse struct {
	Records []TriggerDeadLetter `json:"records"`
}

// TriggerDeadLetter is the read-only wire shape for one row from the
// trigger_dead_letter table. `Detail` is opaque JSON — the kind
// decides the schema (broker-error vs poison-record vs rate-limited
// produce different detail shapes).
type TriggerDeadLetter struct {
	RecordID  string    `json:"record_id"`
	TriggerID string    `json:"trigger_id"`
	Reason    string    `json:"reason"`
	RoutedTo  string    `json:"routed_to"`
	Detail    string    `json:"detail"`
	CreatedAt time.Time `json:"created_at"`
}

// ListTriggerRecordsResponse answers GET /v1/triggers/{id}/records.
type ListTriggerRecordsResponse struct {
	Records []TriggerRecord `json:"records"`
}

// CreateTriggerBatchRequest is the optional inline-manifest path
// (POST /v1/triggers:batch_create). Lets the dashboard fire a
// gregale.yaml blob at the server without staging the tarball.
// Kept distinct from CreateTriggerRequest so the dashboard can
// differentiate "I'm adding one trigger" from "I'm applying a
// manifest".
//
// The handlers_manifest.go validator surface (commit #5) is wired
// here in commit #6 — the handler decodes, validates via
// validateManifestBytes (renamed wire bridge), then enumerates the
// triggers and creates them one at a time through the same store
// path as the per-trigger POST.
type CreateTriggerBatchRequest struct {
	AppID        string `json:"app_id"`
	ManifestYAML string `json:"manifest_yaml"`
}

// CreateTriggerBatchResult is one row of CreateTriggerBatchResponse.Created.
// Either `id` is set (created) or `error` is set (rejected) — never both.
// Mirrors the per-row shape in api/openapi.yaml:9562 + 9567-9576.
type CreateTriggerBatchResult struct {
	Slug  string  `json:"slug"`
	Kind  string  `json:"kind"`
	ID    *string `json:"id"`
	Error *string `json:"error,omitempty"`
}

// CreateTriggerBatchResponse answers POST /v1/triggers:batch_create.
// One row per trigger the manifest described, in the same order. A
// row carries either `id` (created) or `error` (rejected with an
// RFC 7807 code), never both — the dashboard renders the per-row
// "did this succeed?" inline. Rejections don't roll back successful
// rows in the same batch.
type CreateTriggerBatchResponse struct {
	Created []CreateTriggerBatchResult `json:"created"`
}

// TriggerMetricsResponse answers GET /v1/triggers/{id}/metrics.
// Aggregated counters keyed by state — not a Prometheus scrape (the
// /v1/metrics Prometheus surface is separate, issue #684).
type TriggerMetricsResponse struct {
	TriggerID       string `json:"trigger_id"`
	PendingCount    int    `json:"pending_count"`
	ClaimedCount    int    `json:"claimed_count"`
	SucceededCount  int    `json:"succeeded_count"`
	RetryCount      int    `json:"retry_count"`
	DeadLetterCount int    `json:"dead_letter_count"`
}
