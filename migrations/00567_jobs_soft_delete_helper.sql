-- +goose Up
-- +goose StatementBegin
-- Soft-delete helper for DELETE /v1/jobs/{name}. Soft-delete is the
-- customer-facing path; hard delete is rejected by the
-- instances.job_id RESTRICT FK (00527).
--
-- The helper refuses to flip status='deleted' while ANY live
-- (non-parked / non-destroyed) job_task instance exists for the job.
-- Returns TRUE if it actually flipped a row; FALSE if either the job
-- is missing OR a live instance exists (apid maps FALSE → 409
-- CodeJobHasLiveInstances).

CREATE OR REPLACE FUNCTION soft_delete_job_if_no_live_instances(p_job_id uuid)
    RETURNS boolean
    LANGUAGE plpgsql
AS $$
DECLARE
    flipped boolean;
BEGIN
    UPDATE jobs
       SET status     = 'deleted',
           updated_at = now()
     WHERE id = p_job_id
       AND status <> 'deleted'
       AND NOT EXISTS (
           SELECT 1
             FROM instances
            WHERE job_id = p_job_id
              AND kind   = 'job_task'
              AND state NOT IN ('parked', 'destroyed')
       )
    RETURNING TRUE INTO flipped;

    RETURN COALESCE(flipped, FALSE);
END;
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS soft_delete_job_if_no_live_instances(uuid);
-- +goose StatementEnd
