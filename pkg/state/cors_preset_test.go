package state

// Unit tests for the compile-side CORS preset merge helper
// (MergeCorsPresetIntoRule, issue #975 item #4 /
// Mega-Foundation #979-b, slot 00294). The merge contract is
// the rule-overrides-preset convention: rule non-zero fields
// win, preset fills in zero-valued rule fields. The merge is a
// pure function — no I/O, no globals — so the test table is the
// canonical spec PR-B's compile-side caller in
// cmd/gatewayd-internal/edge_rules.go::compileCORSRules will
// depend on.

import (
	"errors"
	"testing"
	"time"
)

func samplePreset(accountID string) CorsPreset {
	return CorsPreset{
		ID:               "preset-1",
		AccountID:        accountID,
		AppID:            "",
		Name:             "marketing",
		AllowOrigins:     []string{"https://app.example.com", "https://admin.example.com"},
		AllowMethods:     []string{"GET", "POST"},
		AllowHeaders:     []string{"Authorization"},
		ExposeHeaders:    []string{"X-Request-Id"},
		AllowCredentials: false,
		MaxAgeSeconds:    600,
		CreatedAt:        time.Now().UTC(),
		UpdatedAt:        time.Now().UTC(),
	}
}

// TestMergeCorsPresetIntoRule_NoPresetID covers the "rule did
// not stamp a preset_id" case. The rule's values are returned
// unchanged — the helper is a no-op when presetID == "".
func TestMergeCorsPresetIntoRule_NoPresetID(t *testing.T) {
	rule := CorsRuleOverride{
		AllowOrigins:  []string{"https://only.example.com"},
		AllowMethods:  []string{"GET"},
		AllowHeaders:  []string{"X-Trace-Id"},
		MaxAgeSeconds: 1200,
	}
	got, err := MergeCorsPresetIntoRule("acct-1", "app-1", "", rule, CorsPreset{})
	if err != nil {
		t.Fatalf("MergeCorsPresetIntoRule: %v", err)
	}
	if got.PresetID != "" {
		t.Errorf("PresetID = %q, want empty", got.PresetID)
	}
	if len(got.AllowOrigins) != 1 || got.AllowOrigins[0] != "https://only.example.com" {
		t.Errorf("AllowOrigins = %v, want rule's value unchanged", got.AllowOrigins)
	}
	if got.MaxAgeSeconds != 1200 {
		t.Errorf("MaxAgeSeconds = %d, want 1200", got.MaxAgeSeconds)
	}
}

// TestMergeCorsPresetIntoRule_PresetFillsInRuleZeros covers the
// core contract: a rule with empty allow_origins takes the
// preset's allow_origins. Same for the other fields.
func TestMergeCorsPresetIntoRule_PresetFillsInRuleZeros(t *testing.T) {
	rule := CorsRuleOverride{
		// AllowOrigins empty — preset fills in.
		// AllowMethods empty — preset fills in.
		AllowHeaders:     []string{"X-Trace-Id"}, // rule override
		ExposeHeaders:    []string{},             // preset fills in
		AllowCredentials: true,                   // rule override (true)
		MaxAgeSeconds:    0,                      // preset fills in (600)
	}
	preset := samplePreset("acct-1")
	got, err := MergeCorsPresetIntoRule("acct-1", "app-1", preset.ID, rule, preset)
	if err != nil {
		t.Fatalf("MergeCorsPresetIntoRule: %v", err)
	}
	if got.PresetID != preset.ID {
		t.Errorf("PresetID = %q, want %q", got.PresetID, preset.ID)
	}
	if len(got.AllowOrigins) != 2 {
		t.Errorf("AllowOrigins len = %d, want 2 (from preset)", len(got.AllowOrigins))
	}
	if len(got.AllowMethods) != 2 {
		t.Errorf("AllowMethods len = %d, want 2 (from preset)", len(got.AllowMethods))
	}
	if len(got.AllowHeaders) != 1 || got.AllowHeaders[0] != "X-Trace-Id" {
		t.Errorf("AllowHeaders = %v, want rule override", got.AllowHeaders)
	}
	if len(got.ExposeHeaders) != 1 {
		t.Errorf("ExposeHeaders len = %d, want 1 (from preset)", len(got.ExposeHeaders))
	}
	if !got.AllowCredentials {
		t.Errorf("AllowCredentials = false, want rule's true value")
	}
	if got.MaxAgeSeconds != 600 {
		t.Errorf("MaxAgeSeconds = %d, want 600 (from preset)", got.MaxAgeSeconds)
	}
}

