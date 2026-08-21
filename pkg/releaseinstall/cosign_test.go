package releaseinstall

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type testBundleCertificate struct {
	RawBytes string `json:"rawBytes"`
}

type testBundleChain struct {
	Certificates []testBundleCertificate `json:"certificates"`
}

type testBundleVerificationMaterial struct {
	X509CertificateChain testBundleChain `json:"x509CertificateChain"`
}

type testBundle struct {
	VerificationMaterial testBundleVerificationMaterial `json:"verificationMaterial"`
}

func testCosignCertificate(t *testing.T, identity string) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	uri, err := url.Parse(identity)
	if err != nil {
		t.Fatalf("parse identity: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		URIs:         []*url.URL{uri},
		NotBefore:    time.Unix(0, 0).UTC(),
		NotAfter:     time.Unix(4_000_000_000, 0).UTC(),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return der
}

func TestBundleCertificateIdentity_NewBundleFormat(t *testing.T) {
	identity := "https://github.com/poyrazK/faas/.github/workflows/release.yml@refs/tags/v1.2.3"
	body, err := json.Marshal(testBundle{VerificationMaterial: testBundleVerificationMaterial{
		X509CertificateChain: testBundleChain{Certificates: []testBundleCertificate{{
			RawBytes: base64.StdEncoding.EncodeToString(testCosignCertificate(t, identity)),
		}}},
	}})
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	path := filepath.Join(t.TempDir(), "release.cosign.bundle")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	got, err := bundleCertificateIdentity(path)
	if err != nil {
		t.Fatalf("bundleCertificateIdentity: %v", err)
	}
	if got != identity {
		t.Fatalf("identity = %q, want %q", got, identity)
	}
}

func TestDefaultGitHubOIDC_PinsReleaseTagWorkflow(t *testing.T) {
	cfg := DefaultGitHubOIDC()
	cases := []struct {
		identity string
		want     bool
	}{
		{identity: "https://github.com/poyrazK/faas/.github/workflows/release.yml@refs/tags/v1.2.3", want: true},
		{identity: "https://github.com/poyrazK/faas/.github/workflows/build-sha256.yml@refs/tags/v1.2.3", want: false},
		{identity: "https://github.com/poyrazK/faas/.github/workflows/release.yml@refs/heads/main", want: false},
	}
	for _, identity := range cases {
		if got := cfg.IdentityRegexp.MatchString(identity.identity); got != identity.want {
			t.Fatalf("identity regexp classified %q as %t, want %t", identity.identity, got, identity.want)
		}
	}
}

func TestBundleCertificateIdentity_LegacyPEM(t *testing.T) {
	identity := "https://github.com/poyrazK/faas/.github/workflows/release.yml@refs/tags/v2.0.0"
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: testCosignCertificate(t, identity)})
	body, err := json.Marshal(struct {
		Cert string `json:"cert"`
	}{Cert: base64.StdEncoding.EncodeToString(certPEM)})
	if err != nil {
		t.Fatalf("marshal legacy bundle: %v", err)
	}
	path := filepath.Join(t.TempDir(), "release.cosign.bundle")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	got, err := bundleCertificateIdentity(path)
	if err != nil {
		t.Fatalf("bundleCertificateIdentity: %v", err)
	}
	if got != identity {
		t.Fatalf("identity = %q, want %q", got, identity)
	}
}
