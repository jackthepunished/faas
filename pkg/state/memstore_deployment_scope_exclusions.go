// memstore_deployment_scope_exclusions.go — MemStore mirror of
// the PgStore CRUD (pkg/state/pgstore_deployment_scope_exclusions.go).
// Mirrors the memstore_app_webhooks.go file shape: separate file
// to keep memstore.go under its ~9k-line target; the (account,
// project, slug) UNIQUE invariant is enforced at insert time
// because the in-memory store has no SQL index to do it for us.
package state

import (
	"context"
	"sort"
	"time"
)

// CreateDeploymentScopeExclusion inserts a single persisted
// --exclude row. UNIQUE (account_id, project_id, slug) is enforced
// at insert time — same invariant the Postgres unique index holds.
func (m *MemStore) CreateDeploymentScopeExclusion(_ context.Context, in DeploymentScopeExclusion) (DeploymentScopeExclusion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.deploymentScopeExclusions {
		if existing.AccountID == in.AccountID && existing.ProjectID == in.ProjectID && existing.Slug == in.Slug {
			return DeploymentScopeExclusion{}, ErrConflict
		}
	}
	if in.ID == "" {
		in.ID = newID()
	}
	now := time.Now()
	if in.CreatedAt.IsZero() {
		in.CreatedAt = now
	}
	in.UpdatedAt = now
	m.deploymentScopeExclusions[in.ID] = in
	return in, nil
}

// ListDeploymentScopeExclusions returns every exclusion for a
// project sorted by created_at DESC. The 90-day retention window
// from the SQL partial index is mirrored here so unit tests
// don't need a Postgres to assert the same shape.
func (m *MemStore) ListDeploymentScopeExclusions(_ context.Context, projectID string) ([]DeploymentScopeExclusion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := time.Now().Add(-90 * 24 * time.Hour)
	out := make([]DeploymentScopeExclusion, 0)
	for _, e := range m.deploymentScopeExclusions {
		if e.ProjectID != projectID {
			continue
		}
		if e.CreatedAt.Before(cutoff) {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// DeleteDeploymentScopeExclusion is the operator-undo path. The
// (account_id, project_id, slug) composite is the lookup key so
// two operators in different accounts/projects cannot step on
// each other's exclusions. Returns ErrNotFound when no row
// matches.
func (m *MemStore) DeleteDeploymentScopeExclusion(_ context.Context, accountID, projectID, slug string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, e := range m.deploymentScopeExclusions {
		if e.AccountID == accountID && e.ProjectID == projectID && e.Slug == slug {
			delete(m.deploymentScopeExclusions, id)
			return nil
		}
	}
	return ErrNotFound
}

// LookupDeploymentScopeExclusions returns every active exclusion
// for the (account, project) tuple the apply path is processing.
// The handler folds each row's slug into the per-deploy exclude
// list when req.Exclude is empty. Sorted by created_at DESC for
// stable apply ordering.
func (m *MemStore) LookupDeploymentScopeExclusions(_ context.Context, accountID, projectID string) ([]DeploymentScopeExclusion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := time.Now().Add(-90 * 24 * time.Hour)
	out := make([]DeploymentScopeExclusion, 0)
	for _, e := range m.deploymentScopeExclusions {
		if e.AccountID != accountID || e.ProjectID != projectID {
			continue
		}
		if e.CreatedAt.Before(cutoff) {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}
