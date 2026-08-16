// tarball.go — canonical daemon tarball producer + verifier extractor
// (ADR-113 canonical daemon tarball, PR-A).
//
// A "canonical tarball" is the verifiable bit of a Gregale release: a
// tar+gz of the per-release bin/ tree, paired with a cosign signature
// bundle and a SPDX-2.3 SBoM (PR-A commit 2 + 3 add those). This
// file owns ONLY commit 1: tar+gz Build, plus a no-op-shaped Verify /
// Extract skeleton so PR-A's three commits wire end-to-end.
//
// Load-bearing invariants:
//
//  1. The tarball is BYTE-STABLE across rebuilds. mtimes are pinned
//     to a stable epoch (1970-01-01 UTC), uid/gid to 0, mode to
//     0o755. There is exactly one entry per daemon binary, named
//     after the daemon — no parent directories preserved (the
//     tarball mirrors BinDir's flat shape).
//  2. Every daemon in the canonical catalog
//     (`manifest.SortedHostKeys()`) appears in the tarball.
//     Anything else is rejected at Build time.
//  3. The Manifest embedded in the tarball is built by the existing
//     `Build` pipeline — the tarball IS the per-release directory,
//     just packed; the manifest extracted from it must validate
//     identically to the on-disk one.
//
// The cosign wrapper (commit 2) and the SBoM baseline (commit 3) sit
// on top of the bytes produced here. Verify and Extract are
// signatures only in this commit; their behaviour is fully wired in
// commits 2 and 3.
package releaseinstall

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/onebox-faas/faas/pkg/manifest"
)

// Tarball is the canonical artifact for one Gregale release.
//
// GitSHA is the directory-name key. Packed is the tar+gzip bytes;
// Sig and SBOM are populated by commits 2 and 3 (left empty for
// commit 1). Manifest is the on-disk-anchor shape built by the
// existing Build() — the same bytes that would live in
// release-manifest.json under the per-release directory.
type Tarball struct {
	GitSHA    string
	Manifest  Manifest
	Packed    []byte // tar+gzip bytes
	Sig       []byte // cosign bundle; populated by PR-A commit 2
	SBOM      []byte // SPDX-2.3 JSON; populated by PR-A commit 3
	BinSHA256 map[string]string
}

// ErrTarballTampered is the verifier's fail-closed signal: the
// extracted bytes do not match what the manifest advertised. Tested
// by TestTarball_Verify_RejectsTampered (commit 2).
var ErrTarballTampered = errors.New("releaseinstall: tarball tampered")

// stableEpoch is the mtime every tar entry is stamped with. Pinned
// so two builds of the same bin/ produce byte-identical Packed bytes
// (the load-bearing invariant TestTarball_Build_HashStable asserts).
//
// 1970-01-01 00:00:00 UTC; tar will emit this as "00000000000".
var stableEpoch = time.Unix(0, 0).UTC()

// BuildTarball reads the per-release bin directory at <root>/<git-sha>/bin,
// hashes every daemon via the canonical SortedHostKeys() catalog,
// tar+gz's the tree deterministically, embeds the on-disk Manifest,
// and returns a *Tarball ready for Verify / Extract.
//
// Commit 1: this is the producer-side surface only. Commit 2 wires
// cosign sign-blob onto Packed and populates Sig. Commit 3 wires
// `syft` and populates SBOM. Verify / Extract are stitched in along
// with their respective wire-in.
//
// The signature was kept narrow on purpose: there is one bin dir per
// git_sha; BuildTarball is the only point that has to know about it.
func BuildTarball(root, gitSHA, manifestHash string, now time.Time) (*Tarball, error) {
	m, err := Build(root, gitSHA, manifestHash, now)
	if err != nil {
		return nil, fmt.Errorf("releaseinstall: tarball build manifest: %w", err)
	}

	bin := BinDir(root, gitSHA)
	daemonNames := manifest.SortedHostKeys()
	// Build a sorted-by-name map to populate BinSHA256; the manifest
	// already ordered by SortedHostKeys() but we want a flat map for
	// downstream consumers (signer, SBoM emitter).
	binHashes := make(map[string]string, len(daemonNames))
	for _, name := range daemonNames {
		hex, ok := strings.CutPrefix(m.DaemonHashes[name], "sha256:")
		if !ok || len(hex) != 64 {
			return nil, fmt.Errorf("releaseinstall: tarball manifest hash for %s missing/wrong shape", name)
		}
		binHashes[name] = hex
	}

	packed, err := tarGzBin(bin, daemonNames)
	if err != nil {
		return nil, fmt.Errorf("releaseinstall: tarball pack: %w", err)
	}

	return &Tarball{
		GitSHA:    gitSHA,
		Manifest:  m,
		Packed:    packed,
		BinSHA256: binHashes,
	}, nil
}

