// Eviction-invariant tests for the per-key and per-sid debouncers.
//
// Without the time.Sleep(window); CompareAndDelete in
// touchTicket.AfterFire, the production map grows linearly with
// the union of "all keys ever authenticated" and "all sessions
// ever seen" over the daemon lifetime. These tests pin the
// eviction contract by overriding the production 30s / 5min
// windows via the in-package test seam (keyTouchWindowForTest /
// sessionTouchWindowForTest) and asserting the maps empty within
// window+epsilon.
//
// The CAS-replace branch (issue #96 / TestKeyDebounce_CASReplace-
// PreservesNewerTicket) validates that a stale ticket's
// AfterFire cannot delete a fresher firer's entry — the
// pointer-identity CompareAndDelete is the eviction version.
package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	authmw "github.com/onebox-faas/faas/pkg/auth/middleware"
	"github.com/onebox-faas/faas/pkg/session"
	"github.com/onebox-faas/faas/pkg/state"
)

func TestKeyDebounce_EvictsAfterWindow(t *testing.T) {
	mw := newMW(t, newFakeAuthn(), nil, nil, nil)
	mw.KeyTouchWindowForTest(50 * time.Millisecond)

	ticket, fire := mw.KeyDebounceShouldTouch("key-A", time.Now())
	if !fire {
		t.Fatalf("first call must fire")
	}
	if ticket == nil {
		t.Fatalf("first call must return a ticket")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer ticket.AfterFire(mw.KeyTouchWindow())
	}()
	<-done

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if mw.KeyDebounceMapSize() == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("keyDebounce map did not evict within window; size=%d", mw.KeyDebounceMapSize())
}

func TestKeyDebounce_CASReplacePreservesNewerTicket(t *testing.T) {
	mw := newMW(t, newFakeAuthn(), nil, nil, nil)
	mw.KeyTouchWindowForTest(100 * time.Millisecond)

	// First call: stores a ticket stamped "now".
	first, fire := mw.KeyDebounceShouldTouch("key-B", time.Now())
	if !fire || first == nil {
		t.Fatalf("first call: fire=%v ticket=%v", fire, first)
	}

	// Manually backdate the stored ticket so shouldTouch treats it
	// as stale and CAS-replaces it.
	past := time.Now().Add(-2 * mw.KeyTouchWindow())
	first.FiredAtSet(past)

	fresh, fire := mw.KeyDebounceShouldTouch("key-B", time.Now())
	if !fire {
		t.Fatalf("expected a fire when previous ticket is past window")
	}
	if fresh == first {
		t.Fatalf("expected a fresh ticket pointer (CAS-replace); got the stale one back")
	}

	// Detach cleanup on both tickets.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		first.AfterFire(mw.KeyTouchWindow())
	}()
	go func() {
		defer wg.Done()
		fresh.AfterFire(mw.KeyTouchWindow())
	}()
	wg.Wait()

	// Give the goroutines a beat to call CompareAndDelete.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if mw.KeyDebounceMapSize() == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("keyDebounce size=%d after window; expected 0 (CAS-replace contract broken)", mw.KeyDebounceMapSize())
}

func TestSessionDebounce_EvictsAfterWindow(t *testing.T) {
	mw := newMW(t, newFakeAuthn(), nil, nil, nil)
	mw.SessionTouchWindowForTest(50 * time.Millisecond)

	ticket, fire := mw.SessionDebounceShouldTouch("sid-A", time.Now())
	if !fire {
		t.Fatalf("first call must fire")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer ticket.AfterFire(mw.SessionTouchWindow())
	}()
	<-done

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if mw.SessionDebounceMapSize() == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("sessionDebounce map did not evict within window; size=%d", mw.SessionDebounceMapSize())
}

func TestKeyDebounce_ManyDistinctKeysAllEvict(t *testing.T) {
	mw := newMW(t, newFakeAuthn(), nil, nil, nil)
	mw.KeyTouchWindowForTest(30 * time.Millisecond)

	const N = 50
	tickets := make([]*authmw.TouchTicket, N)
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		tk, fire := mw.KeyDebounceShouldTouch(keyIDForIndex(i), time.Now())
		if !fire {
			t.Fatalf("key %d: first call did not fire", i)
		}
		tickets[i] = tk
		go func() {
			defer wg.Done()
			defer tk.AfterFire(mw.KeyTouchWindow())
		}()
	}
	wg.Wait()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if mw.KeyDebounceMapSize() == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("keyDebounce size=%d after window; expected 0", mw.KeyDebounceMapSize())
}

// TestRequireSession_BearerTouchKeyLastUsedDebounceEvicts augments
// the blackbox 1-fire case (TestRequireSession_BearerTouchKeyLastUsedDebounce)
// with a map-empty assertion so the invariant is pinned end-to-end
// through RequireSession.
func TestRequireSession_BearerTouchKeyLastUsedDebounceEvicts(t *testing.T) {
	authn := newFakeAuthn()
	hash := api.HashAPIKey(validBearerKey)
	authn.authKey[string(hash)] = authResult{acct: mkActiveAccount("acct-1"), key: mkKey("key-1", api.ScopeAdmin)}

	mw := newMW(t, authn, nil, nil, nil)
	mw.KeyTouchWindowForTest(30 * time.Millisecond)
	h := mw.RequireSession(func(_ http.ResponseWriter, _ *http.Request, _ state.Account) {})

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		r := mkRequest("GET", "/v1/apps", map[string]string{"Authorization": "Bearer " + validBearerKey}, nil)
		h(rec, r)
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if mw.KeyDebounceMapSize() == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("keyDebounce map did not evict within window; size=%d", mw.KeyDebounceMapSize())
}

func TestSessionTouchDebounce_FiresOnceAcrossRepeatedRequests(t *testing.T) {
	authn := newFakeAuthn()
	authn.acctByID["acct-1"] = mkActiveAccount("acct-1")
	sess := &fakeSessions{env: session.Envelope{AccountID: "acct-1", Sid: "sid-rep"}}
	lookups := &fakeLookups{sess: state.Session{ID: "sid-rep", AccountID: "acct-1"}}

	mw := newMW(t, authn, sess, lookups, nil)
	mw.SessionTouchWindowForTest(30 * time.Millisecond)
	h := mw.RequireSession(func(_ http.ResponseWriter, _ *http.Request, _ state.Account) {})

	cookie := "faas_sid=test-session-cookie-value"
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		r := mkRequest("GET", "/v1/account", map[string]string{"Cookie": cookie}, nil)
		h(rec, r)
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		lookups.mu.Lock()
		n := len(lookups.touchCalls)
		lookups.mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	lookups.mu.Lock()
	if got := len(lookups.touchCalls); got != 1 {
		lookups.mu.Unlock()
		t.Errorf("sessionTouch calls = %d, want 1 (debounce should fire only once)", got)
		return
	}
	lookups.mu.Unlock()

	deadline2 := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline2) {
		if mw.SessionDebounceMapSize() == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("sessionDebounce map did not evict within window; size=%d", mw.SessionDebounceMapSize())
}

func keyIDForIndex(i int) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 8)
	for j := range out {
		out[j] = digits[(i>>(j*4))&0xf]
	}
	return "key-" + string(out)
}
