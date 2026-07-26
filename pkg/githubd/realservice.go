// RealService — slice 8. Implements the full pkg/githubdgrpc.Service
// contract (8 methods) using the OAuth + token cache + Checks writer
// from this slice. Slice 7's Service skeleton is the inbound-webhook
// side; RealService is the dashboard/OAuth side. Both share the
// githubdgrpc.Service interface via embedding UnimplementedService.
//
// PR-B: the in-memory `bindings` map becomes a read-through cache
// for the durable BindingsStore (cmd-side adapter backed by
// pkg/state.PgStore). Install state (`installs` map) stays in memory
// for now — it tracks the OAuth exchange handshake and is PR-C scope
// (it survives across binds within a process lifetime, which is
// the contract the v1.0 dashboard needs).
package githubd

import (
	"context"
	"fmt"
	"sync"

	"github.com/onebox-faas/faas/pkg/githubdgrpc"
	"github.com/onebox-faas/faas/pkg/state"
)

// defaultProductionBranch is the branch fallback used when the
// dashboard's bind form omits one. GitHub's "main" is the post-2020
// default; older installs default to "master" via the install
// payload (slice 9's dashboard form captures that).
const defaultProductionBranch = "main"

// RealService is the slice-8 production implementation of
// githubdgrpc.Service. It composes:
//   - AppAuth (RS256 JWT minting + installation-token exchange)
//   - TokenCache (singleflight, proactive refresh)
//   - ChecksAPI (POST /repos/{o}/{r}/check-runs)
//   - BindingsStore (Postgres-backed via cmd-side adapter, PR-B)
//   - in-memory install-state map (OAuth handshake, PR-C scope)
type RealService struct {
	githubdgrpc.UnimplementedService

	Auth   *AppAuth
	Tokens *TokenCache
	Checks *ChecksAPI
	Store  BindingsStore

	// bindingsCache is keyed by accountID → appID → state.GitHubBinding.
	// Demoted from source-of-truth to read-through cache by PR-B.
	// On BindAppRepo, we write to Store first; on success we
	// populate the cache. On GetAppBinding, we hit the cache; on
	// miss we fall back to Store.GetForApp and rebuild.
	bindingsCacheMu sync.RWMutex
	bindingsCache   map[string]map[string]state.GitHubBinding

	// installs is keyed by accountID → install state. Pre-PR-B
	// the install state was reconstructable from the same in-memory
	// map as the bindings; PR-B leaves it in memory because the
	// handshake still has to be tracked per-process (the install
	// token has its own lifetime separate from the durable bind
	// row). A future PR-C will move this too.
	installsMu sync.RWMutex
	installs   map[string]installState
}

// installState mirrors the githubdgrpc.InstallState enum plus the
// installation_id (string for cross-language stability; GitHub's
// integer IDs fit comfortably).
type installState struct {
	State     githubdgrpc.InstallState
	InstID    string
	DefBranch string
}

// NewRealService builds a RealService. auth, tokens, checks, and
// store may all be nil — the service refuses calls that need them.
// The bindings cache is always allocated; an empty store means
// every BindAppRepo call returns an error (the dashboard's bind
// flow is fail-closed without a configured store).
func NewRealService(auth *AppAuth, tokens *TokenCache, checks *ChecksAPI, store BindingsStore) *RealService {
	return &RealService{
		Auth:          auth,
		Tokens:        tokens,
		Checks:        checks,
		Store:         store,
		bindingsCache: map[string]map[string]state.GitHubBinding{},
		installs:      map[string]installState{},
	}
}

// GetInstallState returns the install state for the given account.
// Returns UNSPECIFIED for accounts that haven't connected.
func (s *RealService) GetInstallState(accountID string) (githubdgrpc.InstallState, string, string, error) {
	s.installsMu.RLock()
	defer s.installsMu.RUnlock()
	st, ok := s.installs[accountID]
	if !ok {
		return githubdgrpc.InstallStateUnspecified, "", "", nil
	}
	return st.State, st.InstID, st.DefBranch, nil
}

// ExchangeOAuthCode persists the install state for an account.
// The "code → installation" exchange happens via the dashboard's
// own redirect (slice 9 wires the CLI command). This stub returns
// the new installation_id once the caller hands it to us; the
// real exchange happens in the dashboard handler.
func (s *RealService) ExchangeOAuthCode(accountID, installationID, defaultBranch string) (string, error) {
	if accountID == "" {
		return "", fmt.Errorf("githubd: accountID required")
	}
	if installationID == "" {
		return "", fmt.Errorf("githubd: installationID required")
	}
	s.installsMu.Lock()
	s.installs[accountID] = installState{
		State:     githubdgrpc.InstallStateInstalled,
		InstID:    installationID,
		DefBranch: defaultBranch,
	}
	s.installsMu.Unlock()
	return installationID, nil
}

