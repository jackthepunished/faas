package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// tenantSurfaceIssuerFixture stands up a memstore-backed issuer
// with a pre-seeded account + app + surface + two verified +
// one unverified hostname. Tests that need to vary the seed
// construct their own.
func tenantSurfaceIssuerFixture(t *testing.T) (*TenantSurfaceCertIssuer, *state.MemStore, state.TenantSurface, context.Context) {
	t.Helper()
	m := state.NewMemStore()
	ctx := context.Background()
	acct, err := m.CreateAccount(ctx, "ts-issuer@example.com", api.PlanPro)
	if err != nil {
		t.Fatal(err)
	}
	app, err := m.CreateApp(ctx, state.App{AccountID: acct.ID, Slug: "ts-issuer", RAMMB: 256, Status: state.AppActive})
	if err != nil {
		t.Fatal(err)
	}
	lim := api.Limits{
		TenantSurfacesAllowed:     true,
		TenantSurfacesPerAccount:  5,
		TenantHostnamesPerSurface: 10,
	}
	surf, err := m.CreateTenantSurfaceIfUnderQuota(ctx, state.CreateTenantSurfaceParams{
		AccountID: acct.ID, AppID: app.ID, Name: "issuer",
	}, lim)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"a.example", "b.example", "c.example"} {
		host, err := m.CreateTenantHostnameIfUnderQuota(ctx, state.CreateTenantHostnameParams{
			SurfaceID: surf.ID, Hostname: h, ChallengeToken: "tok",
		}, lim)
		if err != nil {
			t.Fatal(err)
		}
		_ = host
	}
	// Mark a.example and b.example verified; c.example stays
	// unverified (and must NOT appear in the SAN set).
	for _, h := range []string{"a.example", "b.example"} {
		if err := m.MarkTenantHostnameVerified(ctx, h); err != nil {
			t.Fatal(err)
		}
	}
	issuer := NewTenantSurfaceCertIssuer(m, NewMetrics())
	return issuer, m, surf, ctx
}

func TestTenantSurfaceCertIssuer_SuccessStubPath(t *testing.T) {
	issuer, m, surf, ctx := tenantSurfaceIssuerFixture(t)
	if err := issuer.RequestCertForSurface(ctx, surf.ID); err != nil {
		t.Fatalf("RequestCertForSurface: %v", err)
	}
	got, err := m.GetTenantSurfaceByID(ctx, surf.ID)
	if err != nil {
		t.Fatal(err)
	}
	// v1 stub: cert_state=failed with a clear last_error
	// naming the verified count + primary hostname. The
	// follow-up ADR swaps this for cert_state=issued.
	if got.CertState != state.CertStateFailed {
		t.Fatalf("cert_state = %q, want %q", got.CertState, state.CertStateFailed)
	}
	if !strings.Contains(got.CertLastError, "cert engine stub") {
		t.Errorf("last_error = %q; want it to mention the cert engine stub", got.CertLastError)
	}
	if !strings.Contains(got.CertLastError, "a.example") {
		t.Errorf("last_error = %q; want it to name the primary hostname", got.CertLastError)
	}
}

func TestTenantSurfaceCertIssuer_RejectsUnverifiedSurface(t *testing.T) {
	issuer, m, surf, ctx := tenantSurfaceIssuerFixture(t)
	// Reset all verified flags by re-creating the surface and
	// adding ONLY unverified hostnames.
	_ = m
	_ = surf
	lim := api.Limits{
		TenantSurfacesAllowed:     true,
		TenantSurfacesPerAccount:  5,
		TenantHostnamesPerSurface: 10,
	}
	empty, err := m.CreateTenantSurfaceIfUnderQuota(ctx, state.CreateTenantSurfaceParams{
		AccountID: surf.AccountID, AppID: surf.AppID, Name: "unverified",
	}, lim)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateTenantHostnameIfUnderQuota(ctx, state.CreateTenantHostnameParams{
		SurfaceID: empty.ID, Hostname: "u.example", ChallengeToken: "tok",
	}, lim); err != nil {
		t.Fatal(err)
	}
	if err := issuer.RequestCertForSurface(ctx, empty.ID); err != nil {
		t.Fatalf("RequestCertForSurface (unverified): %v", err)
	}
	got, _ := m.GetTenantSurfaceByID(ctx, empty.ID)
	if got.CertState != state.CertStateFailed {
		t.Fatalf("cert_state = %q, want failed (no verified hostnames)", got.CertState)
	}
	if !strings.Contains(got.CertLastError, "no verified hostnames") {
		t.Errorf("last_error = %q; want 'no verified hostnames' sentinel", got.CertLastError)
	}
}

