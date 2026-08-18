package gateway

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// TestCertExpiry_NilGuards pins the fail-closed invariants on
// nil inputs. A misconfigured dashboard endpoint that forwards
// nil must NOT silently return a zero time.
func TestCertExpiry_NilGuards(t *testing.T) {
	if _, err := CertExpiry(context.Background(), nil, "/tmp", "surf"); err == nil {
		t.Error("nil store = nil err; want non-nil")
	}
	if _, err := CertExpiry(context.Background(), &state.MemStore{}, "", "surf"); err == nil {
		t.Error("empty storageDir = nil err; want non-nil")
	}
	if _, err := CertExpiry(context.Background(), &state.MemStore{}, "/tmp", ""); err == nil {
		t.Error("empty surfaceID = nil err; want non-nil")
	}
}

// TestCertExpiry_MissingSurface pins the missing-row branch.
// Returns state.ErrNotFound wrapped so the dashboard renders
// the surface as 'gone' (not 'no cert yet').
func TestCertExpiry_MissingSurface(t *testing.T) {
	store := state.NewMemStore()
	_, err := CertExpiry(context.Background(), store, t.TempDir(), "missing-id")
	if err == nil {
		t.Fatal("missing surface = nil err; want non-nil")
	}
	if !errors.Is(err, state.ErrNotFound) {
		t.Errorf("missing surface err = %v, want wrap of state.ErrNotFound", err)
	}
}

// TestCertExpiry_NoVerifiedHostnames pins the zero-time branch
// for a surface with no verified hostnames. Returns
// (time.Time{}, nil) — the dashboard renders 'no cert yet'
// against the zero time.
func TestCertExpiry_NoVerifiedHostnames(t *testing.T) {
	store := state.NewMemStore()
	ctx := context.Background()
	acct, _ := store.CreateAccount(ctx, "exp@example.com", api.PlanPro)
	app, _ := store.CreateApp(ctx, state.App{AccountID: acct.ID, Slug: "exp", RAMMB: 256, Status: state.AppActive})
	lim := api.Limits{TenantSurfacesAllowed: true, TenantSurfacesPerAccount: 5, TenantHostnamesPerSurface: 10}
	surf, _ := store.CreateTenantSurfaceIfUnderQuota(ctx, state.CreateTenantSurfaceParams{
		AccountID: acct.ID, AppID: app.ID, Name: "exp",
	}, lim)
	na, err := CertExpiry(ctx, store, t.TempDir(), surf.ID)
	if err != nil {
		t.Fatalf("CertExpiry: %v", err)
	}
	if !na.IsZero() {
		t.Errorf("na = %v on empty verified set; want zero", na)
	}
}

// TestCertExpiry_ReadsOnDiskLeaf writes a fake leaf cert via
// crypto/x509.CreateCertificate + pem.Encode, then asserts
// CertExpiry reads back the NotAfter. The leaf path matches
// the certmagic layout (certmagicCertDir + issuerKey +
// primary/primary.crt) so the helper in cert_expiry.go:481
// parses it correctly.
func TestCertExpiry_ReadsOnDiskLeaf(t *testing.T) {
	dir := t.TempDir()
	store := state.NewMemStore()
	ctx := context.Background()
	acct, _ := store.CreateAccount(ctx, "exp2@example.com", api.PlanPro)
	app, _ := store.CreateApp(ctx, state.App{AccountID: acct.ID, Slug: "exp2", RAMMB: 256, Status: state.AppActive})
	lim := api.Limits{TenantSurfacesAllowed: true, TenantSurfacesPerAccount: 5, TenantHostnamesPerSurface: 10}
	surf, _ := store.CreateTenantSurfaceIfUnderQuota(ctx, state.CreateTenantSurfaceParams{
		AccountID: acct.ID, AppID: app.ID, Name: "exp2",
	}, lim)
	if _, err := store.CreateTenantHostnameIfUnderQuota(ctx, state.CreateTenantHostnameParams{
		SurfaceID: surf.ID, Hostname: "a.example", ChallengeToken: "tok",
	}, lim); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkTenantHostnameVerified(ctx, "a.example"); err != nil {
		t.Fatal(err)
	}
	// Build a self-signed leaf with a known NotAfter so the
	// assertion can pin the value.
	notAfter := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	writeFakeCert(t, dir, "a.example", notAfter)
	na, err := CertExpiry(ctx, store, dir, surf.ID)
	if err != nil {
		t.Fatalf("CertExpiry: %v", err)
	}
	if !na.Equal(notAfter) {
		t.Errorf("CertExpiry NotAfter = %v, want %v", na, notAfter)
	}
}

// writeFakeCert creates a self-signed leaf cert + writes it
// under <storageDir>/certificates/<issuerKey>/<primary>/<primary>.crt.
// Used by TestCertExpiry_ReadsOnDiskLeaf.
func writeFakeCert(t *testing.T, storageDir, primary string, notAfter time.Time) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: primary},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{primary},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	leafDir := filepath.Join(storageDir, "certificates", "acme-v02.api.letsencrypt.org-directory", primary)
	if err := os.MkdirAll(leafDir, 0o700); err != nil {
		t.Fatal(err)
	}
	leafPath := filepath.Join(leafDir, primary+".crt")
	if err := os.WriteFile(leafPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}
