// edge_rules_compile_cors_preset_test.go — compile-side tests
// for the CORS preset merge path (issue #975 #4 PR-B /
// ADR-129 D3).
//
// Coverage matrix:
//
//   - inline-only: a kind=cors rule with no preset_id
//     compiles verbatim to the EdgeRuleCORSResolved struct
//     (no preset lookup, no merge error).
//   - preset-backed: a rule with CorsPresetID set resolves
//     to the preset's fields; the PresetID field on the
//     resolved struct is stamped so the runtime applier
//     doesn't need a second lookup.
//   - preset missing: a rule whose preset was deleted (FK
//     ON DELETE SET NULL cleared the FK to NULL after the
//     GetCorsPresetByID cache miss — this test exercises the
//     pre-miss path; the rule's CorsPresetID is still set
//     because the rule's JSONB mirror is stale) is dropped
//     from the compiled slice and surfaces a
//     cors_preset_not_found parse error.
//   - *+credentials footgun: the merge re-validates the
//     dangerous combination (defense in depth — the apid
//     create-time gate ran on rule-create, but the customer
//     may have edited the preset since).
//
// The fakeEdgeRuleStore at edge_rules_test.go:35 carries the
// per-id preset map; tests seed it directly. The
// compileCORSRules method takes ctx + storeRules so the tests
// can call it without booting the full loader.
package main

import (
	"context"
	"testing"

	"github.com/onebox-faas/faas/pkg/state"
)

// sampleCorsRule builds a fully-populated state.EdgeRule
// with the given id and a kind=cors action. Optional presetID
// is non-nil only when the test wires the rule to a preset.
func sampleCorsRule(id, accountID, appID, host, path string, presetID *string, allowOrigins, allowMethods []string) state.EdgeRule {
	return state.EdgeRule{
		ID:           id,
		AccountID:    accountID,
		AppID:        appID,
		MatchHost:    host,
		MatchPath:    path,
		MatchMethods: nil,
		Priority:     100,
		Enabled:      true,
		Kind:         state.EdgeRuleKindCORSA,
		Action: state.EdgeRuleAction{
			Kind: state.EdgeRuleKindCORSA,
			CORS: &state.EdgeRuleCORSAction{
				CorsPresetID:     presetID,
				AllowOrigins:     allowOrigins,
				AllowMethods:     allowMethods,
				AllowHeaders:     nil,
				ExposeHeaders:    nil,
				AllowCredentials: false,
				MaxAgeSeconds:    600,
			},
		},
	}
}

// TestCompileCORSRules_InlineOnly_NoPreset pins the no-preset
// branch: the compile path passes the rule's fields through
// the inline-only arm and stamps PresetID="" on the
// resolved struct.
func TestCompileCORSRules_InlineOnly_NoPreset(t *testing.T) {
	g := &gatewaydEdgeRules{store: &fakeEdgeRuleStore{}}
	rules := []state.EdgeRule{
		sampleCorsRule("cors-1", "acc_test", "app_test", "a.example.com", "/", nil,
			[]string{"https://app.example.com"}, []string{"GET", "POST"}),
	}
	got, parseErrs := g.compileCORSRules(context.Background(), rules)
	if len(parseErrs) != 0 {
		t.Fatalf("parseErrs = %v, want empty", parseErrs)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rules, want 1", len(got))
	}
	if got[0].PresetID != "" {
		t.Errorf("PresetID = %q, want \"\" (inline-only path)", got[0].PresetID)
	}
	if len(got[0].AllowOrigins) != 1 || got[0].AllowOrigins[0] != "https://app.example.com" {
		t.Errorf("AllowOrigins = %v, want verbatim inline values", got[0].AllowOrigins)
	}
	if !got[0].AllowCredentials == false {
		t.Errorf("AllowCredentials = %v, want false", got[0].AllowCredentials)
	}
}

