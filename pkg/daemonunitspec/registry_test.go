package daemonunitspec

import (
	"reflect"
	"testing"
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
