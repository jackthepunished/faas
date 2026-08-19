-- filename: 00296_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin

-- 00296_reserve_slot.sql — cross-PR slot reservation fence.
--
-- This slot is claimed by PR #986 (ADR-120 domain doctor,
-- 00296_domain_doctor_observations.sql). PR #910 (feat(triggers):
-- unified event-source-mapping primitive, issue #757 / ADR-100)
-- renumbered its 296/297/298 chain upward to 297/298/299 to
-- dodge this slot. The fence stays a no-op so
-- TestMigrationsContiguous sees a gap-free 1..N sequence on the
-- triggers-mega branch and the merge of #910 doesn't introduce a
-- slot gap that goose's strict findMissingMigrations would refuse
-- to apply. When PR #986 lands first, this fence should be
-- dropped (per cross-pr-slot-gate-fence-pattern); when PR #910
-- lands first, the fence stays as a held slot and PR #986's
-- chain renumbers past it on its next rebase.

SELECT 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