// TestCompileCORSRules_PresetBacked_StampsPresetID pins the
// preset-backed merge: the rule's resolved AllowOrigins /
// AllowMethods / MaxAgeSeconds come from the preset, and the
// PresetID field is stamped so the runtime applier can
// answer "this rule resolved against preset X" without a
// second lookup.
func TestCompileCORSRules_PresetBacked_StampsPresetID(t *testing.T) {
	const presetID = "preset-uuid"
	preset := state.CorsPreset{
		ID:            presetID,
		AccountID:     "acc_test",
		AppID:         "",
		Name:          "public-https",
		AllowOrigins:  []string{"https://a.example.com", "https://b.example.com"},
		AllowMethods:  []string{"GET", "POST", "PUT"},
		MaxAgeSeconds: 600,
	}
	store := &fakeEdgeRuleStore{
		presetBy: map[string]state.CorsPreset{
			"acc_test:" + presetID: preset,
		},
	}
	g := &gatewaydEdgeRules{store: store}
	rules := []state.EdgeRule{
		sampleCorsRule("cors-1", "acc_test", "app_test", "a.example.com", "/", ptrString(presetID),
			nil, nil),
	}
	got, parseErrs := g.compileCORSRules(context.Background(), rules)
	if len(parseErrs) != 0 {
		t.Fatalf("parseErrs = %v, want empty", parseErrs)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rules, want 1", len(got))
	}
	if got[0].PresetID != presetID {
		t.Errorf("PresetID = %q, want %q (stamped at compile)", got[0].PresetID, presetID)
	}
	if len(got[0].AllowOrigins) != 2 {
		t.Errorf("AllowOrigins = %v, want 2 preset origins", got[0].AllowOrigins)
	}
	if got[0].MaxAgeSeconds != 600 {
		t.Errorf("MaxAgeSeconds = %d, want 600 (from preset)", got[0].MaxAgeSeconds)
	}
}

// TestCompileCORSRules_PresetMissing_DropsRule pins the
// preset-deleted branch (FK ON DELETE SET NULL FK clear in
// the DB; the rule's JSONB mirror still carries the stale
// preset_id — the compile path returns ErrNotFound from the
// GetCorsPresetByID lookup and drops the rule from the
// compiled slice with a cors_preset_not_found parse error).
func TestCompileCORSRules_PresetMissing_DropsRule(t *testing.T) {
	const presetID = "deleted-preset"
	store := &fakeEdgeRuleStore{
		presetBy: map[string]state.CorsPreset{
			// No entry — simulates "preset was deleted".
		},
	}
	g := &gatewaydEdgeRules{store: store}
	rules := []state.EdgeRule{
		sampleCorsRule("cors-1", "acc_test", "app_test", "a.example.com", "/", ptrString(presetID),
			nil, nil),
	}
	got, parseErrs := g.compileCORSRules(context.Background(), rules)
	if len(got) != 0 {
		t.Errorf("got %d rules, want 0 (preset missing → drop)", len(got))
	}
	if len(parseErrs) != 1 {
		t.Fatalf("parseErrs = %v, want 1 cors_preset_not_found", parseErrs)
	}
	if parseErrs[0].Err == nil || parseErrs[0].Err.Error() == "" {
		t.Errorf("parseErrs[0].Err empty, want cors_preset_not_found message")
	}
}

// TestCompileCORSRules_PresetWildcardWithCredentials_DropsRule
// pins the *+credentials defense-in-depth re-validation:
// even though the apid create-time gate would have rejected
// this combination, the preset could have been edited
// independently to add AllowOrigins: ["*"]. The compile
// path re-runs the merge guard and drops the rule.
func TestCompileCORSRules_PresetWildcardWithCredentials_DropsRule(t *testing.T) {
	const presetID = "footgun-preset"
	preset := state.CorsPreset{
		ID:            presetID,
		AccountID:     "acc_test",
		Name:          "wildcard-with-creds",
		AllowOrigins:  []string{"*"},
		AllowMethods:  []string{"GET"},
		MaxAgeSeconds: 600,
	}
	store := &fakeEdgeRuleStore{
		presetBy: map[string]state.CorsPreset{
			"acc_test:" + presetID: preset,
		},
	}
	g := &gatewaydEdgeRules{store: store}
	rules := []state.EdgeRule{
		func() state.EdgeRule {
			r := sampleCorsRule("cors-1", "acc_test", "app_test", "a.example.com", "/", ptrString(presetID),
				nil, nil)
			r.Action.CORS.AllowCredentials = true
			return r
		}(),
	}
	got, parseErrs := g.compileCORSRules(context.Background(), rules)
	if len(got) != 0 {
		t.Errorf("got %d rules, want 0 (footgun detected → drop)", len(got))
	}
	if len(parseErrs) != 1 {
		t.Fatalf("parseErrs = %v, want 1 cors_wildcard_with_credentials", parseErrs)
	}
}

// TestCompileCORSRules_EmptyInput pins the no-input branch
// (returns nil, nil; matches compileRewriteRules /
// compileHeadersRules precedent at edge_rules_test.go:641).
func TestCompileCORSRules_EmptyInput(t *testing.T) {
	g := &gatewaydEdgeRules{store: &fakeEdgeRuleStore{}}
	got, parseErrs := g.compileCORSRules(context.Background(), nil)
	if got != nil || parseErrs != nil {
		t.Errorf("compileCORSRules(nil) = %v, %v, want nil, nil", got, parseErrs)
	}
}

// ptrString is a tiny helper that returns &s for the
// pointer-shaped fields used by the test (mirrors the
// helpers at handlers_cors_presets_test.go).
func ptrString(s string) *string { return &s }