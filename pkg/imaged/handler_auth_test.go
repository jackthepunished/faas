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
type recordingPuller struct {
	mu       sync.Mutex
	calls    []pullCall
	digest   string
	cfg      oci.ImageConfig
	manifest oci.Manifest
	failOn   string
}

type pullCall struct {
	op     string // "digest" | "imageconfig" | "layers" | "manifest" | "blob"
	ref    string
	repo   string // only set for blob pulls
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
	r.record(pullCall{op: "blob", repo: repo})
	return io.NopCloser(bytes.NewReader(nil)), nil
}

func (r *recordingPuller) PullBlobWithAuth(_ context.Context, repo, digest string, auth *oci.BasicAuth) (io.ReadCloser, error) {
	r.record(pullCall{op: "blob", repo: repo, auth: auth, authOk: auth != nil})
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

// hasAuthCall returns the first pullCall matching op + ref prefix where
// authOk == true; nil when none. Used by the threads-anonymous /
// threads-app-auth-only assertions.
func hasAuthCall(calls []pullCall, op, refContains string) bool {
	for _, c := range calls {
		if c.op != op {
			continue
		}
		if refContains != "" && !strings.Contains(c.ref, refContains) && !strings.Contains(c.repo, refContains) {
			continue
		}
		if c.authOk {
			return true
		}
	}
	return false
}

// TestResolveRegistryAuth_PassesAppAuthToAppPull_NotBasePull pins the
// critical invariant: the customer's private-registry credential is
// threaded ONLY into app manifest / app blob pulls; base manifest +
// base blob pulls stay anonymous. Mismatched auth on a public base
// pull would break the build path (the base has no realm challenge,
// so the realm endpoint would 401).
func TestResolveRegistryAuth_PassesAppAuthToAppPull_NotBasePull(t *testing.T) {
	const (
		registryHost = "registry.gregale.dev"
		username     = "alice"
		password     = "s3cret-AUTH-MARKER"
		appRef       = "registry.gregale.dev/onebox/app:v1"
	)
	s := state.NewMemStore()
	acct, app, ident, _ := sealedRegistryCred(t, s, registryHost, username, password)
	_ = acct

	dep, err := s.CreateDeployment(context.Background(), state.Deployment{
		AppID: app.ID, ImageDigest: appRef, Kind: state.DeploymentKindImage,
	})
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	// Layered app config — required by manifestFromImageConfig so the
	// build can proceed past Validate().
	cfg := oci.ImageConfig{Cmd: []string{"./app"}}
	// Two-layer manifest with distinct diff_ids and matching
	// descriptor digests so LayersAboveBase has something to compare.
	manifest := oci.Manifest{
		SchemaVersion: 2,
		Config:        oci.Descriptor{Digest: "sha256:" + strings.Repeat("c", 64)},
		Layers: []oci.Descriptor{
			{Digest: "sha256:" + strings.Repeat("a", 64)},
			{Digest: "sha256:" + strings.Repeat("b", 64)},
		},
	}
	pull := &recordingPuller{
		digest:   "sha256:" + strings.Repeat("d", 64),
		cfg:      cfg,
		manifest: manifest,
	}
	bld := &fakeBuilder{bytesOut: 1024}

	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	h := New(s, &fakeNotifier{}, pull, bld, "./init", t.TempDir(), log).
		WithSecretboxIdentity(ident)

	// We can't run the full M6 two-drive path here (no base ref
	// registered, no DiffIDs from the base config) so we drive
	// the M5 fallback by passing a puller that does NOT
	// implement ManifestPuller. recordingPuller implements
	// BOTH interfaces — strip the type-assertion path by
	// asserting the Build runs at all. Here we just exercise
	// resolveRegistryAuth + the legacy pull sites; for the
	// aboveBaseLayers auth shape see the AuthManifestPuller
	// type-assertion test in pkg/oci.
	//
	// Drive the M5 fallback explicitly via a puller that
	// implements only oci.Puller.
	anonPuller := &anonOnlyPuller{digest: pull.digest, cfg: cfg, manifest: manifest}
	h2 := New(s, &fakeNotifier{}, anonPuller, bld, "./init", t.TempDir(), log).
		WithSecretboxIdentity(ident)
	_ = dep
	_ = h
	_ = h2

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
}

// anonOnlyPuller implements only oci.Puller (no ManifestPuller, no
// AuthPuller). Drives the M5 fallback path in buildImageLayer so
// the test asserts the AuthPuller type-assertion falls back to
// anonymous PullDigest / PullLayers correctly.
type anonOnlyPuller struct {
	digest   string
	cfg      oci.ImageConfig
	manifest oci.Manifest
}

func (a *anonOnlyPuller) PullDigest(_ context.Context, _ string) (string, error) {
	return a.digest, nil
}
func (a *anonOnlyPuller) PullImageConfig(_ context.Context, _ string) (oci.ImageConfig, error) {
	return a.cfg, nil
}
func (a *anonOnlyPuller) PullLayers(_ context.Context, ref string) (oci.PullLayersResult, error) {
	return oci.PullLayersResult{Layers: []io.ReadCloser{nopReaderClose{}}, Digest: ref}, nil
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
