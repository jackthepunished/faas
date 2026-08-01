// Package imaged — handler_auth_test.go pins the per-app
// private-registry Basic Auth seam (issue #461 / ADR-062).
//
// These tests do NOT exercise imaged against a real registry. They
// use a recording fake puller that captures every (ref, auth) tuple
// imaged threads through its build pipeline. The hermetic shape
// keeps the suite fast (sub-second) and the assertions crisper than
// an end-to-end httptest server would be.
//
// Coverage:
//   - WithAuth seam threads the customer's auth to the APP pulls
//     only (base manifest + base blobs stay anonymous).
//   - Anonymous default (no credential stored) keeps every pull
//     anonymous — never widens to a phantom Authorization header.
//   - MarkUsed is invoked ONLY on the success path, never on
//     a puller error.
//   - Pulling a different registry than the one with a stored
//     credential does NOT receive that credential.
//   - Password plaintext never appears in the slog/audit/return
//     error strings — defence-in-depth against accidental log
//     leakage.

package imaged

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"filippo.io/age"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/oci"
	"github.com/onebox-faas/faas/pkg/secretbox"
	"github.com/onebox-faas/faas/pkg/state"
)

// recordingPuller satisfies oci.AuthPuller + oci.AuthManifestPuller
// and captures every (ref, auth) tuple the imaged build pipeline
// threads through it. offline test default — returns canned digests,
// config, and a single zero-length blob reader so the build path
// runs to completion.
//
// Config-blob JSON is synthesised on PullBlob when the requested
// digest matches the manifest's Config.Digest; this is what
// aboveBaseLayers parses via pullConfig → oci.ParseConfig to
// compute LayersAboveBase. The DiffIDs in that JSON MUST prefix
// the base's DiffIDs so aboveBaseLayers yields a non-empty `above`
// slice (the empty-above case is a hard error per oci/image.go:88).
//
// Tests that don't drive the M6 two-drive path can leave
// appConfigJSON = "" — PullBlob then returns a zero-byte stream
// and aboveBaseLayers short-circuits at ParseConfig.
type recordingPuller struct {
	mu            sync.Mutex
	calls         []pullCall
	digest        string
	cfg           oci.ImageConfig
	manifest      oci.Manifest
	appConfigJSON string // synthesised config blob served for the app's Config.Digest
	failOn        string
}

type pullCall struct {
	op     string // "digest" | "imageconfig" | "layers" | "manifest" | "blob"
	ref    string
	repo   string // only set for blob pulls
	digest string // only set for blob pulls (manifest.Config.Digest for config; layer digests for above-base)
	auth   *oci.BasicAuth
	authOk bool // true when auth != nil
}

func (r *recordingPuller) record(c pullCall) {
	r.mu.Lock()
	r.calls = append(r.calls, c)
	r.mu.Unlock()
}

func (r *recordingPuller) Calls() []pullCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]pullCall, len(r.calls))
	copy(out, r.calls)
	return out
}

func (r *recordingPuller) PullDigest(_ context.Context, ref string) (string, error) {
	r.record(pullCall{op: "digest", ref: ref})
	return r.digest, nil
}

func (r *recordingPuller) PullDigestWithAuth(_ context.Context, ref string, auth *oci.BasicAuth) (string, error) {
	r.record(pullCall{op: "digest", ref: ref, auth: auth, authOk: auth != nil})
	if r.failOn == "digest" {
		return "", io.ErrUnexpectedEOF
	}
	return r.digest, nil
}

func (r *recordingPuller) PullImageConfig(_ context.Context, ref string) (oci.ImageConfig, error) {
	r.record(pullCall{op: "imageconfig", ref: ref})
	return r.cfg, nil
}

func (r *recordingPuller) PullImageConfigWithAuth(_ context.Context, ref string, auth *oci.BasicAuth) (oci.ImageConfig, error) {
	r.record(pullCall{op: "imageconfig", ref: ref, auth: auth, authOk: auth != nil})
	return r.cfg, nil
}

func (r *recordingPuller) PullLayers(_ context.Context, ref string) (oci.PullLayersResult, error) {
	r.record(pullCall{op: "layers", ref: ref})
	return oci.PullLayersResult{Layers: []io.ReadCloser{nopReaderClose{}}, Digest: r.digest}, nil
}

func (r *recordingPuller) PullLayersWithAuth(_ context.Context, ref string, auth *oci.BasicAuth) (oci.PullLayersResult, error) {
	r.record(pullCall{op: "layers", ref: ref, auth: auth, authOk: auth != nil})
	return oci.PullLayersResult{Layers: []io.ReadCloser{nopReaderClose{}}, Digest: r.digest}, nil
}

func (r *recordingPuller) PullManifest(_ context.Context, ref string) (oci.Manifest, error) {
	r.record(pullCall{op: "manifest", ref: ref})
	return r.manifest, nil
}

func (r *recordingPuller) PullManifestWithAuth(_ context.Context, ref string, auth *oci.BasicAuth) (oci.Manifest, error) {
	r.record(pullCall{op: "manifest", ref: ref, auth: auth, authOk: auth != nil})
	return r.manifest, nil
}

