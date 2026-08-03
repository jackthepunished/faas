// Tests for pkg/billing/billable.go (issue #463 / ADR-070 / PR-C
// sidecar-aware billing helper).
//
// The mirror itself is one-line; the test pins the integer
// arithmetic so a future per-sidecar rounding rule (e.g. drop
// zero-ram entries) only has to land in pkg/api and pkg/billing
// together — both wrappers collapse to the same number.

package billing_test

import (
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/billing"
)

func TestBillableRAMMBWithSidecars_MirrorsAPI(t *testing.T) {
	cases := []struct {
		name        string
		ramMB       int
		sidecarMBs  []int
	}{
		{"no-sidecars", 256, nil},
		{"no-sidecars-empty", 256, []int{}},
		{"one-sidecar", 256, []int{64}},
		{"two-sidecars", 256, []int{64, 32}},
		{"zero-ram-sidecar-preserved", 256, []int{0}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := billing.BillableRAMMBWithSidecars(c.ramMB, c.sidecarMBs)
			want := api.BillableRAMMBWithSidecars(c.ramMB, c.sidecarMBs)
			if got != want {
				t.Errorf("billing.BillableRAMMBWithSidecars(%d, %v) = %d; want %d (must mirror api)",
					c.ramMB, c.sidecarMBs, got, want)
			}
		})
	}
}
