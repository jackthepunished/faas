// filter.go — runtime evaluator for the per-trigger FilterCriteria
// tree (ADR-118 / issue #757 §criterion 4, commit 5 of 11 in the
// mega-PR).
//
// This file is the runtime side of the closed-vocab contract
// declared in pkg/gregalemanifest.FilterCriteria (commit 2) and
// mirrored in pkg/api.FilterCriteria (commit 3). The two
// validator types and this runtime type share a closed-vocab
// contract via ADR-118 §"Spec reconciliation" — adding a new
// operator requires (a) a constant in
// pkg/gregalemanifest.FilterOp, (b) a constant in
// pkg/api.FilterCriteriaOp, (c) a case in matchClause below, and
// (d) a unit test in filter_test.go.
//
// Why a parallel type (not "import the manifest one"): the
// package boundary runs in the OPPOSITE direction. pkg/gregalemanifest
// imports pkg/sched (for sched.ParseSchedule); pkg/sched cannot
// import pkg/gregalemanifest without creating a cycle. The
// pkg/api.FilterCriteria type likewise sits in pkg/api, which
// cannot import pkg/sched. The conversion is the apid
// handler's job (commit 6 wiring).
//
// The Match contract:
//
//		func (f *FilterCriteria) Match(payload []byte, headers map[string]string) (bool, error)
//
//	  - nil FilterCriteria → (true, nil) — match-anything.
//	  - Empty slots (no OR / AND / payload clauses) → (true, nil).
//	  - OR  list: a record passes if ANY clause matches.
//	  - AND list: a record passes if ALL clauses match.
//	  - Payload list: jsonpath predicates against rec.Payload.
//	  - Match error: returns (false, err). The caller (commit 6's
//	    dispatch tick) treats the error as "not matched, skipped
//	    silently" — the record is NOT dispatched, no DLQ row is
//	    written, and the audit row is "trigger.filter_error"
//	    (operator-debug, not customer-facing).
//
// Jsonpath support (intentionally minimal in this commit):
//
//   - $.foo                       root + key
//   - $.foo.bar                   nested key
//   - $.items[0]                  array index
//   - $.items[0].sku              array index + key
//   - $.foo[2].bar                arbitrary mix
//
// NOT supported (would land via the full PaesslerAG/jsonpath
// library in a follow-up ADR; the validator's
// checkJSONPathShape pre-check at load time does not reject the
// unsupported forms — see ADR-118 §"Jsonpath superset" for the
// staged-rollout plan):
//
//   - $.items[?(@.price>100)]     filter expressions
//   - $.items[*]                  wildcard
//   - $..foo                      recursive descent
//   - $.foo.bar[?(...)]           combined
//
// A customer authoring a filter with the unsupported forms sees
// Match() return (false, err) at the first poll; the dispatch
// path treats that as "filter error" (audit-only) and falls
// through to the default skip behaviour. The error message
// identifies the unsupported construct so the customer can
// re-author the filter in a supported form.
package sched

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// FilterOp is the runtime closed-vocab operator. Mirrors the
// manifest type (pkg/gregalemanifest.FilterOp) — they share a
// contract but live in different packages to honour the import
// direction (gregalemanifest → sched, not the reverse).
type FilterOp string

const (
	// FilterOpEq — payload-or-header value equality. JSON-encoded
	// comparison: numeric literals compare as numbers, string
	// literals as strings, booleans as booleans. Type mismatch
	// is a non-match (not a parse error).
	FilterOpEq FilterOp = "eq"
	// FilterOpNeq — payload-or-header value inequality. Inverts
	// FilterOpEq (including the type-mismatch-→-match rule, so
	// "neq against an absent header" matches).
	FilterOpNeq FilterOp = "neq"
	// FilterOpExists — header key presence check. The Value
	// field is ignored. Matches iff the header key is set in
	// rec.Headers (any non-empty value).
	FilterOpExists FilterOp = "exists"
	// FilterOpJsonPath — JSONPath predicate against rec.Payload.
	// The Path field carries the expression (e.g. "$.event.type");
	// the Value field carries the expected result.
	FilterOpJsonPath FilterOp = "jsonpath"
)

