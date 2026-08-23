// Whitebox tests for pkg/deploydiff/render_json.go. The JSON
// renderer is the canonical wire shape — `gregale deploy --diff
// --json | jq '.blocking'` is the CI gate input, per the docstring
// at render_json.go:17. These tests pin that contract.
//
// Pre-existing render_text_test.go covers the text view; this file
// is the JSON-view coverage pass.
//
// Pattern follows render_text_test.go: asAnyLit + renderTo for the
// public surface, plus direct whitebox assertions on the internal
// sorted* helpers (sortedChanges / sortedBreaks).

package deploydiff

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// renderJSONBytes is a one-line helper for the JSON path.
func renderJSONBytes(t *testing.T, d Diff) string {
	t.Helper()
	var buf bytes.Buffer
	if err := RenderJSON(&buf, d); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	return buf.String()
}

// --- Top-level envelope + Blocking wrap -------------------------------

func TestRenderJSON_EmptyDiff(t *testing.T) {
	got := renderJSONBytes(t, Diff{Slug: "api", Plan: ""})
	// Top-level envelope shape per render_json.go:31-39.
	for _, key := range []string{`"diff"`, `"blocking": false`, `"slug": "api"`, `"plan": ""`} {
		if !strings.Contains(got, key) {
			t.Errorf("empty diff missing %q in:\n%s", key, got)
		}
	}
}

func TestRenderJSON_BlockingFalse_NoErrorBreaks(t *testing.T) {
	// Only warn-severity breaks → blocking stays false.
	d := Diff{
		Slug: "api",
		Breaks: []Break{
			{Code: "warn_only", Severity: SeverityWarn},
		},
	}
	got := renderJSONBytes(t, d)
	if !strings.Contains(got, `"blocking": false`) {
		t.Errorf("warn-only should yield blocking=false; got:\n%s", got)
	}
}

func TestRenderJSON_BlockingTrue_OnErrorSeverity(t *testing.T) {
	// Any error-severity break → blocking=true.
	d := Diff{
		Slug: "api",
		Breaks: []Break{
			{Code: "real_problem", Severity: SeverityError},
		},
	}
	got := renderJSONBytes(t, d)
	if !strings.Contains(got, `"blocking": true`) {
		t.Errorf("error-severity should yield blocking=true; got:\n%s", got)
	}
}

// --- Sort invariants -------------------------------------------------

func TestRenderJSON_ChangesSortedByFieldASC(t *testing.T) {
	d := Diff{
		Slug: "api",
		Changes: []Change{
			{Field: "memory", Kind: ChangeModify, Before: AsAny(256), After: AsAny(512)},
			{Field: "autoscale_target_rps", Kind: ChangeModify, Before: AsAny(1), After: AsAny(2)},
			{Field: "concurrency", Kind: ChangeModify, Before: AsAny(2), After: AsAny(4)},
		},
	}
	got := renderJSONBytes(t, d)
	// Order ASC: autoscale_target_rps < concurrency < memory
	iAuto := strings.Index(got, "autoscale_target_rps")
	iConcur := strings.Index(got, "concurrency")
	iMem := strings.Index(got, "memory")
	if iAuto < 0 || iConcur < 0 || iMem < 0 {
		t.Fatalf("missing field(s): auto=%d concurrency=%d memory=%d in:\n%s", iAuto, iConcur, iMem, got)
	}
	if !(iAuto < iConcur && iConcur < iMem) {
		t.Errorf("fields not sorted ASC: positions auto=%d concur=%d mem=%d", iAuto, iConcur, iMem)
	}
}

func TestRenderJSON_BreaksSortedErrorsFirst_ThenByCode(t *testing.T) {
	d := Diff{
		Slug: "api",
		Breaks: []Break{
			{Code: "warn_b", Severity: SeverityWarn, Reason: "w2"},
			{Code: "err_z", Severity: SeverityError, Reason: "e2"},
			{Code: "err_a", Severity: SeverityError, Reason: "e1"},
			{Code: "warn_a", Severity: SeverityWarn, Reason: "w1"},
		},
	}
	got := renderJSONBytes(t, d)
	// Errors must come first sorted by code ASC, then warns sorted by code ASC.
	iErrA := strings.Index(got, "err_a")
	iErrZ := strings.Index(got, "err_z")
	iWarnA := strings.Index(got, "warn_a")
	iWarnB := strings.Index(got, "warn_b")
	if iErrA < 0 || iErrZ < 0 || iWarnA < 0 || iWarnB < 0 {
		t.Fatalf("missing break codes in:\n%s", got)
	}
	if !(iErrA < iErrZ && iErrZ < iWarnA && iWarnA < iWarnB) {
		t.Errorf("breaks not sorted errors-first + code ASC: positions err_a=%d err_z=%d warn_a=%d warn_b=%d",
			iErrA, iErrZ, iWarnA, iWarnB)
	}
}

