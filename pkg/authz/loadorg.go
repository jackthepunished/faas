// LoadOrgWithResolver resolves the caller's active-org membership for
// the current request and stamps it onto the request context for
// downstream handlers + AuthorizeOrgAction. The middleware is the
// half of the authz seam PR 4 (issue #190 / IAM-6 / ADR-061) ships
// alongside the AuthorizeOrgAction table.
//
// # Reading the active org
//
// The middleware reads the org slug from two surfaces, in priority
// order:
//
//  1. The X-Active-Org request header (recommended for SDK and CLI
//     callers — survives query-string stripping by intermediate
//     proxies).
//  2. The ?org=<slug> query string (the dashboard's primary path;
//     matches the api/openapi.yaml Idempotency-Key convention of
//     accepting both header and query forms for the same parameter).
//
// If both are set, the header wins. If neither is set, the middleware
// passes through with no membership stamped — every pre-PR-5 route
// stays account-scoped and the wire shape is unchanged.
//
// Fail-closed behaviour
//
//   - RequireSession did not run (no principal on ctx) → 500
//     api.CodeCapacity. This is a wiring bug, not a user error.
//   - Unknown org slug → 404 api.CodeOrgNotFound. Slug shape is
//     validated by the regex in pkg/api/errors.go::OrgSlugPattern.
//   - Known org but caller is not a member → 403
//     api.CodeOrgRoleForbidden. IDOR-safe: both 404 and 403 are 4xx
//     so an attacker can't enumerate slugs.
//
// Audit emission is opt-in via LoadOrgConfig.Audit; passing nil keeps
// PR 4's hot path allocation-free. PR 5+ will plumb the real audit
// emitter through cmd/apid/main.go.
package authz

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// LoadOrgConfig configures LoadOrgWithResolver. The zero value is
// usable: header defaults to "X-Active-Org", query to "org", and
// the audit emitter is nil (no audit rows emitted).
type LoadOrgConfig struct {
	// Log is the structured logger; nil → slog.Default().
	Log *slog.Logger

	// HeaderName is the request header to read. Empty defaults to
	// "X-Active-Org" (ADR-061 wording).
	HeaderName string

	// QueryName is the query-string parameter to read. Empty
	// defaults to "org".
	QueryName string

	// Now is the clock function. nil defaults to time.Now. Tests
	// pass a fixed time so audit-row timestamps are stable.
	Now func() time.Time

	// Audit is the audit emitter for org-resolution failures.
	// nil → no audit rows emitted (PR 4 default; PR 5+ wires the
	// real pkg/audit seam).
	Audit AuditEmitter
}

// LoadOrgWithResolver is the canonical LoadOrg constructor. It
// mirrors pkg/middleware.AuthLimitWithLimiter's shape: panics on
// nil resolver, defaults the config, returns a func(next) Handler
// suitable for use with mux.Handle(...) and the cmd/apid façade
// pattern.
//
// The closure captures cfg + resolver; a per-request call site
// invokes the returned handler with the request lifecycle (httptest
// and the production mux both honour this).
func LoadOrgWithResolver(cfg LoadOrgConfig, r OrgResolver) func(http.Handler) http.Handler {
	if r == nil {
		// Same panic shape as pkg/middleware/authlimit.go:161-165.
		// Refuse to silently fall back to a nil-resolver no-op
		// — that's exactly the fail-open path this middleware
		// exists to prevent.
		panic("authz: LoadOrgWithResolver requires a non-nil OrgResolver")
	}
	if cfg.HeaderName == "" {
		cfg.HeaderName = "X-Active-Org"
	}
	if cfg.QueryName == "" {
		cfg.QueryName = "org"
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			handleLoadOrg(cfg, r, w, req, next)
		})
	}
}

