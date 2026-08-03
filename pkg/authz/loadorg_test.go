// loadorg_test.go — unit tests for LoadOrgWithResolver. These
// drive the per-request body with httptest.NewRecorder + a stub
// OrgResolver so the middleware can be exercised without booting
// the apid subprocess or a Postgres service container. The e2e
// probes in cmd/e2e/load_org_e2e_test.go cover the same surface
// end-to-end; these tests cover the per-request semantics so a
// future regression in the resolver path surfaces at `make test`
// rather than CI.
//
// Issue #190 / IAM-6 / ADR-061, PR 4.

package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
	authmw "github.com/onebox-faas/faas/pkg/auth/middleware"
	"github.com/onebox-faas/faas/pkg/state"
)

// stubResolver is the in-memory OrgResolver used by every test
// in this file. The OrgBySlug + OrgMemberByAccount maps let each
// test pre-load exactly the row(s) it wants resolved; reads
// default to state.ErrNotFound so unknown-slug tests get the
// same path production sees.
type stubResolver struct {
	orgs    map[string]state.Org
	members map[string]state.OrgMembership
}

func newStubResolver() *stubResolver {
	return &stubResolver{
		orgs:    map[string]state.Org{},
		members: map[string]state.OrgMembership{},
	}
}

func (s *stubResolver) OrgBySlug(_ context.Context, slug string) (state.Org, error) {
	o, ok := s.orgs[slug]
	if !ok {
		return state.Org{}, state.ErrNotFound
	}
	return o, nil
}

func (s *stubResolver) OrgMemberByAccount(_ context.Context, orgID, accountID string) (state.OrgMembership, error) {
	key := orgID + "|" + accountID
	m, ok := s.members[key]
	if !ok {
		return state.OrgMembership{}, state.ErrNotFound
	}
	return m, nil
}

// reqWithPrincipal builds an *http.Request with a principal
// stamped via pkg/auth/middleware — the same seam RequireSession
// uses in production. The test then drives LoadOrgWithResolver
// directly without booting the auth chain.
func reqWithPrincipal(t *testing.T, method, path string, header string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	acct := state.Account{ID: "acct-1"}
	req = req.WithContext(authmw.WithPrincipal(req.Context(), acct, nil, nil))
	if header != "" {
		req.Header.Set("X-Active-Org", header)
	}
	return req
}

// noopHandler is the next handler LoadOrgWithResolver wraps when
// driving tests. It records pass-through so the test can assert
// whether the middleware forwarded the request or wrote a
// problem response.
type noopHandler struct {
	called             bool
	membershipObserved *state.OrgMembership
}

func (n *noopHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n.called = true
	// Mirror the production read site: MembershipFrom is what
	// the rest of the cmd/apid route table uses to observe the
	// stamped membership.
	if mem, ok := MembershipFrom(r); ok {
		n.membershipObserved = mem
	}
	w.WriteHeader(http.StatusOK)
}

// runLoadOrg drives a single LoadOrg cycle with the given req +
// stub resolver + fake audit. Returns the recorder + the next
// handler so the test can assert both problem responses and
// pass-through state.
func runLoadOrg(t *testing.T, req *http.Request, resolver OrgResolver, audit *fakeAudit) (*httptest.ResponseRecorder, *noopHandler) {
	t.Helper()
	cfg := LoadOrgConfig{
		HeaderName: "X-Active-Org",
		QueryName:  "org",
		Audit:      audit,
	}
	mw := LoadOrgWithResolver(cfg, resolver)
	next := &noopHandler{}
	rec := httptest.NewRecorder()
	mw(next).ServeHTTP(rec, req)
	return rec, next
}

// TestLoadOrg_NilPrincipal_FailsClosed — RequireSession didn't run
// (no principal stamped on the request). LoadOrg must write 500
// CodeCapacity, not call the next handler. 500 is correct: this
// is a wiring bug, not a user error, and a 403 would silently hide
// it.
func TestLoadOrg_NilPrincipal_FailsClosed(t *testing.T) {
	req := httptest.NewRequest("GET", "/v1/orgs/me", nil)
	// No principal stamped.
	rec, next := runLoadOrg(t, req, newStubResolver(), nil)
	if next.called {
		t.Fatal("next handler called without principal; want 500")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), api.CodeCapacity) {
		t.Errorf("body = %q, want code %q", rec.Body.String(), api.CodeCapacity)
	}
}

