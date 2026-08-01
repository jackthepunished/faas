// Tests for installationSourceFetcher (PR-H, repo decomposition
// Phase 5).
//
// Pins the contract for the production adapter that bridges
// pkg/githubd.SourceFetcher → stateInstallsAdapter.ForAccount
// (resolve install row) + secretbox.Open (unseal token) +
// gitfetch.Fetcher (download archive).
//
// The tests do NOT exercise a real pgxpool or a real codeload
// download. The installsLookup is a hand-rolled fake; the
// gitfetch.Fetcher is a stub that records the (repo, sha,
// token) triple so the test can assert the unsealed token
// reached the transport without leaking the value into the
// test log.

package main

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"

	"filippo.io/age"

	"github.com/onebox-faas/faas/pkg/gitfetch"
	"github.com/onebox-faas/faas/pkg/githubd"
	"github.com/onebox-faas/faas/pkg/secretbox"
	"github.com/onebox-faas/faas/pkg/state"
)

// fakeInstalls is the installsLookup stub. It returns one fixed
// install row per account; tests populate Err to drive the
// error paths.
type fakeInstalls struct {
	row state.GitHubInstall
	err error
}

func (f *fakeInstalls) ForAccount(_ context.Context, accountID string) (state.GitHubInstall, error) {
	if f.err != nil {
		return state.GitHubInstall{}, f.err
	}
	if accountID != f.row.AccountID {
		return state.GitHubInstall{}, state.ErrNotFound
	}
	return f.row, nil
}

// fakeGitFetch is the gitfetch.Fetcher stub. It records the
// (repo, sha, token) triple so the test can assert the unsealed
// token reached the transport, and returns a Tree wrapping the
// canned fs.FS.
type fakeGitFetch struct {
	fsys       fs.FS
	gotRepo    string
	gotSHA     string
	gotToken   string
	treeClosed bool
	err        error
}

func (f *fakeGitFetch) Fetch(_ context.Context, repo, sha, token string) (gitfetch.Tree, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.gotRepo, f.gotSHA, f.gotToken = repo, sha, token
	return &fakeTree{inner: f, fsys: f.fsys}, nil
}

type fakeTree struct {
	inner *fakeGitFetch
	fsys  fs.FS
}

func (t *fakeTree) FS() fs.FS { return t.fsys }
func (t *fakeTree) Close() error {
	t.inner.treeClosed = true
	return nil
}

// newSealedInstall produces a sealed token blob the fetcher can
// Open. The keypair is generated per-test so a stale fixture
// can't leak across tests.
func newSealedInstall(t *testing.T, plain string) (*age.X25519Identity, []byte) {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	sealed, err := secretbox.SealOne(identity.Recipient(), installTokenSealKey, plain, 1024)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	return identity, sealed
}

// discardLogger keeps the test output quiet. slog.New with an
// io.Discard handler is the project idiom for a noise-free
// test rig.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestInstallationSourceFetcher_HappyPath(t *testing.T) {
	identity, sealed := newSealedInstall(t, "ghs_test_token_abc")
	want := state.GitHubInstall{
		AccountID:      "acct-1",
		InstallationID: 42,
		SealedToken:    sealed,
	}
	gitFetch := &fakeGitFetch{fsys: fstest.MapFS{"x": &fstest.MapFile{Data: []byte("y")}}}
	f := newInstallationSourceFetcher(&fakeInstalls{row: want}, gitFetch, identity, discardLogger())

	tree, err := f.Fetch(context.Background(), "acct-1", 42, "octo/api", "sha-1")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if gitFetch.gotRepo != "octo/api" || gitFetch.gotSHA != "sha-1" {
		t.Errorf("fetch args = (%q, %q)", gitFetch.gotRepo, gitFetch.gotSHA)
	}
	if gitFetch.gotToken != "ghs_test_token_abc" {
		t.Errorf("token reaching fetcher = %q, want ghs_test_token_abc", gitFetch.gotToken)
	}
	// Pin Close idempotency: the production githubd handler
	// defers tree.Close() so a panic in the reconcile path
	// still cleans up. The fake records the call so the
	// assertion catches a "double-Close leaks the temp dir"
	// regression.
	if err := tree.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := tree.Close(); err != nil {
		t.Errorf("second Close: %v (must be idempotent)", err)
	}
	if !gitFetch.treeClosed {
		t.Error("tree.Close() not recorded in fake")
	}
}

func TestInstallationSourceFetcher_AccountNotFound_NoBinding(t *testing.T) {
	identity, _ := newSealedInstall(t, "ghs_t")
	gitFetch := &fakeGitFetch{}
	f := newInstallationSourceFetcher(
		&fakeInstalls{err: state.ErrNotFound},
		gitFetch, identity, discardLogger())

	_, err := f.Fetch(context.Background(), "missing", 42, "octo/api", "sha-1")
	if !githubd.IsNoBinding(err) {
		t.Errorf("err = %v, want ErrNoBinding", err)
	}
	if gitFetch.gotToken != "" {
		t.Errorf("fetch should not be called; got token %q", gitFetch.gotToken)
	}
}

