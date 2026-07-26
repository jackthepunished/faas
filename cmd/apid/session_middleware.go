// IAM-3 server-side session revocation (ADR-036, issue #187 + #244
// merged). Companion to mfa_middleware.go for the cookie branch of
// s.auth.
//
// Three small helpers — call sites inside s.auth are:
//
//	1. requireSessionCookie(r, w, env, store, audit, log)
//	      Returns (Session{},  false) on a usable session (caller
//	      stamps it into ctx via withSession). Returns a problem
//	      response + false when the cookie is missing sid, the row
//	      is gone, or the row is revoked. The handler calling this
//	      already verified the AEAD envelope via pkg/session.Verify.
//
//	2. sessionFrom(r)                  — read the state.Session out
//	      of the context for handlers that need the current sid.
//
//	3. clearSessionCookie(w, r)        — best-effort delete on the
//	      client when the cookie is rejected.
//
//	4. shouldTouchSession(sid, now)    — 5-minute debounce on
//	      last_seen_at UPDATEs (mirrors the per-key debounce
//	      pattern at server.go:140-175). Reads are lock-free via
//	      sync.Map.Load; writes go through LoadOrStore so two
//	      concurrent first-time callers don't both schedule a
//	      touch.
//
// All four helpers live on a free-function slice that does not
// reach into server.{Store, Audit} directly — the caller wires
// them in. This keeps requireSessionCookie testable (pkg/session
// tests) without booting a full server.

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/session"
	"github.com/onebox-faas/faas/pkg/state"
)

// sessionTouchDebounce is the per-sid debounce map for last_seen_at
// UPDATEs. Distinct from the per-key debounce in server.go (which
// gates api_keys.last_used_at). Same sync.Map idiom; per-process,
// per-daemon. Memory bounded by the number of fresh sids in the
// TTL window — a long-running daemon with N sessions / 5 minutes
// carries N entries.
type sessionTouchDebounce struct {
	m sync.Map // map[string]time.Time
}

// shouldTouch returns true if (a) this is the first time we've
// seen this sid, or (b) the last touch is older than
// sessionTouchWindow. CAS via sync.Map keeps the read path
// lock-free.
func (d *sessionTouchDebounce) shouldTouch(sid string, now time.Time) bool {
	last, loaded := d.m.LoadOrStore(sid, now)
	if !loaded {
		return true
	}
	if now.Sub(last.(time.Time)) < sessionTouchWindow {
		return false
	}
	d.m.Store(sid, now)
	return true
}

// sessionTouchWindow is the minimum interval between per-session
// last_seen_at stamps. Matches keyTouchWindow (server.go:156) —
// observability doesn't need sub-minute resolution, and a long
// window keeps WAL amplification bounded under sustained 1k RPS
// on one session (dashboard pinned open).
const sessionTouchWindow = 5 * time.Minute

// sessionCtxKey is the typed guard for the withSession context value.
// Distinct from principalCtxKey (iota-typed in server.go) so a
// future PR inserting a value in that block doesn't silently
// collide. IAM-3 only writes/reads this value inside server.go's
// cookie branch + the four handlers in handlers_sessions.go.
type sessionCtxKey struct{}

// withSession decorates the request context for handlers that need
// the validated session row (logout, listSessions, revokeSession,
// revokeAllSessions). Stamped by requireSessionCookie on success.
func withSession(ctx context.Context, sess state.Session) context.Context {
	return context.WithValue(ctx, sessionCtxKey{}, sess)
}

// sessionFrom returns the validated session row stamped by
// requireSessionCookie. (state.Session{}, false) means the route
// wiring is broken (s.auth's cookie branch ran but did not stamp
// the session — a developer error).
func sessionFrom(r *http.Request) (state.Session, bool) {
	v := r.Context().Value(sessionCtxKey{})
	if v == nil {
		return state.Session{}, false
	}
	s, ok := v.(state.Session)
	return s, ok
}

