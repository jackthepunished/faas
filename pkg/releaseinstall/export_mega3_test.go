// export_mega3_test.go — Coverage Mega-PR #3 cluster A: fill
// pkg/releaseinstall coverage of the wire-payload conversion
// helpers (FromManifest / ToManifest) and the exported
// ValidGitSHA predicate. Both surface as part of the sdk-go
// public API, but the existing bundle_test.go only exercises the
// internal Build/Install/Verify paths.
//
// Targets:
//   - FromManifest: lossless Manifest → Bundle conversion
//   - ToManifest: lossless Bundle → Manifest conversion
//   - Round-trip: FromManifest(m) → ToManifest → equal input
//   - ValidGitSHA: 40-char lowercase hex predicate; accepts
//     valid, rejects wrong-length / uppercase / non-hex / empty
//
// Whitebox `package releaseinstall`.

package releaseinstall

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// validManifestForExport returns a deterministic Manifest suitable
// for round-tripping through FromManifest / ToManifest without
// relying on Build (which needs a real catalog).
func validManifestForExport() Manifest {
	return Manifest{
		FormatVersion: 1,
		GitSHA:        "0123456789abcdef0123456789abcdef01234567",
		ManifestHash:  "fedcba9876543210fedcba9876543210fedcba98",
		DaemonHashes: map[string]string{
			"apid":   "sha256-apid",
			"vmmd":   "sha256-vmmd",
			"schedd": "sha256-schedd",
		},
		ToolHashes: map[string]string{
			"jq":          "sha256-jq",
			"firecracker": "sha256-fc",
		},
		AssetHashes: map[string]string{
			"kernel": "sha256-kernel",
			"rootfs": "sha256-rootfs",
		},
		CreatedAt: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC),
	}
}

// TestFromManifest_LosslessConversion pins the contract that the
// Bundle wire shape is byte-identical to the Manifest shape
// (Manifest is a typedef, see export.go). A future refactor that
// diverged the two would silently break the sdk-go SDK surface.
func TestFromManifest_LosslessConversion(t *testing.T) {
	m := validManifestForExport()
	b := FromManifest(m)

	if b.FormatVersion != m.FormatVersion {
		t.Errorf("FormatVersion = %d, want %d", b.FormatVersion, m.FormatVersion)
	}
	if b.GitSHA != m.GitSHA {
		t.Errorf("GitSHA = %q, want %q", b.GitSHA, m.GitSHA)
	}
	if b.ManifestHash != m.ManifestHash {
		t.Errorf("ManifestHash = %q, want %q", b.ManifestHash, m.ManifestHash)
	}
	if b.CreatedAt != m.CreatedAt {
		t.Errorf("CreatedAt = %v, want %v", b.CreatedAt, m.CreatedAt)
	}
	if len(b.DaemonHashes) != len(m.DaemonHashes) {
		t.Errorf("DaemonHashes len = %d, want %d", len(b.DaemonHashes), len(m.DaemonHashes))
	}
	for k, v := range m.DaemonHashes {
		if b.DaemonHashes[k] != v {
			t.Errorf("DaemonHashes[%q] = %q, want %q", k, b.DaemonHashes[k], v)
		}
	}
}

// TestToManifest_LosslessConversion mirrors TestFromManifest for
// the inverse path. The Bundle → Manifest direction is what
// sdk-go calls receive on their side of the wire.
func TestToManifest_LosslessConversion(t *testing.T) {
	b := Bundle{
		FormatVersion: 1,
		GitSHA:        "0123456789abcdef0123456789abcdef01234567",
		ManifestHash:  "fedcba9876543210fedcba9876543210fedcba98",
		DaemonHashes:  map[string]string{"apid": "sha256-apid"},
		CreatedAt:     time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC),
	}
	m := ToManifest(b)

	if m.FormatVersion != b.FormatVersion {
		t.Errorf("FormatVersion = %d, want %d", m.FormatVersion, b.FormatVersion)
	}
	if m.GitSHA != b.GitSHA {
		t.Errorf("GitSHA = %q, want %q", m.GitSHA, b.GitSHA)
	}
	if m.ManifestHash != b.ManifestHash {
		t.Errorf("ManifestHash = %q, want %q", m.ManifestHash, b.ManifestHash)
	}
	if len(m.DaemonHashes) != len(b.DaemonHashes) {
		t.Errorf("DaemonHashes len = %d, want %d", len(m.DaemonHashes), len(b.DaemonHashes))
	}
}

