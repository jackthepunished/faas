-- +goose Up
-- +goose StatementBegin
-- filename: 00288_deployments_annotation.sql
--
-- issue #977 / ADR-116 — deployment annotations. Adds four columns
-- to deployments:
--
--   reason       — free-form operator note (≤280 chars). Mirrors the
--                  example from issue #977: "Emergency rollback after
--                  payment provider incident". CHECK constrains length
--                  only; the prose accepts any printable text.
--
--   tag          — closed-set enumeration. Enables grouping /
--                  filtering without killing the prose use case.
--                  Vocabulary: incident_recovery | hotfix |
--                  scheduled_maintenance | compliance_hold |
--                  partner_request. Mirrors the deployments_tag_set
--                  CHECK precedent (parked_reason at migration 00157,
--                  scope at 00213).
--
--   deployed_by  — human-readable operator label. CLI auto-captures
--                  `git config user.name` when in a repo; githubd
--                  stamps `push.pusher.name`; the GitHub Action
--                  defaults the input to `${{ github.actor }}`. Never
--                  required (nullable text).
--
--   pr_number    — positive int when the wire offers it (githubd
--                  pull_request.number; Action
--                  `${{ github.event.pull_request.number }}`). Push
--                  events on a branch with no inferred PR leave NULL.
--                  CHECK constrains the value to be > 0; NULL is the
--                  "no PR" sentinel (D5 in ADR-116).
--
-- All four columns are nullable. Pre-feature rows stay valid (every
-- existing deployment has NULL annotation). The closed-set CHECKs
-- use the DROP+ADD widening shape (mirroring 00157's parked_reason
-- and 00264's scan_status) so the migration is replay-safe on a DB
-- that already has the columns but lost the goose version row
-- (the CD-job-2026-07-27 SQLSTATE 42701 tripwire from 00053's
-- docstring).

ALTER TABLE deployments
    ADD COLUMN IF NOT EXISTS reason      text,
    ADD COLUMN IF NOT EXISTS tag         text,
    ADD COLUMN IF NOT EXISTS deployed_by text,
    ADD COLUMN IF NOT EXISTS pr_number   int;

ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_reason_len_chk;
ALTER TABLE deployments
    ADD  CONSTRAINT deployments_reason_len_chk
        CHECK (reason IS NULL OR length(reason) <= 280);

ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_tag_set_chk;
ALTER TABLE deployments
    ADD  CONSTRAINT deployments_tag_set_chk
        CHECK (tag IS NULL OR tag IN
               ('incident_recovery', 'hotfix', 'scheduled_maintenance',
                'compliance_hold', 'partner_request'));

ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_pr_number_positive_chk;
ALTER TABLE deployments
    ADD  CONSTRAINT deployments_pr_number_positive_chk
        CHECK (pr_number IS NULL OR pr_number > 0);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE deployments DROP CONSTRAINT IF EXISTS deployments_pr_number_positive_chk;
ALTER TABLE deployments DROP CONSTRAINT IF EXISTS deployments_tag_set_chk;
ALTER TABLE deployments DROP CONSTRAINT IF EXISTS deployments_reason_len_chk;
ALTER TABLE deployments DROP COLUMN IF EXISTS pr_number;
ALTER TABLE deployments DROP COLUMN IF EXISTS deployed_by;
ALTER TABLE deployments DROP COLUMN IF EXISTS tag;
ALTER TABLE deployments DROP COLUMN IF EXISTS reason;
-- +goose StatementEnd
