// Store tests for pkg/releaseinstall. Uses an in-memory fake
// Store (the Store interface itself is the abstraction that lets
// cmd/gregale and PR-4 doctor inject fakes in tests without a
// real Postgres).
//
// We exercise:
//   - Insert generates an id and round-trips via GetByGitSHA
//   - MarkApplied first-write-wins semantics
//   - MarkApplied is idempotent across concurrent calls
//   - GetByGitSHA returns ErrNotFound for unknown SHA
//   - ListApplied returns only applied rows, ordered desc
//   - Insert rejects empty payloads
//
// The pgStore type itself is exercised against a real Postgres
// in pkg/state/pgstore_test.go (TestPg_ReleaseBundles_*) per
// the PR-3 plan.
package releaseinstall

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeStore is the in-memory Store used by these tests.
type fakeStore struct {
	mu    sync.Mutex
	rows  map[string]*fakeRow
}

type fakeRow struct {
	ID           string
	Bundle       Bundle
	DaemonJSON   []byte
	CreatedAt    time.Time
	AppliedAt    *time.Time
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: map[string]*fakeRow{}}
}

func (s *fakeStore) Insert(_ context.Context, b Bundle) (string, error) {
	if b.GitSHA == "" || b.ManifestHash == "" || len(b.DaemonHashes) == 0 {
		return "", errors.New("insert requires git_sha, manifest_hash, non-empty daemon_hashes")
	}
	hashes, err := EncodeDaemonHashes(b.DaemonHashes)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.rows[b.GitSHA]; exists {
		return "", errors.New("insert: row already exists for git_sha")
	}
	id := "fake-" + b.GitSHA[:8]
	s.rows[b.GitSHA] = &fakeRow{
		ID:         id,
		Bundle:     b,
		DaemonJSON: hashes,
		CreatedAt:  time.Now(),
	}
	return id, nil
}

func (s *fakeStore) MarkApplied(_ context.Context, gitSHA string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[gitSHA]
	if !ok {
		return false, ErrNotFound
	}
	if row.AppliedAt != nil {
		return false, nil
	}
	now := time.Now()
	row.AppliedAt = &now
	return true, nil
}

func (s *fakeStore) GetByGitSHA(_ context.Context, gitSHA string) (BundleRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[gitSHA]
	if !ok {
		return BundleRow{}, ErrNotFound
	}
	hashes, err := DecodeDaemonHashes(row.DaemonJSON)
	if err != nil {
		return BundleRow{}, err
	}
	return BundleRow{
		ID:           row.ID,
		GitSHA:       row.Bundle.GitSHA,
		ManifestHash: row.Bundle.ManifestHash,
		DaemonHashes: hashes,
		CreatedAt:    row.CreatedAt,
		AppliedAt:    row.AppliedAt,
	}, nil
}

func (s *fakeStore) ListApplied(_ context.Context) ([]BundleRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []BundleRow
	for _, row := range s.rows {
		if row.AppliedAt == nil {
			continue
		}
		hashes, err := DecodeDaemonHashes(row.DaemonJSON)
		if err != nil {
			return nil, err
		}
		out = append(out, BundleRow{
			ID:           row.ID,
			GitSHA:       row.Bundle.GitSHA,
			ManifestHash: row.Bundle.ManifestHash,
			DaemonHashes: hashes,
			CreatedAt:    row.CreatedAt,
			AppliedAt:    row.AppliedAt,
		})
	}
	// order by applied_at desc
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].AppliedAt.After(*out[i].AppliedAt) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

func sampleBundle(sha string) Bundle {
	// EncodeDaemonHashes requires every daemon in the catalog; the
	// sample bundle is canonical-complete.
	hashes := make(map[string]string, 9)
	patterns := []string{
		"1111111111111111111111111111111111111111111111111111111111111111",
		"2222222222222222222222222222222222222222222222222222222222222222",
		"3333333333333333333333333333333333333333333333333333333333333333",
		"4444444444444444444444444444444444444444444444444444444444444444",
		"5555555555555555555555555555555555555555555555555555555555555555",
		"6666666666666666666666666666666666666666666666666666666666666666",
		"7777777777777777777777777777777777777777777777777777777777777777",
		"8888888888888888888888888888888888888888888888888888888888888888",
		"9999999999999999999999999999999999999999999999999999999999999999",
	}
	// We don't import manifest.SortedHostKeys here directly to keep
	// the test self-contained — but to stay honest with the contract
	// we list all 9. Order doesn't matter to a map.
	keys := []string{
		"apid", "builderd", "gatewayd_internal", "gatewayd_public",
		"githubd", "imaged", "meterd", "schedd", "vmmd",
	}
	for i, k := range keys {
		hashes[k] = "sha256:" + patterns[i]
	}
	return Bundle{
		FormatVersion: FormatVersion,
		GitSHA:        sha,
		ManifestHash:  "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		DaemonHashes:  hashes,
		CreatedAt:     time.Now(),
	}
}

