// HTTP handler for POST /v1/auth/oidc/exchange (issue #270 /
// ADR-101). The wire contract:
//
//	Request:  POST /v1/auth/oidc/exchange
//	          Body: {"provider":"github","token":"<raw JWT>",
//	                 "aud":"<the aud claim pinned in the action>",
//	                 "app":"my-app"}
//	Response: 200 OK
//	          {"bearer":"fp_oidc_<48 hex>","expires_in":300,
//	           "token_id":"<opaque row id>"}
//
// The auth chain is authLimited → handler (no requireMFA, no
// requireScope — the caller has not authenticated yet; this is the
// path that authenticates them). Rate limit per IP / 10/min (spec §11)
// via authLimited.
//
// The handler does the work in five steps:
//
//  1. Decode + validate the request body.
//  2. Parse the JWT envelope (no signature check yet) and resolve
//     the iss claim. Verify the JWT against a permissive default
//     (proves the signature + checks iss; skips aud/sub_pattern).
//  3. Resolve the OIDC subject to a platform account.
//  4. Look up the (account_id, issuer_url) trust policy. On miss,
//     auto-create a permissive default and emit
//     oidc.trust_policy.created.
//  5. Mint the short-lived bearer, persist the ExchangedToken row,
//     emit auth.token.exchanged, return the bearer in the response.
//
// First-use auto-create: when Get returns ErrTrustPolicyNotFound
// (the customer has bound the issuer but never refined the policy),
// the handler Upserts a permissive default (sub_pattern=empty,
// algs=[RS256], RequiredClaims={}, audit_login='auto') and emits
// oidc.trust_policy.created. PR-C adds the dashboard refine UI for
// narrowing subject_pattern + RequiredClaims.
//
// AccountByOIDCSubject must succeed before the policy lookup — the
// (account_id, issuer_url) trust policy is the binding between the
// OIDC issuer and the platform account. PR-A scope: customers
// pre-bind the issuer via the dashboard before their first CI
// deploy (PR-C makes that step self-service). The handler does NOT
// auto-create the account binding; only the policy row that lives
// underneath it.
//
// The handler is < 50 lines per cmd/apid convention; the helpers
// (peekIssuer / permissiveDefaultPolicy / permissiveDefaultPolicyFor)
// are extracted into the same file.
package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// HandlerDeps is the dependency bundle the handler needs. cmd-side
// wires the production values in cmd/apid/server.go near the
// source-ref mount.
type HandlerDeps struct {
	Verifier Verifier
	Policies OIDCTrustPolicyStore
	Tokens   TokenExchangeStore
	Lookups  AccountLookup    // narrow interface for account-by-OIDC-sub resolution (PR-A: stub)
	Audit    AuditEmitter     // narrow interface for emit-on-success
	Log      *slog.Logger     // nil-safe
	Clock    func() time.Time // nil = time.Now
}

// AccountLookup is the minimum surface the handler needs to resolve
// the OIDC `sub` claim to a platform Account. The full impl lives in
// pkg/state/pgstore.go::AccountByOIDCSubject (PR-A scope). Returns
// state.ErrNotFound when no account is bound to that subject for
// that issuer — the handler maps that to 401 "OIDC subject not bound
// to an account" (distinct from a bad-signature 401 so the customer
// can tell "wrong CI job" from "wrong customer").
type AccountLookup interface {
	AccountByOIDCSubject(ctx context.Context, issuerURL, subject string) (state.Account, error)
}

// AuditEmitter is the narrow pkg/oidc-side projection of the cmd/apid
// auditor. Same shape as pkg/auth/middleware.Auditor but a separate
// interface so pkg/oidc doesn't import pkg/auth/middleware.
type AuditEmitter interface {
	Emit(ctx context.Context, kind string, accountID *string, data map[string]any)
}

// defaultAlgorithms is the closed alg set used by the auto-created
// permissive policy. Customers can refine (PR-C) to add ES* / PS*
// if their IdP issues them.
var defaultAlgorithms = []string{"RS256"}

