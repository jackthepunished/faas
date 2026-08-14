// Storage interfaces for the OIDC / keyless deploy path
// (issue #270 / ADR-101). Mirrors pkg/githubd/bindings_store.go:32-86
// — narrow per-table interfaces that pkg/state.Store can delegate
// to. The concrete PgStore / MemStore impls land in
// pkg/state/{pgstore,memstore}.go (PR-A scope); the interfaces
// themselves live here so pkg/oidc owns its own contract surface
// and pkg/state stays one-way.
//
// Two interfaces:
//
//   - OIDCTrustPolicyStore — (account_id, issuer_url) keyed; Upsert
//     handles the first-use auto-create path (PK conflict on the
//     existing row is treated as success so the exchange handler
//     can Upsert unconditionally).
//
//   - TokenExchangeStore — hash-keyed lookup for the Authenticator
//     middleware path; Insert on every successful exchange; Delete
//     on revoke (operator-initiated only; the 5-min TTL handles
//     natural expiry). GetByHash is the hot path; runs once per
//     bearer-bearing request.
package oidc

import (
	"context"
	"errors"
)

// ErrTrustPolicyNotFound is returned by GetTrustPolicy /
// LookupTrustPolicy when no row matches. Caller maps to a 404
// Problem; for the first-use auto-create path the caller treats
// this as "insert a default policy and retry". Distinct from
// pkg/state.ErrNotFound so the OIDC layer doesn't accidentally
// inherit a state-store-wide not-found semantics.
var ErrTrustPolicyNotFound = errors.New("oidc: trust policy not found")

// ErrTokenNotFound is returned by TokenExchangeStore.GetByHash when
// the bearer has expired (> 5 min old) or never existed. The
// middleware maps this to 401 "OIDC bearer not recognized" (same
// posture as the api_keys branch — state.ErrNotFound is intentionally
// NOT returned so the audit reader can distinguish "key never
// existed" from "long-lived key was revoked").
var ErrTokenNotFound = errors.New("oidc: exchanged token not found")

// OIDCTrustPolicyStore is the per-(account, issuer) policy table.
// Defined here (not in pkg/state/store.go) so the policy table's
// contract is owned by pkg/oidc.
type OIDCTrustPolicyStore interface {
	// Upsert inserts or updates the policy row. On first-use auto-
	// create the PK is fresh (no conflict); on dashboard-driven
	// refine (PR-C) the conflict path updates RequiredClaims and
	// SubjectPattern in place. Returns the policy as stored (with
	// CreatedAt / UpdatedAt server-stamped).
	Upsert(ctx context.Context, p *OIDCTrustPolicy) (*OIDCTrustPolicy, error)

	// Get returns the policy for (account_id, issuer_url). Returns
	// ErrTrustPolicyNotFound on miss.
	Get(ctx context.Context, accountID, issuerURL string) (*OIDCTrustPolicy, error)

	// ListForAccount returns every trust policy the account owns.
	// Used by the dashboard list page (PR-C). Empty slice on miss.
	ListForAccount(ctx context.Context, accountID string) ([]*OIDCTrustPolicy, error)
}

// TokenExchangeStore is the hash-keyed bearer lookup. Hot path;
// runs once per bearer-bearing request.
type TokenExchangeStore interface {
	// Insert stores a fresh ExchangedToken row. Caller generates the
	// bearer (api.GenerateOIDCKey) and hashes it (api.HashAPIKey)
	// before calling Insert; the row carries only the hash.
	Insert(ctx context.Context, t *ExchangedToken) error

	// GetByHash returns the row whose TokenHash equals the input.
	// Returns ErrTokenNotFound on miss. The caller checks ExpiresAt
	// before using the row — a stale row that survived a TTL race
	// surfaces as a 401, not as silent acceptance.
	GetByHash(ctx context.Context, hash []byte) (*ExchangedToken, error)

	// Delete is the operator-driven revoke path (PR-C). A 5-min
	// TTL row is normally reaped by lazy-Get (GetByHash on a
	// row past ExpiresAt returns ErrTokenNotFound); Delete is
	// for the "kill this CI job's credential now" case.
	Delete(ctx context.Context, id string) error
}
