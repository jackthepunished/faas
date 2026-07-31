package imaged

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/onebox-faas/faas/pkg/oci"
	"github.com/onebox-faas/faas/pkg/rootfs"
	"github.com/onebox-faas/faas/pkg/sched"
	"github.com/onebox-faas/faas/pkg/storage"
	"github.com/onebox-faas/faas/pkg/wire"
)

// Base stage — imaged startup provisions the shared read-only drive0 used by
// builder microVMs (spec §4.6, two-drive scheme). At runtime, schedd hands
// the path of a staged base ext4 to vmmd when cold-booting a builder; that
// path must exist on disk before the first build is admitted.
//
// The conversion runs once per box lifetime, pinned by digest: when the
// remote OCI image's config digest hasn't changed since the last stage, the
// existing ext4 is trusted as-is. When it has, the layers are re-pulled and
// the ext4 is rewritten atomically (write to <out>.tmp, fsync, rename).

// BaseStageResult reports what EnsureBaseExt4 did. Skip=true means the
// existing artifact matched the remote digest and was left untouched.
type BaseStageResult struct {
	// OutImage is the host-side path the staged ext4 lives at. Computed
	// from the routed StorageBackend's "snapshot" path so schedd's
	// drive0 lookup can pass it to vmmd verbatim (spec §4.6 two-drive
	// scheme). Empty when the LocalStorageBackend is not the canonical
	// /srv/fc root (a remote driver; callers downstream use the
	// StorageKey instead).
	OutImage string
	// StorageKey is the canonical key the staged ext4 was published
	// under, e.g. "base/runner-node22.ext4". Same value baseStageKey
	// took; reported back so callers don't have to recompute it.
	StorageKey   string
	ConfigDigest string // empty when Skip
	Skipped      bool
}

