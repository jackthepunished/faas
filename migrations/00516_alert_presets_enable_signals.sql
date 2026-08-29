-- filename: 00516_alert_presets_enable_signals.sql
-- Issue #1233 / ADR-123 PR-B — flip the 5 "Coming soon" catalog rows
-- to enabled_in_catalog=true. Their backing signals were plumbed in
-- PR-A: meterd_account_spend_eur + apid_tenant_surface_cert_expiry_seconds
-- + gateway_queue_depth are wired; meterd_api_reachable and
-- apid_deployment_failed_total land as gauge/counter registrations in
-- this same PR.
--
-- Replay-safety: WHERE enabled_in_catalog = false makes a re-apply a
-- clean no-op. The symmetric Down flips the 5 rows back to false if
-- a rollback is required.

-- +goose Up
-- +goose StatementBegin
UPDATE alert_presets
SET enabled_in_catalog = true,
    updated_at = now()
WHERE enabled_in_catalog = false
  AND name IN (
      'api_down',
      'spend_eur_20',
      'deploy_failed',
      'cert_expiring_14d',
      'queue_backlog_growing'
  );
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
UPDATE alert_presets
SET enabled_in_catalog = false,
    updated_at = now()
WHERE name IN (
    'api_down',
    'spend_eur_20',
    'deploy_failed',
    'cert_expiring_14d',
    'queue_backlog_growing'
);
-- +goose StatementEnd
