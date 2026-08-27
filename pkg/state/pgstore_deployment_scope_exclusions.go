// pgstore_deployment_scope_exclusions.go — PgStore CRUD for the
// deployment_scope_exclusions table (migration 00418, ADR-124
// follow-up #3). Raw-SQL implementations (not sqlc) so this PR
// ships without a sqlc regen; the next sqlc regen picks these up
// automatically. Mirrors the pgstore_app_webhooks.go file shape:
// small CRUD per file, isUniqueViolation funnel for SQLSTATE 23505,
// ErrNotFound for zero-row tag.RowsAffected.
//
// CRITICAL pitfall (from 00488_deployment_scope_exclusions.sql
// header): the table has NO FK to apps(id) by design — soft-deleted
// apps do NOT cascade to exclusions. The CRUD below never queries
// apps; if a future caller joins on app_id, treat the result as a
// snapshot reference (it may be soft-deleted).
package state

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// CreateDeploymentScopeExclusion inserts a single persisted
// --exclude row. UNIQUE (account_id, project_id, slug) trips
// SQLSTATE 23505 on duplicate and the funnel maps it to
// ErrConflict (mirrors CreateAppWebhook's shape at
// pgstore_app_webhooks.go:33).
func (s *PgStore) CreateDeploymentScopeExclusion(ctx context.Context, in DeploymentScopeExclusion) (DeploymentScopeExclusion, error) {
	row := s.pool.QueryRow(ctx, `
		insert into deployment_scope_exclusions
			(account_id, project_id, app_id, slug, reason, created_by)
		values ($1, $2, $3, $4, $5, $6)
		returning id, account_id, project_id, app_id, slug, reason,
		          created_by, created_at, updated_at
	`, in.AccountID, in.ProjectID, in.AppID, in.Slug, in.Reason, in.CreatedBy)
	out, err := scanDeploymentScopeExclusion(row)
	if err != nil {
		if isUniqueViolation(err) {
			return DeploymentScopeExclusion{}, ErrConflict
		}
		return DeploymentScopeExclusion{}, fmt.Errorf("state: insert deployment_scope_exclusion: %w", err)
	}
	return out, nil
}

// ListDeploymentScopeExclusions returns every active exclusion for
// a project, sorted by created_at DESC. Backs the admin tooling's
// "what's persisted for this project?" view. The 90-day partial
// index keeps the lookup O(log n_active) even when the table
// accumulates rows past the retention boundary (janitor reaps
// stale rows; see PurgeOrphanedScopeExclusions).
func (s *PgStore) ListDeploymentScopeExclusions(ctx context.Context, projectID string) ([]DeploymentScopeExclusion, error) {
	rows, err := s.pool.Query(ctx, `
		select id, account_id, project_id, app_id, slug, reason,
		       created_by, created_at, updated_at
		  from deployment_scope_exclusions
		 where project_id = $1
		   and created_at > now() - interval '90 days'
		 order by created_at desc
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("state: list deployment_scope_exclusions: %w", err)
	}
	defer rows.Close()
	return scanDeploymentScopeExclusions(rows)
}

// DeleteDeploymentScopeExclusion is the operator-undo path. The
// (account_id, project_id, slug) composite is the lookup key so
// two operators in different accounts/projects cannot step on
// each other's exclusions. Returns ErrNotFound when no row
// matches.
func (s *PgStore) DeleteDeploymentScopeExclusion(ctx context.Context, accountID, projectID, slug string) error {
	tag, err := s.pool.Exec(ctx, `
		delete from deployment_scope_exclusions
		 where account_id = $1
		   and project_id = $2
		   and slug = $3
	`, accountID, projectID, slug)
	if err != nil {
		return fmt.Errorf("state: delete deployment_scope_exclusion: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// LookupDeploymentScopeExclusions returns every active exclusion
// for the (account, project) tuple the apply path is processing.
// The handler folds each row's slug into the per-deploy exclude
// list when req.Exclude is empty, so a subsequent
// `gregale deploy --tarball=...` without --exclude still honors
// the persisted intent. Sorted by created_at DESC for stable apply
// ordering; the per-deploy exclude list inherits the order.
func (s *PgStore) LookupDeploymentScopeExclusions(ctx context.Context, accountID, projectID string) ([]DeploymentScopeExclusion, error) {
	rows, err := s.pool.Query(ctx, `
		select id, account_id, project_id, app_id, slug, reason,
		       created_by, created_at, updated_at
		  from deployment_scope_exclusions
		 where account_id = $1
		   and project_id = $2
		   and created_at > now() - interval '90 days'
		 order by created_at desc
	`, accountID, projectID)
	if err != nil {
		return nil, fmt.Errorf("state: lookup deployment_scope_exclusions: %w", err)
	}
	defer rows.Close()
	return scanDeploymentScopeExclusions(rows)
}

// scanDeploymentScopeExclusion is the single-row scan helper.
// Mirrors scanAppWebhook's shape at pgstore_app_webhooks.go:118 —
// pgx.Row is the source; pgx.ErrNoRows is the only error case the
// caller must inspect.
func scanDeploymentScopeExclusion(row pgx.Row) (DeploymentScopeExclusion, error) {
	var e DeploymentScopeExclusion
	var createdAt, updatedAt time.Time
	err := row.Scan(&e.ID, &e.AccountID, &e.ProjectID, &e.AppID, &e.Slug,
		&e.Reason, &e.CreatedBy, &createdAt, &updatedAt)
	if err != nil {
		return DeploymentScopeExclusion{}, err
	}
	e.CreatedAt = createdAt
	e.UpdatedAt = updatedAt
	return e, nil
}

// scanDeploymentScopeExclusions is the multi-row scan helper.
// Closes the rows iterator via defer; callers must NOT close it
// themselves. Returns an empty slice (not nil) on zero rows so
// JSON encoders produce `[]` not `null`.
func scanDeploymentScopeExclusions(rows pgx.Rows) ([]DeploymentScopeExclusion, error) {
	out := make([]DeploymentScopeExclusion, 0)
	for rows.Next() {
		e, err := scanDeploymentScopeExclusion(rows)
		if err != nil {
			return nil, fmt.Errorf("state: scan deployment_scope_exclusion: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: iterate deployment_scope_exclusions: %w", err)
	}
	return out, nil
}