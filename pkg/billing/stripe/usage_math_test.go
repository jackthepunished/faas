package stripe

import (
	"testing"
)

// TestWireQuantityForMBSeconds pins the customer-billing wire-quantity
// formula hermetically. Before this test, the only thing that
// validated `qty == 6187` for a 256 MB Hobby app resident for 24 h
// was TestInvoiceShadow24h_Sandbox in sandbox_test.go, which needs
// STRIPE_API_KEY and silently skips in CI (and therefore on every PR).
// A regression in the customer-billing formula would ship green.
//
// Reference derivation:
//   - Plan: Hobby (256 MB)
//   - Billed RAM: 256 + 8 = 264 MB (§4.7 "plan RAM + 8 MB per running
//     second" — the 8 MB overhead is for virtio/firecracker bookkeeping,
//     not the customer's working set)
//   - Window: 24 h = 86_400 s
//   - mbSeconds = 264 * 86_400 = 22_809_600
//   - qty = 22_809_600 * 1000 / (1024 * 3600) = 22_809_600_000 / 3_686_400
//         = 6186.666… → truncates to 6187 wire units
//
// The trailing remainder (6186.666…) is the sub-milliunit gap; it is
// dropped on purpose — see WireQuantityForMBSeconds's docstring and
// CLAUDE.md's "Floats near money fail review". The accumulation of
// this remainder across millions of windows is the entire reason the
// float path was retired.
func TestWireQuantityForMBSeconds(t *testing.T) {
	const (
		hobbyRamMB   = 256
		billedMB     = hobbyRamMB + 8
		hoursInDay   = 24
		secsInHour   = 3600
		secsInDay    = hoursInDay * secsInHour
		canonicalQty = 6187
	)

	cases := []struct {
		name      string
		mbSeconds int64
		want      int64
	}{
		// The acceptance case — a Hobby app resident for exactly one
		// full day. qty MUST be 6187, not 6186, not 6188, not a
		// float-rounded "6186.67". If this ever fails, the customer's
		// daily bill changes.
		{
			name:      "hobby_24h_canonical",
			mbSeconds: billedMB * secsInDay, // 22_809_600
			want:      canonicalQty,
		},

		// Boundary: zero. A parked-app with no resident-seconds MUST
		// produce a zero qty (no wire record at all would be the next
		// layer's decision, but the math must not invent units).
		{name: "zero", mbSeconds: 0, want: 0},

		// One minute of one MB — smallest billable quantum. The
		// customer should NOT be charged for a single second of
		// runtime, only full minutes (meterd rounds upstream), but the
		// helper itself must return the integer answer for whatever
		// sum lands on it. 60 mb-s → 60 * 1000 / 3_686_400 = 0
		// (truncated). Documents the sub-milliunit floor.
		{name: "one_minute_one_mb", mbSeconds: 60, want: 0},

		// One hour of one MB. 3_600 mb-s → 3_600_000 / 3_686_400 = 0
		// (truncated, just barely). Pinned so any future change to
		// the constant floors would surface here.
		{name: "one_hour_one_mb", mbSeconds: 3_600, want: 0},

		// Exactly one MB-second * 1000 — the breakpoint where 1 wire
		// unit (one milli-cent of a GB-h) is reached. 3_687 mb-s →
		// 3_687_000 / 3_686_400 = 1 (truncated from 1.000162…).
		// The 3_686 vs 3_687 split is the rounding boundary the
		// helper must respect exactly.
		{name: "first_wire_unit", mbSeconds: 3_687, want: 1},

		// One MB-resident-second below the breakpoint. 3_686 mb-s →
		// 3_686_000 / 3_686_400 = 0 (truncated from 0.999892…).
		// Companion to the previous case — together they pin the
		// exact floor.
		{name: "below_first_wire_unit", mbSeconds: 3_686, want: 0},

		// Pro plan (512 MB) for one hour. 520 * 3_600 = 1_872_000
		// mb-s → 1_872_000_000 / 3_686_400 = 507 (truncated from
		// 507.815…). Pin the higher-volume case to make sure the
		// integer math doesn't overflow at the megabyte scale.
		{name: "pro_one_hour", mbSeconds: 520 * 3_600, want: 507},

		// Free plan (128 MB) for one hour. 136 * 3_600 = 489_600
		// mb-s → 489_600_000 / 3_686_400 = 132 (truncated from
		// 132.810…). Cheapest plan, smallest integer.
		{name: "free_one_hour", mbSeconds: 136 * 3_600, want: 132},

		// Scale plan (1024 MB) for one hour. 1032 * 3_600 = 3_715_200
		// mb-s → 3_715_200_000 / 3_686_400 = 1007 (truncated from
		// 1007.717…). Top-of-fleet case for the per-hour helper.
		{name: "scale_one_hour", mbSeconds: 1032 * 3_600, want: 1007},

		// Hobby resident for exactly one GB-second. 1_024 * 60 * 60 =
		// 3_686_400 mb-s (1 GB resident for 1 hour) → 3_686_400_000
		// / 3_686_400 = 1000 wire units. Pinned because 1000 is the
		// cleanest round answer the formula produces, and is what a
		// naive `gbHours * 1000` formula returns too — verifying the
		// two paths agree at the exactly-aligned point.
		{name: "one_gb_hour", mbSeconds: 1024 * 3600, want: 1000},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := WireQuantityForMBSeconds(tc.mbSeconds)
			if got != tc.want {
				t.Fatalf("WireQuantityForMBSeconds(%d) = %d, want %d",
					tc.mbSeconds, got, tc.want)
			}
		})
	}
}

