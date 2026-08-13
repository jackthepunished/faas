// pgstore_tenant_surface.go — PgStore implementations for the tenant
// surfaces Store interface (ADR-100 / issue #879). The two
// quota-enforcing methods mirror CreateEdgeRuleIfUnderQuota (pgstore.go:5951):
// BeginTx → FOR UPDATE on the parent row → count → insert → commit.
// The remaining methods are plain pool queries using `mapErr` for
// the established ErrNotFound / ErrConflict / ErrInvalidArgument
// shapes. Hand-written SQL matches the pgstore convention
// (ADR-017 / sqlc TODO is unchanged).
package state

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/onebox-faas/faas/pkg/api"
)

// tenantSurfaceCols is the canonical SELECT column list for a
// tenant_surfaces row. Matches the order of scanTenantSurface.
const tenantSurfaceCols = `id, account_id, app_id, name, cert_kind, status,
	cert_state, coalesce(cert_not_after, 'epoch'::timestamptz),
	coalesce(cert_last_error, ''), created_at, updated_at`

// tenantHostnameCols — single column order used everywhere a hostname
// row scans. The zero-value sentinel for verified_at / last_check_at
// is 'epoch' (mirrors custom_domains.verified_at treatment at
// pgstore.go:5236); the Go-side .Verified() / .LastCheckAt.IsZero()
// accessor compensates.
const tenantHostnameCols = `id, surface_id, hostname, challenge_token,
	coalesce(verified_at, 'epoch'::timestamptz),
	coalesce(last_check_at, 'epoch'::timestamptz),
	coalesce(last_error, '')`

// scanTenantSurface is the single row helper for tenant_surfaces.
func scanTenantSurface(row pgx.Row) (TenantSurface, error) {
	var s TenantSurface
	var certKindRaw, statusRaw, certStateRaw string
	if err := row.Scan(
		&s.ID, &s.AccountID, &s.AppID, &s.Name,
		&certKindRaw, &statusRaw, &certStateRaw,
		&s.CertNotAfter, &s.CertLastError,
		&s.CreatedAt, &s.UpdatedAt,
	); err != nil {
		return TenantSurface{}, mapErr(err)
	}
	s.CertKind = CertKind(certKindRaw)
	s.Status = SurfaceStatus(statusRaw)
	s.CertState = CertState(certStateRaw)
	return s, nil
}