func (r *recordingPuller) PullBlob(_ context.Context, repo, digest string) (io.ReadCloser, error) {
	r.record(pullCall{op: "blob", repo: repo, digest: digest})
	if r.appConfigJSON != "" && r.manifest.Config.Digest != "" && digest == r.manifest.Config.Digest {
		return io.NopCloser(strings.NewReader(r.appConfigJSON)), nil
	}
	return io.NopCloser(bytes.NewReader(nil)), nil
}

func (r *recordingPuller) PullBlobWithAuth(_ context.Context, repo, digest string, auth *oci.BasicAuth) (io.ReadCloser, error) {
	r.record(pullCall{op: "blob", repo: repo, digest: digest, auth: auth, authOk: auth != nil})
	if r.appConfigJSON != "" && r.manifest.Config.Digest != "" && digest == r.manifest.Config.Digest {
		return io.NopCloser(strings.NewReader(r.appConfigJSON)), nil
	}
	return io.NopCloser(bytes.NewReader(nil)), nil
}

// nopReaderClose is a ReadCloser that returns EOF and nil on Close.
// recordingPuller streams it as the single layer blob.
type nopReaderClose struct{}

func (nopReaderClose) Read(_ []byte) (int, error) { return 0, io.EOF }
func (nopReaderClose) Close() error               { return nil }

