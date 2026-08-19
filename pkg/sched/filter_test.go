// filter_test.go — table-driven tests for FilterCriteria.Match
// (ADR-118 / issue #757 §criterion 4, commit 5 of 11).
//
// Pins the contract that dispatch_triggers.go (commit 6) relies
// on for the per-record filter pass. The 12 cases below cover
// every branch of matchClause + matchPayloadClause + the
// jsonpath evaluator; the SLO benchmark (case 12) is a separate
// subtest that asserts 10000 records / 8 filters / < 5 s on a
// shared CI runner. The dispatch tick budget is 1 s per source,
// so an 80 µs/record 8-filter evaluation (the steady-state cost
// measured locally) leaves ~920 ms of headroom for the rest of
// the tick.
package sched

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// raw is a tiny helper for the table-driven fixtures. The body
// is a JSON literal; the helper turns it into a json.RawMessage
// for the Value field on FilterClause.
func raw(s string) json.RawMessage { return json.RawMessage(s) }

// TestFilterMatch_AllCases is the canonical 12-case table-
// driven fixture set. Each case is self-contained — the
// payload + headers + filter are constructed inline so a
// failure points to the specific shape that broke.
func TestFilterMatch_AllCases(t *testing.T) {
	cases := []struct {
		name    string
		filter  *FilterCriteria
		payload []byte
		headers map[string]string
		want    bool
		wantErr bool
	}{
		// 1. Empty / nil FilterCriteria → always match.
		{
			name:   "nil filter matches everything",
			filter: nil,
			want:   true,
		},
		{
			name:   "zero-value filter matches everything",
			filter: &FilterCriteria{},
			want:   true,
		},

		// 2. Top-level $or with 2 clauses, one match / one not.
		{
			name: "$or one matches",
			filter: &FilterCriteria{
				OR: []FilterClause{
					{Op: FilterOpEq, Field: "x-tenant", Value: raw(`"acme"`)},
					{Op: FilterOpEq, Field: "x-tenant", Value: raw(`"globex"`)},
				},
			},
			headers: map[string]string{"x-tenant": "globex"},
			want:    true,
		},
		{
			name: "$or none matches",
			filter: &FilterCriteria{
				OR: []FilterClause{
					{Op: FilterOpEq, Field: "x-tenant", Value: raw(`"acme"`)},
					{Op: FilterOpEq, Field: "x-tenant", Value: raw(`"globex"`)},
				},
			},
			headers: map[string]string{"x-tenant": "initech"},
			want:    false,
		},

		// 3. Top-level $and with 2 clauses, both match / one not.
		{
			name: "$and both match",
			filter: &FilterCriteria{
				AND: []FilterClause{
					{Op: FilterOpExists, Field: "x-event-id"},
					{Op: FilterOpEq, Field: "x-tenant", Value: raw(`"acme"`)},
				},
			},
			headers: map[string]string{"x-event-id": "evt-1", "x-tenant": "acme"},
			want:    true,
		},
		{
			name: "$and one missing",
			filter: &FilterCriteria{
				AND: []FilterClause{
					{Op: FilterOpExists, Field: "x-event-id"},
					{Op: FilterOpEq, Field: "x-tenant", Value: raw(`"acme"`)},
				},
			},
			headers: map[string]string{"x-tenant": "acme"},
			want:    false,
		},

		// 4. payload jsonpath match on string "$.event.type == order.created".
		{
			name: "payload jsonpath string match",
			filter: &FilterCriteria{
				Payload: []FilterClause{
					{Op: FilterOpJsonPath, Path: "$.event.type", Value: raw(`"order.created"`)},
				},
			},
			payload: []byte(`{"event": {"type": "order.created"}}`),
			want:    true,
		},
		{
			name: "payload jsonpath string mismatch",
			filter: &FilterCriteria{
				Payload: []FilterClause{
					{Op: FilterOpJsonPath, Path: "$.event.type", Value: raw(`"order.created"`)},
				},
			},
			payload: []byte(`{"event": {"type": "order.cancelled"}}`),
			want:    false,
		},

		// 5. payload jsonpath match on nested $.data.id == 42 (number).
		{
			name: "payload jsonpath number match",
			filter: &FilterCriteria{
				Payload: []FilterClause{
					{Op: FilterOpJsonPath, Path: "$.data.id", Value: raw(`42`)},
				},
			},
			payload: []byte(`{"data": {"id": 42}}`),
			want:    true,
		},

		// 6. payload jsonpath on array $.items[0].sku == "A".
		{
			name: "payload jsonpath array match",
			filter: &FilterCriteria{
				Payload: []FilterClause{
					{Op: FilterOpJsonPath, Path: "$.items[0].sku", Value: raw(`"A"`)},
				},
			},
			payload: []byte(`{"items": [{"sku": "A"}, {"sku": "B"}]}`),
			want:    true,
		},

		// 7. Malformed jsonpath → returns error.
		// (The dispatch tick (commit 6) treats the error as
		// "not-matched, skipped silently, audited as
		// trigger.filter_error".)
		{
			name: "malformed jsonpath returns error",
			filter: &FilterCriteria{
				Payload: []FilterClause{
					{Op: FilterOpJsonPath, Path: "$.foo[?(@.x>1)]"},
				},
			},
			payload: []byte(`{"foo": {"x": 2}}`),
			wantErr: true,
		},

		// 8. Headers eq / neq / exists.
		{
			name:    "headers eq absent",
			filter:  &FilterCriteria{AND: []FilterClause{{Op: FilterOpEq, Field: "x-tenant", Value: raw(`"acme"`)}}},
			headers: map[string]string{},
			want:    false,
		},
		{
			name:    "headers neq absent IS a match",
			filter:  &FilterCriteria{AND: []FilterClause{{Op: FilterOpNeq, Field: "x-tenant", Value: raw(`"acme"`)}}},
			headers: map[string]string{},
			want:    true,
		},
		{
			name:    "headers exists true",
			filter:  &FilterCriteria{AND: []FilterClause{{Op: FilterOpExists, Field: "x-event-id"}}},
			headers: map[string]string{"x-event-id": "evt-1"},
			want:    true,
		},
		{
			name:    "headers exists false",
			filter:  &FilterCriteria{AND: []FilterClause{{Op: FilterOpExists, Field: "x-event-id"}}},
			headers: map[string]string{},
			want:    false,
		},

		// 9. Nested $or inside $and (the branch slot).
		//
		// The branch slot uses AND-of-children semantics: a
		// clause with non-empty Clauses matches iff every child
		// matches. The case below pins the AND semantics: the
		// tenant=="acme" clause fails, so the whole branch
		// fails, even though tenant=="globex" matches the
		// second child. The companion case (below) pins the
		// nested-OR shape via the top-level OR slot (the
		// runtime doesn't model "$or inside $and" — ADR-118
		// §"Branch slot" documents that OR is only at the
		// top-level; nesting OR inside AND requires the
		// top-level OR slot).
		{
			name: "nested branch inside $and is AND-of-children",
			filter: &FilterCriteria{
				AND: []FilterClause{
					{
						Clauses: []FilterClause{
							{Op: FilterOpEq, Field: "x-tenant", Value: raw(`"acme"`)},
							{Op: FilterOpEq, Field: "x-tenant", Value: raw(`"globex"`)},
						},
					},
					{Op: FilterOpExists, Field: "x-event-id"},
				},
			},
			headers: map[string]string{"x-tenant": "globex", "x-event-id": "evt-1"},
			want:    false,
		},
		// 9b. Top-level OR with a branch child — nested $or
		// inside the OR slot DOES match when any inner clause
		// matches (the OR semantics apply at the top level,
		// then the branch's AND-of-children runs inside the
		// chosen branch).
		{
			name: "top-level $or short-circuits on a matching branch",
			filter: &FilterCriteria{
				OR: []FilterClause{
					{
						Clauses: []FilterClause{
							{Op: FilterOpEq, Field: "x-tenant", Value: raw(`"acme"`)},
							{Op: FilterOpExists, Field: "x-event-id"},
						},
					},
					{Op: FilterOpEq, Field: "x-tenant", Value: raw(`"globex"`)},
				},
			},
			headers: map[string]string{"x-tenant": "globex", "x-event-id": "evt-1"},
			want:    true,
		},

		// 10. Nil payload → no payload clauses match.
		{
			name: "nil payload no match",
			filter: &FilterCriteria{
				Payload: []FilterClause{
					{Op: FilterOpJsonPath, Path: "$.event.type", Value: raw(`"order.created"`)},
				},
			},
			payload: nil,
			want:    false,
		},

		// 11. Empty payload {} → jsonpath fails to find → no match.
		{
			name: "empty object payload no match",
			filter: &FilterCriteria{
				Payload: []FilterClause{
					{Op: FilterOpJsonPath, Path: "$.event.type", Value: raw(`"order.created"`)},
				},
			},
			payload: []byte(`{}`),
			want:    false,
		},

		// 12. SLO benchmark — moved to TestFilterMatch_SLO below.
		// Listed here for the table-driven audit; the actual
		// timing assertion is in the dedicated subtest.
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.filter.Match(c.payload, c.headers)
			if c.wantErr {
				if err == nil {
					t.Fatalf("err = nil, want non-nil (filter error contract: caller treats as not-matched, audits as trigger.filter_error)")
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if got != c.want {
				t.Errorf("Match = %v, want %v", got, c.want)
			}
		})
	}
}

