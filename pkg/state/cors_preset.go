package state

// Compile-side merge helper for cors_presets (issue #975 item #4
// / Mega-Foundation #979-b, slot 00294). PR-A ships the data
// model and the read path; PR-B (#979-c, slot 00295) adds the
// per-rule cors.preset_id field and calls MergeCorsPresetIntoRule
// from cmd/gatewayd-internal/edge_rules.go::compileCORSRules.
//
// The merge contract is the standard "rule overrides, preset
// fills in" convention documented in the issue #975 audit:
//
//   - if the rule has a non-zero / non-empty field, it wins
//   - if the rule's field is the zero value, the preset fills in
//   - if the preset is missing, the rule's value is returned
//     unchanged
//
// The convention lets a customer ship a preset that only sets
// the convention (the allowlist) and override the rest of the
// fields on a per-rule basis — a typical "marketing site
// preset" has allow_origins and allow_methods, the rule stamps
// expose_headers = ["X-Request-Id"] and credentials = true.
//
// The zero-detection is per-field, not per-struct: a rule with
// `allow_origins: []` is interpreted as "no override, take the
// preset's value" because an empty allowlist is meaningless for
// CORS (the apid-Validate gate in PR-B rejects it before
// INSERT). The same goes for the integer zero on
// max_age_seconds: a rule without a max-age override means
// "use the preset's value (default 600)".

// CorsRuleOverride is the input bundle for MergeCorsPresetIntoRule
// — the per-rule fields the customer stamped on a kind=cors rule
// (state.EdgeRuleAction.CORS, after PR-B lands). The struct is
// declared here rather than imported from the action union to
// keep the merge helper free of the per-kind action type
// dependency; PR-B's compile side calls
// `MergeCorsPresetIntoRule(acct, app, presetID, struct{...}{...},
// preset)` to type-coerce the action union.
type CorsRuleOverride struct {
	AllowOrigins     []string
	AllowMethods     []string
	AllowHeaders     []string
	ExposeHeaders    []string
	AllowCredentials bool
	MaxAgeSeconds    int
}

// MergedCorsRuleAction is the compile-side resolved shape
// produced by MergeCorsPresetIntoRule. The PresetID field is
// stamped so gatewayd telemetry can answer "this rule resolved
// against preset X" without re-reading the preset.
type MergedCorsRuleAction struct {
	PresetID         string
	AllowOrigins     []string
	AllowMethods     []string
	AllowHeaders     []string
	ExposeHeaders    []string
	AllowCredentials bool
	MaxAgeSeconds    int
}

// MergeCorsPresetIntoRule applies the rule-overrides-preset
// convention described above. The preset may be the zero value
// (e.g. the rule did not stamp a preset_id) — in that case the
// rule's values are returned unchanged so the caller's compile
// path is uniform.
//
// Errors:
//   - ErrNotFound: the rule stamped a preset_id but the preset
//     was deleted (or owned by a different account) before the
//     compile read. The caller surfaces this as 422 at the apid
//     boundary ("preset has been deleted; re-save the rule") and
//     as a 5xx at the gateway compile path (the rule is
//     misconfigured; the customer must intervene).
//
// IDOR guard: the caller's accountID is matched against the
// preset's AccountID. A preset stamped by one tenant cannot
// influence a rule owned by another — the caller cannot
// accidentally pass the wrong preset because the IDOR check
// fails before the merge. This is the second line of defense
// beyond the apid-Validate gate (which rejects a rule that
// stamps a preset_id owned by a different account at insert
// time).
func MergeCorsPresetIntoRule(accountID, appID, presetID string, rule CorsRuleOverride, preset CorsPreset) (MergedCorsRuleAction, error) {
	_ = appID // reserved for future "app-scoped lookup first" override resolution
	out := MergedCorsRuleAction{
		AllowOrigins:     append([]string(nil), rule.AllowOrigins...),
		AllowMethods:     append([]string(nil), rule.AllowMethods...),
		AllowHeaders:     append([]string(nil), rule.AllowHeaders...),
		ExposeHeaders:    append([]string(nil), rule.ExposeHeaders...),
		AllowCredentials: rule.AllowCredentials,
		MaxAgeSeconds:    rule.MaxAgeSeconds,
	}
	if presetID == "" {
		return out, nil
	}
	if preset.ID != presetID {
		// IDOR guard: the caller passed a preset that does not
		// match the rule's stamped preset_id. Treat as
		// ErrNotFound so the apid boundary returns 422 ("preset
		// has been deleted; re-save the rule") regardless of
		// the actual cause — leaking the difference would
		// expose one tenant's preset_id space to another.
		return MergedCorsRuleAction{}, ErrNotFound
	}
	if preset.AccountID != accountID {
		// Cross-tenant IDOR: the preset exists but is owned by
		// a different account. The handler that called us
		// shouldn't have looked it up — the apid-Validate gate
		// rejects this at insert time. Surface as ErrNotFound
		// to keep the wire-side message stable.
		return MergedCorsRuleAction{}, ErrNotFound
	}
	out.PresetID = preset.ID
	// Rule-wins-zero-overrides-preset. The "non-zero" detector
	// is per-field, not per-struct (a rule that only overrides
	// allow_credentials should not blank the rest of the
	// preset's fields).
	if len(out.AllowOrigins) == 0 {
		out.AllowOrigins = append([]string(nil), preset.AllowOrigins...)
	}
	if len(out.AllowMethods) == 0 {
		out.AllowMethods = append([]string(nil), preset.AllowMethods...)
	}
	if len(out.AllowHeaders) == 0 {
		out.AllowHeaders = append([]string(nil), preset.AllowHeaders...)
	}
	if len(out.ExposeHeaders) == 0 {
		out.ExposeHeaders = append([]string(nil), preset.ExposeHeaders...)
	}
	// AllowCredentials is a bool; "zero" = false. The wire-
	// level omitempty on the rule field means an absent field
	// and a false field are indistinguishable at the action
	// struct level — the apid-Validate gate in PR-B rejects
	// a rule that omits the field, so an absent field is
	// pre-validated as "do not touch the preset's value".
	// The handler must stamp AllowCredentials only when the
	// customer sends the field.
	if !out.AllowCredentials {
		out.AllowCredentials = preset.AllowCredentials
	}
	if out.MaxAgeSeconds == 0 {
		out.MaxAgeSeconds = preset.MaxAgeSeconds
	}
	return out, nil
}
