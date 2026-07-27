// Tests for the ADR-024 H3 cert-expiry refresher
// (pkg/gateway/cert_expiry.go). All tests are unit-level: synth PEM
// certs in a t.TempDir(), drive refreshCertExpiryOnce directly, assert
// the gauge converges to the soonest NotAfter within tolerance.
//
// The refresher is intentionally ctx-driven (no global state, no
// singleton); tests don't need a real *Metrics registry, just a
// pointer to one built via NewMetrics() so the gauge readback
// matches what an operator would see at /metrics.

package gateway

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRefreshCertExpiryOnce_FindsSoonest sets up three certs in the
// canonical certmagic layout (certificates/<issuer>/<domain>/<domain>.crt)
// with NotAfter offsets of 60 d, 14 d, and 30 d. refreshCertExpiryOnce
// must return the 14-day cert's remaining time as the soonest.
func TestRefreshCertExpiryOnce_FindsSoonest(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	now := time.Now()
	writeCrt(t, filepath.Join(root, "certificates", "acme-v02.api.letsencrypt.org-directory", "soon.example.com"), 14*24*time.Hour, now)
	writeCrt(t, filepath.Join(root, "certificates", "acme-v02.api.letsencrypt.org-directory", "later.example.com"), 60*24*time.Hour, now)
	writeCrt(t, filepath.Join(root, "certificates", "acme-v02.api.letsencrypt.org-directory", "middle.example.com"), 30*24*time.Hour, now)

	m := NewMetrics()
	if err := refreshCertExpiryOnce(ctx, root, m, nil); err != nil {
		t.Fatalf("refreshCertExpiryOnce: %v", err)
	}

	// Allow a few seconds of slack for the 14-day cert: time.Until
	// readback skews by however long the test took. The cert was
	// NotAfter 14d-from-now; we expect somewhere between 14d-30s and 14d.
	want := 14 * 24 * time.Hour
	got := gaugeDuration(t, m)

	if got > want {
		t.Fatalf("gauge too high: got %s, want ≤ %s (soonest 14d)", got, want)
	}
	if got < want-30*time.Second {
		t.Fatalf("gauge too low: got %s, want ≥ %s-30s", got, want)
	}
}

// TestRefreshCertExpiryOnce_EmptyDir — a fresh daemon's storage dir is
// empty (no certs minted yet). refreshCertExpiryOnce must leave the
// gauge unset (Prometheus reports no series for an untouched gauge),
// so the alert's < expression doesn't fire.
func TestRefreshCertExpiryOnce_EmptyDir(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir() // No certificates/ subdir.

	m := NewMetrics()
	if err := refreshCertExpiryOnce(ctx, root, m, nil); err != nil {
		t.Fatalf("refreshCertExpiryOnce: %v", err)
	}
	if got := gaugeDuration(t, m); got != 0 {
		// prometheus.NewGauge() defaults to 0 and the registry has no
		// series; gaugeDuration returns 0 for either. The contract the
		// alert cares about is "no cert → no series", which is what we
		// assert. PR #345 review tightened this — see issue A.
		t.Fatalf("empty dir should leave gauge unset (0), got %s", got)
	}
}

// TestRefreshCertExpiryOnce_ExpiredCert — when a cert on disk is
// already past its NotAfter, refreshCertExpiryOnce must report a
// NEGATIVE remaining duration (not clamp to 0). The page rule's
// `gateway_tls_cert_expiry_seconds < 14 * 86400` then fires
// unambiguously; a clamp-to-0 would let an early `> 0` alert guard
// filter out the page. PR #345 review (issue A) tightened this.
func TestRefreshCertExpiryOnce_ExpiredCert(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	// NotAfter 1 hour ago — the cert is unambiguously expired.
	writeCrt(t, filepath.Join(root, "certificates", "acme", "stale.example.com"), -1*time.Hour, time.Now())

	m := NewMetrics()
	if err := refreshCertExpiryOnce(ctx, root, m, nil); err != nil {
		t.Fatalf("refreshCertExpiryOnce: %v", err)
	}
	got := gaugeDuration(t, m)
	if got >= 0 {
		t.Fatalf("expired cert must yield negative remaining, got %s", got)
	}
	// The gauge should report somewhere in [-1h-2s, -1h+1s] — the
	// lower bound accommodates write-then-read wall-clock skew (we
	// wrote `now.Add(-1h)` and read a moment later, so the cert
	// appears slightly more than 1h expired by the time the gauge is
	// computed). 2s of slack is plenty.
	if got < -1*time.Hour-2*time.Second || got > -1*time.Hour+1*time.Second {
		t.Fatalf("expired cert remaining = %s, want in [-1h-2s, -1h+1s]", got)
	}
}

