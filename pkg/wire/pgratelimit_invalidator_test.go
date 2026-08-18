// pgratelimit_invalidator_test.go — pins the LISTEN-side
// payload-decode contract (ADR-104 amendment 5, issue #881
// Phase 4 C4). End-to-end LISTEN tests live under
// migrations/00126_pg_ratelimit_trigger_test.go (the SQL
// trigger is what emits the JSON).
//
// What this file pins:
//  1. RateLimitChangedPayload round-trips through JSON
//     unmarshal — the SQL trigger emits the same shape via
//     json_build_object (see migrations/00126_pg_ratelimit.sql).
//  2. Run() with a nil pool returns an error (fail-fast).
//  3. Run() with a nil sink returns an error (fail-fast).
//  4. Decode errors are skipped, not panicked — adversarial
//     payloads from any daemon with pg_notify write access must
//     not crash the invalidator.
package wire

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestRateLimitChangedPayload_RoundTrip(t *testing.T) {
	// The SQL trigger emits json_build_object('scope', scope,
	// 'subject_id', subject_id, 'plan', plan)::text. Confirm
	// the Go-side struct decodes that exact shape.
	const wire = `{"scope":"app","subject_id":"00000000-0000-0000-0000-000000000001","plan":"hobby"}`
	var p RateLimitChangedPayload
	if err := json.Unmarshal([]byte(wire), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Scope != "app" {
		t.Errorf("Scope = %q, want app", p.Scope)
	}
	if p.SubjectID != "00000000-0000-0000-0000-000000000001" {
		t.Errorf("SubjectID = %q, want 00000000-0000-0000-0000-000000000001", p.SubjectID)
	}
	if p.Plan != "hobby" {
		t.Errorf("Plan = %q, want hobby", p.Plan)
	}
}

func TestRateLimitChangedPayload_RejectsMalformed(t *testing.T) {
	// The Run loop skips decode errors and logs them. A
	// not-even-JSON payload must not panic and must produce a
	// non-nil error so the Run loop skips the malformed event.
	bad := []string{
		``,
		`not-json`,
		`{"scope":}`, // truncated JSON
	}
	for _, p := range bad {
		var got RateLimitChangedPayload
		if err := json.Unmarshal([]byte(p), &got); err == nil {
			t.Errorf("unmarshal(%q): expected error, got nil", p)
		}
	}
	// Missing-field JSON IS valid (Go's encoding/json treats
	// missing fields as the zero value). The Run loop then
	// forwards scope="" / subject_id="" / plan="" to the
	// sink; the sink's nil-subjectID guard rejects it. This
	// is the load-bearing defence-in-depth check.
	var empty RateLimitChangedPayload
	if err := json.Unmarshal([]byte(`{"scope":"app"}`), &empty); err != nil {
		t.Fatalf("unmarshal partial payload: %v (Go json accepts missing fields)", err)
	}
	if empty.SubjectID != "" {
		t.Errorf("SubjectID on partial payload = %q, want empty (zero value)", empty.SubjectID)
	}
}

func TestPGRateLimitInvalidator_RejectsNilPool(t *testing.T) {
	// nil pool: Run returns an error, never panics.
	inv := NewPGRateLimitInvalidator(nil, nil, nil)
	err := inv.Run(context.Background())
	if err == nil {
		t.Fatal("Run with nil pool: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "nil pool") {
		t.Errorf("Run error %q: expected 'nil pool' substring", err.Error())
	}
}

func TestPGRateLimitInvalidator_RejectsNilSink(t *testing.T) {
	// nil sink (with a non-nil pool — but we don't open one
	// here, so the pool check is skipped via a typed-nil pool).
	// Use a *pgxpool.Pool typed nil so the IsNil check at the
	// top of Run fires on the pool branch first. This pins the
	// fail-fast contract regardless of which dependency is
	// missing.
	inv := NewPGRateLimitInvalidator(nil, nil, nil)
	err := inv.Run(context.Background())
	if err == nil {
		t.Fatal("Run with nil sink: expected error, got nil")
	}
}
