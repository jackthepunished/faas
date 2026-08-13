# ADR-100 PR-cluster outline

Four reviewable PRs, each ships behind feature flags where it touches
runtime behaviour, each independently reviewable in ~10 min. Mirrors
the ADR-090 and ADR-098 cluster outline conventions. Cross-PR gates
defined per [`docs/adr/098-pr-cluster-outline.md`](./098-pr-cluster-outline.md).

## Cluster shape

| PR | Theme | Files touched | Behaviour change | Review budget |
|---|---|---|---|---|
| PR-0 | Docs + slot fence + limits + CLI typo | `docs/adr/100-tenant-surfaces.md` (new), `docs/adr/100-pr-cluster-outline.md` (new), `docs/adr/README.md`, `docs/faas_implementation_spec.md`, `migrations/00238_reserve_slot.sql` … `migrations/00243_reserve_slot.sql` (new), `pkg/api/limits.go`, `pkg/api/limits_test.go`, `cmd/gregale/commands2.go` | None at runtime; CLI typo fix only | ~10 min |
| PR-A | Schema + state surface + cert engine | `migrations/00243_tenant_surfaces.sql`, `migrations/00243_tenant_surfaces_test.go`, `pkg/state/{types.go,store.go,pgstore.go,memstore.go}`, `pkg/gateway/cert_issuer.go`, `pkg/gateway/cert_expiry.go`, `pkg/db/notify.go`, `pkg/api/errors.go`, `pkg/api/dto.go` | Tables exist; cert engine dark until PR-C | ~10 min |
| PR-B | Surface parser + routing layer extension | `pkg/gateway/surface_parser.go`, `pkg/gateway/surface_parser_test.go`, `cmd/gatewayd-internal/backend.go` | `pgRouter.ResolveHost` gains the surface branch | ~10 min |
| PR-C | HTTP API + CLI + E2E + adjacent fixes | `pkg/api/dto.go` (extend), `pkg/api/client.go`, `cmd/apid/server.go`, `cmd/apid/handlers_tenant_surfaces.go` (new), `cmd/apid/handlers_ext.go`, `cmd/apid/dns_poller.go`, `api/openapi.yaml`, `pkg/apid/openapi.yaml`, `cmd/gregale/commands_tenant_surfaces.go` (new), `cmd/sdk-coverage/main.go`, `sdk/go/internal/api/client.go`, `sdk/node/src/generated/services/TenantSurfacesService.ts`, `sdk/python/faas_sdk/api/tenant_surfaces/`, `cmd/e2e/tenant_surfaces_e2e_test.go`, `pkg/apid/audit.go` | Surfaces become operator-/customer-facing | ~15 min (wider footprint; spec-sync + sdk-gen) |

## Slot numbering (illustrative — re-check before claiming)

- PR-0 fences: 00238, 00239, 00240, 00241, 00242, 00243 (six co-fences)
- PR-A real migration lands at 00243, replacing the fence at the same slot

## Cross-PR gates

1. ✅ `make lint` + `make test` + `make spec-check` after every PR.
2. ❌ `make metal-lima` — N/A (no `pkg/fcvm` / `pkg/netns` / `vmmd` / `builderd` touch).
3. ✅ Every new quota lives in `pkg/api/limits.go` — no inline numbers.
4. ✅ Owner separation holds — apid writes to surfaces, gatewayd reads via `pg_notify`.
5. ✅ Slot fence discipline — precheck before any migration addition.
6. ✅ `make spec-sync` after `api/openapi.yaml` changes (PR-C only).
7. ✅ SDK regen (`make sdk-gen-node`, `make sdk-gen-python`) clean (PR-C only).

## References

- ADR-100 [`100-tenant-surfaces.md`](./100-tenant-surfaces.md).
- Issue [#879](https://github.com/poyrazK/faas/issues/879).
- Cluster outline template [`098-pr-cluster-outline.md`](./098-pr-cluster-outline.md).