func TestInstallationSourceFetcher_InstallIDMismatch_NoBinding(t *testing.T) {
	// The install row says installation_id=42; the dispatcher
	// passed installID=99. Guard rejects — a malicious push
	// can't use the install row's token under a different
	// install_id claim.
	identity, sealed := newSealedInstall(t, "ghs_t")
	row := state.GitHubInstall{
		AccountID:      "acct-1",
		InstallationID: 42,
		SealedToken:    sealed,
	}
	gitFetch := &fakeGitFetch{}
	f := newInstallationSourceFetcher(&fakeInstalls{row: row}, gitFetch, identity, discardLogger())

	_, err := f.Fetch(context.Background(), "acct-1", 99, "octo/api", "sha-1")
	if !githubd.IsNoBinding(err) {
		t.Errorf("err = %v, want ErrNoBinding", err)
	}
	if gitFetch.gotToken != "" {
		t.Errorf("fetch should not be called on install id mismatch; got token %q", gitFetch.gotToken)
	}
}

func TestInstallationSourceFetcher_PartialInstallRow_NoBinding(t *testing.T) {
	// Defensive: an install row with AccountID="" or
	// InstallationID=0 is incomplete (manual SQL edit, copy-
	// paste error). The fetcher must refuse rather than pass
	// the empty values to the transport.
	identity, _ := newSealedInstall(t, "ghs_t")
	row := state.GitHubInstall{AccountID: "acct-1"} // InstallationID=0
	gitFetch := &fakeGitFetch{}
	f := newInstallationSourceFetcher(&fakeInstalls{row: row}, gitFetch, identity, discardLogger())

	_, err := f.Fetch(context.Background(), "acct-1", 0, "octo/api", "sha-1")
	if !githubd.IsNoBinding(err) {
		t.Errorf("err = %v, want ErrNoBinding for partial row", err)
	}
}

func TestInstallationSourceFetcher_InstallsLookupError_Wraps(t *testing.T) {
	// A non-ErrNotFound error from ForAccount must surface as
	// a wrapped error (NOT ErrNoBinding) so the §12 dashboard
	// counts it as a 5xx not an ignored-200.
	identity, _ := newSealedInstall(t, "ghs_t")
	boom := errors.New("db down")
	gitFetch := &fakeGitFetch{}
	f := newInstallationSourceFetcher(&fakeInstalls{err: boom}, gitFetch, identity, discardLogger())

	_, err := f.Fetch(context.Background(), "acct-1", 42, "octo/api", "sha-1")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if githubd.IsNoBinding(err) {
		t.Errorf("err = %v, must NOT be translated to ErrNoBinding", err)
	}
	if !strings.Contains(err.Error(), "githubd: source fetcher: resolve install") {
		t.Errorf("err = %v, want op prefix", err)
	}
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, must wrap db down", err)
	}
}

func TestInstallationSourceFetcher_EnvelopeMissingKey(t *testing.T) {
	// Sealed token opens fine but the envelope doesn't carry
	// the install-token key. This is a wrong-shape row — likely
	// from a pre-PR-C seal that used a different key. Surface
	// as a wrapped error so the operator notices the migration
	// gap.
	identity, _ := newSealedInstall(t, "ghs_t")
	// Seal with a different key (but the same identity so Open
	// succeeds and we exercise the missing-key path).
	recipient := identity.Recipient()
	sealed, err := secretbox.SealOne(recipient, "OTHER_KEY", "ghs_t", 1024)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	row := state.GitHubInstall{AccountID: "acct-1", InstallationID: 42, SealedToken: sealed}
	gitFetch := &fakeGitFetch{}
	f := newInstallationSourceFetcher(&fakeInstalls{row: row}, gitFetch, identity, discardLogger())

	_, err = f.Fetch(context.Background(), "acct-1", 42, "octo/api", "sha-1")
	if err == nil {
		t.Fatal("expected error for envelope missing key, got nil")
	}
	if githubd.IsNoBinding(err) {
		t.Errorf("err = %v, must not be ErrNoBinding", err)
	}
	if !strings.Contains(err.Error(), "missing key") {
		t.Errorf("err = %v, want missing-key message", err)
	}
}

func TestInstallationSourceFetcher_FetchError_Wraps(t *testing.T) {
	// The transport fails (network error, archive too large).
	// The wrapped error must preserve the underlying sentinel so
	// the webhook handler can branch on it.
	identity, sealed := newSealedInstall(t, "ghs_t")
	row := state.GitHubInstall{AccountID: "acct-1", InstallationID: 42, SealedToken: sealed}
	boom := errors.New("codeload 503")
	gitFetch := &fakeGitFetch{err: boom}
	f := newInstallationSourceFetcher(&fakeInstalls{row: row}, gitFetch, identity, discardLogger())

	_, err := f.Fetch(context.Background(), "acct-1", 42, "octo/api", "sha-1")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "githubd: source fetcher: archive fetch") {
		t.Errorf("err = %v, want op prefix", err)
	}
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, must wrap boom", err)
	}
}
