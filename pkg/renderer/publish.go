// Package renderer is the manifest → on-disk emitter for issue #911 /
// ADR-110. It consumes a validated `pkg/manifest.Manifest` and
// produces the per-host artifacts that the daemons need to run:
// /etc/faas/<daemon>.toml, /etc/systemd/system/faas-<daemon>.service,
// the faas-cp.slice unit, cgroup v2 subtree_control delegation, and
// the per-daemon PKI leaves under /etc/faas/tls/.
//
// All five outputs are published atomically (tmp + rename) and the
// renderer is idempotent — a second run with identical input
// short-circuits on sha256 match.
//
// Scope (PR-2): the entry point + the five sub-renderers. The
// release-bundle install path lives under `gregale release install`
// (PR-3), not `gregale manifest install` — the renderer is the
// read-side of the cluster ship flow, the install is the write-side.
package renderer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// publishAtomic writes body to path atomically: tmp file in the same
// directory (so the rename is on the same filesystem), chmod 0o644,
// sync, close, then os.Rename over the target. Returns the sha256 hex
// digest of body on success. Caller must NOT pre-create path: the
// renderer is the sole writer for these five outputs.
//
// Idempotent: if the existing file at path has the same content as
// body (compared via sha256), the publish is a no-op and the existing
// file is left untouched. The renderer returns the digest and
// `changed = false` in that case so the caller can stamp
// OutputReport.Action = "skipped" vs "wrote".
//
// tmp file path uses os.CreateTemp in the parent directory so the
// rename is on the same filesystem (no cross-device rename). The
// tmp file is removed on any error path to avoid leaving detritus
// on the host filesystem.
func publishAtomic(path string, body []byte, mode os.FileMode) (digest string, changed bool, err error) {
	digest = sha256Hex(body)

	// Idempotent short-circuit: if the existing file matches body,
	// skip the publish. operator-initiated `--force` re-writes would
	// be a separate flag (deferred).
	if existing, statErr := os.ReadFile(path); statErr == nil {
		if sha256Hex(existing) == digest {
			return digest, false, nil
		}
	} else if !os.IsNotExist(statErr) {
		return "", false, fmt.Errorf("renderer: read existing %s: %w", path, statErr)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false, fmt.Errorf("renderer: mkdir %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".render-*.tmp")
	if err != nil {
		return "", false, fmt.Errorf("renderer: create temp for %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpPath)
		}
	}()

	if err = tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return "", false, fmt.Errorf("renderer: chmod %s: %w", tmpPath, err)
	}
	if _, err = tmp.Write(body); err != nil {
		_ = tmp.Close()
		return "", false, fmt.Errorf("renderer: write %s: %w", tmpPath, err)
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", false, fmt.Errorf("renderer: sync %s: %w", tmpPath, err)
	}
	if err = tmp.Close(); err != nil {
		return "", false, fmt.Errorf("renderer: close %s: %w", tmpPath, err)
	}
	if err = os.Rename(tmpPath, path); err != nil {
		return "", false, fmt.Errorf("renderer: rename %s → %s: %w", tmpPath, path, err)
	}
	return digest, true, nil
}

// sha256Hex returns the lowercase hex sha256 of body. Mirrors
// pkg/releaseinstall.hashFile's encoding for wire compatibility.
func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
