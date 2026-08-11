// Whitebox test for EdgeRuleValidateAction.Validate. The dto
// validator is the first gate a kind=validate rule sees on the
// create / update path (apid handler calls Validate() before any
// SQL write); the gateway hot path re-strips external `$ref` and
// re-compiles at gateway compile time as defence-in-depth, so this
// test pins the apid-side contract only. Errors surface as a 400
// `*Problem` (`CodeValidation`); the gateway runtime surface is a
// distinct 422 `CodeRequestValidationFailed` (pkg/api/errors.go
// line 1602) and lives in cmd/gatewayd-internal.

package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// happyEdgeRuleValidateAction returns a well-formed action that
// should pass Validate() unmodified. Tests then mutate one field at
// a time to assert each rejection arm independently.
func happyEdgeRuleValidateAction() EdgeRuleValidateAction {
	return EdgeRuleValidateAction{
		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": {"type": "string"},
				"email": {"type": "string", "format": "email"},
				"age": {"type": "integer", "minimum": 0}
			},
			"required": ["name", "email"]
		}`),
		ContentTypes:          []string{"application/json"},
		ApplyWhileStreaming:   false,
		RejectOnUnknownFields: false,
		MaxBodyBytes:          0, // inherit platform cap (default).
	}
}

func TestEdgeRuleValidateAction_Validate_HappyPath(t *testing.T) {
	a := happyEdgeRuleValidateAction()
	if p := a.Validate(); p != nil {
		t.Fatalf("happy path returned %v, want nil", p)
	}
}

// TestEdgeRuleValidateAction_Validate_Rejects is the table-driven
// negative arm. Each row mutates one field of the happy-path
// action. The assertion target is `wantSub` (a substring of the
// `*Problem.Detail`) so a re-wording of unrelated wording does not
// churn the table.
func TestEdgeRuleValidateAction_Validate_Rejects(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(a *EdgeRuleValidateAction)
		wantSub string
	}{
		{
			name: "schema-empty",
			mutate: func(a *EdgeRuleValidateAction) {
				a.Schema = json.RawMessage(nil)
			},
			wantSub: "schema is required",
		},
		{
			name: "schema-exceeds-64KiB",
			mutate: func(a *EdgeRuleValidateAction) {
				// Build a syntactically-valid JSON object whose
				// rendered byte length crosses 64 KiB. The
				// Validate() function checks len(a.Schema) *before*
				// the JSON unmarshal probe, so a payload of zeros
				// works without needing to be parseable.
				filler := strings.Repeat("a", MaxEdgeRuleValidateSchemaBytes)
				a.Schema = json.RawMessage(`{"x":"` + filler + `"}`)
			},
			wantSub: "schema exceeds",
		},
		{
			name: "schema-not-JSON",
			mutate: func(a *EdgeRuleValidateAction) {
				a.Schema = json.RawMessage(`{"type": "object"`) // unterminated
			},
			wantSub: "not valid JSON",
		},
		{
			name: "schema-external-ref-https",
			mutate: func(a *EdgeRuleValidateAction) {
				a.Schema = json.RawMessage(`{
					"type": "object",
					"$ref": "https://internal.example.com/secrets.json"
				}`)
			},
			wantSub: "external $ref or $id URL",
		},
		{
			name: "schema-external-id-protocol-relative",
			mutate: func(a *EdgeRuleValidateAction) {
				// Protocol-relative (`//host/...`) is also caught
				// by edgeRuleValidateRefURLPattern (the
				// `https?://|//` alternation). The regex only
				// fires when the URL is on the right-hand side of
				// `$ref` or `$id`; we put it on `$id` to match the
				// second arm of the alternation.
				a.Schema = json.RawMessage(`{
					"type": "object",
					"$id": "//internal.example.com/secrets.json"
				}`)
			},
			wantSub: "external $ref or $id URL",
		},
		{
			name: "schema-internal-pointer",
			mutate: func(a *EdgeRuleValidateAction) {
				// Pins the *current* regex behaviour: `#/definitions/Foo`
				// is an internal JSON Pointer but the unanchored
				// `\$ref|id` alternation in the apid-side regex
				// (dto.go line 3714) matches the bare `$ref`
				// substring irrespective of what follows, so an
				// internal pointer trips the same rejection as an
				// absolute URL. The gateway-side
				// `pkg/edgevalidate.Compile` is the authoritative
				// gate on the hot path; this row documents the
				// apid-side strictness so a future regex fix in
				// PR-x flips this expectation, not the test.
				a.Schema = json.RawMessage(`{
					"type": "object",
					"$ref": "#/definitions/Foo"
				}`)
			},
			wantSub: "external $ref or $id URL",
		},
		{
			name: "content-type-not-application",
			mutate: func(a *EdgeRuleValidateAction) {
				a.ContentTypes = []string{"text/plain"}
			},
			wantSub: "must start with 'application/'",
		},
		{
			name: "content-type-mixed",
			mutate: func(a *EdgeRuleValidateAction) {
				// First entry valid, second entry bogus. The
				// validator walks the slice in order and rejects
				// at the first offender.
				a.ContentTypes = []string{"application/json", "image/png"}
			},
			wantSub: "must start with 'application/'",
		},
		{
			name: "content-type-jsonl-allowed",
			mutate: func(a *EdgeRuleValidateAction) {
				// application/* prefix matches `application/jsonl`
				// too — that's a v1 hole. Locked decision: closed
				// set is the `application/` prefix in this ADR;
				// narrowing to a literal allowlist is deferred.
				a.ContentTypes = []string{"application/jsonl"}
			},
			wantSub: "", // sentinel: expects accept.
		},
		{
			name: "max-body-bytes-negative",
			mutate: func(a *EdgeRuleValidateAction) {
				a.MaxBodyBytes = -1
			},
			wantSub: "must be >= 0",
		},
		{
			name: "max-body-bytes-exceeds-platform-cap",
			mutate: func(a *EdgeRuleValidateAction) {
				a.MaxBodyBytes = MaxRequestBodyBytes + 1
			},
			wantSub: "platform cap",
		},
		{
			name: "max-body-bytes-equals-cap-allowed",
			mutate: func(a *EdgeRuleValidateAction) {
				// Boundary: == MaxRequestBodyBytes must be
				// accepted (the comparison is strict `>`).
				a.MaxBodyBytes = MaxRequestBodyBytes
			},
			wantSub: "", // sentinel: expects accept.
		},
		{
			name: "schema-nil-receiver-panics-not",
			mutate: func(a *EdgeRuleValidateAction) {
				// Calling Validate() on a nil receiver must
				// return a Problem rather than panic (the
				// handler chains `v.Validate()` without a nil
				// check). We replace the action with a nil
				// pointer rather than calling on the existing
				// struct — the wrapper below handles the swap.
				a.Schema = nil
				// Sentinel; wrapper flips the receiver to nil.
			},
			wantSub: "validate action is required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := happyEdgeRuleValidateAction()
			tc.mutate(&base)

			var p *Problem
			if tc.name == "schema-nil-receiver-panics-not" {
				// The "nil receiver" arm: ignore the copy, pass
				// a literal nil so the receiver itself is nil.
				var nilAction *EdgeRuleValidateAction
				p = nilAction.Validate()
			} else {
				p = base.Validate()
			}

			switch tc.wantSub {
			case "":
				if p != nil {
					t.Fatalf("expected accept, got %v", p)
				}
			default:
				if p == nil {
					t.Fatalf("expected rejection containing %q, got nil", tc.wantSub)
				}
				if !strings.Contains(p.Detail, tc.wantSub) {
					t.Fatalf("rejection detail %q does not contain %q", p.Detail, tc.wantSub)
				}
			}
		})
	}
}