// ListInstallableRepos returns the repos the installation can see.
// Requires a non-nil Auth + Tokens.
func (s *RealService) ListInstallableRepos(accountID string) ([]githubdgrpc.Repo, error) {
	if s.Auth == nil || s.Tokens == nil {
		return nil, fmt.Errorf("githubd: OAuth not configured")
	}
	s.installsMu.RLock()
	st, ok := s.installs[accountID]
	s.installsMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("githubd: account %s has no installation", accountID)
	}
	var instID int64
	if _, err := fmt.Sscanf(st.InstID, "%d", &instID); err != nil {
		return nil, fmt.Errorf("githubd: invalid installation id %q", st.InstID)
	}
	tok, err := s.Tokens.Token(context.Background(), instID)
	if err != nil {
		return nil, fmt.Errorf("githubd: install token: %w", err)
	}
	repos, err := s.Auth.ListInstallableRepos(context.Background(), tok, 0)
	if err != nil {
		return nil, err
	}
	out := make([]githubdgrpc.Repo, 0, len(repos))
	for _, r := range repos {
		out = append(out, githubdgrpc.Repo{
			FullName:      r.FullName,
			DefaultBranch: r.DefaultBranch,
			Private:       r.Private,
		})
	}
	return out, nil
}

// BindAppRepo associates an app with (repo, branch) for the given
// account. PR-B: writes the bind to the durable Store FIRST; on
// success populates the in-memory cache. On a Store failure the
// cache is untouched so the next read pulls fresh state.
//
// bindingID is the deterministic "bind-<appID>-<repo>" form the
// pre-PR-B in-memory map emitted; the (account_id, binding_id)
// unique partial index in migration 00047 makes the upsert
// idempotent on retry.
func (s *RealService) BindAppRepo(appID, accountID, repoFullName, productionBranch string) (string, error) {
	if appID == "" || accountID == "" || repoFullName == "" {
		return "", fmt.Errorf("githubd: appID, accountID, repoFullName required")
	}
	if productionBranch == "" {
		productionBranch = defaultProductionBranch
	}
	bindingID := fmt.Sprintf("bind-%s-%s", appID, repoFullName)

	// Look up the install id from the in-memory install state. The
	// pre-PR-B bind path didn't have access to it (the in-memory
	// map was the bind); PR-B reconstructs the install id from the
	// handshake so the durable row carries the same fields the
	// 00007 migration expects.
	var installID int64
	s.installsMu.RLock()
	if st, ok := s.installs[accountID]; ok {
		if _, err := fmt.Sscanf(st.InstID, "%d", &installID); err != nil {
			s.installsMu.RUnlock()
			return "", fmt.Errorf("githubd: invalid installation id %q", st.InstID)
		}
	}
	s.installsMu.RUnlock()
	if installID == 0 {
		return "", fmt.Errorf("githubd: no installation for account %s", accountID)
	}

	if s.Store == nil {
		return "", fmt.Errorf("githubd: bindings store not configured")
	}

	bid, err := s.Store.Upsert(context.Background(), state.GitHubBinding{
		AppID:            appID,
		AccountID:        accountID,
		InstallID:        installID,
		RepoFullName:     repoFullName,
		ProductionBranch: productionBranch,
		BindingID:        bindingID,
	})
	if err != nil {
		return "", fmt.Errorf("githubd: upsert binding: %w", err)
	}

	// Cache the row we just persisted.
	s.bindingsCacheMu.Lock()
	if _, ok := s.bindingsCache[accountID]; !ok {
		s.bindingsCache[accountID] = map[string]state.GitHubBinding{}
	}
	s.bindingsCache[accountID][appID] = state.GitHubBinding{
		AppID:            appID,
		AccountID:        accountID,
		InstallID:        installID,
		RepoFullName:     repoFullName,
		ProductionBranch: productionBranch,
		BindingID:        bid,
	}
	s.bindingsCacheMu.Unlock()
	return bid, nil
}