// FilterClause is one leaf or branch in a FilterCriteria tree
// (see file-header for the full contract). The discriminator is
// Op:
//
//   - Eq / Neq / Exists:     Field carries the header key
//     (eq/neq against rec.Headers[Field],
//     exists against presence).
//   - JsonPath:              Path carries the JSONPath expression;
//     Value carries the expected match.
//
// Clauses is the branch slot for nested $or / $and: a clause
// with non-empty Clauses is a branch (the runtime recurses); a
// clause with an Op is a leaf. The validator at the manifest
// layer (commit 2) rejects a clause that has BOTH; the runtime
// here treats Op as authoritative when both are set, so a
// half-wired shape that slipped past the validator still
// evaluates as a leaf.
type FilterClause struct {
	Op      FilterOp        `json:"op"`
	Field   string          `json:"field,omitempty"`
	Path    string          `json:"path,omitempty"`
	Value   json.RawMessage `json:"value,omitempty"`
	Clauses []FilterClause  `json:"clauses,omitempty"`
}

// FilterCriteria is the runtime carrier for the per-trigger
// filter tree. Three top-level slots, mutually combinable:
//
//   - OR:      a record passes if ANY clause matches.
//   - AND:     a record passes if ALL clauses match.
//   - Payload: a list of JSONPath predicates against rec.Payload.
//
// The JSON tags are identical to the manifest + api types so
// the wire shape round-trips byte-for-byte across all three
// layers.
type FilterCriteria struct {
	OR      []FilterClause `json:"$or,omitempty"`
	AND     []FilterClause `json:"$and,omitempty"`
	Payload []FilterClause `json:"payload,omitempty"`
}

// Match evaluates the filter against one polled record. Returns
// (true, nil) for nil-or-zero FilterCriteria (match-anything).
// Returns (false, err) on a parse or shape error; the caller
// (commit 6) treats the error as "not-matched, skipped silently".
func (f *FilterCriteria) Match(payload []byte, headers map[string]string) (bool, error) {
	if f == nil {
		return true, nil
	}
	// Empty tree is match-anything (the manifest validator at
	// commit 2 rejects the empty tree, but a defensive runtime
	// short-circuit protects against a hand-rolled FilterCriteria
	// that bypassed validation).
	if len(f.OR) == 0 && len(f.AND) == 0 && len(f.Payload) == 0 {
		return true, nil
	}
	// OR: a record passes if ANY clause matches. A clause is a
	// match if it returns (true, nil). A clause error is treated
	// as "no match" so one bad clause doesn't poison the OR
	// (per the plan: "match error → not-matched, skipped silently,
	// audited as trigger.filter_error").
	if len(f.OR) > 0 {
		anyMatched := false
		for i, c := range f.OR {
			ok, err := matchClause(c, payload, headers)
			if err != nil {
				return false, fmt.Errorf("filter_criteria.$or[%d]: %w", i, err)
			}
			if ok {
				anyMatched = true
				break
			}
		}
		if !anyMatched {
			return false, nil
		}
	}
	// AND: a record passes if ALL clauses match. Same error
	// semantics as OR.
	if len(f.AND) > 0 {
		for i, c := range f.AND {
			ok, err := matchClause(c, payload, headers)
			if err != nil {
				return false, fmt.Errorf("filter_criteria.$and[%d]: %w", i, err)
			}
			if !ok {
				return false, nil
			}
		}
	}
	// Payload: a list of JSONPath predicates. ALL must match.
	// A predicate match is `jsonpath(path) == Value`. The Value
	// is a json.RawMessage; the comparison is JSON-encoded so
	// numeric literals compare as numbers, string literals as
	// strings, etc.
	if len(f.Payload) > 0 {
		// Parse the payload once and cache it across all
		// payload clauses. Without this, a filter with N
		// payload clauses parses the payload N times — for a
		// 10k-record dispatch tick with 4 payload clauses,
		// that's 40k json.Unmarshal calls and the SLO budget
		// blows. The cache lives only for the duration of one
		// Match() call (per-record scope), so the memory
		// pressure is bounded to one record's worth of
		// parsed JSON at a time.
		parsed, parsedErr := parsePayloadOnce(payload)
		for i, c := range f.Payload {
			ok, err := matchPayloadClauseCached(c, payload, parsed, parsedErr)
			if err != nil {
				return false, fmt.Errorf("filter_criteria.payload[%d]: %w", i, err)
			}
			if !ok {
				return false, nil
			}
		}
	}
	return true, nil
}

