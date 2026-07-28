// handlers_env.go — apid handlers for customer env vars
// (issue #395 / ADR-045).
//
// Routes (registered in server.go::handler):
//
//	GET    /v1/apps/{slug}/env          → listEnv
//	PUT    /v1/apps/{slug}/env/{key}    → setEnv
//	DELETE /v1/apps/{slug}/env/{key}    → deleteEnv
//
// Trust model
//
//   - Plaintext VALUES arrive over TLS via PUT body and live transiently
//     in this handler. There is NO seal step — values land in app_envs as
//     plaintext TEXT, by contract (issue #395 plaintext rationale +
//     ADR-045 §Decision). The values are non-sensitive runtime config
//     (LOG_LEVEL, FEATURE_X, ...); credentials stay on /secrets.
//   - No log line ever contains the plaintext VALUE. Key names are public
//     per spec §11 and flow freely.
//   - Scopes: GET requires env:read (or admin); PUT/DELETE require
//     env:write (or admin). env:write is NOT MFA-gated because env vars
//     are explicitly non-sensitive (ADR-045 §Decision — see the
//     credentials-only MFA rationale in handlers_secrets.go).

package main

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/logsanitize"
	"github.com/onebox-faas/faas/pkg/state"
)

// stdctxEnv alias mirrors handlers_secrets.go so the file is self-
// contained and the helper signatures read cleanly.
type stdctxEnv = context.Context

