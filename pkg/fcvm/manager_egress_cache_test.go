package fcvm

import (
	"net/netip"
	"testing"
)

// TestPerAppAllowlistCache_PreservesOriginalUnderBundleSwap
// pins the per-app cache invariant (issue #679 / PR-A / PR-723
// review fix #2): SetEgressOperatorBundle must read the
// authoritative per-app slice from the cache, NOT from
// inst.Net.EgressAllowlist (which is the merged slice stamped
// by Wake). Without the cache, a subsequent bundle change
// would re-merge the previous operator bundle on top of the
// new one, breaking operator subtraction.
//
// The test exercises the cache directly: it mirrors what Wake
// and UpdateEgressAllowlist do (write the per-app slice keyed
// by appID) and what SetEgressOperatorBundle reads (the same
// cache). It does not boot Wake or UpdateEgressAllowlist
// because those need a working netns + nft exec; the contract
// being tested is the cache itself.
func TestPerAppAllowlistCache_PreservesOriginalUnderBundleSwap(t *testing.T) {
	m := NewManager(nil, nil, Paths{}, "", nil, nil)
	appID := "a-uuid"

	// Wake: cache the per-app slice via the writer-side API
	// (UpdateEgressAllowlist), which is the same path Wake uses.
	perApp := []netip.Prefix{
		netip.MustParsePrefix("1.2.3.0/24"),
		netip.MustParsePrefix("10.0.0.0/8"),
	}
	// We have no live instances, so the cache write is the
	// only side-effect we care about. UpdateEgressAllowlist
	// returns nil for an empty live map.
	if err := m.UpdateEgressAllowlist(nil, appID, perApp); err != nil {
		t.Fatalf("UpdateEgressAllowlist (seed cache): %v", err)
	}

	// Round-trip the cache directly: read it back via the same
	// snapshot path SetEgressOperatorBundle uses.
	got := m.perAppAllowlistSnapshot(appID)
	if len(got) != 2 {
		t.Fatalf("cache len = %d, want 2 (per-app slice must survive)", len(got))
	}
	if got[0] != perApp[0] || got[1] != perApp[1] {
		t.Errorf("cache = %v, want %v", got, perApp)
	}

	// Simulate the operator editing the bundle file from B → C
	// and SIGHUPing. The cache MUST still say perApp — the
	// per-app slice is the only authoritative source.
	m.SetEgressOperatorBundle([]netip.Prefix{
		netip.MustParsePrefix("203.0.113.0/24"),
	})
	got = m.perAppAllowlistSnapshot(appID)
	if len(got) != 2 {
		t.Fatalf("after bundle set, cache len = %d, want 2", len(got))
	}
	if got[0] != perApp[0] || got[1] != perApp[1] {
		t.Errorf("after bundle set, cache = %v, want %v", got, perApp)
	}

	// Second bundle change (B → empty). The cache STILL says
	// perApp — proof that the operator can subtract reachability
	// by editing the bundle.
	m.SetEgressOperatorBundle(nil)
	got = m.perAppAllowlistSnapshot(appID)
	if len(got) != 2 {
		t.Fatalf("after empty bundle, cache len = %d, want 2", len(got))
	}
	if got[0] != perApp[0] || got[1] != perApp[1] {
		t.Errorf("after empty bundle, cache = %v, want %v", got, perApp)
	}
}

// TestPerAppAllowlistCache_WriteThenOverwrite pins the per-app
// cache's overwrite semantics: a Wake followed by a PATCH
// updates the cache to the latest per-app slice. The store
// mirrors what the apid endpoint + Wake do in production.
func TestPerAppAllowlistCache_WriteThenOverwrite(t *testing.T) {
	m := NewManager(nil, nil, Paths{}, "", nil, nil)
	appID := "a-uuid"

	first := []netip.Prefix{netip.MustParsePrefix("1.2.3.0/24")}
	if err := m.UpdateEgressAllowlist(nil, appID, first); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	second := []netip.Prefix{
		netip.MustParsePrefix("5.6.7.0/24"),
		netip.MustParsePrefix("198.51.100.0/24"),
	}
	if err := m.UpdateEgressAllowlist(nil, appID, second); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	got := m.perAppAllowlistSnapshot(appID)
	if len(got) != 2 {
		t.Fatalf("cache len = %d, want 2", len(got))
	}
	if got[0] != second[0] || got[1] != second[1] {
		t.Errorf("cache = %v, want %v", got, second)
	}
}

// TestPerAppAllowlistCache_UnknownAppIDReturnsNil pins the
// "no per-app slice recorded yet" path: a SetEgressOperatorBundle
// before any Wake has happened is a no-op (no instance to
// patch, no per-app slice to merge).
func TestPerAppAllowlistCache_UnknownAppIDReturnsNil(t *testing.T) {
	m := NewManager(nil, nil, Paths{}, "", nil, nil)

	got := m.perAppAllowlistSnapshot("never-seen-app")
	if got != nil {
		t.Errorf("cache = %v, want nil", got)
	}

	// No per-app slices = no UpdateEgressAllowlist calls.
	// SetEgressOperatorBundle must not panic, must not call
	// UpdateEgressAllowlist (no targets).
	m.SetEgressOperatorBundle([]netip.Prefix{
		netip.MustParsePrefix("203.0.113.0/24"),
	})
}

// perAppAllowlistSnapshot is exposed here for whitebox tests
// ONLY; the production reader is the one inside
// SetEgressOperatorBundle. Issue #679 / PR-A.
func (m *Manager) perAppAllowlistSnapshot(appID string) []netip.Prefix {
	m.perAppAllowlistMu.RLock()
	defer m.perAppAllowlistMu.RUnlock()
	slice, ok := m.perAppAllowlist[appID]
	if !ok {
		return nil
	}
	cp := make([]netip.Prefix, len(slice))
	copy(cp, slice)
	return cp
}
