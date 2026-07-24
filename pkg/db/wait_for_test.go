package db

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

// TestWaitForNotification_PredicateMatch fires a notify that matches the
// predicate; the helper must return the payload within the timeout.
func TestWaitForNotification_PredicateMatch(t *testing.T) {
	pool := pgtest.Open(t)
	t.Cleanup(func() { pool.Close() })

	const ch = "wait_for_test_match"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Producer goroutine: brief sleep so the consumer registers LISTEN first.
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = Notify(ctx, pool, ch, `{"invocation_id":"abc-123"}`)
	}()

	payload, err := WaitForNotification(ctx, pool, ch,
		func(p string) bool { return strings.Contains(p, "abc-123") },
		3*time.Second)
	if err != nil {
		t.Fatalf("WaitForNotification: %v", err)
	}
	if !strings.Contains(payload, "abc-123") {
		t.Errorf("payload = %q, want contains abc-123", payload)
	}
}

// TestWaitForNotification_Timeout fires no notify; the helper must
// return ErrWaitTimeout after the timeout elapses.
func TestWaitForNotification_Timeout(t *testing.T) {
	pool := pgtest.Open(t)
	t.Cleanup(func() { pool.Close() })

	const ch = "wait_for_test_timeout"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	_, err := WaitForNotification(ctx, pool, ch,
		func(p string) bool { return false }, 250*time.Millisecond)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("err = %v, want ErrWaitTimeout", err)
	}
	if elapsed < 200*time.Millisecond {
		t.Errorf("elapsed = %v, want >= 200ms (timeout should have been respected)", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("elapsed = %v, want < 2s (timeout should not have taken much longer)", elapsed)
	}
}

// TestWaitForNotification_PredicateNoMatch fires a non-matching payload
// then a matching one; the helper must keep waiting and return the
// second.
func TestWaitForNotification_PredicateNoMatch(t *testing.T) {
	pool := pgtest.Open(t)
	t.Cleanup(func() { pool.Close() })

	const ch = "wait_for_test_no_match"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		time.Sleep(100 * time.Millisecond)
		_ = Notify(ctx, pool, ch, `{"invocation_id":"other-1"}`)
	}()
	go func() {
		defer wg.Done()
		time.Sleep(300 * time.Millisecond)
		_ = Notify(ctx, pool, ch, `{"invocation_id":"target-9"}`)
	}()

	payload, err := WaitForNotification(ctx, pool, ch,
		func(p string) bool { return strings.Contains(p, "target-9") },
		3*time.Second)
	if err != nil {
		t.Fatalf("WaitForNotification: %v", err)
	}
	if !strings.Contains(payload, "target-9") {
		t.Errorf("payload = %q, want contains target-9 (must be the SECOND notification)", payload)
	}
	wg.Wait()
}

// TestWaitForNotification_CtxCancel cancels the caller's context mid-wait;
// the helper must return ctx.Err() (not ErrWaitTimeout).
func TestWaitForNotification_CtxCancel(t *testing.T) {
	pool := pgtest.Open(t)
	t.Cleanup(func() { pool.Close() })

	const ch = "wait_for_test_cancel"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

	// Cancel after 150ms — well before the 5s timeout.
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	_, err := WaitForNotification(ctx, pool, ch,
		func(p string) bool { return false }, 5*time.Second)
	if err == nil {
		t.Fatal("err = nil, want non-nil")
	}
	if errors.Is(err, ErrWaitTimeout) {
		t.Errorf("err = ErrWaitTimeout, want ctx.Canceled (caller-cancel should NOT map to timeout)")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want contains context.Canceled", err)
	}
}

// TestWaitForNotification_ZeroTimeout rejects timeout=0 up-front so a
// buggy caller cannot hang a connection. The helper must return
// ErrWaitTimeout immediately.
func TestWaitForNotification_ZeroTimeout(t *testing.T) {
	pool := pgtest.Open(t)
	t.Cleanup(func() { pool.Close() })

	_, err := WaitForNotification(context.Background(), pool, "wait_for_test_zero",
		func(p string) bool { return true }, 0)
	if !errors.Is(err, ErrWaitTimeout) {
		t.Errorf("err = %v, want ErrWaitTimeout", err)
	}
}
