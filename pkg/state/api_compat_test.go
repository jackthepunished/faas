// Package-level guard: pkg/state.DefaultEnvScope must stay byte-
// equal to pkg/api.DefaultEnvScope, because pkg/state cannot import
// pkg/api (cycle) and the literal is duplicated rather than
// referenced. The test renders the constant reads on both sides
// at runtime; a future refactor that drifts the literal fails
// loudly here. This is the simplest possible synchronisation
// mechanism for two string literals — no build tag, no race.
package state_test

import (
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

func TestDefaultEnvScope_MirrorsAPI(t *testing.T) {
	if state.DefaultEnvScope != api.DefaultEnvScope {
		t.Fatalf("state.DefaultEnvScope (%q) drifted from api.DefaultEnvScope (%q) — keep them equal at the source level since pkg/state cannot import pkg/api",
			state.DefaultEnvScope, api.DefaultEnvScope)
	}
	if state.DefaultEnvScope != "default" {
		t.Fatalf("state.DefaultEnvScope = %q, want \"default\" (the ADR-090 / ADR-092 literal)", state.DefaultEnvScope)
	}
	if api.DefaultEnvScope != "default" {
		// Defensive: api side's literal drifted. Catch even if the
		// first assertion somehow passed (e.g. someone edited both).
		t.Fatalf("api.DefaultEnvScope = %q, want \"default\"", api.DefaultEnvScope)
	}
}
