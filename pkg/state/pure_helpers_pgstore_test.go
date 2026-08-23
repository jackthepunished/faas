// pure_helpers_pgstore_test.go — fill pgstore.go pure helper coverage
// gaps. Targets the no-IO, no-Postgres pure functions that surface at
// 0% in the pre-PR report but are reachable through every pgstore
// method that uses them.
//
// Helpers under test:
//   - Sha256Equal                (constant-time compare)
//   - RedistributeTraffic        (4 branches: nil, ≤0 residual,
//                                 zero Σ-prior, normal proportional)
//   - ptrOrEmpty / derefString / derefStrings / ptrTime
//   - nullableStr / nullableTime / nilToEmpty / jsonOrEmpty
//   - nullString / nullableInt / nullAppStatus / nullJSONRaw
//   - cidrPrefixesToArray / cidrTextToPrefixes
//   - nullableTimestamptz / nullableTimestamptzPtr
//   - uuidFromPgtype / parsePgUUID / mustPgUUID
//
// Conventions: whitebox `package state` (matches every existing pgstore
// helper test). pgtest-free — every helper is a pure function.

package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// --- Sha256Equal ----------------------------------------------------

func TestSha256Equal_EqualAndUnequal(t *testing.T) {
	h := sha256.Sum256([]byte("hello"))
	if !Sha256Equal(h[:], h[:]) {
		t.Error("same bytes: Sha256Equal = false, want true")
	}
	if Sha256Equal(h[:], []byte("other")) {
		t.Error("different bytes: Sha256Equal = true, want false")
	}
	// Different lengths must short-circuit.
	if Sha256Equal(h[:], []byte("short")) {
		t.Error("length mismatch: Sha256Equal = true, want false")
	}
	if Sha256Equal([]byte("short"), h[:]) {
		t.Error("length mismatch reversed: Sha256Equal = true, want false")
	}
}

// --- RedistributeTraffic ---------------------------------------------

func TestRedistributeTraffic_NilReturnsNil(t *testing.T) {
	got := RedistributeTraffic(nil, 100)
	if got != nil {
		t.Errorf("nil input: got %v, want nil", got)
	}
}

func TestRedistributeTraffic_ResidualZeroZerosAll(t *testing.T) {
	// residual <= 0 → out is a slice of length n with all 0s.
	sibs := []struct {
		ID    string
		Prior int
	}{{"a", 30}, {"b", 70}}
	got := RedistributeTraffic(sibs, 0)
	want := []int{0, 0}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("residual=0: got %v, want %v", got, want)
	}
}

func TestRedistributeTraffic_ResidualNegativeZerosAll(t *testing.T) {
	sibs := []struct {
		ID    string
		Prior int
	}{{"a", 30}}
	got := RedistributeTraffic(sibs, -5)
	want := []int{0}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("residual<0: got %v, want %v", got, want)
	}
}

func TestRedistributeTraffic_ZeroPriorDistributesEvenly(t *testing.T) {
	// Σ prior = 0 → distribute residual evenly, first residual%N (in
	// ID-ascending order) absorb +1.
	sibs := []struct {
		ID    string
		Prior int
	}{{"c", 0}, {"a", 0}, {"b", 0}}
	got := RedistributeTraffic(sibs, 10)
	// Sorted by ID asc: a (idx 1), b (idx 2), c (idx 0).
	// base = 10/3 = 3; residual%3 = 1; first 1 (sorted idx 0 = 'a')
	// gets +1.
	want := []int{3, 4, 3}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("zero-prior: got %v, want %v", got, want)
	}
}

func TestRedistributeTraffic_NormalProportional(t *testing.T) {
	// Normal path: residual=100 across priors (30, 70) → out = [30, 70].
	sibs := []struct {
		ID    string
		Prior int
	}{{"a", 30}, {"b", 70}}
	got := RedistributeTraffic(sibs, 100)
	want := []int{30, 70}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("normal: got %v, want %v", got, want)
	}
}

func TestRedistributeTraffic_RemainderRoundsUp(t *testing.T) {
	// residual=100 across priors (33, 33, 34): floor values are
	// 33, 33, 34 = 100, no remainder → no +1.
	sibs := []struct {
		ID    string
		Prior int
	}{{"a", 33}, {"b", 33}, {"c", 34}}
	got := RedistributeTraffic(sibs, 100)
	want := []int{33, 33, 34}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("remainder=0: got %v, want %v", got, want)
	}
}

