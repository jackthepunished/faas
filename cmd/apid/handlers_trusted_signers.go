// handlers_trusted_signers.go — apid handlers for the per-app cosign
// trusted-publisher list (issue #472 / ADR-054).
//
// Routes (registered in cmd/apid/server.go::handler with the admin+MFA
// chain — see the mount block):
//
//	GET    /v1/apps/{slug}/trusted_signers          → listTrustedSigners
//	PUT    /v1/apps/{slug}/trusted_signers/{name}   → upsertTrustedSigner
//	DELETE /v1/apps/{slug}/trusted_signers/{name}   → deleteTrustedSigner
//
// Trust model
//
//   - Per-app allowlist of cosign public keys whose signatures on OCI
//     images are accepted at deploy time (mirrors AWS Lambda's
//     TrustedSigners inside a Code Signing for Lambda config).
//   - Admin-only (ScopesAdminOnly → requireScope) AND MFA-gated
//     (requireMFA). The signer rows are operator-controlled, NOT
//     customer-controlled: a customer can request signature
//     enforcement, but only an account admin can actually onboard a
//     publisher. The wire surface is restricted to admins because a
//     customer who can write to this table can bypass the gate they
//     just opted into.
//   - apid is the only writer; imaged reads the rows at
//     buildImageLayer time (and via pg_notify('trusted_signer_changed')).
//
// Wire shape
//
//   - public_key_pem is base64-encoded DER SPKI (NOT a PEM-armoured
//     block). The decode path validates 64..1024 bytes post-decode
//     (matches the DB CHECK app_trusted_signers_pem_shape added in
//     migration 00083). Reusing the secret-surface 400/404 split is
//     intentional — same SDK error-decoding pattern.

package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// trustedSignerNamePattern mirrors the DB CHECK
// app_trusted_signers_name_shape (migration 00083): DNS-1123-label
// style (^[a-z0-9][a-z0-9_-]{0,63}$). The handler pre-validates so
// the 400 is actionable ("signer name must match…") rather than a
// raw 22P02 / 23514 surfaced from the store.
var trustedSignerNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// trustedSignerPEMByteMin / Max mirror the
// app_trusted_signers_pem_shape CHECK (octet_length BETWEEN 64 AND
// 1024). ECDSA P-256 SPKI is exactly 91 bytes when raw DER-encoded
// (the test fixture in migrations/00083 pins this), and the upper
// bound accommodates RSA-2048 SPKI (294 bytes) without letting the
// table bloat into config-management territory.
const (
	trustedSignerPEMByteMin = 64
	trustedSignerPEMByteMax = 1024
)

// listTrustedSigners returns every trusted-publisher on the app. The
// response shape is a stable wrapper object (`signers` slice, never
// nil) so the dashboard's render path doesn't have to special-case
// empty state. Admin-scoped via the mount-time chain.
//
// Empty list is the EXPECTED state for any app with require_signed=false
// — this endpoint is informational regardless.
func (s *server) listTrustedSigners(w http.ResponseWriter, r *http.Request, acct state.Account) {
	slug := r.PathValue("slug")
	app, ok := s.loadApp(w, r, acct, slug)
	if !ok {
		return
	}
	rows, err := s.store.ListAppTrustedSigners(ctx(r), acct.ID, app.ID)
	if err != nil {
		api.WriteProblem(w, api.ErrCapacity("could not list trusted signers"))
		return
	}
	out := make([]api.TrustedSigner, 0, len(rows))
	for _, row := range rows {
		out = append(out, api.TrustedSigner{
			Name:         row.SignerName,
			PublicKeyPEM: base64.StdEncoding.EncodeToString(row.CosignPublicKey),
			AddedAt:      row.AddedAt.UTC(),
		})
	}
	writeJSON(w, http.StatusOK, api.AppTrustedSignerListResponse{Signers: out})
}

