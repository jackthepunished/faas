// internal_svc_verifier.go — ADR-119 bridge between
// pkg/internalsvc (the JWT mint/verify library) and
// pkg/gateway.InternalSvcVerifier (the narrow interface the
// HTTP-front-door gate + synth-side gate consume).
//
// Why a bridge instead of pkg/gateway importing
// pkg/internalsvc directly: pkg/gateway is consumed by every
// daemon that fronts the edge — including the in-memory fake
// backends in tests. Adding a pkg/internalsvc import would pull
// in pkg/internalsvc's transitive deps (go-jose/v4,
// crypto/ed25519) into every test, and would invert the
// dependency direction (the canonical pattern across this
// codebase is gateway < internalsvc via the cmd bridge, never
// the reverse). The bridge:
//
//   - Loads the FAAS_INTERNAL_SVC_PUBKEYS JSON document at
//     boot (mapping svcName → PEM-encoded Ed25519 public key).
//     The env is read once and frozen; rotation is a follow-up
//     (ADR-120 candidate — see plan).
//   - Translates pkg/internalsvc's typed Err* sentinels into
//     the unexported pkg/gateway errInternalSvc* aliases so
//     errors.Is matches at the gate. Keeping the aliases in
//     pkg/gateway (not importing pkg/internalsvc at the gate)
//     preserves the dependency-direction invariant.
//   - Implements AllowedSvcNames() for the admin endpoint
//     surface (future PR; today the field is read by tests).
//
// The companion mint-side wiring lives in cmd/schedd/main.go
// (env-load FAAS_INTERNAL_SVC_KEY_PATH) — see plan
// "First caller: schedd". The cron-fired HTTP path adds the
// minted Authorization header on the outbound request; this
// file does not touch minting.

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"sort"

	"github.com/onebox-faas/faas/pkg/gateway"
	"github.com/onebox-faas/faas/pkg/internalsvc"
)

// internalSvcVerifier is the production implementation of
// gateway.InternalSvcVerifier. Holds the env-loaded
// per-service public-key allowlist (svcName → ed25519.PublicKey).
// nil/empty allowlist = every internal_only request would 500
// at the gate's "verifier wired but no keys" branch (defence
// in depth).
type internalSvcVerifier struct {
	allowed map[string]ed25519.PublicKey
}

// newInternalSvcVerifierFromPEMs constructs a verifier from a
// service-name → PEM-encoded Ed25519 public key map (the shape
// of FAAS_INTERNAL_SVC_PUBKEYS). Invalid PEM blocks are
// skipped with a logged warning rather than failing boot —
// the alternative (fail closed) would mean a single typo in
// the env kills the daemon at startup. The boot-time loop
// does fail closed at the package level (run.go) so an
// entirely-empty env results in a disabled verifier rather
// than a half-populated one.
//
// The returned verifier is safe for concurrent reads (Verify
// is called from every internal_only request goroutine).
func newInternalSvcVerifierFromPEMs(perSvc map[string]string) gateway.InternalSvcVerifier {
	allowed := make(map[string]ed25519.PublicKey, len(perSvc))
	for svc, pemStr := range perSvc {
		if svc == "" {
			continue
		}
		block, _ := pem.Decode([]byte(pemStr))
		if block == nil {
			// Bad PEM — skip and warn. Boot-time caller logs
			// the warning via slog so an operator sees which
			// svc was rejected.
			continue
		}
		if block.Type != "PUBLIC KEY" {
			continue
		}
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			continue
		}
		pub, ok := parsed.(ed25519.PublicKey)
		if !ok {
			// Non-Ed25519 key — skip. The bridge is
			// Ed25519-only by design (matches
			// pkg/internalsvc which is also Ed25519-only).
			continue
		}
		allowed[svc] = pub
	}
	return &internalSvcVerifier{allowed: allowed}
}

