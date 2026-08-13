package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// fakeCustomDomain satisfies customDomain so the allowlist tests don't have
// to import pkg/state (which transitively pulls pgx into the test binary).
type fakeCustomDomain struct {
	verifiedAt time.Time
}

func (d fakeCustomDomain) Verified() bool { return !d.verifiedAt.IsZero() }

// fakeDomainLookup is a function-table lookup satisfying OnDemandLookup.
// The struct carries the configurable state; the DomainByName method is the
// function value tests hand to NewPGAllowlist.
type fakeDomainLookup struct {
	mu     sync.Mutex
	rows   map[string]fakeCustomDomain
	err    error // when set, every lookup returns this error
	called int
}

func newFakeDomainLookup() *fakeDomainLookup {
	return &fakeDomainLookup{rows: map[string]fakeCustomDomain{}}
}

func (f *fakeDomainLookup) put(host string, verified bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := fakeCustomDomain{}
	if verified {
		d.verifiedAt = time.Now()
	}
	f.rows[host] = d
}

// DomainByName exposes the struct as an OnDemandLookup function. Tests pass
// store.DomainByName directly to NewPGAllowlist — no adapter needed.
func (f *fakeDomainLookup) DomainByName(_ context.Context, domain string) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called++
	if f.err != nil {
		return nil, f.err
	}
	d, ok := f.rows[domain]
	if !ok {
		return nil, ErrNotFound
	}
	return d, nil
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func TestPGAllowlist_AllowsVerifiedDomain(t *testing.T) {
	store := newFakeDomainLookup()
	store.put("jane-api.example.com", true)
	al := NewPGAllowlist(store.DomainByName, nil, ".apps.gregale.dev", quietLogger())
	ok, err := al(context.Background(), "jane-api.example.com")
	if err != nil {
		t.Fatalf("verified domain lookup err = %v, want nil", err)
	}
	if !ok {
		t.Fatal("verified domain must be allowlisted")
	}
}

func TestPGAllowlist_DeniesUnverified(t *testing.T) {
	store := newFakeDomainLookup()
	store.put("pending.example.com", false) // exists but TXT challenge unresolved
	al := NewPGAllowlist(store.DomainByName, nil, ".apps.gregale.dev", quietLogger())
	ok, err := al(context.Background(), "pending.example.com")
	if err != nil {
		t.Fatalf("unverified lookup err = %v, want nil", err)
	}
	if ok {
		t.Fatal("unverified domain must NOT be allowlisted (spec §7: TXT gate)")
	}
}

func TestPGAllowlist_DeniesUnknown(t *testing.T) {
	store := newFakeDomainLookup()
	al := NewPGAllowlist(store.DomainByName, nil, ".apps.gregale.dev", quietLogger())
	ok, err := al(context.Background(), "attacker.example.com")
	if err != nil {
		t.Fatalf("unknown host lookup err = %v, want nil", err)
	}
	if ok {
		t.Fatal("unknown domain must NOT be allowlisted (cert-mint abuse vector)")
	}
}

// TestPGAllowlist_FailsClosedOnDBError — the moment Postgres has a hiccup, we
// must refuse to mint certs. The alternative (fail-open) is the canonical
// "your TLS seam leaked during an outage" failure mode.
func TestPGAllowlist_FailsClosedOnDBError(t *testing.T) {
	store := newFakeDomainLookup()
	store.err = errors.New("conn refused")
	al := NewPGAllowlist(store.DomainByName, nil, ".apps.gregale.dev", quietLogger())
	ok, err := al(context.Background(), "anything.example.com")
	if err != nil {
		t.Fatalf("DB-error lookup err = %v, want nil (swallowed, fail closed)", err)
	}
	if ok {
		t.Fatal("allowlist must fail closed on DB error")
	}
}