func TestTenantSurfaceCertIssuer_RejectsSoftDeleted(t *testing.T) {
	issuer, m, surf, ctx := tenantSurfaceIssuerFixture(t)
	if err := m.DeleteTenantSurface(ctx, surf.ID); err != nil {
		t.Fatal(err)
	}
	if err := issuer.RequestCertForSurface(ctx, surf.ID); err != nil {
		t.Fatalf("RequestCertForSurface (deleted): %v", err)
	}
	// Soft-deleted surfaces must not produce any cert state
	// transition (the cert_state was already 'none'; we don't
	// write 'failed' because the customer can't see a deleted
	// surface anyway, and the operator would rather see the
	// clean initial state than a 'failed' that was never
	// actionable).
	got, _ := m.GetTenantSurfaceByID(ctx, surf.ID)
	if got.CertState != state.CertStateNone {
		t.Errorf("cert_state = %q after soft-delete remint; want none (skipped)", got.CertState)
	}
}

func TestTenantSurfaceCertIssuer_RejectsMissingSurface(t *testing.T) {
	issuer, _, _, ctx := tenantSurfaceIssuerFixture(t)
	if err := issuer.RequestCertForSurface(ctx, "missing-surface-id"); err != nil {
		t.Fatalf("RequestCertForSurface (missing): %v", err)
	}
	// ErrNotFound is non-actionable; the engine swallows
	// the error so a deleted-then-notified race doesn't
	// pollute the log.
}

func TestTenantSurfaceCertIssuer_RejectsSharedWildcard(t *testing.T) {
	issuer, m, surf, ctx := tenantSurfaceIssuerFixture(t)
	lim := api.Limits{
		TenantSurfacesAllowed:     true,
		TenantSurfacesPerAccount:  5,
		TenantHostnamesPerSurface: 10,
	}
	// Add a shared_wildcard surface directly via the store,
	// bypassing the apid validator (the schema accepts
	// shared_wildcard for forward compat; the issuer is the
	// last-line reject).
	wild, err := m.CreateTenantSurfaceIfUnderQuota(ctx, state.CreateTenantSurfaceParams{
		AccountID: surf.AccountID, AppID: surf.AppID, Name: "wild",
		CertKind: state.CertKindSharedWildcard,
	}, lim)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateTenantHostnameIfUnderQuota(ctx, state.CreateTenantHostnameParams{
		SurfaceID: wild.ID, Hostname: "w.example", ChallengeToken: "tok",
	}, lim); err != nil {
		t.Fatal(err)
	}
	if err := m.MarkTenantHostnameVerified(ctx, "w.example"); err != nil {
		t.Fatal(err)
	}
	if err := issuer.RequestCertForSurface(ctx, wild.ID); err != nil {
		t.Fatalf("RequestCertForSurface (shared_wildcard): %v", err)
	}
	got, _ := m.GetTenantSurfaceByID(ctx, wild.ID)
	if got.CertState != state.CertStateFailed {
		t.Fatalf("cert_state = %q, want failed", got.CertState)
	}
	if !strings.Contains(got.CertLastError, "shared_wildcard") {
		t.Errorf("last_error = %q; want it to name the rejected kind", got.CertLastError)
	}
}

func TestTenantSurfaceCertIssuer_RejectsEmptySurfaceID(t *testing.T) {
	issuer, _, _, _ := tenantSurfaceIssuerFixture(t)
	if err := issuer.RequestCertForSurface(context.Background(), ""); err == nil {
		t.Fatal("empty surface id = nil; want non-nil error")
	}
}

func TestTenantSurfaceCertIssuer_IncrementsMetrics(t *testing.T) {
	// Verify the closed (result, kind) cartesian is
	// pre-instantiated at boot and the success path ticks
	// one of them. The simplest way to assert a metric
	// increment without a Prometheus testutil dependency is
	// to gather the registry before/after and read the
	// specific counter out of the proto output.
	issuer, m, surf, ctx := tenantSurfaceIssuerFixture(t)
	_ = m
	_ = surf
	before := gatherTenantSurfaceCounter(t, issuer.metrics, "failed", "per_host_san")
	if err := issuer.RequestCertForSurface(ctx, surf.ID); err != nil {
		t.Fatal(err)
	}
	after := gatherTenantSurfaceCounter(t, issuer.metrics, "failed", "per_host_san")
	if after <= before {
		t.Errorf("counter failed/per_host_san = %v after, want > %v before", after, before)
	}
}

// gatherTenantSurfaceCounter reads a single counter from the
// daemon-level registry. Returns 0 when the series is absent
// (the pre-instantiation guard still pins the series at 0; a
// subsequent call after ObserveTenantSurfaceCert returns the
// accumulated value).
func gatherTenantSurfaceCounter(t *testing.T, m *Metrics, result, kind string) float64 {
	t.Helper()
	if m == nil || m.registry == nil {
		return 0
	}
	families, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() != "gateway_tenant_surface_cert_total" {
			continue
		}
		for _, mt := range fam.GetMetric() {
			matchR, matchK := false, false
			for _, l := range mt.GetLabel() {
				if l.GetName() == "result" && l.GetValue() == result {
					matchR = true
				}
				if l.GetName() == "kind" && l.GetValue() == kind {
					matchK = true
				}
			}
			if matchR && matchK {
				return mt.GetCounter().GetValue()
			}
		}
	}
	return 0
}
