// Account-wide deployment history must follow the same live-app visibility
// rule as the app list. Soft-deleted apps retain child rows for slug reuse and
// auditability, but those deployments must not leak into customer history.
//
//go:build !no_pg

package state_test

import (
	"testing"
	"time"
)

func TestPg_ListDeploymentsForAccount_ExcludesSoftDeletedApps(t *testing.T) {
	s, ctx := pgStore(t)
	acctID, appID, _ := seedLiveDeploy(t, s, ctx, "soft-delete-deployments-")

	rows, err := s.ListDeploymentsForAccount(ctx, acctID, time.Time{}, 100)
	if err != nil {
		t.Fatalf("ListDeploymentsForAccount before delete: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListDeploymentsForAccount before delete = %d rows, want 1", len(rows))
	}

	if _, err := s.SoftDeleteAppCascade(ctx, appID); err != nil {
		t.Fatalf("SoftDeleteAppCascade: %v", err)
	}

	rows, err = s.ListDeploymentsForAccount(ctx, acctID, time.Time{}, 100)
	if err != nil {
		t.Fatalf("ListDeploymentsForAccount after delete: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("ListDeploymentsForAccount after delete = %d rows, want 0", len(rows))
	}

	// Exercise the cursor branch as well; the status predicate must be applied
	// consistently to both pagination forms.
	rows, err = s.ListDeploymentsForAccount(ctx, acctID, time.Now().UTC().Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("ListDeploymentsForAccount cursor after delete: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("ListDeploymentsForAccount cursor after delete = %d rows, want 0", len(rows))
	}
}