// TestLoadOrg_PassthroughNoSlug — neither header nor query set →
// LoadOrg forwards the request with no membership stamped. The
// /v1/orgs/me route relies on this for the "no active org" case.
func TestLoadOrg_PassthroughNoSlug(t *testing.T) {
	req := reqWithPrincipal(t, "GET", "/v1/orgs/me", "" /* no header */)
	rec, next := runLoadOrg(t, req, newStubResolver(), nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !next.called {
		t.Fatal("next handler not called; LoadOrg should pass through when no slug is set")
	}
	if next.membershipObserved != nil {
		t.Errorf("membership = %+v, want nil", next.membershipObserved)
	}
}

// TestLoadOrg_HeaderPreferredOverQuery — both header and query set
// → header wins. The good header resolves; the bad query is
// ignored. This is the IDOR-safe precedence: a malicious query
// string can't override a legit header.
func TestLoadOrg_HeaderPreferredOverQuery(t *testing.T) {
	resolver := newStubResolver()
	resolver.orgs["good"] = state.Org{ID: "org-1", Slug: "good"}
	resolver.orgs["bad"] = state.Org{ID: "org-bad", Slug: "bad"}
	resolver.members["org-1|acct-1"] = state.OrgMembership{OrgID: "org-1", AccountID: "acct-1", Role: state.OrgRoleOwner}

	req := httptest.NewRequest("GET", "/v1/orgs/me?org=bad", nil)
	acct := state.Account{ID: "acct-1"}
	req = req.WithContext(authmw.WithPrincipal(req.Context(), acct, nil, nil))
	req.Header.Set("X-Active-Org", "good")

	rec, next := runLoadOrg(t, req, resolver, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !next.called {
		t.Fatal("next handler not called")
	}
	if next.membershipObserved == nil || next.membershipObserved.OrgID != "org-1" {
		t.Errorf("membership = %+v, want OrgID=org-1 (header should win)", next.membershipObserved)
	}
}

// TestLoadOrg_UnknownSlug_404 — resolver returns state.ErrNotFound
// for OrgBySlug → LoadOrg writes 404 org_not_found and emits one
// audit row. The audit shape must include the slug in the
// "msg"-style fields so pkg/audit's downstream consumers can
// group by slug.
func TestLoadOrg_UnknownSlug_404(t *testing.T) {
	resolver := newStubResolver() // empty: any slug → ErrNotFound
	audit := &fakeAudit{}
	req := reqWithPrincipal(t, "GET", "/v1/orgs/me", "no-such-org")
	rec, next := runLoadOrg(t, req, resolver, audit)

	if next.called {
		t.Fatal("next handler called for unknown slug; want 404")
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), api.CodeOrgNotFound) {
		t.Errorf("body = %q, want code %q", rec.Body.String(), api.CodeOrgNotFound)
	}
	if len(audit.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(audit.events))
	}
	if audit.events[0].event != "authz.denied" {
		t.Errorf("event = %q, want authz.denied", audit.events[0].event)
	}
	if audit.events[0].fields["action"] != "org.load.not_found" {
		t.Errorf("fields[action] = %v, want org.load.not_found", audit.events[0].fields["action"])
	}
}

// TestLoadOrg_NonMember_403 — OrgBySlug OK but OrgMemberByAccount
// returns ErrNotFound → 403 org_role_forbidden. IDOR-safe: the
// response shape is the same as 404 except the code, so an
// attacker enumerating slugs cannot distinguish "unknown" from
// "known but not a member".
func TestLoadOrg_NonMember_403(t *testing.T) {
	resolver := newStubResolver()
	resolver.orgs["acme"] = state.Org{ID: "org-1", Slug: "acme"}
	// No member row for acct-1 in org org-1.
	audit := &fakeAudit{}
	req := reqWithPrincipal(t, "GET", "/v1/orgs/me", "acme")
	rec, next := runLoadOrg(t, req, resolver, audit)

	if next.called {
		t.Fatal("next handler called for non-member; want 403")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), api.CodeOrgRoleForbidden) {
		t.Errorf("body = %q, want code %q", rec.Body.String(), api.CodeOrgRoleForbidden)
	}
	if len(audit.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(audit.events))
	}
	if audit.events[0].fields["action"] != "org.load.not_member" {
		t.Errorf("fields[action] = %v, want org.load.not_member", audit.events[0].fields["action"])
	}
}

// TestLoadOrg_SlugTrimAndCap — whitespace-only and oversize
// values must degrade to passthrough. The schema's CHECK
// (migrations/00099) rejects oversize values with a non-
// ErrNotFound error, which would surface as 500; the length cap
// pushes the test below that boundary. Trim must run so
// "  acme  " passes through to "acme" (mirroring how the
// pkg/api.OrgSlugPattern regex is matched case-insensitively
// elsewhere).
func TestLoadOrg_SlugTrimAndCap(t *testing.T) {
	t.Run("whitespace-only header passthrough", func(t *testing.T) {
		req := reqWithPrincipal(t, "GET", "/v1/orgs/me", "   ")
		rec, next := runLoadOrg(t, req, newStubResolver(), nil)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200 (passthrough); body=%s", rec.Code, rec.Body.String())
		}
		if !next.called {
			t.Fatal("next handler not called; want passthrough")
		}
	})

	t.Run("oversize slug passthrough", func(t *testing.T) {
		// 65 chars exceeds maxSlugLen (64) but is well below HTTP
		// header limits; the cap is what protects the SQL CHECK.
		oversize := strings.Repeat("a", 65)
		req := reqWithPrincipal(t, "GET", "/v1/orgs/me", oversize)
		rec, next := runLoadOrg(t, req, newStubResolver(), nil)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200 (passthrough); body=%s", rec.Code, rec.Body.String())
		}
		if !next.called {
			t.Fatal("next handler not called; want passthrough")
		}
	})

	t.Run("padded slug resolves", func(t *testing.T) {
		resolver := newStubResolver()
		resolver.orgs["acme"] = state.Org{ID: "org-1", Slug: "acme"}
		resolver.members["org-1|acct-1"] = state.OrgMembership{OrgID: "org-1", AccountID: "acct-1", Role: state.OrgRoleOwner}
		req := reqWithPrincipal(t, "GET", "/v1/orgs/me", "  acme  ")
		rec, next := runLoadOrg(t, req, resolver, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if !next.called {
			t.Fatal("next handler not called")
		}
		if next.membershipObserved == nil || next.membershipObserved.OrgID != "org-1" {
			t.Errorf("membership = %+v, want OrgID=org-1", next.membershipObserved)
		}
	})
}