// listEnv returns every env var on the app, key + timestamps only — the
// VALUE never appears in the GET response (guest-init reads the value
// from /etc/faas/env.json at process start). Quota info is included so
// the CLI can show "3/8 env vars" without a separate call.
func (s *server) listEnv(w http.ResponseWriter, r *http.Request, acct state.Account) {
	slug := r.PathValue("slug")
	app, ok := s.loadApp(w, r, acct, slug)
	if !ok {
		return
	}
	rows, err := s.store.ListAppEnv(ctx(r), acct.ID, app.ID)
	if err != nil {
		api.WriteProblem(w, api.ErrCapacity("could not list env vars"))
		return
	}
	limits := api.MustLimitsFor(acct.Plan)
	out := make([]api.AppEnvResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, api.AppEnvResponse{
			Key:       row.Key,
			CreatedAt: row.CreatedAt.UTC().Format(time.RFC3339),
			UpdatedAt: row.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	// Wrap with quota metadata so the CLI can render a progress bar.
	writeJSON(w, http.StatusOK, struct {
		Env   []api.AppEnvResponse `json:"env"`
		Quota int                  `json:"quota_max"`
		Count int                  `json:"count"`
	}{Env: out, Quota: limits.EnvVarsMax, Count: len(out)})
}

// setEnv upserts the (app_id, key) row with the plaintext VALUE. Quota
// is enforced before the persist so an over-cap request is rejected
// without touching the store. Idempotent: re-PUT replaces value +
// bumps updated_at.
//
// Hand-rolled phases (validate key → resolve app → validate body →
// check quota → persist → log → audit), not a helper, because the line
// budget is well under the §Conventions 50-line cap and the phase
// order matters for auditing.
func (s *server) setEnv(w http.ResponseWriter, r *http.Request, acct state.Account) {
	slug := r.PathValue("slug")
	key := r.PathValue("key")
	if prob := api.ValidateEnvKey(key); prob != nil {
		api.WriteProblem(w, prob)
		return
	}
	app, ok := s.loadApp(w, r, acct, slug)
	if !ok {
		return
	}
	var req api.PutAppEnvRequest
	if err := decodeJSON(r, &req); err != nil {
		api.WriteProblem(w, api.ErrValidation("invalid JSON body"))
		return
	}
	limits := api.MustLimitsFor(acct.Plan)
	if prob := req.Validate(limits.EnvValueMaxBytes); prob != nil {
		api.WriteProblem(w, prob)
		return
	}
	if prob := s.checkEnvQuota(ctx(r), acct, app, key, limits); prob != nil {
		api.WriteProblem(w, prob)
		return
	}
	if err := s.store.UpsertAppEnv(ctx(r), acct.ID, app.ID, key, req.Value); err != nil {
		api.WriteProblem(w, api.ErrCapacity("could not persist env var"))
		return
	}
	// Audit + log. VALUE never reaches slog. logsanitize.RedactValue is
	// used defensively even though we never log req.Value directly — a
	// future refactor that adds a "request echo" log line won't leak.
	s.log.Info("env set",
		"app", app.Slug,
		"key", logsanitize.Field(key),
		"account", acct.ID,
		"value_bytes", logsanitize.RedactValue(req.Value),
	)
	// Issue #395: env.set is distinct from secret.set in the audit
	// taxonomy so the secret-quota bypass argument is closed — a
	// customer can't hide credentials under the env var surface
	// without losing the audit-kind signal that says "config change"
	// vs "credential change". Same posture as slog: no plaintext.
	s.audit.Emit(ctx(r), "env.set", &acct.ID, map[string]any{
		"app_id": app.ID,
		"name":   key,
	})
	writeJSON(w, http.StatusOK, struct {
		Key string `json:"key"`
	}{Key: key})
}

// checkEnvQuota returns nil when a PUT for (app, key) is allowed under
// the per-plan EnvVarsMax, or a ready-to-write *api.Problem otherwise.
// Re-PUTs of an existing key are not new rows and so don't count against
// the quota — the (count - 1) for the row being replaced is implicit.
//
// Shape mirrors checkSecretQuota exactly so a future refactor that drops
// one or the other (e.g. special-casing env vs secret) trips this seam.
func (s *server) checkEnvQuota(c stdctxEnv, acct state.Account, app state.App, key string, limits api.Limits) *api.Problem {
	n, err := s.store.CountAppEnv(c, acct.ID, app.ID)
	if err != nil {
		return api.ErrCapacity("could not count env vars")
	}
	already, err := s.envExists(c, acct.ID, app.ID, key)
	if err != nil {
		return api.ErrCapacity("could not check env var")
	}
	if !already && n >= limits.EnvVarsMax {
		return api.ErrPlanLimitEnvVars(limits, n)
	}
	return nil
}

// deleteEnv removes the (app_id, key) row. Two distinct failure
// modes map to two distinct status codes — both are documented on
// the OpenAPI DELETE shape (api/openapi.yaml:1509-1513):
//
//   - loadApp misses        → 404 NotFound (the *app* doesn't exist
//     or isn't owned by the caller; loadApp owns this contract)
//   - DeleteAppEnv misses   → 400 CodeEnvVarNotFound (the env var
//     key isn't set on an existing app; the URL resource IS the
//     env var, not the app)
//
// The 400/404 split mirrors the secrets DELETE surface (handlers_secrets.go
// + api/openapi.yaml:1429-1438) so SDK callers reuse the same error-
// decoding branch. The DELETE is idempotent on the row side: a re-
// DELETE of a just-removed row returns 400 env_var_not_found, not
// 404 — by design.
func (s *server) deleteEnv(w http.ResponseWriter, r *http.Request, acct state.Account) {
	slug := r.PathValue("slug")
	key := r.PathValue("key")
	if prob := api.ValidateEnvKey(key); prob != nil {
		api.WriteProblem(w, prob)
		return
	}
	app, ok := s.loadApp(w, r, acct, slug)
	if !ok {
		return
	}
	if err := s.store.DeleteAppEnv(ctx(r), acct.ID, app.ID, key); err != nil {
		if errors.Is(err, state.ErrNotFound) {
			api.WriteProblem(w, api.ErrEnvVarNotFound(key))
			return
		}
		api.WriteProblem(w, api.ErrCapacity("could not delete env var"))
		return
	}
	s.log.Info("env deleted",
		"app", app.Slug,
		"key", logsanitize.Field(key),
		"account", acct.ID,
	)
	// Issue #395: env.deleted is the DELETE counterpart of env.set.
	s.audit.Emit(ctx(r), "env.deleted", &acct.ID, map[string]any{
		"app_id": app.ID,
		"name":   key,
	})
	w.WriteHeader(http.StatusNoContent)
}

// envExists checks if a (app_id, key) row exists for the account. Used
// by setEnv to subtract 1 from the quota count when an upsert is
// replacing an existing row. Mirrors secretExists — see that helper's
// comment for the O(n) rationale and the Store-surface decision.
func (s *server) envExists(c stdctxEnv, accountID, appID, key string) (bool, error) {
	rows, err := s.store.ListAppEnv(c, accountID, appID)
	if err != nil {
		return false, err
	}
	for _, r := range rows {
		if r.Key == key {
			return true, nil
		}
	}
	return false, nil
}
