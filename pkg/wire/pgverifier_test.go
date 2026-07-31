// Tests for PGNodeVerifier (ADR-056).
//
// Coverage:
//   - Refresh replaces the snapshot on success.
//   - Loader failure keeps last-known-good (the load-bearing safety
//     property — a transient DB blip must not de-sync to "allow
//     nothing" and brick every mTLS leg).
//   - LookupCN accepts registered names and rejects unknown names.
//   - nil-receiver LookupCN returns nil (AllowAll).
//   - Refresh with nil receiver / nil loader errors loudly.
//   - Run drains the channel until ctx cancel.
//   - The drain-loop survives a loader failure on a delivered
//     notification (last-known-good).

package wire

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/db"
)

// stubNodeLoader is a map-backed NodeLoader for tests. errFn
// (optional) lets a test inject a transient failure on the Nth call.
type stubNodeLoader struct {
	rows  []NodeRow
	calls atomic.Int32
	errFn func(call int32) error
}

func (s *stubNodeLoader) LoadNodes(_ context.Context) ([]NodeRow, error) {
	n := s.calls.Add(1)
	if s.errFn != nil {
		if err := s.errFn(n); err != nil {
			return nil, err
		}
	}
	// Return a copy so callers can't mutate the stub's slice.
	out := make([]NodeRow, len(s.rows))
	copy(out, s.rows)
	return out, nil
}

func newSilentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestPGNodeVerifier_RefreshReplacesSnapshot(t *testing.T) {
	loader := &stubNodeLoader{rows: []NodeRow{
		{CN: "vmmd", ID: "uuid-1"},
		{CN: "schedd", ID: "uuid-2"},
	}}
	v := NewPGNodeVerifier(loader, newSilentLogger())

	if got := v.Size(); got != 0 {
		t.Fatalf("Size()=%d on empty verifier; want 0", got)
	}

	n, err := v.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh err=%v; want nil", err)
	}
	if n != 2 {
		t.Fatalf("Refresh n=%d; want 2", n)
	}

	if err := v.LookupCN("vmmd"); err != nil {
		t.Errorf("LookupCN(vmmd)=%v; want nil", err)
	}
	if err := v.LookupCN("schedd"); err != nil {
		t.Errorf("LookupCN(schedd)=%v; want nil", err)
	}
	if err := v.LookupCN("unknown"); !errors.Is(err, ErrNodeVerifierCNMismatch) {
		t.Errorf("LookupCN(unknown)=%v; want ErrNodeVerifierCNMismatch", err)
	}
}

// TestPGNodeVerifier_LoaderFailureKeepsLastKnownGood is the
// load-bearing safety property: a transient loader failure on the
// SECOND Refresh must not erase the snapshot the FIRST Refresh
// populated. Without this, a single Postgres hiccup would brick
// every mTLS leg in the cluster.
func TestPGNodeVerifier_LoaderFailureKeepsLastKnownGood(t *testing.T) {
	loader := &stubNodeLoader{rows: []NodeRow{
		{CN: "vmmd", ID: "uuid-1"},
	}}
	loader.errFn = func(call int32) error {
		if call == 2 {
			return errors.New("synthetic loader failure")
		}
		return nil
	}
	v := NewPGNodeVerifier(loader, newSilentLogger())

	// First Refresh succeeds — snapshot populated.
	if _, err := v.Refresh(context.Background()); err != nil {
		t.Fatalf("first Refresh err=%v; want nil", err)
	}
	if got := v.Size(); got != 1 {
		t.Fatalf("Size after first Refresh=%d; want 1", got)
	}

	// Second Refresh fails — snapshot must be preserved.
	_, err := v.Refresh(context.Background())
	if err == nil {
		t.Fatalf("second Refresh err=nil; want non-nil")
	}
	if got := v.Size(); got != 1 {
		t.Fatalf("Size after failed Refresh=%d; want 1 (last-known-good)", got)
	}
	if err := v.LookupCN("vmmd"); err != nil {
		t.Errorf("LookupCN(vmmd) after failed Refresh=%v; want nil (last-known-good)", err)
	}
}

