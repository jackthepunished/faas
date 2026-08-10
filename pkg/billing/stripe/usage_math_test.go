package stripe

// usage_math_test.go used to own TestWireQuantityForMBSeconds,
// TestWireQuantityForMBSeconds_HundredTBResidentDay,
// TestWireQuantityForMBSeconds_NeverNegativeMathResult, and
// TestWireQuantityConstants. The constants and helper moved to
// pkg/billing/plans.go so both providers (Stripe + Paddle) share
// the same integer-money formula (PR-P-fixes — closes the
// Paddle under-billing bug where defaultFlushLocked hard-coded
// Quantity=1 and silently under-billed every account by ~250×
// for the canonical Hobby 24h case).
//
// The shared tests now live in pkg/billing/plans_test.go. This
// file keeps TestLegacyWireQuantityForGBHours_BitIdentical
// because the legacy float wire path is stripe-specific
// (Paddle never had it) — the formula `int64(gbHours * 1000)`
// is pinned against the shared WireQuantityMillicentsPerGBHour
// constant.

import (
	"testing"
)

// TestLegacyWireQuantityForGBHours_BitIdentical pins the deprecated
// path's contract: it MUST be bit-identical to
// `int64(gbHours * 1000)`. TestInvoiceShadow24h_Sandbox (live
// Stripe) and the existing legacy callers rely on this exact
// formula — a future refactor that accidentally routes through
// the integer path's division would silently under-bill every
// customer on the legacy wire path.
//
// The reference answer is computed inline as
// `int64(gbHours * WireQuantityMillicentsPerGBHour)` so the test
// is self-documenting: there's no "expected" value to mis-transcribe,
// and a future refactor that changes the inline reference
// expression is caught by code review rather than a magic-number
// discrepancy.
func TestLegacyWireQuantityForGBHours_BitIdentical(t *testing.T) {
	cases := []struct {
		name    string
		gbHours float64
	}{
		// Floor.
		{name: "zero", gbHours: 0},
		// 0.001 GB-h (one MB-resident-hour) → 1 wire unit.
		// The cleanest alignment of the legacy formula.
		{name: "one_mb_hour", gbHours: 0.001},
		// 6.187 GB-h → 6187 wire units. Same number as the
		// integer path's canonical 24h Hobby case, but here
		// the floating-point representation is the only
		// thing that determines the answer.
		{name: "hobby_24h_legacy", gbHours: 6.187},
		// Below the sub-milliunit truncation floor: 0.0009 →
		// 0.9 → int64 truncates to 0.
		{name: "below_floor", gbHours: 0.0009},
		// A non-round number that exercises the float→int64
		// cast at a value where the bit pattern matters.
		// 0.006187 → 6.187 → 6187 (the same canonical qty,
		// but at a different magnitude).
		{name: "non_round", gbHours: 0.006187},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := legacyWireQuantityForGBHours(tc.gbHours)
			// Reference: the literal formula the function
			// is supposed to be a wrapper for. If anyone
			// "improves" the test by replacing this with a
			// magic number, the inline comment is the
			// prompt that explains what the number is.
			want := int64(tc.gbHours * float64(WireQuantityMillicentsPerGBHour))
			if got != want {
				t.Fatalf("legacyWireQuantityForGBHours(%v) = %d, "+
					"want %d (bit-identical to int64(gbHours * 1000))",
					tc.gbHours, got, want)
			}
		})
	}
}
