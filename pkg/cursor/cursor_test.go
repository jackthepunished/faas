package cursor

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// jsonMarshal is a tiny helper so the table-driven test does not
// pull in encoding/json directly at the test scope; the test only
// covers the cursor package's Decode-vs-malformed-input contract.
func jsonMarshal(k Key) ([]byte, error) { return json.Marshal(k) }

func TestEncodeEmptyKey(t *testing.T) {
	t.Parallel()
	if got := Encode(Key{}); got != "" {
		t.Fatalf("Encode(Key{}) = %q, want \"\"", got)
	}
	if got := Encode(Key{ID: "abc"}); got != "" {
		// Missing CreatedAt is treated as zero.
		t.Fatalf("Encode(missing CreatedAt) = %q, want \"\"", got)
	}
	if got := Encode(Key{CreatedAt: time.Now()}); got != "" {
		t.Fatalf("Encode(missing ID) = %q, want \"\"", got)
	}
}

func TestDecodeEmpty(t *testing.T) {
	t.Parallel()
	k, err := Decode("")
	if err != nil {
		t.Fatalf("Decode(\"\") err = %v, want nil", err)
	}
	if !k.IsZero() {
		t.Fatalf("Decode(\"\") = %+v, want zero Key", k)
	}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	want := Key{
		CreatedAt: time.Date(2026, 8, 6, 12, 34, 56, 0, time.UTC),
		ID:        "5b2a9b4f-4f3d-4d6f-9a32-1a4d9b2c8e1f",
	}
	c := Encode(want)
	if c == "" {
		t.Fatalf("Encode round-trip returned empty")
	}
	got, err := Decode(c)
	if err != nil {
		t.Fatalf("Decode err = %v", err)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, want.CreatedAt)
	}
	if got.ID != want.ID {
		t.Errorf("ID = %q, want %q", got.ID, want.ID)
	}
}

func TestRoundTripPreservesPadded(t *testing.T) {
	t.Parallel()
	// A bare string like "a" base64-url-encodes to "YQ==" (with
	// padding). Some clients treat "=" as a query-string separator;
	// the cursor package signs everything URL-safe so the encoded
	// value is round-trip safe in url.Values (Go's url.QueryEscape
	// does NOT escape '=', but the encoder is stable).
	c := Encode(Key{
		CreatedAt: time.Unix(1, 0).UTC(),
		ID:        "a",
	})
	if !strings.Contains(c, "=") {
		t.Fatalf("expected padded base64, got %q", c)
	}
	got, err := Decode(c)
	if err != nil {
		t.Fatalf("Decode err = %v", err)
	}
	if got.ID != "a" {
		t.Errorf("ID = %q, want \"a\"", got.ID)
	}
}

func TestDecodeMalformedBase64(t *testing.T) {
	t.Parallel()
	_, err := Decode("not!base64")
	if err == nil {
		t.Fatalf("Decode(\"not!base64\") err = nil, want error")
	}
	if !strings.Contains(err.Error(), "base64") {
		t.Errorf("err = %v, want base64-prefixed", err)
	}
}

func TestDecodeMalformedJSON(t *testing.T) {
	t.Parallel()
	bad := base64.URLEncoding.EncodeToString([]byte("not json"))
	_, err := Decode(bad)
	if err == nil {
		t.Fatalf("Decode(bad json) err = nil, want error")
	}
	if !strings.Contains(err.Error(), "json") {
		t.Errorf("err = %v, want json-prefixed", err)
	}
}

func TestDecodeMissingFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		key  Key
	}{
		{"missing created_at", Key{ID: "abc"}},
		{"missing id", Key{CreatedAt: time.Now()}},
		{"both empty", Key{}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw, err := jsonMarshal(tc.key)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			c := base64.URLEncoding.EncodeToString(raw)
			_, err = Decode(c)
			if err == nil {
				t.Fatalf("Decode(%s) err = nil, want error", tc.name)
			}
			if !strings.Contains(err.Error(), "missing") {
				t.Errorf("err = %v, want missing-prefixed", err)
			}
		})
	}
}
