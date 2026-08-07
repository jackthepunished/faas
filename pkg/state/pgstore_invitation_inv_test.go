// PR-9 §2 adversarial cursor test: seed four rows whose UUIDs
// sort lexically in the OPPOSITE direction of the created_at
// ordering. The v1 cursor (bare id, `id::text < $cursor` against
// `ORDER BY created_at DESC, id DESC`) would skip the
// highest-created_at row (the one whose id is lexically larger
// than the cursor's predecessor). The PR-9 compound cursor
// (created_at, id) walks all four rows correctly.
package state_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/cursor"
	"github.com/onebox-faas/faas/pkg/state"
)

func TestPg_ListOrgInvitationsForOrgPage_CompoundCursor_Adversarial(t *testing.T) {
	s, ctx := pgStore(t)

	// Create an account + shared org to host the invitations.
	acct, err := s.CreateAccount(ctx, "pr9-adversarial-1@example.com", api.PlanPro)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	org, err := s.CreateOrg(ctx, state.Org{Slug: "pr9-adv", Name: "PR9 Adv", Plan: api.PlanPro})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	ib := acct.ID
	// Seed four rows with strictly-ordered created_at and
	// lexically-INVERSE UUID ids. The expected cursor walk
	// must visit all four in (created_at DESC) order.
	base := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	seeds := []struct {
		email string
		ts    time.Time
		id    string
	}{
		{"a@example.com", base.Add(0 * time.Second), "ffffffff-ffff-ffff-ffff-ffffffffffff"},
		{"b@example.com", base.Add(1 * time.Second), "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"},
		{"c@example.com", base.Add(2 * time.Second), "dddddddd-dddd-dddd-dddd-dddddddddddd"},
		{"d@example.com", base.Add(3 * time.Second), "cccccccc-cccc-cccc-cccc-cccccccccccc"},
	}
	for _, seed := range seeds {
		// Force the seeded id; CreateOrgInvitation accepts a
		// pre-set ID and CreatedAt.
		_, err := s.CreateOrgInvitation(ctx, state.OrgInvitation{
			ID:                 seed.id,
			OrgID:              org.ID,
			Email:              seed.email,
			Role:               state.OrgRoleDeveloper,
			TokenHash:          []byte(seed.id),
			InvitedByAccountID: &ib,
			CreatedAt:          seed.ts,
			ExpiresAt:          base.Add(24 * time.Hour),
		})
		if err != nil {
			t.Fatalf("CreateOrgInvitation %s: %v", seed.email, err)
		}
	}

	// Helper: walk all pages with limit=1 and collect emails.
	emails := []string{}
	before := ""
	for i := 0; i < 10; i++ { // safety cap
		page, err := s.ListOrgInvitationsForOrgPage(ctx, org.ID, 1, before)
		if err != nil {
			t.Fatalf("page %d: %v", i, err)
		}
		if len(page) == 0 {
			break
		}
		emails = append(emails, page[0].Email)
		k := cursor.Key{CreatedAt: page[0].CreatedAt, ID: page[0].ID}
		before = cursor.Encode(k)
	}
	// Strictly descending created_at → d, c, b, a.
	want := []string{"d@example.com", "c@example.com", "b@example.com", "a@example.com"}
	if len(emails) != len(want) {
		t.Fatalf("walked %d rows, want %d (emails=%v)", len(emails), len(want), emails)
	}
	for i, w := range want {
		if emails[i] != w {
			t.Errorf("walk[%d] = %q, want %q (full=%v)", i, emails[i], w, emails)
		}
	}

	// PR-9: a malformed cursor must surface as ErrInvalidCursor
	// (the v1 cursor silently returned 0 rows on malformed input
	// — a regression the test below catches).
	_, err = s.ListOrgInvitationsForOrgPage(ctx, org.ID, 5, "not!base64")
	if err == nil {
		t.Fatalf("malformed cursor: want error, got nil")
	}
	if !errors.Is(err, state.ErrInvalidCursor) {
		t.Errorf("malformed cursor: err = %v, want errors.Is(err, state.ErrInvalidCursor)", err)
	}

	// Sanity: the test scaffolding is reachable (uuid import
	// used to be a no-op; the cursor test above uses it).
	_ = uuid.NewString
	_ = context.Background
}
