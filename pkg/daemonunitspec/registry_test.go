package daemonunitspec

import (
	"reflect"
	"testing"

	"github.com/onebox-faas/faas/pkg/manifest"
)

func TestActivationOrder(t *testing.T) {
	got := ActivationOrder()
	want := []string{"vmmd", "apid", "schedd", "gatewayd-internal", "gatewayd-public", "meterd", "githubd", "imaged"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ActivationOrder() = %v, want %v", got, want)
	}
}

func TestLifecycleDependenciesAndProbes(t *testing.T) {
	byName := make(map[string]Entry, len(Registry))
	for _, entry := range Registry {
		byName[entry.Name] = entry
	}
	if got := byName["schedd"].Lifecycle.After; !reflect.DeepEqual(got, []string{"vmmd"}) {
		t.Fatalf("schedd dependencies = %v", got)
	}
	if got := byName["gatewayd-internal"].Lifecycle.ProbeTarget; got != "127.0.0.1:9090" {
		t.Fatalf("gatewayd-internal probe = %q", got)
	}
	if got := byName["gatewayd-public"].Lifecycle.ProbeTarget; got != "127.0.0.1:8080" {
		t.Fatalf("gatewayd-public probe = %q", got)
	}
}

// TestRegistryIsSubsetOfHostKeys pins the catalog-parity invariant
// between the daemonunitspec.Registry (systemd-managed daemons, drives
// the renderer + the deploy generator) and pkg/manifest.HostKeys (the
// TOML key catalog, drives the validator).
//
// The Registry MUST be a subset of HostKeys — every Registry entry
// has a manifest.DaemonConfig row + a HostKeys row, otherwise the
// renderer's renderTOML / ValidateTOMLPlacement call will fail with
// "no HostKeys descriptor". The reverse is NOT required: HostKeys
// may carry daemons that aren't systemd-managed (e.g. builderd —
// vmmd spawns it per-build inside an ephemeral microVM per ADR-003).
//
// A future change that adds a daemon to the Registry without a
// matching HostKeys row will trip this test before it reaches CI.
func TestRegistryIsSubsetOfHostKeys(t *testing.T) {
	registry := map[string]bool{}
	for _, e := range Registry {
		registry[e.Name] = true
	}
	// The Registry uses dashed names (gatewayd-internal); the
	// HostKeys catalog uses underscored names (gatewayd_internal).
	// Translate the Registry set before the subset check.
	translated := make(map[string]bool, len(registry))
	for name := range registry {
		switch name {
		case "gatewayd-internal":
			translated["gatewayd_internal"] = true
		case "gatewayd-public":
			translated["gatewayd_public"] = true
		default:
			translated[name] = true
		}
	}
	for hostKey := range manifest.HostKeys {
		if !translated[hostKey] {
			// HostKeys entry with no Registry match is fine
			// (builderd is the canonical example). Surface it
			// in test output so a future maintainer can audit
			// the asymmetry against the per-daemon decision
			// documented in daemonunitspec.go.
			t.Logf("HostKeys entry %q has no Registry counterpart (OK if per-build or future-spawn)", hostKey)
		}
	}
	for name := range translated {
		if _, ok := manifest.HostKeys[name]; !ok {
			t.Errorf("Registry name %q has no HostKeys row — renderer will fail at renderTOML", name)
		}
	}
}
