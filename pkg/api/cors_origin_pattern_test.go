// CorsOriginPattern <-> matchOrigin coherence (CORS improvements
// D2/D6 hygiene; code-review finding #4).
//
// The apid-side CorsOriginPattern regex enforces the allow_origins
// grammar at create-time; the gateway hot path applies the same
// predicates in matchOrigin (pkg/gateway/handler.go). A rule that
// passes the regex but can never match at runtime leaves the customer
// with a rule that stamps nothing — the regex and the matcher must
// agree on which shapes count as valid. The tests below pin both
// sides:
//
//   - Bare-star-host ("http://*") is rejected (used to pass).
//   - Suffix-star-host ("https://example.*") is rejected (used to pass).
//   - The four supported shapes are accepted:
//     bare "*", "https://host[:port|:*]", "https://*.host[:port|:*]",
//     "https://localhost[:port|:*]".
//   - All other obvious bad shapes are rejected.
package api_test

import (
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
)

func TestCorsOriginPattern_BareStarHostRejected(t *testing.T) {
	rejected := []string{
		"http://*",
		"https://*",
		"https://*:80",
		"https://*.*",
	}
	for _, in := range rejected {
		if api.CorsOriginPattern.MatchString(in) {
			t.Errorf("CorsOriginPattern: %q must be rejected (no host prefix matches at runtime)", in)
		}
	}
}

func TestCorsOriginPattern_SuffixStarHostRejected(t *testing.T) {
	rejected := []string{
		"https://example.*",
		"https://api.example.*",
	}
	for _, in := range rejected {
		if api.CorsOriginPattern.MatchString(in) {
			t.Errorf("CorsOriginPattern: %q must be rejected (suffix wildcards are not supported)", in)
		}
	}
}

func TestCorsOriginPattern_ValidShapesAccepted(t *testing.T) {
	accepted := []string{
		"*",
		"https://app.example.com",
		"https://app.example.com:8080",
		"https://*.example.com",
		"https://*.example.com:*",
		"https://*.example.com:8080",
		"https://localhost",
		"https://localhost:3000",
		"https://localhost:*",
		"http://api.example.com",
		"http://api.example.com:8080",
	}
	for _, in := range accepted {
		if !api.CorsOriginPattern.MatchString(in) {
			t.Errorf("CorsOriginPattern: %q must be accepted", in)
		}
	}
}

func TestCorsOriginPattern_BadShapesRejected(t *testing.T) {
	rejected := []string{
		"app.example.com",         // no scheme
		"ftp://app.example.com",   // non-http scheme
		"https://*.*.example.com", // multi-label wildcard (not supported)
		"https://app.*.com",       // mid-host wildcard
		"https://app.example.com:",
		"",  // empty
		" ", // whitespace
		"javascript:alert(1)",
	}
	for _, in := range rejected {
		if api.CorsOriginPattern.MatchString(in) {
			t.Errorf("CorsOriginPattern: %q must be rejected", in)
		}
	}
}
