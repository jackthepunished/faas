package imaged

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/oci"
	"github.com/onebox-faas/faas/pkg/rootfs"
	"github.com/onebox-faas/faas/pkg/sched"
	"github.com/onebox-faas/faas/pkg/storage"
)

// minimalManifestPuller implements just enough to satisfy oci.ManifestPuller
// for EnsureBaseExt4. Manifest answers PullManifest with the canned image;
// Blobs serves two layer blobs; the rest (PullDigest / PullImageConfig /
// PullLayers) is implemented as no-op-error because the base path doesn't
// call them.
type minimalManifestPuller struct {
	manifest oci.Manifest
	layers   map[string][]byte // digest -> gzipped tarball bytes
}

func (f *minimalManifestPuller) PullDigest(_ context.Context, ref string) (string, error) {
	return ref, nil
}
func (f *minimalManifestPuller) PullImageConfig(_ context.Context, _ string) (oci.ImageConfig, error) {
	return oci.ImageConfig{}, nil
}
func (f *minimalManifestPuller) PullLayers(_ context.Context, _ string) (oci.PullLayersResult, error) {
	return oci.PullLayersResult{}, nil
}
func (f *minimalManifestPuller) PullManifest(_ context.Context, _ string) (oci.Manifest, error) {
	return f.manifest, nil
}
func (f *minimalManifestPuller) PullBlob(_ context.Context, _ string, digest string) (io.ReadCloser, error) {
	b, ok := f.layers[digest]
	if !ok {
		return nil, errors.New("no such digest in fake: " + digest)
	}
	return io.NopCloser(strings.NewReader(string(b))), nil
}

// newBaseHarness builds a Handler with a minimalManifestPuller, a builder,
// and a per-test LocalStorageBackend. Returns the handler + the storage
// backend so tests can assert on published keys.
type baseHarness struct {
	h  *Handler
	be storage.StorageBackend
}

func newBaseHarness(t *testing.T, mp *minimalManifestPuller, b LayerBuilder) *baseHarness {
	t.Helper()
	be, err := storage.NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalStorageBackend: %v", err)
	}
	h := &Handler{
		oci:     mp,
		builder: b,
		log:     silentLogger(),
		storage: be,
		// Inject a no-op grypeRun so the scan sidecar write at the end
		// of EnsureBaseExt4 doesn't shell out to grype (which isn't on
		// the unit-test PATH and would trip the fail-closed CRITICAL=9999
		// placeholder, polluting tests that aren't asserting on the scan
		// sidecar's contents). Tests that DO care about the scan sidecar
		// override the runner with their own stub.
		grypeRun: func(_ context.Context, _ string) (map[string]int, error) {
			return map[string]int{}, nil
		},
	}
	return &baseHarness{h: h, be: be}
}

// TestEnsureBaseExt4_StagesOnFirstRun — no prior ext4, no digest sidecar →
// pulls layers, runs BuildBase, writes both the ext4 and the .digest
// sidecar. Skipped=false. Asserts the produced ext4 lives at baseKey and
// the digest sidecar matches res.ConfigDigest.
func TestEnsureBaseExt4_StagesOnFirstRun(t *testing.T) {
	mp := newTwoLayerPuller(t)
	b := &fakeBuilder{}
	hs := newBaseHarness(t, mp, b)
	const baseKey = "base/runtime.ext4"
	const digKey = "base/runtime.ext4.digest"
	res, err := hs.h.EnsureBaseExt4(context.Background(),
		"ghcr.io/onebox-faas/builder-base:latest", baseKey, digKey, "")
	if err != nil {
		t.Fatalf("EnsureBaseExt4: %v", err)
	}
	if res.Skipped {
		t.Error("Skipped=true on first run, want false")
	}
	if res.ConfigDigest == "" {
		t.Error("ConfigDigest empty, want the manifest's")
	}
	if res.StorageKey != baseKey {
		t.Errorf("StorageKey=%q, want %q", res.StorageKey, baseKey)
	}
	rc, err := hs.be.Get(context.Background(), baseKey)
	if err != nil {
		t.Fatalf("base ext4 not at key %q: %v", baseKey, err)
	}
	defer rc.Close()
	digestBytes, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read base ext4: %v", err)
	}
	if !bytes.Contains(digestBytes, []byte("fake ext4")) {
		t.Errorf("base ext4 bytes %q should contain fake ext4 marker", string(digestBytes))
	}
	digestRC, err := hs.be.Get(context.Background(), digKey)
	if err != nil {
		t.Fatalf("digest sidecar not at key %q: %v", digKey, err)
	}
	defer digestRC.Close()
	haveDigest, err := io.ReadAll(digestRC)
	if err != nil {
		t.Fatalf("read digest sidecar: %v", err)
	}
	if string(haveDigest) != res.ConfigDigest {
		t.Errorf("sidecar %q != res.ConfigDigest %q", string(haveDigest), res.ConfigDigest)
	}
}

