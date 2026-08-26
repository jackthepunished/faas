package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/onebox-faas/faas/pkg/httpsec"
	"github.com/onebox-faas/faas/pkg/state"
)

func TestRuntimeConfigManagerDoesNotPromotePendingNonHotValue(t *testing.T) {
	previous := httpsec.HSTSEnabled
	t.Cleanup(func() { httpsec.SetHSTSEnabled(previous) })
	store := state.NewMemStore()
	row, err := store.UpsertRuntimeConfig(context.Background(), state.RuntimeConfigUpdate{
		Key: runtimeConfigHSTS, Scope: state.RuntimeConfigScopeGlobal,
		DesiredValue: json.RawMessage("false"), ApplyMode: state.RuntimeConfigApplyGraceful,
		Reason: "test pending state",
	})
	if err != nil {
		t.Fatalf("UpsertRuntimeConfig: %v", err)
	}
	m := newRuntimeConfigManager(func(string) string { return "" })
	if err := m.reconcile(context.Background(), store); err != nil {
		t.Fatalf("reconcile pending: %v", err)
	}
	if got := m.Bool(runtimeConfigHSTS, true); !got {
		t.Fatalf("pending graceful value became effective: got %v", got)
	}
	if err := store.MarkRuntimeConfigApplied(context.Background(), row.Key, row.Scope, row.ScopeID, row.Version, row.DesiredValue, ""); err != nil {
		t.Fatalf("MarkRuntimeConfigApplied: %v", err)
	}
	if err := m.reconcile(context.Background(), store); err != nil {
		t.Fatalf("reconcile applied: %v", err)
	}
	if got := m.Bool(runtimeConfigHSTS, true); got {
		t.Fatalf("applied graceful value did not become effective: got %v", got)
	}
}

func TestRuntimeConfigManagerValidation(t *testing.T) {
	m := newRuntimeConfigManager(func(string) string { return "" })
	if err := m.apply(runtimeConfigDomainDoctorTTL, json.RawMessage(`0`)); err == nil {
		t.Fatal("expected TTL validation error")
	}
	if err := m.apply(runtimeConfigDomainDoctor, json.RawMessage(`"true"`)); err == nil {
		t.Fatal("expected boolean validation error")
	}
}
