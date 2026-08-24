package state

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
)

// TestCoverageSlice15StaticEgressIPSet drives the new
// SetStaticEgressIP branch in memstore.go::UpdateApp (ADR-119). The
// Set-bit convention is identical to SetOverflowNode / SetCORS*:
// SetStaticEgressIP=true with a non-nil pointer writes the IP and
// stamps SetAt; SetStaticEgressIP=true with nil clears both.
func TestCoverageSlice15StaticEgressIPSet(t *testing.T) {
	m, ctx, _, app, _ := memCoverageFixture(t)

	ip := netip.MustParseAddr("203.0.113.42")
	updated, err := m.UpdateApp(ctx, app.ID, UpdateAppParams{
		SetStaticEgressIP: true,
		StaticEgressIP:    &ip,
	})
	if err != nil {
		t.Fatalf("UpdateApp set static: %v", err)
	}
	if updated.StaticEgressIP == nil {
		t.Fatal("StaticEgressIP nil after set")
	}
	if updated.StaticEgressIP.String() != ip.String() {
		t.Errorf("StaticEgressIP = %s, want %s", updated.StaticEgressIP, ip)
	}
	if updated.StaticEgressIPSetAt == nil {
		t.Error("StaticEgressIPSetAt nil after set")
	}
}

// TestCoverageSlice15StaticEgressIPClear drives the nil-pointer
// clears-both-columns branch. Used by the DELETE wire shape.
func TestCoverageSlice15StaticEgressIPClear(t *testing.T) {
	m, ctx, _, app, _ := memCoverageFixture(t)

	ip := netip.MustParseAddr("203.0.113.42")
	if _, err := m.UpdateApp(ctx, app.ID, UpdateAppParams{
		SetStaticEgressIP: true,
		StaticEgressIP:    &ip,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	updated, err := m.UpdateApp(ctx, app.ID, UpdateAppParams{
		SetStaticEgressIP: true,
		StaticEgressIP:    nil,
	})
	if err != nil {
		t.Fatalf("UpdateApp clear static: %v", err)
	}
	if updated.StaticEgressIP != nil {
		t.Errorf("StaticEgressIP = %s after clear, want nil", updated.StaticEgressIP)
	}
	if updated.StaticEgressIPSetAt != nil {
		t.Errorf("StaticEgressIPSetAt = %s after clear, want nil", updated.StaticEgressIPSetAt)
	}
}

// TestCoverageSlice15StaticEgressIPNoTouch drives the
// SetStaticEgressIP=false (don't touch) branch. The columns must
// remain at their pre-PATCH values.
func TestCoverageSlice15StaticEgressIPNoTouch(t *testing.T) {
	m, ctx, _, app, _ := memCoverageFixture(t)

	ip := netip.MustParseAddr("203.0.113.42")
	if _, err := m.UpdateApp(ctx, app.ID, UpdateAppParams{
		SetStaticEgressIP: true,
		StaticEgressIP:    &ip,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	before, err := m.AppByID(ctx, app.ID)
	if err != nil {
		t.Fatalf("AppByID: %v", err)
	}

	// PATCH with SetStaticEgressIP=false (the default-zero path).
	other := netip.MustParseAddr("198.51.100.7")
	if _, err := m.UpdateApp(ctx, app.ID, UpdateAppParams{
		StaticEgressIP: &other, // ignored when Set is false
	}); err != nil {
		t.Fatalf("UpdateApp no-touch: %v", err)
	}
	after, err := m.AppByID(ctx, app.ID)
	if err != nil {
		t.Fatalf("AppByID: %v", err)
	}
	if after.StaticEgressIP == nil || after.StaticEgressIP.String() != before.StaticEgressIP.String() {
		t.Errorf("StaticEgressIP changed during no-touch PATCH: %s vs %s", after.StaticEgressIP, before.StaticEgressIP)
	}
}

// TestCoverageSlice15StaticEgressIPDefaultIsZero pins the fixture
// invariant: a fresh app has StaticEgressIP=nil + SetAt=nil. The
// migration 00325 default is NULL on both columns, and the
// MemStore's CreateApp mirrors that.
func TestCoverageSlice15StaticEgressIPDefaultIsZero(t *testing.T) {
	m, ctx, _, app, _ := memCoverageFixture(t)
	got, err := m.AppByID(ctx, app.ID)
	if err != nil {
		t.Fatalf("AppByID: %v", err)
	}
	if got.StaticEgressIP != nil {
		t.Errorf("fresh StaticEgressIP = %s, want nil", got.StaticEgressIP)
	}
	if got.StaticEgressIPSetAt != nil {
		t.Errorf("fresh StaticEgressIPSetAt = %s, want nil", got.StaticEgressIPSetAt)
	}
}

// TestCoverageSlice15StaticEgressIPCrossAppConflict drives the
// same-account cross-app conflict branch in memstore.go::UpdateApp.
// Mirrors the pgstore's apps_static_egress_ip_key partial unique
// index — the apid handler branches on errors.Is(err, ErrConflict)
// and the index-name substring to return 403 plan_static_egress_ip_quota.
func TestCoverageSlice15StaticEgressIPCrossAppConflict(t *testing.T) {
	m, ctx, _, app, _ := memCoverageFixture(t)

	ip := netip.MustParseAddr("203.0.113.42")
	if _, err := m.UpdateApp(ctx, app.ID, UpdateAppParams{
		SetStaticEgressIP: true,
		StaticEgressIP:    &ip,
	}); err != nil {
		t.Fatalf("first UpdateApp: %v", err)
	}

	// Second app on the same account tries to pin the same IP.
	second, err := m.CreateApp(ctx, App{
		AccountID: app.AccountID,
		Slug:      "second-app-" + app.Slug,
		RAMMB:     256,
		Status:    AppActive,
	})
	if err != nil {
		t.Fatalf("CreateApp second: %v", err)
	}
	_, err = m.UpdateApp(ctx, second.ID, UpdateAppParams{
		SetStaticEgressIP: true,
		StaticEgressIP:    &ip,
	})
	if err == nil {
		t.Fatal("expected ErrConflict on cross-app same-IP, got nil")
	}
	if !errors.Is(err, ErrConflict) {
		t.Errorf("err = %v, want ErrConflict", err)
	}
	if !strings.Contains(err.Error(), "apps_static_egress_ip_key") {
		t.Errorf("err %q missing index name (apId handler branch)", err.Error())
	}
}

// TestCoverageSlice15ProvisionedStaticEgressIPExists pins the
// provisioned-bucket lookup (ADR-119 operator-bundle gate).
// Covers the empty-accountID short-circuit + the !Is4 short-circuit
// + the normal hit + the normal miss branches.
func TestCoverageSlice15ProvisionedStaticEgressIPExists(t *testing.T) {
	m, ctx, _, app, _ := memCoverageFixture(t)

	ip := netip.MustParseAddr("203.0.113.42")

	// Empty accountID short-circuit returns (false, nil).
	got, err := m.ProvisionedStaticEgressIPExists(ctx, "", ip)
	if err != nil || got {
		t.Errorf("empty accountID: got (%v, %v), want (false, nil)", got, err)
	}

	// Non-v4 IP short-circuit returns (false, nil).
	v6 := netip.MustParseAddr("2001:db8::1")
	got, err = m.ProvisionedStaticEgressIPExists(ctx, app.AccountID, v6)
	if err != nil || got {
		t.Errorf("non-v4 IP: got (%v, %v), want (false, nil)", got, err)
	}

	// Miss: nothing seeded for this account yet.
	got, err = m.ProvisionedStaticEgressIPExists(ctx, app.AccountID, ip)
	if err != nil || got {
		t.Errorf("miss: got (%v, %v), want (false, nil)", got, err)
	}

	// Seed and re-query for hit.
	if err := m.ReplaceProvisionedStaticEgressIPs(ctx, app.AccountID, []netip.Addr{ip}); err != nil {
		t.Fatalf("seed Replace: %v", err)
	}
	got, err = m.ProvisionedStaticEgressIPExists(ctx, app.AccountID, ip)
	if err != nil || !got {
		t.Errorf("hit: got (%v, %v), want (true, nil)", got, err)
	}
}

// TestCoverageSlice15ReplaceProvisionedStaticEgressIPs pins the
// provisioned-bucket write (ADR-119 vmmd SIGHUP path).
// Covers the empty-accountID error branch + the non-v4 IP error
// branch + the normal replace (clear-then-insert) branch.
func TestCoverageSlice15ReplaceProvisionedStaticEgressIPs(t *testing.T) {
	m, ctx, _, app, _ := memCoverageFixture(t)

	// Empty accountID returns error.
	if err := m.ReplaceProvisionedStaticEgressIPs(ctx, "", nil); err == nil {
		t.Error("empty accountID: got nil err, want error")
	}

	// Non-v4 IP in the slice returns error.
	v6 := netip.MustParseAddr("2001:db8::1")
	if err := m.ReplaceProvisionedStaticEgressIPs(ctx, app.AccountID, []netip.Addr{v6}); err == nil {
		t.Error("non-v4 IP: got nil err, want error")
	}

	// Normal replace: 2 IPs, then a clear (empty slice replaces with empty bucket).
	ip1 := netip.MustParseAddr("203.0.113.1")
	ip2 := netip.MustParseAddr("203.0.113.2")
	if err := m.ReplaceProvisionedStaticEgressIPs(ctx, app.AccountID, []netip.Addr{ip1, ip2}); err != nil {
		t.Fatalf("first replace: %v", err)
	}
	for _, ip := range []netip.Addr{ip1, ip2} {
		got, _ := m.ProvisionedStaticEgressIPExists(ctx, app.AccountID, ip)
		if !got {
			t.Errorf("after first replace, %s missing from bucket", ip)
		}
	}

	// Clear: empty slice replaces with empty bucket.
	if err := m.ReplaceProvisionedStaticEgressIPs(ctx, app.AccountID, nil); err != nil {
		t.Fatalf("clear replace: %v", err)
	}
	for _, ip := range []netip.Addr{ip1, ip2} {
		got, _ := m.ProvisionedStaticEgressIPExists(ctx, app.AccountID, ip)
		if got {
			t.Errorf("after clear, %s still in bucket", ip)
		}
	}
}
