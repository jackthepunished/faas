package nodeclaim

import (
	"errors"
	"strings"
	"testing"
)

const validClaim = `api_version: gregale.dev/v1alpha1
kind: ComputeNodeClaim
metadata:
  name: fsn-3
spec:
  ssh:
    host: 203.0.113.27
  storage:
    device: /dev/disk/by-id/provider-data
    format: true
`

func TestParseAndNormalize(t *testing.T) {
	claim, err := Parse([]byte(validClaim))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if errs := claim.Validate(); errs != nil {
		t.Fatalf("Validate: %v", errs)
	}
	n := claim.Normalize()
	if n.Name != "fsn-3" || n.SSHUser != "root" || n.SSHPort != 22 {
		t.Fatalf("Normalize = %#v", n)
	}
	if !n.FormatStorage || n.StorageDevice != "/dev/disk/by-id/provider-data" {
		t.Fatalf("normalized storage = %#v", n)
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	_, err := Parse([]byte(validClaim + "unexpected: true\n"))
	if err == nil || !strings.Contains(err.Error(), "field unexpected not found") {
		t.Fatalf("Parse error = %v, want unknown-field error", err)
	}
}

func TestValidateCollectsClaimErrors(t *testing.T) {
	claim, err := Parse([]byte(`api_version: wrong
kind: Wrong
metadata:
  name: FSN_3
spec:
  ssh:
    host: https://bad.example/path
    port: 70000
  storage:
    format: true
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	errs := claim.Validate()
	if len(errs) < 5 {
		t.Fatalf("Validate returned %d errors: %v", len(errs), errs)
	}
	if !errors.Is(errs, ErrInvalid) {
		t.Fatal("claim errors do not match ErrInvalid")
	}
	for _, want := range []string{"api_version", "kind", "metadata.name", "spec.ssh.host", "spec.ssh.port", "spec.storage.format"} {
		if !strings.Contains(errs.Error(), want) {
			t.Errorf("errors missing %q: %v", want, errs)
		}
	}
}

func TestValidateFormatRequiresDevice(t *testing.T) {
	claim, err := Parse([]byte(`api_version: gregale.dev/v1alpha1
kind: ComputeNodeClaim
metadata:
  name: fsn-3
spec:
  ssh:
    host: fsn-3.example
  storage:
    format: true
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if errs := claim.Validate(); errs == nil || !strings.Contains(errs.Error(), "requires spec.storage.device") {
		t.Fatalf("Validate = %v", errs)
	}
}

func TestValidateRejectsUnsafeStoragePath(t *testing.T) {
	claim, err := Parse([]byte(`api_version: gregale.dev/v1alpha1
kind: ComputeNodeClaim
metadata:
  name: fsn-3
spec:
  ssh:
    host: fsn-3.example
  storage:
    device: "/dev/disk/by-id/device with whitespace"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if errs := claim.Validate(); errs == nil || !strings.Contains(errs.Error(), "without whitespace") {
		t.Fatalf("Validate = %v", errs)
	}
}