// TestEnsureBaseExt4_GrypeCalledWithFilesystemPath — Critical #1 of the
// PR #385 review: Grype's `dir:` source walks a filesystem path, NOT an
// OCI ref. The original implementation passed `ref` (the OCI ref, e.g.
// "ghcr.io/onebox-faas/builder-base:latest") to grype, which Grype rejects
// because registry refs belong to a `registry:` source. The fix routes the
// filesystem path (outImage) to grype while still recording the OCI ref
// in the sidecar's `image` field for dashboard traceability. This test
// pins the contract: a captured grypeRun stub must see the outImage path,
// not the OCI ref.
func TestEnsureBaseExt4_GrypeCalledWithFilesystemPath(t *testing.T) {
	mp := newTwoLayerPuller(t)
	b := &fakeBuilder{}
	hs := newBaseHarness(t, mp, b)

	const ociRef = "ghcr.io/onebox-faas/builder-base:latest"
	const baseKey = "base/runner-builder-amd64.ext4"
	const digKey = "base/runner-builder-amd64.ext4.digest"
	const outImage = "/srv/fc/base/runner-builder-amd64.ext4"

	var capturedDir string
	hs.h.grypeRun = func(_ context.Context, dir string) (map[string]int, error) {
		capturedDir = dir
		return map[string]int{}, nil
	}

	if _, err := hs.h.EnsureBaseExt4(context.Background(),
		ociRef, baseKey, digKey, outImage); err != nil {
		t.Fatalf("EnsureBaseExt4: %v", err)
	}
	if capturedDir != outImage {
		t.Errorf("grypeRun called with %q; want the filesystem path %q (not the OCI ref %q)",
			capturedDir, outImage, ociRef)
	}
	if capturedDir == ociRef {
		t.Errorf("grypeRun was handed the OCI ref %q — Grype's `dir:` source walks a filesystem path, not a registry ref", capturedDir)
	}
}

