package fleetbundle

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/nodeclaim"
)

func TestNewGeneratesValidBoundedBundle(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	claim, err := nodeclaim.Parse([]byte(`api_version: gregale.dev/v1alpha1
kind: ComputeNodeClaim
metadata:
  name: fsn-4
spec:
  ssh:
    host: 203.0.113.27
    host_key_sha256: SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
`))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := New("production", 8, now, time.Hour, []nodeclaim.Claim{*claim})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if errs := bundle.ValidateAt(now); errs != nil {
		t.Fatalf("new bundle invalid: %v", errs)
	}
	if bundle.Spec.Nonce == "" || bundle.Spec.ExpiresAt.Sub(bundle.Spec.IssuedAt) != time.Hour {
		t.Fatalf("bundle timing/nonce = %#v", bundle.Spec)
	}
	body, err := Marshal(bundle)
	if err != nil || len(body) == 0 {
		t.Fatalf("Marshal: len=%d err=%v", len(body), err)
	}
}

func TestNewRejectsUnpinnedClaim(t *testing.T) {
	claim := nodeclaim.Claim{
		APIVersion: nodeclaim.APIVersion,
		Kind:       nodeclaim.Kind,
		Metadata:   nodeclaim.Metadata{Name: "fsn-4"},
		Spec:       nodeclaim.Spec{SSH: nodeclaim.SSH{Host: "203.0.113.27"}},
	}
	if _, err := New("production", 1, time.Now(), time.Hour, []nodeclaim.Claim{claim}); err == nil || !strings.Contains(err.Error(), "host_key_sha256") {
		t.Fatalf("unpinned claim error = %v", err)
	}
}

const validBundle = `api_version: gregale.dev/v1alpha1
kind: FleetEnrollmentBundle
metadata:
  name: production
  generation: 7
spec:
  issued_at: 2026-08-28T12:00:00Z
  expires_at: 2026-08-29T12:00:00Z
  nonce: MDEyMzQ1Njc4OWFiY2RlZg
  claims:
    - api_version: gregale.dev/v1alpha1
      kind: ComputeNodeClaim
      metadata:
        name: fsn-4
      spec:
        ssh:
          host: 203.0.113.27
          user: root
          port: 22
          host_key_sha256: SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
        storage:
          device: /dev/disk/by-id/provider-data
          format: false
`

func TestParseValidateAndClaim(t *testing.T) {
	bundle, err := Parse([]byte(validBundle))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	now := time.Date(2026, 8, 28, 12, 30, 0, 0, time.UTC)
	if errs := bundle.ValidateAt(now); errs != nil {
		t.Fatalf("ValidateAt: %v", errs)
	}
	claim, err := bundle.ClaimForNode("fsn-4")
	if err != nil {
		t.Fatalf("ClaimForNode: %v", err)
	}
	if claim.SSHHost != "203.0.113.27" || claim.HostKeySHA256 == "" {
		t.Fatalf("claim = %#v", claim)
	}
	if got := ReplayKey(*bundle, "fsn-4"); len(got) != 64 {
		t.Fatalf("ReplayKey length = %d, want 64", len(got))
	}
}

func TestParseRejectsUnknownAndMultipleDocuments(t *testing.T) {
	if _, err := Parse([]byte(validBundle + "unexpected: true\n")); err == nil || !strings.Contains(err.Error(), "field unexpected not found") {
		t.Fatalf("unknown field error = %v", err)
	}
	if _, err := Parse([]byte(validBundle + "---\napi_version: gregale.dev/v1alpha1\n")); err == nil || !strings.Contains(err.Error(), "multiple documents") {
		t.Fatalf("multiple document error = %v", err)
	}
}

func TestValidateAtRejectsExpiredFutureAndLongLived(t *testing.T) {
	bundle, err := Parse([]byte(validBundle))
	if err != nil {
		t.Fatal(err)
	}
	for name, now := range map[string]time.Time{
		"expired": time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC),
		"future":  time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC),
	} {
		t.Run(name, func(t *testing.T) {
			if errs := bundle.ValidateAt(now); errs == nil {
				t.Fatal("ValidateAt unexpectedly accepted time outside authorization window")
			}
		})
	}
	bundle.Spec.ExpiresAt = bundle.Spec.IssuedAt.Add(MaxLifetime + time.Second)
	if errs := bundle.Validate(); errs == nil || !strings.Contains(errs.Error(), "lifetime") {
		t.Fatalf("long lifetime error = %v", errs)
	}
}

func TestValidateRequiresHostKeyAndRejectsDuplicateNodes(t *testing.T) {
	bundle, err := Parse([]byte(validBundle))
	if err != nil {
		t.Fatal(err)
	}
	bundle.Spec.Claims = append(bundle.Spec.Claims, bundle.Spec.Claims[0])
	bundle.Spec.Claims[0].Spec.SSH.HostKeySHA256 = ""
	errs := bundle.Validate()
	if len(errs) < 2 || !strings.Contains(errs.Error(), "host_key_sha256") || !strings.Contains(errs.Error(), "duplicate node") {
		t.Fatalf("Validate errors = %v", errs)
	}
}

func TestReplayStateIsAtomicSingleUse(t *testing.T) {
	bundle, err := Parse([]byte(validBundle))
	if err != nil {
		t.Fatal(err)
	}
	state := ReplayState{Dir: filepath.Join(t.TempDir(), "used")}
	if err := state.Check(*bundle, "fsn-4"); err != nil {
		t.Fatalf("initial Check: %v", err)
	}
	if err := state.Mark(*bundle, "fsn-4"); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if err := state.Check(*bundle, "fsn-4"); !errors.Is(err, ErrReplay) {
		t.Fatalf("second Check = %v, want ErrReplay", err)
	}
	if err := state.Mark(*bundle, "fsn-4"); !errors.Is(err, ErrReplay) {
		t.Fatalf("second Mark = %v, want ErrReplay", err)
	}
	entries, err := os.ReadDir(state.Dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("replay state entries = %v, err=%v", entries, err)
	}
}
