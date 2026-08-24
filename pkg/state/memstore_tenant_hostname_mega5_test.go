// memstore_tenant_hostname_mega5_test.go — Coverage Mega-PR #5
// cluster 6: pin GetTenantHostnameByName
// (pkg/state/memstore_tenant_surface.go:511) at 100%. The method
// does a 2-pass lookup (exact key → lowercase key) and a
// soft-deleted-parent filter.
//
// Whitebox `package state`. No Postgres dependency.

package state

import (
	"testing"
	"time"
)

func seedTenantHostname_Mega5(m *MemStore, hostname, surfaceID string) TenantHostname {
	h := TenantHostname{
		ID:        "th-" + hostname,
		SurfaceID: surfaceID,
		Hostname:  hostname,
		CreatedAt: time.Now(),
	}
	m.tenantHostnames[hostname] = h
	return h
}

func seedTenantSurface_Mega5(m *MemStore, id string, status SurfaceStatus) TenantSurface {
	s := TenantSurface{
		ID:     id,
		Name:   "s-" + id,
		Status: status,
	}
	m.tenantSurfaces[id] = s
	return s
}

func TestGetTenantHostnameByName_ExactMatch_Mega5(t *testing.T) {
	t.Parallel()
	m := NewMemStore()
	seedTenantSurface_Mega5(m, "sf-1", SurfaceStatusActive)
	seedTenantHostname_Mega5(m, "example.com", "sf-1")

	got, err := m.GetTenantHostnameByName(t.Context(), "example.com")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got.Hostname != "example.com" {
		t.Errorf("Hostname = %q, want example.com", got.Hostname)
	}
	if got.SurfaceID != "sf-1" {
		t.Errorf("SurfaceID = %q, want sf-1", got.SurfaceID)
	}
}

func TestGetTenantHostnameByName_LowercaseFallback_Mega5(t *testing.T) {
	t.Parallel()
	m := NewMemStore()
	seedTenantSurface_Mega5(m, "sf-1", SurfaceStatusActive)
	// Seed canonical lowercase; the method's 2nd-pass lookup must
	// normalize the query and find it.
	seedTenantHostname_Mega5(m, "example.com", "sf-1")

	got, err := m.GetTenantHostnameByName(t.Context(), "EXAMPLE.COM")
	if err != nil {
		t.Fatalf("err = %v, want nil (lowercase fallback)", err)
	}
	if got.Hostname != "example.com" {
		t.Errorf("Hostname = %q, want example.com", got.Hostname)
	}
}

func TestGetTenantHostnameByName_NotFound_Mega5(t *testing.T) {
	t.Parallel()
	m := NewMemStore()
	if _, err := m.GetTenantHostnameByName(t.Context(), "missing.example"); err != ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestGetTenantHostnameByName_SurfaceDeleted_Mega5(t *testing.T) {
	t.Parallel()
	m := NewMemStore()
	seedTenantSurface_Mega5(m, "sf-gone", SurfaceStatusDeleted)
	seedTenantHostname_Mega5(m, "gone.example", "sf-gone")

	if _, err := m.GetTenantHostnameByName(t.Context(), "gone.example"); err != ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound (parent surface soft-deleted)", err)
	}
}

func TestGetTenantHostnameByName_SurfaceMissing_Mega5(t *testing.T) {
	t.Parallel()
	m := NewMemStore()
	// Hostname points at a surface ID that doesn't exist in
	// m.tenantSurfaces. Treat as not-found (matches the SQL JOIN's
	// behavior when the parent row is gone).
	seedTenantHostname_Mega5(m, "orphan.example", "sf-ghost")

	if _, err := m.GetTenantHostnameByName(t.Context(), "orphan.example"); err != ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound (parent surface missing)", err)
	}
}

func TestGetTenantHostnameByName_AllNonDeletedStatuses_Mega5(t *testing.T) {
	t.Parallel()
	// Pin the contract: any non-Deleted status (pending/active/
	// suspended) returns the hostname.
	for _, status := range []SurfaceStatus{
		SurfaceStatusPending,
		SurfaceStatusActive,
		SurfaceStatusSuspended,
	} {
		m := NewMemStore()
		seedTenantSurface_Mega5(m, "sf-1", status)
		seedTenantHostname_Mega5(m, "example.com", "sf-1")

		got, err := m.GetTenantHostnameByName(t.Context(), "example.com")
		if err != nil {
			t.Errorf("status %q: err = %v, want nil", status, err)
			continue
		}
		if got.Hostname != "example.com" {
			t.Errorf("status %q: Hostname = %q", status, got.Hostname)
		}
	}
}
