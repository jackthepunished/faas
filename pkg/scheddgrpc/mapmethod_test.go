// White-box test for the unexported mapMethod (server.go:165-170).
// Lives in `package scheddgrpc` (not scheddgrpc_test) because
// mapMethod is unexported and the wire-shape tests in
// bufconn_test.go can't reach it directly. The function translates
// vmmdpb.WakeMethod to scheddpb.WakeMethod, with a default branch
// that maps any unknown enum to COLD_BOOT. That default branch
// is the load-bearing one: a future vmmd WakeMethod addition that
// forgets to add a case in mapMethod silently maps to COLD_BOOT,
// which is the *safer* wrong answer (cold boot is slower but
// always works) but still wrong. This test pins both branches.
package scheddgrpc

import (
	"testing"

	scheddpb "github.com/onebox-faas/faas/api/proto/onebox/faas/schedd/v1"
	vmmdpb "github.com/onebox-faas/faas/api/proto/onebox/faas/vmmd/v1"
)

func TestMapMethod(t *testing.T) {
	cases := []struct {
		name string
		in   vmmdpb.WakeMethod
		want scheddpb.WakeMethod
	}{
		{"restore maps to restore", vmmdpb.WakeMethod_WAKE_RESTORE, scheddpb.WakeMethod_WAKE_RESTORE},
		{"cold boot maps to cold boot", vmmdpb.WakeMethod_WAKE_COLD_BOOT, scheddpb.WakeMethod_WAKE_COLD_BOOT},
		// Unknown enum (a future vmmd addition that drifts from the
		// schedd switch) defaults to cold boot. This is the
		// "if either enum drifts" defense the comment on
		// server.go:163-164 describes — better a slow cold boot
		// than a panicking switch.
		{"unknown enum defaults to cold boot", vmmdpb.WakeMethod(999), scheddpb.WakeMethod_WAKE_COLD_BOOT},
		{"zero value defaults to cold boot", vmmdpb.WakeMethod(0), scheddpb.WakeMethod_WAKE_COLD_BOOT},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mapMethod(tc.in); got != tc.want {
				t.Errorf("mapMethod(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