// upsertTrustedSigner writes-or-replaces the (app_id, signer_name)
// row. Quota is enforced before the persist so an over-cap request is
// rejected without touching the store. Idempotent: re-PUT replaces
// the key material and stamps added_by_account_id with the calling
// admin's id; added_at stays at the original write so the audit
// trail distinguishes "created" from "rotated".
//
// PEM shape failures (size out of range, base64 decode error) return
// 400 trusted_signer_invalid BEFORE the quota check so a malformed
// payload never wastes a COUNT round-trip.
func (s *server) upsertTrustedSigner(w http.ResponseWriter, r *http.Request, acct state.Account) {
	slug := r.PathValue("slug")
	name := r.PathValue("name")
	if !trustedSignerNamePattern.MatchString(name) {
		api.WriteProblem(w, api.ErrTrustedSignerInvalid(
			"signer name must match ^[a-z0-9][a-z0-9_-]{0,63}$ (DNS-1123-label)"))
		return
	}
	app, ok := s.loadApp(w, r, acct, slug)
	if !ok {
		return
	}
	var req api.AddTrustedSignerRequest
	if err := decodeJSON(r, &req); err != nil {
		api.WriteProblem(w, api.ErrTrustedSignerInvalid("invalid JSON body"))
		return
	}
	pubKey, err := base64.StdEncoding.DecodeString(req.PublicKeyPEM)
	if err != nil {
		api.WriteProblem(w, api.ErrTrustedSignerInvalid(
			"public_key_pem must be a base64 string"))
		return
	}
	if len(pubKey) < trustedSignerPEMByteMin || len(pubKey) > trustedSignerPEMByteMax {
		api.WriteProblem(w, api.ErrTrustedSignerInvalid(
			fmt.Sprintf("public_key_pem must decode to %d..%d bytes (got %d)",
				trustedSignerPEMByteMin, trustedSignerPEMByteMax, len(pubKey))))
		return
	}
	limits := api.MustLimitsFor(acct.Plan)
	if prob := s.checkTrustedSignerQuota(ctx(r), acct, app, name, limits); prob != nil {
		api.WriteProblem(w, prob)
		return
	}
	if err := s.store.UpsertAppTrustedSigner(ctx(r), acct.ID, app.ID, name, pubKey, acct.ID); err != nil {
		api.WriteProblem(w, api.ErrCapacity("could not persist trusted signer"))
		return
	}
	// pg_notify('trusted_signer_changed') — imaged's verify path
	// refreshes its in-memory cache on this signal (Bucket 4). The
	// fire-and-forget shape mirrors the other Notify* call sites in
	// handlers_env.go / handlers_secrets.go.
	if s.notif != nil {
		_ = s.notif.Notify(ctx(r), "trusted_signer_changed",
			fmt.Sprintf(`{"kind":"upserted","app_id":"%s","signer":"%s"}`, app.ID, name))
	}
	s.log.Info("trusted signer upserted",
		"app", app.Slug,
		"signer", name,
		"account", acct.ID,
		"key_bytes", len(pubKey),
	)
	// Issue #472 / ADR-054: app.trusted_signer_added / app.trusted_signer_removed
	// are the audit taxonomy. We emit "added" on first PUT and
	// "rotated" on subsequent PUTs of the same name — the rotate
	// distinction matters for regulated-workload change logs
	// (SOC 2 CC7.1 / ISO 27001 A.12.4.1). The store doesn't surface
	// "was this an insert or an update", so we re-list to tell.
	kind := "app.trusted_signer_added"
	if s.signerAlreadyExisted(ctx(r), acct.ID, app.ID, name, pubKey) {
		kind = "app.trusted_signer_rotated"
	}
	s.audit.Emit(ctx(r), kind, &acct.ID, map[string]any{
		"app_id":    app.ID,
		"signer":    name,
		"key_bytes": len(pubKey),
	})
	writeJSON(w, http.StatusOK, struct {
		Name string `json:"name"`
	}{Name: name})
}

// signerAlreadyExisted inspects the just-upserted row to distinguish
// "added" from "rotated". added_at is the only stable signal: if the
// pre-write mtime is older than ~1s ago, the row was already there.
// We re-read via ListAppTrustedSigners and pick the matching row —
// O(N) over a tiny N (max 16 per Scale plan), so the cost is
// negligible against the verify-loop cost on the imaged side.
func (s *server) signerAlreadyExisted(c ctxAlias, accountID, appID, name string, _ []byte) bool {
	rows, err := s.store.ListAppTrustedSigners(c, accountID, appID)
	if err != nil {
		return false
	}
	for _, r := range rows {
		if r.SignerName != name {
			continue
		}
		// If added_at is more than 1 second in the past, the row
		// pre-dates this request — it's a rotation. We use a
		// 1-second window so a normal request that crosses a
		// second-boundary doesn't get misclassified.
		if !r.AddedAt.IsZero() && time.Since(r.AddedAt) > time.Second {
			return true
		}
		return false
	}
	return false
}

