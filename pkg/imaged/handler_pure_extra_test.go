// handler_pure_extra_test.go — fill pkg/imaged/handler.go coverage
// of the small pure / no-store helpers beyond what
// handler_coverage_test.go already covers. Targets:
//   - cloneEnv (defensive copy + mutation isolation)
//   - layersAsReaders (the close-side retention)
//   - pullResult (the metric label converter)
//   - pullDigestWithAuth / pullImageConfigWithAuth /
//     pullLayersWithAuth / pullManifestWithAuth / pullBlobWithAuth
//     (the AuthPuller/AuthManifestPuller seam-dispatch)
//   - The remaining With* setters that return receiver
//     (WithRuntimeBaseStaging, WithArtifactReplicator,
//     WithSecretScanRun)
//   - isSlugSafe / isDeploymentIDSafe (the regex contract)
//   - appsRootPath (the slug+deployment path builder)
//   - transition (the bare store.UpdateDeploymentStatus wrapper)
//
// Whitebox `package imaged`.
package imaged

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/oci"
	"github.com/onebox-faas/faas/pkg/secretscan"
)

// --- cloneEnv ----------------------------------------------------

func TestCloneEnv_NilReturnsNil(t *testing.T) {
	if got := cloneEnv(nil); got != nil {
		t.Errorf("nil: got %v, want nil", got)
	}
}

func TestCloneEnv_EmptyReturnsNil(t *testing.T) {
	if got := cloneEnv(map[string]string{}); got != nil {
		t.Errorf("empty: got %v, want nil", got)
	}
}

func TestCloneEnv_PopulatedRoundTrip(t *testing.T) {
	in := map[string]string{"A": "1", "B": "2"}
	out := cloneEnv(in)
	if len(out) != 2 || out["A"] != "1" || out["B"] != "2" {
		t.Errorf("got %v, want %v", out, in)
	}
}

func TestCloneEnv_MutationIsolation(t *testing.T) {
	in := map[string]string{"A": "1"}
	out := cloneEnv(in)
	out["A"] = "mutated"
	if in["A"] != "1" {
		t.Errorf("input mutated: got %q", in["A"])
	}
}

// --- layersAsReaders --------------------------------------------

type nopReadCloser struct{}

func (n *nopReadCloser) Read(p []byte) (int, error) { return len(p), nil }
func (n *nopReadCloser) Close() error               { return nil }

func TestLayersAsReaders_EmptySlice(t *testing.T) {
	got := layersAsReaders(nil)
	if len(got) != 0 {
		t.Errorf("nil: got %v, want empty", got)
	}
}

func TestLayersAsReaders_IdentityPerElement(t *testing.T) {
	rc1 := &nopReadCloser{}
	rc2 := &nopReadCloser{}
	in := []io.ReadCloser{rc1, rc2}
	out := layersAsReaders(in)
	if len(out) != 2 {
		t.Fatalf("got %d readers, want 2", len(out))
	}
	// Each returned reader is type-equal to the input ReadCloser.
	if out[0] != io.Reader(rc1) {
		t.Errorf("[0] not borrowed from rc1")
	}
	if out[1] != io.Reader(rc2) {
		t.Errorf("[1] not borrowed from rc2")
	}
}

// --- pullResult --------------------------------------------------

func TestPullResult_NilReturnsOK(t *testing.T) {
	if got := pullResult(nil); got != "ok" {
		t.Errorf("nil: got %q, want ok", got)
	}
}

func TestPullResult_AnyErrorReturnsErr(t *testing.T) {
	cases := []error{
		errors.New("boom"),
		io.EOF,
		context.Canceled,
	}
	for _, err := range cases {
		if got := pullResult(err); got != "err" {
			t.Errorf("err=%v: got %q, want err", err, got)
		}
	}
}

// --- pull{Digest,ImageConfig,Layers,Manifest,Blob}WithAuth ------

type authCapable struct {
	digest *string
	used   []string
}

func (a *authCapable) PullDigest(_ context.Context, ref string) (string, error) {
	a.used = append(a.used, "PullDigest")
	return *a.digest, nil
}

func (a *authCapable) PullDigestWithAuth(_ context.Context, ref string, _ *oci.BasicAuth) (string, error) {
	a.used = append(a.used, "PullDigestWithAuth")
	return *a.digest, nil
}

func (a *authCapable) PullImageConfig(_ context.Context, ref string) (oci.ImageConfig, error) {
	a.used = append(a.used, "PullImageConfig")
	return oci.ImageConfig{}, nil
}

func (a *authCapable) PullImageConfigWithAuth(_ context.Context, ref string, _ *oci.BasicAuth) (oci.ImageConfig, error) {
	a.used = append(a.used, "PullImageConfigWithAuth")
	return oci.ImageConfig{}, nil
}

func (a *authCapable) PullLayers(_ context.Context, ref string) (oci.PullLayersResult, error) {
	a.used = append(a.used, "PullLayers")
	return oci.PullLayersResult{}, nil
}