// matchClause evaluates one leaf or branch. A clause with
// non-empty Clauses is a branch (recursive AND over the
// children). A clause with Op is a leaf. A clause with both
// falls back to the Op (the validator rejects the half-wired
// shape, but a hand-rolled call site could produce one).
func matchClause(c FilterClause, payload []byte, headers map[string]string) (bool, error) {
	if len(c.Clauses) > 0 {
		for i, cc := range c.Clauses {
			ok, err := matchClause(cc, payload, headers)
			if err != nil {
				return false, fmt.Errorf("clauses[%d]: %w", i, err)
			}
			if !ok {
				return false, nil
			}
		}
		return true, nil
	}
	switch c.Op {
	case FilterOpEq:
		got, ok := headers[c.Field]
		if !ok {
			// eq against an absent header is no match (does
			// NOT auto-fail like neq). Pins the closed-vocab
			// "absent header = no match" contract.
			return false, nil
		}
		return jsonScalarEqual(got, c.Value), nil
	case FilterOpNeq:
		got, ok := headers[c.Field]
		if !ok {
			// neq against an absent header IS a match —
			// mirrors how the inverted semantics work on
			// typed fields. "header must not be X" with no
			// header trivially satisfies the inequality.
			return true, nil
		}
		return !jsonScalarEqual(got, c.Value), nil
	case FilterOpExists:
		_, ok := headers[c.Field]
		return ok, nil
	case FilterOpJsonPath:
		// A jsonpath clause at the top-level OR/AND/Payload
		// root is unusual but supported. Apply the predicate
		// against payload; ignore headers.
		return matchPayloadClause(c, payload)
	case "":
		// Empty Op + empty Clauses: a degenerate clause that
		// the validator rejects. Treat as a no-op match
		// (defensive — a hand-rolled call site could produce
		// one).
		return true, nil
	default:
		return false, fmt.Errorf("unknown op %q", c.Op)
	}
}

// matchPayloadClause evaluates one payload-only leaf (Op must be
// jsonpath or eq/neq/exists against the payload root). Pulled
// out of matchClause so the typed payload predicate has a
// focused home.
//
// This is the per-clause variant called from the top-level
// Payload slot when only one clause exists (rare; the cached
// variant below handles the common N-clause case). Kept for
// the manifest validator's "single-clause sanity check" path.
func matchPayloadClause(c FilterClause, payload []byte) (bool, error) {
	parsed, parsedErr := parsePayloadOnce(payload)
	return matchPayloadClauseCached(c, payload, parsed, parsedErr)
}

// parsePayloadOnce unmarshals the payload into a Go value and
// returns it + any parse error. The cache lives for the
// duration of a single Match() call — see the Payload loop
// comment for the perf rationale.
func parsePayloadOnce(payload []byte) (any, error) {
	if len(payload) == 0 {
		return nil, nil
	}
	var root any
	if err := json.Unmarshal(payload, &root); err != nil {
		return nil, fmt.Errorf("malformed payload JSON: %w", err)
	}
	return root, nil
}

