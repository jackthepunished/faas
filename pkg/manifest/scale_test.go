package manifest

import (
	"fmt"
	"strings"
	"testing"
)

func TestFleetValidateComputeNodeScale(t *testing.T) {
	for _, tc := range []struct {
		name        string
		compute     int
		wantInvalid bool
	}{
		{name: "one", compute: 1},
		{name: "ten", compute: 10},
		{name: "one hundred", compute: 100},
		{name: "supported ceiling", compute: MaxComputeNodes},
		{name: "above supported ceiling", compute: MaxComputeNodes + 1, wantInvalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hosts := make([]Host, 0, tc.compute+1)
			hosts = append(hosts, Host{Name: "control-plane", Role: "control-plane"})
			for i := 1; i <= tc.compute; i++ {
				hosts = append(hosts, Host{
					Name: fmt.Sprintf("compute-%04d", i),
					Role: "compute-only",
				})
			}
			fleet := Fleet{Hosts: hosts}
			if got := fleet.ComputeNodeCount(); got != tc.compute {
				t.Fatalf("ComputeNodeCount() = %d, want %d", got, tc.compute)
			}

			errs := fleet.validate()
			if tc.wantInvalid {
				if len(errs) == 0 || !strings.Contains(errs.Error(), "maximum supported") {
					t.Fatalf("fleet.validate() = %v, want maximum-scale error", errs)
				}
				return
			}
			if errs != nil {
				t.Fatalf("fleet.validate() = %v, want valid", errs)
			}
		})
	}
}
