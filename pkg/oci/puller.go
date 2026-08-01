// Package oci — OCI digest puller (spec §4.6, §9).
//
// The Puller interface is the single seam imaged uses to resolve a digest-pinned
// image and stream its layers + image config for the app-layer build.
// RegistryClient (registry.go) is the production implementation: a registry v2
// client that resolves a reference to its content digest over the public
// registry API (gap G1) and then fetches layer/config blobs. DefaultPuller is
// the offline/test default that echoes the reference and returns no layers —
// pkg/imaged's orchestration tests need no network.
package oci

import (
	"context"
	"io"
)

// ImageConfig is the parsed subset of an OCI/Docker image config blob that
// imaged needs to construct the AppManifest (spec §4.6). We intentionally
// don't model the full image config schema — just the fields we map.
//
// Field naming follows the OCI image config spec
// (https://github.com/opencontainers/image-spec/blob/main/config.md).
type ImageConfig struct {
	Cmd        []string          // → AppManifest.Entrypoint
	Env        map[string]string // "KEY" → "VALUE"; imaged flattens to AppManifest.Env
	WorkingDir string            // → AppManifest.WorkingDir
	// ExposedPorts is the set of ports the image declares; we don't use them
	// directly (the customer pins a port via the app's manifest) but parsing
	// them keeps a future "expose-all" mode cheap.
	ExposedPorts map[string]struct{}
}

// PullLayersResult is what PullLayers returns. Layers are streamed bottom-to-top
// in gzip-compressed form (the format `mkfs.ext4 -d` via rootfs.Builder
// expects, after ApplyLayerGz decompresses). Each ReadCloser MUST be closed by
// the caller; RegistryClient returns one per layer blob. Digest is the
// canonical content digest of the manifest the layers came from.
type PullLayersResult struct {
	Layers []io.ReadCloser
	Config ImageConfig
	Digest string
}

// Puller fetches OCI data for imaged.
//
// PullDigest resolves a reference to its canonical digest.
// PullImageConfig fetches only the small image-config blob and parses it —
// no layer streaming. The build pipeline uses this BEFORE PullLayers so a
// manifest that can't become a valid AppManifest (e.g. no Cmd) is rejected
// without fetching dozens of MB of layer blobs (review issue #6, DoS
// amplification on public registries).
// PullLayers streams every layer blob along with the parsed config; it
// internally uses PullImageConfig's manifest handling so the two paths
// can't drift.
type Puller interface {
	PullDigest(ctx context.Context, ref string) (string, error)
	PullImageConfig(ctx context.Context, ref string) (ImageConfig, error)
	PullLayers(ctx context.Context, ref string) (PullLayersResult, error)
}

// AuthPuller is the additive seam for per-app private-registry Basic
// Auth (issue #461 / ADR-062). Production RegistryClient satisfies
// Puller AND AuthPuller; offline DefaultPuller satisfies Puller
// (auth ignored by callers via the type-asserted fallback in imaged).
//
// The interface is intentionally separate from Puller so existing test
// doubles (cmd/e2e/fakevmm, etc.) and every Puller implementation
// across the codebase don't break. imaged type-asserts to AuthPuller
// and falls back to the anonymous path when the assertion fails —
// that mirrors the ManifestPuller pattern (puller.go:71).
//
// Pass `auth == nil` for the anonymous path; the caller's Basic Auth
// is sourced from app_registry_credentials (imaged transiently
// unseals the password). The handler-side egress gate
// (`apps.egress_allowlist`) is evaluated BEFORE the credential lookup
// so an egress-denied host fails the dial without ever touching the
// credential.
type AuthPuller interface {
	Puller
	PullDigestWithAuth(ctx context.Context, ref string, auth *BasicAuth) (string, error)
	PullImageConfigWithAuth(ctx context.Context, ref string, auth *BasicAuth) (ImageConfig, error)
	PullLayersWithAuth(ctx context.Context, ref string, auth *BasicAuth) (PullLayersResult, error)
}

// ManifestPuller is the M6 extension surface: production's RegistryClient
// satisfies it; offline fakes do not. imaged's handleDeployment type-asserts
// to ManifestPuller and falls back to the digest-only flow when the assertion
// fails — that keeps every unit test green without bringing the network in.
//
// PullManifest returns the decoded manifest for ref, including the config
// descriptor and every layer descriptor with its size and digest. PullBlob
// streams the bytes of a blob (layer tarball or config JSON) referenced by
// digest from repo. The caller MUST close the returned reader; the reader is
// gzipped when the underlying blob is.
type ManifestPuller interface {
	Puller
	PullManifest(ctx context.Context, ref string) (Manifest, error)
	PullBlob(ctx context.Context, repo, digest string) (io.ReadCloser, error)
}

// DefaultPuller is the offline default — it echoes the reference back from
// PullDigest / PullImageConfig and returns no layers from PullLayers.
// imaged.New substitutes it when no puller is injected; the shape
// pkg/imaged tests exercise.
//
// Production wires oci.RegistryClient, which serves real layer blobs and
// implements ManifestPuller (M6).
type DefaultPuller struct{}

func (DefaultPuller) PullDigest(_ context.Context, ref string) (string, error) {
	return ref, nil
}

func (DefaultPuller) PullImageConfig(_ context.Context, _ string) (ImageConfig, error) {
	return ImageConfig{}, nil
}

func (DefaultPuller) PullLayers(_ context.Context, digest string) (PullLayersResult, error) {
	return PullLayersResult{Digest: digest}, nil
}

// DefaultPuller also satisfies AuthPuller (issue #461 / ADR-062). The
// auth argument is ignored — offline tests don't ship credentials and
// the seam is exercised only in production via RegistryClient. Keeping
// the auth parameter on the signature pins the AuthPuller interface.
func (DefaultPuller) PullDigestWithAuth(_ context.Context, ref string, _ *BasicAuth) (string, error) {
	return ref, nil
}

func (DefaultPuller) PullImageConfigWithAuth(_ context.Context, _ string, _ *BasicAuth) (ImageConfig, error) {
	return ImageConfig{}, nil
}

func (DefaultPuller) PullLayersWithAuth(_ context.Context, digest string, _ *BasicAuth) (PullLayersResult, error) {
	return PullLayersResult{Digest: digest}, nil
}
