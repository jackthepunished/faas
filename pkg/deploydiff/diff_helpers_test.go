// Whitebox tests for the helpers + ToWire envelope in
// pkg/deploydiff/diff.go. The engine's public surface (Compute,
// Quota, RenderJSON, RenderText) is covered elsewhere; this file
// pins the small surface in diff.go that doesn't have its own
// caller-side test:
//
//   - AsAny / anyJSON.MarshalJSON — the polymorphism bridge that
//     lets the renderer + ToWire hand scalars, slices, maps, and
//     structs to encoding/json without case-splitting.
//   - anyJSONToRaw — the canonical wire conversion ToWire uses to
//     turn anyJSON → json.RawMessage (nil bypass).
//   - EqualAny — reflect.DeepEqual wrapper used by diffCrons /
//     diffEdgeRules / diffDeployment list-compare fallbacks.
//   - EmptyBaseline — constructor for the fresh-app path.
//   - Diff.ToWire — the canonical wire envelope (sorts +
//     polymorphic re-emission + HasBlockingBreaks wrap).

package deploydiff

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
)

// --- AsAny / anyJSON.MarshalJSON -------------------------------------

func TestAsAny_NilValue(t *testing.T) {
	a := AsAny(nil)
	if a.Value != nil {
		t.Errorf("AsAny(nil).Value = %v, want nil", a.Value)
	}
	got, err := a.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if string(got) != "null" {
		t.Errorf("MarshalJSON(nil) = %s, want null", got)
	}
}

