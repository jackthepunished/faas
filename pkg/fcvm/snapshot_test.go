package fcvm

import "testing"

func TestSnapshotUsable(t *testing.T) {
	good := &Snapshot{FCVersion: "1.7.0", StorageKey: "snap/d/mem", VMStatePath: "/s"}
	tests := []struct {
		name    string
		snap    *Snapshot
		version string
		want    bool
	}{
		{"nil is never usable", nil, "1.7.0", false},
		{"match", good, "1.7.0", true},
		{"version mismatch (ADR-005)", good, "1.8.0", false},
		{"stale", &Snapshot{FCVersion: "1.7.0", Stale: true, StorageKey: "snap/d/mem", VMStatePath: "/s"}, "1.7.0", false},
		// #96 slice 3 contract: StorageKey is the only mem blob locator;
		// vmstate is acceptable via either VMStatePath (legacy host path,
		// default-local / single-box) or VMStateStorageKey (canonical
		// StorageBackend key, remote / multi-node, #121 ADR-025 axis 2
		// slice 4).
		{"missing storage key", &Snapshot{FCVersion: "1.7.0", VMStatePath: "/s"}, "1.7.0", false},
		{"missing vmstate", &Snapshot{FCVersion: "1.7.0", StorageKey: "snap/d/mem"}, "1.7.0", false},
		// #121: vmstate via storage key alone (remote-node shape, no host
		// path) is usable. A regression that re-tightens the predicate to
		// require VMStatePath would silently fail every multi-node wake.
		{"vmstate via storage key", &Snapshot{FCVersion: "1.7.0", StorageKey: "snap/d/mem", VMStateStorageKey: "snap/d/vmstate"}, "1.7.0", true},
		// #121: vmstate via either locator, the engine-populated shape
		// (both fields carried for diagnostic logging on remote nodes).
		{"vmstate via both locators", &Snapshot{FCVersion: "1.7.0", StorageKey: "snap/d/mem", VMStatePath: "/s", VMStateStorageKey: "snap/d/vmstate"}, "1.7.0", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.snap.Usable(tt.version); got != tt.want {
				t.Errorf("Usable() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPlanWake(t *testing.T) {
	usable := &Snapshot{FCVersion: "1.7.0", StorageKey: "snap/d/mem", VMStatePath: "/s"}
	if PlanWake(usable, "1.7.0") != WakeRestore {
		t.Error("usable snapshot should plan a restore")
	}
	if PlanWake(usable, "9.9.9") != WakeColdBoot {
		t.Error("version-mismatched snapshot should plan a cold boot")
	}
	if PlanWake(nil, "1.7.0") != WakeColdBoot {
		t.Error("no snapshot should plan a cold boot")
	}
}

func TestWakeMethodString(t *testing.T) {
	if WakeRestore.String() != "restore" || WakeColdBoot.String() != "cold_boot" {
		t.Errorf("unexpected method strings: %s %s", WakeRestore, WakeColdBoot)
	}
}

// FuzzPlanWakeFCVersionMismatchFallsBack pins ADR-005 (snapshots are
// cache, never truth) at the FC-version-mismatch seam: any change of
// firecracker version (or, more importantly, any future contributor
// who weakens the equality check in Usable() to a prefix or
// semver-style comparison) must never silently restore a snapshot
// made with a different version. The fuzz target exercises the
// PlanWake ↔ Usable pair across arbitrary FC-version strings and
// snapshot metadata; for each input, PlanWake must return
// WakeRestore iff snap.FCVersion == currentFCVersion AND snap is
// otherwise usable. The seed corpus covers the canonical cases; the
// fuzz engine generates new mutations from there.
//
// CLAUDE.md: "Invariants — enforce with property-based tests, never
// delete." Mirror: FuzzAllocatorNoLiveCollision in alloc_property_test.go.
//
// Note: this test runs the seed corpus during ordinary `go test`.
// Sustained fuzzing is a developer-machine command — current CI has
// no -fuzz= lane (verified, no invocation in .github/workflows).
// To fuzz continuously:
//
//	go test ./pkg/fcvm -run '^$' -fuzz '^FuzzPlanWakeFCVersionMismatchFallsBack$' -fuzztime=10s
func FuzzPlanWakeFCVersionMismatchFallsBack(f *testing.F) {
	// Matching version, usable snapshot → expect WakeRestore.
	// (staleFlag=0 means !Stale.)
	f.Add(uint8(0), "1.7.0", "1.7.0", "snap/d/mem", "snap/d/vmstate", "/srv/fc/snap/d/vmstate")
	// Mismatched version, otherwise usable → expect WakeColdBoot.
	// This is the ADR-005 load-bearing case the test guards.
	f.Add(uint8(0), "1.7.0", "1.8.0", "snap/d/mem", "snap/d/vmstate", "/srv/fc/snap/d/vmstate")
	// Empty current version (FC not detected at boot) → cold boot.
	f.Add(uint8(0), "1.7.0", "", "snap/d/mem", "snap/d/vmstate", "/srv/fc/snap/d/vmstate")
	// Empty snapshot version (legacy row from before #96) → cold boot.
	f.Add(uint8(0), "", "1.7.0", "snap/d/mem", "snap/d/vmstate", "/srv/fc/snap/d/vmstate")
	// Version-with-suffix: "1.7.0" vs "1.7.0+custom" must NOT match.
	f.Add(uint8(0), "1.7.0", "1.7.0+custom", "snap/d/mem", "snap/d/vmstate", "/srv/fc/snap/d/vmstate")
	// Stale snapshot (staleFlag=1) → cold boot even with matching version.
	f.Add(uint8(1), "1.7.0", "1.7.0", "snap/d/mem", "snap/d/vmstate", "/srv/fc/snap/d/vmstate")
	// Usable via legacy VMStatePath only (no storage key), version mismatch → cold boot.
	f.Add(uint8(0), "1.7.0", "1.8.0", "snap/d/mem", "", "/srv/fc/snap/d/vmstate")
	// Usable via VMStateStorageKey only (no host path), version match → restore.
	f.Add(uint8(0), "1.7.0", "1.7.0", "snap/d/mem", "snap/d/vmstate", "")
	// Empty fields, both versions match → still cold boot (no StorageKey).
	f.Add(uint8(0), "1.7.0", "1.7.0", "", "", "")
	// nil-equivalent path: empty everything, staleFlag set → cold boot.
	f.Add(uint8(1), "", "", "", "", "")

	f.Fuzz(func(t *testing.T, staleFlag uint8, snapFC, currentFC, storageKey, vmstateStorageKey, vmstatePath string) {
		snap := &Snapshot{
			FCVersion:         snapFC,
			StorageKey:        storageKey,
			VMStateStorageKey: vmstateStorageKey,
			VMStatePath:       vmstatePath,
			// Low bit of staleFlag selects the Stale branch. Using a
			// bit instead of a magic-string keeps the fuzzer's
			// mutations targeted: any change to staleFlag flips the
			// branch, so both stale=true and stale=false paths get
			// exercised across the corpus.
			Stale: staleFlag&0x01 == 0x01,
		}
		got := PlanWake(snap, currentFC)
		// Independent recomputation of the expected result so the
		// fuzz target doesn't share a bug with the production code.
		usable := snap != nil && !snap.Stale &&
			snap.StorageKey != "" &&
			(snap.VMStateStorageKey != "" || snap.VMStatePath != "") &&
			snap.FCVersion == currentFC
		want := WakeColdBoot
		if usable {
			want = WakeRestore
		}
		if got != want {
			t.Errorf("PlanWake(snapFC=%q, currentFC=%q, storage=%q, vmstateKey=%q, vmstatePath=%q, stale=%v) = %v, want %v",
				snapFC, currentFC, storageKey, vmstateStorageKey, vmstatePath, snap.Stale, got, want)
		}
		// ADR-005 cross-check: a version mismatch must NEVER produce
		// WakeRestore, even if every other field is populated. This
		// is the contract every future contributor to Usable() must
		// keep intact.
		if snapFC != currentFC && got == WakeRestore {
			t.Errorf("PlanWake restored despite FC version mismatch: snapFC=%q currentFC=%q",
				snapFC, currentFC)
		}
	})
}
