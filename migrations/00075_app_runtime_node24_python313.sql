-- filename: 00075_app_runtime_node24_python313.sql
-- +goose Up
-- +goose StatementBegin
-- Widen the apps.runtime CHECK to also allow 'node24' (Node 24 LTS) and
-- 'python313' (Python 3.13, RHEL/Fedora default). Mirrors the prior widening
-- pattern in migrations/00043_app_runtime_go124_alpine.sql: DROP-and-re-ADD
-- is the contract because CHECK constraints cannot be altered in place and
-- the apid handler-side whitelist plus the imaged runtime matrix must move
-- in lockstep with this widening (cross-checked in pkg/imaged/handler_test.go
-- matrix rows and cmd/apid/handlers_test.go TestCreateApp_*).
--
-- The default base for runtime=node22 / runtime=python312 stays as-is. No
-- default-flip is performed in this PR; node22 remains the default for
-- new function apps. PR 2 (auto-stage runtime bases) registers the matching
-- entries in imaged/base_stage.go::DefaultRuntimeBaseRefs so the runtime
-- bases are pulled on first boot instead of being staged by hand.
--
-- Existing rows with NULL or one of the older runtime values are untouched.
ALTER TABLE apps DROP CONSTRAINT apps_runtime_check;
ALTER TABLE apps ADD CONSTRAINT apps_runtime_check
  CHECK (runtime IS NULL OR runtime IN ('node22', 'python312', 'go124', 'go124-alpine', 'node24', 'python313'));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Reverse: narrow the runtime CHECK back to the 00043 set. Apps created at
-- runtime='node24' or runtime='python313' between this migration's apply
-- and a downgrade become the constraint violators and force the downgrade
-- to fail with 23514; that's the desired safety (a downgrade should never
-- silently delete runtime data, but the CHECK drop-and-re-ADD will reject
-- the narrower re-add before any row is touched).
ALTER TABLE apps DROP CONSTRAINT apps_runtime_check;
ALTER TABLE apps ADD CONSTRAINT apps_runtime_check
  CHECK (runtime IS NULL OR runtime IN ('node22', 'python312', 'go124', 'go124-alpine'));
-- +goose StatementEnd
