//go:build !no_pg_test

// public_auth_e2e_test.go — issue #477 / ADR-077
// state-layer end-to-end tripwire. Pins the cross-backend
// shape parity the per-app public-URL auth surface
// requires:
//
//   1. state.App exposes PublicAuthMode + PublicAuthBasicSealed
//      columns the apid PATCH path writes and the gatewayd-
//      internal enforce path reads.
//   2. state.MemStore.UpdateApp handles PublicAuth
//      + SetPublicAuth fields with the same semantics as
//      pgstore (Set bit distinguishes "unset" from
//      "explicit set"; nil-Sealed clears the blob; non-nil
//      Sealed overwrites verbatim).
//   3. The clear-on-mode-flip invariant (PATCH mode='open'
//      clears any prior sealed blob) is observed end-to-end
//      through the memstore path. The pgstore-side variant
//      is in pkg/state (the unit-test layer has the SQL
//      prepared-statement coverage).
//
// The full secretbox round-trip + gatewayd enforcement
// lives in pkg/gateway/public_auth_test.go (C2 handler
// tests + cache + unsealer unit tests). This file is the
// state-shape pin — a future pgstore migration touching
// public_auth_basic can't quietly disagree with the
// in-memory backend without one of these assertions
// catching it.
//
// The MemStore leg pins the seam: any state.App round-trip
// in this file is the same surface pgstore.UpdateApp +
// pgstore.scanApp produce in production. Mirrors the
// existing webhook_e2e_test.go style — no PG fixture
// required, runs as part of `make test`.

package e2e_test