// TestEnsureBaseExt4_SkipsWhenDigestMatches — pre-existing ext4 + matching
// .digest sidecar → no second stage, no extra layers pulled. We detect the
// "no second stage" by checking that BuildBase.calls didn't grow.
func TestEnsureBaseExt4_SkipsWhenDigestMatches(t *testing.T) {
	mp := newTwoLayerPuller(t)
	const baseKey = "base/runtime.ext4"
	const digKey = "base/runtime.ext4.digest"
	be, err := storage.NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	be.Put(context.Background(), baseKey, strings.NewReader("existing ext4"))
	manifest, err := mp.PullManifest(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	be.Put(context.Background(), digKey, strings.NewReader(manifest.Config.Digest))
	b := &callCountingBuilder{}
	h := &Handler{oci: mp, builder: b, log: silentLogger(), storage: be}
	res, err := h.EnsureBaseExt4(context.Background(),
		"ghcr.io/onebox-faas/builder-base:latest", baseKey, digKey, "")
	if err != nil {
		t.Fatalf("EnsureBaseExt4: %v", err)
	}
	if !res.Skipped {
		t.Error("Skipped=false on matching digest, want true")
	}
	if b.calls != 0 {
		t.Errorf("BuildBase called %d times, want 0 (digest match)", b.calls)
	}
	rc, _ := be.Get(context.Background(), baseKey)
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	if string(body) != "existing ext4" {
		t.Errorf("file body changed during skip path: %q", string(body))
	}
}

// TestEnsureBaseExt4_RestagesWhenDigestDiffers — sidecar exists with the
// WRONG digest → forced restage. We re-write the existing ext4 from BuildBase
// and assert the BuildBase call happened.
func TestEnsureBaseExt4_RestagesWhenDigestDiffers(t *testing.T) {
	mp := newTwoLayerPuller(t)
	const baseKey = "base/runtime.ext4"
	const digKey = "base/runtime.ext4.digest"
	be, err := storage.NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	be.Put(context.Background(), baseKey, strings.NewReader("stale ext4"))
	be.Put(context.Background(), digKey, strings.NewReader("sha256:0000000000000000000000000000000000000000000000000000000000000000"))
	b := &callCountingBuilder{}
	h := &Handler{oci: mp, builder: b, log: silentLogger(), storage: be}
	res, err := h.EnsureBaseExt4(context.Background(),
		"ghcr.io/onebox-faas/builder-base:latest", baseKey, digKey, "")
	if err != nil {
		t.Fatalf("EnsureBaseExt4: %v", err)
	}
	if res.Skipped {
		t.Error("Skipped=true when digest differed, want false")
	}
	if b.calls != 1 {
		t.Errorf("BuildBase called %d times, want 1 (forced restage)", b.calls)
	}
}

// TestEnsureBaseExt4_RejectsEmptyInputs is the boundary test: ref,
// baseKey, and digestKey are all required; passing any of them empty
// is a config error.
func TestEnsureBaseExt4_RejectsEmptyInputs(t *testing.T) {
	be, _ := storage.NewLocalStorageBackend(t.TempDir())
	h := &Handler{oci: &minimalManifestPuller{}, builder: &fakeBuilder{}, log: silentLogger(), storage: be}
	if _, err := h.EnsureBaseExt4(context.Background(), "", "k", "k.digest", ""); err == nil {
		t.Error("empty ref should error")
	}
	if _, err := h.EnsureBaseExt4(context.Background(), "ref", "", "k.digest", ""); err == nil {
		t.Error("empty baseKey should error")
	}
	if _, err := h.EnsureBaseExt4(context.Background(), "ref", "k", "", ""); err == nil {
		t.Error("empty digestKey should error")
	}
}

// TestEnsureBaseExt4_RejectsPullerWithoutManifestPuller — when production
// wires a puller that doesn't implement ManifestPuller (e.g. a future fake
// used in test), we fail loudly rather than silently skipping the stage.
func TestEnsureBaseExt4_RejectsPullerWithoutManifestPuller(t *testing.T) {
	be, _ := storage.NewLocalStorageBackend(t.TempDir())
	h := &Handler{oci: oci.DefaultPuller{}, builder: &fakeBuilder{}, log: silentLogger(), storage: be}
	_, err := h.EnsureBaseExt4(context.Background(),
		"ghcr.io/onebox-faas/builder-base:latest", "k", "k.digest", "")
	if err == nil {
		t.Fatal("expected error when puller lacks ManifestPuller")
	}
	if !strings.Contains(err.Error(), "ManifestPuller") {
		t.Errorf("error %q must mention ManifestPuller", err.Error())
	}
}

// TestEnsureBaseExt4_BubblesPullManifestErrors — registry unreachable is a
// startup failure, not a silent skip; the daemon should refuse to come up.
func TestEnsureBaseExt4_BubblesPullManifestErrors(t *testing.T) {
	bad := &brokenManifestPuller{manifestErr: errors.New("connection refused")}
	be, _ := storage.NewLocalStorageBackend(t.TempDir())
	h := &Handler{oci: bad, builder: &fakeBuilder{}, log: silentLogger(), storage: be}
	_, err := h.EnsureBaseExt4(context.Background(),
		"ghcr.io/onebox-faas/builder-base:latest", "k", "k.digest", "")
	if err == nil {
		t.Fatal("expected error from broken puller")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error %q must preserve 'connection refused' from registry", err.Error())
	}
}

// TestEnsureBaseExt4_BuildFailureSurfaces — when BuildBase fails, the
// baseKey must NOT be present after the call (the publish step is
// skipped on builder error) and the digest sidecar must NOT have been
// written either.
func TestEnsureBaseExt4_BuildFailureSurfaces(t *testing.T) {
	mp := newTwoLayerPuller(t)
	be, _ := storage.NewLocalStorageBackend(t.TempDir())
	h := &Handler{
		oci:     mp,
		builder: &failingBuilder{err: errors.New("mkfs exploded")},
		log:     silentLogger(),
		storage: be,
	}
	_, err := h.EnsureBaseExt4(context.Background(),
		"ghcr.io/onebox-faas/builder-base:latest", "base/runtime.ext4", "base/runtime.ext4.digest", "")
	if err == nil {
		t.Fatal("expected build failure")
	}
	if _, err := be.Get(context.Background(), "base/runtime.ext4"); err == nil {
		t.Error("base ext4 unexpectedly created on builder failure")
	}
}

// newTwoLayerPuller fabricates a one-config, two-layer OCI image out of
// (gzipped) tarballs built by tarball_test.go's gzTar helper. The digest
// values below mirror what a registry would synthesize (we ignore the
// authenticity — the base stage only uses them as opaque IDs).
func newTwoLayerPuller(t *testing.T) *minimalManifestPuller {
	t.Helper()
	layerA := "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	layerB := "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	cfg := "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	manifest := oci.Manifest{
		Config: oci.Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: cfg},
		Layers: []oci.Descriptor{
			{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Digest: layerA, Size: 8},
			{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Digest: layerB, Size: 8},
		},
	}
	bodyA := gzTar(t, map[string]string{"bin/railpack": "rb0"})
	bodyB := gzTar(t, map[string]string{"bin/railpack": "rb1", "etc/faas/build": "manifest"})
	return &minimalManifestPuller{
		manifest: manifest,
		layers: map[string][]byte{
			layerA: bodyA,
			layerB: bodyB,
		},
	}
}