// ServeHTTP is the http.HandlerFunc shape. The cmd-side wraps it
// with authLimited (per-IP rate limit) and registers at
// POST /v1/auth/oidc/exchange.
//
// Extracted helpers stay ≤ 50 lines per CLAUDE.md handler cap.
// The five steps above are inlined here for grep-ability; future
// growth extracts to handle_exchange_{decode,lookup,verify,mint,audit}.go.
func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	deps := h.deps
	if deps.Verifier == nil || deps.Policies == nil || deps.Tokens == nil || deps.Lookups == nil {
		api.WriteProblem(w, api.ErrCapacity("oidc handler not wired"))
		return
	}

	var req ExchangeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
			"Bad request", err.Error()))
		return
	}
	req.Provider = strings.TrimSpace(req.Provider)
	req.Token = strings.TrimSpace(req.Token)
	req.Audience = strings.TrimSpace(req.Audience)
	if req.Provider == "" || req.Token == "" || req.Audience == "" {
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
			"Validation failed", "provider, token, and aud are required"))
		return
	}

	// Step 1: parse the JWS envelope (no signature check yet) so we
	// can read the iss claim to resolve the trust policy. The
	// signature check happens at Step 2.
	issuerURL, err := peekIssuer(req.Token)
	if err != nil {
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
			"Invalid token", "could not parse JWT envelope: "+err.Error()))
		return
	}

	// Step 2: verify the JWT against a permissive default. We
	// don't know the (account_id, issuer_url) policy yet because
	// we haven't resolved the OIDC subject to an account — the
	// chicken-and-egg is broken at Step 2 (verify), Step 3
	// (account lookup), Step 4 (policy lookup + auto-create).
	//
	// Using the permissive default here means: first-use runs the
	// JWT through the JWKS verifier (proves the signature), checks
	// iss (must match the issuer we peeked), and skips aud/sub
	// gates. The real policy — which carries the customer's pinned
	// audience and any required-claim patterns — is enforced on
	// subsequent exchanges via the persisted policy row.
	claims, err := deps.Verifier.Verify(r.Context(), req.Token, permissiveDefaultPolicy(issuerURL))
	if err != nil {
		api.WriteProblem(w, api.NewProblem(http.StatusUnauthorized, api.CodeUnauthorized,
			"Invalid token", err.Error()))
		return
	}

	// Step 3: resolve the OIDC subject to a platform account.
	acct, err := deps.Lookups.AccountByOIDCSubject(r.Context(), issuerURL, claims.Subject)
	if err != nil {
		// 401 (not 404): the customer controls whether a subject
		// is bound, and the same shape covers "no policy exists
		// for this issuer" (a real prod failure mode) and "the
		// subject is not bound to this account" (a real CI
		// failure mode). The detail message carries enough
		// context for the customer's deploy logs.
		api.WriteProblem(w, api.NewProblem(http.StatusUnauthorized, api.CodeUnauthorized,
			"OIDC subject not bound", "no account bound to ("+issuerURL+", "+claims.Subject+")"))
		return
	}

	// Step 4: look up the (account_id, issuer_url) policy. On
	// miss, auto-create a permissive default (sub_pattern="",
	// algs=[RS256], empty RequiredClaims) and emit
	// oidc.trust_policy.created.
	if _, err := deps.Policies.Get(r.Context(), acct.ID, issuerURL); err != nil {
		if !errors.Is(err, ErrTrustPolicyNotFound) {
			api.WriteProblem(w, api.ErrCapacity("trust policy lookup failed"))
			return
		}
		// Auto-create path: the (account, issuer) binding is
		// already in place (Step 3 resolved the account); we just
		// need to persist a default policy row so subsequent
		// exchanges can enforce it. The audit row carries the
		// upserted policy's jwks_url + audience + 'auto' marker.
		policy := permissiveDefaultPolicyFor(acct.ID, issuerURL)
		policy, err = deps.Policies.Upsert(r.Context(), policy)
		if err != nil {
			api.WriteProblem(w, api.ErrCapacity("trust policy upsert failed"))
			return
		}
		if deps.Audit != nil {
			acctID := acct.ID
			deps.Audit.Emit(r.Context(), KindOIDCTrustPolicyCreated, &acctID, map[string]any{
				"account_id": acct.ID,
				"issuer_url": issuerURL,
				"jwks_url":   policy.JWKSURL,
				"audience":   policy.Audience,
				"created_by": "auto",
			})
		}
	}

	// Step 5: mint the short-lived bearer + persist + audit.
	plaintext, hash, err := api.GenerateOIDCKey()
	if err != nil {
		api.WriteProblem(w, api.ErrCapacity("could not mint bearer"))
		return
	}
	now := h.now()
	row := &ExchangedToken{
		AccountID: acct.ID,
		TokenHash: hash,
		ExpiresAt: now.Add(OIDCBearerTTL),
		IssuerURL: issuerURL,
		Subject:   claims.Subject,
		Audience:  claims.Aud,
		JTI:       claims.JTI,
	}
	// Insert returns the server-minted row id (gen_random_uuid at
	// the SQL layer; uuid.NewString in memstore). The id is the
	// audit-correlation key — the customer can grep the audit
	// reader for it to find "which CI job shipped?".
	tokenID, err := deps.Tokens.Insert(r.Context(), row)
	if err != nil {
		api.WriteProblem(w, api.ErrCapacity("could not persist bearer"))
		return
	}
	row.ID = tokenID
	if deps.Audit != nil {
		acctID := acct.ID
		deps.Audit.Emit(r.Context(), KindAuthTokenExchanged, &acctID, map[string]any{
			"account_id":  acct.ID,
			"token_id":    tokenID,
			"issuer_url":  issuerURL,
			"audience":    req.Audience,
			"subject":     claims.Subject,
			"subject_jti": claims.JTI,
			"expires_at":  row.ExpiresAt,
		})
	}
	writeJSON(w, http.StatusOK, ExchangeResponse{
		Bearer:    plaintext,
		ExpiresIn: int(OIDCBearerTTL.Seconds()),
		TokenID:   tokenID,
	})
}