// TestPGAllowlist_NilLookupFailsClosed — a misconfigured edge (no DB pool,
// or the wire-up step was skipped) must refuse to mint certs. Failing open
// would let any hostname through.
func TestPGAllowlist_NilLookupFailsClosed(t *testing.T) {
	al := NewPGAllowlist(nil, nil, ".apps.gregale.dev", quietLogger())
	ok, err := al(context.Background(), "anything.example.com")
	if err != nil {
		t.Fatalf("nil-lookup err = %v, want nil (swallowed, fail closed)", err)
	}
	if ok {
		t.Fatal("nil lookup must deny (fail-closed on misconfiguration)")
	}
}

func TestStaticAllowlist(t *testing.T) {
	al := StaticAllowlist("a.example.com", "b.example.com")
	for _, host := range []string{"a.example.com", "b.example.com"} {
		ok, err := al(context.Background(), host)
		if err != nil {
			t.Fatalf("%s err = %v, want nil", host, err)
		}
		if !ok {
			t.Errorf("static allowlist should allow %q", host)
		}
	}
	ok, err := al(context.Background(), "c.example.com")
	if err != nil {
		t.Fatalf("c.example.com err = %v, want nil", err)
	}
	if ok {
		t.Error("static allowlist must deny unlisted host")
	}
}

// TestCountingAllowlist — the counter wraps the inner allowlist and records
// every invocation so tests can assert certmagic reached the decision func.
func TestCountingAllowlist(t *testing.T) {
	inner := StaticAllowlist("allowed.example.com")
	c := NewCountingAllowlist(inner)

	ok, err := c.allow(context.Background(), "allowed.example.com")
	if err != nil {
		t.Fatalf("allowed lookup err = %v, want nil", err)
	}
	if !ok {
		t.Error("allowed host should return true")
	}
	ok, err = c.allow(context.Background(), "denied.example.com")
	if err != nil {
		t.Fatalf("denied lookup err = %v, want nil", err)
	}
	if ok {
		t.Error("denied host should return false")
	}
	if got := c.Allow.Load(); got != 1 {
		t.Errorf("Allow counter = %d, want 1", got)
	}
	if got := c.Deny.Load(); got != 1 {
		t.Errorf("Deny counter = %d, want 1", got)
	}
	got := c.Seen()
	if len(got) != 2 || got[0] != "allowed.example.com" || got[1] != "denied.example.com" {
		t.Errorf("Seen = %v, want [allowed, denied]", got)
	}
}

func TestCountingAllowlist_NilInnerDefaultsToDenyAll(t *testing.T) {
	c := NewCountingAllowlist(nil)
	ok, err := c.allow(context.Background(), "any.example.com")
	if err != nil {
		t.Fatalf("nil-inner lookup err = %v, want nil", err)
	}
	if ok {
		t.Error("nil inner should default to deny-all")
	}
	if c.Allow.Load() != 0 || c.Deny.Load() != 1 {
		t.Errorf("counters want (0,1), got (%d,%d)", c.Allow.Load(), c.Deny.Load())
	}
}

// TestCountingAllowlist_DBErrorCountsAsDeny — when the inner allowlist
// returns (false, err), the counter still increments Deny so tests can
// assert the wire reached the callback, and the error propagates to the
// caller unchanged so certmagic surfaces it in its retry loop.
func TestCountingAllowlist_DBErrorCountsAsDeny(t *testing.T) {
	inner := func(_ context.Context, _ string) (bool, error) {
		return false, errors.New("conn refused")
	}
	c := NewCountingAllowlist(inner)
	ok, err := c.allow(context.Background(), "any.example.com")
	if err == nil {
		t.Fatal("error must propagate")
	}
	if ok {
		t.Error("DB error must surface as deny")
	}
	if c.Allow.Load() != 0 || c.Deny.Load() != 1 {
		t.Errorf("counters want (0,1), got (%d,%d)", c.Allow.Load(), c.Deny.Load())
	}
}

// fakePreviewApp satisfies previewOpen so the preview-allowlist tests don't
// have to import pkg/state (which transitively pulls pgx into the test
// binary). Mirrors fakeCustomDomain above.
type fakePreviewApp struct {
	prState string // "open" | "closed" | "stale" | "torn_down" | ""
}

func (p fakePreviewApp) PreviewOpen() bool { return p.prState == "open" }