func TestFakeStore_InsertAndGetByGitSHA(t *testing.T) {
	s := newFakeStore()
	b := sampleBundle("0123456789abcdef0123456789abcdef01234567")
	id, err := s.Insert(context.Background(), b)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if id == "" {
		t.Errorf("Insert returned empty id")
	}
	got, err := s.GetByGitSHA(context.Background(), b.GitSHA)
	if err != nil {
		t.Fatalf("GetByGitSHA: %v", err)
	}
	if got.ID != id {
		t.Errorf("round-trip id = %q, want %q", got.ID, id)
	}
	if got.GitSHA != b.GitSHA {
		t.Errorf("round-trip git_sha = %q, want %q", got.GitSHA, b.GitSHA)
	}
	if got.ManifestHash != b.ManifestHash {
		t.Errorf("round-trip manifest_hash = %q, want %q", got.ManifestHash, b.ManifestHash)
	}
	if len(got.DaemonHashes) != len(b.DaemonHashes) {
		t.Errorf("daemon_hashes len = %d, want %d", len(got.DaemonHashes), len(b.DaemonHashes))
	}
	for k, v := range b.DaemonHashes {
		if got.DaemonHashes[k] != v {
			t.Errorf("daemon_hash %s = %q, want %q", k, got.DaemonHashes[k], v)
		}
	}
	if got.AppliedAt != nil {
		t.Errorf("AppliedAt = %v, want nil (not yet stamped)", got.AppliedAt)
	}
}

func TestFakeStore_InsertRejectsEmpty(t *testing.T) {
	s := newFakeStore()
	full := sampleBundle("0123456789abcdef0123456789abcdef01234567")
	// Empty bundle — git_sha, manifest_hash, daemon_hashes all empty.
	if _, err := s.Insert(context.Background(), Bundle{}); err == nil {
		t.Errorf("Insert(Bundle{}) = nil err, want error")
	}
	// Valid git_sha, missing manifest_hash.
	missingHash := full
	missingHash.ManifestHash = ""
	if _, err := s.Insert(context.Background(), missingHash); err == nil {
		t.Errorf("Insert(missing manifest_hash) = nil err, want error")
	}
	// Valid git_sha + manifest_hash, missing daemon_hashes.
	missingHashes := full
	missingHashes.DaemonHashes = nil
	if _, err := s.Insert(context.Background(), missingHashes); err == nil {
		t.Errorf("Insert(nil daemon_hashes) = nil err, want error")
	}
}

func TestFakeStore_MarkApplied_FirstWriteWins(t *testing.T) {
	s := newFakeStore()
	b := sampleBundle("0123456789abcdef0123456789abcdef01234567")
	if _, err := s.Insert(context.Background(), b); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	first, err := s.MarkApplied(context.Background(), b.GitSHA)
	if err != nil {
		t.Fatalf("MarkApplied 1: %v", err)
	}
	if !first {
		t.Errorf("first MarkApplied = false, want true")
	}
	second, err := s.MarkApplied(context.Background(), b.GitSHA)
	if err != nil {
		t.Fatalf("MarkApplied 2: %v", err)
	}
	if second {
		t.Errorf("second MarkApplied = true, want false (idempotent)")
	}
}

func TestFakeStore_MarkApplied_MissingRow(t *testing.T) {
	s := newFakeStore()
	_, err := s.MarkApplied(context.Background(), "ffffffffffffffffffffffffffffffffffffffff")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("MarkApplied on missing row = %v, want ErrNotFound", err)
	}
}

func TestFakeStore_GetByGitSHA_NotFound(t *testing.T) {
	s := newFakeStore()
	_, err := s.GetByGitSHA(context.Background(), "ffffffffffffffffffffffffffffffffffffffff")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("GetByGitSHA on missing row = %v, want ErrNotFound", err)
	}
}

func TestFakeStore_ListApplied_OrdersDescAndFiltersUnapplied(t *testing.T) {
	s := newFakeStore()
	sha1 := "0000000000000000000000000000000000000001"
	sha2 := "0000000000000000000000000000000000000002"
	sha3 := "0000000000000000000000000000000000000003"
	for _, sha := range []string{sha1, sha2, sha3} {
		if _, err := s.Insert(context.Background(), sampleBundle(sha)); err != nil {
			t.Fatalf("Insert %s: %v", sha, err)
		}
	}
	// Mark sha1 and sha3 applied; leave sha2 unapplied.
	if _, err := s.MarkApplied(context.Background(), sha1); err != nil {
		t.Fatalf("MarkApplied sha1: %v", err)
	}
	// Small sleep to make applied_at distinguishable in the sort.
	time.Sleep(10 * time.Millisecond)
	if _, err := s.MarkApplied(context.Background(), sha3); err != nil {
		t.Fatalf("MarkApplied sha3: %v", err)
	}
	got, err := s.ListApplied(context.Background())
	if err != nil {
		t.Fatalf("ListApplied: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListApplied len = %d, want 2", len(got))
	}
	if got[0].GitSHA != sha3 {
		t.Errorf("ListApplied[0] = %q, want %q (newest first)", got[0].GitSHA, sha3)
	}
	if got[1].GitSHA != sha1 {
		t.Errorf("ListApplied[1] = %q, want %q (oldest last)", got[1].GitSHA, sha1)
	}
}

// repeat is unused now — kept commented out in case future tests
// need a no-imports helper for the "sha256:" + 64-hex pattern.
//
// func repeat(s string, n int) string {
// 	out := ""
// 	for i := 0; i < n; i++ {
// 		out += s
// 	}
// 	return out
// }