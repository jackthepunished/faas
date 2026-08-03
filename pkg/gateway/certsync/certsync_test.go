package certsync

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// TestLeader_LexMinElection pins the load-bearing contract: the
// leader is the lex-min compute_node.id, regardless of the order
// the lister returns nodes.
func TestLeader_LexMinElection(t *testing.T) {
	lister := &FakeLister{Nodes: []Node{
		{ID: "cccccccc-cccc-cccc-cccc-cccccccccccc", Name: "box-c"},
		{ID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", Name: "box-a"},
		{ID: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", Name: "box-b"},
	}}
	l := NewLeader("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", lister, slog.Default())
	if _, err := l.Recompute(context.Background()); err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	if !l.IsLeader() {
		t.Errorf("IsLeader() = false, want true (this node is lex-min)")
	}
	if l.LeaderID() != "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa" {
		t.Errorf("LeaderID() = %q, want the lex-min id", l.LeaderID())
	}
}

// TestLeader_Follower pins the contract that a non-lex-min node is
// NOT the leader.
func TestLeader_Follower(t *testing.T) {
	lister := &FakeLister{Nodes: []Node{
		{ID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", Name: "box-a"},
		{ID: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", Name: "box-b"},
	}}
	l := NewLeader("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", lister, slog.Default())
	if _, err := l.Recompute(context.Background()); err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	if l.IsLeader() {
		t.Errorf("IsLeader() = true, want false (lex-min is a, this is b)")
	}
	if l.LeaderID() != "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa" {
		t.Errorf("LeaderID() = %q, want a", l.LeaderID())
	}
}

// TestLeader_EmptyLister pins the empty-cluster case: with no
// nodes, no replica claims leadership.
func TestLeader_EmptyLister(t *testing.T) {
	lister := &FakeLister{Nodes: nil}
	l := NewLeader("solo-node", lister, slog.Default())
	if _, err := l.Recompute(context.Background()); err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	if l.IsLeader() {
		t.Errorf("IsLeader() = true with empty cluster, want false")
	}
	if l.LeaderID() != "" {
		t.Errorf("LeaderID() = %q with empty cluster, want \"\"", l.LeaderID())
	}
}

// TestLeader_RecomputeError propagates the lister error to the caller.
func TestLeader_RecomputeError(t *testing.T) {
	lister := &FakeLister{Err: errors.New("pg down")}
	l := NewLeader("solo-node", lister, slog.Default())
	_, err := l.Recompute(context.Background())
	if err == nil {
		t.Fatalf("Recompute returned nil err, want error")
	}
}

// TestLeader_PeersExcludesLeader pins the "peers" semantics: a
// leader's peer list excludes itself.
func TestLeader_PeersExcludesLeader(t *testing.T) {
	lister := &FakeLister{Nodes: []Node{
		{ID: "a", Name: "box-a"},
		{ID: "b", Name: "box-b"},
		{ID: "c", Name: "box-c"},
	}}
	l := NewLeader("a", lister, slog.Default())
	if _, err := l.Recompute(context.Background()); err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	peers := l.Peers()
	if len(peers) != 2 {
		t.Fatalf("len(Peers) = %d, want 2", len(peers))
	}
	for _, p := range peers {
		if p.ID == "a" {
			t.Errorf("Peers includes the leader (id=a)")
		}
	}
}

// TestLeader_Renew_FollowerRejected pins the load-bearing safety:
// only the leader can renew.
func TestLeader_Renew_FollowerRejected(t *testing.T) {
	lister := &FakeLister{Nodes: []Node{
		{ID: "a", Name: "box-a"},
		{ID: "b", Name: "box-b"},
	}}
	l := NewLeader("b", lister, slog.Default())
	_, _ = l.Recompute(context.Background())
	called := false
	_, _, err := l.Renew(context.Background(), "example.com", func(_ context.Context, _ string) ([]byte, []byte, error) {
		called = true
		return []byte("cert"), []byte("key"), nil
	})
	if !errors.Is(err, ErrNotLeader) {
		t.Errorf("Renew err = %v, want ErrNotLeader", err)
	}
	if called {
		t.Errorf("Renew delegated to closure on follower")
	}
}

// TestLeader_Renew_LeaderDelegates pins the happy path: the leader
// passes the closure through.
func TestLeader_Renew_LeaderDelegates(t *testing.T) {
	lister := &FakeLister{Nodes: []Node{{ID: "a", Name: "box-a"}}}
	l := NewLeader("a", lister, slog.Default())
	_, _ = l.Recompute(context.Background())
	cert, key, err := l.Renew(context.Background(), "example.com", func(_ context.Context, _ string) ([]byte, []byte, error) {
		return []byte("cert-bytes"), []byte("key-bytes"), nil
	})
	if err != nil {
		t.Fatalf("Renew: %v", err)
	}
	if string(cert) != "cert-bytes" {
		t.Errorf("cert = %q, want cert-bytes", string(cert))
	}
	if string(key) != "key-bytes" {
		t.Errorf("key = %q, want key-bytes", string(key))
	}
}

// TestEncodeDecodeWire_RoundTrip pins the wire format.
func TestEncodeDecodeWire_RoundTrip(t *testing.T) {
	cert := []byte("-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n")
	key := []byte("-----BEGIN PRIVATE KEY-----\nfake\n-----END PRIVATE KEY-----\n")
	encoded := EncodeWire(cert, key)
	gotCert, gotKey, err := DecodeWire(encoded)
	if err != nil {
		t.Fatalf("DecodeWire: %v", err)
	}
	if !bytes.Equal(gotCert, cert) {
		t.Errorf("cert round-trip mismatch: %q vs %q", gotCert, cert)
	}
	if !bytes.Equal(gotKey, key) {
		t.Errorf("key round-trip mismatch: %q vs %q", gotKey, key)
	}
}

// TestDecodeWire_BadMagic pins the rejection of unknown wire formats.
func TestDecodeWire_BadMagic(t *testing.T) {
	buf := make([]byte, HeaderSize)
	binary.BigEndian.PutUint32(buf[0:4], 0xDEADBEEF) // not "CSYN"
	binary.BigEndian.PutUint32(buf[4:8], WireVersion)
	binary.BigEndian.PutUint64(buf[8:16], 0)
	binary.BigEndian.PutUint64(buf[16:24], 0)
	_, _, err := DecodeWire(buf)
	if !errors.Is(err, ErrWireMagic) {
		t.Errorf("err = %v, want ErrWireMagic", err)
	}
}

// TestDecodeWire_BadVersion pins the rejection of unsupported
// versions (so a v0.2 leader doesn't accidentally push to a v0.1
// follower that misinterprets the bytes).
func TestDecodeWire_BadVersion(t *testing.T) {
	buf := make([]byte, HeaderSize)
	binary.BigEndian.PutUint32(buf[0:4], WireMagic)
	binary.BigEndian.PutUint32(buf[4:8], 99) // unsupported
	binary.BigEndian.PutUint64(buf[8:16], 0)
	binary.BigEndian.PutUint64(buf[16:24], 0)
	_, _, err := DecodeWire(buf)
	if !errors.Is(err, ErrWireVersion) {
		t.Errorf("err = %v, want ErrWireVersion", err)
	}
}

// TestDecodeWire_ShortBuffer pins the truncation safety.
func TestDecodeWire_ShortBuffer(t *testing.T) {
	_, _, err := DecodeWire([]byte{0x43, 0x53}) // shorter than HeaderSize
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("err = %v, want io.ErrUnexpectedEOF", err)
	}
}

// TestWriteCertAndKeyToDisk exercises the canonical writer. It
// creates a tempdir, writes the cert + key, then re-reads them.
func TestWriteCertAndKeyToDisk(t *testing.T) {
	dir := t.TempDir()
	cert := []byte("cert")
	key := []byte("key")
	if err := WriteCertAndKeyToDisk(context.Background(), dir, "example.com", cert, key); err != nil {
		t.Fatalf("WriteCertAndKeyToDisk: %v", err)
	}
	certPath := filepath.Join(dir, "certificates", "example.com.crt")
	keyPath := filepath.Join(dir, "certificates", "example.com.key")
	gotCert, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("ReadFile cert: %v", err)
	}
	if !bytes.Equal(gotCert, cert) {
		t.Errorf("cert = %q, want %q", gotCert, cert)
	}
	gotKey, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("ReadFile key: %v", err)
	}
	if !bytes.Equal(gotKey, key) {
		t.Errorf("key = %q, want %q", gotKey, key)
	}
	// Permissions check — the canonical helper sets 0600 so cert
	// blobs are not world-readable.
	info, err := os.Stat(certPath)
	if err != nil {
		t.Fatalf("Stat cert: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("cert perms = %o, want 0600", perm)
	}
}

// TestLeader_ConcurrentReads pins the safety of concurrent
// IsLeader / LeaderID reads.
func TestLeader_ConcurrentReads(t *testing.T) {
	lister := &FakeLister{Nodes: []Node{{ID: "a"}, {ID: "b"}, {ID: "c"}}}
	l := NewLeader("b", lister, slog.Default())
	_, _ = l.Recompute(context.Background())
	var wg sync.WaitGroup
	var n atomic.Int32
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				_ = l.IsLeader()
				_ = l.LeaderID()
				_ = l.Peers()
				n.Add(3)
			}
		}()
	}
	wg.Wait()
	if got := n.Load(); got != 300*1000 {
		t.Errorf("counter = %d, want %d", got, 300*1000)
	}
}

// TestFakeLister_PassesThroughError pins the fake's error path so
// callers don't accidentally swallow a real PG outage in tests.
func TestFakeLister_PassesThroughError(t *testing.T) {
	want := errors.New("synthetic pg outage")
	f := &FakeLister{Err: want}
	_, err := f.ListActive(context.Background())
	if !errors.Is(err, want) {
		t.Errorf("ListActive err = %v, want %v", err, want)
	}
	// Sanity: no nodes returned on error.
	if nodes := f.Nodes; nodes != nil {
		t.Errorf("FakeLister.Nodes = %v with Err set, want nil", nodes)
	}
}
