-- filename: 00278_triggers_payload_max.sql
-- +goose Up
-- +goose StatementBegin
--
-- 00278_triggers_payload_max.sql — per-trigger broker payload size cap
-- (audit finding #7 from PR #910).
--
-- Before: the dispatch tick accepted every broker-delivered record's
-- payload verbatim and shoved it through /v1/invocations:dispatch_batch.
-- A 6 MiB record (the JSON gateway's HTTP body cap) silently truncated
-- past byte 6 MiB with no failure signal — the function runner
-- received a sliced body and the trigger_records row carried no
-- evidence of the truncation. Dead-letter and retry counters never
-- ticked for these records.
--
-- After: each trigger carries an explicit payload_max_bytes column.
-- Records above the cap are DLQ'd at insert time with reason =
-- 'payload_too_large' (the existing trigger_dead_letter CHECK already
-- admits the value). 6 MiB is the default; the upper bound 64 MiB is
-- the largest plausible per-record broker payload (SQS max is 256 KiB,
-- Kafka default 1 MiB, NATS no formal cap but no broker in the cluster
-- advertises > 8 MiB). The lower bound 1 KiB guards against a
-- misconfiguration where the field accidentally lands at 0 and the
-- dispatcher rejects every record.
--
-- Cross-references: pkg/sched/dispatch_triggers.go::closeBatch
-- (the per-tick byte-cap enforcer) + pkg/api/limits.go
-- (the per-plan TriggerPayloadMaxBytes cap that this migration
-- surfaces).
--
-- Replay-safety: ADD COLUMN IF NOT EXISTS guards the apply path so
-- a drifted box (relation present, goose row missing) re-applies
-- without tripping SQLSTATE 42701. The DEFAULT 6291456 matches the
-- previous hardcoded cap in pkg/sched/dispatch_triggers.go::closeBatch
-- so existing rows behave identically after the migration lands.

ALTER TABLE triggers
    ADD COLUMN IF NOT EXISTS payload_max_bytes INT NOT NULL DEFAULT 6291456
        CHECK (payload_max_bytes BETWEEN 1024 AND 67108864);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE triggers DROP COLUMN IF EXISTS payload_max_bytes;
-- +goose StatementEnd