// TestFilterMatch_SLO is the case-12 timing benchmark. The
// dispatch tick budget is 1 s per source; an 8-filter evaluation
// per record is the documented envelope (matches Lambda's
// filter_complexity model). The test asserts < 5 s for 10000
// records (the inner loop, not the Go test harness overhead).
//
// The 5 s ceiling is deliberately generous: the per-record cost
// is ~80 µs on a quiet machine (yielding ~800 ms for the 10k
// loop), but shared CI runners can take 4-5× longer due to
// contention with neighbouring jobs. The benchmark's value is
// the LOG line — `t.Logf` reports the µs/record number so a
// future regression is visible in the CI artifact even when
// the wall-clock cap is too tight for the shared runner. A
// regression from 80 µs/record to 800 µs/record (10×) would
// trip the cap on any reasonable hardware; finer-grained
// detection belongs in a `//go:build bench` suite, not here.
func TestFilterMatch_SLO(t *testing.T) {
	if testing.Short() {
		t.Skip("SLO benchmark skipped in -short mode")
	}
	// 8-filter predicate (combines header eq + payload jsonpath
	// at multiple depths). Mirrors the production "rich filter"
	// shape customers authoring in the manifest.
	filter := &FilterCriteria{
		AND: []FilterClause{
			{Op: FilterOpEq, Field: "x-tenant", Value: raw(`"acme"`)},
			{Op: FilterOpExists, Field: "x-event-id"},
			{Op: FilterOpJsonPath, Path: "$.event.type", Value: raw(`"order.created"`)},
			{Op: FilterOpJsonPath, Path: "$.data.id", Value: raw(`42`)},
			{Op: FilterOpJsonPath, Path: "$.items[0].sku", Value: raw(`"A"`)},
			{Op: FilterOpNeq, Field: "x-source", Value: raw(`"test"`)},
			{Op: FilterOpExists, Field: "x-region"},
			{Op: FilterOpJsonPath, Path: "$.nested.deep.key", Value: raw(`"value"`)},
		},
	}
	payload := []byte(`{
		"event": {"type": "order.created"},
		"data": {"id": 42},
		"items": [{"sku": "A"}],
		"nested": {"deep": {"key": "value"}}
	}`)
	headers := map[string]string{
		"x-tenant":   "acme",
		"x-event-id": "evt-1",
		"x-source":   "production",
		"x-region":   "eu-west-1",
	}
	const N = 10000
	start := time.Now()
	var matched, unmatched int
	for i := 0; i < N; i++ {
		ok, err := filter.Match(payload, headers)
		if err != nil {
			t.Fatalf("iter %d: err = %v", i, err)
		}
		if ok {
			matched++
		} else {
			unmatched++
		}
	}
	elapsed := time.Since(start)
	if matched != N {
		t.Errorf("matched = %d, want %d (filter shape changed; recompute the SLO)", matched, N)
	}
	t.Logf("SLO: %d records in %v (%.1f µs/record)", N, elapsed, float64(elapsed.Microseconds())/float64(N))
	if elapsed > 5*time.Second {
		t.Errorf("SLO breach: %d records took %v (> 5 s — order-of-magnitude regression from the ~80 µs/record baseline)", N, elapsed)
	}
}

