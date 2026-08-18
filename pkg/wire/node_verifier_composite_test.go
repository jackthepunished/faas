package wire

import (
	"errors"
	"testing"
)

func TestAnyNodeVerifierAcceptsAnyChild(t *testing.T) {
	compute := NewInmemNodeVerifier()
	compute.Set([]string{"vmmd.faas"})
	control := NewInmemNodeVerifier()
	control.Set([]string{"schedd.faas"})

	verifier := NewAnyNodeVerifier(compute, control)
	for _, cn := range []string{"vmmd.faas", "schedd.faas"} {
		if err := verifier.LookupCN(cn); err != nil {
			t.Errorf("LookupCN(%q) = %v, want nil", cn, err)
		}
	}
	if err := verifier.LookupCN("unknown.faas"); !errors.Is(err, ErrNodeVerifierCNMismatch) {
		t.Fatalf("LookupCN(unknown.faas) = %v, want ErrNodeVerifierCNMismatch", err)
	}
}

func TestAnyNodeVerifierIgnoresNilChildren(t *testing.T) {
	control := NewInmemNodeVerifier()
	control.Set([]string{"schedd.faas"})
	verifier := NewAnyNodeVerifier(nil, control)
	if err := verifier.LookupCN("schedd.faas"); err != nil {
		t.Fatalf("LookupCN(schedd.faas) = %v, want nil", err)
	}
}