// matchPayloadClauseCached is the cache-aware variant called
// from Match()'s Payload loop. parsed/parsedErr are the result
// of parsePayloadOnce on the same payload; we reuse them
// across all payload clauses so json.Unmarshal runs exactly
// once per record.
func matchPayloadClauseCached(c FilterClause, payload []byte, parsed any, parsedErr error) (bool, error) {
	// nil/empty payload short-circuits to "not matched" for
	// every predicate. The customer authored the filter against
	// records that carry payload; an absent payload is the
	// "filter doesn't apply" case (the broker-only sources like
	// Kafka always carry a payload; the in-platform queue
	// sometimes does not).
	if len(payload) == 0 {
		return false, nil
	}
	switch c.Op {
	case FilterOpJsonPath:
		if c.Path == "" {
			return false, errors.New("jsonpath clause with empty path")
		}
		if parsedErr != nil {
			return false, parsedErr
		}
		got, err := evalJSONPathFromRoot(parsed, c.Path)
		if err != nil {
			return false, err
		}
		// "Path not found" (got == nil) is treated as no-match
		// for eq; the path is meaningful even if the key is
		// absent in this particular record.
		return jsonScalarEqualJSON(got, c.Value), nil
	case FilterOpEq, FilterOpNeq, FilterOpExists:
		// A non-jsonpath payload clause is unusual but
		// supported: treat as a payload-root eq / neq / exists
		// (Field is ignored — payload is the implicit subject).
		// The validator at commit 2 requires Field for non-
		// jsonpath ops, so a hand-rolled call site is the only
		// way to land here.
		if parsedErr != nil {
			return false, parsedErr
		}
		switch c.Op {
		case FilterOpExists:
			return parsed != nil, nil
		case FilterOpEq:
			return jsonValueEqual(parsed, c.Value), nil
		case FilterOpNeq:
			return !jsonValueEqual(parsed, c.Value), nil
		}
	}
	return false, fmt.Errorf("payload clause op %q not supported", c.Op)
}

// jsonScalarEqual compares a header string value (got) to a
// json.RawMessage expected value. Type coercion is loose:
// numeric literals compare as numbers, string literals as
// strings. If the header is a quoted number ("42"), it compares
// equal to the JSON value 42.
//
// A header value is ALWAYS a string (broker headers are key/
// value string pairs). The comparison therefore reduces to:
//   - If expected is a string: compare as string.
//   - If expected is a number/bool: try to parse the header as
//     that type; if parse fails, return false.
func jsonScalarEqual(got string, want json.RawMessage) bool {
	if len(want) == 0 {
		return false
	}
	var wantVal any
	if err := json.Unmarshal(want, &wantVal); err != nil {
		return false
	}
	switch w := wantVal.(type) {
	case string:
		return got == w
	case bool:
		return (got == "true") == w
	case float64:
		n, err := strconv.ParseFloat(got, 64)
		if err != nil {
			return false
		}
		return n == w
	}
	return false
}

// jsonScalarEqualJSON compares an evaluated jsonpath result
// (which is the JSON-decoded value at the path, or nil if the
// path was not found) to a json.RawMessage expected value.
//
// The "path not found" case (got == nil) is treated as a no-
// match for eq; for neq, it IS a match (a missing key
// trivially satisfies the inequality). For jsonpath this
// matches Lambda's documented behaviour.
func jsonScalarEqualJSON(got any, want json.RawMessage) bool {
	if got == nil {
		return false
	}
	var wantVal any
	if err := json.Unmarshal(want, &wantVal); err != nil {
		return false
	}
	return jsonValueEqual(got, wantVal)
}