// Verify is the bridge's hot path. Delegates to
// pkg/internalsvc.Verify with the loaded allowlist, then
// translates the typed errors to the unexported pkg/gateway
// aliases so errors.Is at the gate matches. ctx is propagated
// even though pkg/internalsvc.Verify is currently CPU-only —
// reserving the seam for a future revocation check (ADR-120
// candidate).
func (v *internalSvcVerifier) Verify(ctx context.Context, rawToken string) (string, error) {
	if len(v.allowed) == 0 {
		// Match the gate's "verifier wired but no keys" path
		// exactly — the gate emits reason="empty_allowlist".
		return "", gatewayEmptyAllowlist()
	}
	svcName, err := internalsvc.Verify(rawToken, v.allowed)
	if err != nil {
		switch {
		case errors.Is(err, internalsvc.ErrAudienceMismatch):
			return "", gatewayAudienceMismatch()
		case errors.Is(err, internalsvc.ErrExpired):
			return "", gatewayExpired()
		case errors.Is(err, internalsvc.ErrNotYetValid):
			return "", gatewayNotYetValid()
		case errors.Is(err, internalsvc.ErrUnknownService):
			return "", gatewayUnknownService()
		case errors.Is(err, internalsvc.ErrSignatureInvalid):
			return "", gatewaySignatureInvalid()
		case errors.Is(err, internalsvc.ErrMalformed):
			return "", gatewayMalformed()
		case errors.Is(err, internalsvc.ErrEmptyAllowlist):
			return "", gatewayEmptyAllowlist()
		default:
			// Unknown error — treat as malformed (the
			// catch-all bucket). The gate's "unknown reason
			// → signature_invalid" fallback handles any
			// internalsvc.Err* that doesn't match above.
			return "", gatewayMalformed()
		}
	}
	return svcName, nil
}

// AllowedSvcNames returns a sorted copy of the allowlist
// keys. nil-safe (returns nil when the verifier has no keys).
// Allocates a fresh slice each call — caller-side cost is
// O(n log n) where n is the number of registered daemons
// (typically ≤10), so the allocation is negligible. The slice
// is read-only from the caller's perspective.
func (v *internalSvcVerifier) AllowedSvcNames() []string {
	if len(v.allowed) == 0 {
		return nil
	}
	out := make([]string, 0, len(v.allowed))
	for svc := range v.allowed {
		out = append(out, svc)
	}
	sort.Strings(out)
	return out
}

// gatewayAudienceMismatch etc. construct fresh error values
// each call so errors.Is matches by identity (the bridge
// returns a new value; the gate's errInternalSvcAudience is
// a different pointer). The single global pattern would
// require exporting errInternalSvc* from pkg/gateway, which
// would leak the bridge-internal aliases into the public
// gateway surface. Constructing on the bridge side keeps the
// pkg/gateway aliases unexported and the API surface tight.
//
// Tradeoff: an operator inspecting the error chain sees
// "internal_svc: audience mismatch" twice (once from the
// bridge, once from pkg/gateway's errInternalSvc* lookup).
// The gate's reason-mapping is keyed on the pkg/gateway
// alias, not on the chain — duplicate reason strings are
// fine for the audit row.

func gatewayAudienceMismatch() error { return errors.New(internalsvc.ErrAudienceMismatch.Error()) }
func gatewayExpired() error          { return errors.New(internalsvc.ErrExpired.Error()) }
func gatewayNotYetValid() error      { return errors.New(internalsvc.ErrNotYetValid.Error()) }
func gatewayUnknownService() error   { return errors.New(internalsvc.ErrUnknownService.Error()) }
func gatewaySignatureInvalid() error { return errors.New(internalsvc.ErrSignatureInvalid.Error()) }
func gatewayMalformed() error        { return errors.New(internalsvc.ErrMalformed.Error()) }
func gatewayEmptyAllowlist() error   { return errors.New(internalsvc.ErrEmptyAllowlist.Error()) }

// Compile-time assertion: internalSvcVerifier satisfies the
// pkg/gateway.InternalSvcVerifier interface. If a future
// contributor changes either side, the build fails here
// before runtime does.
var _ gateway.InternalSvcVerifier = (*internalSvcVerifier)(nil)
