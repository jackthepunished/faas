// handlers_env.go — apid handlers for customer env vars
// (issue #395 / ADR-045 / ADR-090).
//
// Routes (registered in server.go::handler):
//
//	GET    /v1/apps/{slug}/env          → listEnv
//	PUT    /v1/apps/{slug}/env/{key}    → setEnv
//	DELETE /v1/apps/{slug}/env/{key}    → deleteEnv
//
// All three routes accept an optional `?scope=` query param
// (ADR-090 D2 / PR-B). The scope is a domain-valid slug
// (3..40 lowercase alnum + dash, see api.EnvScopePattern) or
// the reserved sentinel `__all__` on GET only. Omitted `?scope=`
// means `scope=default` — the wire shape is byte-identical to
// pre-PR-B. See api.ValidateScope for the rejection rules.
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
	"sort"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/logsanitize"
	"github.com/onebox-faas/faas/pkg/state"
)

// stdctxEnv alias mirrors handlers_secrets.go so the file is self-
// contained and the helper signatures read cleanly.
type stdctxEnv = context.Context

// defaultEnvScope is the scope name stamped on rows that pre-date
// ADR-090 (created before migration 00203 added the `scope` column)
// and on writes that omit `?scope=`. The constant is reused at
// every read/write call site so a future rename (e.g. to "prod") is
// a one-line change. See ADR-090 D1 — pre-00203 rows get
// scope='default' via the PG11+ fast-default backfill in the
// migration's `NOT NULL DEFAULT 'default'` clause.
const defaultEnvScope = "default"

// scopeFromQuery resolves the optional `?scope=` query param. Empty
// means "use the default scope" — the same wire shape pre-PR-B
// customers rely on. The reserved sentinel "__all__" is allowed on
// GET only (rejected as ErrEnvScopeReserved on PUT/DELETE); any
// other malformed value is rejected as ErrEnvScopeInvalid. Returns
// (scope, isAll, problem) so listEnv can branch on isAll without
// re-parsing the query string.
//
// PutEnvScopeQueryParam is a stable identifier; the gregale CLI
// and the SDK generator hard-code it. If you rename it, also update
// the `?scope=` references in api/openapi.yaml and the SDK.
const scopeQueryParam = "scope"

func scopeFromQuery(r *http.Request, allowAll bool) (scope string, isAll bool, prob *api.Problem) {
	raw := r.URL.Query().Get(scopeQueryParam)
	if raw == "" {
		return defaultEnvScope, false, nil
	}
	if raw == api.EnvScopeAllSentinel {
		if !allowAll {
			return "", false, api.ErrEnvScopeReserved(api.EnvScopeAllSentinel)
		}
		return "", true, nil
	}
	if prob := api.ValidateScope(raw); prob != nil {
		return "", false, prob
	}
	return raw, false, nil
}

