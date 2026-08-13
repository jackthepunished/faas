// memstore_tenant_surface.go — MemStore implementations for the
// tenant surfaces Store interface (ADR-100 / issue #879). MemStore is
// the test-only in-process mirror of PgStore; every method synchronises
// against m.mu (MemStore is single-process; per-row FOR UPDATE is
// unnecessary because the lock is implicit). The two quota-check
// methods mirror CreateEdgeRuleIfUnderQuota (memstore.go:9580).
package state

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/onebox-faas/faas/pkg/api"
)

// CreateTenantSurfaceIfUnderQuota — same TOCTOU-defence shape as
// memstore.go:9580 CreateEdgeRuleIfUnderQuota: single m.mu.Lock()
// wraps the parent lookup + count + insert. There is no SoftDelete
// tombstoning; the in-memory map lacks the partial-unique predicate
// the SQL schema has, so duplicate (account_id, name) pairs are
// rejected with the same ErrConflict the pgstore returns (state-level
// invariant: a soft-deleted surface frees the name, so a re-create
// post-delete is allowed).
func (m *MemStore) CreateTenantSurfaceIfUnderQuota(_ context.Context, in CreateTenantSurfaceParams, limits api.Limits) (TenantSurface, error) {
	if !limits.TenantSurfacesAllowed {
		return TenantSurface{}, ErrTenantSurfacesNotAllowed
	}
	if in.CertKind == "" {
		in.CertKind = CertKindPerHostSAN
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	acct, ok := m.accounts[in.AccountID]
	if !ok || acct.Status == AccountDeletedPending {
		return TenantSurface{}, ErrNotFound
	}
	observed := 0
	for _, s := range m.tenantSurfaces {
		if s.AccountID == in.AccountID && s.Status != SurfaceStatusDeleted {
			observed++
		}
	}
	if observed >= limits.TenantSurfacesPerAccount {
		return TenantSurface{}, &TenantSurfaceQuotaError{
			Limit:    limits.TenantSurfacesPerAccount,
			Observed: observed,
		}
	}
	for _, s := range m.tenantSurfaces {
		if s.AccountID == in.AccountID && s.Name == in.Name && s.Status != SurfaceStatusDeleted {
			return TenantSurface{}, ErrConflict
		}
	}
	now := time.Now().UTC()
	surf := TenantSurface{
		ID:        uuid.NewString(),
		AccountID: in.AccountID,
		AppID:     in.AppID,
		Name:      in.Name,
		CertKind:  in.CertKind,
		Status:    SurfaceStatusPending,
		CertState: CertStateNone,
		CreatedAt: now,
		UpdatedAt: now,
	}
	m.tenantSurfaces[surf.ID] = surf
	return surf, nil
}

// GetTenantSurfaceByID — direct map lookup.
func (m *MemStore) GetTenantSurfaceByID(_ context.Context, id string) (TenantSurface, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.tenantSurfaces[id]
	if !ok {
		return TenantSurface{}, ErrNotFound
	}
	return s, nil
}

// GetTenantSurfaceByName — linear scan; dataset is bounded by the
// per-account quota (max 25 today) so the cost is trivial.
func (m *MemStore) GetTenantSurfaceByName(_ context.Context, accountID, name string) (TenantSurface, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.tenantSurfaces {
		if s.AccountID == accountID && s.Name == name {
			return s, nil
		}
	}
	return TenantSurface{}, ErrNotFound
}

// ListTenantSurfacesForAccount — sorted by CreatedAt then ID; soft
// deletes are filtered out so the consumer sees the same shape
// pgstore returns.
func (m *MemStore) ListTenantSurfacesForAccount(_ context.Context, accountID string) ([]TenantSurface, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]TenantSurface, 0)
	for _, s := range m.tenantSurfaces {
		if s.AccountID == accountID && s.Status != SurfaceStatusDeleted {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// ListTenantSurfacesForApp — required by the pgRouter.ResolveHost
// inverse path.
func (m *MemStore) ListTenantSurfacesForApp(_ context.Context, appID string) ([]TenantSurface, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]TenantSurface, 0)
	for _, s := range m.tenantSurfaces {
		if s.AppID == appID && s.Status != SurfaceStatusDeleted {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// CountTenantSurfacesForAccount — quota + dashboard hot path.
func (m *MemStore) CountTenantSurfacesForAccount(_ context.Context, accountID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, s := range m.tenantSurfaces {
		if s.AccountID == accountID && s.Status != SurfaceStatusDeleted {
			n++
		}
	}
	return n, nil
}

// UpdateTenantSurfaceStatus — mirrors the status flip but doesn't
// touch updated_at at the time.Now() level; we update it so the
// (apiserver) audit + dashboard see the change.
func (m *MemStore) UpdateTenantSurfaceStatus(_ context.Context, id string, status SurfaceStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.tenantSurfaces[id]
	if !ok {
		return ErrNotFound
	}
	s.Status = status
	s.UpdatedAt = time.Now().UTC()
	m.tenantSurfaces[id] = s
	return nil
}

// UpdateTenantSurfaceCert — the in-memory cert state transition.
func (m *MemStore) UpdateTenantSurfaceCert(_ context.Context, in UpdateSurfaceCertParams) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.tenantSurfaces[in.SurfaceID]
	if !ok {
		return ErrNotFound
	}
	s.CertState = in.CertState
	s.CertNotAfter = in.NotAfter
	s.CertLastError = in.LastError
	s.UpdatedAt = time.Now().UTC()
	m.tenantSurfaces[in.SurfaceID] = s
	return nil
}

// DeleteTenantSurface — soft delete; the row stays for audit /
// cert_history paths.
func (m *MemStore) DeleteTenantSurface(_ context.Context, id string) error {
	return m.UpdateTenantSurfaceStatus(context.Background(), id, SurfaceStatusDeleted)
}

// TenantSurfaceByHostname — pgRouter.ResolveHost hot path; linear
// scan over hostnames. Dataset is bounded by (surfaces per acct) ×
// (hostnames per surface) = 25*250 = 6250 worst case, well under the
// ms-bound latency budget. Lookup returns the parent surface, same
// shape as DomainByName / custom_domains.
func (m *MemStore) TenantSurfaceByHostname(_ context.Context, hostname string) (TenantSurface, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, h := range m.tenantHostnames {
		if h.Hostname == hostname {
			if s, ok := m.tenantSurfaces[h.SurfaceID]; ok && s.Status != SurfaceStatusDeleted {
				return s, nil
			}
			return TenantSurface{}, ErrNotFound
		}
	}
	return TenantSurface{}, ErrNotFound
}

// CreateTenantHostnameIfUnderQuota — locks on the parent surface (m.mu
// here is process-wide), counts, enforces the UQ on hostname, inserts.
func (m *MemStore) CreateTenantHostnameIfUnderQuota(_ context.Context, in CreateTenantHostnameParams, limits api.Limits) (TenantHostname, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.tenantSurfaces[in.SurfaceID]
	if !ok || s.Status == SurfaceStatusDeleted {
		return TenantHostname{}, ErrNotFound
	}
	observed := 0
	for _, h := range m.tenantHostnames {
		if h.SurfaceID == in.SurfaceID {
			observed++
		}
	}
	if observed >= limits.TenantHostnamesPerSurface {
		return TenantHostname{}, &TenantHostnameQuotaError{
			Limit:     limits.TenantHostnamesPerSurface,
			Observed:  observed,
			SurfaceID: in.SurfaceID,
		}
	}
	if _, exists := m.tenantHostnames[in.Hostname]; exists {
		return TenantHostname{}, ErrConflict
	}
	now := time.Now().UTC()
	h := TenantHostname{
		ID:             uuid.NewString(),
		SurfaceID:      in.SurfaceID,
		Hostname:       in.Hostname,
		ChallengeToken: in.ChallengeToken,
		CreatedAt:      now,
	}
	m.tenantHostnames[h.Hostname] = h
	return h, nil
}

// ListTenantHostnamesForSurface — sorted by hostname for parity with
// the pgstore ORDER BY.
func (m *MemStore) ListTenantHostnamesForSurface(_ context.Context, surfaceID string) ([]TenantHostname, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]TenantHostname, 0)
	for _, h := range m.tenantHostnames {
		if h.SurfaceID == surfaceID {
			out = append(out, h)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hostname < out[j].Hostname })
	return out, nil
}