// requireSessionCookie cross-validates the AEAD-bound envelope's
// sid against the sessions row. The caller has already verified
// the cookie AEAD via s.sessions.Verify; this is the IAM-3 layer.
//
// Returns (Session{}, false, nil) on a problem that the caller
// surfaces via WriteProblem: missing-sid (rollout pre-IAM-3 cookie),
// missing-row, revoked-row, account-mismatch. The bool indicates
// whether a problem response was already written — the caller
// short-circuits to the next auth branch on true.
//
// On success returns (sess, true, nil) — the caller stamps the
// session via withSession and continues to the principal.
//
// "stolen" audit emission for found-revoked rows is the
// load-bearing distinct signal: a missing row is silent (never
// valid), a revoked row may have been valid before rotation and is
// the operator's pivot for "the customer's cookie was stolen."
// The audit Emit fires only on the found-revoked case to keep the
// row count honest. Never logs the cookie value itself (per CLAUDE
// "never log secret values").
func requireSessionCookie(
	w http.ResponseWriter, r *http.Request,
	env session.Envelope, store StoreLike, audit AuditLike, log *slog.Logger, debounce *sessionTouchDebounce,
) (state.Session, bool, error) {
	// (1) empty sid = pre-IAM-3 rollout cookie. Fail closed.
	if env.Sid == "" {
		clearSessionCookie(w, r)
		api.WriteProblem(w, api.NewProblem(http.StatusUnauthorized, api.CodeSessionExpired,
			"Session expired", "this dashboard session was issued before the session-revocation rollout; sign in again"))
		return state.Session{}, true, nil
	}

	// (2) row lookup. ErrNotFound = never-valid sid (the seal
	//     accepts AEAD-bound envelopes regardless of DB state, so
	//     this branch is the live defense — never leak existence).
	sess, err := store.GetSession(r.Context(), env.Sid)
	if err != nil {
		clearSessionCookie(w, r)
		api.WriteProblem(w, api.NewProblem(http.StatusUnauthorized, api.CodeSessionExpired,
			"Session expired", "this dashboard session has been revoked; sign in again"))
		if errors.Is(err, state.ErrNotFound) {
			// The shape we contractually surface: handled=true,
			// err=nil. The caller has already written the
			// 401; we don't need to propagate the not-found
			// (the cookie is cleared, the next request is a
			// fresh login).
			return state.Session{}, true, nil
		}
		// Real DB failure: return the err so the caller can
		// log + emit a different (operational) shape.
		return state.Session{}, true, fmt.Errorf("session lookup: %w", err)
	}

	// (3) revoked-row = possibly-stolen cookie. Distinct audit.
	if sess.RevokedAt != nil {
		clearSessionCookie(w, r)
		api.WriteProblem(w, api.NewProblem(http.StatusUnauthorized, api.CodeSessionExpired,
			"Session expired", "this dashboard session has been revoked; sign in again"))
		if audit != nil {
			audit.Emit(r.Context(), "auth.session.stolen", &env.AccountID, map[string]any{
				"sid":    env.Sid,
				"method": r.Method,
				"path":   r.URL.Path,
			})
		}
		return state.Session{}, true, nil
	}

	// (4) account-mismatch is defensive — the AEAD envelope binds
	//     AccountID into the same ciphertext as Sid, so a mismatch
	//     implies an AEAD forgery (callers reach err==nil here
	//     only with the same key that minted the cookie). 401.
	if sess.AccountID != env.AccountID {
		clearSessionCookie(w, r)
		api.WriteProblem(w, api.NewProblem(http.StatusUnauthorized, api.CodeSessionInvalid,
			"Session invalid", "session and account binding mismatch"))
		if log != nil {
			log.Warn("session account mismatch (AEAD bind broken?)",
				"sid", env.Sid, "path", r.URL.Path)
		}
		return state.Session{}, true, nil
	}

	// (5) stamp + async touch. The touch is detached — a slow PG
	//     must not block the user's request, and a canceled client
	//     still leaves a stamp for ops triage. We derive the
	//     context from the request (contextcheck lint) and attach
	//     a fresh timeout so a stuck PG doesn't accumulate
	//     goroutines.
	if debounce != nil && debounce.shouldTouch(env.Sid, time.Now()) {
		go func(parentCtx context.Context, sid string) {
			ctx, cancel := context.WithTimeout(parentCtx, 2*time.Second)
			defer cancel()
			if err := store.TouchSessionLastSeen(ctx, sid); err != nil {
				if log != nil {
					log.Warn("session last_seen_at touch failed", "sid", sid, "error", err.Error())
				}
			}
		}(r.Context(), env.Sid)
	}
	return sess, false, nil
}

// clearSessionCookie evicts the faas_sid cookie on the client.
// Path "/" + MaxAge=-1 matches the cookie Name + Secure + SameSite
// set by the issuer in handlers_auth.go. We deliberately do NOT
// touch the CSRF cookie — the CSRF cookie is bound to the
// double-submit envelope and survives session-revoke (a customer
// who just revoked their own session can still POST a /login
// without clearing the dashboard's CSRF row).
func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "faas_sid",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// StoreLike is the slice of pkg/state.Store requireSessionCookie
// needs. Defining it here as an interface keeps this file's
// imports free of pgx and avoids forcing tests to spin up a real
// pool.
type StoreLike interface {
	GetSession(ctx context.Context, id string) (state.Session, error)
	TouchSessionLastSeen(ctx context.Context, id string) error
}

// AuditLike is the slice of the audit emitter requireSessionCookie
// needs. Mirrors s.audit's Emit signature.
type AuditLike interface {
	Emit(ctx context.Context, kind string, accountID *string, data map[string]any)
}