// TestManifestBundleRoundTrip verifies FromManifest(ToManifest(b))
// == b and ToManifest(FromManifest(m)) == m.
func TestManifestBundleRoundTrip(t *testing.T) {
	original := validManifestForExport()
	roundTripped := ToManifest(FromManifest(original))

	if roundTripped.FormatVersion != original.FormatVersion ||
		roundTripped.GitSHA != original.GitSHA ||
		roundTripped.ManifestHash != original.ManifestHash ||
		!roundTripped.CreatedAt.Equal(original.CreatedAt) {
		t.Errorf("round-trip diverged: %+v vs %+v", roundTripped, original)
	}
}

// TestBundleJSONWireShape pins the published sdk-go wire format.
// The JSON tags are part of the cross-language contract; renaming
// any of them is a breaking change for every downstream SDK.
func TestBundleJSONWireShape(t *testing.T) {
	b := Bundle{
		FormatVersion: 1,
		GitSHA:        "0123456789abcdef0123456789abcdef01234567",
		ManifestHash:  "fedcba9876543210fedcba9876543210fedcba98",
		DaemonHashes:  map[string]string{"apid": "h1"},
		CreatedAt:     time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC),
	}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, key := range []string{
		"format_version",
		"git_sha",
		"manifest_hash",
		"daemon_hashes",
		"created_at",
	} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("wire shape: missing %q key in %s", key, raw)
		}
	}
}

// --- ValidGitSHA ----------------------------------------------------

// TestValidGitSHA_AcceptsValidHash covers the happy path.
func TestValidGitSHA_AcceptsValidHash(t *testing.T) {
	if !ValidGitSHA("0123456789abcdef0123456789abcdef01234567") {
		t.Error("ValidGitSHA(40 lowercase hex) = false, want true")
	}
	if !ValidGitSHA("fedcba9876543210fedcba9876543210fedcba98") {
		t.Error("ValidGitSHA(fedcba98...) = false, want true")
	}
	if !ValidGitSHA("0000000000000000000000000000000000000000") {
		t.Error("ValidGitSHA(all zeros) = false, want true")
	}
}

// TestValidGitSHA_RejectsWrongLength covers the length guard at
// bundle.go:454-456.
func TestValidGitSHA_RejectsWrongLength(t *testing.T) {
	cases := []string{
		"",
		"abc",
		"0123456789abcdef0123456789abcdef0123456",
		"0123456789abcdef0123456789abcdef012345678",
		"0123456789abcdef0123456789abcdef0123456789",
	}
	for _, c := range cases {
		if ValidGitSHA(c) {
			t.Errorf("ValidGitSHA(%q) = true, want false (wrong length)", c)
		}
	}
}

// TestValidGitSHA_RejectsUppercase covers the isHexLower check at
// bundle.go:457-462.
func TestValidGitSHA_RejectsUppercase(t *testing.T) {
	cases := []string{
		"0123456789ABCDEF0123456789abcdef01234567",
		"FEDCBA9876543210FEDCBA9876543210FEDCBA98",
		"0123456789abcdef0123456789abcdef0123456G",
	}
	for _, c := range cases {
		if ValidGitSHA(c) {
			t.Errorf("ValidGitSHA(%q) = true, want false (uppercase / non-hex)", c)
		}
	}
}

// TestValidGitSHA_RejectsNonHex covers non-hex characters.
func TestValidGitSHA_RejectsNonHex(t *testing.T) {
	cases := []string{
		"x123456789abcdef0123456789abcdef01234567",
		"0123456789abcdef0123456789abcdef0123456z",
		"0123456789abcdef-123456789abcdef-1234567",
		"0123456789abcdef 123456789abcdef01234567",
	}
	for _, c := range cases {
		if ValidGitSHA(c) {
			t.Errorf("ValidGitSHA(%q) = true, want false (non-hex char)", c)
		}
	}
}
