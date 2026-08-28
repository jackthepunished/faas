package main

import (
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

	"github.com/onebox-faas/faas/pkg/pki"
)

func writeComputeNodeTestCertificate(t *testing.T, path string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: new(big.Int).SetInt64(1),
		Subject:      pkix.Name{CommonName: "vmmd.faas"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0o444); err != nil {
		t.Fatalf("write certificate: %v", err)
	}
	return pemBytes
}

func TestLoadComputeNodeCertificateAttestationUsesVMMDLeaf(t *testing.T) {
	root := t.TempDir()
	certPath := filepath.Join(root, "vmmd", "server.crt")
	if err := os.MkdirAll(filepath.Dir(certPath), 0o755); err != nil {
		t.Fatalf("mkdir certificate directory: %v", err)
	}
	wantPEM := writeComputeNodeTestCertificate(t, certPath)
	t.Setenv("FAAS_TLS_DIR", root)

	gotPEM, gotFingerprint, err := loadComputeNodeCertificateAttestation()
	if err != nil {
		t.Fatalf("load attestation: %v", err)
	}
	if string(gotPEM) != string(wantPEM) {
		t.Fatal("attestation returned certificate bytes different from vmmd/server.crt")
	}
	wantFingerprint, err := pki.LoadCertificateFingerprint(certPath)
	if err != nil {
		t.Fatalf("load expected fingerprint: %v", err)
	}
	if gotFingerprint != wantFingerprint {
		t.Fatalf("fingerprint = %q, want %q", gotFingerprint, wantFingerprint)
	}
}
