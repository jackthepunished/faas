package renderer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
)

// hashManifestFile returns the lowercase hex sha256 of the file at
// path. Used by the renderer to compute the manifest_hash stamp the
// idempotent short-circuit compares against. Mirrors the formatting
// of pkg/releaseinstall.hashFile so the cross-package wire format
// (manifest_hash on disk, manifest_hash in release_bundles) is
// consistent.
func hashManifestFile(path string) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("renderer: read %s: %w", path, err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}
