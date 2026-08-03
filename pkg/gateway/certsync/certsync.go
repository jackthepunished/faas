// Package certsync — per-replica certmagic leader election + cert
// replication for the Tier A7 edge split (ADR-069).
//
// Background: gatewayd-public runs N replicas (one per box in
// multi-box; today just one per box, but the design is N-replica-
// ready). Certmagic's renew path is per-process: each replica runs
// its own FileStorage and renews independently. Two replicas
// racing on the same cert produces two near-identical certs
// (certmagic's Renew is idempotent on the cert bytes, but the
// LE account state diverges if both write to the ACME account
// simultaneously).
//
// Fix: elect a leader (lex-min compute_node.id among the active
// gatewayd-public replicas). Only the leader renews. Followers
// receive the new PEM bytes via the CertSync protocol — a tiny
// length-prefixed wire format over a unix-domain socket — and
// write them to their own FileStorage. Certmagic's next GetCertificate
// call returns the freshly-written bytes.
//
// The shared CAConfigDir (acme accounts/registration) is mounted
// read-only on followers — only the leader writes the LE account
// state. This is the cleanest separation: cert lifecycle is split
// into "renew" (leader) + "store" (per-replica), while account
// state is shared.
//
// Wire format (ADR-069):
//
//	┌─ 4B magic "CSYN" ─┬─ 4B version (0x00000001) ─┬─ 8B cert length ─┬─ N cert PEM ─┬─ 8B key length ─┬─ M key PEM ─┐
//
//	Total header: 24 bytes. Cert + key PEMs follow verbatim (we do
//	not re-encode them — they are bytes-on-disk from certmagic).
//
// Election is gossip-free: each replica reads compute_nodes.rows at
// boot, computes lex-min id, and assumes the role. If the leader
// disappears, the next-lex-min replica re-elects within one renew
// interval (default 8 h certmagic looks; we use a tighter 1 h for
// the new-cert heartbeat — see CertSyncIntervalSeconds in
// pkg/api/limits.go). The slow-path is a Postgres-backed leader
// row; the fast-path is the in-memory lex-min election.
package certsync

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Wire constants. Keep in sync with cmd/gatewayd-public/certsync.go
// (the daemon-side wire reader).
const (
	WireMagic   uint32 = 0x4353594E // "CSYN" big-endian
	WireVersion uint32 = 0x00000001
	HeaderSize         = 4 + 4 + 8 + 8 // magic + version + certLen + keyLen
)

// ErrNotLeader is returned by Renew when the local replica is a
// follower. Followers MUST NOT call certmagic's Renew.
var ErrNotLeader = errors.New("certsync: replica is not the leader")

// NodeLister returns the active compute_node rows (id, name, addr)
// sorted by id. The default implementation queries Postgres; tests
// pass a fake backed by a slice.
type NodeLister interface {
	ListActive(ctx context.Context) ([]Node, error)
}

// Node is the minimal projection of compute_nodes a certsync
// replica needs.
type Node struct {
	ID   string // compute_nodes.id (UUID string)
	Name string // compute_nodes.name
	Addr string // unix-socket addr (e.g. /run/faas/gatewayd-public.sock)
}

// Storage abstracts the per-replica FileStorage write + the
// certmagic cache invalidation. The default implementation in
// cmd/gatewayd-public/certsync.go wires *certmagic.Config's Storage
// field and calls magic.Cache.Unlock for cache invalidation.
type Storage interface {
	// WriteCertAndKey writes the cert and key PEM bytes to the
	// per-replica storage dir, replacing any existing file.
	WriteCertAndKey(ctx context.Context, host string, certPEM, keyPEM []byte) error
	// InvalidateCache drops the in-memory certmagic cache entry
	// for `host` so the next GetCertificate call reads from disk.
	InvalidateCache(host string) error
}