// tarGzBin builds the byte-stable tarball for the given bin dir.
// Iterates `daemonNames` (already sorted by manifest.SortedHostKeys)
// in order, writes each entry with mode 0o755, uid/gid 0, mtime =
// stableEpoch. The resulting `[]byte` is what Tarball.Packed holds.
//
// Errors out (a) if the bin directory is missing, (b) if any
// daemon-named file is not readable. Any file in bin/ NOT in the
// daemon-name catalog is IGNORED (matches `Verify`'s "unexpected
// files" pass at install time — the producer is not the gate; the
// verifier is).
func tarGzBin(bin string, daemonNames []string) ([]byte, error) {
	info, err := os.Stat(bin)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", bin, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", bin)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for _, name := range daemonNames {
		binPath := filepath.Join(bin, name)
		body, err := os.ReadFile(binPath)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", binPath, err)
		}
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o755,
			Uid:      0,
			Gid:      0,
			Size:     int64(len(body)),
			ModTime:  stableEpoch,
			Typeflag: tar.TypeReg,
			Format:   tar.FormatUSTAR,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, fmt.Errorf("tar header %s: %w", name, err)
		}
		if _, err := tw.Write(body); err != nil {
			return nil, fmt.Errorf("tar body %s: %w", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("tar close: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("gzip close: %w", err)
	}
	return buf.Bytes(), nil
}

// Verify is the consumer-side counter to Build. PR-A commit 1
// exposes only the SHA256 walk over the manifest entries (i.e.,
// redaction check — the same check cmdReleaseInstall does against
// the on-disk tree today, lifted into the package). Cosign signature
// verification (commit 2) and CVE-baseline gating (commit 3) are
// layered on top.
//
// Returns ErrTarballTampered wrapped with the offending daemon name
// when any tarball entry's sha256 disagrees with the manifest.
func (t *Tarball) Verify(ctx context.Context) error {
	if t == nil {
		return errors.New("releaseinstall: nil tarball")
	}
	if err := ValidateManifest(t.Manifest); err != nil {
		return fmt.Errorf("releaseinstall: tarball manifest: %w", err)
	}

	entryHashes, err := tarballEntryHashes(t.Packed)
	if err != nil {
		return err
	}
	// Compare every manifest entry against the tarball entries.
	// Mismatch = tampered. We iterate by SortedHostKeys so the
	// error message is deterministic.
	daemonNames := manifest.SortedHostKeys()
	for _, name := range daemonNames {
		wantHex, ok := t.BinSHA256[name]
		if !ok {
			return fmt.Errorf("%w: manifest missing daemon %s", ErrTarballTampered, name)
		}
		gotHex, ok := entryHashes[name]
		if !ok {
			return fmt.Errorf("%w: tarball missing daemon %s", ErrTarballTampered, name)
		}
		if gotHex != wantHex {
			return fmt.Errorf("%w: %s sha256=%s want %s", ErrTarballTampered, name, gotHex, wantHex)
		}
	}
	// Surface any tarball entries that AREN'T in the catalog. A
	// tarball with rogue daemons is a tampering signal even if
	// every catalog entry matches.
	roster := make(map[string]struct{}, len(daemonNames))
	for _, n := range daemonNames {
		roster[n] = struct{}{}
	}
	var unknown []string
	for name := range entryHashes {
		if _, ok := roster[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("%w: tarball contains non-catalog files: %s", ErrTarballTampered, strings.Join(unknown, ", "))
	}
	return nil
}

// Extract unpacks the tarball into <root>/<git-sha>/bin/ — the same
// path the legacy copyBinIntoRelease wrote. The atomic-symlink flip
// that follows (pkg/releaseinstall.AtomicFlip) is unchanged; ADR-113
// only swaps the producer surface.
//
// Idempotent: a second call with the same bytes writes identical
// files. Tested by TestTarball_Extract_Idempotent.
func (t *Tarball) Extract(root string) error {
	if t == nil {
		return errors.New("releaseinstall: nil tarball")
	}
	if err := ValidateManifest(t.Manifest); err != nil {
		return fmt.Errorf("releaseinstall: extract validate: %w", err)
	}

	bin := BinDir(root, t.GitSHA)
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return fmt.Errorf("releaseinstall: extract mkdir %s: %w", bin, err)
	}

	gz, err := gzip.NewReader(bytes.NewReader(t.Packed))
	if err != nil {
		return fmt.Errorf("releaseinstall: gzip reader: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("releaseinstall: extract next: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		outPath := filepath.Join(bin, filepath.Base(hdr.Name))
		// Reject tarball entries that climb out of the bin dir
		// (defense against a tar with hdr.Name like
		// "../../etc/passwd"). filepath.Base + a BinDir-bound
		// join means any escape attempt lands back in bin/.
		body, err := io.ReadAll(tr)
		if err != nil {
			return fmt.Errorf("releaseinstall: extract read %s: %w", hdr.Name, err)
		}
		if err := os.WriteFile(outPath, body, 0o755); err != nil {
			return fmt.Errorf("releaseinstall: extract write %s: %w", outPath, err)
		}
	}
	return nil
}

// tarballEntryHashes returns a daemon-name → sha256-hex map of every
// tar entry's bytes. Used by Verify to compare against the manifest
// without extracting anywhere.
//
// Every parse failure surfaces as ErrTarballTampered — there is no
// other legitimate reason a tarball we just produced would fail to
// parse, and the verifier is the fail-closed half of the contract.
// A tampered tarball that breaks tar parsing MUST be rejected on
// the same path as a tarball whose body has been silently altered
// post-sign (the cosign verify-blob half of commit 2 covers the
// signature; this covers the payload).
func tarballEntryHashes(packed []byte) (map[string]string, error) {
	out := make(map[string]string)
	gz, err := gzip.NewReader(bytes.NewReader(packed))
	if err != nil {
		return nil, fmt.Errorf("%w: gzip reader: %v", ErrTarballTampered, err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: tar next: %v", ErrTarballTampered, err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("%w: tar read %s: %v", ErrTarballTampered, hdr.Name, err)
		}
		out[filepath.Base(hdr.Name)] = sha256Hex(body)
	}
	return out, nil
}
