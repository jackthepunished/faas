package githubd

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/onebox-faas/faas/pkg/audit"
	"github.com/onebox-faas/faas/pkg/githubdgrpc"
	"github.com/onebox-faas/faas/pkg/reconcile"
	"github.com/onebox-faas/faas/pkg/reposcan"
	"github.com/onebox-faas/faas/pkg/state"
)

// stubBindings is a hand-rolled fake of AppBindingStore. PR-H widens
// the return type to state.GitHubBinding so the push-dispatch path
// can read AccountID + InstallID without a second round-trip. Tests
// populate the bind row via the same shape the production adapter
// returns, so the fake stays a faithful mirror.
type stubBindings struct {
	byRepo map[string]state.GitHubBinding // key: "owner/repo|branch"
	err    error
}

func (s *stubBindings) GetAppBinding(_ context.Context, repo, branch string) (state.GitHubBinding, error) {
	if s.err != nil {
		return state.GitHubBinding{}, s.err
	}
	return s.byRepo[repo+"|"+branch], nil
}

// stubInstalls is the fake InstallsLookup. The githubd push path
// resolves the durable install row by account ID (PR-H's chosen
// key). Errors here surface as 5xx; ErrNotFound surfaces as
// ErrNoBinding so the test suite can pin the no-binding fall-
// through.
type stubInstalls struct {
	byAccount map[string]state.GitHubInstall
	err       error
}

func (s *stubInstalls) ForAccount(_ context.Context, accountID string) (state.GitHubInstall, error) {
	if s.err != nil {
		return state.GitHubInstall{}, s.err
	}
	return s.byAccount[accountID], nil
}

// stubSourceTree satisfies SourceTree. The reconciler pulls repos
// off the FS so the fake exposes the same fstest.MapFS a real
// archive extraction would produce. Close is a no-op — tests don't
// allocate a temp dir.
type stubSourceTree struct {
	fsys fs.FS
}

func (s *stubSourceTree) FS() fs.FS    { return s.fsys }
func (s *stubSourceTree) Close() error { return nil }

// stubSource is the fake SourceFetcher. It returns the test's
// canned MapFS for any (installID, repo, sha) triple; tests that
// need a fetch failure inject an err.
type stubSource struct {
	fsys fs.FS
	err  error
}

func (s *stubSource) Fetch(_ context.Context, _ string, _ int64, _, _ string) (SourceTree, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &stubSourceTree{fsys: s.fsys}, nil
}

// testRig bundles the dependencies Service.HandlePushRequest needs
// so individual tests can override the slice they care about
// (e.g. drive the feature-branch guard by setting a non-prod
// branch via the test body, drive a Source error by setting
// source.err).
type testRig struct {
	mem     *state.MemStore
	auditor *audit.Auditor
	rec     *reconcile.Service
	acct    string
	install int64
}

