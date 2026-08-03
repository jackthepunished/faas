// Package billing — issue #463 / ADR-070 / PR-C sidecar-aware
// billing helper. Mirrors api.BillableRAMMBWithSidecars so
// pkg/billing/stripe and pkg/billing/paddle can compute the
// billable RAM without dragging the per-plan math into the
// provider packages. The provider-specific packages post integer
// cent/millicent values to Stripe/Paddle; this helper ensures the
// "Sidecar RAM" line in the financial-model addendum reaches the
// provider in the same arithmetic the schedd ledger enforces.
//
// Why a mirror (not a re-export): pkg/billing/stripe already imports
// pkg/billing; pkg/billing can import pkg/api without a cycle. The
// wrapper is a one-line pass-through today and exists so a future
// billing-only adjustment (e.g. a per-sidecar rounding rule) can
// live in pkg/billing without touching pkg/api.
package billing

import "github.com/onebox-faas/faas/pkg/api"

// BillableRAMMBWithSidecars is the sidecar-shape variant of the
// per-instance RAM arithmetic for billing (issue #463 / ADR-070
// §Decision 6). The shutter is
//
//	plan.RAMMB + Σ(sidecar.ram_mb) + api.PerVMOverheadMB
//
// where:
//
//   - plan.RAMMB is the plan default (Free 128 / Hobby 256 / Pro 512
//     / Scale 1024).
//   - sidecar.ram_mb is each entry's value from the deployment's
//     `sidecars jsonb` column (cap: 2, enforced upstream).
//   - api.PerVMOverheadMB is the per-instance +8 MB baseline that
//     covers the shared netns, the cgroup scope, and the
//     single-VM-overhead that sidecars inherit (no per-sidecar
//     duplication).
//
// PR-C is the first consumer: pkg/meter/sampler.go::sampleAppAndLive.
// pkg/billing/stripe's UsageByHour builder is the second consumer
// (it posts the past day's mb_seconds to Stripe as a metered usage
// record). Both call sites use this helper, never the no-sidecar
// form, so a future per-sidecar rounding rule (e.g. drop zero-ram
// entries) only has to land here.
//
// The cap on the sidecar count is enforced upstream in
// api.Sidecar.Validate (request boundary) and the schema CHECK on
// `deployments.sidecars` (migration 00095 + 00118). This helper
// trusts len(sidecarMBs) ≤ api.SidecarCapMax = 2 and never
// re-checks — duplicating the cap here would split the truth
// between pkg/api and pkg/billing.
func BillableRAMMBWithSidecars(ramMB int, sidecarMBs []int) int {
	return api.BillableRAMMBWithSidecars(ramMB, sidecarMBs)
}