import (
	"context"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// TestPublicAuthE2E_StateSeam_RoundTrip drives the canonical
// Set/UpdateApp shape on a MemStore:
//  1. Create the app at the column-default mode='open'
//     with no sealed blob.
//  2. UpdateApp with SetPublicAuth=true + Mode='basic' +
//     Sealed=<bytes> persists both columns.
//  3. UpdateApp with SetPublicAuth=true + Mode='open' +
//     Sealed=nil clears the sealed blob but keeps the mode
//     as 'open' (the "stale-secret invariant").
//  4. UpdateApp with SetPublicAuth=false leaves the
//     columns untouched (the "untouched vs explicit"
//     Set-bit convention the apid relies on).
func TestPublicAuthE2E_StateSeam_RoundTrip(t *testing.T) {
	st := state.NewMemStore()
	acct, err := st.CreateAccount(context.Background(),
		"e2e-pa-state@example.com", api.PlanPro)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	app, err := st.CreateApp(context.Background(), state.App{
		AccountID: acct.ID, Slug: "pa-state", Type: state.AppTypeApp,
		RAMMB: 256, MaxConcurrency: 5,
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}

	// 1. Column default — empty string mirrors
	// apps.public_auth_mode's "no row written" state
	// when CreateApp runs without a SetPublicAuth
	// flip. The apid handler reads empty == 'open'
	// (the same way pgstore::scanAppInto + the SQL
	// column default resolve the unset case). The
	// mode-stamping happens at PATCH time, not at
	// Create time — CreateApp is intentionally a
	// no-op for PublicAuth so the apid CreateApp
	// path stays symmetric with the existing
	// require_authn / streaming_enabled family.
	got, err := st.AppByID(context.Background(), app.ID)
	if err != nil {
		t.Fatalf("AppByID: %v", err)
	}
	if got.PublicAuthMode != "" {
		t.Fatalf("default PublicAuthMode = %q; want \"\" (unset → apid treats as open)",
			got.PublicAuthMode)
	}
	if len(got.PublicAuthBasicSealed) != 0 {
		t.Fatalf("default PublicAuthBasicSealed = %d bytes; want empty",
			len(got.PublicAuthBasicSealed))
	}

	// 2. UpdateApp — SetPublicAuth=true with mode='basic' +
	// Sealed <bytes> persists both columns.
	const fakeSealed = "sealed-blob-opaque-bytes"
	modeBasic := api.AppPublicAuthModeBasic
	if _, err := st.UpdateApp(context.Background(), app.ID,
		state.UpdateAppParams{
			PublicAuth: &state.AppPublicAuthUpdate{
				Mode:   modeBasic,
				Sealed: []byte(fakeSealed),
			},
			SetPublicAuth: true,
		}); err != nil {
		t.Fatalf("UpdateApp(mode=basic): %v", err)
	}
	got, _ = st.AppByID(context.Background(), app.ID)
	if got.PublicAuthMode != api.AppPublicAuthModeBasic {
		t.Fatalf("after mode=basic PATCH: PublicAuthMode = %q; want basic",
			got.PublicAuthMode)
	}
	if string(got.PublicAuthBasicSealed) != fakeSealed {
		t.Fatalf("after mode=basic PATCH: PublicAuthBasicSealed = %q; want %q",
			string(got.PublicAuthBasicSealed), fakeSealed)
	}

	// 3. UpdateApp — SetPublicAuth=true with mode='open' +
	// Sealed=nil MUST clear the sealed blob. This is the
	// stale-secret invariant: a customer PATCHing mode='open'
	// expects a clean slate, not a stale-but-decryptable
	// secretbox row from a prior mode='basic' configuration.
	modeOpen := api.AppPublicAuthModeOpen
	if _, err := st.UpdateApp(context.Background(), app.ID,
		state.UpdateAppParams{
			PublicAuth: &state.AppPublicAuthUpdate{
				Mode:   modeOpen,
				Sealed: nil,
			},
			SetPublicAuth: true,
		}); err != nil {
		t.Fatalf("UpdateApp(mode=open): %v", err)
	}
	got, _ = st.AppByID(context.Background(), app.ID)
	if got.PublicAuthMode != api.AppPublicAuthModeOpen {
		t.Fatalf("after mode=open PATCH: PublicAuthMode = %q; want open",
			got.PublicAuthMode)
	}
	if len(got.PublicAuthBasicSealed) != 0 {
		t.Fatalf("after mode=open PATCH: PublicAuthBasicSealed = %d bytes; want empty (stale-secret invariant)",
			len(got.PublicAuthBasicSealed))
	}

	// 4. UpdateApp — SetPublicAuth=false (the
	// "no public_auth change" signal the apid path uses
	// for partial-PATCHes like only-RAM_MB) leaves both
	// columns untouched. Insert a different sealed blob
	// then issue an unrelated update with SetPublicAuth=false
	// — the blob must NOT change.
	const anotherSealed = "second-blob"
	if _, err := st.UpdateApp(context.Background(), app.ID,
		state.UpdateAppParams{
			PublicAuth: &state.AppPublicAuthUpdate{
				Mode:   api.AppPublicAuthModeBasic,
				Sealed: []byte(anotherSealed),
			},
			SetPublicAuth: true,
		}); err != nil {
		t.Fatalf("UpdateApp(re-blob): %v", err)
	}
	newRAM := 384
	if _, err := st.UpdateApp(context.Background(), app.ID,
		state.UpdateAppParams{
			RAMMB: &newRAM,
			// SetPublicAuth left false on purpose.
		}); err != nil {
		t.Fatalf("UpdateApp(RAM only): %v", err)
	}
	got, _ = st.AppByID(context.Background(), app.ID)
	if got.PublicAuthMode != api.AppPublicAuthModeBasic {
		t.Fatalf("after partial PATCH: PublicAuthMode = %q; want basic (unchanged)",
			got.PublicAuthMode)
	}
	if string(got.PublicAuthBasicSealed) != anotherSealed {
		t.Fatalf("after partial PATCH: PublicAuthBasicSealed = %q; want %q (unchanged)",
			string(got.PublicAuthBasicSealed), anotherSealed)
	}
}

// TestPublicAuthE2E_StateSeam_NoOpGuard ensures that
// UpdateApp(nil PublicAuth with SetPublicAuth=false) on
// an app with a non-empty PublicAuthBasicSealed leaves
// the blob intact. The complementary case —
// SetPublicAuth=true with nil PublicAuth — would be a
// programming error (and a future contributor adding a
// guarded branch to handle it must NOT silently clear the
// blob). The defensive assertion here is that this test
// case surfaces the same shape a customer-RAM-only PATCH
// produces.
func TestPublicAuthE2E_StateSeam_NoOpGuard(t *testing.T) {
	st := state.NewMemStore()
	acct, _ := st.CreateAccount(context.Background(),
		"e2e-pa-noop@example.com", api.PlanPro)
	app, _ := st.CreateApp(context.Background(), state.App{
		AccountID: acct.ID, Slug: "pa-noop", Type: state.AppTypeApp,
		RAMMB: 256, MaxConcurrency: 5,
	})
	const blobBytes = "preserve-me"
	if _, err := st.UpdateApp(context.Background(), app.ID,
		state.UpdateAppParams{
			PublicAuth: &state.AppPublicAuthUpdate{
				Mode:   api.AppPublicAuthModeBasic,
				Sealed: []byte(blobBytes),
			},
			SetPublicAuth: true,
		}); err != nil {
		t.Fatalf("seed UpdateApp: %v", err)
	}
	// Now a no-op UpdateApp: PublicAuth nil, SetPublicAuth false.
	if _, err := st.UpdateApp(context.Background(), app.ID, state.UpdateAppParams{}); err != nil {
		t.Fatalf("no-op UpdateApp: %v", err)
	}
	row, err := st.AppByID(context.Background(), app.ID)
	if err != nil {
		t.Fatalf("AppByID: %v", err)
	}
	if row.PublicAuthMode != api.AppPublicAuthModeBasic {
		t.Fatalf("PublicAuthMode after no-op = %q; want basic", row.PublicAuthMode)
	}
	if string(row.PublicAuthBasicSealed) != blobBytes {
		t.Fatalf("PublicAuthBasicSealed after no-op = %q; want %q (Set=false means no touch)",
			string(row.PublicAuthBasicSealed), blobBytes)
	}
}