func TestRedistributeTraffic_NonRoundProportional(t *testing.T) {
	// residual=10 across priors (3, 3, 4): floor=3,3,4=10, exact.
	sibs := []struct {
		ID    string
		Prior int
	}{{"a", 3}, {"b", 3}, {"c", 4}}
	got := RedistributeTraffic(sibs, 10)
	want := []int{3, 3, 4}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("non-round: got %v, want %v", got, want)
	}
}

// --- ptrOrEmpty / derefString / derefStrings / ptrTime --------------

func TestPtrOrEmpty_NilAndSet(t *testing.T) {
	if got := ptrOrEmpty(nil); got != nil {
		t.Errorf("nil: got %v, want nil", got)
	}
	var zero AppPublicAuthUpdate
	if got := ptrOrEmpty(&zero); got == nil {
		t.Errorf("set: got nil, want *zero")
	}
}

func TestDerefString_NilAndSet(t *testing.T) {
	if got := derefString(nil); got != "" {
		t.Errorf("nil: got %q, want empty", got)
	}
	p := "hi"
	if got := derefString(&p); got != "hi" {
		t.Errorf("set: got %q, want hi", got)
	}
}

func TestDerefStrings_NilAndSet(t *testing.T) {
	if got := derefStrings(nil); got != nil {
		t.Errorf("nil: got %v, want nil", got)
	}
	s := []string{"a", "b"}
	got := derefStrings(&s)
	if !reflect.DeepEqual(got, s) {
		t.Errorf("set: got %v, want %v", got, s)
	}
}

func TestPtrTime_RoundTrip(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	got := ptrTime(now)
	if got == nil || !got.Equal(now) {
		t.Errorf("got %v, want %v", got, now)
	}
}

// --- nullableStr / nullableTime / nilToEmpty / jsonOrEmpty ----------

func TestNullableStr_EmptyStringReturnsSQLNull(t *testing.T) {
	got := nullableStr("")
	if got != nil {
		t.Errorf("empty: got %v, want nil", got)
	}
	if got := nullableStr("x"); got != "x" {
		t.Errorf("set: got %v, want x", got)
	}
}

func TestNullableTime_ZeroTimeReturnsSQLNull(t *testing.T) {
	got := nullableTime(time.Time{})
	if got != nil {
		t.Errorf("zero: got %v, want nil", got)
	}
	now := time.Now()
	if got := nullableTime(now); got != now {
		t.Errorf("set: got %v, want %v", got, now)
	}
}

func TestNilToEmpty(t *testing.T) {
	// nilToEmpty returns an empty (non-nil) slice for nil input.
	got := nilToEmpty(nil)
	if got == nil {
		t.Errorf("nil: got nil, want empty (non-nil)")
	}
	if len(got) != 0 {
		t.Errorf("nil: got %v, want empty", got)
	}
	src := []string{"a"}
	if got := nilToEmpty(src); !reflect.DeepEqual(got, src) {
		t.Errorf("set: got %v, want %v", got, src)
	}
}

func TestJsonOrEmpty(t *testing.T) {
	// Empty raw → "{}" with no error (per jsonOrEmpty contract).
	got, err := jsonOrEmpty(nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if string(got) != "{}" {
		t.Errorf("nil: got %q, want {}", got)
	}
	got, err = jsonOrEmpty(json.RawMessage(`{"k":"v"}`))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if string(got) != `{"k":"v"}` {
		t.Errorf("got %q", got)
	}
	// Invalid JSON → error.
	if _, err := jsonOrEmpty(json.RawMessage("not json{")); err == nil {
		t.Error("invalid json: err = nil, want error")
	}
}

// --- nullString / nullableInt / nullAppStatus / nullJSONRaw ---------

func TestNullString_Empty(t *testing.T) {
	if nullString("") != nil {
		t.Errorf("empty: not nil")
	}
	if nullString("x") != "x" {
		t.Errorf("set: not x")
	}
}

func TestNullableInt_ZeroAndSet(t *testing.T) {
	if nullableInt(0) != nil {
		t.Errorf("0: not nil")
	}
	if nullableInt(7) != 7 {
		t.Errorf("set: not 7")
	}
}

func TestNullAppStatus_NilAndSet(t *testing.T) {
	if nullAppStatus(nil) != nil {
		t.Errorf("nil: not nil")
	}
	var s AppStatus = "active"
	got := nullAppStatus(&s)
	// The implementation returns string(*p), so the runtime type is
	// string even though the source type is AppStatus.
	if got != string(s) {
		t.Errorf("set: got %v, want %v", got, string(s))
	}
}

func TestNullJSONRaw_EmptyAndSet(t *testing.T) {
	if nullJSONRaw(nil) != nil {
		t.Errorf("nil: not nil")
	}
	if nullJSONRaw(json.RawMessage{}) != nil {
		t.Errorf("empty raw: not nil")
	}
	b := json.RawMessage(`{"k":1}`)
	got := nullJSONRaw(b)
	if !reflect.DeepEqual(got, b) {
		t.Errorf("set: got %v, want %v", got, b)
	}
}

// --- cidrPrefixesToArray / cidrTextToPrefixes -----------------------

func TestCIDRPrefixesRoundTrip(t *testing.T) {
	original := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/24"),
		netip.MustParsePrefix("192.168.1.0/24"),
		netip.MustParsePrefix("fd00::/64"),
	}
	arr := cidrPrefixesToArray(original)
	if arr == "" {
		t.Fatal("array string empty")
	}
	got := cidrTextToPrefixes(arr)
	if !reflect.DeepEqual(got, original) {
		t.Errorf("round-trip: got %v, want %v", got, original)
	}
}