// TestLoadOrg_QueryMalformed_PassThrough — URLs encode %00 %01 %02
// as literal bytes after URL-decoding; the schema rejects them
// with a non-ErrNotFound error. The trim+length-cap path
// already protects against oversize; the breakage is for empty
// bytes between valid chars (e.g. "%20%20%20" → "   " — whitespace
// that TrimSpace strips). The trim_to_empty path falls through to
// the passthrough branch, so the missing-org surface is never
// reached. This test pins that behaviour.
func TestLoadOrg_QueryMalformed_PassThrough(t *testing.T) {
	req := reqWithPrincipal(t, "GET", "/v1/orgs/me?org=%20%20%20", "")
	rec, next := runLoadOrg(t, req, newStubResolver(), nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (passthrough); body=%s", rec.Code, rec.Body.String())
	}
	if !next.called {
		t.Fatal("next handler not called; want passthrough")
	}
}

// stubBrokenResolver returns a non-ErrNotFound error from OrgBySlug
// so the middleware takes the "lookup failed" branch (the only branch
// that emits a Warn log). The empty-org stubResolver returns
// state.ErrNotFound which is the 404 path — that doesn't reach the
// log line at all, so we need a separate fake to exercise the
// sanitization gate.
type stubBrokenResolver struct{}

func (stubBrokenResolver) OrgBySlug(_ context.Context, _ string) (state.Org, error) {
	return state.Org{}, errors.New("synthetic pg outage")
}
func (stubBrokenResolver) OrgMemberByAccount(_ context.Context, _, _ string) (state.OrgMembership, error) {
	return state.OrgMembership{}, state.ErrNotFound
}

// TestLoadOrg_LogSanitizesSlug — CodeQL probes #152-156 fired on the
// org-lookup-failed Warn line because the slug flows from the
// X-Active-Org header (an attacker-controlled source) directly into
// slog. The fix wraps slug + acct.ID with pkg/logsanitize.Field so
// ASCII control characters (CR/LF/NUL) become U+00B7 before they
// reach the log stream. This test pins that: a header containing
// "acme\nfake-line" must round-trip through the log as
// "acme·fake-line" — a malicious actor cannot forge log lines via
// CR/LF injection. Without sanitization, the log stream would emit
// two lines per event and one customer's diagnostic would silently
// rotate into another's, breaking the audit story.
//
// The synthetic resolver error is what makes this test cover the
// Warn branch — the empty stubResolver returns state.ErrNotFound,
// which goes through the 404 path (no log line).
func TestLoadOrg_LogSanitizesSlug(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	cfg := LoadOrgConfig{
		HeaderName: "X-Active-Org",
		QueryName:  "org",
		Log:        logger,
	}
	req := reqWithPrincipal(t, "GET", "/v1/orgs/me", "acme\nfake-line")
	mw := LoadOrgWithResolver(cfg, stubBrokenResolver{})
	rec := httptest.NewRecorder()
	mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(rec, req)

	// The middleware should have written a 500 — and the log line
	// must contain the sanitized slug, not the raw header value.
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	out := logBuf.String()
	if out == "" {
		t.Fatal("expected a log line; got empty buffer")
	}

	// Decode the JSON log line so we can inspect the "slug" field
	// directly (instead of grepping for substrings across the
	// whole JSON).
	var entry map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &entry); err != nil {
		t.Fatalf("log line not JSON: %v (raw=%q)", err, out)
	}
	slugField, ok := entry["slug"].(string)
	if !ok {
		t.Fatalf("slug field missing or not a string: %+v", entry)
	}
	if strings.Contains(slugField, "\n") || strings.Contains(slugField, "\r") {
		t.Errorf("slug field still contains CR/LF: %q", slugField)
	}
	if !strings.Contains(slugField, "acme") || !strings.Contains(slugField, "fake-line") {
		t.Errorf("slug field lost legitimate content: %q", slugField)
	}
	// The original "acme\nfake-line" must NOT appear verbatim.
	if strings.Contains(slugField, "acme\nfake-line") {
		t.Errorf("slug field un-sanitized: %q", slugField)
	}
}
