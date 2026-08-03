package imaged

// TestMarkFCSnapshotsStale_EmitsAudit (issue #470 / PR C /
// ADR-072) pins the imaged-side audit emit from
// MarkFCSnapshotsStale. When the sweep marks N rows stale and
// the Handler has an audit, the function emits one
// app.warm_snapshot_stale row per app that surfaced in the
// post-mark GC projection — i.e. apps with at least one
// SURVIVING non-stale row, since ListSnapshotsForGC filters
// stale=false (the ADR-072 §3.2 caveat: an app whose entire
// fleet goes stale in one sweep receives no audit row, only
// the fleet-level counter).
//
// Test shape: 3 apps on the same account. App A has 2
// snapshots at the old FC version (both go stale) — emits 0
// rows (no survivors). Apps B and C each have 2 snapshots:
// one old (goes stale) + one current (survives) — each emits
// exactly 1 row. Total audit rows = 2.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/onebox-faas/faas/pkg/audit"
	"github.com/onebox-faas/faas/pkg/state"
)

func TestMarkFCSnapshotsStale_EmitsAudit(t *testing.T) {
	store := state.NewMemStore()
	acct, _ := store.CreateAccount(context.Background(), "u@example.com", "pro")

	// App A: 2 snapshots, both at the old FC version. Both go
	// stale in the sweep → no survivor → no audit row.
	appA, _ := store.CreateApp(context.Background(), state.App{
		AccountID: acct.ID, Slug: "audit-stale-a", RAMMB: 256,
		IdleTimeoutS: 30, MaxConcurrency: 2,
	})
	for i := 0; i < 2; i++ {
		depA, _ := store.CreateDeployment(context.Background(), state.Deployment{
			AppID: appA.ID, Kind: state.DeploymentKindImage,
			ImageDigest: "sha256:old" + string(rune('a'+i)),
		})
		if _, err := store.CreateSnapshot(context.Background(), state.Snapshot{
			DeploymentID: depA.ID, MemBytes: 100, DiskBytes: 100,
			FCVersion:  "1.0.0", // pre-#470 baseline — sweep will mark stale
			StorageKey: state.SnapMemKey(depA.ID),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Apps B and C: each has 1 old snapshot (stale post-sweep) + 1
	// current snapshot (survives). Each emits 1 audit row.
	appB, _ := store.CreateApp(context.Background(), state.App{
		AccountID: acct.ID, Slug: "audit-stale-b", RAMMB: 256,
		IdleTimeoutS: 30, MaxConcurrency: 2,
	})
	appC, _ := store.CreateApp(context.Background(), state.App{
		AccountID: acct.ID, Slug: "audit-stale-c", RAMMB: 256,
		IdleTimeoutS: 30, MaxConcurrency: 2,
	})
	for _, app := range []state.App{appB, appC} {
		depOld, _ := store.CreateDeployment(context.Background(), state.Deployment{
			AppID: app.ID, Kind: state.DeploymentKindImage,
			ImageDigest: "sha256:old-" + app.Slug,
		})
		if _, err := store.CreateSnapshot(context.Background(), state.Snapshot{
			DeploymentID: depOld.ID, MemBytes: 100, DiskBytes: 100,
			FCVersion:  "1.0.0", // sweep will mark stale
			StorageKey: state.SnapMemKey(depOld.ID),
		}); err != nil {
			t.Fatal(err)
		}
		depCur, _ := store.CreateDeployment(context.Background(), state.Deployment{
			AppID: app.ID, Kind: state.DeploymentKindImage,
			ImageDigest: "sha256:cur-" + app.Slug,
		})
		if _, err := store.CreateSnapshot(context.Background(), state.Snapshot{
			DeploymentID: depCur.ID, MemBytes: 100, DiskBytes: 100,
			FCVersion:  "1.10.0", // already matches — survives the sweep
			StorageKey: state.SnapMemKey(depCur.ID),
		}); err != nil {
			t.Fatal(err)
		}
	}

	h := newHandler(store)
	h.WithAudit(audit.New(store, h.log, nil, "imaged"))

	// MarkFCSnapshotsStale("1.10.0") marks every FC<1.10.0 row
	// stale. The 4 "1.0.0" rows above transition; the 2 "1.10.0"
	// rows survive. Total n returned = 4.
	n, err := h.MarkFCSnapshotsStale(context.Background(), "1.10.0")
	if err != nil {
		t.Fatalf("MarkFCSnapshotsStale: %v", err)
	}
	if n != 4 {
		t.Errorf("marked stale = %d, want 4", n)
	}

	events, err := store.ListEvents(context.Background(), acct.ID, 100)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var staleByApp = make(map[string]int)
	for _, e := range events {
		if e.Kind != "app.warm_snapshot_stale" {
			continue
		}
		// Parse the payload to count per-app rows. We unmarshal
		// into a generic shape because the audit payload uses
		// json.RawMessage in storage.
		var payload struct {
			AppID string `json:"app_id"`
		}
		// Data is []byte on Event (per memstore's AppendEvent
		// signature); unmarshal via stdlib json. We assume the
		// audit package stored the data verbatim.
		if len(e.Data) > 0 {
			if err := json.Unmarshal(e.Data, &payload); err == nil && payload.AppID != "" {
				staleByApp[payload.AppID]++
			}
		}
	}
	// App A: no surviving non-stale row → 0 audit rows.
	// Apps B, C: each has a survivor → 1 audit row each.
	if got := staleByApp[appA.ID]; got != 0 {
		t.Errorf("app A audit rows = %d, want 0 (entire fleet went stale)", got)
	}
	if got := staleByApp[appB.ID]; got != 1 {
		t.Errorf("app B audit rows = %d, want 1", got)
	}
	if got := staleByApp[appC.ID]; got != 1 {
		t.Errorf("app C audit rows = %d, want 1", got)
	}
}