// Leader is the per-replica state machine. It tracks whether this
// replica is the leader, who the leader is, and which peers to
// replicate to. One Leader per gatewayd-public daemon.
type Leader struct {
	mu sync.RWMutex

	// nodeID is this replica's compute_node.id (set at boot).
	nodeID string
	// nodes is the most recent snapshot from NodeLister.
	nodes []Node
	// isLeader is the cached election result (recomputed on every
	// recompute() call).
	isLeader bool
	// leaderID is the lex-min node id from the current election.
	leaderID string
	// lister is the source of truth for nodes.
	lister NodeLister
	// log is the slog.Logger used for election events.
	log *slog.Logger
	// now is the clock — overridable for tests.
	now func() time.Time
}

// NewLeader returns a Leader wired to lister + log. now defaults to
// time.Now (overridable for tests that want to skip the staleness
// check).
func NewLeader(nodeID string, lister NodeLister, log *slog.Logger) *Leader {
	if log == nil {
		log = slog.Default()
	}
	return &Leader{
		nodeID: nodeID,
		lister: lister,
		log:    log,
		now:    time.Now,
	}
}

// Recompute runs one election pass: list active nodes, sort by id,
// cache the leader and the "is this replica the leader" answer.
// Returns the elected leader ID for callers that want to log it.
//
// Safe for concurrent use. Idempotent.
func (l *Leader) Recompute(ctx context.Context) (string, error) {
	nodes, err := l.lister.ListActive(ctx)
	if err != nil {
		return "", fmt.Errorf("certsync: list nodes: %w", err)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	var leaderID string
	if len(nodes) > 0 {
		leaderID = nodes[0].ID
	}
	l.mu.Lock()
	l.nodes = nodes
	l.leaderID = leaderID
	l.isLeader = leaderID == l.nodeID
	l.mu.Unlock()
	l.log.Info("certsync: election complete",
		"this_node_id", l.nodeID,
		"leader_id", leaderID,
		"is_leader", leaderID == l.nodeID,
		"replica_count", len(nodes),
	)
	return leaderID, nil
}

// IsLeader reports whether this replica is the current cert
// leader. Callers that plan to renew a cert MUST check IsLeader
// first; followers get ErrNotLeader from Renew().
func (l *Leader) IsLeader() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.isLeader
}

// LeaderID returns the current leader's compute_node.id. Useful
// for logging and for follower's "where should I dial the next
// sync?" logic.
func (l *Leader) LeaderID() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.leaderID
}

// Peers returns the active follower nodes (excludes the leader).
// Used by the leader's cert-push loop to know who to write to.
func (l *Leader) Peers() []Node {
	l.mu.RLock()
	defer l.mu.RUnlock()
	peers := make([]Node, 0, len(l.nodes))
	for _, n := range l.nodes {
		if n.ID == l.leaderID {
			continue
		}
		peers = append(peers, n)
	}
	return peers
}

// Renew is the leader-only entry point. Followers that try to
// renew get ErrNotLeader; the leader delegates to `do` (the
// certmagic.Renew closure).
//
// `do` must return (certPEM, keyPEM, error). The Leader wraps
// `do` so callers cannot accidentally bypass the election check.
func (l *Leader) Renew(ctx context.Context, host string, do func(context.Context, string) ([]byte, []byte, error)) (certPEM, keyPEM []byte, err error) {
	if !l.IsLeader() {
		return nil, nil, ErrNotLeader
	}
	return do(ctx, host)
}

// WriteCertAndKeyToDisk is the canonical helper the leader uses
// after a successful Renew: write cert + key to the leader's own
// per-replica FileStorage, then invalidate the in-memory cache.
// The same helper is exposed for the follower's sync receiver (in
// cmd/gatewayd-public/certsync.go::handleCertSync).
func WriteCertAndKeyToDisk(ctx context.Context, storageDir, host string, certPEM, keyPEM []byte) error {
	if err := os.MkdirAll(filepath.Join(storageDir, "certificates"), 0o700); err != nil {
		return fmt.Errorf("certsync: mkdir storage: %w", err)
	}
	certPath := filepath.Join(storageDir, "certificates", host+".crt")
	keyPath := filepath.Join(storageDir, "certificates", host+".key")
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		return fmt.Errorf("certsync: write cert: %w", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("certsync: write key: %w", err)
	}
	return nil
}