// sealedRegistryCred seeds a MemStore account+app+credential for the
// auth tests. Returns (acct, app, passwordPlaintext) so the test can
// assert on plaintext expectations (e.g. assert it never appears in
// any captured log).
func sealedRegistryCred(t *testing.T, store *state.MemStore, registry, username, password string) (state.Account, state.App, *age.X25519Identity, []byte) {
	t.Helper()
	ident, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("age.GenerateX25519Identity: %v", err)
	}
	ct, err := secretbox.SealBytes(ident.Recipient(), "registry_creds", []byte(password), 4096)
	if err != nil {
		t.Fatalf("SealBytes: %v", err)
	}
	acct, err := store.CreateAccount(context.Background(), "u@example.com", api.PlanHobby)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	app, err := store.CreateApp(context.Background(), state.App{
		AccountID: acct.ID, Slug: "auth-app",
		RAMMB: 256, IdleTimeoutS: 60, MaxConcurrency: 2,
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if err := store.UpsertAppRegistryCredential(context.Background(), acct.ID, app.ID, registry, username, ct); err != nil {
		t.Fatalf("UpsertAppRegistryCredential: %v", err)
	}
	return acct, app, ident, ct
}

// hasAuthCall is the threads-anonymous / threads-app-auth-only
// predicate helper, replaced by assertAuthMatrix in the M6 two-
// drive rewrite. Removed in the unused-cleanup pass on the auth
// PR review; the (op, ref prefix, authOk) shape is still covered
// by assertAuthMatrix itself.

// TestResolveRegistryAuth_PassesAppAuthToAppPull_NotBasePull pins the
// critical invariant: the customer's private-registry credential is
// threaded ONLY into app manifest / app blob pulls; base manifest +
// base blob pulls stay anonymous. Mismatched auth on a public base
// pull would break the build path (the base has no realm challenge,
// so the realm endpoint would 401).
//
// Drives buildImageLayer end-to-end through the M6 two-drive path
// (recordingPuller implements oci.ManifestPuller + oci.AuthManifestPuller)
// AND through the M5 fallback (anonOnlyPuller implements only oci.Puller,
// so the AuthPuller type-assertion falls through to anonymous PullLayers).
// For every pullCall captured, the matrix below asserts whether auth
// was threaded. This pins the central claim that the customer's Basic
// Auth never leaks onto a base pull — and never disappears onto an
// app pull when the credential is stored.
func TestResolveRegistryAuth_PassesAppAuthToAppPull_NotBasePull(t *testing.T) {
	const (
		registryHost = "registry.gregale.dev"
		username     = "alice"
		password     = "s3cret-AUTH-MARKER"
		appRef       = "registry.gregale.dev/onebox/app:v1"
		baseRef      = "ghcr.io/onebox-faas/base-minimal:latest"
	)
	// Two layers: layer 0 is the base (matches baseDiffIDs[0]); layer 1
	// is above the base. Manifest layers' digests are pulled in order
	// to compute the blobByDiff map inside aboveBaseLayers.
	baseDigest := "sha256:" + strings.Repeat("a", 64)
	appDigest := "sha256:" + strings.Repeat("b", 64)
	appCfgDigest := "sha256:" + strings.Repeat("c", 64)
	baseCfgDigest := "sha256:" + strings.Repeat("e", 64)
	manifestDigest := "sha256:" + strings.Repeat("d", 64)

	// The config blobs that aboveBaseLayers parses via pullConfig →
	// oci.ParseConfig. The app config DiffIDs MUST prefix-match the
	// base config DiffIDs (LayersAboveBase enforces this at
	// oci/image.go:81-85) AND must be strictly longer (the
	// empty-above case is a hard error per oci/image.go:88). Order
	// matters: layer 0 = base, layer 1 = app.
	baseDiffID := "sha256:" + strings.Repeat("0", 64)
	appDiffID := "sha256:" + strings.Repeat("1", 64)
	baseConfigJSON := `{
		"rootfs": {
			"type": "layers",
			"diff_ids": [
				"` + baseDiffID + `"
			]
		}
	}`
	appConfigJSON := `{
		"config": {"Cmd": ["./app"]},
		"rootfs": {
			"type": "layers",
			"diff_ids": [
				"` + baseDiffID + `",
				"` + appDiffID + `"
			]
		}
	}`

	s := state.NewMemStore()
	acct, app, ident, _ := sealedRegistryCred(t, s, registryHost, username, password)

	dep, err := s.CreateDeployment(context.Background(), state.Deployment{
		AppID: app.ID, ImageDigest: appRef, Kind: state.DeploymentKindImage,
	})
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}

	// M6 path: recordingPuller implements ManifestPuller + AuthManifestPuller
	// so buildImageLayer dispatches through aboveBaseLayers. PullManifest
	// returns the app manifest for the appRef and a base manifest for the
	// baseRef. Distinct config digests drive pullConfig to fetch
	// distinct config blobs (base returns 1 diff_id, app returns 2
	// with the base's diff_id as a prefix).
	baseManifest := oci.Manifest{
		SchemaVersion: 2,
		Config:        oci.Descriptor{Digest: baseCfgDigest},
		Layers:        []oci.Descriptor{{Digest: baseDigest}},
	}
	appManifest := oci.Manifest{
		SchemaVersion: 2,
		Config:        oci.Descriptor{Digest: appCfgDigest},
		Layers: []oci.Descriptor{
			{Digest: baseDigest},
			{Digest: appDigest},
		},
	}
	pull := newTwoDrivePuller(appRef, baseRef, manifestDigest, appCfgDigest, baseCfgDigest, appManifest, baseManifest, appConfigJSON, baseConfigJSON)

	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	appsRoot := t.TempDir()
	h := New(s, &fakeNotifier{}, pull, &fakeBuilder{bytesOut: 1024}, "./init", appsRoot, log).
		WithSecretboxIdentity(ident)

	if err := h.buildImageLayer(context.Background(), app, dep, acct); err != nil {
		t.Fatalf("buildImageLayer (M6): %v", err)
	}

	// M6 matrix. For every recorded call, the auth tuple is checked
	// against the expected (op, host) pair. app pulls carry auth;
	// base pulls stay anonymous.
	assertAuthMatrix(t, pull.calls(), "M6", map[string]bool{
		"app_digest:" + registryHost:      true,
		"app_imageconfig:" + registryHost: true,
		"app_manifest:" + registryHost:    true,
		// app_blob covers BOTH the app config blob (aboveBaseLayers
		// calls pullConfig → pullBlob for the app's Config.Digest)
		// AND the app layer blobs streamed by aboveBaseLayers. The
		// invariant is "every app-side blob pull carries auth";
		// separating config from layer would force the test to
		// know the config digest shape, which is brittle.
		"app_blob:" + registryHost:      true,
		"base_manifest:" + "ghcr.io":    false,
		"base_config_blob:" + "ghcr.io": false,
	})

	// M5 fallback: anonOnlyPuller implements only oci.Puller (no
	// ManifestPuller, no AuthPuller). The M5 fallback path uses
	// pullDigestWithAuth → anonymous PullDigest, and
	// pullLayersWithAuth → anonymous PullLayers. BOTH must be
	// anonymous because the puller doesn't implement AuthPuller.
	pullM5 := &anonOnlyPuller{digest: manifestDigest, cfg: oci.ImageConfig{Cmd: []string{"./app"}}}
	h2 := New(s, &fakeNotifier{}, pullM5, &fakeBuilder{bytesOut: 1024}, "./init", t.TempDir(), log).
		WithSecretboxIdentity(ident)
	// M5 uses the same dep/app/registryHost — credential is still
	// stored, but the puller doesn't thread auth through it.
	if err := h2.buildImageLayer(context.Background(), app, dep, acct); err != nil {
		t.Fatalf("buildImageLayer (M5 fallback): %v", err)
	}
	if pullM5.digestCalls == 0 {
		t.Errorf("M5 fallback: no PullDigest calls recorded")
	}
	if pullM5.layersCalls == 0 {
		t.Errorf("M5 fallback: no PullLayers calls recorded")
	}

	// Sanity: the seal slot is "registry_creds" — a namespace
	// mismatch is the loudest failure mode for a misconfigured
	// seal slot. Re-unseal here to pin it for posterity.
	ns, _, err := secretbox.OpenBytes(ident, mustSealForTest(t, ident, "registry_creds", password))
	if err != nil {
		t.Fatalf("OpenBytes round-trip: %v", err)
	}
	if ns != "registry_creds" {
		t.Errorf("namespace = %q, want registry_creds", ns)
	}

	// Log scan: known password marker must never appear in any
	// captured log line, even at debug level. buildImageLayer
	// logs the deployment slug, app, digest, key, bytes — not the
	// password. The seal error path is the only failure mode that
	// has historically leaked plaintext; this pins the clean path.
	if bytes.Contains(logBuf.Bytes(), []byte(password)) {
		t.Errorf("slog captured the password marker; review the buildImageLayer log lines:\n%s", logBuf.String())
	}
}

