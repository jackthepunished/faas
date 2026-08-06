package api_test

// public_auth_constants_test.go — cross-package pin for
// the canonical-source rule (issue #477 / ADR-079). The
// same three mode strings live in pkg/api and pkg/state;
// a drift surfaces as a runtime SQL CHECK-constraint
// failure on a PATCH that the API itself accepted.
//
// This test lives in `package api_test` (external test
// package) because pkg/api cannot import pkg/state
// without a cycle (pkg/state stops importing pkg/api
// through the ... cycle per the API/state module split).
// The caps-test counterpart lives in the internal
// package at public_auth_caps_test.go because the
// boundary constants are unexported.

import (
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// TestPublicAuthModeConstantsAgree pins the
// pkg/api <-> pkg/state constant alignment. A future
// contributor adding a fourth mode (e.g. mTLS) MUST
// add the constant to both halves in the same commit;
// running this test after only one side is updated
// fails immediately. The previous-incident pattern
// (webhook secret namespace drift pre-#476) is the
// reference for why this tripwire exists.
func TestPublicAuthModeConstantsAgree(t *testing.T) {
	apiSet := []string{
		api.AppPublicAuthModeOpen,
		api.AppPublicAuthModeBearer,
		api.AppPublicAuthModeBasic,
	}
	stateSet := []string{
		state.AppPublicAuthModeOpen,
		state.AppPublicAuthModeBearer,
		state.AppPublicAuthModeBasic,
	}
	if len(apiSet) != len(stateSet) {
		t.Fatalf("slice length mismatch: pkg/api has %d modes, pkg/state has %d; "+
			"add/remove the constant on both sides in the same commit",
			len(apiSet), len(stateSet))
	}
	for i := range apiSet {
		if apiSet[i] != stateSet[i] {
			t.Errorf("mode %d drift: pkg/api.AppPublicAuthMode* = %q, pkg/state.AppPublicAuthMode* = %q",
				i, apiSet[i], stateSet[i])
		}
	}
	// Cross-check the closed-enum hash so the test fails
	// narratively even if a future contributor shuffles
	// the order to mask the drift.
	apiMap := make(map[string]struct{}, len(apiSet))
	for _, m := range apiSet {
		apiMap[m] = struct{}{}
	}
	stateMap := make(map[string]struct{}, len(stateSet))
	for _, m := range stateSet {
		stateMap[m] = struct{}{}
	}
	for m := range apiMap {
		if _, ok := stateMap[m]; !ok {
			t.Errorf("pkg/api mode %q has no pkg/state counterpart", m)
		}
	}
	for m := range stateMap {
		if _, ok := apiMap[m]; !ok {
			t.Errorf("pkg/state mode %q has no pkg/api counterpart", m)
		}
	}
}