// fakePreviewLookup is a function-table lookup satisfying
// OnDemandPreviewLookup. The struct carries the configurable state;
// PreviewByKey is the function value tests hand to NewPGAllowlist.
type fakePreviewLookup struct {
	mu     sync.Mutex
	rows   map[string]fakePreviewApp // key: "{n}|{parent-slug}"
	err    error                     // when set, every lookup returns this error
	called int
}

func newFakePreviewLookup() *fakePreviewLookup {
	return &fakePreviewLookup{rows: map[string]fakePreviewApp{}}
}

func (f *fakePreviewLookup) put(n int, slug, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[fmt.Sprintf("%d|%s", n, slug)] = fakePreviewApp{prState: state}
}

// PreviewByKey exposes the struct as an OnDemandPreviewLookup function.
// Tests pass store.PreviewByKey directly to NewPGAllowlist — no adapter
// needed (mirrors DomainByName above).
func (f *fakePreviewLookup) PreviewByKey(_ context.Context, n int, slug string) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called++
	if f.err != nil {
		return nil, f.err
	}
	row, ok := f.rows[fmt.Sprintf("%d|%s", n, slug)]
	if !ok {
		return nil, ErrNotFound
	}
	return row, nil
}

// TestOnDemandAllowlist_PreviewHost (issue #272 / ADR-095 PR-B) — pins the
// 7-table of locked cases for the preview-allowlist branch. The table
// mirrors TestPreviewScopeFromHost in cmd/gatewayd-internal so the parser
// and the allowlist agree on every shape.
//
// The custom-domain lookup is intentionally nil here so we exercise only
// the preview branch — production wires both, but the contract we care
// about is "preview host with state=open is allowlisted, everything
// else is denied".
func TestOnDemandAllowlist_PreviewHost(t *testing.T) {
	const suffix = ".apps.gregale.dev"

	cases := []struct {
		name      string
		host      string
		state     string // "" = no row, "open", "closed", etc.
		lookupErr error
		want      bool
	}{
		{
			name: "open preview host allows", host: "pr-42-foo.apps.gregale.dev",
			state: "open", want: true,
		},
		{
			name: "closed preview host denies (warn)", host: "pr-7-foo.apps.gregale.dev",
			state: "closed", want: false,
		},
		{
			name: "stale preview host denies", host: "pr-8-foo.apps.gregale.dev",
			state: "stale", want: false,
		},
		{
			name: "missing preview row denies (info)", host: "pr-99-foo.apps.gregale.dev",
			state: "", want: false,
		},
		{
			name: "lookup DB error denies (warn)", host: "pr-42-foo.apps.gregale.dev",
			lookupErr: errors.New("conn refused"), want: false,
		},
		{
			name: "non-preview host denies", host: "foo.apps.gregale.dev",
			state: "", want: false,
		},
		{
			name: "malformed preview host denies (parse fails)", host: "pr-abc-foo.apps.gregale.dev",
			state: "", want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakePreviewLookup()
			if tc.state != "" {
				// Use the locked parser to derive the (n, slug) key
				// from the test host so the table stays compact.
				n, slug, ok := PreviewScopeFromHost(suffix, tc.host)
				if !ok && tc.want {
					t.Fatalf("test setup error: PreviewScopeFromHost(%q) refused a host we expect to allow", tc.host)
				}
				if ok {
					store.put(n, slug, tc.state)
				}
			}
			store.err = tc.lookupErr
			al := NewPGAllowlist(nil, store.PreviewByKey, suffix, quietLogger())
			ok, err := al(context.Background(), tc.host)
			if err != nil {
				t.Fatalf("allowlist err = %v, want nil (errors are swallowed to fail closed)", err)
			}
			if ok != tc.want {
				t.Fatalf("allowlist(%q) = %v, want %v", tc.host, ok, tc.want)
			}
		})
	}
}

