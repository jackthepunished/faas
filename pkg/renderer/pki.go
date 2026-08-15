package renderer

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/onebox-faas/faas/pkg/pki"
)

// renderPKI ensures the per-host PKI leaves exist under rootDir. The
// leaf set is filtered by hostRole via `pkg/pki.RolesForBox` — a
// control-plane box doesn't get vmmd's leaves, a compute-only box
// doesn't get apid's, etc. Each leaf is issued via
// `pkg/pki.EnsureLeaf(..., false)` which is idempotent: leaves whose
// NotAfter is more than ReissueThreshold away are skipped
// (ErrLeafNotExpiringSoon).
//
// The CA is materialised via `pkg/pki.EnsureCA(rootDir, false)` —
// the renderer's job is to make the CA + leaves exist on disk. The
// operator's job is to seed the CA before the first render (PR-X
// secrets init owns the production CA on a clean host; PR-2's
// renderer is the canary that ships the same CA on a freshly-
// bootstrapped dev/lima box).
//
// Returns the per-role paths that were reissued (skipped leaves are
// not counted in the OutputReport) so the caller can stamp the
// RenderReport.
func renderPKI(rootDir, hostRole string) ([]string, error) {
	// EnsureCA is idempotent (force=false → don't re-issue if a
	// fresh CA already exists). On a fresh box this generates the
	// CA cert + key at <rootDir>/ca/{ca.crt,ca.key}. The mode
	// pinning (0o444 / 0o400) is owned by EnsureCA.
	caCert, caKey, err := pki.EnsureCA(rootDir, false)
	if err != nil {
		return nil, fmt.Errorf("renderer: pki: ensure CA: %w", err)
	}

	// Per-role leaves. RolesForBox returns an empty slice for
	// unknown roles (fail-closed — the operator sees "0 leaves
	// written" rather than a silent full-fleet issuance). PR-2
	// surfaces this as a render error.
	roles := pki.RolesForBox(hostRole)
	if len(roles) == 0 {
		return nil, fmt.Errorf("renderer: pki: no roles for host role %q (known: control-plane|compute-only|single-box)", hostRole)
	}

	var issued []string
	for _, role := range roles {
		err := pki.EnsureLeaf(rootDir, role, caCert, caKey, false)
		if err != nil {
			if errors.Is(err, pki.ErrLeafNotExpiringSoon) {
				// Idempotent: leaf already exists and is fresh.
				continue
			}
			return nil, fmt.Errorf("renderer: pki: ensure leaf %s/%s: %w", role.Directory, role.Filename, err)
		}
		certPath, keyPath := pki.LeafPaths(rootDir, role)
		issued = append(issued, filepath.ToSlash(certPath))
		issued = append(issued, filepath.ToSlash(keyPath))
	}
	return issued, nil
}
