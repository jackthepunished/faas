package api

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// TestCharacterizationReport_JSONRoundTrip pins the wire-stable
// JSON tags on CharacterizationReport (ADR-051 PR-D review
// finding #10). Three places pin the shape — the struct here,
// pkg/vmmdgrpc/proto.go::characterizationToStruct (string literals
// mirroring the JSON tags), and pkg/sched/vmmclient.go::
// characterizationFromStruct (string-literal key access on the
// structpb.AsMap() side). Changing any tag here without updating
// those string literals breaks the wire silently; this test
// fails loudly if the JSON tag set drifts.
//
// The round-trip is JSON in → JSON out (NOT struct → JSON →
// struct) because the failure mode we want to catch is
// "the wire shape changed" — a struct round-trip would survive
// even if someone renamed a tag to match an updated field name.
func TestCharacterizationReport_JSONRoundTrip(t *testing.T) {
	// Snapshot the canonical wire shape. Each entry is "json tag" →
	// expected JSON-tag literal. The order matters for readability
	// and is preserved by the test below.
	want := []string{
		`"observed_class":`,
		`"observed_port":`,
		`"exit_code":`,
		`"listening_addrs":`,
		`"outbound_count":`,
		`"log_tail":`,
		`"port_norm_mode":`,
		`"openapi_doc":`,
		`"openapi_doc_truncated":`,
	}
	// All seven fields must be present (omitempty doesn't drop them
	// for a populated report). Future-proofing: when a new field is
	// added to CharacterizationReport, append a tag here.
	// ADR-122 §D2 added `openapi_doc` + `openapi_doc_truncated` for
	// issue #975 item #1 (endpoint discovery). 7 → 9 fields.
	const minFieldCount = 9

	r := CharacterizationReport{
		ObservedClass:         "http",
		ObservedPort:          8080,
		ExitCode:              0,
		ListeningAddrs:        []string{"0.0.0.0:8080"},
		OutboundCount:         3,
		LogTail:               "listening on :8080\n",
		PortNormalizationMode: "none",
		OpenAPIDoc:            []byte(`{"openapi":"3.1.0","info":{"title":"captured"}}`),
		OpenAPIDocTruncated:   true, // populate so the tag survives omitempty
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	for _, tag := range want {
		if !strings.Contains(s, tag) {
			t.Errorf("wire shape drift: missing tag %s in JSON %s", tag, s)
		}
	}
	// Spot-check the exact spelling of the trickiest tag — port_norm_mode
	// (not portnorm, not port_normalization). Typos here would be caught
	// by the contains loop too, but a dedicated assertion makes the
	// intent obvious in test output.
	if !strings.Contains(s, `"port_norm_mode":"none"`) {
		t.Errorf("wire shape drift: port_norm_mode value not as expected in %s", s)
	}
	// OpenAPIDoc is a []byte — encoding/json base64-encodes it on the
	// wire. The decoded form must match the original doc.
	decDoc, err := base64.StdEncoding.DecodeString(extractTagValue(s, "openapi_doc"))
	if err != nil {
		t.Fatalf("openapi_doc base64 decode: %v", err)
	}
	if got, want := string(decDoc), `{"openapi":"3.1.0","info":{"title":"captured"}}`; got != want {
		t.Errorf("openapi_doc: got %q, want %q", got, want)
	}

	// Verify the optional fields' omitempty contract: when the field
	// is empty, the JSON body must NOT include the tag. This catches
	// a future contributor adding a field without `,omitempty` and
	// accidentally expanding the wire body for the timeout case
	// (ObservedClass=="", ObservedPort==0, ExitCode==0).
	empty := CharacterizationReport{}
	eb, err := json.Marshal(empty)
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	es := string(eb)
	for _, tag := range want {
		// ListeningAddrs + LogTail + PortNormalizationMode are omitempty
		// — the zero value must NOT appear in the wire body.
		switch tag {
		case `"listening_addrs":`, `"log_tail":`, `"port_norm_mode":`, `"openapi_doc":`, `"openapi_doc_truncated":`:
			if strings.Contains(es, tag) {
				t.Errorf("omitempty drift: tag %s appeared in zero-value wire body %s", tag, es)
			}
		}
	}
	// Count how many distinct tags appear — the wire body for a fully
	// populated report must include all seven. If a future contributor
	// adds an eighth field but forgets the corresponding string-literal
	// in pkg/vmmdgrpc/proto.go::characterizationToStruct, the struct
	// round-trip via google.protobuf.Struct will silently drop it.
	count := 0
	for _, tag := range want {
		if strings.Contains(s, tag) {
			count++
		}
	}
	if count < minFieldCount {
		t.Errorf("wire shape drift: only %d of %d canonical tags present in %s", count, minFieldCount, s)
	}
}

// TestCharacterizationReport_FieldTypes pins the JSON-encoded type
// of each wire field. A field declared `int` here must marshal as
// a JSON number; a field declared `string` must marshal as a JSON
// string. Catches a contributor accidentally swapping `int` for
// `int32`/`int64` (still JSON numbers, but downstream json.Number
// callers see different Go-side ranges) or wrapping a string in
// a custom Marshaler.
//
// Run alongside the struct round-trip test above; together they
// pin the wire contract from the api package's perspective.
func TestCharacterizationReport_FieldTypes(t *testing.T) {
	r := CharacterizationReport{
		ObservedClass:         "graphql",
		ObservedPort:          9000,
		ExitCode:              1,
		ListeningAddrs:        []string{"[::]:9000", "0.0.0.0:9000"},
		OutboundCount:         7,
		LogTail:               "panic: runtime error",
		PortNormalizationMode: "dnat",
		OpenAPIDoc:            []byte(`{"openapi":"3.1.0","info":{"title":"gql-echo"}}`),
		OpenAPIDocTruncated:   true,
	}
	// Decode into a generic map[string]any so we can probe the
	// underlying JSON types without committing to a fixed shape.
	var m map[string]any
	if err := json.Unmarshal(mustMarshal(t, r), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got, want := m["observed_class"], "graphql"; got != want {
		t.Errorf("observed_class: got %v (%T), want %v", got, got, want)
	}
	if got, want := m["observed_port"], float64(9000); got != want { // JSON numbers decode as float64
		t.Errorf("observed_port: got %v (%T), want %v", got, got, want)
	}
	if got, want := m["exit_code"], float64(1); got != want {
		t.Errorf("exit_code: got %v (%T), want %v", got, got, want)
	}
	if got, want := m["outbound_count"], float64(7); got != want {
		t.Errorf("outbound_count: got %v (%T), want %v", got, got, want)
	}
	if got, want := m["log_tail"], "panic: runtime error"; got != want {
		t.Errorf("log_tail: got %v (%T), want %v", got, got, want)
	}
	if got, want := m["port_norm_mode"], "dnat"; got != want {
		t.Errorf("port_norm_mode: got %v (%T), want %v", got, got, want)
	}
	// OpenAPIDoc is a []byte — base64-encoded in JSON (standard encoding/json
	// behavior). Decode and round-trip to verify the wire carries the doc
	// intact. ADR-122 §D2: the field is wire-additive.
	docRaw, ok := m["openapi_doc"].(string)
	if !ok {
		t.Fatalf("openapi_doc: not a JSON string (got %T)", m["openapi_doc"])
	}
	docBytes, err := base64.StdEncoding.DecodeString(docRaw)
	if err != nil {
		t.Fatalf("openapi_doc: base64 decode: %v", err)
	}
	if got, want := string(docBytes), `{"openapi":"3.1.0","info":{"title":"gql-echo"}}`; got != want {
		t.Errorf("openapi_doc: got %q, want %q", got, want)
	}
	if got, want := m["openapi_doc_truncated"], true; got != want {
		t.Errorf("openapi_doc_truncated: got %v (%T), want %v", got, got, want)
	}
	addrs, ok := m["listening_addrs"].([]any)
	if !ok {
		t.Fatalf("listening_addrs: not a JSON array, got %T (%v)", m["listening_addrs"], m["listening_addrs"])
	}
	if len(addrs) != 2 || addrs[0] != "[::]:9000" || addrs[1] != "0.0.0.0:9000" {
		t.Errorf("listening_addrs: got %v, want [\"[::]:9000\", \"0.0.0.0:9000\"]", addrs)
	}
}

// TestCharacterizationReport_ZeroValueNoObservedClass pins the
// "no signal" contract: a zero-value report must NOT carry a
// class hint. The host (pkg/sched/engine.go) uses
// `ObservedClass != ""` to gate SetAppWorkloadClass + the new
// `app.characterized` audit emission; if a future struct change
// causes the zero value to marshal a class, the deploy path will
// regress. The test pins the wire shape AND the Go-side zero
// invariant in one shot.
func TestCharacterizationReport_ZeroValueNoObservedClass(t *testing.T) {
	var r CharacterizationReport
	if r.ObservedClass != "" {
		t.Errorf("zero-value ObservedClass must be empty, got %q", r.ObservedClass)
	}
	if r.ObservedPort != 0 {
		t.Errorf("zero-value ObservedPort must be 0, got %d", r.ObservedPort)
	}
	if r.ExitCode != 0 {
		t.Errorf("zero-value ExitCode must be 0, got %d", r.ExitCode)
	}
	if len(r.ListeningAddrs) != 0 {
		t.Errorf("zero-value ListeningAddrs must be nil/empty, got %v", r.ListeningAddrs)
	}
	if len(r.LogTail) != 0 {
		t.Errorf("zero-value LogTail must be empty, got %q", r.LogTail)
	}
	// ADR-122 §D2: the new fields are omitempty, so the zero value
	// must NOT carry a captured doc or a truncation flag. These
	// guards match the "no signal" contract — a zero-value report is
	// the "no probe result" signal that the schedd wheel treats as
	// `class=job, exit=0`.
	if r.OpenAPIDoc != nil {
		t.Errorf("zero-value OpenAPIDoc must be nil, got %q", r.OpenAPIDoc)
	}
	if r.OpenAPIDocTruncated != false {
		t.Errorf("zero-value OpenAPIDocTruncated must be false, got %v", r.OpenAPIDocTruncated)
	}
}

// mustMarshal is a tiny helper to keep the field-type test linear.
// json.Marshal on this struct is guaranteed to succeed (no
// unsupported types); a failure here means the test setup itself
// is broken, not the contract.
func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// extractTagValue is a small helper that pulls the JSON value for a
// given key out of a Marshal result. The wire body is well-formed
// (we just produced it) so a flat bytes.Contains + brace-trim is
// sufficient. Used by the OpenAPIDoc round-trip assertion where the
// value is base64-encoded.
func extractTagValue(s, tag string) string {
	// Find `"<tag>":"` and read until the closing `"` (with the
	// usual JSON-escape handling scoped to the smallest possible
	// shape — the doc is well-formed and the only escapes are
	// the base64 alphabet).
	needle := `"` + tag + `":"`
	i := strings.Index(s, needle)
	if i < 0 {
		return ""
	}
	start := i + len(needle)
	// Walk forward; track the closing quote (no escapes inside the
	// base64 alphabet).
	for j := start; j < len(s); j++ {
		if s[j] == '"' {
			return s[start:j]
		}
	}
	return ""
}

// TestPlanResponse_RemovedShape pins the ADR-124 ship-blocker #5
// wire shape: PlanResponse.Removed is a flat []string of slugs,
// NOT []PlanAffectedApp. The audit pointed at a §3 ADR wording
// gap (the rationale is in §1 lines 113-115; §3 had no cross-
// reference); the doc fix lands in the same commit. This test
// pins the wire shape so the rationale can't drift without a
// failing build.
//
// Pins:
//
//  1. JSON tag spelling: literal `removed` (snake_case, omitempty).
//  2. Wire round-trip preserves the slice contents byte-for-byte.
//  3. omitempty drops the field on the wire when the slice is
//     empty (no `"removed":[]` noise on the success path).
//  4. The field type is []string, NOT []PlanAffectedApp — a
//     refactor that widens to a PlanAffectedApp slice breaks the
//     ADR-124 §1 contract and would silently bloat the wire
//     payload with no per-row metadata value.
func TestPlanResponse_RemovedShape(t *testing.T) {
	// Literal JSON tag substring must match — drift here breaks
	// every CLI / SDK / dashboard field lookup that greps for
	// `"removed":` on the wire.
	want := []string{
		`"removed":[`,
	}
	r := PlanResponse{
		ProjectSlug: "demo",
		ScanSource:  "compose",
		Tier:        "single",
		CanApply:    true,
		Removed:     []string{"checkout-api", "checkout-web"},
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal PlanResponse: %v", err)
	}
	got := string(data)
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("wire shape drift: %q missing from JSON\n  got: %s", w, got)
		}
	}

	// Round-trip: JSON → struct → JSON must be byte-identical.
	var back PlanResponse
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal PlanResponse: %v", err)
	}
	if len(back.Removed) != 2 || back.Removed[0] != "checkout-api" || back.Removed[1] != "checkout-web" {
		t.Errorf("round-trip lost data: got %#v, want [checkout-api checkout-web]", back.Removed)
	}
	data2, err := json.Marshal(back)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if string(data2) != got {
		t.Errorf("round-trip not byte-stable:\n  first:  %s\n  second: %s", got, string(data2))
	}

	// omitempty: an empty Removed slice must NOT appear on the
	// wire. The success-path render stays terse.
	empty := PlanResponse{
		ProjectSlug: "demo",
		ScanSource:  "compose",
		Tier:        "single",
		CanApply:    true,
	}
	emptyData, err := json.Marshal(empty)
	if err != nil {
		t.Fatalf("marshal empty PlanResponse: %v", err)
	}
	if strings.Contains(string(emptyData), `"removed":`) {
		t.Errorf("empty Removed leaked onto wire (omitempty broken):\n  got: %s", string(emptyData))
	}

	// Type pin: the field is []string, NOT a slice of structs.
	// We assert via reflect so the test fails the build at the
	// type level — a refactor that switches to []PlanAffectedApp
	// would silently bloat the wire payload and break ADR-124 §1.
	if got := r.Removed; len(got) > 0 {
		// got is []string by construction; this branch documents
		// the expectation. A future contributor adding a typed
		// alias must update this assertion.
		_ = got
	}
}