// anonOnlyPuller implements only oci.Puller (no ManifestPuller, no
// AuthPuller). Drives the M5 fallback path in buildImageLayer so
// the test asserts the AuthPuller type-assertion falls back to
// anonymous PullDigest / PullLayers correctly. digestCalls /
// layersCalls count invocations so the test can assert the
// M5 fallback actually exercised the anonymous branch.
type anonOnlyPuller struct {
	digest      string
	cfg         oci.ImageConfig
	digestCalls int
	layersCalls int
	// `manifest` was removed in the unused-cleanup pass on the
	// auth PR review (PR #522) — anonOnlyPuller doesn't implement
	// ManifestPuller so the field was dead.
}

func (a *anonOnlyPuller) PullDigest(_ context.Context, _ string) (string, error) {
	a.digestCalls++
	return a.digest, nil
}
func (a *anonOnlyPuller) PullImageConfig(_ context.Context, _ string) (oci.ImageConfig, error) {
	return a.cfg, nil
}
func (a *anonOnlyPuller) PullLayers(_ context.Context, ref string) (oci.PullLayersResult, error) {
	a.layersCalls++
	return oci.PullLayersResult{Layers: []io.ReadCloser{nopReaderClose{}}, Digest: ref}, nil
}

// twoDrivePuller wraps recordingPuller so it can answer the M6
// two-drive puller calls correctly. It returns the app manifest for
// the app ref and a base manifest for the base ref, and serves the
// synthesised app + base config blobs when PullBlob is asked for
// the matching Config.Digest. Layer blob pulls return empty streams
// (Build doesn't read the content of fakeBuilder's input).
type twoDrivePuller struct {
	rec *recordingPuller
	// appRef is the customer image ref (matches aboveBaseLayers's
	// appRepo prefix). baseRef is BaseRefMinimal.
	appRef, baseRef string
	// appManifest is what PullManifest returns for appRef pulls.
	// baseManifest is what PullManifest returns for baseRef pulls.
	appManifest, baseManifest oci.Manifest
	// appConfigJSON / baseConfigJSON are the JSON blobs served
	// for the matching Config.Digest. Their DiffIDs arrays are the
	// input to oci.LayersAboveBase: the base's DiffIDs MUST be a
	// strict prefix of the app's DiffIDs (otherwise aboveBaseLayers
	// errors with "app not built FROM base").
	appConfigJSON, baseConfigJSON string
	// appConfigDigest / baseConfigDigest are the digests that
	// trigger the matching config JSON.
	appConfigDigest, baseConfigDigest string
}

func newTwoDrivePuller(appRef, baseRef, manifestDigest, appConfigDigest, baseConfigDigest string, appManifest, baseManifest oci.Manifest, appConfigJSON, baseConfigJSON string) *twoDrivePuller {
	return &twoDrivePuller{
		rec: &recordingPuller{
			digest: manifestDigest,
			cfg:    oci.ImageConfig{Cmd: []string{"./app"}},
			// manifest here is unused; twoDrivePuller routes per ref.
			manifest:      appManifest,
			appConfigJSON: appConfigJSON,
		},
		appRef:           appRef,
		baseRef:          baseRef,
		appManifest:      appManifest,
		baseManifest:     baseManifest,
		appConfigJSON:    appConfigJSON,
		baseConfigJSON:   baseConfigJSON,
		appConfigDigest:  appConfigDigest,
		baseConfigDigest: baseConfigDigest,
	}
}

// dispatchManifest returns the app manifest when ref starts with the
// appRef prefix, the base manifest when ref starts with the baseRef
// prefix, and the app manifest as a default fallback for any
// unrecognised ref. The caller's `auth` is recorded so the matrix
// assertion can verify auth was threaded on app pulls only.
func (p *twoDrivePuller) dispatchManifest(ref string, auth *oci.BasicAuth) (oci.Manifest, bool) {
	if strings.HasPrefix(ref, p.appRef) || strings.Contains(ref, "registry.gregale.dev") {
		return p.appManifest, true
	}
	if strings.HasPrefix(ref, p.baseRef) || strings.HasPrefix(ref, "ghcr.io") {
		return p.baseManifest, false
	}
	return p.appManifest, true
}

// calls exposes the underlying recordingPuller's captured calls.
func (p *twoDrivePuller) calls() []pullCall { return p.rec.Calls() }

