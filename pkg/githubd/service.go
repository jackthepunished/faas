// githubd service (spec §14 M7.5, ADR-012, ADR-050).
//
// Service is the business-logic core of the githubd daemon. It
// implements the gRPC contract (see pkg/githubdgrpc/server.go) and
// the loopback HTTP webhook handler. PR-H (mega-PR-GH of repo
// decomposition Phase 5) rewrites the push-dispatch path through
// pkg/reconcile.Service so githubd and apid share a single workload-
// mutation primitive. The legacy CreateDeployment function-typed
// seam (slice 7) is retired in this commit.
//
// Service is constructed by cmd/githubd/main.go and shared across
// the gRPC server + the HTTP webhook listener.
package githubd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/onebox-faas/faas/pkg/githubdgrpc"
	"github.com/onebox-faas/faas/pkg/reconcile"
	"github.com/onebox-faas/faas/pkg/state"
)

// AppBindingStore is the slice of store githubd reads to look up
// (repo → app) bindings for incoming pushes. PR-H widens the
// return type from githubdgrpc.AppBinding to state.GitHubBinding so
// the push-dispatch path can read AccountID + InstallID without a
// second round-trip — the bind row already carries both fields,
// and the state adapter (cmd/githubd/state_adapter.go:91) returns
// the full state row verbatim.
//
// PR-H retires the AppBinding struct in githubdgrpc — the only
// remaining caller is cmd/gatewayd/end_to_end_test.go, which gets
// updated to the new return shape in this commit.
type AppBindingStore interface {
	GetAppBinding(ctx context.Context, repoFullName, branch string) (state.GitHubBinding, error)
}

// InstallsLookup is the read seam githubd uses to resolve a
// GitHub App installation row by account ID. The store-backed
// adapter (stateInstallsAdapter.ForAccount) is the production
// implementation; tests inject a stub that returns a fixed
// GitHubInstall with a sealed token.
//
// The lookup is keyed on AccountID (not installation_id) because
// the webhook handler knows the binding's account — and from
// there the install row is one round-trip. Resolving the install
// first and threading its ID through the binding would require
// the webhook handler to scan every binding for a match.
type InstallsLookup interface {
	ForAccount(ctx context.Context, accountID string) (state.GitHubInstall, error)
}

// WriteCheck is the seam githubd uses to push build-phase updates
// back to GitHub. Slice 8 fills this in with the real Checks writer;
// slice 7 leaves it as a stub that records the call into the log
// so the smoke test can assert on the order.
type WriteCheck func(ctx context.Context, repoFullName, commitSHA string, phase githubdgrpc.CheckPhase) error

// Service is the business-logic object shared across the HTTP
// webhook handler and the gRPC server. nil fields fall back to
// safe no-ops (so partial deployments degrade gracefully until
// every dependency is wired). The Reconcile + Source + Installs
// fields are required for HandlePushRequest to reach the
// reconcile step; the production wiring in cmd/githubd sets
// all three from a single boot path.
type Service struct {
	Log        *slog.Logger
	Bindings   AppBindingStore
	Installs   InstallsLookup
	Source     SourceFetcher
	Reconcile  *reconcile.Service
	WriteCheck WriteCheck
}

// NewService builds a Service. Tests inject fakes for the seams;
// production wires the live implementations in cmd/githubd/main.go.
func NewService(log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{Log: log}
}

