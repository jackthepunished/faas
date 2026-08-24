// Tests for pkg/wire/runandshutdown.go (issue #571 PR-A2).

package wire

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func TestRunAndShutdown_DrainsOnCtxDone(t *testing.T) {
	probe := &ReadyzProbe{}
	signal := probe.Register()
	signal.Set(true, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// fn blocks on ctx.Done — same shape as every daemon's
	// serve loop. RunAndShutdown wraps it; on cancel, fn
	// returns and the drain path flips the signal.
	ranFn := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- RunAndShutdown(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), probe, "test", func(ctx context.Context, _ *slog.Logger) error {
			<-ctx.Done()
			close(ranFn)
			return nil
		})
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("RunAndShutdown returned err = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunAndShutdown did not return within 2 s of cancel")
	}
	select {
	case <-ranFn:
	default:
		t.Errorf("fn never executed")
	}
	// After RunAndShutdown returns, every signal must report
	// not-ready with "draining" reason. The post-fn MarkReady
	// doesn't touch the signals — it only touches the gauge.
	r, reason := probe.All()
	if r {
		t.Errorf("after shutdown: All() = true, want false")
	}
	if reason != "draining" {
		t.Errorf("after shutdown: reason = %q, want \"draining\"", reason)
	}
}

func TestRunAndShutdown_FnError(t *testing.T) {
	probe := &ReadyzProbe{}
	probe.Register().Set(true, "")
	wantErr := errors.New("boom")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := RunAndShutdown(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), probe, "test", func(ctx context.Context, _ *slog.Logger) error {
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Errorf("RunAndShutdown err = %v, want %v", err, wantErr)
	}
}

func TestRunAndShutdown_NilProbe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := RunAndShutdown(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, "test", func(ctx context.Context, _ *slog.Logger) error {
		return nil
	})
	if err != nil {
		t.Errorf("RunAndShutdown(nil probe) err = %v, want nil", err)
	}
}

func TestRunAndShutdown_DrainsBeforeFnReturns(t *testing.T) {
	// Confirm the ordering: when fn is still inside its
	// cleanup phase (still hasn't returned), the drain path
	// has already flipped the signals to "draining". This is
	// the load-bearing invariant — LB scraping /readyz during
	// the SIGTERM drain window must see 503.
	probe := &ReadyzProbe{}
	signal := probe.Register()
	signal.Set(true, "")

	ctx, cancel := context.WithCancel(context.Background())

	var (
		mu             sync.Mutex
		drainDoneFirst bool
		fnStillRunning bool
	)

	// Slow fn — fn sleeps 200 ms after ctx.Done to simulate a
	// real daemon's graceful listener drain. By the time fn
	// returns, the drain watcher has already flipped the
	// signal (drained flag set first).
	done := make(chan struct{})
	go func() {
		err := RunAndShutdown(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), probe, "test", func(ctx context.Context, _ *slog.Logger) error {
			<-ctx.Done()
			mu.Lock()
			fnStillRunning = true
			mu.Unlock()
			time.Sleep(200 * time.Millisecond)
			mu.Lock()
			fnStillRunning = false
			drainDoneFirst = func() bool {
				_, r := probe.All()
				return r == "draining"
			}()
			mu.Unlock()
			return nil
		})
		if err != nil {
			t.Errorf("RunAndShutdown err = %v", err)
		}
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunAndShutdown did not return within 2 s")
	}
	mu.Lock()
	defer mu.Unlock()
	if fnStillRunning {
		t.Errorf("fnStillRunning still true after return")
	}
	if !drainDoneFirst {
		t.Errorf("drain watcher did not flip signal to \"draining\" before fn returned")
	}
}

func TestReadyzProbe_Drain(t *testing.T) {
	probe := &ReadyzProbe{}
	s1 := probe.Register()
	s2 := probe.Register()
	s1.Set(true, "")
	s2.Set(true, "")
	probe.Drain("test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	r, reason := probe.All()
	if r {
		t.Errorf("after Drain: All() = true, want false")
	}
	// Two registered signals both flipped to "draining";
	// joinWireReasons concatenates with "; " — that's the
	// canonical /readyz body shape.
	if reason != "draining; draining" {
		t.Errorf("after Drain: reason = %q, want \"draining; draining\"", reason)
	}
}

func TestReadyzProbe_DrainNilProbe(t *testing.T) {
	// nil probe should not panic. Drains is no-op.
	var p *ReadyzProbe
	p.Drain("test", nil)
}
