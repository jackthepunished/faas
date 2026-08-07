# ADR-081 · Operator-managed egress CIDR bundle (issue #679 / PR-A)

- **Status:** accepted
- **Date:** 2026-08-07
- **Decision:** vmmd reads a TOML file at
  `/etc/faas/egress/operator_allowlist.toml` at startup and on
  `SIGHUP` and merges those CIDRs into every tenant's effective
  egress allowlist. Operators can ONLY ADD reachability, never
  subtract. The per-app validator cap (Pro=16, Scale=64) stays
  untouched — the bundle is additive on top of every per-app
  `apps.egress_allowlist`. Per-account additive cap is a separate
  concern (PR-B / ADR-082).

## Context

Issue #679 / G1: Pro/Scale customers routinely hit the
`apps.egress_allowlist` (cidr[]) wall at 16 / 64 entries — any
customer integrating with multi-vendor SaaS (analytics + error
tracking + payments + email + idempotency + feature flags + …)
gets blocked at the apid validator. Today the cap is hard-wired
to the plan; there's no operator escape hatch for curated
allowlists (e.g. a partner IP range that's the same for every
customer) and no per-account additive budget for one-off
overrides.

This ADR covers the operator-bundle leg (PR-A). The
per-account additive cap is PR-B / ADR-082 (separate PR,
separate schema column, separate change).

## The shape

**On-disk file:** flat TOML at the path
`EgressOperatorAllowlist` points to (default empty = disabled).
The file is root-only on the vmmd host. Reload is `kill -HUP $(pidof vmmd)`.

```toml
cidrs = [
    "203.0.113.0/24",   # partner VPC range
    "198.51.100.0/24",
    "2606:4700::/32",   # partner v6 range
]
```

**Loader** (`cmd/vmmd/egress_bundle.go`): reads, parses, sorts,
dedups. Per-entry errors (parse, /0, dup) are Warned and dropped
— a single bad entry does not poison the rest of the file. TOML
parse failure is a hard error (fail-loud so the operator
notices). Missing file is not an error (returns zero-value
bundle = "no operator additions"). Used both at startup and on
SIGHUP.

**Renderer surface:** unchanged. The per-app allowlist path
still renders via `pkg/netns.Config.ForwardAllowlistRule` /
`ForwardAllowlistRule6` (one inline anonymous-set rule per
family). The merged slice MUST partition cleanly by
`prefix.Addr().Is4()` — the loader does not partition, the
renderer does.

**Wake path** (`pkg/fcvm/manager.go`, after the per-request
ParsePrefix loop at line 1900-1912): append
`m.operatorBundleSnapshot()` to `nc.EgressAllowlist`. Order
matters: per-app first so an admin-set entry stays in its
expected position when the renderer's family partition reads the
slice.

**Live-patch path** (`pkg/fcvm/manager.go::UpdateEgressAllowlist`):
the per-app PATCH receives the per-app slice but the manager
injects the operator bundle before rendering. The `samePrefixSet`
fast-path at the top of the patch helper must compare the
**merged** slice against the live netns state — otherwise a
bundle change won't converge without a wake cycle.

**SIGHUP handler** (`cmd/vmmd/main.go`): the wire layer already
wires a SIGHUP-driven log-level reload in `pkg/wire/daemon.go`
(`watchLogLevelReload`). The egress watcher follows the same
shape — a dedicated goroutine in `cmd/vmmd/main.go` registered
via `signal.Notify(hupCh, syscall.SIGHUP)`. A failed reload
keeps the prior bundle live (best-effort; the daemon never
refuses to keep running on a bundle error — a missing or
malformed bundle just means the operator bundle is empty).

## Trust boundary

- **Who can edit:** root on the vmmd host. The bundle file is
  mode 0400/0440 vmmd-readable; warn on permissive modes but
  don't block — additive to the per-app validator's already-strict
  gate, so a broad chmod can't subtract.
- **Distribution:** ansible role deploys the file via the existing
  vmmd config sync (no new transport).
- **No API surface:** a compromised tenant cannot influence the
  bundle. Same shape as the vmmd config itself.
- **/0 contract:** a single `0.0.0.0/0` or `::/0` entry is rejected
  at the loader, at the dedup helper, and at the per-app parser
  — same three-layer gate the `apps_egress_allowlist_cidr`
  ADR-032 contract relies on.

## Why not just widen the per-app cap

Three regressions:

1. The cap is plan-gated for a reason (financial model §1 —
  predictable bills). A static-bump to 64/256/1024 by plan
  favours customers who don't need it.
2. Per-app CIDRs are tenant-curated. Operator-curated CIDRs
  (partner VPC ranges, regulatory allowlists) belong to a
  different namespace — operators can vouch for them, tenants
  cannot.
3. Operators need to be able to add a CIDR to every tenant
  without coordinating with each one ("your app now needs to
  reach X").

The additive merge keeps the per-app cap authoritative for the
**default** case and gives operators a knob for curated
allowlists.

## Verification

- `migrations/00155_apps_websocket_enabled.sql` is the last
  schema migration; PR-A is schema-free.
- TOML loader tests (`cmd/vmmd/egress_bundle_test.go`): valid /
  missing / empty / malformed / per-entry-error / /0 / dedup /
  all-rejected.
- Merge + dedup tests (`pkg/fcvm/manager_egress_merge_test.go`):
  empty = no-op, append+dedup, empty+empty, /0 defence, order
  preservation.
- SIGHUP reload tests (`cmd/vmmd/egress_bundle_reload_test.go`):
  happy-path, multiple hups, empty-path, malformed-TOML keeps
  prior, ctx.Done exits cleanly.
- Regression: `pkg/netns/...` (render path), `pkg/fcvm/...`
  (`UpdateEgressAllowlist`), `cmd/apid/...` (per-app validator).

## Out of scope

- Per-account additive cap (PR-B / ADR-082; migration slot 156).
- HTTP/3 / QUIC (issue #680).
- Snapshot de-localization (issue #681).
- Services tier (issue #677).
- ADR-052 mTLS roll-out (issue #678).
- Plan-keyed ceiling on `extra` (Pro ≤1024, Scale ≤4096) — flat
  1024 for now, plan-key later if Scale customers ask.
- Cross-tenant CIDR de-duplication at the daemon level (the
  renderer's inline anonymous-set already handles per-netns
  duplication).