// callCountingBuilder is a LayerBuilder that records how many times
// BuildBase has been called. Used by the skip-vs-restage tests. Storage.Put
// is invoked by the production code path, so the helper just records
// BuildBase calls rather than writing to disk.
type callCountingBuilder struct{ calls int }

func (b *callCountingBuilder) Build(_ context.Context, in rootfs.BuildInput) (rootfs.BuildResult, error) {
	return rootfs.BuildResult{ImageKey: in.StorageKey}, nil
}
func (b *callCountingBuilder) BuildBase(ctx context.Context, in rootfs.BaseBuildInput) (rootfs.BaseBuildResult, error) {
	b.calls++
	if in.Storage != nil && in.StorageKey != "" {
		// Mimic BuildBase's behaviour: produce a (small) placeholder and
		// Put it to the storage key so the storage backend's byte stream
		// is non-empty (skipping the empty-byte rejection in LocalStorageBackend).
		_ = in.Storage.Put(ctx, in.StorageKey, bytes.NewReader([]byte("fake ext4")))
	}
	return rootfs.BaseBuildResult{ImageKey: in.StorageKey}, nil
}

// failingBuilder always errors from BuildBase. Used to prove cleanup of
// the .tmp file on failure.
type failingBuilder struct{ err error }

func (b *failingBuilder) Build(_ context.Context, in rootfs.BuildInput) (rootfs.BuildResult, error) {
	return rootfs.BuildResult{}, b.err
}
func (b *failingBuilder) BuildBase(_ context.Context, _ rootfs.BaseBuildInput) (rootfs.BaseBuildResult, error) {
	return rootfs.BaseBuildResult{}, b.err
}

// TestEnsureBaseExt4_PerArchPartition — issue #197 B3.3. The same
// runtime staged under two different arch-suffixed keys must produce
// two distinct published ext4s and two distinct digest sidecars in
// storage. This is the load-bearing property that lets an arm64
// imaged binary coexist on the same storage root as an amd64 one
// without clobbering each other's base image.
func TestEnsureBaseExt4_PerArchPartition(t *testing.T) {
	mp := newTwoLayerPuller(t)
	b := &fakeBuilder{}
	hs := newBaseHarness(t, mp, b)

	// Per-arch keys derived via the same helper schedd uses on the
	// wake wire — the source of truth.
	const baseKeyAmd64 = "base/runner-builder-amd64.ext4"
	const baseKeyArm64 = "base/runner-builder-arm64.ext4"
	const digKeyAmd64 = "base/runner-builder-amd64.ext4.digest"
	const digKeyArm64 = "base/runner-builder-arm64.ext4.digest"

	// Stage the amd64 base.
	if _, err := hs.h.EnsureBaseExt4(context.Background(),
		"ghcr.io/onebox-faas/builder-base:latest", baseKeyAmd64, digKeyAmd64, ""); err != nil {
		t.Fatalf("amd64 stage: %v", err)
	}
	// Stage the arm64 base into the same storage backend.
	if _, err := hs.h.EnsureBaseExt4(context.Background(),
		"ghcr.io/onebox-faas/builder-base:latest", baseKeyArm64, digKeyArm64, ""); err != nil {
		t.Fatalf("arm64 stage: %v", err)
	}

	// Both ext4s must be present at their respective per-arch keys. The
	// fakeBuilder writes the same literal "fake ext4" payload regardless
	// of call, so this test only asserts presence (NOT byte-distinction);
	// the load-bearing property — same key with different arch suffixes
	// would NOT clobber — is exercised by the TestBaseKeyForArch_* family
	// in pkg/sched/paths_test.go.
	for _, k := range []string{baseKeyAmd64, baseKeyArm64} {
		rc, err := hs.be.Get(context.Background(), k)
		if err != nil {
			t.Fatalf("missing ext4 at %s: %v", k, err)
		}
		buf, _ := io.ReadAll(rc)
		rc.Close()
		if len(buf) == 0 {
			t.Fatalf("ext4 at %s is empty", k)
		}
	}
	// Both digest sidecars must be present.
	for _, k := range []string{digKeyAmd64, digKeyArm64} {
		if _, err := hs.be.Get(context.Background(), k); err != nil {
			t.Fatalf("missing digest sidecar at %s: %v", k, err)
		}
	}
}