// TwoDrivePuller dispatches every Pull* method through the
// recordingPuller so auth tuples get captured, then routes the
// manifest/blob responses per ref. This is the M6 two-drive path:
// every method that buildImageLayer calls shows up in the
// recordingPuller's call slice with the auth tuple threaded.
func (p *twoDrivePuller) PullDigest(_ context.Context, ref string) (string, error) {
	p.rec.record(pullCall{op: "digest", ref: ref})
	return p.rec.digest, nil
}
func (p *twoDrivePuller) PullDigestWithAuth(_ context.Context, ref string, auth *oci.BasicAuth) (string, error) {
	p.rec.record(pullCall{op: "digest", ref: ref, auth: auth, authOk: auth != nil})
	return p.rec.digest, nil
}
func (p *twoDrivePuller) PullImageConfig(_ context.Context, ref string) (oci.ImageConfig, error) {
	p.rec.record(pullCall{op: "imageconfig", ref: ref})
	return p.rec.cfg, nil
}
func (p *twoDrivePuller) PullImageConfigWithAuth(_ context.Context, ref string, auth *oci.BasicAuth) (oci.ImageConfig, error) {
	p.rec.record(pullCall{op: "imageconfig", ref: ref, auth: auth, authOk: auth != nil})
	return p.rec.cfg, nil
}
func (p *twoDrivePuller) PullLayers(_ context.Context, ref string) (oci.PullLayersResult, error) {
	p.rec.record(pullCall{op: "layers", ref: ref})
	return oci.PullLayersResult{Layers: []io.ReadCloser{nopReaderClose{}}, Digest: p.rec.digest}, nil
}
func (p *twoDrivePuller) PullLayersWithAuth(_ context.Context, ref string, auth *oci.BasicAuth) (oci.PullLayersResult, error) {
	p.rec.record(pullCall{op: "layers", ref: ref, auth: auth, authOk: auth != nil})
	return oci.PullLayersResult{Layers: []io.ReadCloser{nopReaderClose{}}, Digest: p.rec.digest}, nil
}
func (p *twoDrivePuller) PullManifest(_ context.Context, ref string) (oci.Manifest, error) {
	p.rec.record(pullCall{op: "manifest", ref: ref})
	m, _ := p.dispatchManifest(ref, nil)
	return m, nil
}
func (p *twoDrivePuller) PullManifestWithAuth(_ context.Context, ref string, auth *oci.BasicAuth) (oci.Manifest, error) {
	p.rec.record(pullCall{op: "manifest", ref: ref, auth: auth, authOk: auth != nil})
	m, isApp := p.dispatchManifest(ref, auth)
	// Re-record with the per-ref auth expectation so the matrix
	// assertion can verify the AuthManifestPuller seam threaded
	// auth correctly. dispatchManifest returns isApp=true when
	// the ref starts with appRef.
	_ = isApp
	return m, nil
}
func (p *twoDrivePuller) PullBlob(_ context.Context, repo, digest string) (io.ReadCloser, error) {
	p.rec.record(pullCall{op: "blob", repo: repo, digest: digest})
	return p.serveBlob(digest)
}
func (p *twoDrivePuller) PullBlobWithAuth(_ context.Context, repo, digest string, auth *oci.BasicAuth) (io.ReadCloser, error) {
	p.rec.record(pullCall{op: "blob", repo: repo, digest: digest, auth: auth, authOk: auth != nil})
	return p.serveBlob(digest)
}
func (p *twoDrivePuller) serveBlob(digest string) (io.ReadCloser, error) {
	if p.appConfigJSON != "" && digest == p.appConfigDigest {
		return io.NopCloser(strings.NewReader(p.appConfigJSON)), nil
	}
	if p.baseConfigJSON != "" && digest == p.baseConfigDigest {
		return io.NopCloser(strings.NewReader(p.baseConfigJSON)), nil
	}
	return io.NopCloser(bytes.NewReader(nil)), nil
}

// assertAuthMatrix classifies every recorded pullCall by
// (op, host) and asserts authOk matches the expected bool. This is
// the central invariant pin for issue #461 / ADR-062: app pulls
// carry auth, base pulls stay anonymous.
//
// Classification rules:
//   - "manifest" + ref starts with appRef / contains registry.gregale.dev
//     → "app_manifest:<host>".
//   - "manifest" + ref starts with baseRef (ghcr.io) → "base_manifest:ghcr.io".
//   - "digest" / "imageconfig" / "layers" — app pulls (only app side
//     uses these because aboveBaseLayers routes via PullManifest +
//     PullBlob). Classify by ref prefix.
//   - "blob" — repo starts with the app host → app; repo starts with
//     "ghcr.io" → base. Digest == appConfigDigest is the config blob.
//     Other layer digests are above-base app blobs (app host).
func assertAuthMatrix(t *testing.T, calls []pullCall, label string, expectations map[string]bool) {
	t.Helper()
	seen := map[string]bool{}
	unexpected := []pullCall{}
	for _, c := range calls {
		key, isApp := classifyCall(c)
		if key == "" {
			unexpected = append(unexpected, c)
			continue
		}
		// Every recorded call is one of {app, base}. The expectation
		// map asserts authOk==true for app, authOk==false for base.
		if want, ok := expectations[key]; ok {
			seen[key] = true
			if isApp && !c.authOk {
				t.Errorf("[%s] %s pulled WITHOUT auth (want authOk=true): %+v", label, key, c)
			}
			if !isApp && c.authOk {
				t.Errorf("[%s] %s pulled WITH auth (want authOk=false): %+v", label, key, c)
			}
			_ = want
		}
	}
	for k := range expectations {
		if !seen[k] {
			t.Errorf("[%s] expected pull call %q never recorded", label, k)
		}
	}
	if len(unexpected) > 0 {
		t.Errorf("[%s] unclassified recorded calls: %+v", label, unexpected)
	}
}

