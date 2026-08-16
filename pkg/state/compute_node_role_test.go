// compute_node_role_test.go — coverage for SetComputeNodeRole
// (ADR-112 PR-B). External test package (state_test) so we
// exercise the interface contract from the caller's perspective,
// matching the existing pgstore_*_test.go convention.
//
// The MemStore half runs on every test invocation. The PgStore
// half uses pgtest.Open(t) which skips if DATABASE_URL is unset,
// so unit tests stay green in CI runners without a live cluster.
package state_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/onebox-faas/faas/pkg/db/pgtest"
	"github.com/onebox-faas/faas/pkg/state"
)

func TestSetComputeNodeRole_MemStore_AllowList(t *testing.T) {
	ctx := context.Background()
	m := state.NewMemStore()
	node, err := m.CreateComputeNode(ctx, state.ComputeNode{Name: "role-mem-" + uuid.NewString()[:8], TargetURL: "unix:///run/vmmd.sock", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.DeleteComputeNode(ctx, node.ID) })

	cases := []struct {
		name    string
		role    string
		wantErr bool
	}{
		{"control-plane", "control-plane", false},
		{"compute-only", "compute-only", false},
		{"empty rejected", "", true},
		{"unknown rejected", "control-plan", true},
		{"legacy rejected", "compute-node", true},
		{"single-box rejected", "single-box", true},
		{"sql injection rejected", "control-plane'; drop table", true},
		{"too-long rejected", strings.Repeat("a", 33), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := m.SetComputeNodeRole(ctx, node.ID, tc.role)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("SetComputeNodeRole(%q) = nil err, want error", tc.role)
				}
				return
			}
			if err != nil {
				t.Fatalf("SetComputeNodeRole(%q) = %v, want nil", tc.role, err)
			}
			// Verify the round-trip: ListComputeNodes returns the new role.
			nodes, err := m.ListComputeNodes(ctx, true)
			if err != nil {
				t.Fatal(err)
			}
			var got string
			found := false
			for _, n := range nodes {
				if n.ID == node.ID {
					found = true
					if n.Role == nil {
						t.Fatalf("Role is nil after SetComputeNodeRole(%q)", tc.role)
					}
					got = *n.Role
					break
				}
			}
			if !found {
				t.Fatalf("node %q not in ListComputeNodes", node.ID)
			}
			if got != tc.role {
				t.Fatalf("Role round-trip: got %q, want %q", got, tc.role)
			}
		})
	}
}

func TestSetComputeNodeRole_MemStore_Idempotent(t *testing.T) {
	ctx := context.Background()
	m := state.NewMemStore()
	node, err := m.CreateComputeNode(ctx, state.ComputeNode{Name: "role-mem-idem-" + uuid.NewString()[:8], TargetURL: "unix:///run/vmmd.sock", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.DeleteComputeNode(ctx, node.ID) })

	if err := m.SetComputeNodeRole(ctx, node.ID, "control-plane"); err != nil {
		t.Fatal(err)
	}
	// Second call with the same role must succeed (idempotent).
	if err := m.SetComputeNodeRole(ctx, node.ID, "control-plane"); err != nil {
		t.Fatalf("idempotent re-call: %v", err)
	}
	// Flipping to the other role is also valid.
	if err := m.SetComputeNodeRole(ctx, node.ID, "compute-only"); err != nil {
		t.Fatal(err)
	}
}

func TestSetComputeNodeRole_MemStore_RowMissing(t *testing.T) {
	ctx := context.Background()
	m := state.NewMemStore()
	if err := m.SetComputeNodeRole(ctx, "deadbeef-no-such-row", "control-plane"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("missing row: got %v, want ErrNotFound", err)
	}
}

func TestSetComputeNodeRole_PgStore_AllowList(t *testing.T) {
	pool := pgtest.Open(t)
	s := state.NewPgStore(pool)
	ctx := context.Background()

	node, err := s.CreateComputeNode(ctx, state.ComputeNode{Name: "role-pg-" + uuid.NewString()[:8], TargetURL: "unix:///run/vmmd.sock", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.DeleteComputeNode(ctx, node.ID) })

	cases := []struct {
		name    string
		role    string
		wantErr bool
	}{
		{"control-plane", "control-plane", false},
		{"compute-only", "compute-only", false},
		{"empty rejected", "", true},
		{"unknown rejected", "control-plan", true},
		{"legacy rejected", "compute-node", true},
		{"too-long rejected", strings.Repeat("a", 33), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.SetComputeNodeRole(ctx, node.ID, tc.role)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("SetComputeNodeRole(%q) = nil err, want error", tc.role)
				}
				// Negative case: confirm the row's role did NOT change.
				got, gerr := s.ComputeNodeByID(ctx, node.ID)
				if gerr != nil {
					t.Fatalf("ComputeNodeByID(%s): %v", node.ID, gerr)
				}
				if got.Role != nil && *got.Role == tc.role {
					t.Fatalf("rejected role %q was still written to the row", tc.role)
				}
				return
			}
			if err != nil {
				t.Fatalf("SetComputeNodeRole(%q) = %v, want nil", tc.role, err)
			}
			// Round-trip: read the row back and confirm the role
			// matches what we wrote. Mirrors the MemStore test's
			// post-success assertion; without this check a future
			// SQL refactor that drops the role column from the
			// UPDATE SET list would silently no-op while keeping
			// the test green.
			got, gerr := s.ComputeNodeByID(ctx, node.ID)
			if gerr != nil {
				t.Fatalf("ComputeNodeByID(%s): %v", node.ID, gerr)
			}
			if got.Role == nil {
				t.Fatalf("Role is nil after SetComputeNodeRole(%q)", tc.role)
			}
			if *got.Role != tc.role {
				t.Fatalf("Role round-trip: got %q, want %q", *got.Role, tc.role)
			}
		})
	}
}

func TestSetComputeNodeRole_PgStore_RowMissing(t *testing.T) {
	pool := pgtest.Open(t)
	s := state.NewPgStore(pool)
	ctx := context.Background()
	if err := s.SetComputeNodeRole(ctx, "deadbeef-no-such-row", "control-plane"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("missing row: got %v, want ErrNotFound", err)
	}
}