// EnsureBaseExt4 guarantees baseKey exists and reflects ref's current
// layers.
//
// ref is the OCI reference to pull the base image from (e.g. ghcr.io/onebox-
// faas/builder-base:latest). When ref's config digest matches the digest
// sidecar at digestKey, the existing artifact is left in place and Skipped
// is true. When ref has changed, the layers are pulled fresh and baseKey
// is republished via Storage.Put; storage.Put's internal temp+rename
// preserves the atomicity the legacy os.Rename provided.
//
// parentRef + parentBaseKey (ADR-053) gate the parent-ref staging
// branch: when parentRef is non-empty, EnsureBaseExt4 asks vmmd
// (the only root component, spec §11) to loopback-mount the parent
// ext4 at parentBaseKey read-only, cp -a's the parent tree into a
// fresh staging dir, applies ONLY the runtime delta OCI layers
// (oci.LayersAboveBase) on top, and hands the staging tree to
// BuildBaseFromStaging. Empty parentRef stays on the legacy "apply
// ALL layers" path (builder-base / go124 / go124-alpine /
// base-debian-parent itself).
//
// outImage is the resolved host path schedd hands to vmmd when cold-
// booting a builder against the local /srv/fc base. For a non-canonical
// storage root (a future remote driver) outImage is empty and schedd
// must read from baseKey via Get instead — handled by the cmd/vmmd
// caller.
//
// Requires the OCI puller to implement oci.ManifestPuller (registry v2
// streaming). Without it, EnsureBaseExt4 returns an error: M6+'s builderd
// only runs with full M6 wiring, and skipping silently would mask a real
// config error.
func (h *Handler) EnsureBaseExt4(
	ctx context.Context,
	ref, baseKey, digestKey, outImage string,
	parentRef, parentBaseKey string, // ADR-053; empty = legacy path
) (BaseStageResult, error) {
	if ref == "" {
		return BaseStageResult{}, errors.New("imaged: EnsureBaseExt4: empty ref")
	}
	if baseKey == "" {
		return BaseStageResult{}, errors.New("imaged: EnsureBaseExt4: empty baseKey")
	}
	if digestKey == "" {
		return BaseStageResult{}, errors.New("imaged: EnsureBaseExt4: empty digestKey")
	}

	mp, ok := h.oci.(oci.ManifestPuller)
	if !ok {
		return BaseStageResult{}, fmt.Errorf(
			"imaged: EnsureBaseExt4: puller %T does not implement ManifestPuller", h.oci)
	}

	manifest, err := mp.PullManifest(ctx, ref)
	if err != nil {
		return BaseStageResult{}, fmt.Errorf("imaged: pull base manifest %s: %w", ref, err)
	}

	// Idempotency: digest sidecar at digestKey records the config digest
	// the staged ext4 was built from. When it matches, trust the
	// existing artifact — re-fetching tens of MB of layers on every daemon
	// restart would be wasteful and would also race the cold-boot path
	// if a build happened to land mid-stage.
	wantDigest := manifest.Config.Digest
	be, err := h.storageFor()
	if err != nil {
		return BaseStageResult{}, fmt.Errorf("imaged: storageFor for base stage: %w", err)
	}
	// Idempotency: trust the digest sidecar. When it matches, the base
	// ext4 is the right artifact — we don't re-stream its bytes here.
	// A bare Get-and-close on baseKey would stream 130 MB through the
	// daemon for nothing; the sidecar is the source of truth. A missing
	// base would surface as Get returning ErrNotFound, which the next
	// cold-boot would also surface — no silent corruption.
	if haveRC, err := be.Get(ctx, digestKey); err == nil {
		haveBytes, rerr := io.ReadAll(haveRC)
		_ = haveRC.Close()
		if rerr == nil && string(haveBytes) == wantDigest {
			if rc, err := be.Get(ctx, baseKey); err == nil {
				_ = rc.Close()
				return BaseStageResult{
					OutImage:     outImage,
					StorageKey:   baseKey,
					ConfigDigest: wantDigest,
					Skipped:      true,
				}, nil
			}
		}
	}

	// ADR-053: dispatch on parentRef. The parent-ref branch asks
	// vmmd to loopback-mount the parent ext4 read-only, cp -a's
	// the parent tree into a fresh staging dir, applies ONLY the
	// runtime delta OCI layers (oci.LayersAboveBase), and hands
	// the staging tree to BuildBaseFromStaging. Empty parentRef
	// stays on the legacy "apply ALL layers" path
	// (builder-base / go124 / go124-alpine / base-debian-parent).
	if parentRef != "" {
		return h.ensureBaseExt4ParentRef(ctx, mp, manifest, ref, baseKey, digestKey, outImage, parentRef, parentBaseKey, wantDigest, be)
	}

	// --- Legacy "apply ALL layers" path ----------------------------
	// Pre-allocate the readers slice + closers so a partial pull on layer N
	// still closes layers 0..N-1. PullBlob streams the gzipped tarball; we
	// hand it to Builder.BuildBase which copies it through ApplyLayerGz.
	//
	// PullBlob takes a repo like "ghcr.io/onebox-faas/builder-base" — the
	// host:port + path with no tag/digest suffix. ParseReference splits
	// the ref for us (same parser the registry client uses internally).
	ociRef, err := oci.ParseReference(ref)
	if err != nil {
		return BaseStageResult{}, fmt.Errorf("imaged: parse base ref %q: %w", ref, err)
	}
	readers := make([]io.Reader, 0, len(manifest.Layers))
	closers := make([]io.ReadCloser, 0, len(manifest.Layers))
	for _, l := range manifest.Layers {
		body, err := mp.PullBlob(ctx, ociRef.Registry+"/"+ociRef.Repository, l.Digest)
		if err != nil {
			for _, c := range closers {
				_ = c.Close()
			}
			return BaseStageResult{}, fmt.Errorf("imaged: pull base layer %s: %w", l.Digest, err)
		}
		readers = append(readers, body)
		closers = append(closers, body)
	}
	defer func() {
		for _, c := range closers {
			_ = c.Close()
		}
	}()

	res, err := h.builder.BuildBase(ctx, rootfs.BaseBuildInput{
		Layers:     readers,
		Storage:    be,
		StorageKey: baseKey,
	})
	if err != nil {
		return BaseStageResult{}, fmt.Errorf("imaged: build base ext4: %w", err)
	}

	if err := h.writeBaseDigestSidecar(ctx, be, digestKey, wantDigest); err != nil {
		h.log.Warn("imaged: write base digest sidecar", "err", err)
	}
	if err := h.writeScanSidecar(ctx, baseKey, ref, outImage); err != nil {
		h.log.Warn("imaged: write grype scan sidecar",
			"key", wire.ScanKeyForBaseKey(baseKey), "err", err)
	}

	h.log.Info("imaged: staged builder base",
		"ref", ref, "key", res.ImageKey, "size_bytes", res.SizeBytes,
		"digest", wantDigest)

	return BaseStageResult{
		OutImage:     outImage,
		StorageKey:   res.ImageKey,
		ConfigDigest: wantDigest,
	}, nil
}

