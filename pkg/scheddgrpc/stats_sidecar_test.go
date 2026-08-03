// Tests for the SidecarMBs wire field on InstanceStatsRow
// (issue #463 / ADR-070 §Decision 6 / PR-C). Pins the round-trip
// from int-sidecar-MB slice to []int32 proto to []int client mirror
// so a future wire-discipline change is caught here.

package scheddgrpc_test

import (
	"context"
	"testing"

	scheddpb "github.com/onebox-faas/faas/api/proto/onebox/faas/schedd/v1"
	"github.com/onebox-faas/faas/pkg/sched/instancestats"
)

// TestInstanceStatsRow_SidecarMBs_RoundTrip pins the wire shape:
// the server populates SidecarRamMbs on the proto when the
// source row's SidecarMBs is non-empty, and omits the field
// entirely (nil repeated) when it is empty/nil so the wire stays
// compact for the no-sidecar case.
func TestInstanceStatsRow_SidecarMBs_RoundTrip(t *testing.T) {
	cases := []struct {
		name string
		mbs  []int
	}{
		{"nil-no-sidecars", nil},
		{"empty-no-sidecars", []int{}},
		{"one-sidecar", []int{64}},
		{"two-sidecars", []int{64, 32}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rowIn := instancestats.InstanceStat{
				InstanceID: "vm-A",
				AppID:      "app-1",
				NodeID:     "node-1",
				SidecarMBs: c.mbs,
			}
			stats := &fakeStatsReader{rows: []instancestats.InstanceStat{rowIn}}
			cli := newServerWithStats(t, stats)

			resp, err := cli.ListInstanceStats(context.Background(),
				&scheddpb.ListInstanceStatsRequest{})
			if err != nil {
				t.Fatalf("ListInstanceStats: %v", err)
			}
			if got := len(resp.GetRows()); got != 1 {
				t.Fatalf("rows = %d; want 1", got)
			}
			got := resp.GetRows()[0].GetSidecarRamMbs()
			if len(got) != len(c.mbs) {
				t.Fatalf("SidecarRamMbs len = %d; want %d", len(got), len(c.mbs))
			}
			for i, v := range got {
				if int(v) != c.mbs[i] {
					t.Errorf("SidecarRamMbs[%d] = %d; want %d", i, v, c.mbs[i])
				}
			}
		})
	}
}