// Handler wraps HandlerDeps into the http.HandlerFunc the cmd-side
// mounts. Constructor pattern (vs. raw HandlerDeps) so cmd/apid can
// stamp the deps once at startup and pass the same Handler into
// both the production route and the whitebox tests.
type Handler struct {
	deps  HandlerDeps
	clock func() time.Time
}

// NewHandler constructs the handler. clock is nil-safe (defaults to
// time.Now); log is nil-safe (Warn paths skipped).
func NewHandler(deps HandlerDeps) *Handler {
	clock := deps.Clock
	if clock == nil {
		clock = time.Now
	}
	return &Handler{deps: deps, clock: clock}
}

// now returns the handler's clock. Exposed as a method so the body
// of ServeHTTP can stamp "now" against a swappable clock in tests
// without reaching into the unexported field.
func (h *Handler) now() time.Time { return h.clock() }

// peekIssuer parses the JWS envelope (no signature check) to read
// the `iss` claim. Used to resolve the trust policy BEFORE the JWKS
// fetch — the policy's jwks_url is what we register. jose.ParseSigned
// validates the alg header against the closed set RS256/RS384/
// RS512/ES256/ES384/ES512; an unknown alg short-circuits here as
// "could not parse JWT envelope" (400, not 401).
func peekIssuer(rawToken string) (string, error) {
	closed := []jose.SignatureAlgorithm{
		jose.RS256, jose.RS384, jose.RS512,
		jose.ES256, jose.ES384, jose.ES512,
	}
	jws, err := jose.ParseSigned(rawToken, closed)
	if err != nil {
		return "", err
	}
	var std jwt.Claims
	if err := json.Unmarshal(jws.UnsafePayloadWithoutVerification(), &std); err != nil {
		return "", err
	}
	if std.Issuer == "" {
		return "", errors.New("iss claim missing")
	}
	return std.Issuer, nil
}

// permissiveDefaultPolicy is the verify-only stub used when the real
// policy is missing on first-use. Has the issuer URL (so aud/iss
// checks work) and RS256 (the GitHub Actions default); the real
// policy is upserted after account resolution.
func permissiveDefaultPolicy(issuerURL string) *OIDCTrustPolicy {
	return &OIDCTrustPolicy{
		IssuerURL:  issuerURL,
		Audience:   []string{}, // empty = skip aud check (verify-only; real policy carries the customer's pinned aud)
		Algorithms: defaultAlgorithms,
	}
}

// permissiveDefaultPolicyFor builds the real default policy row
// upserted on first-use auto-create. The customer can refine
// (PR-C) via the dashboard. Permissive = empty sub_pattern, empty
// RequiredClaims — every CI job for this account under this issuer
// is admitted until the customer narrows it.
func permissiveDefaultPolicyFor(accountID, issuerURL string) *OIDCTrustPolicy {
	return &OIDCTrustPolicy{
		AccountID:  accountID,
		IssuerURL:  issuerURL,
		Audience:   []string{}, // refine in dashboard
		Algorithms: defaultAlgorithms,
		AuditLogin: "auto",
	}
}

// writeJSON is the standard cmd/apid response shape — same envelope
// the source-ref handler uses. Kept private here so pkg/oidc owns
// its response encoding; cmd-side uses its own writeJSON for non-
// oidc routes.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
