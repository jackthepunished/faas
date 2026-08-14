// surface_parser_test.go — table-driven tests for SurfaceParse.
// Mirrors preview_parser_test.go / allowlist_test.go conventions:
// short table, table-driven assertions, run under -race -count=5.
//
// The grammar is locked at the SurfaceParse doc comment; these
// tests pin every documented constraint. Adding a new case is
// expected when extending the grammar in a new ADR.
package gateway

import "testing"

func TestSurfaceParse(t *testing.T) {
	cases := []struct {
		name      string
		host      string
		wantLabel string
		wantApex  string
		wantOK    bool
	}{
		// --- apex: 2 labels ---
		{"apex_simple", "customer-a.com", "customer-a", "customer-a.com", true},
		{"apex_numeric", "shop.example42.com", "shop", "example42.com", true},
		{"apex_with_hyphen", "my-app.io", "my-app", "my-app.io", true},

		// --- subdomain: 3 labels ---
		{"sub_api", "api.customer-a.com", "api", "customer-a.com", true},
		{"sub_www", "www.example.com", "www", "example.com", true},
		{"sub_deep_hyphen", "auth-prod.my-app.io", "auth-prod", "my-app.io", true},

		// --- multi-subdomain: 4+ labels ---
		{"multi_sub", "auth.api.customer-a.com", "auth.api", "customer-a.com", true},
		{"multi_sub_5", "a.b.c.d.example.com", "a.b.c.d", "example.com", true},

		// --- case: DNS hostnames are case-insensitive (RFC 1035
		// §2.3.1); the parser preserves case at this layer (the
		// store normalises via citext on tenant_hostnames.hostname).
		// This pins the case-insensitive acceptance + the
		// preserved-case output. ---
		{"uppercase_accepted_at_label_level", "API.customer-a.com", "API", "customer-a.com", true},
		{"mixed_case_preserved", "Customer-a.MyApp.COM", "Customer-a", "MyApp.COM", true},

		// --- wildcard: rejected (SAN-aggregation is v1) ---
		{"wildcard_rejected", "*.customer-a.com", "", "", false},

		// --- trailing dot: rejected ---
		{"trailing_dot", "customer-a.com.", "", "", false},
		{"trailing_dot_sub", "api.customer-a.com.", "", "", false},

		// --- empty host ---
		{"empty", "", "", "", false},

		// --- single label: no apex ---
		{"single_label", "localhost", "", "", false},
		{"single_label_with_hyphen", "my-host", "", "", false},

		// --- empty label (consecutive dots) ---
		{"empty_label", "api..customer-a.com", "", "", false},
		{"leading_dot", ".customer-a.com", "", "", false},

		// --- label too long (>63) ---
		{"label_too_long", "this-label-has-far-more-than-sixty-three-characters-which-is-illegal-per-rfc-1035.com", "", "", false},

		// --- total host length > 253 (RFC 1035 §2.3.4). Fixture
		// is 4 labels of 63 chars + 1 label of 2 chars + 4 dots =
		// 258 octets; every per-label check passes, so this pins
		// the total-length branch specifically. ---
		{"total_too_long", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.aa", "", "", false},

		// --- edge: 253 octets (allowed). 4 labels of (63,63,63,61)
		// + 3 dots = 253. tenantLabel is the joined first 2 labels
		// ("a*63.a*63"); apex is the joined last 2 labels
		// ("a*63.a*61"). The fixture reads cleaner than the
		// 5-label form because the per-label lengths are
		// distinct and the dot count is minimal. ---
		{"total_at_max", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", true},

		// --- leading/trailing hyphen in label ---
		{"leading_hyphen", "-api.customer-a.com", "", "", false},
		{"trailing_hyphen", "api-.customer-a.com", "", "", false},
		{"hyphen_in_apex", "customer-a-.com", "", "", false},

		// --- illegal charset in label ---
		{"underscore_in_label", "api_v2.customer-a.com", "", "", false},
		{"unicode_in_label", "api™.customer-a.com", "", "", false},
		{"space_in_label", "api v2.customer-a.com", "", "", false},
		{"slash_in_label", "api/v2.customer-a.com", "", "", false},

		// --- IDN: punycode-only is accepted at this layer (the
		// apid handler is responsible for the unicode-to-ASCII
		// conversion). This pins the design contract. ---
		{"punycode_apex", "xn--bcher-kva.example.com", "xn--bcher-kva", "example.com", true},

		// --- edge: 63-char label (allowed) ---
		{"label_at_max", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.example.com", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "example.com", true},

		// --- edge: 64-char label (rejected) ---
		{"label_above_max", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.example.com", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotLabel, gotApex, gotOK := SurfaceParse(tc.host)
			if gotOK != tc.wantOK {
				t.Fatalf("SurfaceParse(%q) ok = %v, want %v", tc.host, gotOK, tc.wantOK)
			}
			if !gotOK {
				return
			}
			if gotLabel != tc.wantLabel {
				t.Errorf("SurfaceParse(%q) tenantLabel = %q, want %q", tc.host, gotLabel, tc.wantLabel)
			}
			if gotApex != tc.wantApex {
				t.Errorf("SurfaceParse(%q) apex = %q, want %q", tc.host, gotApex, tc.wantApex)
			}
		})
	}
}

// TestSurfaceParse_Pure pins the no-I/O, no-globals invariant by
// running the parser concurrently and verifying outputs are
// deterministic and independent of goroutine scheduling. Run
// under -race as a regression net for "did someone add a global
// to the parser".
func TestSurfaceParse_Pure(t *testing.T) {
	host := "api.customer-a.com"
	wantLabel, wantApex := "api", "customer-a.com"
	done := make(chan struct{})
	for i := 0; i < 100; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				l, a, ok := SurfaceParse(host)
				if !ok || l != wantLabel || a != wantApex {
					t.Errorf("SurfaceParse(%q) = (%q, %q, %v), want (%q, %q, true)", host, l, a, ok, wantLabel, wantApex)
				}
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 100; i++ {
		<-done
	}
}
