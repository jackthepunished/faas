// In-memory NodeVerifier for tests and operator-override flows
// (ADR-056).
//
// Production uses PGNodeVerifier (a snapshot of compute_nodes.name
// refreshed on every compute_node_changed pg_notify). InmemNodeVerifier
// is the same shape — a CN-set — but populated manually. Tests use it
// to exercise the LookupCN contract without standing up Postgres.
// Operators can also use it as a static allow-list when the DB is
// unreachable (rare; the production wiring falls back to the
// last-known-good snapshot in PGNodeVerifier).
//
// Strict-nil semantics: a nil *InmemNodeVerifier returns
// ErrNodeVerifierCNMismatch for any CN. This is the OPPOSITE of
// AllowAllNodeVerifier — a forgotten wire in a test must NOT silently
// allow every CN. Tests that want allow-all use an
// AllowAllNodeVerifier explicitly.

package wire

import "sync"

// InmemNodeVerifier is the test + operator-override NodeVerifier.
// Backed by a CN-set; nil-receiver returns ErrNodeVerifierCNMismatch
// (strict) so a forgotten wire doesn't accidentally AllowAll.
//
// The set is rebuilt on every Set (full-snapshot, like PGNodeVerifier);
// LookupCN is RLock-guarded for parallel-handshake safety.
type InmemNodeVerifier struct {
	mu  sync.RWMutex
	set map[string]struct{}
}

// NewInmemNodeVerifier constructs an empty verifier. Use
// Set([]string{...}) to populate. The zero value is NOT usable
// directly — always construct via NewInmemNodeVerifier (or an explicit
// set field) so the map is non-nil.
func NewInmemNodeVerifier() *InmemNodeVerifier {
	return &InmemNodeVerifier{set: make(map[string]struct{})}
}

// Set atomically replaces the registered set. nil/empty clears.
// Mirrors PGNodeVerifier.Refresh's full-snapshot contract: a successful
// Set always swaps; a concurrent reader sees either the previous or
// the next snapshot, never a half-built map.
func (v *InmemNodeVerifier) Set(cns []string) {
	fresh := make(map[string]struct{}, len(cns))
	for _, c := range cns {
		if c != "" {
			fresh[c] = struct{}{}
		}
	}
	v.mu.Lock()
	v.set = fresh
	v.mu.Unlock()
}

// LookupCN implements NodeVerifier. nil-receiver returns
// ErrNodeVerifierCNMismatch so a forgotten wire in a test fails
// loudly instead of letting every handshake succeed.
func (v *InmemNodeVerifier) LookupCN(cn string) error {
	if v == nil {
		return nodeVerifierWithCN(ErrNodeVerifierCNMismatch, cn)
	}
	v.mu.RLock()
	_, ok := v.set[cn]
	v.mu.RUnlock()
	if !ok {
		return nodeVerifierWithCN(ErrNodeVerifierCNMismatch, cn)
	}
	return nil
}

// Size returns the registered count (tests + diagnostics).
func (v *InmemNodeVerifier) Size() int {
	if v == nil {
		return 0
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	return len(v.set)
}

// Ensure InmemNodeVerifier satisfies NodeVerifier at compile time.
var _ NodeVerifier = (*InmemNodeVerifier)(nil)
