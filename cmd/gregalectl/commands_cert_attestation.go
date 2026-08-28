package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/onebox-faas/faas/pkg/pki"
)

// computeNodeCertificatePath is the single operator-side source of truth for
// the certificate that identifies a compute node. It must stay aligned with
// cmd/vmmd/register.go: vmmd presents this leaf to the control plane and
// stamps its canonical DER fingerprint in compute_nodes.cert_fingerprint.
//
// FAAS_TLS_DIR is intentionally supported for tests and non-standard
// installations; production uses pki.DefaultRootDir.
func computeNodeCertificatePath() string {
	root := pki.DefaultRootDir
	if value := strings.TrimSpace(os.Getenv("FAAS_TLS_DIR")); value != "" {
		root = value
	}
	return filepath.Join(root, "vmmd", "server.crt")
}

// loadComputeNodeCertificateAttestation returns the public vmmd leaf and the
// canonical fingerprint stored in compute_nodes. The fingerprint is derived
// by pkg/pki.LoadCertificateFingerprint so secrets init, doctor, and vmmd
// cannot silently disagree about whether a node certificate changed.
func loadComputeNodeCertificateAttestation() ([]byte, string, error) {
	path := computeNodeCertificatePath()
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read compute node certificate %q: %w", path, err)
	}
	fingerprint, err := pki.LoadCertificateFingerprint(path)
	if err != nil {
		return nil, "", err
	}
	return pemBytes, fingerprint, nil
}