// HandlePushRequest is the HTTP webhook entry point. It verifies
// the signature (the proxy already did HMAC verify on the edge;
// this is a defense-in-depth check), decodes the body, resolves the
// (repo, branch) → project binding, fetches the source tree, runs
// reposcan.Scan, and dispatches the result through
// reconcile.Service.Reconcile.
//
// Returns:
//
//   - reconcile.Result on success. The webhook HTTP handler
//     serialises Result.Added ∪ Result.Changed into the
//     {status, deployment_id} response body (naive fan-out: every
//     touched app is its own deployment).
//   - ErrNoBinding (sentinel) when the push doesn't match any
//     binding OR the binding's project row is missing. The HTTP
//     handler turns this into a 200 with an ignored-payload body
//     so GitHub does not retry.
//   - ErrIgnored (sentinel) when the production-branch guard
//     tripped. The HTTP handler returns 200 with
//     {status:ignored, reason:feature_branch}.
//   - any other error → 500 (logged with op context).
//
// The source tree's lifecycle is owned by HandlePushRequest: a
// deferred Close runs even on the panic path so a malicious
// archive can't leak temp dirs.
func (s *Service) HandlePushRequest(ctx context.Context, body []byte) (reconcile.Result, error) {
	ev, err := DecodePush(body)
	if err != nil {
		return reconcile.Result{}, err
	}
	branch := refToBranch(ev.Ref)
	if branch == "" {
		// Non-branch ref (tag push, etc.) — slice 7 only handles
		// branch pushes. Tag pushes arrive in a future slice.
		return reconcile.Result{}, ErrNoBinding
	}

	// 1. Resolve the (repo, branch) binding. An empty BindingID
	// is the canonical "no row" shape; ErrNoBinding covers both
	// "store said not-found" and "store returned an empty row".
	binding, err := s.Bindings.GetAppBinding(ctx, ev.Repository.FullName, branch)
	if err != nil {
		return reconcile.Result{}, ErrNoBinding
	}
	if binding.BindingID == "" || binding.AccountID == "" {
		return reconcile.Result{}, ErrNoBinding
	}

	// 2. Resolve the install row. The account → install mapping
	// is one-to-one (every account has at most one GitHub App
	// install). state.ErrNotFound surfaces as ErrNoBinding so the
	// webhook handler renders the same ignored shape.
	install, err := s.Installs.ForAccount(ctx, binding.AccountID)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return reconcile.Result{}, ErrNoBinding
		}
		return reconcile.Result{}, fmt.Errorf("githubd: resolve install: %w", err)
	}
	if install.AccountID == "" || install.InstallationID == 0 {
		// Defensive: the install row exists but is incomplete.
		// The OAuth handshake should never write a partial row,
		// but a manual SQL edit could.
		return reconcile.Result{}, ErrNoBinding
	}

	// 3. Resolve the project row. ProjectByRepo is the
	// push-dispatch lookup from PR-F; missing projects map to
	// ErrNoBinding (a bind row without a project is a soft-
	// deleted binding — ignore the push).
	project, err := s.Reconcile.Store.ProjectByRepo(ctx, binding.AccountID, install.InstallationID, ev.Repository.FullName)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return reconcile.Result{}, ErrNoBinding
		}
		return reconcile.Result{}, fmt.Errorf("githubd: resolve project: %w", err)
	}

	// 4. Fetch the source tree. The fetcher unseals the install
	// token internally (cmd/githubd/source_fetcher.go) and
	// returns a Tree whose Close() removes the temp dir. The
	// deferred Close runs on every exit path including panics
	// in Reconcile (the Go runtime runs deferreds in the panic
	// unwind).
	tree, err := s.Source.Fetch(ctx, binding.AccountID, install.InstallationID, ev.Repository.FullName, ev.After)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("githubd: source fetch: %w", err)
	}
	defer func() { _ = tree.Close() }()

	// 5. Scan + reconcile. reposcan.Scan is wired on the
	// reconcile Service by NewService; tests inject a stub via
	// the Service.Scan field. The production-branch guard
	// returns ErrIgnored when the pushed branch differs from
	// project.ProductionBranch — the caller renders 200-ignored.
	scan, err := s.Reconcile.Scan(tree.FS())
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("githubd: scan: %w", err)
	}
	result, err := s.Reconcile.Reconcile(ctx, project, scan, ev.After, branch)
	if err != nil {
		if errors.Is(err, reconcile.ErrIgnored) {
			// Defensive: ErrIgnored is the Plan-side sentinel;
			// the apply path surfaces feature-branch via
			// Result.WasIgnored instead. The check stays so a
			// future guard that errors out with ErrIgnored
			// gets the same translation.
			result.WasIgnored = true
			return result, ErrIgnored
		}
		return result, err
	}
	// Production-branch guard trips without returning an error;
	// reconcile marks Result.WasIgnored so the caller can render
	// the 200-ignored body. Translate to ErrIgnored so the HTTP
	// handler can branch on the typed sentinel.
	if result.WasIgnored {
		return result, ErrIgnored
	}

	// 6. Best-effort: queue the queued check on GitHub. Errors
	// here don't block the deploy from being recorded locally.
	if s.WriteCheck != nil {
		if werr := s.WriteCheck(ctx, ev.Repository.FullName, ev.After, githubdgrpc.CheckPhaseQueued); werr != nil {
			s.Log.Warn("githubd: write check", "err", werr, "repo", ev.Repository.FullName, "sha", ev.After)
		}
	}
	s.Log.Info("githubd push → reconcile",
		"repo", ev.Repository.FullName, "branch", branch,
		"sha", ev.After, "binding", binding.BindingID,
		"added", len(result.Added), "changed", len(result.Changed),
		"removed", len(result.Removed), "pusher", ev.Pusher.Name)
	return result, nil
}

// ErrNoBinding is returned by HandlePushRequest when the push
// doesn't match any registered binding. The HTTP handler turns
// this into a 200 with an ignored-payload body.
var ErrNoBinding = errNoBinding{}

type errNoBinding struct{}

func (errNoBinding) Error() string { return "githubd: no binding for push" }

// IsNoBinding reports whether err is the no-binding sentinel.
func IsNoBinding(err error) bool {
	return errors.As(err, new(errNoBinding))
}

// ErrIgnored is returned by HandlePushRequest when the pushed
// branch is not the project's production branch and the guard
// short-circuited reconcile. The HTTP handler turns this into a
// 200 with {status:ignored, reason:feature_branch} so GitHub does
// not retry.
var ErrIgnored = errIgnored{}

type errIgnored struct{}

func (errIgnored) Error() string { return "githubd: push to non-production branch" }

// IsIgnored reports whether err is the ignored sentinel.
func IsIgnored(err error) bool {
	return errors.As(err, new(errIgnored))
}

// refToBranch converts "refs/heads/main" → "main". Returns "" for
// refs that aren't a branch (e.g. refs/tags/v1.0 — slice 7 only
// handles branch pushes; tag pushes arrive in a future slice).
func refToBranch(ref string) string {
	const prefix = "refs/heads/"
	if len(ref) <= len(prefix) {
		return ""
	}
	if ref[:len(prefix)] != prefix {
		return ""
	}
	return ref[len(prefix):]
}

// WebhookHTTPHandler returns an http.Handler that serves
// POST /webhooks/github. Today it returns 503 because the proxy
// (cmd/gatewayd) verifies the signature and forwards; this handler
// is loopback-only and reachable from the gatewayd reverse proxy.
// A future PR may let githubd stand up its own listener when
// gatewayd isn't on the same host (not in v1.0).
func (s *Service) WebhookHTTPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "githubd: webhook arrives via gatewayd's edge-verifying proxy", http.StatusNotImplemented)
	})
}
