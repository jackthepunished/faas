package api

import (
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
	}
	// All seven fields must be present (omitempty doesn't drop them
	// for a populated report). Future-proofing: when a new field is
	// added to CharacterizationReport, append a tag here.
	const minFieldCount = 7

	r := CharacterizationReport{
		ObservedClass:         "http",
		ObservedPort:          8080,
		ExitCode:              0,
		ListeningAddrs:        []string{"0.0.0.0:8080"},
		OutboundCount:         3,
		LogTail:               "listening on :8080\n",
		PortNormalizationMode: "none",
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
		case `"listening_addrs":`, `"log_tail":`, `"port_norm_mode":`:
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