// listEnv returns every env var on the app in the requested scope.
// The VALUE never appears in the GET response (guest-init reads the
// value from /etc/faas/env.json at process start). Quota info is
// included so the CLI can show "3/8 env vars" without a separate
// call.
//
// `?scope=__all__` returns a nested `env_by_scope` map shape
// (ADR-090 D3); the flat `env` array is empty. All other scopes
// return the flat `env` array as before. The count + quota fields
// count across ALL scopes (the per-app EnvVarsMax is unchanged
// across scopes per ADR-090 D6) so the CLI can render a unified
// progress bar.
func (s *server) listEnv(w http.ResponseWriter, r *http.Request, acct state.Account) {
	slug := r.PathValue("slug")
	app, ok := s.loadApp(w, r, acct, slug)
	if !ok {
		return
	}
	scope, isAll, prob := scopeFromQuery(r, true /* allowAll */)
	if prob != nil {
		api.WriteProblem(w, prob)
		return
	}
	limits := api.MustLimitsFor(acct.Plan)

	if isAll {
		// `?scope=__all__` returns the nested map shape. Read every
		// row on the app via the scope-agnostic store path. We do
		// NOT introduce a new State method for this — the
		// all-scope read is rare (operator-only) and the in-memory
		// sort is O(n log n) on the per-app env count (capped at
		// Limits.EnvVarsMax).
		rows, err := s.listAllEnvForApp(r.Context(), acct.ID, app.ID)
		if err != nil {
			api.WriteProblem(w, api.ErrCapacity("could not list env vars"))
			return
		}
		writeEnvListAll(w, rows, limits.EnvVarsMax)
		return
	}

	rows, err := s.store.ListAppEnvInScope(r.Context(), acct.ID, app.ID, scope)
	if err != nil {
		api.WriteProblem(w, api.ErrCapacity("could not list env vars"))
		return
	}
	out := make([]api.AppEnvResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, api.AppEnvResponse{
			Key:       row.Key,
			CreatedAt: row.CreatedAt.UTC().Format(time.RFC3339),
			UpdatedAt: row.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	// Count across ALL scopes so the dashboard renders one unified
	// "N / EnvVarsMax" bar regardless of which scope the customer
	// is currently inspecting. Per-ADR-090 D6 the per-app quota is
	// unchanged across scopes — a customer's "staging" rows count
	// toward the same EnvVarsMax as their "default" rows.
	totalCount, err := s.store.CountAppEnv(r.Context(), acct.ID, app.ID)
	if err != nil {
		api.WriteProblem(w, api.ErrCapacity("could not count env vars"))
		return
	}
	writeJSON(w, http.StatusOK, api.AppEnvListResponse{
		Env:   out,
		Quota: limits.EnvVarsMax,
		Count: totalCount,
	})
}

// listAllEnvForApp returns every env row on the app across all
// scopes, sorted by (scope ASC, key ASC). Used by listEnv's
// `?scope=__all__` arm. Delegates to store.ListAllAppEnv so the
// production pgstore + the test MemStore agree on the wire shape.
// The per-app row count is bounded by Limits.EnvVarsMax (8..2000 by
// plan) and the call is rare (operator-only), so the read is
// cheap.
func (s *server) listAllEnvForApp(c stdctxEnv, accountID, appID string) ([]state.AppEnv, error) {
	rows, err := s.store.ListAllAppEnv(c, accountID, appID)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// writeEnvListAll renders the nested `env_by_scope` response shape
// for `?scope=__all__`. The flat `env` array is empty; the map
// keys are scope names. Rows are grouped by scope and the per-scope
// slice is sorted by key ASC to match the flat response. Count is
// the row total (cross-scope); Quota is unchanged.
func writeEnvListAll(w http.ResponseWriter, rows []state.AppEnv, quota int) {
	// Bucket rows by scope. Deterministic ordering: scope ASC, then
	// per-scope key ASC — matches the flat response's key ASC for
	// each scope, and the per-scope list in the nested response.
	bucket := map[string][]api.ScopedAppEnvResponse{}
	for _, r := range rows {
		bucket[r.Scope] = append(bucket[r.Scope], api.ScopedAppEnvResponse{
			Scope:     r.Scope,
			Key:       r.Key,
			CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339),
			UpdatedAt: r.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	for scope := range bucket {
		sort.Slice(bucket[scope], func(i, j int) bool {
			return bucket[scope][i].Key < bucket[scope][j].Key
		})
	}
	// Render the map in scope-ASC order so the wire output is
	// stable across calls (Go's map iteration is randomized).
	scopes := make([]string, 0, len(bucket))
	for scope := range bucket {
		scopes = append(scopes, scope)
	}
	sort.Strings(scopes)
	ordered := make(api.EnvByScope, len(bucket))
	for _, scope := range scopes {
		ordered[scope] = bucket[scope]
	}
	writeJSON(w, http.StatusOK, api.AppEnvListResponse{
		Env:        nil, // discriminated union: empty in the __all__ arm
		EnvByScope: ordered,
		Quota:      quota,
		Count:      len(rows),
	})
}

// setEnv upserts the (app_id, scope, key) row with the plaintext
// VALUE. Quota is enforced before the persist so an over-cap
// request is rejected without touching the store. Idempotent:
// re-PUT replaces value + bumps updated_at.
//
// The `?scope=` query param selects which scope to write into; the
// reserved sentinel `__all__` is rejected (400 env_scope_reserved)
// because it has no meaning on a single-row write. Omitted scope
// means `scope=default` — pre-PR-B callers see no behaviour change.
//
// Hand-rolled phases (validate key → resolve app → validate body →
// validate scope → check quota → persist → log → audit), not a
// helper, because the line budget is well under the §Conventions
// 50-line cap and the phase order matters for auditing.
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
	scope, _, prob := scopeFromQuery(r, false /* allowAll */)
	if prob != nil {
		api.WriteProblem(w, prob)
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
	if prob := s.checkEnvQuota(r.Context(), acct, app, scope, key, limits); prob != nil {
		api.WriteProblem(w, prob)
		return
	}
	if err := s.store.UpsertAppEnvInScope(r.Context(), acct.ID, app.ID, scope, key, req.Value); err != nil {
		api.WriteProblem(w, api.ErrCapacity("could not persist env var"))
		return
	}
	// Audit + log. VALUE never reaches slog. logsanitize.RedactValue is
	// used defensively even though we never log req.Value directly — a
	// future refactor that adds a "request echo" log line won't leak.
	s.log.Info("env set",
		"app", app.Slug,
		"scope", logsanitize.Field(scope),
		"key", logsanitize.Field(key),
		"account", acct.ID,
		"value_bytes", logsanitize.RedactValue(req.Value),
	)
	// Issue #395: env.set is distinct from secret.set in the audit
	// taxonomy so the secret-quota bypass argument is closed — a
	// customer can't hide credentials under the env var surface
	// without losing the audit-kind signal that says "config change"
	// vs "credential change". Same posture as slog: no plaintext.
	//
	// ADR-090 PR-B: the audit payload widens to include `scope` so
	// downstream consumers (SIEM, dashboard filter) can attribute
	// the change to a specific environment. `scope="default"` is
	// the pre-PR-B shape byte-for-byte — this is purely additive.
	s.audit.Emit(r.Context(), "env.set", &acct.ID, map[string]any{
		"app_id": app.ID,
		"scope":  scope,
		"name":   key,
	})
	writeJSON(w, http.StatusOK, struct {
		Key   string `json:"key"`
		Scope string `json:"scope"`
	}{Key: key, Scope: scope})
}

// checkEnvQuota returns nil when a PUT for (app, scope, key) is
// allowed under the per-plan EnvVarsMax, or a ready-to-write
// *api.Problem otherwise. Re-PUTs of an existing (scope, key) are
// not new rows and so don't count against the quota — the
// (count - 1) for the row being replaced is implicit.
//
// Scope is part of the quota path: a customer's "staging" rows
// count toward the same EnvVarsMax as their "default" rows per
// ADR-090 D6. The quota is unchanged across scopes.
//
// Shape mirrors checkSecretQuota exactly so a future refactor that
// drops one or the other (e.g. special-casing env vs secret) trips
// this seam.
func (s *server) checkEnvQuota(c stdctxEnv, acct state.Account, app state.App, scope, key string, limits api.Limits) *api.Problem {
	// Count across ALL scopes — the per-app EnvVarsMax is global
	// per ADR-090 D6, so a customer moving keys between scopes
	// (write-then-delete) is bounded by the same cap.
	n, err := s.store.CountAppEnv(c, acct.ID, app.ID)
	if err != nil {
		return api.ErrCapacity("could not count env vars")
	}
	already, err := s.envExistsInScope(c, acct.ID, app.ID, scope, key)
	if err != nil {
		return api.ErrCapacity("could not check env var")
	}
	if !already && n >= limits.EnvVarsMax {
		return api.ErrPlanLimitEnvVars(limits, n)
	}
	return nil
}

// deleteEnv removes the (app_id, scope, key) row. Two distinct
// failure modes map to two distinct status codes — both are
// documented on the OpenAPI DELETE shape
// (api/openapi.yaml:1509-1513):
//
//   - loadApp misses        → 404 NotFound (the *app* doesn't exist
//     or isn't owned by the caller; loadApp owns this contract)
//   - DeleteAppEnv misses   → 400 CodeEnvVarNotFound (the env var
//     key isn't set on an existing app; the URL resource IS the
//     env var, not the app)
//
// The 400/404 split mirrors the secrets DELETE surface
// (handlers_secrets.go + api/openapi.yaml:1429-1438) so SDK
// callers reuse the same error-decoding branch. The DELETE is
// idempotent on the row side: a re-DELETE of a just-removed row
// returns 400 env_var_not_found, not 404 — by design.
//
// `?scope=__all__` is rejected (400 env_scope_reserved) — same
// reason as on PUT: the sentinel has no meaning on a single-row
// delete. Omitted scope means `scope=default`.
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
	scope, _, prob := scopeFromQuery(r, false /* allowAll */)
	if prob != nil {
		api.WriteProblem(w, prob)
		return
	}
	if err := s.store.DeleteAppEnvInScope(r.Context(), acct.ID, app.ID, scope, key); err != nil {
		if errors.Is(err, state.ErrNotFound) {
			api.WriteProblem(w, api.ErrEnvVarNotFound(key))
			return
		}
		api.WriteProblem(w, api.ErrCapacity("could not delete env var"))
		return
	}
	s.log.Info("env deleted",
		"app", app.Slug,
		"scope", logsanitize.Field(scope),
		"key", logsanitize.Field(key),
		"account", acct.ID,
	)
	// Issue #395: env.deleted is the DELETE counterpart of env.set.
	// ADR-090 PR-B: payload widens with `scope` (same shape as
	// env.set). Pre-PR-B audit consumers see an extra field but
	// no semantic change to the existing ones.
	s.audit.Emit(r.Context(), "env.deleted", &acct.ID, map[string]any{
		"app_id": app.ID,
		"scope":  scope,
		"name":   key,
	})
	w.WriteHeader(http.StatusNoContent)
}

// envExistsInScope checks if a (app_id, scope, key) row exists for
// the account. Used by setEnv to subtract 1 from the quota count
// when an upsert is replacing an existing row. Mirrors secretExists
// — see that helper's comment for the O(n) rationale and the
// Store-surface decision.
func (s *server) envExistsInScope(c stdctxEnv, accountID, appID, scope, key string) (bool, error) {
	rows, err := s.store.ListAppEnvInScope(c, accountID, appID, scope)
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