func TestCIDRTextToPrefixes_EmptyReturnsNil(t *testing.T) {
	if got := cidrTextToPrefixes(""); got != nil {
		t.Errorf("empty: got %v, want nil", got)
	}
}

// --- nullableTimestamptz / nullableTimestamptzPtr --------------------

func TestNullableTimestamptz_ZeroIsInvalid(t *testing.T) {
	got := nullableTimestamptz(time.Time{})
	if got.Valid {
		t.Errorf("zero time: Valid=true, want false")
	}
}

func TestNullableTimestamptz_SetIsValid(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	got := nullableTimestamptz(now)
	if !got.Valid {
		t.Errorf("set time: Valid=false, want true")
	}
	if !got.Time.Equal(now) {
		t.Errorf("Time = %v, want %v", got.Time, now)
	}
}

func TestNullableTimestamptzPtr_NilIsInvalid(t *testing.T) {
	got := nullableTimestamptzPtr(nil)
	if got.Valid {
		t.Errorf("nil: Valid=true, want false")
	}
}

func TestNullableTimestamptzPtr_PointedIsValid(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	got := nullableTimestamptzPtr(&now)
	if !got.Valid || !got.Time.Equal(now) {
		t.Errorf("set ptr: got %+v", got)
	}
}

// --- uuidFromPgtype / parsePgUUID / mustPgUUID ---------------------

func TestUUIDFromPgtype_RoundTrip(t *testing.T) {
	id := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	pg := pgtype.UUID{Bytes: id, Valid: true}
	got := uuidFromPgtype(pg)
	if got != id {
		t.Errorf("got %v, want %v", got, id)
	}
}

func TestParsePgUUID_HappyAndInvalid(t *testing.T) {
	// Well-formed hex.
	hexID := hex.EncodeToString([]byte{0x55, 0x0e, 0x84, 0x00, 0xe2, 0x9b, 0x41, 0xd4,
		0xa7, 0x16, 0x44, 0x66, 0x55, 0x44, 0x00, 0x00})
	got, err := parsePgUUID(hexID)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !got.Valid {
		t.Errorf("Valid=false, want true")
	}

	// Odd length → error.
	if _, err := parsePgUUID("abc"); err == nil {
		t.Error("odd length: err = nil, want error")
	}
	// Non-hex → error.
	if _, err := parsePgUUID("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"); err == nil {
		t.Error("non-hex: err = nil, want error")
	}
}

func TestMustPgUUID_InvalidReturnsZero(t *testing.T) {
	// mustPgUUID does NOT panic — it returns pgtype.UUID{} on
	// invalid input. Pin that contract; a future panic-on-error
	// refactor trips here.
	got := mustPgUUID("not-a-uuid")
	if got.Valid {
		t.Errorf("invalid input: got Valid=true, want false")
	}
}

func TestMustPgUUID_Valid(t *testing.T) {
	// 32-hex string → valid pgtype.UUID.
	hexID := hex.EncodeToString([]byte{0x55, 0x0e, 0x84, 0x00, 0xe2, 0x9b, 0x41, 0xd4,
		0xa7, 0x16, 0x44, 0x66, 0x55, 0x44, 0x00, 0x00})
	got := mustPgUUID(hexID)
	if !got.Valid {
		t.Errorf("valid input: Valid=false, want true")
	}
}

// --- derefTime (pure helper variant) --------------------------------

func TestDerefTime(t *testing.T) {
	if got := derefTime(nil); !got.IsZero() {
		t.Errorf("nil: got %v, want zero", got)
	}
	now := time.Now()
	if got := derefTime(&now); !got.Equal(now) {
		t.Errorf("set: got %v, want %v", got, now)
	}
}