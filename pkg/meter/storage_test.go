// Storage rollup tests (ADR-049 §B.3). The FAIL-SOFT contract
// is the load-bearing piece (PR #428 review warning #3):
//
//   1. Happy path — every app's LatestSnapshotBytes + upsert
//      succeeds → return (n, nil).
//   2. One app's LatestSnapshotBytes fails mid-loop → we log +
//      skip it; the remaining apps still get rolled up; return
//      (writtenSoFar, firstErr).
//   3. ListAllApps fails (catastrophic) → return (0, err); the
//      loop aborts because there is nothing to walk.

package meter

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

// fakeStore is the minimal pkg/meter.Store stub the rollup walks.
type fakeStore struct {
	apps             []AppRow
	latestErrByApp   map[string]error
	upsertErrByApp   map[string]error
	upsertCalls      []upsertCall
	upsertAlwaysFail bool
}

type upsertCall struct {
	AccountID string
	AppID     string
	Day       time.Time
	SnapshotB int64
	LayerB    int64
}

func (f *fakeStore) ListAllApps(_ context.Context) ([]AppRow, error) {
	return f.apps, nil
}

func (f *fakeStore) LatestSnapshotBytes(_ context.Context, appID string) (int64, int64, error) {
	if err, ok := f.latestErrByApp[appID]; ok {
		return 0, 0, err
	}
	// Per-app synthetic byte counts: 1 KiB mem + 2 KiB disk.
	return 1024, 2048, nil
}

func (f *fakeStore) AppendSnapshotStorage(_ context.Context, accountID, appID string, day time.Time, snapBytes, layerBytes int64) error {
	if f.upsertAlwaysFail {
		return errors.New("upsert failed")
	}
	if err, ok := f.upsertErrByApp[appID]; ok {
		return err
	}
	f.upsertCalls = append(f.upsertCalls, upsertCall{
		AccountID: accountID,
		AppID:     appID,
		Day:       day,
		SnapshotB: snapBytes,
		LayerB:    layerBytes,
	})
	return nil
}

func TestStorageRollupOnce_HappyPath(t *testing.T) {
	store := &fakeStore{
		apps: []AppRow{
			{AccountID: "acct_a", AppID: "app_1"},
			{AccountID: "acct_a", AppID: "app_2"},
			{AccountID: "acct_b", AppID: "app_3"},
		},
	}
	n, err := StorageRollupOnce(context.Background(), store, time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC), nil, nil)
	if err != nil {
		t.Fatalf("StorageRollupOnce: %v", err)
	}
	if n != 3 {
		t.Errorf("n = %d, want 3", n)
	}
	if got := len(store.upsertCalls); got != 3 {
		t.Errorf("upsertCalls = %d, want 3", got)
	}
}

func TestStorageRollupOnce_FailsSoftPerApp(t *testing.T) {
	// One app's LatestSnapshotBytes fails. The remaining two
	// must still be rolled up; the first error is returned so
	// the loop can log it.
	store := &fakeStore{
		apps: []AppRow{
			{AccountID: "acct_a", AppID: "app_1"},
			{AccountID: "acct_a", AppID: "app_2_BAD"},
			{AccountID: "acct_b", AppID: "app_3"},
		},
		latestErrByApp: map[string]error{
			"app_2_BAD": errors.New("transient lookup"),
		},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	n, err := StorageRollupOnce(context.Background(), store, time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC), nil, log)
	if err == nil {
		t.Fatal("err = nil, want fail-soft non-nil first error")
	}
	if n != 2 {
		t.Errorf("n = %d, want 2 (fail-soft: skip app_2_BAD, write app_1 + app_3)", n)
	}
	if got := len(store.upsertCalls); got != 2 {
		t.Errorf("upsertCalls = %d, want 2", got)
	}
	for _, c := range store.upsertCalls {
		if c.AppID == "app_2_BAD" {
			t.Errorf("app_2_BAD must not have been upserted; upsertCalls=%+v", store.upsertCalls)
		}
	}
}

func TestStorageRollupOnce_ContinuesAfterUpsertError(t *testing.T) {
	// One app's upsert fails. Fail-soft: skip + continue.
	store := &fakeStore{
		apps: []AppRow{
			{AccountID: "acct_a", AppID: "app_1"},
			{AccountID: "acct_a", AppID: "app_2_BAD"},
			{AccountID: "acct_b", AppID: "app_3"},
		},
		upsertErrByApp: map[string]error{
			"app_2_BAD": errors.New("upsert dropped"),
		},
	}
	n, err := StorageRollupOnce(context.Background(), store, time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC), nil, nil)
	if err == nil {
		t.Fatal("err = nil, want fail-soft non-nil first error")
	}
	if n != 2 {
		t.Errorf("n = %d, want 2 (skip the upsert-failed app)", n)
	}
}

func TestStorageRollupOnce_LayerFnErrorIsFailSoft(t *testing.T) {
	// layerFn errors per app should also be skipped.
	store := &fakeStore{
		apps: []AppRow{
			{AccountID: "acct_a", AppID: "app_1"},
			{AccountID: "acct_a", AppID: "app_2"},
		},
	}
	failingLayer := func(_ context.Context, appID string) (int64, error) {
		if appID == "app_2" {
			return 0, errors.New("overlay staging not yet wired")
		}
		return 0, nil
	}
	n, err := StorageRollupOnce(context.Background(), store, time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC), failingLayer, nil)
	if err == nil {
		t.Fatal("err = nil, want fail-soft first error from layerFn")
	}
	if n != 1 {
		t.Errorf("n = %d, want 1 (only app_1 succeeded)", n)
	}
}
