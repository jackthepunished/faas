package fcvm

import (
	"net/netip"
	"testing"
)

// TestMergeOperatorBundle_EmptyIsNoop pins the no-bundle baseline
// (issue #679 / PR-A): an unset operator bundle returns the per-app
// slice unchanged. Pre-PR-A this was the only path; the PR-A merge
// must preserve it exactly so existing apps keep their behaviour.
func TestMergeOperatorBundle_EmptyIsNoop(t *testing.T) {
	m := NewManager(nil, nil, Paths{}, "", nil, nil)
	perApp := []netip.Prefix{
		mustPrefixTest(t, "1.2.3.0/24"),
		mustPrefixTest(t, "10.0.0.0/8"),
	}
	got := m.mergeOperatorBundle(perApp)
	if len(got) != 2 {
		t.Fatalf("merged len = %d, want 2 (empty bundle = no-op)", len(got))
	}
	for i, p := range got {
		if p != perApp[i] {
			t.Errorf("merged[%d] = %s, want %s", i, p, perApp[i])
		}
	}
}

// TestMergeOperatorBundle_AppendsAndDedups pins the merge contract:
// per-app first, operator appended, dedup at boundaries. /0 entries
// dropped at the dedup layer (defence in depth — the loader already
// strips, but a wire-bypass could otherwise smuggle one).
func TestMergeOperatorBundle_AppendsAndDedups(t *testing.T) {
	m := NewManager(nil, nil, Paths{}, "", nil, nil)
	m.SetEgressOperatorBundle([]netip.Prefix{
		mustPrefixTest(t, "10.0.0.0/8"),     // dup with per-app
		mustPrefixTest(t, "203.0.113.0/24"), // new
	})
	perApp := []netip.Prefix{
		mustPrefixTest(t, "1.2.3.0/24"),
		mustPrefixTest(t, "10.0.0.0/8"),
	}
	got := m.mergeOperatorBundle(perApp)
	want := []netip.Prefix{
		mustPrefixTest(t, "1.2.3.0/24"),
		mustPrefixTest(t, "10.0.0.0/8"),
		mustPrefixTest(t, "203.0.113.0/24"),
	}
	if len(got) != len(want) {
		t.Fatalf("merged len = %d, want %d (got=%v)", len(got), len(want), got)
	}
	for i, p := range got {
		if p != want[i] {
			t.Errorf("merged[%d] = %s, want %s", i, p, want[i])
		}
	}
}

// TestMergeOperatorBundle_EmptyAppEmptyOperator is the pre-PR-A
// "no allowlist at all" path — chain policy stays accept, no rule
// emitted.
func TestMergeOperatorBundle_EmptyAppEmptyOperator(t *testing.T) {
	m := NewManager(nil, nil, Paths{}, "", nil, nil)
	got := m.mergeOperatorBundle(nil)
	if len(got) != 0 {
		t.Errorf("merged = %v, want empty", got)
	}
}

// TestMergeOperatorBundle_DropsZeroMaskFromWire is the
// defence-in-depth /0 reject at the dedup layer. Same contract
// as the loader and the per-app parser at manager.go:1900-1912.
func TestMergeOperatorBundle_DropsZeroMaskFromWire(t *testing.T) {
	m := NewManager(nil, nil, Paths{}, "", nil, nil)
	m.SetEgressOperatorBundle([]netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/0"),
		mustPrefixTest(t, "203.0.113.0/24"),
		netip.MustParsePrefix("::/0"),
	})
	got := m.mergeOperatorBundle(nil)
	if len(got) != 1 {
		t.Fatalf("merged len = %d, want 1 (both /0 entries dropped); got=%v", len(got), got)
	}
	if got[0] != mustPrefixTest(t, "203.0.113.0/24") {
		t.Errorf("merged[0] = %s, want 203.0.113.0/24", got[0])
	}
}

// TestDedupSortedPrefixes_PreservesFirstSeen pins the
// order-preservation contract: per-app entries arrive first and
// stay first; the dedup is non-shuffling.
func TestDedupSortedPrefixes_PreservesFirstSeen(t *testing.T) {
	in := []netip.Prefix{
		mustPrefixTest(t, "10.0.0.0/8"),
		mustPrefixTest(t, "1.2.3.0/24"),
		mustPrefixTest(t, "10.0.0.0/8"), // dup
		mustPrefixTest(t, "203.0.113.0/24"),
		mustPrefixTest(t, "1.2.3.0/24"), // dup
	}
	got := dedupSortedPrefixes(in)
	want := []netip.Prefix{
		mustPrefixTest(t, "10.0.0.0/8"),
		mustPrefixTest(t, "1.2.3.0/24"),
		mustPrefixTest(t, "203.0.113.0/24"),
	}
	if len(got) != len(want) {
		t.Fatalf("deduped len = %d, want %d (got=%v)", len(got), len(want), got)
	}
	for i, p := range got {
		if p != want[i] {
			t.Errorf("deduped[%d] = %s, want %s", i, p, want[i])
		}
	}
}

// TestDedupSortedPrefixes_DropsZeroMask mirrors the loader's
// /0-reject at the dedup helper.
func TestDedupSortedPrefixes_DropsZeroMask(t *testing.T) {
	in := []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/0"),
		mustPrefixTest(t, "203.0.113.0/24"),
		netip.MustParsePrefix("::/0"),
	}
	got := dedupSortedPrefixes(in)
	if len(got) != 1 {
		t.Fatalf("deduped len = %d, want 1 (both /0 dropped); got=%v", len(got), got)
	}
	if got[0] != mustPrefixTest(t, "203.0.113.0/24") {
		t.Errorf("deduped[0] = %s, want 203.0.113.0/24", got[0])
	}
}

func mustPrefixTest(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return p
}