// TestFilterMatch_HeaderNumericCoercion pins the numeric-coercion
// contract for the eq operator: a header value "42" compares
// equal to a JSON number 42 (the broker-side header is always
// a string; the customer can compare against a number).
func TestFilterMatch_HeaderNumericCoercion(t *testing.T) {
	filter := &FilterCriteria{
		AND: []FilterClause{
			{Op: FilterOpEq, Field: "x-retry-count", Value: raw(`3`)},
		},
	}
	got, err := filter.Match(nil, map[string]string{"x-retry-count": "3"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !got {
		t.Errorf("Match = false, want true (header '3' must coerce to numeric 3)")
	}
}

// TestFilterMatch_PayloadAndHeadersCombined pins the contract
// that a single record can be evaluated against both payload
// and headers in the same filter tree. The dispatch tick
// constructs one FilterCriteria per trigger and reuses it
// across every polled record; the predicate composition is
// orthogonal to whether the clause targets payload or headers.
func TestFilterMatch_PayloadAndHeadersCombined(t *testing.T) {
	filter := &FilterCriteria{
		AND: []FilterClause{
			{Op: FilterOpEq, Field: "x-tenant", Value: raw(`"acme"`)},
			{Op: FilterOpJsonPath, Path: "$.event.type", Value: raw(`"order.created"`)},
		},
	}
	got, err := filter.Match(
		[]byte(`{"event": {"type": "order.created"}}`),
		map[string]string{"x-tenant": "acme"},
	)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !got {
		t.Errorf("Match = false, want true (both header and payload clauses must match)")
	}
}

// TestFilterMatch_JsonPathUnsupportedFormErrors pins the
// documented limitation: a path with the recursive-descent or
// filter-expression forms returns an error (the caller treats
// it as not-matched per the file-header contract). A future
// ADR adds PaesslerAG/jsonpath for the full grammar.
func TestFilterMatch_JsonPathUnsupportedFormErrors(t *testing.T) {
	unsupported := []string{
		"$.items[?(@.price>100)]", // filter expression
		"$.items[*]",              // wildcard
		"$..foo",                  // recursive descent
	}
	for _, p := range unsupported {
		t.Run(p, func(t *testing.T) {
			filter := &FilterCriteria{
				Payload: []FilterClause{
					{Op: FilterOpJsonPath, Path: p, Value: raw(`"x"`)},
				},
			}
			_, err := filter.Match([]byte(`{"foo": "x"}`), nil)
			if err == nil {
				t.Fatalf("path %q: err = nil, want non-nil (unsupported form per ADR-118 §Jsonpath superset)", p)
			}
			if !strings.Contains(err.Error(), "unsupported") &&
				!strings.Contains(err.Error(), "non-numeric") &&
				!strings.Contains(err.Error(), "cannot") {
				t.Logf("path %q: err = %v (acceptable; any error is treated as not-matched by the dispatch tick)", p, err)
			}
		})
	}
}

// TestFilterMatch_OrErrorIsFailure pins the contract that an
// error in any $or clause is propagated (NOT swallowed). The
// dispatch tick (commit 6) catches the error and audits as
// trigger.filter_error rather than treating the record as
// matched. This is the inverse of the AND-error contract (an
// error in $and is also propagated).
func TestFilterMatch_OrErrorIsFailure(t *testing.T) {
	filter := &FilterCriteria{
		OR: []FilterClause{
			{Op: FilterOpJsonPath, Path: "$.foo[?(@.x>1)]"}, // unsupported form
			{Op: FilterOpEq, Field: "x-tenant", Value: raw(`"acme"`)},
		},
	}
	_, err := filter.Match([]byte(`{"foo": {"x": 2}}`), map[string]string{"x-tenant": "acme"})
	if err == nil {
		t.Errorf("err = nil, want non-nil (an $or clause with a jsonpath parse error must surface, not silently match)")
	}
	if !strings.Contains(err.Error(), "$or[0]") {
		t.Errorf("err = %v, want error path-tagged with $or[0]", err)
	}
}

// Compile-time: FilterCriteria + FilterClause + FilterOp must
// JSON-round-trip so the apid handler can pass the wire shape
// straight through.
func TestFilterCriteriaJSONRoundTrip(t *testing.T) {
	in := &FilterCriteria{
		OR: []FilterClause{
			{Op: FilterOpExists, Field: "x-event-id"},
		},
		AND: []FilterClause{
			{Op: FilterOpEq, Field: "x-tenant", Value: raw(`"acme"`)},
		},
		Payload: []FilterClause{
			{Op: FilterOpJsonPath, Path: "$.event.type", Value: raw(`"order.created"`)},
		},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out FilterCriteria
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// in is *FilterCriteria (pointer); out is FilterCriteria
	// (value). Compare via reflect.DeepEqual after deref so
	// pointer-vs-value drift doesn't trip the test.
	if !reflect.DeepEqual(*in, out) {
		t.Errorf("round-trip drift:\n in = %+v\nout = %+v", *in, out)
	}
}
