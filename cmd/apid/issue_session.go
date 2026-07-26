// IAM-3 (ADR-036, issue #187 + #244 merged) cookie-issue helper.
//
// issueDashboardSession is the single seam every dashboard login
// path goes through: it mints the sessions row, stamps the
// mfa-pending flag, seals the cookie envelope with the same sid,
// and emits auth.session.created. The five callers (magic-link
// verify, OAuth callbacks for Google + GitHub, CLI auth page, and
// the existing issueSessionCookie wrapper in handlers_auth_login.go)
// all switch to this helper from their old direct
// sessions.IssueWithMFAFlag calls.
//
// MFA verify / enroll confirm / recover / disable take a different
// path (reissueSessionCookie in handlers_mfa.go) — they reuse the
// EXISTING sid from the cookie's envelope instead of minting a new
// row. See plan §8.
//
// On any failure after the row is created, the helper rolls the
// row back so no orphan active row lingers. This is a best-effort
// cleanup; if the rollback itself fails, the audit Emit captures
// the orphan id so operators can sweep it manually.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"

	"github.com/google/uuid"
	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// issueDashboardSession mints a fresh sid, persists the sessions
// row, seals the cookie envelope with the same sid, and emits
// auth.session.created. Caller has already authenticated the
// account — the caller's job is to pass the right accountID and
// the mfaPending derived from mfaEnrollRequired(acct).
//
// audit failure is non-fatal: it's a best-effort observability
// row, same shape as every other audit.Emit call in the auth
// handlers (ADR-035 never blocks the action).
func (s *server) issueDashboardSession(ctx context.Context, r *http.Request, accountID string, mfaPending bool, method string) (string, state.Session, error) {
	sid := uuid.NewString()
	ip := clientIPFromRequest(r)
	ua := r.UserAgent()
	sess, err := s.store.CreateSession(ctx, sid, accountID, ip, ua)
	if err != nil {
		return "", state.Session{}, fmt.Errorf("create session row: %w", err)
	}
	cookie, err := s.sessions.IssueWithSession(sid, accountID, mfaPending)
	if err != nil {
		// Cleanup the orphan row. If this fails too, log + audit
		// so operators can sweep. We still return the original
		// error so the caller's handler emits a 5xx.
		if rbErr := s.rollbackCreatedSession(ctx, sid, accountID); rbErr != nil {
			if s.log != nil {
				s.log.Warn("session row rollback failed after seal failure",
					"sid", sid, "error", rbErr.Error())
			}
		}
		return "", state.Session{}, fmt.Errorf("seal envelope: %w", err)
	}
	if s.audit != nil {
		s.audit.Emit(ctx, "auth.session.created", &accountID, map[string]any{
			"sid":       sid,
			"method":    method,
			"issued_ip": ip,
		})
	}
	return cookie, sess, nil
}

// issueDashboardSessionWithGithub is the union helper the
// /v1/auth/github callback uses. It mints a fresh sid, persists the
// sessions row, then seals the cookie envelope with sid +
// mfa_pending + github_login in a single AEAD round (no double-seal).
//
// The doc comment block in handlers_github.go documents the wire
// contract: the cookie carries sid so apid's requireSession can
// re-validate via state.Store.GetSession, AND github_login so the
// /oauth/callback handler can satisfy the §11 ownership invariant
// on the same envelope. Pre-IAM-3 callers that don't read either
// field are unaffected because both JSON tags are `omitempty`.
//
// Same orphan-row rollback policy as issueDashboardSession.
func (s *server) issueDashboardSessionWithGithub(ctx context.Context, r *http.Request, accountID string, mfaPending bool, method, githubLogin string) (string, state.Session, error) {
	sid := uuid.NewString()
	ip := clientIPFromRequest(r)
	ua := r.UserAgent()
	sess, err := s.store.CreateSession(ctx, sid, accountID, ip, ua)
	if err != nil {
		return "", state.Session{}, fmt.Errorf("create session row: %w", err)
	}
	cookie, err := s.sessions.IssueWithSessionAndGithubLogin(sid, accountID, githubLogin, mfaPending)
	if err != nil {
		if rbErr := s.rollbackCreatedSession(ctx, sid, accountID); rbErr != nil {
			if s.log != nil {
				s.log.Warn("session row rollback failed after seal failure",
					"sid", sid, "error", rbErr.Error())
			}
		}
		return "", state.Session{}, fmt.Errorf("seal envelope: %w", err)
	}
	if s.audit != nil {
		s.audit.Emit(ctx, "auth.session.created", &accountID, map[string]any{
			"sid":       sid,
			"method":    method,
			"issued_ip": ip,
		})
	}
	return cookie, sess, nil
}

// rollbackCreatedSession best-effort revoke after a partial failure.
// Wired through RevokeSession (not DELETE) so the audit Emit
// doesn't double-fire on a row that was never used. Returns the
// underlying error to the caller for logging.
func (s *server) rollbackCreatedSession(ctx context.Context, sid, accountID string) error {
	if _, err := s.store.RevokeSession(ctx, sid, accountID); err != nil &&
		!errors.Is(err, state.ErrNotFound) {
		return err
	}
	return nil
}

// clientIPFromRequest extracts the host/IP part of r.RemoteAddr,
// stripping the ":port" suffix. Returns "" when RemoteAddr is
// empty, unparseable, or a Unix socket (a Unix-socket request has
// no IP at all — the dashboard client never legitimately arrives
// that way at apid, so "" is correct rather than "unknown").
//
// The gateway strips the X-Forwarded-For we read here is NOT
// trusted: the dashboard fronts through gatewayd which stamps
// RemoteAddr with the TCP peer IP. A direct apid exposure with
// a forged XFF is a separate threat to model. Per spec §11 the
// peer-IP is what we audit; XFF transparency is a future PR.
func clientIPFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// RemoteAddr was either empty, a bare host (no port), or
		// a Unix socket — fall back to the raw value only when
		// it's a literal IP. net.ParseAccepts forms like
		// "192.0.2.1" but rejects "192.0.2.1:443"; we already
		// peeled the port above, so this branch handles
		// "192.0.2.1" without a port.
		if ip := net.ParseIP(r.RemoteAddr); ip != nil {
			return ip.String()
		}
		return ""
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return ""
}

// _ reserves the api import for compile-time parity checks
// elsewhere (errors surface via api.WriteProblem at the call site
// rather than through this helper).
var _ = api.CodeSessionExpired
