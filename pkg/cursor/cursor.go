// Package cursor encodes and decodes the compound (created_at, id)
// pagination cursor used by list endpoints whose primary ordering is
// (created_at DESC, id DESC).
//
// Background: the v1 cursor was the bare invitation ID; the SQL
// predicate `id::text < $cursor` against an `ORDER BY created_at DESC,
// id DESC` ordering is unsound under random UUIDs because two rows
// can have an inverted `id::text` comparison relative to the actual
// `created_at` ordering (call this regression "v1 cursor parity";
// it is documented at pkg/state/memstore.go's
// ListOrgInvitationsForOrgPage v1 doc-comment). PR-9 replaces the
// v1 cursor with the compound key here.
//
// Wire shape: base64.URLEncoding(URLEncoding includes padding
// characters `=`) of a JSON object with exactly two fields,
// `created_at` (RFC 3339) and `id`. The JSON shape is the public
// contract; the base64 encoding is the transport. Callers MUST NOT
// introspect the cursor — pass `next_before` back unchanged on the
// next request. Future migrations (e.g. an opaque v3 cursor) go
// through this same package so the wire stays in one place.
//
// Empty input round-trips: empty string decodes to Key{} (the
// "first page" / "no further page" case). Encode returns "" for
// any zero-value Key so the empty cursor is the canonical sentinel
// on the wire everywhere.
package cursor

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// Key is the compound (created_at, id) tuple used for cursor-
// paginated list endpoints. The zero value is the "empty key"
// sentinel — Encode returns "" for it and Decode returns it for "".
type Key struct {
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
}

// IsZero reports whether the key is the empty sentinel. Mirrors
// the zero-value check Encode performs before marshalling.
func (k Key) IsZero() bool {
	return k.CreatedAt.IsZero() || k.ID == ""
}

// Encode returns the base64-url-encoded JSON form of k. The empty
// key encodes to "" so the first-page case is the empty string on
// the wire (callers do not need to special-case the first page).
func Encode(k Key) string {
	if k.IsZero() {
		return ""
	}
	raw, err := json.Marshal(k)
	if err != nil {
		// json.Marshal of a time.Time + string can only fail on
		// unsupported value types; the struct fields are fixed.
		// Returning "" here matches the empty-case contract and
		// avoids a panic on the always-async cursor path.
		return ""
	}
	return base64.URLEncoding.EncodeToString(raw)
}

// Decode parses the cursor. Empty input returns Key{}, nil. Malformed
// base64 or JSON, or a JSON object missing either field, returns a
// non-nil error so the handler can surface a 400 validation_failed
// (the strict-mode pagination contract; the v1 cursor silently
// returned 0 rows on malformed input which made broken clients
// silently fall behind).
func Decode(s string) (Key, error) {
	if s == "" {
		return Key{}, nil
	}
	raw, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		return Key{}, fmt.Errorf("cursor: base64: %w", err)
	}
	var k Key
	if err := json.Unmarshal(raw, &k); err != nil {
		return Key{}, fmt.Errorf("cursor: json: %w", err)
	}
	if k.IsZero() {
		return Key{}, fmt.Errorf("cursor: missing created_at or id")
	}
	return k, nil
}
