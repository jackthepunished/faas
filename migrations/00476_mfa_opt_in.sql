-- filename: 00476_mfa_opt_in.sql
-- Issue #186 follow-up: make customer MFA genuinely opt-in.
--
-- Earlier versions armed mfa_required from plan upgrades, billing
-- subscription events, and the second deployment. Those lifecycle
-- triggers have been removed from apid. Clear their remaining
-- unenrolled flags so existing customers are not still forced through
-- enrollment after the code change. Enrolled accounts are untouched:
-- their confirmed authenticator remains the opt-in state and continues
-- to require verification on new dashboard sessions.
--
-- This is replay-safe because the update is idempotent. A down migration
-- cannot reconstruct which rows were armed by the old triggers, so it is
-- intentionally a no-op rather than guessing and re-enabling MFA.

-- +goose Up
-- +goose StatementBegin
UPDATE accounts
   SET mfa_required = false
 WHERE mfa_required = true
   AND mfa_enrolled_at IS NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