func TestAnyJSON_Primitive(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{int(256), "256"},
		{int64(256), "256"},
		{float64(1.5), "1.5"},
		{true, "true"},
		{"hello", `"hello"`},
	}
	for _, c := range cases {
		got, err := AsAny(c.in).MarshalJSON()
		if err != nil {
			t.Errorf("MarshalJSON(%v): %v", c.in, err)
		}
		if string(got) != c.want {
			t.Errorf("MarshalJSON(%v) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestAnyJSON_Slice(t *testing.T) {
	got, err := AsAny([]string{"a", "b"}).MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	want := `["a","b"]`
	if string(got) != want {
		t.Errorf("slice = %s, want %s", got, want)
	}
}

func TestAnyJSON_Map(t *testing.T) {
	got, err := AsAny(map[string]int{"x": 1}).MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	// map keys come back sorted by encoding/json.
	want := `{"x":1}`
	if string(got) != want {
		t.Errorf("map = %s, want %s", got, want)
	}
}

func TestAnyJSON_Struct(t *testing.T) {
	type box struct{ X int }
	got, err := AsAny(box{X: 42}).MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if string(got) != `{"X":42}` {
		t.Errorf("struct = %s, want %s", got, `{"X":42}`)
	}
}

// --- anyJSONToRaw (canonical wire conversion) ------------------------

func TestAnyJSONToRaw_NilValue(t *testing.T) {
	// The contract at diff.go:264-279: nil → nil RawMessage.
	// On the wire path this hits api.DiffChange.Before's omitempty
	// and the field is dropped. (Different from anyJSON's
	// MarshalJSON → "null" on the renderer path; same input,
	// different encoder, different wire shape.)
	got := anyJSONToRaw(AsAny(nil))
	if got != nil {
		t.Errorf("anyJSONToRaw(nil) = %s, want nil", got)
	}
}

func TestAnyJSONToRaw_Primitive(t *testing.T) {
	got := anyJSONToRaw(AsAny(256))
	if string(got) != "256" {
		t.Errorf("anyJSONToRaw(256) = %s, want 256", got)
	}
}

func TestAnyJSONToRaw_String(t *testing.T) {
	got := anyJSONToRaw(AsAny("boom"))
	if string(got) != `"boom"` {
		t.Errorf("anyJSONToRaw(\"boom\") = %s, want %q", got, `"boom"`)
	}
}

func TestAnyJSONToRaw_Struct(t *testing.T) {
	type box struct{ X int }
	got := anyJSONToRaw(AsAny(box{X: 7}))
	if string(got) != `{"X":7}` {
		t.Errorf("anyJSONToRaw(struct{X:7}) = %s, want %s", got, `{"X":7}`)
	}
}

func TestAnyJSONToRaw_Unrepresentable_FallsBackToString(t *testing.T) {
	// json.Marshal fails on channels / functions. per diff.go:268-276
	// the package falls back to the fmt.Sprintf("%v") form so the
	// wire never silently drops a value. Channels are unrepresentable;
	// a function value also is.
	got := anyJSONToRaw(AsAny(make(chan int)))
	// Should at least be non-nil and parse as valid JSON.
	if len(got) == 0 {
		t.Fatal("anyJSONToRaw(chan) = empty, want fallback string")
	}
	var v any
	if err := json.Unmarshal(got, &v); err != nil {
		t.Errorf("anyJSONToRaw(chan) fallback is not valid JSON: %v (%s)", err, got)
	}
}

// --- EqualAny --------------------------------------------------------

func TestEqualAny_DifferentTypes(t *testing.T) {
	if EqualAny(int(1), int64(1)) {
		t.Error("EqualAny should reject different numeric types")
	}
	if EqualAny("a", 1) {
		t.Error("EqualAny should reject mismatched types")
	}
}

func TestEqualAny_SameStruct(t *testing.T) {
	type box struct{ X int }
	if !EqualAny(box{X: 1}, box{X: 1}) {
		t.Error("EqualAny should accept identical structs")
	}
	if EqualAny(box{X: 1}, box{X: 2}) {
		t.Error("EqualAny should reject differing structs")
	}
}

func TestEqualAny_NilAndTypedNil(t *testing.T) {
	// Both nil interface → equal.
	if !EqualAny(nil, nil) {
		t.Error("nil == nil should be equal under EqualAny")
	}
	// Typed nil vs nil interface → reflect.DeepEqual says not equal
	// (nil interface is a different shape than (*int)(nil)). This
	// pin protects future refactors away from reflect.DeepEqual.
	if EqualAny((*int)(nil), nil) {
		t.Error("typed-nil vs nil interface should NOT be equal under reflect.DeepEqual")
	}
}

// --- EmptyBaseline ---------------------------------------------------

func TestEmptyBaseline_EnvByScopeInitialized(t *testing.T) {
	got := EmptyBaseline()
	// Constructor seeds an empty (non-nil) map so callers can append
	// without nil-checking (verified by diffEnvByScope walking the
	// base map without an explicit guard).
	if got.EnvByScope == nil {
		t.Error("EmptyBaseline().EnvByScope must be non-nil (see diffEnvByScope)")
	}
	if len(got.EnvByScope) != 0 {
		t.Errorf("EmptyBaseline().EnvByScope len = %d, want 0", len(got.EnvByScope))
	}
	if got.App != nil {
		t.Error("EmptyBaseline().App must be nil")
	}
	if got.LatestDeployment != nil {
		t.Error("EmptyBaseline().LatestDeployment must be nil")
	}
}

// --- EnvByScopeFromList additional paths -----------------------------

func TestEnvByScopeFromList_EmptyNestedShape(t *testing.T) {
	// Nested shape present but empty: maps to empty string→[]string.
	got := EnvByScopeFromList(api.AppEnvListResponse{
		EnvByScope: api.EnvByScope{},
	})
	for scope, keys := range got {
		if len(keys) != 0 {
			t.Errorf("scope %q has %d keys, want 0", scope, len(keys))
		}
	}
}

func TestEnvByScopeFromList_BothShapesDropFlatFallback(t *testing.T) {
	// When EnvByScope is present, the flat Env list is ignored
	// (EnvByScope takes precedence per diff.go:321-343).
	got := EnvByScopeFromList(api.AppEnvListResponse{
		EnvByScope: api.EnvByScope{
			"default": []api.ScopedAppEnvResponse{{Key: "NESTED"}},
		},
		Env: []api.AppEnvResponse{{Key: "FLAT"}},
	})
	if len(got["default"]) != 1 || got["default"][0] != "NESTED" {
		t.Errorf("nested should win over flat: %+v", got)
	}
	if _, ok := got[api.DefaultEnvScope]; ok && len(got[api.DefaultEnvScope]) > 1 {
		t.Errorf("flat list should be ignored when nested is present: %+v", got)
	}
}

// --- Diff.ToWire -----------------------------------------------------

func TestToWire_EmptyDiff(t *testing.T) {
	d := Diff{Slug: "api", Plan: "hobby"}
	w := d.ToWire()
	if w.Slug != "api" {
		t.Errorf("slug = %q, want api", w.Slug)
	}
	if w.Plan != "hobby" {
		t.Errorf("plan = %q, want hobby", w.Plan)
	}
	if w.Blocking {
		t.Error("empty diff should not block")
	}
	if len(w.Diff.Changes) != 0 {
		t.Errorf("changes len = %d, want 0", len(w.Diff.Changes))
	}
	if len(w.Diff.Breaks) != 0 {
		t.Errorf("breaks len = %d, want 0", len(w.Diff.Breaks))
	}
}

func TestToWire_SortsChangesByField(t *testing.T) {
	d := Diff{
		Slug: "api",
		Changes: []Change{
			{Field: "z_field", Kind: ChangeModify, Before: AsAny(1), After: AsAny(2)},
			{Field: "a_field", Kind: ChangeModify, Before: AsAny(10), After: AsAny(20)},
		},
	}
	w := d.ToWire()
	if len(w.Diff.Changes) != 2 {
		t.Fatalf("changes len = %d, want 2", len(w.Diff.Changes))
	}
	if w.Diff.Changes[0].Field != "a_field" {
		t.Errorf("Changes[0].Field = %q, want a_field", w.Diff.Changes[0].Field)
	}
	if w.Diff.Changes[1].Field != "z_field" {
		t.Errorf("Changes[1].Field = %q, want z_field", w.Diff.Changes[1].Field)
	}
	// Kind as string, not the typed ChangeKind.
	if w.Diff.Changes[0].Kind != "modify" {
		t.Errorf("Kind = %q, want modify", w.Diff.Changes[0].Kind)
	}
}

func TestToWire_SortsBreaks_ErrorsBefore(t *testing.T) {
	d := Diff{
		Slug: "api",
		Breaks: []Break{
			{Code: "warn_b", Severity: SeverityWarn},
			{Code: "err_a", Severity: SeverityError},
			{Code: "err_b", Severity: SeverityError},
			{Code: "warn_a", Severity: SeverityWarn},
		},
	}
	w := d.ToWire()
	if len(w.Diff.Breaks) != 4 {
		t.Fatalf("breaks len = %d, want 4", len(w.Diff.Breaks))
	}
	wantOrder := []string{"err_a", "err_b", "warn_a", "warn_b"}
	for i, want := range wantOrder {
		if w.Diff.Breaks[i].Code != want {
			t.Errorf("Breaks[%d].Code = %q, want %q", i, w.Diff.Breaks[i].Code, want)
		}
	}
}

func TestToWire_OmitsNilPolymorphics(t *testing.T) {
	// Add change → Before nil → api.DiffChange.Before omitempty drops it.
	d := Diff{
		Slug: "api",
		Changes: []Change{
			{Field: "memory", Kind: ChangeAdd, After: AsAny(256)},
		},
	}
	w := d.ToWire()
	c := w.Diff.Changes[0]
	if c.Before != nil {
		t.Errorf("ToWire Add.Before should be nil (api.DiffChange.Before omitempty); got %s", c.Before)
	}
	if len(c.After) == 0 {
		t.Error("After should be set")
	}
	if string(c.After) != "256" {
		t.Errorf("After = %s, want 256", c.After)
	}
}

func TestToWire_BlockingBoolOnErrorSeverity(t *testing.T) {
	d := Diff{
		Slug: "api",
		Breaks: []Break{
			{Code: "real_problem", Severity: SeverityError},
		},
	}
	w := d.ToWire()
	if !w.Blocking {
		t.Error("Diff{Error break} → ToWire().Blocking must be true")
	}
}

func TestToWire_PolymorphicLimitValues(t *testing.T) {
	// Quota breaks carry observed/limit as numbers → must round-trip
	// as JSON numbers on the wire (anyJSONToRaw → json.Marshal).
	d := Diff{
		Slug: "api",
		Breaks: []Break{
			{
				Code:     "plan_limit_ram",
				Severity: SeverityError,
				Reason:   "ram too big",
				Field:    "memory",
				Observed: AsAny(1024),
				Limit:    AsAny(256),
			},
		},
	}
	w := d.ToWire()
	if len(w.Diff.Breaks) != 1 {
		t.Fatalf("breaks len = %d, want 1", len(w.Diff.Breaks))
	}
	b := w.Diff.Breaks[0]
	if string(b.Observed) != "1024" {
		t.Errorf("Observed = %s, want 1024", b.Observed)
	}
	if string(b.Limit) != "256" {
		t.Errorf("Limit = %s, want 256", b.Limit)
	}
	if b.Field != "memory" {
		t.Errorf("Field = %q, want memory", b.Field)
	}
}

func TestToWire_Field_OptionalButSentinelSafe(t *testing.T) {
	// api.DiffBreak.Field has omitempty — when empty, must NOT
	// appear on the wire. This is the dual of TestToWire_OmitsNilPolymorphics.
	d := Diff{
		Slug: "api",
		Breaks: []Break{
			{Code: "no_field", Severity: SeverityWarn, Reason: "y"},
		},
	}
	w := d.ToWire()
	// Field is empty string → omitempty on api.DiffBreak drops it.
	if w.Diff.Breaks[0].Field != "" {
		t.Errorf("Field = %q, want empty (omitempty on wire DTO)", w.Diff.Breaks[0].Field)
	}
}

// --- HasBlockingBreaks (already covered in engine_test.go, but the
//     no-breaks edge case isn't — Diff{} vs nil-slice). ---------

func TestToWire_NoBreaksPathIsNotBlocking(t *testing.T) {
	d := Diff{Slug: "api"}
	w := d.ToWire()
	if w.Blocking {
		t.Error("Diff{} (no breaks) must not be blocking")
	}
}

// --- ToWire wire shape is JSON-roundtrippable -----------------------

func TestToWire_RoundTripsAsJSON(t *testing.T) {
	// Sanity: the wire envelope must serialize + deserialize without
	// ambiguity. This guards against any future field-add that
	// conflicts with an existing one (the api.DiffChange type has
	// exactly these fields per dto.go — pin the contract here too).
	d := Diff{
		Slug: "api",
		Plan: "hobby",
		Changes: []Change{
			{Field: "memory", Kind: ChangeModify, Before: AsAny(256), After: AsAny(512)},
		},
		Breaks: []Break{
			{Code: "x", Severity: SeverityWarn, Reason: "y", Field: "z",
				Observed: AsAny(7), Limit: AsAny(3)},
		},
	}
	w := d.ToWire()
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(b), `"plan":"hobby"`) {
		t.Errorf("wire plan not present: %s", b)
	}
	if !strings.Contains(string(b), `"blocking":false`) {
		t.Errorf("wire blocking not present: %s", b)
	}
}