func TestPGNodeVerifier_LookupCN_NilReceiverIsAllowAll(t *testing.T) {
	var v *PGNodeVerifier
	if err := v.LookupCN("anything"); err != nil {
		t.Errorf("nil receiver LookupCN()=%v; want nil (AllowAll)", err)
	}
	if got := v.Size(); got != 0 {
		t.Errorf("nil receiver Size()=%d; want 0", got)
	}
}

func TestPGNodeVerifier_Refresh_NilReceiverErrors(t *testing.T) {
	var v *PGNodeVerifier
	if _, err := v.Refresh(context.Background()); err == nil {
		t.Errorf("Refresh on nil receiver returned nil err; want non-nil")
	}
}

func TestPGNodeVerifier_Refresh_NilLoaderErrors(t *testing.T) {
	v := NewPGNodeVerifier(nil, newSilentLogger())
	if _, err := v.Refresh(context.Background()); err == nil {
		t.Errorf("Refresh with nil loader returned nil err; want non-nil")
	}
}

func TestPGNodeVerifier_Refresh_SkipsEmptyCNRows(t *testing.T) {
	loader := &stubNodeLoader{rows: []NodeRow{
		{CN: "vmmd", ID: "uuid-1"},
		{CN: "", ID: "uuid-bad"},
		{CN: "schedd", ID: "uuid-2"},
	}}
	v := NewPGNodeVerifier(loader, newSilentLogger())
	n, err := v.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh err=%v; want nil", err)
	}
	if n != 2 {
		t.Fatalf("Refresh n=%d; want 2 (empty CN row skipped)", n)
	}
}

func TestPGNodeVerifier_Run_DrainsUntilCancel(t *testing.T) {
	loader := &stubNodeLoader{rows: []NodeRow{
		{CN: "vmmd", ID: "uuid-1"},
	}}
	v := NewPGNodeVerifier(loader, newSilentLogger())

	// First Refresh before Run, so a CN is registered.
	if _, err := v.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh err=%v; want nil", err)
	}

	ch := make(chan db.Notification, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- v.Run(ctx, ch) }()

	// Drain should keep refreshing on every notification.
	ch <- db.Notification{Channel: db.NotifyComputeNodeChanged}
	ch <- db.Notification{Channel: db.NotifyComputeNodeChanged}

	// Give the drain time to process at least one notification.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if loader.calls.Load() >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := loader.calls.Load(); got < 2 {
		t.Errorf("loader calls=%d; want >= 2 (Refresh + at least one notify-driven Refresh)", got)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run err=%v; want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// TestPGNodeVerifier_Run_SurvivesLoaderFailure asserts the drain
// loop keeps spinning after a loader failure on a notification
// (last-known-good preserved across multiple notify ticks).
func TestPGNodeVerifier_Run_SurvivesLoaderFailure(t *testing.T) {
	loader := &stubNodeLoader{rows: []NodeRow{
		{CN: "vmmd", ID: "uuid-1"},
	}}
	loader.errFn = func(call int32) error {
		if call == 2 {
			return errors.New("synthetic failure")
		}
		return nil
	}
	v := NewPGNodeVerifier(loader, newSilentLogger())

	ch := make(chan db.Notification, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- v.Run(ctx, ch) }()

	// Initial Refresh inside Run was never called — but the first
	// notify drives a Refresh that fails. Last-known-good stays
	// empty (no prior snapshot), but the loop survives and tries
	// again on the next notify.
	ch <- db.Notification{Channel: db.NotifyComputeNodeChanged}
	ch <- db.Notification{Channel: db.NotifyComputeNodeChanged}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if loader.calls.Load() >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := loader.calls.Load(); got < 2 {
		t.Errorf("loader calls=%d after failed notify; want >= 2 (loop survived failure)", got)
	}

	cancel()
	<-done
}

func TestPGNodeVerifier_Run_NilReceiverBlocksUntilCancel(t *testing.T) {
	var v *PGNodeVerifier
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- v.Run(ctx, make(chan db.Notification)) }()

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run err=%v; want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}