// scanTenantSurfaces is the multi-row helper.
func scanTenantSurfaces(rows pgx.Rows) ([]TenantSurface, error) {
	var out []TenantSurface
	for rows.Next() {
		s, err := scanTenantSurface(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// scanTenantHostname — single row.
func scanTenantHostname(row pgx.Row) (TenantHostname, error) {
	var h TenantHostname
	if err := row.Scan(
		&h.ID, &h.SurfaceID, &h.Hostname, &h.ChallengeToken,
		&h.VerifiedAt, &h.LastCheckAt, &h.LastError,
	); err != nil {
		return TenantHostname{}, mapErr(err)
	}
	return h, nil
}

// scanTenantHostnames — multi-row.
func scanTenantHostnames(rows pgx.Rows) ([]TenantHostname, error) {
	var out []TenantHostname
	for rows.Next() {
		h, err := scanTenantHostname(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateTenantSurfaceIfUnderQuota inserts a surface iff the account
// is under limits.TenantSurfacesPerAccount and the plan allows
// surfaces at all (limits.TenantSurfacesAllowed). Returns:
//   - (TenantSurface{}, *TenantSurfaceQuotaError) when limit trips
//   - (TenantSurface{}, ErrTenantSurfacesNotAllowed) when the plan
//     gate is off (Free today)
//   - (TenantSurface{}, ErrNotFound) when the parent account row is
//     missing or soft-deleted
//
// TOCTOU defence: BeginTx + FOR UPDATE on the accounts row
// serialises concurrent inserts before the count.
func (s *PgStore) CreateTenantSurfaceIfUnderQuota(ctx context.Context, in CreateTenantSurfaceParams, limits api.Limits) (TenantSurface, error) {
	if !limits.TenantSurfacesAllowed {
		return TenantSurface{}, ErrTenantSurfacesNotAllowed
	}
	if in.CertKind == "" {
		in.CertKind = CertKindPerHostSAN
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return TenantSurface{}, fmt.Errorf("state: begin create tenant surface tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after Commit

	// Lock the parent account row so concurrent inserts serialise.
	var locked int
	if err := tx.QueryRow(ctx,
		`select 1 from accounts where id = $1 and status <> 'deleted' for update`,
		in.AccountID,
	).Scan(&locked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TenantSurface{}, ErrNotFound
		}
		return TenantSurface{}, fmt.Errorf("state: lock account %s: %w", in.AccountID, err)
	}

	var observed int
	if err := tx.QueryRow(ctx,
		`select count(*) from tenant_surfaces
		 where account_id = $1 and status <> 'deleted'`,
		in.AccountID,
	).Scan(&observed); err != nil {
		return TenantSurface{}, fmt.Errorf("state: count tenant_surfaces for account %s: %w", in.AccountID, err)
	}
	if observed >= limits.TenantSurfacesPerAccount {
		return TenantSurface{}, &TenantSurfaceQuotaError{
			Limit:    limits.TenantSurfacesPerAccount,
			Observed: observed,
		}
	}

	row := tx.QueryRow(ctx,
		`insert into tenant_surfaces (account_id, app_id, name, cert_kind)
		 values ($1, $2, $3, $4)
		 returning `+tenantSurfaceCols,
		in.AccountID, in.AppID, in.Name, string(in.CertKind),
	)
	surf, err := scanTenantSurface(row)
	if err != nil {
		// pgerrcode.UniqueViolation on tenant_surfaces_account_name_uniq
		// surfaces as ErrConflict via mapErr; CHECK violations on
		// tenant_surfaces_app_or_not_chk / cert_kind_chk flow through
		// unchanged (the apid validates the inputs before calling,
		// and tripwire tests want the raw SQLSTATE for visibility).
		return TenantSurface{}, mapErr(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TenantSurface{}, fmt.Errorf("state: commit create tenant surface: %w", err)
	}
	return surf, nil
}

// GetTenantSurfaceByID — primary-key lookup.
func (s *PgStore) GetTenantSurfaceByID(ctx context.Context, id string) (TenantSurface, error) {
	row := s.pool.QueryRow(ctx,
		`select `+tenantSurfaceCols+` from tenant_surfaces where id = $1`, id)
	return scanTenantSurface(row)
}

// GetTenantSurfaceByName — account-scoped name lookup. Includes
// soft-deleted rows so audit / cert_history flows can find them.
func (s *PgStore) GetTenantSurfaceByName(ctx context.Context, accountID, name string) (TenantSurface, error) {
	row := s.pool.QueryRow(ctx,
		`select `+tenantSurfaceCols+` from tenant_surfaces
		 where account_id = $1 and name = $2`,
		accountID, name)
	return scanTenantSurface(row)
}

// ListTenantSurfacesForAccount — operator + apid listings; excludes
// soft-deleted surfaces (they live in audit trails).
func (s *PgStore) ListTenantSurfacesForAccount(ctx context.Context, accountID string) ([]TenantSurface, error) {
	rows, err := s.pool.Query(ctx,
		`select `+tenantSurfaceCols+` from tenant_surfaces
		 where account_id = $1 and status <> 'deleted'
		 order by created_at, id`,
		accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTenantSurfaces(rows)
}

// ListTenantSurfacesForApp — pgRouter.ResolveHost hot path
// reverse-lookup from an app id.
func (s *PgStore) ListTenantSurfacesForApp(ctx context.Context, appID string) ([]TenantSurface, error) {
	rows, err := s.pool.Query(ctx,
		`select `+tenantSurfaceCols+` from tenant_surfaces
		 where app_id = $1 and status <> 'deleted'
		 order by created_at, id`,
		appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTenantSurfaces(rows)
}

// CountTenantSurfacesForAccount — quota + dashboard hot path.
func (s *PgStore) CountTenantSurfacesForAccount(ctx context.Context, accountID string) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx,
		`select count(*) from tenant_surfaces
		 where account_id = $1 and status <> 'deleted'`,
		accountID,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("state: count tenant_surfaces for account %s: %w", accountID, err)
	}
	return n, nil
}

// UpdateTenantSurfaceStatus — apid sets status (pending/active/suspended/deleted).
// Returns ErrNotFound if the row is gone.
func (s *PgStore) UpdateTenantSurfaceStatus(ctx context.Context, id string, status SurfaceStatus) error {
	tag, err := s.pool.Exec(ctx,
		`update tenant_surfaces set status = $1, updated_at = now() where id = $2`,
		string(status), id)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateTenantSurfaceCert — the cert engine's only writer to cert_*
// columns. The trigger on tenant_surfaces emits tenant_surface_changed
// regardless of which column we touch; the cert-remint goroutine
// coalesces.
func (s *PgStore) UpdateTenantSurfaceCert(ctx context.Context, in UpdateSurfaceCertParams) error {
	var notAfter any
	if !in.NotAfter.IsZero() {
		notAfter = in.NotAfter
	}
	tag, err := s.pool.Exec(ctx,
		`update tenant_surfaces
		    set cert_state = $1,
		        cert_not_after = $2,
		        cert_last_error = $3,
		        updated_at = now()
		  where id = $4`,
		string(in.CertState), notAfter, in.LastError, in.SurfaceID)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteTenantSurface — soft delete: flips status to 'deleted' so the
// audit / cert_history paths keep referencing the row.
func (s *PgStore) DeleteTenantSurface(ctx context.Context, id string) error {
	return s.UpdateTenantSurfaceStatus(ctx, id, SurfaceStatusDeleted)
}

// TenantSurfaceByHostname — pgRouter.ResolveHost hot path; joins
// tenant_hostnames to tenant_surfaces via the UQ on hostname alone.
// Returns ErrNotFound when no surface claims the hostname (the
// caller then consults custom_domains via DomainByName).
func (s *PgStore) TenantSurfaceByHostname(ctx context.Context, hostname string) (TenantSurface, error) {
	row := s.pool.QueryRow(ctx,
		`select `+tenantSurfaceCols+` from tenant_surfaces s
		    join tenant_hostnames h on h.surface_id = s.id
		  where h.hostname = $1
		    and s.status <> 'deleted'`,
		hostname)
	return scanTenantSurface(row)
}

// CreateTenantHostnameIfUnderQuota — locks the parent tenant_surfaces
// row before counting hostnames. Distinct from CreateTenantSurfaceIfUnderQuota:
//   - the parent lock is on tenant_surfaces (not accounts)
//   - the per-hostname UQ on tenant_hostnames.hostname surfaces as
//     ErrConflict (CodeTenantHostnameAlreadyClaimed in PR-C)
//
// Returns:
//   - (TenantHostname{}, *TenantHostnameQuotaError) when per-surface cap trips
//   - (TenantHostname{}, ErrNotFound) when the parent surface is missing
//   - (TenantHostname{}, ErrConflict) on UQ violation
func (s *PgStore) CreateTenantHostnameIfUnderQuota(ctx context.Context, in CreateTenantHostnameParams, limits api.Limits) (TenantHostname, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return TenantHostname{}, fmt.Errorf("state: begin create tenant hostname tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after Commit

	var locked int
	if err := tx.QueryRow(ctx,
		`select 1 from tenant_surfaces where id = $1 and status <> 'deleted' for update`,
		in.SurfaceID,
	).Scan(&locked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TenantHostname{}, ErrNotFound
		}
		return TenantHostname{}, fmt.Errorf("state: lock tenant_surface %s: %w", in.SurfaceID, err)
	}

	var observed int
	if err := tx.QueryRow(ctx,
		`select count(*) from tenant_hostnames where surface_id = $1`,
		in.SurfaceID,
	).Scan(&observed); err != nil {
		return TenantHostname{}, fmt.Errorf("state: count tenant_hostnames for surface %s: %w", in.SurfaceID, err)
	}
	if observed >= limits.TenantHostnamesPerSurface {
		return TenantHostname{}, &TenantHostnameQuotaError{
			Limit:     limits.TenantHostnamesPerSurface,
			Observed:  observed,
			SurfaceID: in.SurfaceID,
		}
	}

	row := tx.QueryRow(ctx,
		`insert into tenant_hostnames (surface_id, hostname, challenge_token)
		 values ($1, $2, $3)
		 returning `+tenantHostnameCols,
		in.SurfaceID, in.Hostname, in.ChallengeToken,
	)
	host, err := scanTenantHostname(row)
	if err != nil {
		return TenantHostname{}, mapErr(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TenantHostname{}, fmt.Errorf("state: commit create tenant hostname: %w", err)
	}
	return host, nil
}

// ListTenantHostnamesForSurface — full enumeration (including
// unverified) for the apid response shape.
func (s *PgStore) ListTenantHostnamesForSurface(ctx context.Context, surfaceID string) ([]TenantHostname, error) {
	rows, err := s.pool.Query(ctx,
		`select `+tenantHostnameCols+` from tenant_hostnames
		 where surface_id = $1
		 order by hostname`,
		surfaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTenantHostnames(rows)
}

// ListVerifiedTenantHostnamesForSurface — SAN assembly hot path.
// Sort-by-hostname is deterministic so re-mints produce identical
// (primary, sans) tuples.
func (s *PgStore) ListVerifiedTenantHostnamesForSurface(ctx context.Context, surfaceID string) ([]TenantHostname, error) {
	rows, err := s.pool.Query(ctx,
		`select `+tenantHostnameCols+` from tenant_hostnames
		 where surface_id = $1 and verified_at is not null
		 order by hostname`,
		surfaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTenantHostnames(rows)
}

// CountTenantHostnamesForSurface — quota hot path; cousin of ListVerified*.
func (s *PgStore) CountTenantHostnamesForSurface(ctx context.Context, surfaceID string) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx,
		`select count(*) from tenant_hostnames where surface_id = $1`,
		surfaceID,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("state: count tenant_hostnames for surface %s: %w", surfaceID, err)
	}
	return n, nil
}

// MarkTenantHostnameVerified — sets verified_at = now() and clears
// last_error. Idempotent: a second call with last_error stays
// unchanged.
func (s *PgStore) MarkTenantHostnameVerified(ctx context.Context, hostname string) error {
	tag, err := s.pool.Exec(ctx,
		`update tenant_hostnames
		    set verified_at = now(),
		        last_check_at = now(),
		        last_error = ''
		  where hostname = $1`,
		hostname)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkTenantHostnameCheckFailed — the dns_poller path. Preserves
// verified_at (a previously-verified hostname stays verified across
// a transient DNS resolution failure); only updates last_check_at +
// last_error.
func (s *PgStore) MarkTenantHostnameCheckFailed(ctx context.Context, hostname, reason string) error {
	tag, err := s.pool.Exec(ctx,
		`update tenant_hostnames
		    set last_check_at = now(),
		        last_error = $1
		  where hostname = $2`,
		reason, hostname)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListPendingTenantHostnames — dns_poller queue fetch. Returns the
// `limit` oldest unverified rows whose last_check_at is older than
// `olderThan` (the poller sleeps batch+1 interval seconds between
// passes, so a row re-enters the queue roughly every batch-time).
// Rows with last_check_at IS NULL are always eligible (fresh inserts).
func (s *PgStore) ListPendingTenantHostnames(ctx context.Context, olderThan time.Time, limit int) ([]TenantHostname, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx,
		`select `+tenantHostnameCols+` from tenant_hostnames
		 where verified_at is null
		   and (last_check_at is null or last_check_at < $1)
		 order by coalesce(last_check_at, 'epoch'::timestamptz), created_at
		 limit $2`,
		olderThan, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTenantHostnames(rows)
}

// DeleteTenantHostname — apid path; cascades nothing (hostnames have
// no children).
func (s *PgStore) DeleteTenantHostname(ctx context.Context, hostname string) error {
	tag, err := s.pool.Exec(ctx,
		`delete from tenant_hostnames where hostname = $1`, hostname)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Compile-time guard. Add the tenant_surfaces CHECK constraints to
// mapErr's named-mapping so apid-validate-time errors surface as
// ErrInvalidArgument instead of bubbling the raw SQLSTATE. The
// tripwire tests at pgstore_test.go (snapshot of the original
// named-mapping) stay green because we ADD entries, never remove.
func init() {
	checkViolationMappedToInvalid["tenant_surfaces_app_or_not_chk"] = struct{}{}
	checkViolationMappedToInvalid["tenant_surfaces_cert_kind_chk"] = struct{}{}
	checkViolationMappedToInvalid["tenant_surfaces_status_chk"] = struct{}{}
	checkViolationMappedToInvalid["tenant_surfaces_cert_state_chk"] = struct{}{}
	checkViolationMappedToInvalid["tenant_hostnames_hostname_len_chk"] = struct{}{}
}