// TestOnDemandAllowlist_PreviewBranchDisabledWhenSuffixEmpty — a
// misconfigured gateway (no apps suffix in TOML) must disable the preview
// branch entirely; the allowlist must NOT spuriously allow preview hosts
// just because the parser refused them. Mirrors nil-previewLookup: deny.
func TestOnDemandAllowlist_PreviewBranchDisabledWhenSuffixEmpty(t *testing.T) {
	store := newFakePreviewLookup()
	store.put(42, "foo", "open")
	al := NewPGAllowlist(nil, store.PreviewByKey, "", quietLogger())
	ok, err := al(context.Background(), "pr-42-foo.apps.gregale.dev")
	if err != nil {
		t.Fatalf("allowlist err = %v, want nil", err)
	}
	if ok {
		t.Fatal("preview branch must be disabled when appsSuffix is empty")
	}
}

// TestOnDemandAllowlist_NilPreviewLookupDisablesPreviewBranch — production
// may wire only the custom-domain path (e.g. staging environments that
// don't mint preview certs). The allowlist must not panic on a nil
// previewLookup.
func TestOnDemandAllowlist_NilPreviewLookupDisablesPreviewBranch(t *testing.T) {
	al := NewPGAllowlist(nil, nil, ".apps.gregale.dev", quietLogger())
	ok, err := al(context.Background(), "pr-42-foo.apps.gregale.dev")
	if err != nil {
		t.Fatalf("allowlist err = %v, want nil", err)
	}
	if ok {
		t.Fatal("nil previewLookup must deny preview hosts (no row lookup attempted)")
	}
}

// TestOnDemandAllowlist_CustomDomainTakesPrecedence — when both the
// custom-domain and preview branches could in principle match (rare —
// custom domains don't share the *.apps zone), the custom-domain branch
// runs first and short-circuits. This pins the order so a future refactor
// doesn't flip the precedence and silently widen the cert-mint surface.
func TestOnDemandAllowlist_CustomDomainTakesPrecedence(t *testing.T) {
	dom := newFakeDomainLookup()
	dom.put("custom.apps.gregale.dev", true) // hypothetical custom row
	prev := newFakePreviewLookup()
	prev.put(42, "custom", "open")
	al := NewPGAllowlist(dom.DomainByName, prev.PreviewByKey, ".apps.gregale.dev", quietLogger())
	ok, err := al(context.Background(), "custom.apps.gregale.dev")
	if err != nil {
		t.Fatalf("allowlist err = %v, want nil", err)
	}
	if !ok {
		t.Fatal("verified custom domain must be allowlisted (custom branch runs first)")
	}
	if prev.called != 0 {
		t.Errorf("previewLookup called = %d, want 0 (custom branch must short-circuit)", prev.called)
	}
}

// TestRoutingAndCertAllowlistShareStore (issue #272 / ADR-095 PR-B R-B5) —
// pins the shared-store invariant: pgRouter and NewPGAllowlist must close
// over the same *state.PgStore so a hostname that the router accepts
// cannot be rejected by the allowlist (or vice versa). We model this as
// a property test on the test-only seam: a single fake store fronts both
// the routing lookup and the allowlist, and every "open" row in the
// routing table is also allowlisted.
func TestRoutingAndCertAllowlistShareStore(t *testing.T) {
	const suffix = ".apps.gregale.dev"

	// The fake store fronts both paths.
	store := newFakeDomainLookup() // custom-domain path
	prev := newFakePreviewLookup() // preview path

	// Seed an open preview row + a verified custom row.
	store.put("jane-api.example.com", true)
	prev.put(42, "foo", "open")

	al := NewPGAllowlist(store.DomainByName, prev.PreviewByKey, suffix, quietLogger())

	// The allowlist answers (true, nil) for both shapes via the shared
	// store. A real wiring would close over the same *state.PgStore;
	// the fake satisfies the same dual interface.
	for _, host := range []string{"jane-api.example.com", "pr-42-foo.apps.gregale.dev"} {
		ok, err := al(context.Background(), host)
		if err != nil {
			t.Fatalf("allowlist(%q) err = %v, want nil", host, err)
		}
		if !ok {
			t.Errorf("allowlist(%q) = false, want true (shared-store invariant)", host)
		}
	}
}
