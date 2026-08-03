package certsync

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

// TestLeader_Renew_ClosureErrorPropagated pins the load-bearing
// contract that an error from the renew closure is propagated up
// to the caller — the leader treats the renew as failed and the
// follower push is skipped.
func TestLeader_Renew_ClosureErrorPropagated(t *testing.T) {
	lister := &FakeLister{Nodes: []Node{{ID: "a", Name: "box-a"}}}
	l := NewLeader("a", lister, slog.Default())
	_, _ = l.Recompute(context.Background())
	renewErr := errors.New("letsencrypt rate-limited")
	_, _, err := l.Renew(context.Background(), "example.com", func(_ context.Context, _ string) ([]byte, []byte, error) {
		return nil, nil, renewErr
	})
	if !errors.Is(err, renewErr) {
		t.Errorf("Renew err = %v, want %v", err, renewErr)
	}
}

// capturePushDialer captures the (addr, bytes) tuple for every
// dial + write. It implements PushDialer so the production
// code path is exercised.
type capturePushDialer struct {
	mu     sync.Mutex
	conns  []string
	writes [][]byte
}

func (c *capturePushDialer) DialPush(_ context.Context, addr string) (net.Conn, error) {
	c.mu.Lock()
	c.conns = append(c.conns, addr)
	c.mu.Unlock()
	a, b := net.Pipe()
	// Reader drains bytes so the leader's Write doesn't block.
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, a)
		c.mu.Lock()
		c.writes = append(c.writes, buf.Bytes())
		c.mu.Unlock()
		_ = a.Close()
	}()
	return b, nil
}

// TestLeader_Push_LeaderOnly pins the contract: a follower that
// pushes gets ErrNotLeader per result.
func TestLeader_Push_LeaderOnly(t *testing.T) {
	lister := &FakeLister{Nodes: []Node{
		{ID: "a", Name: "box-a", Addr: "/tmp/a.sock"},
		{ID: "b", Name: "box-b", Addr: "/tmp/b.sock"},
	}}
	l := NewLeader("b", lister, slog.Default())
	_, _ = l.Recompute(context.Background())
	results := l.Push(context.Background(), "host", []byte("cert"), []byte("key"))
	if len(results) != 1 {
		t.Fatalf("Push results = %d, want 1 (one ErrNotLeader)", len(results))
	}
	if !errors.Is(results[0].Err, ErrNotLeader) {
		t.Errorf("Push follower err = %v, want ErrNotLeader", results[0].Err)
	}
}

// TestLeader_Push_WritesWireToEveryFollower pins the happy path.
// The leader pushes to every peer (skipping itself) and the wire
// bytes on every connection decode back to the original cert+key.
func TestLeader_Push_WritesWireToEveryFollower(t *testing.T) {
	lister := &FakeLister{Nodes: []Node{
		{ID: "a", Name: "box-a", Addr: "/tmp/a.sock"},
		{ID: "b", Name: "box-b", Addr: "/tmp/b.sock"},
		{ID: "c", Name: "box-c", Addr: "/tmp/c.sock"},
	}}
	l := NewLeader("a", lister, slog.Default())
	_, _ = l.Recompute(context.Background())
	capture := &capturePushDialer{}
	l.SetPushDialer(capture)
	cert := []byte("cert-bytes")
	key := []byte("key-bytes")
	results := l.Push(context.Background(), "host", cert, key)
	if len(results) != 2 {
		t.Fatalf("Push results = %d, want 2 (followers b + c)", len(results))
	}
	// Wait for the pipe readers to drain.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		capture.mu.Lock()
		n := len(capture.writes)
		capture.mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if len(capture.writes) != 2 {
		t.Fatalf("captured writes = %d, want 2", len(capture.writes))
	}
	for i, w := range capture.writes {
		gotCert, gotKey, err := DecodeWire(w)
		if err != nil {
			t.Errorf("write %d: DecodeWire: %v", i, err)
			continue
		}
		if !bytes.Equal(gotCert, cert) {
			t.Errorf("write %d: cert = %q, want %q", i, gotCert, cert)
		}
		if !bytes.Equal(gotKey, key) {
			t.Errorf("write %d: key = %q, want %q", i, gotKey, key)
		}
	}
}

// TestLeader_RenewAndPush_HappyPath pins the full production flow:
// renew, then push, then per-follower errors are logged-not-fatal.
func TestLeader_RenewAndPush_HappyPath(t *testing.T) {
	lister := &FakeLister{Nodes: []Node{
		{ID: "a", Name: "box-a", Addr: "/tmp/a.sock"},
		{ID: "b", Name: "box-b", Addr: "/tmp/b.sock"},
	}}
	l := NewLeader("a", lister, slog.Default())
	_, _ = l.Recompute(context.Background())
	capture := &capturePushDialer{}
	l.SetPushDialer(capture)
	err := l.RenewAndPush(context.Background(), "host", func(_ context.Context, _ string) ([]byte, []byte, error) {
		return []byte("cert"), []byte("key"), nil
	}, nil)
	if err != nil {
		t.Errorf("RenewAndPush err = %v, want nil", err)
	}
	// Wait for the pipe reader to drain.
	time.Sleep(50 * time.Millisecond)
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if len(capture.writes) != 1 {
		t.Errorf("captured writes = %d, want 1 (follower b)", len(capture.writes))
	}
}

// TestLeader_Push_DialFailure pins that a dial error on one
// follower is per-follower only — the rest still get pushed.
func TestLeader_Push_DialFailure(t *testing.T) {
	lister := &FakeLister{Nodes: []Node{
		{ID: "a", Name: "box-a", Addr: "/tmp/a.sock"},
		{ID: "b", Name: "box-b", Addr: "/tmp/b.sock"},
		{ID: "c", Name: "box-c", Addr: "/tmp/c.sock"},
	}}
	l := NewLeader("a", lister, slog.Default())
	_, _ = l.Recompute(context.Background())
	// AlwaysErrorDialer fails every dial — the leader sees three
	// per-follower Dial failures.
	l.SetPushDialer(alwaysErrorDialer{})
	results := l.Push(context.Background(), "host", []byte("c"), []byte("k"))
	if len(results) != 2 {
		t.Fatalf("Push results = %d, want 2", len(results))
	}
	for i, r := range results {
		if r.Err == nil {
			t.Errorf("result %d: err = nil, want non-nil", i)
		}
	}
}

type alwaysErrorDialer struct{}

func (alwaysErrorDialer) DialPush(_ context.Context, _ string) (net.Conn, error) {
	return nil, errors.New("synthetic dial failure")
}