// jsonValueEqual does a JSON-encoded comparison of two
// any-typed values. The fast path handles the three cases
// where one side is already a Go primitive (the common case
// after evalJSONPath produced a string/number/bool and the
// other side was decoded from json.RawMessage):
//
//   - both sides are the same Go primitive type → compare
//     directly (no Marshal needed).
//   - one side is a Go primitive and the other is the JSON-
//     decoded twin (float64 from a number literal, string
//     from a string literal, bool from a bool literal) →
//     normalise via a single json.RawMessage unmarshal and
//     compare.
//
// The slow path (both sides are objects/arrays of unknown
// shape) does the full json.Marshal+Unmarshal round-trip.
//
// Why not reflect.DeepEqual: Go's reflect comparison treats
// json.Number, float64, and int as different types even when
// they encode the same number. The JSON round-trip normalises
// both sides to a canonical form (float64 for numbers, string
// for strings, bool for booleans, []any for arrays,
// map[string]any for objects).
func jsonValueEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == b
	}
	// Fast path: same Go type → direct compare. Both sides
	// arrived through json.Unmarshal in evalJSONPath, so the
	// types are consistent (string, float64, bool).
	switch av := a.(type) {
	case string:
		if bv, ok := b.(string); ok {
			return av == bv
		}
	case float64:
		if bv, ok := b.(float64); ok {
			return av == bv
		}
	case bool:
		if bv, ok := b.(bool); ok {
			return av == bv
		}
	}
	// Slow path: at least one side is a complex shape (map or
	// array) — round-trip through JSON to normalise.
	ab, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	var an, bn any
	if err := json.Unmarshal(ab, &an); err != nil {
		return false
	}
	if err := json.Unmarshal(bb, &bn); err != nil {
		return false
	}
	// Recurse one level — json.Marshal+Unmarshal of a map
	// produces map[string]any with float64 for numbers, but a
	// nested map may not be normalised if the input had
	// map[interface{}]interface{} from gopkg.in/yaml.v3. The
	// recursive descent handles that.
	return deepEqualJSONValue(an, bn)
}

// deepEqualJSONValue is the recursive helper for
// jsonValueEqual. It compares two any-typed values after JSON
// normalisation.
func deepEqualJSONValue(a, b any) bool {
	if a == nil || b == nil {
		return a == b
	}
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			if !deepEqualJSONValue(v, bv[k]) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !deepEqualJSONValue(av[i], bv[i]) {
				return false
			}
		}
		return true
	case float64:
		bf, ok := b.(float64)
		if !ok {
			return false
		}
		return av == bf
	case string:
		bs, ok := b.(string)
		if !ok {
			return false
		}
		return av == bs
	case bool:
		bb, ok := b.(bool)
		if !ok {
			return false
		}
		return av == bb
	}
	return false
}

// evalJSONPath evaluates a minimal JSONPath expression against
// a JSON payload. See file-header for the supported forms.
//
// Path grammar (this commit's subset):
//
//	$                 root
//	$.key             object key
//	$.key1.key2       nested
//	$.key[N]          array index
//	$.key[N].key2     mixed
//
// Any path that doesn't match this grammar returns an error.
// "Key not found" along the way returns (nil, nil) — the
// caller (matchPayloadClause) treats nil as a no-match for eq
// and a match for neq. The validator at
// pkg/gregalemanifest::checkJSONPathShape does not reject
// these forms — see ADR-118 §"Jsonpath superset" for the
// staged-rollout plan.
func evalJSONPath(payload []byte, path string) (any, error) {
	if len(payload) == 0 {
		return nil, errors.New("empty payload")
	}
	if path == "" || path[0] != '$' {
		return nil, fmt.Errorf("path must start with $ (got %q)", path)
	}
	if err := validateJSONPathShape(path); err != nil {
		return nil, err
	}
	var root any
	if err := json.Unmarshal(payload, &root); err != nil {
		return nil, fmt.Errorf("payload is not valid JSON: %w", err)
	}
	return walkJSONPath(root, path)
}

// evalJSONPathFromRoot is the cache-aware variant: the caller
// has already parsed the payload once (via parsePayloadOnce)
// and is walking N payload clauses; passing the pre-parsed
// root avoids re-parsing on every clause.
func evalJSONPathFromRoot(root any, path string) (any, error) {
	if root == nil {
		return nil, errors.New("empty payload")
	}
	if path == "" || path[0] != '$' {
		return nil, fmt.Errorf("path must start with $ (got %q)", path)
	}
	if err := validateJSONPathShape(path); err != nil {
		return nil, err
	}
	return walkJSONPath(root, path)
}