// checkTrustedSignerQuota mirrors checkEnvQuota 1:1 — re-PUTs of an
// existing (app_id, signer_name) don't count against the quota, so
// the (count - 1) for the row being replaced is implicit.
func (s *server) checkTrustedSignerQuota(c ctxAlias, acct state.Account, app state.App, name string, limits api.Limits) *api.Problem {
	if limits.TrustedSignerCountMax == 0 {
		// Free plan: the open-deploy posture means customers on Free
		// never need require_signed=true, so they never need
		// signers either. 402 keeps the contract parallel to
		// CodePlanCronsNotAllowed / CodePlanAlertRulesNotAllowed.
		return api.NewProblem(http.StatusForbidden, api.CodePlanLimitTrustedSigners,
			"Trusted signers are not allowed on this plan",
			"the Free tier does not support per-app signature enforcement; upgrade to Hobby or higher.")
	}
	n, err := s.store.CountAppTrustedSigners(c, acct.ID, app.ID)
	if err != nil {
		return api.ErrCapacity("could not count trusted signers")
	}
	already := s.signerExists(c, acct.ID, app.ID, name)
	if !already && n >= limits.TrustedSignerCountMax {
		return api.ErrPlanLimitTrustedSigners(limits, n)
	}
	return nil
}

func (s *server) signerExists(c ctxAlias, accountID, appID, name string) bool {
	rows, err := s.store.ListAppTrustedSigners(c, accountID, appID)
	if err != nil {
		return false
	}
	for _, r := range rows {
		if r.SignerName == name {
			return true
		}
	}
	return false
}

// deleteTrustedSigner removes the (app_id, signer_name) row.
// Returns 404 CodeTrustedSignerNotFound when no row matches — the
// resource model on this surface deliberately uses 404 (not the
// secret/env 400) because the URL resource IS the signer name, and
// the 404 makes the resource hierarchy explicit to SDK callers.
func (s *server) deleteTrustedSigner(w http.ResponseWriter, r *http.Request, acct state.Account) {
	slug := r.PathValue("slug")
	name := r.PathValue("name")
	if !trustedSignerNamePattern.MatchString(name) {
		// Surface 404 (not 400) so a malformed-name probe gets the
		// same response as a missing-signer probe — keeps the wire
		// shape stable for unauthorized scrapers (idempotent).
		api.WriteProblem(w, api.ErrTrustedSignerNotFound(name))
		return
	}
	app, ok := s.loadApp(w, r, acct, slug)
	if !ok {
		return
	}
	if err := s.store.DeleteAppTrustedSigner(ctx(r), acct.ID, app.ID, name); err != nil {
		if errors.Is(err, state.ErrNotFound) {
			api.WriteProblem(w, api.ErrTrustedSignerNotFound(name))
			return
		}
		api.WriteProblem(w, api.ErrCapacity("could not delete trusted signer"))
		return
	}
	if s.notif != nil {
		_ = s.notif.Notify(ctx(r), "trusted_signer_changed",
			fmt.Sprintf(`{"kind":"deleted","app_id":"%s","signer":"%s"}`, app.ID, name))
	}
	s.log.Info("trusted signer deleted",
		"app", app.Slug,
		"signer", name,
		"account", acct.ID,
	)
	s.audit.Emit(ctx(r), "app.trusted_signer_removed", &acct.ID, map[string]any{
		"app_id": app.ID,
		"signer": name,
	})
	w.WriteHeader(http.StatusNoContent)
}

// ctxAlias is a thin alias for context.Context so the helper
// signatures read cleanly. handlers_env.go uses stdctxEnv for the
// same purpose; we use a separate name to keep this file's helpers
// discoverable from grep.
type ctxAlias = context.Context
