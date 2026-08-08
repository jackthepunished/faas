package api

import (
	"bytes"
	"testing"
)

// TestCoverageSliceHashToken pins HashToken's SHA-256 contract. Login
// tokens (M7.5 magic link) are random raw bytes; the storage key
// is the SHA-256 of the raw token.
func TestCoverageSliceHashToken(t *testing.T) {
	raw := []byte("login-token-abc-123")
	got := HashToken(raw)
	if len(got) != 32 {
		t.Errorf("HashToken length = %d, want 32", len(got))
	}
	// The same input must hash deterministically.
	got2 := HashToken(raw)
	if !bytes.Equal(got, got2) {
		t.Errorf("HashToken not deterministic")
	}
}

// TestCoverageSliceHashAPIKeyVsHashToken pins that HashAPIKey and
// HashToken are the same function under the hood (both SHA-256),
// but accept different input shapes.
func TestCoverageSliceHashAPIKeyVsHashToken(t *testing.T) {
	s := "fp_live_abc"
	a := HashAPIKey(s)
	b := HashToken([]byte(s))
	if !bytes.Equal(a, b) {
		t.Errorf("HashAPIKey vs HashToken mismatch: %x vs %x", a, b)
	}
}

// TestCoverageSliceErrNoBody pins the only exported error sentinel
// the API package surfaces. Other errors flow through APIError.
func TestCoverageSliceErrNoBody(t *testing.T) {
	if ErrNoBody == nil {
		t.Fatal("ErrNoBody is nil")
	}
	if ErrNoBody.Error() == "" {
		t.Error("ErrNoBody.Error() = empty")
	}
}

// TestCoverageSliceAPIErrorIsError pins the APIError implements the
// error interface. The Problem embed is exported so handlers can
// also access the structured fields.
func TestCoverageSliceAPIErrorIsError(t *testing.T) {
	var e error = &APIError{Problem: Problem{Code: "x", Detail: "y"}}
	if e.Error() == "" {
		t.Error("APIError.Error() = empty")
	}
}