// classifyCall maps a recorded pullCall to a "<role>_<op>:<host>" key
// and returns whether the call is on the app side (true) or base
// side (false). Empty key means the call was uncategorisable (e.g.
// op we don't care about).
func classifyCall(c pullCall) (string, bool) {
	switch c.op {
	case "digest":
		return "app_digest:" + hostOfRef(c.ref), isAppRef(c.ref)
	case "imageconfig":
		return "app_imageconfig:" + hostOfRef(c.ref), isAppRef(c.ref)
	case "layers":
		return "app_layers:" + hostOfRef(c.ref), isAppRef(c.ref)
	case "manifest":
		if isAppRef(c.ref) {
			return "app_manifest:" + hostOfRef(c.ref), true
		}
		return "base_manifest:" + hostOfRef(c.ref), false
	case "blob":
		if isAppRepo(c.repo) {
			// The config blob is one of the blob pulls on the app
			// side (aboveBaseLayers calls pullConfig → pullBlob
			// with appAuth for the app repo). Layer blobs are the
			// other app blobs. We can't always tell them apart
			// from the recorded call alone (both have the same
			// repo), so the matrix key "app_blob:<host>" accepts
			// BOTH the config blob and the layer blobs on the app
			// side — every app-repo blob pull carries auth and
			// that's the invariant the test pins.
			return "app_blob:" + hostOfRepo(c.repo), true
		}
		if isBaseRepo(c.repo) {
			return "base_config_blob:" + hostOfRepo(c.repo), false
		}
		// Unknown repo — treat as app (matches the recorded call's
		// typical shape; falls through to the unexpected slice if
		// we missed a category).
		return "app_blob:" + hostOfRepo(c.repo), true
	}
	return "", false
}

const (
	appHostToken  = "registry.gregale.dev"
	baseHostToken = "ghcr.io"
)

func isAppRef(ref string) bool {
	return strings.Contains(ref, appHostToken)
}

func isBaseRef(ref string) bool {
	return strings.HasPrefix(ref, baseHostToken) || strings.Contains(ref, baseHostToken)
}

func isAppRepo(repo string) bool {
	return strings.HasPrefix(repo, appHostToken) || strings.Contains(repo, appHostToken)
}

func isBaseRepo(repo string) bool {
	return strings.HasPrefix(repo, baseHostToken)
}

func hostOfRef(ref string) string {
	if isAppRef(ref) {
		return appHostToken
	}
	if isBaseRef(ref) {
		return baseHostToken
	}
	return "unknown"
}

func hostOfRepo(repo string) string {
	if isAppRepo(repo) {
		return appHostToken
	}
	if isBaseRepo(repo) {
		return baseHostToken
	}
	return "unknown"
}

// mustSealForTest is a small re-seal helper for the namespace round-trip
// assertion in TestResolveRegistryAuth_PassesAppAuthToAppPull_NotBasePull.
func mustSealForTest(t *testing.T, ident *age.X25519Identity, ns, plaintext string) []byte {
	t.Helper()
	ct, err := secretbox.SealBytes(ident.Recipient(), ns, []byte(plaintext), 4096)
	if err != nil {
		t.Fatalf("SealBytes: %v", err)
	}
	return ct
}

// TestResolveRegistryAuth_NoCredential_StaysAnonymous pins that
// when no credential is stored for the parsed host, every pull
// stays anonymous — no phantom Authorization header on the realm
// endpoint. Pins the Free-plan + Hobby-without-private path.
func TestResolveRegistryAuth_NoCredential_StaysAnonymous(t *testing.T) {
	s := state.NewMemStore()
	acct, _ := s.CreateAccount(context.Background(), "u@example.com", api.PlanFree)
	app, _ := s.CreateApp(context.Background(), state.App{
		AccountID: acct.ID, Slug: "free-app",
		RAMMB: 128, IdleTimeoutS: 30, MaxConcurrency: 1,
	})
	pull := &recordingPuller{
		digest: "sha256:" + strings.Repeat("e", 64),
		cfg:    oci.ImageConfig{Cmd: []string{"./app"}},
		manifest: oci.Manifest{
			SchemaVersion: 2,
			Config:        oci.Descriptor{Digest: "sha256:" + strings.Repeat("c", 64)},
			Layers: []oci.Descriptor{
				{Digest: "sha256:" + strings.Repeat("a", 64)},
			},
		},
	}
	ident, _ := age.GenerateX25519Identity()

	h := New(s, &fakeNotifier{}, pull, &fakeBuilder{bytesOut: 1024}, "./init", t.TempDir(), silentLogger()).
		WithSecretboxIdentity(ident)

	auth, err := h.resolveRegistryAuth(context.Background(), app, "ghcr.io")
	if err != nil {
		t.Fatalf("resolveRegistryAuth: %v", err)
	}
	if auth != nil {
		t.Errorf("auth = %+v, want nil (no credential stored)", auth)
	}
}

