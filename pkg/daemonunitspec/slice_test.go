package daemonunitspec

import (
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/daemonunit"
)

func TestUnitSlice_Renders(t *testing.T) {
	u := UnitSlice()
	if u.Slice != "faas-cp.slice" {
		t.Errorf("Slice = %q, want %q", u.Slice, "faas-cp.slice")
	}
	if u.MemoryMax != "3G" {
		t.Errorf("MemoryMax = %q, want %q", u.MemoryMax, "3G")
	}
	if u.WantedBy != "multi-user.target" {
		t.Errorf("WantedBy = %q, want %q", u.WantedBy, "multi-user.target")
	}
	if u.Description == "" {
		t.Error("Description is empty")
	}

	body := string(u.Render())
	// Shape checks: every load-bearing directive must be present.
	for _, want := range []string{
		"[Unit]",
		"Description=",
		// [Slice] MemoryMax renders as a top-level Slice= directive
		// in pkg/daemonunit's emitter (the Slice field is shared
		// between slice units and service units that pin a Slice).
		"Slice=faas-cp.slice",
		"MemoryMax=3G",
		"[Install]",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Render() missing %q\n--- body ---\n%s", want, body)
		}
	}

	// Round-trip via Decode confirms the struct is well-formed.
	parsed, err := daemonunit.Decode([]byte(body))
	if err != nil {
		t.Fatalf("Decode: %v\nbody:\n%s", err, body)
	}
	if parsed.Slice != "faas-cp.slice" {
		t.Errorf("round-trip Slice = %q", parsed.Slice)
	}
	if parsed.MemoryMax != "3G" {
		t.Errorf("round-trip MemoryMax = %q", parsed.MemoryMax)
	}
}

func TestFaasCPSliceMemoryMax_DefaultIsThreeGigabytes(t *testing.T) {
	// Pin the constant. Bumping the slice ceiling is a deliberate
	// operator action; the test prevents an accidental edit from
	// sliding through CI.
	if FaasCPSliceMemoryMax != "3G" {
		t.Errorf("FaasCPSliceMemoryMax = %q, want %q", FaasCPSliceMemoryMax, "3G")
	}
}
