// IAM-3 server-side session revocation (ADR-039, issue #187 + #244
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
//
// Cleanup: every accepted touch is keyed by a fresh *touchTicket
// pointer returned by sync.Map.LoadOrStore. After the touch
// goroutine fires its update it calls ticket.AfterFire(window)
// which atomically deletes the entry IFF no concurrent firer has
// stamped a newer ticket in the meantime. Working set stays at
// "active sids in the last 5 minutes" rather than "all sids
// ever seen".
type sessionTouchDebounce struct {
	m sync.Map // map[string]*touchTicket
}

// touchTicket is the per-fire record. Pointer identity is what
// the eviction path checks: a stale firer's AfterFire only
// deletes when its ticket is still the one in the map.
type touchTicket struct {
	m    *sync.Map
	sid  string
	fire time.Time
}

// shouldTouch returns true if (a) this is the first time we've
// seen this sid, or (b) the last touch is older than
// sessionTouchWindow. CAS via sync.Map.LoadOrStore keeps the
// read path lock-free AND elects exactly one firer per window.
// Callers that receive true here fire the touch goroutine and
// call ticket.AfterFire at the end of it.
func (d *sessionTouchDebounce) shouldTouch(sid string, now time.Time) (*touchTicket, bool) {
	ticket := &touchTicket{m: &d.m, sid: sid, fire: now}
	existing, loaded := d.m.LoadOrStore(sid, ticket)
	if loaded {
		prev := existing.(*touchTicket)
		if now.Sub(prev.fire) < sessionTouchWindow {
			return nil, false
		}
		// Window elapsed but a prior ticket still lingers.
		// CAS-store ours — if a concurrent caller stored a
		// ticket in between, ours loses.
		if cur, ok := d.m.Load(sid); ok && cur == prev {
			if d.m.CompareAndSwap(sid, prev, ticket) {
				return ticket, true
			}
			// Lost the race: re-check whether the new
			// ticket is still inside the window.
			if cur, ok := d.m.Load(sid); ok {
				latest := cur.(*touchTicket)
				if now.Sub(latest.fire) < sessionTouchWindow {
					return nil, false
				}
			}
		}
	}
	return ticket, true
}

// AfterFire is called by the firing goroutine after the
// last_seen_at UPDATE returns (success or failure — the eviction
// is independent of the touch result). It atomically removes
// this ticket from the map IFF the stored entry is still this
// pointer; if a fresher firer has stamped its own ticket, ours
// is stale and we exit silently.
func (t *touchTicket) AfterFire(window time.Duration) {
	if t == nil {
		return
	}
	time.Sleep(window)
	t.m.CompareAndDelete(t.sid, t)
}

// active reports the size of the underlying sync.Map for
// diagnostics. Tests use it; production code does not.
func (d *sessionTouchDebounce) active() int {
	count := 0
	d.m.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
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
	//
	// shouldTouch elects exactly one firer per window and
	// returns a *touchTicket; the firing goroutine calls
	// ticket.AfterFire(window) at the end (success or failure)
	// so the per-sid map entry is removed `sessionTouchWindow`
	// later — the eviction is versioned via ticket pointer so
	// a fresher firer doesn't get its entry deleted by a
	// stale goroutine.
	if debounce != nil {
		if ticket, fire := debounce.shouldTouch(env.Sid, time.Now()); fire {
			go func(parentCtx context.Context, sid string, t *touchTicket) {
				defer t.AfterFire(sessionTouchWindow)
				ctx, cancel := context.WithTimeout(parentCtx, 2*time.Second)
				defer cancel()
				if err := store.TouchSessionLastSeen(ctx, sid); err != nil {
					if log != nil {
						log.Warn("session last_seen_at touch failed", "sid", sid, "error", err.Error())
					}
				}
			}(r.Context(), env.Sid, ticket)
		}
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