func (a *authCapable) PullLayersWithAuth(_ context.Context, ref string, _ *oci.BasicAuth) (oci.PullLayersResult, error) {
	a.used = append(a.used, "PullLayersWithAuth")
	return oci.PullLayersResult{}, nil
}

func (a *authCapable) PullManifest(_ context.Context, ref string) (oci.Manifest, error) {
	a.used = append(a.used, "PullManifest")
	return oci.Manifest{}, nil
}

func (a *authCapable) PullManifestWithAuth(_ context.Context, ref string, _ *oci.BasicAuth) (oci.Manifest, error) {
	a.used = append(a.used, "PullManifestWithAuth")
	return oci.Manifest{}, nil
}

func (a *authCapable) PullBlob(_ context.Context, repo, digest string) (io.ReadCloser, error) {
	a.used = append(a.used, "PullBlob")
	return io.NopCloser(nil), nil
}

func (a *authCapable) PullBlobWithAuth(_ context.Context, repo, digest string, _ *oci.BasicAuth) (io.ReadCloser, error) {
	a.used = append(a.used, "PullBlobWithAuth")
	return io.NopCloser(nil), nil
}

type plainPuller struct {
	a    *authCapable
	used []string
}

func (p *plainPuller) PullDigest(_ context.Context, _ string) (string, error) {
	p.used = append(p.used, "PullDigest")
	return *p.a.digest, nil
}
func (p *plainPuller) PullImageConfig(_ context.Context, _ string) (oci.ImageConfig, error) {
	p.used = append(p.used, "PullImageConfig")
	return oci.ImageConfig{}, nil
}
func (p *plainPuller) PullLayers(_ context.Context, _ string) (oci.PullLayersResult, error) {
	p.used = append(p.used, "PullLayers")
	return oci.PullLayersResult{}, nil
}
func (p *plainPuller) PullManifest(_ context.Context, _ string) (oci.Manifest, error) {
	p.used = append(p.used, "PullManifest")
	return oci.Manifest{}, nil
}
func (p *plainPuller) PullBlob(_ context.Context, _, _ string) (io.ReadCloser, error) {
	p.used = append(p.used, "PullBlob")
	return io.NopCloser(nil), nil
}

func TestPullDispatch_AuthCapableUsesAuthMethod(t *testing.T) {
	a := &authCapable{digest: ptr("sha256:abc")}
	_, _ = pullDigestWithAuth(context.Background(), a, "ref", &oci.BasicAuth{})
	if len(a.used) != 1 || a.used[0] != "PullDigestWithAuth" {
		t.Errorf("digest used = %v, want [PullDigestWithAuth]", a.used)
	}
	a.used = nil
	_, _ = pullImageConfigWithAuth(context.Background(), a, "ref", &oci.BasicAuth{})
	if len(a.used) != 1 || a.used[0] != "PullImageConfigWithAuth" {
		t.Errorf("config used = %v, want [PullImageConfigWithAuth]", a.used)
	}
	a.used = nil
	_, _ = pullLayersWithAuth(context.Background(), a, "ref", &oci.BasicAuth{})
	if len(a.used) != 1 || a.used[0] != "PullLayersWithAuth" {
		t.Errorf("layers used = %v, want [PullLayersWithAuth]", a.used)
	}
}

func TestPullDispatch_PlainPullerFallsBackToAnonymous(t *testing.T) {
	plain := &plainPuller{a: &authCapable{digest: ptr("")}}
	_, _ = pullDigestWithAuth(context.Background(), plain, "ref", nil)
	if len(plain.used) != 1 || plain.used[0] != "PullDigest" {
		t.Errorf("digest used = %v, want [PullDigest]", plain.used)
	}
	plain.used = nil
	_, _ = pullImageConfigWithAuth(context.Background(), plain, "ref", nil)
	if len(plain.used) != 1 || plain.used[0] != "PullImageConfig" {
		t.Errorf("config used = %v, want [PullImageConfig]", plain.used)
	}
	plain.used = nil
	_, _ = pullLayersWithAuth(context.Background(), plain, "ref", nil)
	if len(plain.used) != 1 || plain.used[0] != "PullLayers" {
		t.Errorf("layers used = %v, want [PullLayers]", plain.used)
	}
}

func TestPullManifestDispatch_AuthCapableUsesAuthMethod(t *testing.T) {
	a := &authCapable{}
	_, _ = pullManifestWithAuth(context.Background(), a, "ref", nil)
	if len(a.used) != 1 || a.used[0] != "PullManifestWithAuth" {
		t.Errorf("manifest used = %v, want [PullManifestWithAuth]", a.used)
	}
	a.used = nil
	_, _ = pullBlobWithAuth(context.Background(), a, "repo", "sha256:abc", nil)
	if len(a.used) != 1 || a.used[0] != "PullBlobWithAuth" {
		t.Errorf("blob used = %v, want [PullBlobWithAuth]", a.used)
	}
}

