// Smoke tests for the shared jsonschemautil helpers. The full
// validator integration is exercised in pkg/edgevalidate and
// pkg/openapiimport — this file pins the shape of the helpers
// themselves so a future regression in the JSON Pointer
// separator or the printer pointer lands in one place.
package jsonschemautil_test

import (
	"testing"

	"github.com/onebox-faas/faas/pkg/jsonschemautil"
)

func TestJoinInstanceLocation_Empty(t *testing.T) {
	if got := jsonschemautil.JoinInstanceLocation(nil); got != "" {
		t.Errorf("nil loc: got %q, want empty", got)
	}
	if got := jsonschemautil.JoinInstanceLocation([]string{}); got != "" {
		t.Errorf("[] loc: got %q, want empty", got)
	}
}

func TestJoinInstanceLocation_Single(t *testing.T) {
	got := jsonschemautil.JoinInstanceLocation([]string{"openapi"})
	if got != "/openapi" {
		t.Errorf("single: got %q, want /openapi", got)
	}
}

func TestJoinInstanceLocation_Multi(t *testing.T) {
	got := jsonschemautil.JoinInstanceLocation([]string{"info", "title"})
	if got != "/info/title" {
		t.Errorf("multi: got %q, want /info/title", got)
	}
}

func TestJoinInstanceLocation_PreservesSpecialChars(t *testing.T) {
	// The JSON Pointer separator is "/" — a token containing
	// "/" is technically percent-encoded per RFC 6901 §3 but
	// the helper is a passthrough; the encoder is upstream.
	got := jsonschemautil.JoinInstanceLocation([]string{"paths", "/users/{id}"})
	if got != "/paths//users/{id}" {
		t.Errorf("special chars: got %q, want /paths//users/{id}", got)
	}
}

func TestDefaultPrinter_NonNil(t *testing.T) {
	if jsonschemautil.DefaultPrinter == nil {
		t.Error("DefaultPrinter is nil; LocalizedString would panic")
	}
}