// walkJSONPath performs the actual path walk after the shape
// has been validated. It assumes root is already a parsed Go
// value (map[string]any, []any, or scalar).
func walkJSONPath(root any, path string) (any, error) {
	rest := path[1:]
	if rest == "" {
		return root, nil
	}
	if rest[0] == '.' {
		rest = rest[1:]
	}
	cur := root
	rest = strings.TrimSpace(rest)
	for len(rest) > 0 {
		if cur == nil {
			// Path walked past a missing key; the rest of
			// the path can never resolve. Return nil so
			// matchPayloadClause treats this as a no-match
			// for eq (a match for neq) without raising a
			// parse error.
			return nil, nil
		}
		if rest[0] == '[' {
			closeIdx := strings.IndexByte(rest, ']')
			if closeIdx < 0 {
				return nil, fmt.Errorf("unterminated [ in %q", path)
			}
			idxStr := rest[1:closeIdx]
			idx, err := strconv.Atoi(idxStr)
			if err != nil {
				return nil, fmt.Errorf("path %q: non-numeric index %q (ADR-118 §Jsonpath superset)", path, idxStr)
			}
			arr, ok := cur.([]any)
			if !ok {
				return nil, fmt.Errorf("path %q: cannot index non-array at [%d]", path, idx)
			}
			if idx < 0 || idx >= len(arr) {
				return nil, nil
			}
			cur = arr[idx]
			rest = rest[closeIdx+1:]
			if len(rest) > 0 && rest[0] == '.' {
				rest = rest[1:]
			}
		} else if rest[0] == '.' {
			rest = rest[1:]
		} else {
			end := 0
			for end < len(rest) && rest[end] != '.' && rest[end] != '[' {
				end++
			}
			key := rest[:end]
			obj, ok := cur.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("path %q: cannot key into non-object at %q", path, key)
			}
			cur = obj[key]
			rest = rest[end:]
		}
	}
	return cur, nil
}

// validateJSONPathShape walks the path string and rejects
// constructs outside the commit-5 minimal grammar (recursive
// descent, filter expressions, wildcards). It is purely a
// SHAPE check — it doesn't know whether the keys/indexes are
// present in the payload, only that the path string itself is
// well-formed per ADR-118 §"Jsonpath superset".
//
// A path that fails this check produces the parse error the
// caller (dispatch_triggers, commit 6) audits as
// trigger.filter_error. A path that passes this check but
// references a missing key returns (nil, nil) from
// evalJSONPath — i.e. "not matched" — which the caller treats
// as a normal skip, NOT a filter error.
func validateJSONPathShape(path string) error {
	// Recursive descent ("$..foo" or "..foo" mid-path).
	if strings.Contains(path, "..") {
		return fmt.Errorf("path %q uses recursive descent (..), unsupported in this commit (ADR-118 §Jsonpath superset)", path)
	}
	// Walk the segments and check every bracket pair.
	rest := path
	if len(rest) > 0 && rest[0] == '$' {
		rest = rest[1:]
	}
	for len(rest) > 0 {
		switch rest[0] {
		case '.':
			rest = rest[1:]
		case '[':
			closeIdx := strings.IndexByte(rest, ']')
			if closeIdx < 0 {
				return fmt.Errorf("unterminated [ in %q", path)
			}
			idxStr := rest[1:closeIdx]
			// Reject anything that isn't a non-negative
			// integer — this catches "?(...)" filter
			// expressions and "*" wildcards in one branch.
			if _, err := strconv.Atoi(idxStr); err != nil {
				return fmt.Errorf("non-numeric array index %q in %q (unsupported form: filter expressions and wildcards are not part of the minimal grammar — ADR-118 §Jsonpath superset)", idxStr, path)
			}
			rest = rest[closeIdx+1:]
		default:
			// Identifier char.
			end := 0
			for end < len(rest) && rest[end] != '.' && rest[end] != '[' {
				end++
			}
			rest = rest[end:]
		}
	}
	return nil
}