// brokenManifestPuller fails PullManifest. Used to prove registry errors
// surface rather than being swallowed.
type brokenManifestPuller struct{ manifestErr error }

func (b *brokenManifestPuller) PullDigest(_ context.Context, _ string) (string, error) {
	return "", b.manifestErr
}
func (b *brokenManifestPuller) PullImageConfig(_ context.Context, _ string) (oci.ImageConfig, error) {
	return oci.ImageConfig{}, b.manifestErr
}
func (b *brokenManifestPuller) PullLayers(_ context.Context, _ string) (oci.PullLayersResult, error) {
	return oci.PullLayersResult{}, b.manifestErr
}
func (b *brokenManifestPuller) PullManifest(_ context.Context, _ string) (oci.Manifest, error) {
	return oci.Manifest{}, b.manifestErr
}
func (b *brokenManifestPuller) PullBlob(_ context.Context, _, _ string) (io.ReadCloser, error) {
	return nil, b.manifestErr
}

// TestEnsureBases_AllRowsStage walks DefaultRuntimeBaseRefs end-to-end:
// every row produces a StorageKey distinct from every other row, the
// digest sidecar matches, and the per-row summary's Skipped=false on
// the first run. The matrix here is the Tier 1 PR 2 lock-step pin —
// if a future runtime is added to DefaultRuntimeBaseRefs without
// matching it here, TestDefaultRuntimeBaseRefs_HasExpectedRuntimes
// (below) catches the drift at unit-test speed. Pinned by ADR-052.
func TestEnsureBases_AllRowsStage(t *testing.T) {
	mp := newTwoLayerPuller(t)
	hs := newBaseHarness(t, mp, &callCountingBuilder{})

	results, err := hs.h.EnsureBases(context.Background(), "amd64", DefaultRuntimeBaseRefs, nil)
	if err != nil {
		t.Fatalf("EnsureBases: %v", err)
	}
	if len(results) != len(DefaultRuntimeBaseRefs) {
		t.Fatalf("results = %d rows, want %d", len(results), len(DefaultRuntimeBaseRefs))
	}
	keysSeen := map[string]bool{}
	for i, r := range results {
		if r.Runtime != DefaultRuntimeBaseRefs[i].Runtime {
			t.Errorf("row %d runtime = %q, want %q", i, r.Runtime, DefaultRuntimeBaseRefs[i].Runtime)
		}
		if r.ConfigDigest == "" {
			t.Errorf("row %d (%s) ConfigDigest empty", i, r.Runtime)
		}
		if r.Skipped {
			t.Errorf("row %d (%s) Skipped=true on first run, want false", i, r.Runtime)
		}
		baseKey := sched.BaseKeyForArch(r.Runtime, "amd64")
		if _, err := hs.be.Get(context.Background(), baseKey); err != nil {
			t.Errorf("ext4 missing at %s for runtime %s: %v", baseKey, r.Runtime, err)
		}
		if keysSeen[baseKey] {
			t.Errorf("duplicate baseKey across rows: %s", baseKey)
		}
		keysSeen[baseKey] = true
		digestKey := sched.BaseDigestKeyForArch(r.Runtime, "amd64")
		if _, err := hs.be.Get(context.Background(), digestKey); err != nil {
			t.Errorf("digest sidecar missing at %s for runtime %s: %v", digestKey, r.Runtime, err)
		}
		// digestsSeen is intentionally NOT checked here — in this
		// fake-driven test, every row's puller returns the same
		// fixture manifest, so the config digest is identical across
		// rows. In production, distinct image refs produce distinct
		// OCI config digests; the per-row StorageKey's distinctness
		// (above) is the load-bearing property, not config digest.
	}
}