// TestMergeCorsPresetIntoRule_RuleWinsOnNonZero covers the
// override path: every field the rule sets wins, regardless of
// the preset.
func TestMergeCorsPresetIntoRule_RuleWinsOnNonZero(t *testing.T) {
	rule := CorsRuleOverride{
		AllowOrigins:     []string{"https://override.example.com"},
		AllowMethods:     []string{"DELETE"},
		AllowHeaders:     []string{"X-Custom"},
		ExposeHeaders:    []string{"X-Custom-Trace"},
		AllowCredentials: true,
		MaxAgeSeconds:    7200,
	}
	preset := samplePreset("acct-1")
	got, err := MergeCorsPresetIntoRule("acct-1", "app-1", preset.ID, rule, preset)
	if err != nil {
		t.Fatalf("MergeCorsPresetIntoRule: %v", err)
	}
	if got.AllowOrigins[0] != "https://override.example.com" {
		t.Errorf("AllowOrigins = %v, want rule override", got.AllowOrigins)
	}
	if got.AllowMethods[0] != "DELETE" {
		t.Errorf("AllowMethods = %v, want rule override", got.AllowMethods)
	}
	if got.AllowHeaders[0] != "X-Custom" {
		t.Errorf("AllowHeaders = %v, want rule override", got.AllowHeaders)
	}
	if got.ExposeHeaders[0] != "X-Custom-Trace" {
		t.Errorf("ExposeHeaders = %v, want rule override", got.ExposeHeaders)
	}
	if !got.AllowCredentials {
		t.Errorf("AllowCredentials = false, want rule override true")
	}
	if got.MaxAgeSeconds != 7200 {
		t.Errorf("MaxAgeSeconds = %d, want rule override 7200", got.MaxAgeSeconds)
	}
}

// TestMergeCorsPresetIntoRule_PresetIDMismatchReturnsErrNotFound
// pins the IDOR guard. The caller passed a preset that does
// not match the rule's stamped preset_id — this must surface as
// ErrNotFound (not a typed CORS error) so the apid boundary
// returns 422 with the same wire shape as a deleted preset.
func TestMergeCorsPresetIntoRule_PresetIDMismatchReturnsErrNotFound(t *testing.T) {
	rule := CorsRuleOverride{AllowOrigins: []string{"https://x.example.com"}}
	preset := samplePreset("acct-1")
	preset.ID = "preset-different"
	_, err := MergeCorsPresetIntoRule("acct-1", "app-1", "preset-1", rule, preset)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound (IDOR guard)", err)
	}
}

// TestMergeCorsPresetIntoRule_CrossAccountReturnsErrNotFound
// pins the cross-tenant IDOR guard. The preset exists but is
// owned by a different account — same ErrNotFound response
// shape so the wire-side message is stable.
func TestMergeCorsPresetIntoRule_CrossAccountReturnsErrNotFound(t *testing.T) {
	rule := CorsRuleOverride{AllowOrigins: []string{"https://x.example.com"}}
	preset := samplePreset("acct-other")
	_, err := MergeCorsPresetIntoRule("acct-1", "app-1", preset.ID, rule, preset)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound (cross-tenant IDOR)", err)
	}
}

// TestMergeCorsPresetIntoRule_RuleDefensiveCopyOfSlices pins the
// defensive copy: mutating the returned slice after the merge
// must not affect the rule's input slice (nor the preset's).
// The compile path caches the rule action union in memory; a
// cross-slice mutation through the helper would silently
// corrupt the cache.
func TestMergeCorsPresetIntoRule_RuleDefensiveCopyOfSlices(t *testing.T) {
	ruleOrigins := []string{"https://a.example.com"}
	rule := CorsRuleOverride{AllowOrigins: ruleOrigins}
	preset := samplePreset("acct-1")
	got, err := MergeCorsPresetIntoRule("acct-1", "app-1", preset.ID, rule, preset)
	if err != nil {
		t.Fatalf("MergeCorsPresetIntoRule: %v", err)
	}
	got.AllowOrigins[0] = "https://mutated.example.com"
	if ruleOrigins[0] != "https://a.example.com" {
		t.Errorf("rule input slice mutated: %v", ruleOrigins)
	}
	if preset.AllowOrigins[0] == "https://mutated.example.com" {
		t.Errorf("preset input slice mutated")
	}
}