// writeBaseDigestSidecar writes the per-base config-digest
// sidecar at digestKey. Extracted from the legacy EnsureBaseExt4
// path so the parent-ref branch can call the same helper
// (ADR-053). The sidecar is the source of truth for the
// "did this base already stage?" check — re-fetching tens of
// MB of layers on every daemon restart would be wasteful.
func (h *Handler) writeBaseDigestSidecar(ctx context.Context, be storage.StorageBackend, digestKey, wantDigest string) error {
	digestRC, err := openStringReader(wantDigest)
	if err != nil {
		return fmt.Errorf("imaged: open digest sidecar: %w", err)
	}
	if err := be.Put(ctx, digestKey, digestRC); err != nil {
		return fmt.Errorf("imaged: write digest sidecar %q: %w", digestKey, err)
	}
	return nil
}

// ensureBaseExt4ParentRef is the ADR-053 staging path: ask vmmd
// to mount the parent ext4, cp -a the parent tree, apply ONLY
// the runtime delta OCI layers (oci.LayersAboveBase), and
// hand the staging tree to BuildBaseFromStaging for mkfs +
// publish.
//
// Pre-conditions:
//   - parentRef is non-empty; parentBaseKey is the staged
//     parent's StorageBackend key (e.g.
//     "base/runner-base-debian-parent-amd64.ext4"). The parent
//     must already be staged via EnsureBases (the loop stages
//     RuntimeDebianParent at index 0 so this precondition holds
//     for every child row that follows).
//   - h.vmmClient is wired (cmd/imaged calls WithVMMClient at
//     startup). Fail loud if nil — the legacy "apply all layers"
//     path stays operational without a client, but the parent-ref
//     path requires vmmd to run the loopback mount.
//
// Failure modes (all surfaced as wrapped errors so cmd/imaged's
// EnsureBases loop can abort cleanly):
//   - parent base not yet staged: vmmClient.MountParentExt4ReadOnly
//     returns vmmdmount.ErrNotFound → wrapped here.
//   - vmmd down or unreachable: dial error.
//   - delta resolution fails (LayersAboveBase reports the parent's
//     DiffIDs aren't a prefix of the runtime's): re-wrap the
//     sentinel so the operator sees the OCI-chain composability
//     invariant failure (the load-bearing ADR-053 invariant —
//     a Dockerfile that drifts to `FROM scratch + COPY` would
//     surface here).
func (h *Handler) ensureBaseExt4ParentRef(
	ctx context.Context,
	mp oci.ManifestPuller,
	manifest oci.Manifest,
	ref, baseKey, digestKey, outImage string,
	parentRef, parentBaseKey, wantDigest string,
	be storage.StorageBackend,
) (BaseStageResult, error) {
	if h.vmmClient == nil {
		return BaseStageResult{}, fmt.Errorf(
			"imaged: EnsureBaseExt4 (parent-ref): no VMMClient wired (run WithVMMClient in cmd/imaged; ADR-053)")
	}

	// Pull + parse the runtime config to get DiffIDs. The runtime
	// manifest is already in hand (EnsureBaseExt4 pulled it).
	ociRef, err := oci.ParseReference(ref)
	if err != nil {
		return BaseStageResult{}, fmt.Errorf("imaged: parse runtime ref %q: %w", ref, err)
	}
	runtimeCfg, err := h.pullConfig(ctx, mp, ociRef.Registry+"/"+ociRef.Repository, manifest.Config.Digest)
	if err != nil {
		return BaseStageResult{}, fmt.Errorf("imaged: pull runtime config: %w", err)
	}

	// Pull + parse the parent config so we can compute the delta.
	parentManifest, err := mp.PullManifest(ctx, parentRef)
	if err != nil {
		return BaseStageResult{}, fmt.Errorf("imaged: pull parent manifest %s: %w", parentRef, err)
	}
	parentOCI, err := oci.ParseReference(parentRef)
	if err != nil {
		return BaseStageResult{}, fmt.Errorf("imaged: parse parent ref %q: %w", parentRef, err)
	}
	parentCfg, err := h.pullConfig(ctx, mp, parentOCI.Registry+"/"+parentOCI.Repository, parentManifest.Config.Digest)
	if err != nil {
		return BaseStageResult{}, fmt.Errorf("imaged: pull parent config: %w", err)
	}

	// Load-bearing invariant: parent's DiffIDs MUST be an exact
	// prefix of runtime's. If they aren't, the staging-time
	// composition can't apply the delta layers on top of the
	// parent ext4 (LayersAboveBase returns ErrEmptyDelta or
	// ErrNotPrefix). The failure message names the ADR so an
	// operator chasing a `FROM scratch + COPY` regression sees
	// the root cause in the log.
	delta, err := oci.LayersAboveBase(parentCfg.DiffIDs, runtimeCfg.DiffIDs)
	if err != nil {
		return BaseStageResult{}, fmt.Errorf(
			"imaged: parent-ref delta (ADR-053): %w "+
				"(parent_ref=%s parent_diff_ids=%d runtime_ref=%s runtime_diff_ids=%d)",
			err, parentRef, len(parentCfg.DiffIDs), ref, len(runtimeCfg.DiffIDs))
	}
	if len(delta) == 0 {
		// Empty delta = runtime has no layers above parent = the
		// staged child IS the parent. Surface this loudly so a
		// future Dockerfile drift (e.g. accidentally bumping
		// FROM parent → FROM scratch) trips here rather than
		// silently producing a base-less child ext4.
		return BaseStageResult{}, fmt.Errorf(
			"imaged: parent-ref delta (ADR-053): empty — runtime %s has no layers above parent %s "+
				"(check the Dockerfile's FROM chain)", ref, parentRef)
	}

	// Map runtime DiffIDs → compressed-blob descriptors (same shape
	// as aboveBaseLayers in pkg/imaged/handler.go).
	if len(manifest.Layers) != len(runtimeCfg.DiffIDs) {
		return BaseStageResult{}, fmt.Errorf(
			"imaged: parent-ref layer count mismatch: manifest=%d config=%d",
			len(manifest.Layers), len(runtimeCfg.DiffIDs))
	}
	blobByDiff := make(map[string]oci.Descriptor, len(manifest.Layers))
	for i, l := range manifest.Layers {
		blobByDiff[runtimeCfg.DiffIDs[i]] = l
	}

	// Mount the parent ext4 read-only via vmmd (the only root
	// component). Defer the umount so a partial copy or
	// ApplyLayerGz error still releases the mount — a stuck
	// parent mount would surface as the next 5-minute orphan
	// sweep, but explicit defer-on-error is faster + safer.
	mountpoint, err := h.vmmClient.MountParentExt4ReadOnly(ctx, parentBaseKey)
	if err != nil {
		return BaseStageResult{}, fmt.Errorf("imaged: mount parent ext4 %q: %w", parentBaseKey, err)
	}
	defer func() {
		if uerr := h.vmmClient.UmountParentExt4(context.WithoutCancel(ctx), mountpoint); uerr != nil {
			h.log.Warn("imaged: umount parent ext4 failed", "mountpoint", mountpoint, "err", uerr)
		}
	}()

	// cp -a the parent tree into a fresh staging dir. The
	// staging dir is the input to BuildBaseFromStaging — same
	// caller-owned-cleanup contract as the per-runtime staging
	// dir MkdirBaseStaging returns.
	staging, err := rootfs.MkdirBaseStaging()
	if err != nil {
		return BaseStageResult{}, fmt.Errorf("imaged: parent-ref staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	if err := CopyTree(ctx, mountpoint+"/.", staging); err != nil {
		return BaseStageResult{}, fmt.Errorf("imaged: cp -a parent tree %s -> %s: %w", mountpoint, staging, err)
	}

	// Release the parent mount as early as possible — the delta
	// layers we apply next don't read the parent tree, they
	// only write on top of staging. The deferred umount is
	// still there for the error path, but this explicit call
	// drops the mount's RSS contribution immediately and lets a
	// subsequent sibling restage of a different runtime see the
	// freed slot in vmmd's parent-mount registry.
	if err := h.vmmClient.UmountParentExt4(ctx, mountpoint); err != nil {
		h.log.Warn("imaged: parent umount (early release) failed", "mountpoint", mountpoint, "err", err)
	}

	// Apply the delta layers on top of the parent's tree. Each
	// layer is streamed via rootfs.ApplyLayerGz directly — the
	// legacy path collects readers into a slice and hands them to
	// BuildBase, but the parent-ref path applies layers in-place
	// on the staging dir (BuildBaseFromStaging doesn't read
	// BaseBuildInput.Layers — see pkg/rootfs/build_base.go).
	closers := make([]io.ReadCloser, 0, len(delta))
	defer func() {
		for _, c := range closers {
			_ = c.Close()
		}
	}()
	for _, diffID := range delta {
		desc, ok := blobByDiff[diffID]
		if !ok {
			return BaseStageResult{}, fmt.Errorf("imaged: parent-ref missing blob for diff %s", diffID)
		}
		rc, err := mp.PullBlob(ctx, ociRef.Registry+"/"+ociRef.Repository, desc.Digest)
		if err != nil {
			return BaseStageResult{}, fmt.Errorf("imaged: parent-ref pull blob %s: %w", desc.Digest, err)
		}
		closers = append(closers, rc)
		if err := rootfs.ApplyLayerGz(staging, rc); err != nil {
			return BaseStageResult{}, fmt.Errorf("imaged: parent-ref apply delta layer %s: %w", desc.Digest, err)
		}
	}

	res, err := h.builder.BuildBaseFromStaging(ctx, staging, rootfs.BaseBuildInput{
		Storage:    be,
		StorageKey: baseKey,
	})
	if err != nil {
		return BaseStageResult{}, fmt.Errorf("imaged: parent-ref build base ext4: %w", err)
	}

	if err := h.writeBaseDigestSidecar(ctx, be, digestKey, wantDigest); err != nil {
		h.log.Warn("imaged: write base digest sidecar", "err", err)
	}
	if err := h.writeScanSidecar(ctx, baseKey, ref, outImage); err != nil {
		h.log.Warn("imaged: write grype scan sidecar",
			"key", wire.ScanKeyForBaseKey(baseKey), "err", err)
	}

	h.log.Info("imaged: staged runtime base (parent-ref)",
		"ref", ref, "parent_ref", parentRef,
		"key", res.ImageKey, "size_bytes", res.SizeBytes,
		"delta_layers", len(delta), "digest", wantDigest)

	return BaseStageResult{
		OutImage:     outImage,
		StorageKey:   res.ImageKey,
		ConfigDigest: wantDigest,
	}, nil
}

// openStringReader returns an io.Reader for the supplied string. The
// helper exists so the digest sidecar Put has a content source without
// dragging in bytes.NewReader (which would also force a package-level
// bytes import that's only used here).
func openStringReader(s string) (io.Reader, error) {
	return stringReader(s), nil
}

type stringReaderImpl struct {
	s   string
	off int
}

func stringReader(s string) io.Reader { return &stringReaderImpl{s: s} }
func (r *stringReaderImpl) Read(p []byte) (int, error) {
	if r.off >= len(r.s) {
		return 0, io.EOF
	}
	n := copy(p, r.s[r.off:])
	r.off += n
	return n, nil
}

// writeScanSidecar runs a Grype scan of the staged base ext4 and
// writes the per-severity finding counts to the scan sidecar
// (issue #299). The sidecar is keyed by wire.ScanKeyForBaseKey so
// vmmd's bringUpScanCheck can find it at boot. Fail-closed: a scan
// error or nil findings writes a CRITICAL=9999 placeholder so
// vmmd refuses to boot any un-scanned artifact.
//
// `ref` is the OCI ref the base ext4 was pulled from (recorded in
// the sidecar's `image` field for dashboard traceability — a
// customer looking at `vmmd_trivy_image_vulns_total{image=...}`
// needs to see the registry ref, not the local staging path).
// `outImage` is the filesystem path Grype's `dir:` source walks.
// Passing the OCI ref to `dir:` was the original implementation;
// Grype's `dir:` source is filesystem-only and rejected registry
// refs, which tripped the fail-closed branch on every staged
// base (Critical #1 of the PR #385 review). The mapped path
// is recorded in the sidecar's `image` field for dashboard
// traceability.
func (h *Handler) writeScanSidecar(ctx context.Context, baseKey, ref, outImage string) error {
	be, err := h.storageFor()
	if err != nil {
		return fmt.Errorf("imaged: writeScanSidecar storageFor: %w", err)
	}
	findings, scanErr := h.runGrype(ctx, outImage)
	if scanErr != nil || findings == nil {
		h.log.Warn("imaged: grype scan failed; writing fail-closed sidecar",
			"ref", ref, "err", scanErr)
		// CRITICAL=9999 (and the other buckets set to 9999 as well)
		// ensures vmmd's bringUpScanCheck fails closed on every
		// severity, not just CRITICAL. A future admission policy
		// could distinguish "CRITICAL known-bad" from "no scan
		// at all" via the Findings field — for now, both collapse
		// to "refuse to boot".
		findings = map[string]int{
			"CRITICAL": 9999, "HIGH": 9999, "MEDIUM": 9999,
			"LOW": 9999, "UNKNOWN": 0,
		}
	}
	scanBlob, err := json.Marshal(struct {
		Image     string         `json:"image"`
		Findings  map[string]int `json:"findings"`
		ScannedAt time.Time      `json:"scanned_at"`
	}{
		Image:     ref,
		Findings:  findings,
		ScannedAt: time.Now().UTC(),
	})
	if err != nil {
		return fmt.Errorf("imaged: marshal scan sidecar: %w", err)
	}
	scanKey := wire.ScanKeyForBaseKey(baseKey)
	if err := be.Put(ctx, scanKey, bytes.NewReader(scanBlob)); err != nil {
		return fmt.Errorf("imaged: write scan sidecar %q: %w", scanKey, err)
	}
	return nil
}

// RuntimeBaseRef pairs a runtime id with its default OCI ref and the
// env-var name an operator may use to override that ref with a digest-
// pinned value. Used by DefaultRuntimeBaseRefs to drive imaged's
// startup auto-stage loop (Tier 1 PR 2, ADR-052).
type RuntimeBaseRef struct {
	// Runtime is the apps.runtime constant the function deploy stores
	// in the database (e.g. "node24"). It is also the base key's path
	// component: sched.BaseKeyForArch(runtime, arch) →
	// "base/runner-<runtime>-<arch>.ext4".
	Runtime string
	// Ref is the default OCI ref for the runtime base. The `:latest`
	// default is correct for dev (a fresh box auto-pulls whatever the
	// registry currently serves); production must override via
	// EnvOverride because a deploy keyed to today's `:latest` will
	// silently resolve to tomorrow's image on the next cold-boot.
	Ref string
	// EnvOverride is the operator-facing env-var name (e.g.
	// "FAAS_DEPLOY_BASE_REF_NODE24"). When the env var is set the
	// override MUST be digest-pinned (oci.ParseReference.Digest != "")
	// — a tag-only override aborts imaged's startup loop with a
	// fail-loud error, the same posture the deploy-base-ref gate uses
	// in cmd/imaged/main.go.
	EnvOverride string
	// ParentRef (ADR-053) is the OCI ref of the staging-only parent
	// runtime this row's base composes on top of. Empty for rows on
	// the legacy "apply all layers" path (builder-base, go124,
	// go124-alpine, base-minimal). When set, EnsureBaseExt4 takes
	// the parent-ref branch: ask vmmd to mount the parent ext4,
	// cp -a its tree into a fresh staging dir, apply ONLY the
	// delta OCI layers, and mkfs. The parent runtime's row
	// (ParentRef="") is staged first so children always find a
	// valid parent.
	ParentRef string
}

// DefaultRuntimeBaseRefs is the canonical runtime → OCI-base mapping
// for every supported function runtime. The table is the Tier 1 PR 2
// analog of the older FAAS_BUILDER_BASE_REF knob: a single seeded map
// replaces the per-runtime staging recipe. Adding a new runtime means
// adding a row here, mirroring the matrix pins at
// pkg/imaged/handler_test.go (TestBaseRefFor_Runtimes /
// TestBuildFunctionLayer_Runtimes / TestMissingRunnerFailsLoud).
//
// go124-alpine shares the go124 runner shim but is on a different
// base image (musl vs glibc). It gets its own row because the OCI
// ref is distinct (BaseRefGo124Alpine) and the staged ext4 sits under
// its own key (`base/runner-go124-alpine-<arch>.ext4`), even though
// the build/run path at function-deploy time reuses go124's runner
// binary.
//
// ADR-053: the four node/python runtime rows now declare
// ParentRef: BaseRefDebianParent. The shared parent is staged
// first (index 0) so a parent re-stage failure aborts the loop
// before any child is attempted — half-staged fleet is worse
// than refuse. The parent row's ParentRef is "" (legacy path:
// apply ALL its layers, no composition).
var DefaultRuntimeBaseRefs = []RuntimeBaseRef{
	{Runtime: RuntimeDebianParent, Ref: BaseRefDebianParent, EnvOverride: "FAAS_DEPLOY_BASE_REF_DEBIAN_PARENT"},
	{Runtime: RuntimeNode22, Ref: BaseRefNode22, EnvOverride: "FAAS_DEPLOY_BASE_REF_NODE22", ParentRef: BaseRefDebianParent},
	{Runtime: RuntimePython312, Ref: BaseRefPython312, EnvOverride: "FAAS_DEPLOY_BASE_REF_PYTHON312", ParentRef: BaseRefDebianParent},
	{Runtime: RuntimeGo124, Ref: BaseRefGo124, EnvOverride: "FAAS_DEPLOY_BASE_REF_GO124"},
	{Runtime: RuntimeGo124Alpine, Ref: BaseRefGo124Alpine, EnvOverride: "FAAS_DEPLOY_BASE_REF_GO124_ALPINE"},
	{Runtime: RuntimeNode24, Ref: BaseRefNode24, EnvOverride: "FAAS_DEPLOY_BASE_REF_NODE24", ParentRef: BaseRefDebianParent},
	{Runtime: RuntimePython313, Ref: BaseRefPython313, EnvOverride: "FAAS_DEPLOY_BASE_REF_PYTHON313", ParentRef: BaseRefDebianParent},
}

// EnsureBasesResult reports what EnsureBases did for a single runtime
// row. Used by the imaged-ready log line so the §12 dashboard sees a
// per-runtime summary at startup (skip vs rebuild, observed digest).
type EnsureBasesResult struct {
	// Runtime is the apps.runtime constant the row belongs to.
	Runtime string
	// Ref is the OCI ref that was actually staged (defaults from the
	// table, overridden via EnvOverride when set).
	Ref string
	// ConfigDigest is the OCI config digest the staged ext4 was built
	// from (empty only when the row was rejected pre-stage, e.g. a
	// non-digest EnvOverride).
	ConfigDigest string
	// Skipped is true when the digest sidecar matched and the
	// existing artifact was left untouched.
	Skipped bool
}

// EnsureBases iterates DefaultRuntimeBaseRefs and stages every runtime
// base at imaged startup, mirroring the builder-base auto-stage that
// pre-dates this PR. The first non-skip, non-EnvOverride-validate
// failure aborts the loop — half-staged fleet is worse than refuse,
// because a partial staging of N-1 runtimes would silently omit one
// runtime on the customer's first wake.
//
// Per-row idempotency and digest-pin handling is identical to the
// builder-base path: the digest sidecar short-circuits rebuilds when
// the OCI ref is unchanged (digest match → Skipped=true), and an
// operator EnvOverride set to a tag-only ref (no `Digest` in
// oci.ParseReference) fails loud before any layer is pulled.
//
// The legacy pre-PR operator recipe ("docker build + mkfs.ext4 + scp
// to /srv/fc/base/runner-<rt>.ext4") is preserved by docs/runtimes/*.md
// for boxes that haven't upgraded imaged yet — the auto-stage path is
// strictly additive.
func (h *Handler) EnsureBases(ctx context.Context, arch string, refs []RuntimeBaseRef, envLookup func(string) string) ([]EnsureBasesResult, error) {
	if arch == "" {
		return nil, errors.New("imaged: EnsureBases: empty arch")
	}
	if len(refs) == 0 {
		return nil, nil
	}
	if envLookup == nil {
		envLookup = os.Getenv
	}
	out := make([]EnsureBasesResult, 0, len(refs))
	for _, row := range refs {
		ref := row.Ref
		if v := strings.TrimSpace(envLookup(row.EnvOverride)); v != "" {
			// Operator wants this runtime pinned. Reject tag-only
			// overrides before any byte is pulled — a deploy keyed
			// to a today-stable digest would silently resolve to
			// whatever `:v2` or `:latest` the registry serves
			// tomorrow, and a cold-boot in two weeks would rebuild
			// the fleet base against the new bytes. This is the
			// same posture the deploy-base-ref gate uses
			// (cmd/imaged/main.go).
			parsed, perr := oci.ParseReference(v)
			if perr != nil || parsed.Digest == "" {
				return nil, fmt.Errorf("imaged: %s=%q must be a digest-pinned reference (e.g. registry.gregale.dev/img@sha256:...)", row.EnvOverride, v)
			}
			ref = v
		}
		baseKey := sched.BaseKeyForArch(row.Runtime, arch)
		digestKey := sched.BaseDigestKeyForArch(row.Runtime, arch)
		outImage := sched.BaseKeyForArch(row.Runtime, arch) // LocalStorageBackend joins under FAAS_STORAGE_ROOT
		// ADR-053: when the row declares a parent runtime, pass its
		// resolved OCI ref + StorageBackend key so EnsureBaseExt4
		// can take the parent-ref branch. The parent row's
		// ParentRef is "" (its own loop iteration skips this
		// block) and runs the legacy "apply ALL layers" path —
		// staged first because DefaultRuntimeBaseRefs[0] is the
		// parent.
		parentRef := row.ParentRef
		parentBaseKey := ""
		if parentRef != "" {
			parentBaseKey = sched.BaseKeyForArch(RuntimeDebianParent, arch)
		}
		res, err := h.EnsureBaseExt4(ctx, ref, baseKey, digestKey, outImage, parentRef, parentBaseKey)
		if err != nil {
			return nil, fmt.Errorf("imaged: stage runtime base %s (%s → %s): %w", row.Runtime, ref, baseKey, err)
		}
		out = append(out, EnsureBasesResult{
			Runtime:      row.Runtime,
			Ref:          ref,
			ConfigDigest: res.ConfigDigest,
			Skipped:      res.Skipped,
		})
	}
	return out, nil
}

// envLookup nil-falls-back to os.Getenv; the test seam is a map
// literal (TestEnsureBases_OperatorOverride_*).
