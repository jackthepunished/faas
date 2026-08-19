// Public types exposed to sdk-go. Mirrors the structure of the
// Manifest + row types so the JSON wire shape is byte-identical
// to what the on-disk manifest.json + the release_bundles table
// row encode.
//
// These are intentionally a thin pass-through — the package's
// semantics (Build, Install, store) are internal; the export
// surface is just the wire payload.
package releaseinstall

import "time"

// Bundle is the wire shape for a release bundle. Matches the
// Manifest JSON tag-for-tag.
type Bundle struct {
	FormatVersion int               `json:"format_version"`
	GitSHA        string            `json:"git_sha"`
	ManifestHash  string            `json:"manifest_hash"`
	DaemonHashes  map[string]string `json:"daemon_hashes"`
	ToolHashes    map[string]string `json:"tool_hashes,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
	Signature     string            `json:"signature,omitempty"`
}

// FromManifest converts the internal Manifest to the wire Bundle.
func FromManifest(m Manifest) Bundle {
	return Bundle(m)
}

// ToManifest converts the wire Bundle back to the internal Manifest.
func ToManifest(b Bundle) Manifest {
	return Manifest(b)
}

// BundleRow is the wire shape for a release_bundles row.
//
// Mirrors the migration 00272 schema: id (uuid), git_sha, manifest_hash,
// daemon_hashes (jsonb), created_at, applied_at. The `id` is the
// surrogate key from gen_random_uuid(); CreateAt/AppliedAt are
// nullable AppliedAt to preserve the "unapplied" state.
type BundleRow struct {
	ID           string            `json:"id"`
	GitSHA       string            `json:"git_sha"`
	ManifestHash string            `json:"manifest_hash"`
	DaemonHashes map[string]string `json:"daemon_hashes"`
	CreatedAt    time.Time         `json:"created_at"`
	AppliedAt    *time.Time        `json:"applied_at,omitempty"`
}