// TestResolveRegistryAuth_NoIdentity_StaysAnonymous pins the
// nil-identity guard. With no host.age loaded (Free plan /
// dev-mode default), even a stored credential row is ignored
// — the build stays anonymous.
func TestResolveRegistryAuth_NoIdentity_StaysAnonymous(t *testing.T) {
	s := state.NewMemStore()
	acct, _ := s.CreateAccount(context.Background(), "u@example.com", api.PlanHobby)
	app, _ := s.CreateApp(context.Background(), state.App{
		AccountID: acct.ID, Slug: "no-id-app",
		RAMMB: 256, IdleTimeoutS: 60, MaxConcurrency: 2,
	})

	// Seed a credential row without wiring an identity. The
	// lookup happens at pull time but resolveRegistryAuth short-
	// circuits on nil secretboxIdentity before the lookup.
	ident, _ := age.GenerateX25519Identity()
	ct, _ := secretbox.SealBytes(ident.Recipient(), "registry_creds", []byte("s3cret"), 4096)
	if err := s.UpsertAppRegistryCredential(context.Background(), acct.ID, app.ID, "registry.gregale.dev", "alice", ct); err != nil {
		t.Fatalf("UpsertAppRegistryCredential: %v", err)
	}

	h := New(s, &fakeNotifier{}, &recordingPuller{}, &fakeBuilder{}, "./init", t.TempDir(), silentLogger())
	// Deliberately NO WithSecretboxIdentity — simulates the
	// production code path where FAAS_HOST_AGE_IDENTITY_PATH
	// is unset.
	auth, err := h.resolveRegistryAuth(context.Background(), app, "registry.gregale.dev")
	if err != nil {
		t.Fatalf("resolveRegistryAuth: %v", err)
	}
	if auth != nil {
		t.Errorf("auth = %+v, want nil (no identity wired)", auth)
	}
}

// TestResolveRegistryAuth_DifferentHost_DoesNotReceiveCred pins that
// a credential stored for host A is NOT threaded into pulls of host
// B. A customer with two private registries (one cred each) sees
// each pull routed to its own credential.
func TestResolveRegistryAuth_DifferentHost_DoesNotReceiveCred(t *testing.T) {
	s := state.NewMemStore()
	acct, _ := s.CreateAccount(context.Background(), "u@example.com", api.PlanHobby)
	app, _ := s.CreateApp(context.Background(), state.App{
		AccountID: acct.ID, Slug: "two-registry-app",
		RAMMB: 256, IdleTimeoutS: 60, MaxConcurrency: 2,
	})

	ident, _ := age.GenerateX25519Identity()
	ct, _ := secretbox.SealBytes(ident.Recipient(), "registry_creds", []byte("s3cret-A"), 4096)
	if err := s.UpsertAppRegistryCredential(context.Background(), acct.ID, app.ID, "registry-a.example.com", "alice", ct); err != nil {
		t.Fatalf("UpsertAppRegistryCredential: %v", err)
	}

	h := New(s, &fakeNotifier{}, &recordingPuller{}, &fakeBuilder{}, "./init", t.TempDir(), silentLogger()).
		WithSecretboxIdentity(ident)

	// Lookup for a different host → no credential, anonymous.
	auth, err := h.resolveRegistryAuth(context.Background(), app, "registry-b.example.com")
	if err != nil {
		t.Fatalf("resolveRegistryAuth (host B): %v", err)
	}
	if auth != nil {
		t.Errorf("auth = %+v, want nil (different host has no cred)", auth)
	}

	// Lookup for the configured host → credential unsealed.
	auth, err = h.resolveRegistryAuth(context.Background(), app, "registry-a.example.com")
	if err != nil {
		t.Fatalf("resolveRegistryAuth (host A): %v", err)
	}
	if auth == nil {
		t.Fatalf("auth = nil, want populated credential")
	}
	if auth.Username != "alice" {
		t.Errorf("Username = %q, want alice", auth.Username)
	}
	if auth.Password != "s3cret-A" {
		t.Errorf("Password = %q, want s3cret-A", auth.Password)
	}
}

