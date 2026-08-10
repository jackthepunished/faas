// rekey_test.go — unit tests for pkg/rekey.
//
// Tests use a tiny in-memory fakeStore (the methods pkg/rekey
// uses — ListAppSecretsForRekey, UpsertAppSecretWithKid). The
// fakeStore matches the cursor encoding pgstore + memstore use so
// the tests exercise the same wire shape.
//
// Coverage:
//   - Run on an empty store is a no-op (zero rows visited).
//   - Run with rows under previous kid re-seals and stamps kid.
//   - Idempotent: Run twice is a no-op the second time.
//   - Cursor walk: a non-empty cursor skips rows <= cursor.
//   - Constructor: empty identities slice rejected.
//   - Constructor: zero/negative cfg fields fall back to defaults.
package rekey

import (
	"context"
	"sort"
	"sync"
	"testing"

	"filippo.io/age"

	"github.com/onebox-faas/faas/pkg/secretbox"
	"github.com/onebox-faas/faas/pkg/state"
)

// fakeStore implements just the three methods pkg/rekey uses.
// Cursor encoding matches pgstore/pgstore_memstore
// ("<account_id>|<app_id>|<key>").
type fakeStore struct {
	mu   sync.Mutex
	rows map[string]state.AppSecret
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: map[string]state.AppSecret{}}
}

func (s *fakeStore) put(row state.AppSecret) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[encodeCursor(row.AccountID, row.AppID, row.Key)] = row
}