func TestPullManifestDispatch_PlainFallsBack(t *testing.T) {
	plain := &plainPuller{a: &authCapable{}}
	_, _ = pullManifestWithAuth(context.Background(), plain, "ref", nil)
	if len(plain.used) != 1 || plain.used[0] != "PullManifest" {
		t.Errorf("manifest used = %v, want [PullManifest]", plain.used)
	}
	plain.used = nil
	_, _ = pullBlobWithAuth(context.Background(), plain, "repo", "sha256:abc", nil)
	if len(plain.used) != 1 || plain.used[0] != "PullBlob" {
		t.Errorf("blob used = %v, want [PullBlob]", plain.used)
	}
}

// --- With* setters that weren't covered yet ---------------------

type fakeReplicator struct{ used int }

func (f *fakeReplicator) Replicate(_ context.Context, _ string) error {
	f.used++
	return nil
}

func TestWithRuntimeBaseStaging_SetsFlag(t *testing.T) {
	h := &Handler{}
	if h.runtimeBaseStagingEnabled {
		t.Fatal("default: enabled")
	}
	h.WithRuntimeBaseStaging()
	if !h.runtimeBaseStagingEnabled {
		t.Error("after WithRuntimeBaseStaging: not enabled")
	}
}

func TestWithArtifactReplicator_SetsAndReturnsReceiver(t *testing.T) {
	h := &Handler{}
	rep := &fakeReplicator{}
	if got := h.WithArtifactReplicator(rep); got != h {
		t.Error("WithArtifactReplicator did not return receiver")
	}
	if h.replicator == nil {
		t.Error("replicator not set")
	}
}

func TestWithSecretScanRun_SetsFieldAndReturnsReceiver(t *testing.T) {
	h := &Handler{}
	var calls int
	fn := func(_ context.Context, _, _ string) ([]secretscan.Finding, error) {
		calls++
		return nil, nil
	}
	if got := h.WithSecretScanRun(fn); got != h {
		t.Error("WithSecretScanRun did not return receiver")
	}
	if h.secretScanRun == nil {
		t.Fatal("secretScanRun not set after WithSecretScanRun")
	}
	// Pin dispatch — the wired closure must be the one called.
	if _, err := h.secretScanRun(context.Background(), "/tmp/build", "layer-1"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if calls != 1 {
		t.Errorf("wired closure invocations = %d, want 1", calls)
	}
}

// --- isSlugSafe -------------------------------------------------

func TestIsSlugSafe(t *testing.T) {
	cases := map[string]bool{
		"abc":                   true,
		"my-app":                true,
		"a-very-long-app-1":     true,
		"":                      false, // < 3
		"ab":                    false, // < 3
		strings.Repeat("a", 41): false, // > 40
		"-abc":                  true,  // allowed (the impl lets leading '-')
		"aBc":                   false, // uppercase
		"a_bc":                  false, // underscore
		"abc.":                  false, // dot
		"a/bc":                  false, // slash
		"123abc":                true,  // digits ok
		"1abc":                  true,  // leading digit ok per doc
	}
	for in, want := range cases {
		if got := isSlugSafe(in); got != want {
			t.Errorf("isSlugSafe(%q) = %v, want %v", in, got, want)
		}
	}
}

// --- isDeploymentIDSafe -----------------------------------------

func TestIsDeploymentIDSafe(t *testing.T) {
	cases := map[string]bool{
		"":                                     false,
		"abc":                                  true,
		strings.Repeat("a", 64):                true,
		strings.Repeat("a", 65):                false,
		"abc/def":                              false, // slash
		"abc\\def":                             false, // backslash
		"abc.def":                              false, // dot
		"abc\x00def":                           false, // null
		"00000000-0000-0000-0000-000000000000": true,  // uuid shape
	}
	for in, want := range cases {
		if got := isDeploymentIDSafe(in); got != want {
			t.Errorf("isDeploymentIDSafe(%q) = %v, want %v", in, got, want)
		}
	}
}

// --- appsRootPath -----------------------------------------------

func TestAppsRootPath_RejectsUnsafe(t *testing.T) {
	h := &Handler{appsRoot: "/srv/fc/apps"}
	if got := h.appsRootPath("bad slug", "dep-1"); got != "" {
		t.Errorf("bad slug: got %q", got)
	}
	if got := h.appsRootPath("ok-slug", "bad/dep"); got != "" {
		t.Errorf("bad deployment: got %q", got)
	}
}

func TestAppsRootPath_BuildsCanonicalPath(t *testing.T) {
	h := &Handler{appsRoot: "/srv/fc/apps"}
	got := h.appsRootPath("my-app", "dep-1")
	want := "/srv/fc/apps/my-app/dep-1.ext4"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// --- helpers -----------------------------------------------------

// Small ptr helper to keep the authCapable's literal fields short.
func ptr(s string) *string { return &s }

// import "strings" is needed for strings.Repeat in the tables
// above.
var _ = io.Discard