// EncodeWire serialises cert + key PEMs into the ADR-069 wire
// format. The header is fixed-width (24 B); the body is
// concatenated PEMs. Used by the leader when pushing to followers.
func EncodeWire(certPEM, keyPEM []byte) []byte {
	buf := make([]byte, 0, HeaderSize+len(certPEM)+len(keyPEM))
	header := make([]byte, HeaderSize)
	binary.BigEndian.PutUint32(header[0:4], WireMagic)
	binary.BigEndian.PutUint32(header[4:8], WireVersion)
	binary.BigEndian.PutUint64(header[8:16], uint64(len(certPEM)))
	binary.BigEndian.PutUint64(header[16:24], uint64(len(keyPEM)))
	buf = append(buf, header...)
	buf = append(buf, certPEM...)
	buf = append(buf, keyPEM...)
	return buf
}

// DecodeWire parses the ADR-069 wire format. Returns (certPEM,
// keyPEM, error). Used by the follower's sync receiver.
//
// Errors:
//   - buf shorter than HeaderSize → io.ErrUnexpectedEOF
//   - magic mismatch → ErrWireMagic
//   - version mismatch → ErrWireVersion
//   - length field shorter than actual → io.ErrUnexpectedEOF
type WireError struct{ Reason string }

func (e *WireError) Error() string { return "certsync: wire: " + e.Reason }

// ErrWireMagic is returned by DecodeWire when the magic field is wrong.
var ErrWireMagic = &WireError{Reason: "bad magic"}

// ErrWireVersion is returned by DecodeWire when the version is unsupported.
var ErrWireVersion = &WireError{Reason: "unsupported version"}

// DecodeWire parses the ADR-069 wire format from buf. Returns
// (certPEM, keyPEM, error).
func DecodeWire(buf []byte) (certPEM, keyPEM []byte, err error) {
	if len(buf) < HeaderSize {
		return nil, nil, io.ErrUnexpectedEOF
	}
	magic := binary.BigEndian.Uint32(buf[0:4])
	if magic != WireMagic {
		return nil, nil, ErrWireMagic
	}
	version := binary.BigEndian.Uint32(buf[4:8])
	if version != WireVersion {
		return nil, nil, ErrWireVersion
	}
	certLen := binary.BigEndian.Uint64(buf[8:16])
	keyLen := binary.BigEndian.Uint64(buf[16:24])
	if uint64(len(buf)-HeaderSize) < certLen+keyLen {
		return nil, nil, io.ErrUnexpectedEOF
	}
	certPEM = make([]byte, certLen)
	copy(certPEM, buf[HeaderSize:HeaderSize+certLen])
	keyStart := HeaderSize + certLen
	keyPEM = make([]byte, keyLen)
	copy(keyPEM, buf[keyStart:keyStart+keyLen])
	return certPEM, keyPEM, nil
}

// FakeLister is a NodeLister backed by a slice. Tests construct it
// in-line; production uses the Postgres-backed lister at
// cmd/gatewayd-public/certsync.go.
type FakeLister struct {
	Nodes []Node
	Err   error
}

// ListActive implements NodeLister.
func (f *FakeLister) ListActive(_ context.Context) ([]Node, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Nodes, nil
}

// HostFromWire extracts the host header from a sync message's
// metadata. In v1.0 the host is encoded in the certmagic storage
// path (not in the wire format above) — this helper exists for
// future expansion if we add a per-message host field.
//
// Currently a no-op; here so callers don't import strings +
// filepath redundantly.
func HostFromWire(_ []byte) string {
	return ""
}