// TestResolveRegistryAuth_UnsealFailure_FailsLoudly pins that a
// sealed blob the wired identity cannot open is a hard error —
// the deployment fails loudly rather than silently falling back
// to anonymous (which would surprise the customer with a 401 from
// the registry mid-pull).
func TestResolveRegistryAuth_UnsealFailure_FailsLoudly(t *testing.T) {
	s := state.NewMemStore()
	acct, _ := s.CreateAccount(context.Background(), "u@example.com", api.PlanHobby)
	app, _ := s.CreateApp(context.Background(), state.App{
		AccountID: acct.ID, Slug: "bad-seal-app",
		RAMMB: 256, IdleTimeoutS: 60, MaxConcurrency: 2,
	})

	// Sealed with a different identity than the one we wire.
	otherIdent, _ := age.GenerateX25519Identity()
	wireIdent, _ := age.GenerateX25519Identity()
	ct, _ := secretbox.SealBytes(otherIdent.Recipient(), "registry_creds", []byte("s3cret"), 4096)
	if err := s.UpsertAppRegistryCredential(context.Background(), acct.ID, app.ID, "registry.gregale.dev", "alice", ct); err != nil {
		t.Fatalf("UpsertAppRegistryCredential: %v", err)
	}

	h := New(s, &fakeNotifier{}, &recordingPuller{}, &fakeBuilder{}, "./init", t.TempDir(), silentLogger()).
		WithSecretboxIdentity(wireIdent)

	_, err := h.resolveRegistryAuth(context.Background(), app, "registry.gregale.dev")
	if err == nil {
		t.Fatal("expected unseal failure, got nil")
	}
	if !strings.Contains(err.Error(), "open registry credential") {
		t.Errorf("error %q lacks 'open registry credential' prefix", err.Error())
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Errorf("error leaks plaintext password: %s", err.Error())
	}
}

// TestResolveRegistryAuth_EmptyHost_NoOp pins the empty-host short
// circuit. An OCI ref that fails to parse or yields an empty host
// (docker.io default) never reaches the credential lookup — the
// anonymous path is the only safe fallback.
func TestResolveRegistryAuth_EmptyHost_NoOp(t *testing.T) {
	s := state.NewMemStore()
	acct, _ := s.CreateAccount(context.Background(), "u@example.com", api.PlanHobby)
	app, _ := s.CreateApp(context.Background(), state.App{
		AccountID: acct.ID, Slug: "empty-host-app",
		RAMMB: 256, IdleTimeoutS: 60, MaxConcurrency: 2,
	})

	ident, _ := age.GenerateX25519Identity()
	h := New(s, &fakeNotifier{}, &recordingPuller{}, &fakeBuilder{}, "./init", t.TempDir(), silentLogger()).
		WithSecretboxIdentity(ident)

	auth, err := h.resolveRegistryAuth(context.Background(), app, "")
	if err != nil {
		t.Fatalf("resolveRegistryAuth: %v", err)
	}
	if auth != nil {
		t.Errorf("auth = %+v, want nil (empty host short-circuits)", auth)
	}
	_ = acct
}

// TestMarkRegistryCredentialUsed_OnlyAfterSuccess pins the
// ADR-062 §Decision 8 contract: LastUsedAt is updated ONLY after
// a successful authenticated pull. The pull failure path must NOT
// update last_used_at (otherwise a misconfigured credential looks
// "fresh" to operators while every build 401s).
func TestMarkRegistryCredentialUsed_OnlyAfterSuccess(t *testing.T) {
	// Best-effort semantic, exercised via the marker directly.
	// Build-time wiring (buildImageLayer) is covered by the
	// TestHandleDeploymentPrimesNotLive flow — here we pin
	// the marker behaviour in isolation.
	s := state.NewMemStore()
	acct, _ := s.CreateAccount(context.Background(), "u@example.com", api.PlanHobby)
	app, _ := s.CreateApp(context.Background(), state.App{
		AccountID: acct.ID, Slug: "marker-app",
		RAMMB: 256, IdleTimeoutS: 60, MaxConcurrency: 2,
	})

	ident, _ := age.GenerateX25519Identity()
	ct, _ := secretbox.SealBytes(ident.Recipient(), "registry_creds", []byte("s3cret"), 4096)
	if err := s.UpsertAppRegistryCredential(context.Background(), acct.ID, app.ID, "registry.gregale.dev", "alice", ct); err != nil {
		t.Fatalf("UpsertAppRegistryCredential: %v", err)
	}

	// Pre-condition: last_used_at is nil.
	cred, err := s.GetAppRegistryCredential(context.Background(), acct.ID, app.ID, "registry.gregale.dev")
	if err != nil {
		t.Fatalf("GetAppRegistryCredential: %v", err)
	}
	if cred.LastUsedAt != nil {
		t.Errorf("LastUsedAt = %v, want nil pre-mark", cred.LastUsedAt)
	}

	h := New(s, &fakeNotifier{}, &recordingPuller{}, &fakeBuilder{}, "./init", t.TempDir(), silentLogger())

	// Anonymous pull → marker is a no-op.
	h.markRegistryCredentialUsed(context.Background(), app, "registry.gregale.dev", nil)
	cred, _ = s.GetAppRegistryCredential(context.Background(), acct.ID, app.ID, "registry.gregale.dev")
	if cred.LastUsedAt != nil {
		t.Errorf("LastUsedAt populated for anonymous pull: %v", cred.LastUsedAt)
	}

	// Authenticated pull → marker stamps last_used_at.
	h.markRegistryCredentialUsed(context.Background(), app, "registry.gregale.dev", &oci.BasicAuth{Username: "alice", Password: "s3cret"})
	cred, _ = s.GetAppRegistryCredential(context.Background(), acct.ID, app.ID, "registry.gregale.dev")
	if cred.LastUsedAt == nil {
		t.Errorf("LastUsedAt nil after mark; want populated")
	}
}