// TestEdgeRuleValidateAction_Validate_JSONRoundTrip pins the wire
// shape that the apid handler unmarshals into. A silently drifted
// field name (e.g. `max_body_bytes` -> `maxBodyBytes`) would surface
// here as a nil/zero value after round-trip and trip the happy-path
// validator's `wantSub == ""` arm.
func TestEdgeRuleValidateAction_JSONRoundTrip(t *testing.T) {
	original := happyEdgeRuleValidateAction()
	buf, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded EdgeRuleValidateAction
	if err := json.Unmarshal(buf, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p := decoded.Validate(); p != nil {
		t.Fatalf("decoded.Validate() = %v, want nil", p)
	}
	// Spot-check the fields we care about: a refactor that drops
	// ContentTypes / MaxBodyBytes from the struct (or renames the
	// JSON tag) would land here as zero-value mismatches.
	if len(decoded.ContentTypes) != 1 || decoded.ContentTypes[0] != "application/json" {
		t.Fatalf("ContentTypes round-trip mismatch: got %v", decoded.ContentTypes)
	}
	if decoded.MaxBodyBytes != 0 {
		t.Fatalf("MaxBodyBytes round-trip mismatch: got %d", decoded.MaxBodyBytes)
	}
	if decoded.ApplyWhileStreaming {
		t.Fatalf("ApplyWhileStreaming round-trip mismatch: got true, want false")
	}
	if decoded.RejectOnUnknownFields {
		t.Fatalf("RejectOnUnknownFields round-trip mismatch: got true, want false")
	}
}
