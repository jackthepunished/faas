-- filename: 00043_app_runtime_go124_alpine.sql
-- +goose Up
-- +goose StatementBegin
-- Widen the apps.runtime CHECK to allow 'go124-alpine' (alpine/musl
-- variant of the go124 runtime) alongside the existing 'node22' /
-- 'python312' / 'go124'. The original inline CHECK
-- (migrations/00001_init.sql:35) was named apps_runtime_check by
-- PostgreSQL; the apid server-side whitelist in cmd/apid/handlers.go
-- and the BuildFramework enum in pkg/api/build.go are the matching
-- Go-side guards. There is no app-id-touching transition here —
-- existing rows with NULL or one of the older runtimes keep their
-- values, and the new constant is the only additive change.
--
-- The default base for runtime=go124 stays bookworm (glibc);
-- go124-alpine is opt-in via the runtime field on the function/app
-- manifest. No default-flip is performed in this PR.
--
-- Per the migrations contract (docs/adr/README.md, "migrations are
-- append-only and contiguous") this is additive: we DROP and re-ADD
-- the constraint rather than ALTER it in place. The down migration
-- reverses the change.
ALTER TABLE apps DROP CONSTRAINT apps_runtime_check;
ALTER TABLE apps ADD CONSTRAINT apps_runtime_check
  CHECK (runtime IS NULL OR runtime IN ('node22', 'python312', 'go124', 'go124-alpine'));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE apps DROP CONSTRAINT apps_runtime_check;
ALTER TABLE apps ADD CONSTRAINT apps_runtime_check
  CHECK (runtime IS NULL OR runtime IN ('node22', 'python312', 'go124'));
-- +goose StatementEnd