func newRig(t *testing.T, scanFn func(fs.FS) (reposcan.Result, error)) *testRig {
	t.Helper()
	mem := state.NewMemStore()
	aud := audit.New(mem, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, "githubd_test")
	rec := reconcile.NewService(mem, aud, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if scanFn != nil {
		rec.Scan = scanFn
	}
	acct, err := mem.CreateAccount(context.Background(), "octo@example.com", "hobby")
	if err != nil {
		t.Fatalf("seed account: %v", err)
	}
	return &testRig{mem: mem, auditor: aud, rec: rec, acct: acct.ID, install: 42}
}

// seedProject seeds the (installID, repo) → project row the push-
// dispatch path resolves in step 3. ScanSource is set to the
// Tier-1 "single" tier so the happy-path test (which stubs
// reposcan.Scan → Tier=1) doesn't trip the scan-source-stability
// guard. The ReconcileErrorBubbles test overrides the tier upward
// after seeding.
func (r *testRig) seedProject(t *testing.T, repo, prodBranch string) state.Project {
	t.Helper()
	p, err := r.mem.CreateProject(context.Background(), state.Project{
		AccountID:        r.acct,
		Slug:             "demo",
		InstallID:        r.install,
		RepoFullName:     repo,
		ProductionBranch: prodBranch,
		ScanSource:       state.ProjectScanSource("single"),
	})
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return p
}

func happyScan() reposcan.Result {
	return reposcan.Result{
		Workloads: []reposcan.Workload{{Class: reposcan.ClassHTTP, Name: "api", RootDir: "."}},
		Managed:   []reposcan.Managed{},
		Tier:      1,
	}
}

func newServiceForRig(r *testRig) *Service {
	svc := NewService(slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.Bindings = &stubBindings{byRepo: map[string]state.GitHubBinding{
		"octo/api|main": {BindingID: "b-1", AccountID: r.acct, InstallID: r.install, RepoFullName: "octo/api", ProductionBranch: "main"},
	}}
	svc.Installs = &stubInstalls{byAccount: map[string]state.GitHubInstall{
		r.acct: {AccountID: r.acct, InstallationID: r.install, DefaultBranch: "main"},
	}}
	svc.Source = &stubSource{fsys: fstest.MapFS{
		"docker-compose.yml": &fstest.MapFile{Data: []byte("version: '3'\nservices:\n  api:\n    build: .\n")},
	}}
	svc.Reconcile = r.rec
	return svc
}

func TestHandlePushRequest_HappyPath(t *testing.T) {
	// Seed the project with the matching scan tier (single) so the
	// scan-source-stability guard doesn't trip. The stub Scan
	// returns a Tier=1 result (one compose file → single).
	rig := newRig(t, func(_ fs.FS) (reposcan.Result, error) { return happyScan(), nil })
	rig.seedProject(t, "octo/api", "main")
	svc := newServiceForRig(rig)
	var checkRepo, checkSHA string
	var checkPhase githubdgrpc.CheckPhase
	svc.WriteCheck = func(_ context.Context, repo, sha string, phase githubdgrpc.CheckPhase) error {
		checkRepo, checkSHA, checkPhase = repo, sha, phase
		return nil
	}
	body := []byte(`{"ref":"refs/heads/main","after":"cafebabe","repository":{"full_name":"octo/api","name":"api"},"pusher":{"name":"alice"}}`)
	result, err := svc.HandlePushRequest(context.Background(), body)
	if err != nil {
		t.Fatalf("HandlePushRequest: %v", err)
	}
	if len(result.Added) != 1 {
		t.Errorf("result.Added = %d, want 1", len(result.Added))
	}
	if checkRepo != "octo/api" || checkSHA != "cafebabe" || checkPhase != githubdgrpc.CheckPhaseQueued {
		t.Errorf("WriteCheck args = (%q,%q,%v)", checkRepo, checkSHA, checkPhase)
	}
}

func TestHandlePushRequest_NoBindingIsSilent(t *testing.T) {
	rig := newRig(t, func(_ fs.FS) (reposcan.Result, error) { return happyScan(), nil })
	svc := NewService(slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.Bindings = &stubBindings{byRepo: map[string]state.GitHubBinding{}}
	svc.Installs = &stubInstalls{byAccount: map[string]state.GitHubInstall{rig.acct: {AccountID: rig.acct, InstallationID: rig.install}}}
	svc.Source = &stubSource{fsys: fstest.MapFS{}}
	svc.Reconcile = rig.rec
	body := []byte(`{"ref":"refs/heads/main","after":"deadbeef","repository":{"full_name":"octo/api","name":"api"},"pusher":{"name":"alice"}}`)
	_, err := svc.HandlePushRequest(context.Background(), body)
	if !IsNoBinding(err) {
		t.Errorf("err = %v, want ErrNoBinding", err)
	}
}

func TestHandlePushRequest_FeatureBranchIgnored(t *testing.T) {
	// Seed a project whose production_branch="main". Push to
	// refs/heads/feature/x — bind matches via the feature/x row,
	// reconcile's productionBranchOnly guard trips, HandlePushRequest
	// returns ErrIgnored.
	rig := newRig(t, func(_ fs.FS) (reposcan.Result, error) { return happyScan(), nil })
	rig.seedProject(t, "octo/api", "main")
	svc := newServiceForRig(rig)
	// Override the binding map to include the feature/x branch.
	svc.Bindings = &stubBindings{byRepo: map[string]state.GitHubBinding{
		"octo/api|feature/x": {BindingID: "b-1", AccountID: rig.acct, InstallID: rig.install, RepoFullName: "octo/api", ProductionBranch: "main"},
	}}
	body := []byte(`{"ref":"refs/heads/feature/x","after":"x","repository":{"full_name":"octo/api","name":"api"},"pusher":{"name":"alice"}}`)
	_, err := svc.HandlePushRequest(context.Background(), body)
	if !IsIgnored(err) {
		t.Errorf("err = %v, want ErrIgnored", err)
	}
}

func TestHandlePushRequest_SourceFetchFailure(t *testing.T) {
	// Seed project + binding + install so the Source error fires
	// before any ProjectByRepo fall-through.
	rig := newRig(t, func(_ fs.FS) (reposcan.Result, error) { return happyScan(), nil })
	rig.seedProject(t, "octo/api", "main")
	svc := newServiceForRig(rig)
	want := errors.New("codeload down")
	svc.Source = &stubSource{err: want}
	body := []byte(`{"ref":"refs/heads/main","after":"x","repository":{"full_name":"octo/api","name":"api"},"pusher":{"name":"alice"}}`)
	_, err := svc.HandlePushRequest(context.Background(), body)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "githubd: source fetch") {
		t.Errorf("err = %v, want op prefix 'githubd: source fetch'", err)
	}
}

func TestHandlePushRequest_ReconcileErrorBubbles(t *testing.T) {
	// Drive a real reconcile-package error: a scan-source downgrade
	// trips the scan-source-stability guard and Reconcile returns
	// a typed error. The githubd handler does NOT translate it to
	// ErrNoBinding or ErrIgnored, so the error bubbles.
	rig := newRig(t, func(_ fs.FS) (reposcan.Result, error) { return happyScan(), nil })
	// Seed with a higher-tier scan source (compose) so the stub
	// Scan's Tier=1 result is a downgrade.
	_, err := rig.mem.CreateProject(context.Background(), state.Project{
		AccountID:        rig.acct,
		Slug:             "demo",
		InstallID:        rig.install,
		RepoFullName:     "octo/api",
		ProductionBranch: "main",
		ScanSource:       state.ProjectScanSourceCompose,
	})
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	svc := newServiceForRig(rig)
	body := []byte(`{"ref":"refs/heads/main","after":"x","repository":{"full_name":"octo/api","name":"api"},"pusher":{"name":"alice"}}`)
	_, err = svc.HandlePushRequest(context.Background(), body)
	if err == nil {
		t.Fatal("expected scan-source-downgrade error, got nil")
	}
	if IsNoBinding(err) || IsIgnored(err) {
		t.Errorf("err = %v, must not be translated to ErrNoBinding/ErrIgnored", err)
	}
	if !errors.Is(err, state.ErrScanSourceDowngrade) {
		t.Errorf("err = %v, want state.ErrScanSourceDowngrade", err)
	}
}

func TestHandlePushRequest_TagIsIgnored(t *testing.T) {
	rig := newRig(t, func(_ fs.FS) (reposcan.Result, error) { return happyScan(), nil })
	svc := newServiceForRig(rig)
	body := []byte(`{"ref":"refs/tags/v1.0","after":"x","repository":{"full_name":"octo/api","name":"api"},"pusher":{"name":"alice"}}`)
	_, err := svc.HandlePushRequest(context.Background(), body)
	if !IsNoBinding(err) {
		t.Errorf("tag push → err = %v, want ErrNoBinding", err)
	}
}

func TestWebhookHTTPHandler_IsLoopbackOnly(t *testing.T) {
	svc := NewService(slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader("{}"))
	rr := httptest.NewRecorder()
	svc.WebhookHTTPHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotImplemented {
		t.Errorf("direct handler status = %d, want 501", rr.Code)
	}
}

// recordingEnqueuer is the BuildEnqueuer stub for PR-GH.5
// fan-out tests. It records every call and returns the
// configured buildID (or error).
type recordingEnqueuer struct {
	buildID string
	err     error
	calls   []enqueueCall
}

type enqueueCall struct {
	accountID string
	appID     string
	commitSHA string
}

func (r *recordingEnqueuer) Enqueue(_ context.Context, accountID, appID, commitSHA string) (string, error) {
	r.calls = append(r.calls, enqueueCall{accountID: accountID, appID: appID, commitSHA: commitSHA})
	if r.err != nil {
		return "", r.err
	}
	return r.buildID, nil
}

// TestHandlePushRequest_FanOut_HappyPath is the PR-GH.5
// gate. The stub Scan returns one Tier-1 workload, so the
// reconcile populates Result.Added with one app. The fan-out
// must enqueue exactly one build with the right (accountID,
// appID, commitSHA) triple.
func TestHandlePushRequest_FanOut_HappyPath(t *testing.T) {
	rig := newRig(t, func(_ fs.FS) (reposcan.Result, error) { return happyScan(), nil })
	rig.seedProject(t, "octo/api", "main")
	svc := newServiceForRig(rig)
	enq := &recordingEnqueuer{buildID: "build-1"}
	svc.Enqueuer = enq
	body := []byte(`{"ref":"refs/heads/main","after":"sha-1","repository":{"full_name":"octo/api","name":"api"},"pusher":{"name":"alice"}}`)

	result, err := svc.HandlePushRequest(context.Background(), body)
	if err != nil {
		t.Fatalf("HandlePushRequest: %v", err)
	}
	if len(enq.calls) != 1 {
		t.Fatalf("enqueue calls = %d, want 1", len(enq.calls))
	}
	if len(result.BuildIDs) != 1 || result.BuildIDs[0] != "build-1" {
		t.Errorf("result.BuildIDs = %v, want [build-1]", result.BuildIDs)
	}
	if enq.calls[0].commitSHA != "sha-1" {
		t.Errorf("enqueue commitSHA = %q, want sha-1", enq.calls[0].commitSHA)
	}
	if enq.calls[0].accountID != rig.acct {
		t.Errorf("enqueue accountID = %q, want %q", enq.calls[0].accountID, rig.acct)
	}
}

// TestHandlePushRequest_FanOut_EmptyBuilds_NoEnqueue:
// the reconcile returns no Added/Changed (nothing to deploy)
// so the fan-out is a no-op. The HTTP response carries
// build_ids: [].
func TestHandlePushRequest_FanOut_EmptyBuilds_NoEnqueue(t *testing.T) {
	// Empty scan triggers the never-empty alert via
	// Result.Alerts — no app rows added. The fan-out sees
	// an empty Added+Changed list and does not enqueue.
	rig := newRig(t, func(_ fs.FS) (reposcan.Result, error) {
		return reposcan.Result{Workloads: nil, Managed: nil, Tier: 1}, nil
	})
	rig.seedProject(t, "octo/api", "main")
	svc := newServiceForRig(rig)
	enq := &recordingEnqueuer{buildID: "build-1"}
	svc.Enqueuer = enq
	body := []byte(`{"ref":"refs/heads/main","after":"sha-1","repository":{"full_name":"octo/api","name":"api"},"pusher":{"name":"alice"}}`)

	result, err := svc.HandlePushRequest(context.Background(), body)
	if err != nil {
		t.Fatalf("HandlePushRequest: %v", err)
	}
	if len(enq.calls) != 0 {
		t.Errorf("enqueue calls = %d, want 0 (no apps touched)", len(enq.calls))
	}
	if len(result.BuildIDs) != 0 {
		t.Errorf("result.BuildIDs = %v, want []", result.BuildIDs)
	}
}

// TestHandlePushRequest_FanOut_EnqueueError_SoftFail drives
// the partial-success path. The reconcile returns one
// added app, the enqueuer returns an error. The push is
// still successful (200 OK with empty build_ids); the
// error is logged but not propagated. This is the
// "best-effort fan-out" contract: failing the whole push
// because one of N builds was rejected is worse for the
// customer than logging + continuing.
func TestHandlePushRequest_FanOut_EnqueueError_SoftFail(t *testing.T) {
	rig := newRig(t, func(_ fs.FS) (reposcan.Result, error) { return happyScan(), nil })
	rig.seedProject(t, "octo/api", "main")
	svc := newServiceForRig(rig)
	enq := &recordingEnqueuer{err: errors.New("queue full")}
	svc.Enqueuer = enq
	body := []byte(`{"ref":"refs/heads/main","after":"sha-1","repository":{"full_name":"octo/api","name":"api"},"pusher":{"name":"alice"}}`)

	result, err := svc.HandlePushRequest(context.Background(), body)
	if err != nil {
		t.Fatalf("HandlePushRequest returned error: %v (must be soft-fail)", err)
	}
	if len(result.BuildIDs) != 0 {
		t.Errorf("result.BuildIDs = %v, want [] (enqueue failed)", result.BuildIDs)
	}
}

// TestNoopEnqueuer_SyntheticID pins the noop enqueuer's
// deterministic-but-unique-per-(appID, commitSHA) ID. The
// ID is opaque to the caller but must differ across both
// axes so the build log can correlate to the push.
func TestNoopEnqueuer_SyntheticID(t *testing.T) {
	e := NewNoopEnqueuer(slog.New(slog.NewTextHandler(io.Discard, nil)))
	id1, err := e.Enqueue(context.Background(), "acct-1", "app-1", "sha-1")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	id2, err := e.Enqueue(context.Background(), "acct-1", "app-2", "sha-1")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if id1 == id2 {
		t.Errorf("id1 == id2 (%q); different apps must produce different IDs", id1)
	}
	id3, err := e.Enqueue(context.Background(), "acct-1", "app-1", "sha-2")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if id1 == id3 {
		t.Errorf("id1 == id3 (%q); different commits must produce different IDs", id1)
	}
}