// ListVerifiedTenantHostnamesForSurface — SAN-assembly hot path.
func (m *MemStore) ListVerifiedTenantHostnamesForSurface(_ context.Context, surfaceID string) ([]TenantHostname, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]TenantHostname, 0)
	for _, h := range m.tenantHostnames {
		if h.SurfaceID == surfaceID && h.Verified() {
			out = append(out, h)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hostname < out[j].Hostname })
	return out, nil
}

// CountTenantHostnamesForSurface.
func (m *MemStore) CountTenantHostnamesForSurface(_ context.Context, surfaceID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, h := range m.tenantHostnames {
		if h.SurfaceID == surfaceID {
			n++
		}
	}
	return n, nil
}

// MarkTenantHostnameVerified — sets VerifiedAt + LastCheckAt, clears
// LastError.
func (m *MemStore) MarkTenantHostnameVerified(_ context.Context, hostname string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.tenantHostnames[hostname]
	if !ok {
		return ErrNotFound
	}
	now := time.Now().UTC()
	h.VerifiedAt = now
	h.LastCheckAt = now
	h.LastError = ""
	m.tenantHostnames[hostname] = h
	return nil
}

// MarkTenantHostnameCheckFailed — dns_poller path; preserves
// VerifiedAt across a transient DNS failure.
func (m *MemStore) MarkTenantHostnameCheckFailed(_ context.Context, hostname, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.tenantHostnames[hostname]
	if !ok {
		return ErrNotFound
	}
	h.LastCheckAt = time.Now().UTC()
	h.LastError = reason
	m.tenantHostnames[hostname] = h
	return nil
}

// ListPendingTenantHostnames — dns_poller queue. Bounded by limit
// (default 50). Rows with LastCheckAt.IsZero() are always eligible;
// older rows come first.
func (m *MemStore) ListPendingTenantHostnames(_ context.Context, olderThan time.Time, limit int) ([]TenantHostname, error) {
	if limit <= 0 {
		limit = 50
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]TenantHostname, 0)
	for _, h := range m.tenantHostnames {
		if h.Verified() {
			continue
		}
		if !h.LastCheckAt.IsZero() && !h.LastCheckAt.Before(olderThan) {
			continue
		}
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool {
		ti := out[i].LastCheckAt
		tj := out[j].LastCheckAt
		if ti.IsZero() && !tj.IsZero() {
			return true
		}
		if !ti.IsZero() && tj.IsZero() {
			return false
		}
		if ti.Equal(tj) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return ti.Before(tj)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// DeleteTenantHostname — for tests + the apid path.
func (m *MemStore) DeleteTenantHostname(_ context.Context, hostname string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tenantHostnames[hostname]; !ok {
		return ErrNotFound
	}
	delete(m.tenantHostnames, hostname)
	return nil
}

// errors.Is is used by tests for *TenantSurfaceQuotaError +
// *TenantHostnameQuotaError assertions; keep the symbol live so the
// blank import doesn't get stripped from the editor view.
var _ = errors.Is