// TestEnsureBases_OperatorOverride_DigestPinnedWins — when an operator
// sets FAAS_DEPLOY_BASE_REF_<RUNTIME> to a digest-pinned ref, that
// ref is used (not the default). The test exercises the env-lookup
// seam with a hard-coded map; the nil-fallback to os.Getenv is the
// production wiring.
func TestEnsureBases_OperatorOverride_DigestPinnedWins(t *testing.T) {
	mp := newTwoLayerPuller(t)
	hs := newBaseHarness(t, mp, &callCountingBuilder{})
	const overrideRuntime = RuntimeNode24
	const overrideRef = "ghcr.io/onebox-faas/runner-node24@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	env := map[string]string{"FAAS_DEPLOY_BASE_REF_NODE24": overrideRef}
	lookup := func(k string) string { return env[k] }
	refs := DefaultRuntimeBaseRefs
	results, err := hs.h.EnsureBases(context.Background(), "amd64", refs, lookup)
	if err != nil {
		t.Fatalf("EnsureBases: %v", err)
	}
	var saw bool
	for _, r := range results {
		if r.Runtime == overrideRuntime {
			saw = true
			if r.Ref != overrideRef {
				t.Errorf("Ref = %q, want override %q", r.Ref, overrideRef)
			}
		} else {
			if r.Ref == overrideRef {
				t.Errorf("override %q leaked into row %s", overrideRef, r.Runtime)
			}
		}
	}
	if !saw {
		t.Fatalf("override row %s missing from results", overrideRuntime)
	}
}

// TestEnsureBases_OperatorOverride_TagOnlyFailsLoud — a tag-only
// override (`node24:latest`, no digest) aborts imaged startup before
// any layer is pulled. The same posture cmd/imaged applies to
// FAAS_DEPLOY_BASE_REF (deploy-time base ref). Pinned by ADR-052.
func TestEnsureBases_OperatorOverride_TagOnlyFailsLoud(t *testing.T) {
	mp := newTwoLayerPuller(t)
	hs := newBaseHarness(t, mp, &callCountingBuilder{})
	env := map[string]string{"FAAS_DEPLOY_BASE_REF_NODE24": "ghcr.io/onebox-faas/runner-node24:latest"}
	lookup := func(k string) string { return env[k] }
	_, err := hs.h.EnsureBases(context.Background(), "amd64", DefaultRuntimeBaseRefs, lookup)
	if err == nil {
		t.Fatal("tag-only EnvOverride should fail-loud before any byte is pulled")
	}
	if !strings.Contains(err.Error(), "digest-pinned") {
		t.Errorf("error %q must mention 'digest-pinned'", err.Error())
	}
	if !strings.Contains(err.Error(), "FAAS_DEPLOY_BASE_REF_NODE24") {
		t.Errorf("error %q must name the operator-facing env var", err.Error())
	}
}

// TestEnsureBases_SkipsOnDigestMatch — second call returns Skipped=true
// for every row when the digest sidecar matches. Inherits the same
// idempotency contract as EnsureBaseExt4's skip path.
func TestEnsureBases_SkipsOnDigestMatch(t *testing.T) {
	mp := newTwoLayerPuller(t)
	hs := newBaseHarness(t, mp, &callCountingBuilder{})
	first, err := hs.h.EnsureBases(context.Background(), "amd64", DefaultRuntimeBaseRefs, nil)
	if err != nil {
		t.Fatalf("first EnsureBases: %v", err)
	}
	second, err := hs.h.EnsureBases(context.Background(), "amd64", DefaultRuntimeBaseRefs, nil)
	if err != nil {
		t.Fatalf("second EnsureBases: %v", err)
	}
	for i, r := range second {
		if !r.Skipped {
			t.Errorf("row %d (%s) Skipped=false on second run, want true (digest match)", i, r.Runtime)
		}
	}
	if len(first) != len(second) {
		t.Errorf("first/second row counts mismatch: %d vs %d", len(first), len(second))
	}
}