// handleLoadOrg is the per-request body, factored out so the tests
// can drive it directly with an httptest.ResponseRecorder and a
// stub OrgResolver.
func handleLoadOrg(cfg LoadOrgConfig, r OrgResolver, w http.ResponseWriter, req *http.Request, next http.Handler) {
	ctx := req.Context()

	// (1) Recover the principal. RequireSession stamps it before
	// LoadOrg runs (the cmd/apid route table composes
	// s.auth → s.loadOrg). If the principal is missing, the route
	// is mis-wired — fail closed.
	acct, _, _, ok := principalFromCtx(ctx)
	if !ok {
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
			api.CodeCapacity,
			"LoadOrg wired before RequireSession",
			"no principal on request context; check the route table"))
		return
	}

	// (2) Read the slug from header (preferred) or query.
	slug := req.Header.Get(cfg.HeaderName)
	if slug == "" {
		slug = req.URL.Query().Get(cfg.QueryName)
	}
	if slug == "" {
		// Passthrough: no active org requested. Pre-PR-5 routes
		// stay account-scoped; the membership field on the
		// principal is left nil.
		next.ServeHTTP(w, req)
		return
	}

	// (3) Resolve the org.
	org, err := r.OrgBySlug(ctx, slug)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			emitAudit(ctx, cfg.Audit, acct, "org.load.not_found", api.ErrOrgNotFound(slug))
			api.WriteProblem(w, api.ErrOrgNotFound(slug))
			return
		}
		cfg.Log.Warn("org lookup failed",
			"slug", slug,
			"account_id", acct.ID,
			"error", err.Error())
		emitAudit(ctx, cfg.Audit, acct, "org.load.error", api.NewProblem(http.StatusInternalServerError,
			api.CodeCapacity,
			"Org lookup failed",
			"try again; if the problem persists, contact support"))
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
			api.CodeCapacity,
			"Org lookup failed",
			"try again; if the problem persists, contact support"))
		return
	}

	// (4) Resolve the membership. Non-member → 403 IDOR-safe.
	mem, err := r.OrgMemberByAccount(ctx, org.ID, acct.ID)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			emitAudit(ctx, cfg.Audit, acct, "org.load.not_member", api.ErrOrgRoleForbidden("access this organization"))
			api.WriteProblem(w, api.ErrOrgRoleForbidden("access this organization"))
			return
		}
		cfg.Log.Warn("membership lookup failed",
			"org_id", org.ID,
			"account_id", acct.ID,
			"error", err.Error())
		emitAudit(ctx, cfg.Audit, acct, "org.load.error", api.NewProblem(http.StatusInternalServerError,
			api.CodeCapacity,
			"Membership lookup failed",
			"try again; if the problem persists, contact support"))
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
			api.CodeCapacity,
			"Membership lookup failed",
			"try again; if the problem persists, contact support"))
		return
	}

	// (5) Replace the request context with the membership stamped.
	// The principal stays the same; only Membership changes.
	// Pointer-mutation contract (issue #278 / PR #332): we mutate
	// *req so downstream observeWrap (cmd/apid) sees the new
	// membership via MembershipFrom.
	newCtx := WithActiveOrg(ctx, &mem)
	newCtx = WithRequestOnContext(newCtx, req)
	*req = *req.WithContext(newCtx)

	// (6) Continue.
	next.ServeHTTP(w, req)
}

// LoadOrg is the convenience wrapper that calls LoadOrgWithResolver
// with no Audit emitter. Equivalent to pkg/middleware.AuthLimit in
// shape (single-arg, sensible defaults). PR 4 cmd/apid wiring calls
// LoadOrgWithResolver directly so the audit emitter can be plumbed
// in PR 5 without changing the route table.
func LoadOrg(cfg LoadOrgConfig, r OrgResolver) func(http.Handler) http.Handler {
	return LoadOrgWithResolver(cfg, r)
}

// Compile-time guard: the constructor signature must stay
// assignable to func(http.Handler) http.Handler. If a future refactor
// accidentally widens it (e.g. adds an extra return value), this
// trips at build time.
var _ func(http.Handler) http.Handler = LoadOrgWithResolver(LoadOrgConfig{}, (*StoreBackedResolver)(nil))