// --- Before/After polymorphic JSON encoding ---------------------------

func TestRenderJSON_PolymorphicValues_Primitives(t *testing.T) {
	// Before/After round-trip primitives via anyJSON → MarshalJSON.
	d := Diff{
		Slug: "api",
		Changes: []Change{
			{Field: "memory", Kind: ChangeModify, Before: AsAny(256), After: AsAny(512)},
		},
	}
	got := renderJSONBytes(t, d)
	// JSON encoder emits ints unquoted.
	if !strings.Contains(got, `"before": 256`) || !strings.Contains(got, `"after": 512`) {
		t.Errorf("primitive before/after not encoded as JSON numbers; got:\n%s", got)
	}
}

func TestRenderJSON_PolymorphicValues_Slices(t *testing.T) {
	d := Diff{
		Slug: "api",
		Changes: []Change{
			{Field: "egress_allowlist", Kind: ChangeModify,
				Before: AsAny([]string{"a", "b"}), After: AsAny([]string{"a", "c"})},
		},
	}
	got := renderJSONBytes(t, d)
	for _, want := range []string{`"before": [`, `"after": [`, `"a"`, `"b"`, `"c"`} {
		if !strings.Contains(got, want) {
			t.Errorf("slice encoding missing %q in:\n%s", want, got)
		}
	}
}

func TestRenderJSON_NilPolymorphicRendersAsJSONNull(t *testing.T) {
	// Custom MarshalJSON methods on a struct make Go's
	// encoding/json omitempty tag ineffective (the encoder
	// does not call IsZero on a struct that has its own marshaler).
	// anyJSON's MarshalJSON emits "null" when Value is nil, so on
	// the renderer path a ChangeAdd with no Before renders as
	// `"before": null` (not absent). The ToWire path uses
	// anyJSONToRaw → nil RawMessage → api.DiffChange.Before
	// omitempty (RawMessage is a slice and IS omitempty-friendly),
	// so ToWire is the clean omitted path. Different contracts;
	// this test pins the renderer's null behavior.
	d := Diff{
		Slug: "api",
		Changes: []Change{
			{Field: "memory", Kind: ChangeAdd, After: AsAny(256)},
		},
	}
	got := renderJSONBytes(t, d)
	if !strings.Contains(got, `"before": null`) {
		t.Errorf("RenderJSON: Add change's Before should render as null (anyJSON custom MarshalJSON); got:\n%s", got)
	}
	if !strings.Contains(got, `"after": 256`) {
		t.Errorf("Add change should render After=256; got:\n%s", got)
	}
}

// --- sortedChanges / sortedBreaks direct -----------------------------

func TestSortedChanges_StableSort(t *testing.T) {
	// Direct call verifies the helper is independently idempotent.
	in := []Change{
		{Field: "c"},
		{Field: "a"},
		{Field: "b"},
		{Field: "a"}, // second "a" — stable sort keeps original relative order
	}
	got := sortedChanges(in)
	want := []string{"a", "a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("sortedChanges len = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Field != w {
			t.Errorf("sortedChanges[%d] = %q, want %q", i, got[i].Field, w)
		}
	}
}

func TestSortedChanges_InsertionSortIsStable(t *testing.T) {
	// The implementation at render_json.go:51-57 is an insertion
	// sort (not sort.Strings), so it MUST be stable. Verify two
	// equal "a"s preserve the original order vs a sort.Strings
	// reference (insertion sort + sort.Strings produce identical
	// output for non-equal keys, but stable order matters for the
	// identical-key case — sort.Strings is unstable for that).
	in := []Change{
		{Field: "a", Kind: ChangeAdd},
		{Field: "a", Kind: ChangeModify},
		{Field: "a", Kind: ChangeRemove},
	}
	got := sortedChanges(in)
	wantOrder := []ChangeKind{ChangeAdd, ChangeModify, ChangeRemove}
	for i, w := range wantOrder {
		if got[i].Kind != w {
			t.Errorf("sortedChanges[%d] = %s, want %s (stable insertion sort dropped this slot)",
				i, got[i].Kind, w)
		}
	}
}