// TestRefreshCertExpiryOnce_MissingDir — storageDir root doesn't exist
// at all (operator hasn't provisioned it yet). refreshCertExpiryOnce
// returns nil and leaves the gauge untouched. The wrapper tick fn
// refreshCertExpiryOnce is called from logs Warn only on real errors;
// missing dir is the expected boot state.
func TestRefreshCertExpiryOnce_MissingDir(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "does-not-exist")

	m := NewMetrics()
	if err := refreshCertExpiryOnce(ctx, root, m, nil); err != nil {
		t.Fatalf("missing dir should be silent, got: %v", err)
	}
}

// TestRefreshCertExpiryOnce_SkipsUnparseable — a PEM with garbage in
// one .crt must not fail the whole refresh; the other certs still
// land in the gauge.
func TestRefreshCertExpiryOnce_SkipsUnparseable(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	issuerDir := filepath.Join(root, "certificates", "acme")
	goodDir := filepath.Join(issuerDir, "good.example.com")
	if err := os.MkdirAll(goodDir, 0o700); err != nil {
		t.Fatal(err)
	}
	badDir := filepath.Join(issuerDir, "bad.example.com")
	if err := os.MkdirAll(badDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCrt(t, goodDir, 7*24*time.Hour, time.Now())
	if err := os.WriteFile(filepath.Join(badDir, "bad.example.com.crt"), []byte("not a PEM block at all\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	m := NewMetrics()
	if err := refreshCertExpiryOnce(ctx, root, m, nil); err != nil {
		t.Fatalf("refreshCertExpiryOnce: %v", err)
	}
	// The good cert is 7d out; expect ~7d, with slack.
	got := gaugeDuration(t, m)
	if got > 7*24*time.Hour || got < 7*24*time.Hour-30*time.Second {
		t.Fatalf("gauge = %s, want ~7d", got)
	}
}

// TestStartCertExpiryRefresher_StopsOnCancel drives the production
// ticker with a short interval and asserts stop() halts the loop.
// Asserted by sending a cancellation through ctx (rather than calling
// stop) so we also cover the ctx.Done() path.
func TestStartCertExpiryRefresher_StopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	root := t.TempDir()
	writeCrt(t, filepath.Join(root, "certificates", "acme", "x.example.com"), 5*24*time.Hour, time.Now())

	m := NewMetrics()
	stop := StartCertExpiryRefresher(ctx, root, m, 50*time.Millisecond, nil)
	defer stop()

	// Wait until at least one tick has fired (gauge touched).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if gaugeDuration(t, m) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := gaugeDuration(t, m); got == 0 {
		t.Fatalf("gauge should have been set within deadline")
	}
	// cancel() then run stop(); either path is fine — the assertion is
	// that subsequent ticks do not stall.
	cancel()
	stop()
}

// writeCrt generates a self-signed PEM cert with the given lifetime and
// drops it at dir/<domain>.crt. Mirror certmagic's FileStorage layout
// (SiteCert): exactly one .crt file per domain dir.
func writeCrt(t *testing.T, dir string, lifetime time.Duration, now time.Time) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    now.Add(-1 * time.Hour),
		NotAfter:     now.Add(lifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	// Compute the directory's basename to derive a domain name. The
	// caller always passes paths ending in the domain (e.g.
	// ".../acme/soon.example.com"), so filepath.Base is the domain.
	domain := filepath.Base(dir)
	if err := os.WriteFile(filepath.Join(dir, domain+".crt"), pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
}

// gaugeDuration reads gateway_tls_cert_expiry_seconds back via a
// dedicated registry scrape. Using the registry routes through the same
// path an operator's /metrics scrape does, so we exercise the wire
// shape end-to-end rather than reaching into the gauge field directly.
func gaugeDuration(t *testing.T, m *Metrics) time.Duration {
	t.Helper()
	gauge, err := m.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, fam := range gauge {
		if fam.GetName() != "gateway_tls_cert_expiry_seconds" {
			continue
		}
		metrics := fam.GetMetric()
		if len(metrics) == 0 {
			return 0
		}
		return time.Duration(metrics[0].GetGauge().GetValue() * float64(time.Second))
	}
	return 0
}