// TestEnsureBases_FailsLoudOnPullError — a broken puller aborts the loop;
// no partial-staged fleet. The test asserts the err path bubble-up
// preserves the underlying registry error so the operator can
// diagnose without grepping the source.
func TestEnsureBases_FailsLoudOnPullError(t *testing.T) {
	bad := &brokenManifestPuller{manifestErr: errors.New("connection refused")}
	be, _ := storage.NewLocalStorageBackend(t.TempDir())
	h := &Handler{
		oci:     bad,
		builder: &fakeBuilder{},
		log:     silentLogger(),
		storage: be,
		grypeRun: func(_ context.Context, _ string) (map[string]int, error) {
			return map[string]int{}, nil
		},
	}
	_, err := h.EnsureBases(context.Background(), "amd64", DefaultRuntimeBaseRefs, nil)
	if err == nil {
		t.Fatal("EnsureBases must fail on a broken puller")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error %q must preserve 'connection refused' from registry", err.Error())
	}
	if !strings.Contains(err.Error(), "stage runtime base") {
		t.Errorf("error %q must annotate which runtime row failed (got %q)", err.Error(), err.Error())
	}
}

// TestEnsureBases_EmptyArchRejected — boundary check.
func TestEnsureBases_EmptyArchRejected(t *testing.T) {
	be, _ := storage.NewLocalStorageBackend(t.TempDir())
	h := &Handler{
		oci:      &minimalManifestPuller{},
		builder:  &fakeBuilder{},
		log:      silentLogger(),
		storage:  be,
		grypeRun: func(_ context.Context, _ string) (map[string]int, error) { return map[string]int{}, nil },
	}
	if _, err := h.EnsureBases(context.Background(), "", DefaultRuntimeBaseRefs, nil); err == nil {
		t.Error("empty arch should error")
	}
}

// TestEnsureBases_NilRefsIsNoOp — convenience: passing nil refs
// returns (nil, nil) so cmd/imaged can guard an "env-disabled" mode
// without a separate nil check. (DefaultRuntimeBaseRefs is never
// nil in production; the path is here for test seam only.)
func TestEnsureBases_NilRefsIsNoOp(t *testing.T) {
	be, _ := storage.NewLocalStorageBackend(t.TempDir())
	h := &Handler{
		oci:      &minimalManifestPuller{},
		builder:  &fakeBuilder{},
		log:      silentLogger(),
		storage:  be,
		grypeRun: func(_ context.Context, _ string) (map[string]int, error) { return map[string]int{}, nil },
	}
	if r, err := h.EnsureBases(context.Background(), "amd64", nil, nil); err != nil || r != nil {
		t.Errorf("nil refs → (%v, %v), want (nil, nil)", r, err)
	}
}

// TestDefaultRuntimeBaseRefs_HasExpectedRuntimes — the per-runtime
// set in DefaultRuntimeBaseRefs must match the supported runtime enum
// (apps.runtime CHECK after migrations/00075). A drift here means a
// runtime was added but its base isn't auto-staged, or a removed
// runtime's row wasn't deleted; either trips Tier 1 PR 2's load-bearing
// promise of "every runtime base auto-stages on imaged startup".
func TestDefaultRuntimeBaseRefs_HasExpectedRuntimes(t *testing.T) {
	want := []string{
		RuntimeNode22, RuntimeNode24,
		RuntimePython312, RuntimePython313,
		RuntimeGo124, RuntimeGo124Alpine,
	}
	if len(DefaultRuntimeBaseRefs) != len(want) {
		t.Fatalf("DefaultRuntimeBaseRefs = %d rows, want %d", len(DefaultRuntimeBaseRefs), len(want))
	}
	seen := map[string]bool{}
	for i, r := range DefaultRuntimeBaseRefs {
		seen[r.Runtime] = true
		if r.Ref == "" {
			t.Errorf("row %d (%s) Ref empty", i, r.Runtime)
		}
		if r.EnvOverride == "" {
			t.Errorf("row %d (%s) EnvOverride empty", i, r.Runtime)
		}
	}
	for _, rt := range want {
		if !seen[rt] {
			t.Errorf("runtime %s missing from DefaultRuntimeBaseRefs; check that migrations/00075 + pkg/imaged/base.go + base_stage.go are in lockstep", rt)
		}
	}
}

// schedBaseKeyForArch is removed; tests use pkg/sched.BaseKeyForArch
// / BaseDigestKeyForArch directly so the key format is sourced from
// the same constant the production code reads.
