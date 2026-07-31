package sched

// nodekeys.go — in-memory registry of (key_id → *ecdsa.PublicKey)
// populated from the compute_node_keys table (migration 00075).
//
// Background. ADR-053 closes the CapacityReport trust gap:
// every report carries a 64-byte ECDSA-P-256 (r||s) signature
// over the canonical payload, and the schedd handler verifies
// it against a key registered in this table. The registry is
// keyed by key_id — the SHA-256 hex of the leaf's
// SubjectPublicKeyInfo — so a rotated key doesn't accept
// signatures minted under the old one.
//
// The registry is the load-bearing enforcement; without it, a
// misconfigured vmmd (or an attacker with the wire) could send
// "valid ECDSA" signatures under an arbitrary ephemeral key,
// defeating the whole slice. The key_id binding is what makes
// the signature path useful.
//
// Lifecycle. NewNodeKeyRegistry returns an empty registry;
// schedd's wiring calls Refresh on startup AND subscribes to
// the 'compute_node_changed' pg_notify channel so a vmmd
// registering a new key (rotation, fresh boot) lands within
// the listener's next refresh tick. The lookup is a single
// map read under RWMutex.RLock; the chooser goroutine is the
// only reader.
//
// Out of scope (here, deferred to #316 + a future ADR):
//   - Overlap-window key rotation (one key_id stays accepted
//     for N hours after a new key_id is added).
//   - Audit table for rotations.
//
// Both ship when the runbook (issue #316) lands.

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"sync"
)

// NodeKeyLookup is the production-style accessor exported for
// other schedd internals (the gRPC handler in
// pkg/scheddgrpc.Server.ReportCapacity). It composes the same
// shape as nodeKeyLookup so callers can inject a stub in tests.
//
// PublicKey returns the registered *ecdsa.PublicKey for keyID.
// OK=false when the registry is nil or the key is unknown.
func (r *NodeKeyRegistry) PublicKey(keyID string) (*ecdsa.PublicKey, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	pub, ok := r.keys[keyID]
	return pub, ok
}

// NodeKeyRegistry is the in-memory key_id → *ecdsa.PublicKey
// map. Constructed once at schedd startup; refreshed via the
// 'compute_node_changed' pg_notify listener.
//
// RWMutex guards the map. Read path (PublicKey) takes RLock;
// write path (ReplaceAll) takes WLock. Refresh is full-snapshot
// (read every row, swap the map) — partial updates would
// require a per-row delta log and aren't worth the complexity
// at the typical fleet size (handful of rows).
type NodeKeyRegistry struct {
	mu   sync.RWMutex
	keys map[string]*ecdsa.PublicKey
	// loader is the production-side row loader; tests inject a
	// stub. Returns one (key_id, public_key_pem) tuple per row.
	// Returning an error aborts the refresh — schedd keeps the
	// last-known-good map and logs the failure.
	loader NodeKeyLoader
	// log is optional; pass nil in unit tests, the daemon's
	// slog.Logger at wiring time.
	log NodeKeyLogger
}

// NodeKeyLoader is the production-side Postgres loader. schedd
// constructs an implementation that runs
//
//	select key_id, public_key_pem from compute_node_keys
//
// against the pool. The interface lets tests inject an
// in-memory loader without spinning up a database.
type NodeKeyLoader interface {
	LoadNodeKeys(ctx context.Context) ([]NodeKeyRow, error)
}

// NodeKeyRow is one row from compute_node_keys. Loaded fresh on
// every Refresh; replaced atomically in the registry map.
type NodeKeyRow struct {
	KeyID        string
	PublicKeyPEM string
}

// NodeKeyLogger is the minimal slog.Logger interface so schedd's
// wiring can pass the daemon's structured logger and tests can
// pass nil (silent).
type NodeKeyLogger interface {
	Warn(msg string, args ...any)
	Info(msg string, args ...any)
}

// NewNodeKeyRegistry constructs an empty registry bound to
// loader + log. The wiring goroutine calls Refresh once at
// startup; the pg_notify listener calls it on every
// 'compute_node_changed' tick. Both paths share the
// full-snapshot ReplaceAll so partial-update bugs can't
// silently de-sync the map.
func NewNodeKeyRegistry(loader NodeKeyLoader, log NodeKeyLogger) *NodeKeyRegistry {
	return &NodeKeyRegistry{
		keys:   make(map[string]*ecdsa.PublicKey),
		loader: loader,
		log:    log,
	}
}

// ReplaceAll atomically swaps the registry's map for a fresh
// build from rows. Rows whose PEM fails to parse are skipped
// (with a Warn log) so a single malformed row doesn't
// sabotage the whole registry.
//
// nil receiver is tolerated (no-op). Returns the number of
// rows successfully parsed — useful for diagnostics and tests.
func (r *NodeKeyRegistry) ReplaceAll(rows []NodeKeyRow) int {
	if r == nil {
		return 0
	}
	fresh := make(map[string]*ecdsa.PublicKey, len(rows))
	for _, row := range rows {
		pub, err := parsePublicKeyPEM(row.PublicKeyPEM)
		if err != nil {
			if r.log != nil {
				r.log.Warn("sched: skip unparseable node key",
					"key_id", row.KeyID, "err", err)
			}
			continue
		}
		fresh[row.KeyID] = pub
	}
	r.mu.Lock()
	r.keys = fresh
	r.mu.Unlock()
	return len(fresh)
}

// Size returns the count of registered keys. Useful for
// Prometheus metrics (a future slice adds
// node_key_registry_size).
func (r *NodeKeyRegistry) Size() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.keys)
}

// Refresh calls the loader and swaps the registry map. Errors
// keep the last-known-good map (no destructive partial updates)
// and are logged at Warn. Returns the count of keys after
// refresh; 0 + a non-nil error means the registry is now empty
// (loaders must not return rows + error).
func (r *NodeKeyRegistry) Refresh(ctx context.Context) (int, error) {
	if r == nil {
		return 0, errors.New("sched: nil registry")
	}
	if r.loader == nil {
		return 0, errors.New("sched: nil loader")
	}
	rows, err := r.loader.LoadNodeKeys(ctx)
	if err != nil {
		if r.log != nil {
			r.log.Warn("sched: node key loader failed; keeping last-known-good",
				"err", err)
		}
		return r.Size(), fmt.Errorf("load: %w", err)
	}
	n := r.ReplaceAll(rows)
	if r.log != nil {
		r.log.Info("sched: node key registry refreshed",
			"keys", n, "rows_loaded", len(rows))
	}
	return n, nil
}

// parsePublicKeyPEM parses a PEM-encoded SubjectPublicKeyInfo
// into an *ecdsa.PublicKey. Returns an error when the PEM is
// malformed, the algorithm is not ECDSA, or the curve is not
// P-256 (the only curve the platform supports; mirrors
// pkg/cosign's strictness).
func parsePublicKeyPEM(pemStr string) (*ecdsa.PublicKey, error) {
	if pemStr == "" {
		return nil, errors.New("empty PEM")
	}
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("not PEM-encoded")
	}
	if block.Type != "PUBLIC KEY" {
		return nil, fmt.Errorf("PEM type %q, want PUBLIC KEY", block.Type)
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse PKIX: %w", err)
	}
	ec, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("not ECDSA (got %T)", pub)
	}
	if ec.Curve != ecdsaP256() {
		return nil, fmt.Errorf("curve %s, want P-256", ec.Curve.Params().Name)
	}
	return ec, nil
}

// Compile-time guards.
var (
	_ nodeKeyLookup = (*NodeKeyRegistry)(nil)
)