// UnbindAppRepo removes the binding for an app. Idempotent: nil
// even if no binding existed. PR-B: deletes the durable row first,
// then clears the cache.
func (s *RealService) UnbindAppRepo(appID, accountID string) error {
	if s.Store == nil {
		return nil // no store → no persistent binding to clear
	}
	if err := s.Store.Delete(context.Background(), appID); err != nil {
		// ErrAppNotFound is fine (idempotent); bubble other errors.
		if err != ErrAppNotFound {
			return fmt.Errorf("githubd: delete binding: %w", err)
		}
	}
	s.bindingsCacheMu.Lock()
	if byApp, ok := s.bindingsCache[accountID]; ok {
		delete(byApp, appID)
	}
	s.bindingsCacheMu.Unlock()
	return nil
}

// GetAppBinding looks up the binding for an app. Cache-first;
// falls back to the durable Store on miss and rebuilds the cache.
// Returns the gRPC-facing AppBinding shape (BindingID empty = no
// binding).
func (s *RealService) GetAppBinding(appID, accountID string) (githubdgrpc.AppBinding, error) {
	s.bindingsCacheMu.RLock()
	if byApp, ok := s.bindingsCache[accountID]; ok {
		if b, ok := byApp[appID]; ok {
			s.bindingsCacheMu.RUnlock()
			return bindingToGRPC(b), nil
		}
	}
	s.bindingsCacheMu.RUnlock()

	// Miss → Store.
	if s.Store == nil {
		return githubdgrpc.AppBinding{}, nil
	}
	b, err := s.Store.GetForApp(context.Background(), appID, accountID)
	if err != nil {
		if err == state.ErrNotFound {
			return githubdgrpc.AppBinding{}, nil
		}
		return githubdgrpc.AppBinding{}, err
	}
	// Rebuild cache.
	s.bindingsCacheMu.Lock()
	if _, ok := s.bindingsCache[accountID]; !ok {
		s.bindingsCache[accountID] = map[string]state.GitHubBinding{}
	}
	s.bindingsCache[accountID][appID] = b
	s.bindingsCacheMu.Unlock()
	return bindingToGRPC(b), nil
}

// CreateDeploymentFromPush is the inbound gRPC entry from apid.
// Today it returns Unimplemented-equivalent errors — the inbound
// webhook path uses HTTP, not gRPC. Kept for the gRPC contract
// round-trip test (slice 7 bufconn_test).
func (s *RealService) CreateDeploymentFromPush(_, _, _, _ string) (string, string, error) {
	return "", "", fmt.Errorf("githubd: CreateDeploymentFromPush is HTTP-driven (slice 7 webhook path)")
}

// WriteCheck pushes a check-run for (repo, sha, phase). Requires
// non-nil Checks.
func (s *RealService) WriteCheck(repoFullName, commitSHA string, phase githubdgrpc.CheckPhase, logsURL, summary string) error {
	if s.Checks == nil {
		return fmt.Errorf("githubd: Checks writer not configured")
	}
	return s.Checks.WriteCheck(context.Background(), repoFullName, commitSHA, phase, logsURL, summary)
}

// VerifyInstallation confirms the installation_id is real for the
// configured GitHub App AND, when expectedLogin is non-empty, that
// the install's account.login matches (PR-B §11 ownership proof).
// A 404 means the callback was forged or stale — verified=false,
// err=nil so the dashboard renders a "connection failed" banner
// rather than a 500. An account.login mismatch returns
// verified=false too (the install is real, just not owned by this
// user); the caller distinguishes by inspecting the AccountLogin
// field on the Installation payload (only the apid-side §11
// path consumes that field).
func (s *RealService) VerifyInstallation(installationID int64, expectedLogin string) (bool, string, string, error) {
	if s.Auth == nil {
		return false, "", "", fmt.Errorf("githubd: OAuth not configured")
	}
	inst, verified, err := s.Auth.VerifyInstallation(context.Background(), installationID, expectedLogin)
	if err != nil {
		return false, "", "", err
	}
	if !verified {
		// Don't surface AccountLogin on a non-verified install —
		// the §11 check is the whole point of the call, and a
		// mismatched login should look identical to a 404 to the
		// forged caller.
		return false, "", "", nil
	}
	return true, inst.AccountLogin, defaultProductionBranch, nil
}

// bindingToGRPC translates the durable state row into the gRPC
// AppBinding shape (which deliberately omits install_id and
// linked_at — those are githubd-internal).
func bindingToGRPC(b state.GitHubBinding) githubdgrpc.AppBinding {
	return githubdgrpc.AppBinding{
		RepoFullName:     b.RepoFullName,
		ProductionBranch: b.ProductionBranch,
		BindingID:        b.BindingID,
	}
}
