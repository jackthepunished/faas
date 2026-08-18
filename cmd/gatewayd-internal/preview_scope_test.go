// preview_scope_test.go — table-driven unit tests for pgRouter.previewScopeFromHost
// (issue #272 / ADR-095 PR-B). The 8 locked cases below pin the parser
// contract used by both the routing layer (Backend.Lookup) and the on-demand
// cert allowlist (NewPGAllowlist's preview branch); the two must agree on
// every shape, otherwise a hostname that routes fails to mint a cert (or
// vice versa).
//
// The cases were locked in the plan file. Any deviation requires updating
// the plan first — no "while-I'm-here" parser loosening.

package main

import (
	"testing"
)

func TestPreviewScopeFromHost(t *testing.T) {
	const suffix = ".apps.gregale.dev"

	cases := []struct {
		name     string
		host     string
		suffix   string
		wantN    int
		wantSlug string
		wantOK   bool
	}{
		// ── Happy path: preview hosts ───────────────────────────────
		{
			name: "two-digit PR number", host: "pr-42-hello.apps.gregale.dev",
			suffix: suffix, wantN: 42, wantSlug: "hello", wantOK: true,
		},
		{
			name: "single-digit PR number", host: "pr-1-my-app.apps.gregale.dev",
			suffix: suffix, wantN: 1, wantSlug: "my-app", wantOK: true,
		},

		// ── Non-preview hosts (slugFor handles these; previewScopeFromHost
		//    must refuse so the two helpers are disjoint) ────────────
		{
			name: "prod slug, not preview", host: "hello.apps.gregale.dev",
			suffix: suffix, wantN: 0, wantSlug: "", wantOK: false,
		},
		{
			name: "uppercase PR refused (case-sensitive)", host: "PR-42-hello.apps.gregale.dev",
			suffix: suffix, wantN: 0, wantSlug: "", wantOK: false,
		},

		// ── Malformed preview hosts ─────────────────────────────────
		{
			name: "PR number 0 refused", host: "pr-0-hello.apps.gregale.dev",
			suffix: suffix, wantN: 0, wantSlug: "", wantOK: false,
		},
		{
			name: "missing parent slug", host: "pr-42.apps.gregale.dev",
			suffix: suffix, wantN: 0, wantSlug: "", wantOK: false,
		},
		{
			name: "empty parent slug", host: "pr-42-.apps.gregale.dev",
			suffix: suffix, wantN: 0, wantSlug: "", wantOK: false,
		},
		{
			name: "non-numeric PR number", host: "pr-abc-hello.apps.gregale.dev",
			suffix: suffix, wantN: 0, wantSlug: "", wantOK: false,
		},

		// ── Robustness cases not in the locked list but worth pinning ─
		{
			name: "leading-zero PR refused", host: "pr-007-hello.apps.gregale.dev",
			suffix: suffix, wantN: 0, wantSlug: "", wantOK: false,
		},
		{
			name: "host without configured suffix refuses", host: "pr-42-hello.example.com",
			suffix: suffix, wantN: 0, wantSlug: "", wantOK: false,
		},
		{
			name: "empty host refuses", host: "",
			suffix: suffix, wantN: 0, wantSlug: "", wantOK: false,
		},
		{
			name: "multi-label slug (dot inside) refuses", host: "pr-42-foo.bar.apps.gregale.dev",
			suffix: suffix, wantN: 0, wantSlug: "", wantOK: false,
		},
		{
			name: "missing pr- prefix refuses", host: "42-hello.apps.gregale.dev",
			suffix: suffix, wantN: 0, wantSlug: "", wantOK: false,
		},
		{
			name: "empty apps suffix refuses", host: "pr-42-hello.apps.gregale.dev",
			suffix: "", wantN: 0, wantSlug: "", wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := pgRouter{appsSuffix: tc.suffix}
			gotN, gotSlug, gotOK := r.previewScopeFromHost(tc.host)
			if gotN != tc.wantN || gotSlug != tc.wantSlug || gotOK != tc.wantOK {
				t.Fatalf("previewScopeFromHost(%q) = (%d, %q, %v); want (%d, %q, %v)",
					tc.host, gotN, gotSlug, gotOK,
					tc.wantN, tc.wantSlug, tc.wantOK)
			}
		})
	}
}
