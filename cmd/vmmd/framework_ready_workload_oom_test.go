// Tests for the type=0x05 workload-OOM envelope (Cluster C /
// ADR-121). Mirrors the parseFrameworkReadyDatagram test pattern
// from framework_ready_recv_test.go but for the new closed-set
// member. The wire shape is:
//
//	[1B type=0x05][json:{"peak_mb":N,"plan_mb":N}]
//
// Two failure classes are pinned:
//
//  1. Malformed JSON envelope → parse error (the dispatcher
//     never sees it).
//  2. Closed-set byte outside {0x01..0x05} → parse error
//     ("unknown msg sub-type 0xNN").
//
// The TypeLabel round-trip is also pinned so the Debug log
// classification tracks the constant.
//
// Build tag mirrors framework_ready_recv.go (linux-only —
// the wire-envelope types live behind the linux build tag
// because they're driven by the AF_VSOCK DGRAM listener).
//go:build linux

package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestParseFrameworkReadyDatagram_WorkloadOOM_ValidBody asserts
// the type=0x05 envelope parses into the typed (peak_mb,
// plan_mb) tuple verbatim. The values flow end-to-end into
// the schedd's whycopy Observed closure
// (pkg/whycopy/whycopy.go::CodeAppRuntimeOOM); a future
// refactor that re-shaped the values would silently break the
// customer's templated ErrorWhy / ErrorFix prose.
func TestParseFrameworkReadyDatagram_WorkloadOOM_ValidBody(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                       string
		peakMB, planMB             int
		wantKind                   parseFWKind
		wantPeakMB, wantPlanMB     int
		wantTypeLabelSubstring     string
	}{
		{"hobby_plan_over", 384, 256, parseFWReadyKindWorkloadOOM, 384, 256, "0x05"},
		{"pro_plan_under", 96, 512, parseFWReadyKindWorkloadOOM, 96, 512, "0x05"},
		{"free_plan_zero_peak", 0, 128, parseFWReadyKindWorkloadOOM, 0, 128, "0x05"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := append([]byte{VsockFrameworkReadyHostTypeWorkloadOOM}, []byte(makeOOMWire(tc.peakMB, tc.planMB))...)
			got, err := parseFrameworkReadyDatagram(body)
			if err != nil {
				t.Fatalf("parseFrameworkReadyDatagram(%v): unexpected err: %v", body, err)
			}
			if got.Kind != tc.wantKind {
				t.Errorf("Kind = %d, want %d", got.Kind, tc.wantKind)
			}
			if got.WorkloadOOM.PeakMB != tc.wantPeakMB {
				t.Errorf("WorkloadOOM.PeakMB = %d, want %d", got.WorkloadOOM.PeakMB, tc.wantPeakMB)
			}
			if got.WorkloadOOM.PlanMB != tc.wantPlanMB {
				t.Errorf("WorkloadOOM.PlanMB = %d, want %d", got.WorkloadOOM.PlanMB, tc.wantPlanMB)
			}
		})
	}
}

// TestParseFrameworkReadyDatagram_WorkloadOOM_MalformedJSON
// pins the dispatcher's contract: a non-JSON payload following
// the type byte is rejected with the "workload_oom" sentinel
// (mirroring the sidecar_init_exit / restart error tags). The
// loop's warn-log + drop pattern is unchanged.
func TestParseFrameworkReadyDatagram_WorkloadOOM_MalformedJSON(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		body    []byte
		errSub  string
	}{
		{
			name:   "non_json_payload",
			body:   append([]byte{VsockFrameworkReadyHostTypeWorkloadOOM}, []byte{0xFF, 0xFE, 0xFD}...),
			errSub: "workload_oom",
		},
		{
			name:   "json_missing_closing_brace",
			body:   append([]byte{VsockFrameworkReadyHostTypeWorkloadOOM}, []byte(`{"peak_mb":384`)...),
			errSub: "workload_oom",
		},
		{
			name:   "empty_body_after_type",
			body:   []byte{VsockFrameworkReadyHostTypeWorkloadOOM},
			errSub: "workload_oom",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseFrameworkReadyDatagram(tc.body)
			if err == nil {
				t.Fatalf("parseFrameworkReadyDatagram(%v): err = nil, want error", tc.body)
			}
			if !strings.Contains(err.Error(), tc.errSub) {
				t.Errorf("err = %q, want substring %q", err.Error(), tc.errSub)
			}
		})
	}
}

// TestParseFrameworkReadyDatagram_TypeClosedSet pins the
// 5-value closed-set discipline: {0x01..0x05} parse OK;
// anything else returns the "unknown msg sub-type" sentinel.
// Mirrors TestParseFrameworkReadyDatagram's earlier
// closed-set guard. Future event classes add a byte + a
// switch case + a test in lockstep — this tripwire catches a
// forgotten case.
func TestParseFrameworkReadyDatagram_TypeClosedSet(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body []byte
	}{
		{"type_0x00", []byte{0x00}},
		{"type_0x06", []byte{0x06, 0x00}},
		{"type_0xFF", []byte{0xFF, 0x00, 0x00}},
		{"type_0x07_then_payload", []byte{0x07, '{', '}'}},
		{"type_0x10_then_long_payload", append([]byte{0x10}, []byte(strings.Repeat("x", 32))...)},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseFrameworkReadyDatagram(tc.body)
			if err == nil {
				t.Fatalf("parseFrameworkReadyDatagram(%v) = nil err, want unknown-type error", tc.body)
			}
			if !strings.Contains(err.Error(), "unknown msg sub-type") {
				t.Errorf("err = %q, want substring 'unknown msg sub-type'", err.Error())
			}
		})
	}
}

// TestTypeLabel_WorkloadOOM pins the diagnostic label for
// the new closed-set member. TypeLabel is for Debug logs;
// the loop never dispatches on it (the dispatch is the
// parseFWKind switch), but the label is the operator-
// facing identifier and breaking it would silently gut
// the on-call's fleet-debug experience.
func TestTypeLabel_WorkloadOOM(t *testing.T) {
	t.Parallel()
	m := parseFWReadyMsg{Kind: parseFWReadyKindWorkloadOOM}
	got := m.TypeLabel()
	if !strings.Contains(got, "workload_oom") {
		t.Errorf("TypeLabel = %q, want substring 'workload_oom'", got)
	}
	if !strings.Contains(got, "0x05") {
		t.Errorf("TypeLabel = %q, want substring '0x05'", got)
	}
}

// makeOOMWire constructs a JSON envelope for the type=0x05
// body. Used by the closed-set + payload tests above.
func makeOOMWire(peakMB, planMB int) string {
	b, err := json.Marshal(workloadOOMWire{PeakMB: peakMB, PlanMB: planMB})
	if err != nil {
		// json.Marshal on a struct of two ints can't fail;
		// panic is appropriate here — the test code itself
		// is broken.
		panic(err)
	}
	return string(b)
}