func TestSortedChanges_Empty(t *testing.T) {
	if got := sortedChanges(nil); len(got) != 0 {
		t.Errorf("sortedChanges(nil) = %v, want empty", got)
	}
	if got := sortedChanges([]Change{}); len(got) != 0 {
		t.Errorf("sortedChanges([]) = %v, want empty", got)
	}
}

func TestSortedBreaks_ErrorsBeforeWarns(t *testing.T) {
	in := []Break{
		{Code: "warn_a", Severity: SeverityWarn},
		{Code: "err_b", Severity: SeverityError},
		{Code: "err_a", Severity: SeverityError},
		{Code: "warn_b", Severity: SeverityWarn},
	}
	got := sortedBreaks(in)
	wantCodes := []string{"err_a", "err_b", "warn_a", "warn_b"}
	if len(got) != len(wantCodes) {
		t.Fatalf("sortedBreaks len = %d, want %d", len(got), len(wantCodes))
	}
	for i, w := range wantCodes {
		if got[i].Code != w {
			t.Errorf("sortedBreaks[%d] = %q, want %q", i, got[i].Code, w)
		}
	}
}

func TestSortedBreaks_Empty(t *testing.T) {
	if got := sortedBreaks(nil); len(got) != 0 {
		t.Errorf("sortedBreaks(nil) len = %d, want 0", len(got))
	}
}

// --- Plan echo -------------------------------------------------------

func TestRenderJSON_PlanEchoed(t *testing.T) {
	d := Diff{Slug: "api", Plan: "hobby"}
	got := renderJSONBytes(t, d)
	// Plan shows up twice: once at the diff level (filled by Diff.Plan)
	// and once at the top level (filled by the wrapper struct).
	count := strings.Count(got, `"plan": "hobby"`)
	if count != 2 {
		t.Errorf("plan=hobby should appear twice (Diff + envelope); got %d occurrences in:\n%s", count, got)
	}
}

// --- Idempotence -----------------------------------------------------

func TestRenderJSON_Idempotent(t *testing.T) {
	// Running RenderJSON twice on the same Diff should produce the
	// same byte slice — the function does not mutate its argument.
	d := Diff{
		Slug: "api",
		Changes: []Change{
			{Field: "memory", Kind: ChangeModify, Before: AsAny(256), After: AsAny(512)},
			{Field: "concurrency", Kind: ChangeModify, Before: AsAny(2), After: AsAny(4)},
		},
	}
	a := renderJSONBytes(t, d)
	b := renderJSONBytes(t, d)
	if a != b {
		t.Errorf("RenderJSON not idempotent; first=len %d, second=len %d", len(a), len(b))
	}
}

// --- json.NewEncoder indents output --------------------------------

func TestRenderJSON_IndentedOutput(t *testing.T) {
	d := Diff{Slug: "api"}
	var buf bytes.Buffer
	if err := RenderJSON(&buf, d); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	// Encoders emit a trailing newline; the JSON must be valid json.Unmarshal-target.
	var decoded map[string]any
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("RenderJSON output is not valid JSON: %v\nraw: %s", err, buf.String())
	}
	if decoded["slug"] != "api" {
		t.Errorf("slug round-trip wrong; got %v", decoded["slug"])
	}
	if _, ok := decoded["diff"]; !ok {
		t.Errorf("diff envelope missing; got keys: %v", keys(decoded))
	}
}

// --- isHeadlineScalar coverage via RenderText regression guard ------

func TestRenderJSON_DoesNotMutateInput(t *testing.T) {
	original := []Change{
		{Field: "z_field", Kind: ChangeModify, Before: AsAny(1), After: AsAny(2)},
		{Field: "a_field", Kind: ChangeModify, Before: AsAny(10), After: AsAny(20)},
	}
	d := Diff{Slug: "api", Changes: original}
	_ = renderJSONBytes(t, d)
	if original[0].Field != "z_field" || original[1].Field != "a_field" {
		t.Errorf("RenderJSON mutated input slice: %+v", original)
	}
}

// keys is a tiny helper to print map keys for assertion failures.
func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