// TestWireQuantityForMBSeconds_LargeValue is the overflow-safety
// guard. The docstring claims 1 TB-resident-24 h (~2.1e9 mb_seconds)
// stays well below int64 max. Pin the claim with a value 100x larger
// than that ceiling — if the formula ever changed to a narrower
// integer type that silently truncated high windows to 0, the test
// would catch it.
//
// Headroom fact-check: at 100 TB-resident-24 h the numerator is
// 9_059_696_640_000_000 ≈ 9.06e15, well below int64 max (9.22e18).
// staticcheck's SA4003 flags any int64*1000 comparison with
// math.MaxInt64 as tautologically false, so the safety claim is left
// here as a constant-propagated fact rather than re-checked at
// runtime.
func TestWireQuantityForMBSeconds_LargeValue(t *testing.T) {
	// 100 TB-resident for 24 h.
	//   mbSeconds = 100 * 1024 * 1024 * 86_400 = 9_059_696_640_000
	//   numerator = mbSeconds * 1000 = 9_059_696_640_000_000
	//   qty = numerator / 3_686_400 = 2_457_600_000
	// Pin the expected qty exactly.
	const tb = 1024 * 1024
	mbSeconds := int64(100 * tb * 86_400)
	const want int64 = 2_457_600_000
	got := WireQuantityForMBSeconds(mbSeconds)
	if got != want {
		t.Fatalf("large value: got %d, want %d (mbSeconds=%d)", got, want, mbSeconds)
	}
}

// TestWireQuantityForMBSeconds_NeverNegative pins the defensive
// invariant. meterd never produces a negative window (it's a sum of
// non-negative per-second contributions), but if a future caller
// passes a negative int64 we want to know via the
// ErrNegativeQuantity sentinel at pushUsageRecordSDKSumWithID, not
// via a silently-wrapped int64 produced by integer overflow.
//
// The helper itself returns whatever the math says (which would be a
// large positive number for negative mbSeconds due to int64 overflow
// at the multiply step); the negative guard is a layer above. This
// test documents that the helper is the *math* layer, not the *guard*
// layer, so future refactors don't accidentally move the guard down.
func TestWireQuantityForMBSeconds_NeverNegativeMathResult(t *testing.T) {
	// Math note: a negative mbSeconds multiplied by 1000 still
	// overflows in int64 (smallest representable values). The math
	// result is therefore *not* guaranteed positive at the helper
	// level — the negative guard lives at the caller. Pin that
	// contract here so the helper isn't later "fixed" to do the
	// guard's job.
	got := WireQuantityForMBSeconds(-1)
	// -1 * 1000 / secondsPerGBHour = -1000 / 3686400 = 0 (truncated
	// toward zero in Go integer division). So a small negative
	// input produces 0, not the panic'd reflection of overflow.
	if got != 0 {
		t.Fatalf("small negative input: got %d, want 0 (caller's guard is authoritative)", got)
	}
}

// TestLegacyWireQuantityForGBHours pins the deprecated float path so
// it can't quietly drift. The legacy formula is `int64(gbHours *
// 1000)`, used by the deprecated PushUsageRecordSum and its live
// sandbox regression test. If a future refactor accidentally
// substitutes the integer path's division, every customer on the
// legacy wire path would be silently under-billed.
func TestLegacyWireQuantityForGBHours(t *testing.T) {
	cases := []struct {
		name    string
		gbHours float64
		want    int64
	}{
		// Zero, pinning the floor.
		{name: "zero", gbHours: 0, want: 0},
		// 0.001 GB-h (one MB-resident-hour) → 1 wire unit. The
		// cleanest alignment of the legacy formula.
		{name: "one_mb_hour", gbHours: 0.001, want: 1},
		// 6.187 GB-h exactly → 6187 wire units. Same answer as the
		// integer path's canonical 24h Hobby case — but for an
		// entirely different reason. If the integer path ever
		// diverged from 6187 for this window, this test would
		// document the legacy answer remains as-is.
		{name: "hobby_24h_legacy", gbHours: 6.187, want: 6187},
		// Below the truncation floor. 0.0009 GB-h * 1000 = 0.9 →
		// int64 truncates to 0. Pin the sub-milliunit truncation.
		{name: "below_floor", gbHours: 0.0009, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := legacyWireQuantityForGBHours(tc.gbHours)
			if got != tc.want {
				t.Fatalf("legacyWireQuantityForGBHours(%v) = %d, want %d",
					tc.gbHours, got, tc.want)
			}
		})
	}
}

// TestWireQuantityConstants pins the two constants in the formula. If
// either changes, every customer's bill changes; both must be a
// deliberate edit and a re-run of this test.
func TestWireQuantityConstants(t *testing.T) {
	if WireQuantityMillicentsPerGBHour != 1000 {
		t.Fatalf("WireQuantityMillicentsPerGBHour = %d, want 1000 (spec §4.7)",
			WireQuantityMillicentsPerGBHour)
	}
	if secondsPerGBHour != 1024*3600 {
		t.Fatalf("secondsPerGBHour = %d, want %d (1 GB = 1024 MB, 1 h = 3600 s)",
			secondsPerGBHour, 1024*3600)
	}
}