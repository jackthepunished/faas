package auditutil

import (
	"testing"
)

// TestWithResult_NilDataReturnsEmptyMap pins the contract that a
// nil input gets an allocated empty map back. Callers rely on this
// so they can use the single-line `data := WithResult(payload, "ok")`
// shape without nil-checking before the JSON marshal.
func TestWithResult_NilDataReturnsEmptyMap(t *testing.T) {
	got := WithResult(nil, "success")
	if got == nil {
		t.Fatal("WithResult(nil, _) = nil, want non-nil empty map")
	}
	if len(got) != 1 {
		t.Errorf("len = %d, want 1 (just the result field)", len(got))
	}
	if got["result"] != "success" {
		t.Errorf("result = %v, want %q", got["result"], "success")
	}
}

// TestWithResult_EmptyResultLeavesDataUntouched pins that passing
// "" as result is a no-op. The auditor's success path always knows
// the result; legacy call sites without a meaningful result must
// not be forced to add one. This is the escape hatch for the
// "caller has no semantic outcome" case.
func TestWithResult_EmptyResultLeavesDataUntouched(t *testing.T) {
	data := map[string]any{"kind": "edge_rule.cors_matched"}
	got := WithResult(data, "")
	if _, ok := got["result"]; ok {
		t.Errorf("empty result stamped a field: %v", got)
	}
	if got["kind"] != "edge_rule.cors_matched" {
		t.Errorf("kind was mutated: %v", got)
	}
}

// TestWithResult_CallerValueWins pins the override-safety contract:
// if a call site has already set data["result"], the helper does NOT
// overwrite it. This matters for emit sites that want to encode a
// finer-grained result than the binary success/error — e.g. an
// implicit deny emit might stamp "result":"error,code=im403".
func TestWithResult_CallerValueWins(t *testing.T) {
	data := map[string]any{"result": "error:code=im403"}
	got := WithResult(data, "success")
	if got["result"] != "error:code=im403" {
		t.Errorf("result = %v, want %q (caller's value must win)",
			got["result"], "error:code=im403")
	}
}

// TestWithResult_StampsResultWhenAbsent is the happy-path twin of
// the override test: when the caller hasn't set result, the helper
// stamps the supplied value. This is the load-bearing case for the
// 25+ emit sites that adopt `data := WithResult(payload, "success")`.
func TestWithResult_StampsResultWhenAbsent(t *testing.T) {
	data := map[string]any{"kind": "edge_rule.cors_matched"}
	got := WithResult(data, "success")
	if got["result"] != "success" {
		t.Errorf("result = %v, want %q", got["result"], "success")
	}
	if got["kind"] != "edge_rule.cors_matched" {
		t.Errorf("kind was mutated: %v", got)
	}
}

// TestWithResult_DoesNotCopyInput pins the documented contract that
// the helper does NOT defensively copy. Both auditors mutate their
// data parameter (pkg/audit adds trace_id / span_id; cmd/gatewayd-
// internal/audit.go adds similar fields). A defensive copy here
// would silently break that pattern — this test pins the behaviour
// so a future contributor who adds a copy-on-write sees a red flag.
func TestWithResult_DoesNotCopyInput(t *testing.T) {
	data := map[string]any{"kind": "edge_rule.cors_matched"}
	got := WithResult(data, "success")
	if len(data) != 2 {
		t.Errorf("input map len = %d, want 2 (helper must mutate, not copy)",
			len(data))
	}
	if got["result"] != "success" {
		t.Errorf("returned map result = %v, want %q", got["result"], "success")
	}
}