func (s *fakeStore) ListAppSecretsForRekey(_ context.Context, limit int, cursor string) ([]state.AppSecret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var curA, curB, curC string
	if cursor != "" {
		parts := splitCursor(cursor)
		if parts == nil {
			return nil, nil
		}
		curA, curB, curC = parts[0], parts[1], parts[2]
	}
	var out []state.AppSecret
	for _, r := range s.rows {
		if curA != "" && lessOrEqTriples(r.AccountID, r.AppID, r.Key, curA, curB, curC) {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		return lessTriples(out[i].AccountID, out[i].AppID, out[i].Key,
			out[j].AccountID, out[j].AppID, out[j].Key)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *fakeStore) UpsertAppSecretWithKid(_ context.Context, accountID, appID, key, kid string, ciphertext []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.rows[encodeCursor(accountID, appID, key)]
	row.AccountID = accountID
	row.AppID = appID
	row.Key = key
	row.Kid = kid
	row.Ciphertext = ciphertext
	s.rows[encodeCursor(accountID, appID, key)] = row
	return nil
}

func splitCursor(c string) []string {
	out := []string{""}
	for i := 0; i < len(c); i++ {
		if c[i] == '|' {
			out = append(out, "")
			continue
		}
		out[len(out)-1] += string(c[i])
	}
	if len(out) != 3 {
		return nil
	}
	return out
}

func lessTriples(a1, a2, a3, b1, b2, b3 string) bool {
	if a1 != b1 {
		return a1 < b1
	}
	if a2 != b2 {
		return a2 < b2
	}
	return a3 < b3
}

func lessOrEqTriples(a1, a2, a3, b1, b2, b3 string) bool {
	if a1 != b1 {
		return a1 < b1
	}
	if a2 != b2 {
		return a2 < b2
	}
	return a3 <= b3
}

// sealUnder is a one-shot seal helper using pkg/secretbox.Seal so
// the test exercises the real envelope shape end-to-end.
func sealUnder(t *testing.T, recipient *age.X25519Recipient, key, value string) []byte {
	t.Helper()
	blob, err := secretbox.Seal(recipient, secretbox.Envelope{key: value})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	return blob
}

// TestRun_EmptyStore: Run on an empty store is a clean no-op.
func TestRun_EmptyStore(t *testing.T) {
	id := mustIdentity(t)
	store := newFakeStore()
	r, err := New(store, []*age.X25519Identity{id}, RekeyConfig{RowsPerSecond: 1000, BatchSize: 50})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var last RekeyProgress
	err = r.Run(context.Background(), "", func(p RekeyProgress) { last = p })
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if last.Total != 0 || last.Rekeyed != 0 || last.Skipped != 0 || last.Failed != 0 {
		t.Fatalf("empty store: got %+v, want zero counters", last)
	}
}

// TestRun_RekeysRowsUnderPreviousKid: rows sealed under the
// previous identity get re-sealed under current and have kid
// updated to the current identity's fingerprint.
func TestRun_RekeysRowsUnderPreviousKid(t *testing.T) {
	previous := mustIdentity(t)
	current := mustIdentity(t)

	store := newFakeStore()
	// Seed three rows under the previous identity with kid =
	// previous's recipient string.
	prevKid := previous.Recipient().String()
	for i, key := range []string{"A", "B", "C"} {
		store.put(state.AppSecret{
			AccountID:  "acct-1",
			AppID:      "app-1",
			Key:        key,
			Ciphertext: sealUnder(t, previous.Recipient(), key, "v"+string(rune('0'+i))),
			Kid:        prevKid,
		})
	}

	r, err := New(store, []*age.X25519Identity{current, previous}, RekeyConfig{RowsPerSecond: 5000, BatchSize: 50})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var last RekeyProgress
	if err := r.Run(context.Background(), "", func(p RekeyProgress) { last = p }); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if last.Total != 3 || last.Rekeyed != 3 || last.Skipped != 0 || last.Failed != 0 {
		t.Fatalf("got %+v, want Total=3 Rekeyed=3", last)
	}

	// Each row's kid must now equal the current identity's
	// recipient string.
	wantKid := current.Recipient().String()
	for _, key := range []string{"A", "B", "C"} {
		row, ok := store.rows[encodeCursor("acct-1", "app-1", key)]
		if !ok {
			t.Fatalf("row %q missing after rekey", key)
		}
		if row.Kid != wantKid {
			t.Fatalf("row %q kid = %q, want %q", key, row.Kid, wantKid)
		}
	}
}

// TestRun_Idempotent: running Replayer twice is a no-op the
// second time — every row has kid = current after the first pass.
func TestRun_Idempotent(t *testing.T) {
	previous := mustIdentity(t)
	current := mustIdentity(t)

	store := newFakeStore()
	store.put(state.AppSecret{
		AccountID:  "acct-1",
		AppID:      "app-1",
		Key:        "A",
		Ciphertext: sealUnder(t, previous.Recipient(), "A", "v"),
		Kid:        previous.Recipient().String(),
	})

	r, err := New(store, []*age.X25519Identity{current, previous}, RekeyConfig{RowsPerSecond: 5000, BatchSize: 50})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// First pass: rekeys the row.
	var p1 RekeyProgress
	if err := r.Run(context.Background(), "", func(p RekeyProgress) { p1 = p }); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if p1.Rekeyed != 1 {
		t.Fatalf("first pass: got %+v, want Rekeyed=1", p1)
	}

	// Second pass: skips everything (kid == current).
	var p2 RekeyProgress
	if err := r.Run(context.Background(), "", func(p RekeyProgress) { p2 = p }); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if p2.Rekeyed != 0 || p2.Skipped != 1 {
		t.Fatalf("second pass: got %+v, want Rekeyed=0 Skipped=1", p2)
	}
}

// TestRun_CursorResumes: a non-empty cursor starts the walk past
// the cursor tuple — useful for daemon-restart resumption.
func TestRun_CursorResumes(t *testing.T) {
	previous := mustIdentity(t)
	current := mustIdentity(t)
	store := newFakeStore()
	for _, key := range []string{"A", "B", "C"} {
		store.put(state.AppSecret{
			AccountID:  "acct-1",
			AppID:      "app-1",
			Key:        key,
			Ciphertext: sealUnder(t, previous.Recipient(), key, "v"),
			Kid:        previous.Recipient().String(),
		})
	}

	r, err := New(store, []*age.X25519Identity{current, previous}, RekeyConfig{RowsPerSecond: 5000, BatchSize: 50})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Start past "B" — only C should be visited.
	var last RekeyProgress
	if err := r.Run(context.Background(), encodeCursor("acct-1", "app-1", "B"), func(p RekeyProgress) { last = p }); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if last.Total != 1 || last.Rekeyed != 1 {
		t.Fatalf("cursor walk: got %+v, want Total=1 Rekeyed=1 (only C)", last)
	}
}

// TestNew_RejectsEmptyIdentities: constructor precondition contract.
func TestNew_RejectsEmptyIdentities(t *testing.T) {
	if _, err := New(newFakeStore(), nil, RekeyConfig{}); err == nil {
		t.Fatal("expected error for empty identities slice")
	}
	if _, err := New(newFakeStore(), []*age.X25519Identity{nil}, RekeyConfig{}); err == nil {
		t.Fatal("expected error for nil current identity")
	}
}

// TestNew_FillsDefaults: zero cfg fields fall back to
// DefaultRekeyConfig so a unit-test or misconfigured daemon
// doesn't get a runaway goroutine.
func TestNew_FillsDefaults(t *testing.T) {
	id := mustIdentity(t)
	r, err := New(newFakeStore(), []*age.X25519Identity{id}, RekeyConfig{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if r.cfg.RowsPerSecond != DefaultRekeyConfig.RowsPerSecond {
		t.Fatalf("RowsPerSecond = %d, want %d", r.cfg.RowsPerSecond, DefaultRekeyConfig.RowsPerSecond)
	}
	if r.cfg.BatchSize != DefaultRekeyConfig.BatchSize {
		t.Fatalf("BatchSize = %d, want %d", r.cfg.BatchSize, DefaultRekeyConfig.BatchSize)
	}
	if r.cfg.OpenTimeout != DefaultRekeyConfig.OpenTimeout {
		t.Fatalf("OpenTimeout = %v, want %v", r.cfg.OpenTimeout, DefaultRekeyConfig.OpenTimeout)
	}
}

// mustIdentity generates a fresh X25519 identity for tests.
func mustIdentity(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	return id
}
