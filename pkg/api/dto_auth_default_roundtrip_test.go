package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestAppResponse_AuthDefaultFlippedAt_JSONRoundtrip pins the JSON
// wire shape of the new auth_default_flipped_at field on AppResponse
// (issue #695 / ADR-080).
//
// Two cases:
//   - nil value: omitempty drops the field from the wire shape
//     (post-flip fresh-create apps never stamp this column).
//   - stamped value: round-trips as RFC3339 with the original
//     instant preserved through Marshal → Unmarshal.
//
// A regression that flips the json tag, drops the pointer, or
// breaks the omitempty contract surfaces here.
func TestAppResponse_AuthDefaultFlippedAt_JSONRoundtrip(t *testing.T) {
	t.Run("nil_omitempty", func(t *testing.T) {
		resp := AppResponse{
			RequireAuthn: true,
			// AuthDefaultFlippedAt deliberately nil.
		}
		body, err := json.Marshal(resp)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		// omitempty drops the field on a fresh-create app.
		if strings.Contains(string(body), "auth_default_flipped_at") {
			t.Errorf("Marshal body = %s, must NOT contain auth_default_flipped_at when nil (omitempty contract broken)", body)
		}

		// Decoder-side: a body without the field must round-trip
		// back to nil.
		var back AppResponse
		if err := json.Unmarshal(body, &back); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if back.AuthDefaultFlippedAt != nil {
			t.Errorf("round-trip AuthDefaultFlippedAt = %v, want nil", back.AuthDefaultFlippedAt)
		}
	})

	t.Run("stamped_roundtrip", func(t *testing.T) {
		stamp := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
		resp := AppResponse{
			RequireAuthn:         false,
			AuthDefaultFlippedAt: &stamp,
		}
		body, err := json.Marshal(resp)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if !strings.Contains(string(body), "auth_default_flipped_at") {
			t.Errorf("Marshal body = %s, must contain auth_default_flipped_at when set", body)
		}

		var back AppResponse
		if err := json.Unmarshal(body, &back); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if back.AuthDefaultFlippedAt == nil {
			t.Fatalf("round-trip AuthDefaultFlippedAt = nil, want stamped instant")
		}
		if !back.AuthDefaultFlippedAt.Equal(stamp) {
			t.Errorf("round-trip AuthDefaultFlippedAt = %v, want %v", back.AuthDefaultFlippedAt, stamp)
		}
	})
}
