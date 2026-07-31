// Unit tests for the Registry lifecycle. The MountExt4ReadOnly /
// UmountExt4 syscalls require root + a loop device, so the
// bufconn-style end-to-end coverage lives in pkg/vmmdgrpc; this
// file pins the registry's bookkeeping in isolation.
package vmmdmount

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRegistry_NewDefaults: zero cap falls back to 16; positive
// cap is preserved. The cap is load-shedding — a misbehaving
// imaged can't grow the map unboundedly.
func TestRegistry_NewDefaults(t *testing.T) {
	for _, c := range []struct {
		in, want int
	}{
		{0, 16}, {-1, 16}, {32, 32}, {1, 1},
	} {
		r := NewRegistry(c.in)
		if r.cap != c.want {
			t.Errorf("NewRegistry(%d).cap = %d, want %d", c.in, r.cap, c.want)
		}
		if len(r.entries) != 0 {
			t.Errorf("fresh registry has %d entries, want 0", len(r.entries))
		}
	}
}

// TestRegistry_RegisterOrEvict_HappyPath: register two entries
// without exceeding cap. Both Lookup-able.
func TestRegistry_RegisterOrEvict_HappyPath(t *testing.T) {
	r := NewRegistry(8)
	if ev := r.RegisterOrEvict("/tmp/a", "key-a", "/src/a"); ev != "" {
		t.Errorf("unexpected eviction on first register: %q", ev)
	}
	if ev := r.RegisterOrEvict("/tmp/b", "key-b", "/src/b"); ev != "" {
		t.Errorf("unexpected eviction on second register: %q", ev)
	}
	for _, mp := range []string{"/tmp/a", "/tmp/b"} {
		if _, ok := r.Lookup(mp); !ok {
			t.Errorf("Lookup(%q) = false, want true", mp)
		}
	}
}

// TestRegistry_RegisterOrEvict_LoadShedsOldest: when the cap is
// reached, the oldest registered entry is force-evicted (the
// returned mountpoint path is non-empty). The new entry is in
// the registry.
func TestRegistry_RegisterOrEvict_LoadShedsOldest(t *testing.T) {
	r := NewRegistry(2)
	r.RegisterOrEvict("/tmp/old1", "k1", "/s1")
	// Force a measurable mountedAt gap so the load-shed
	// deterministically picks the OLDEST (old1), not new1.
	timeSleep(t, 10)
	r.RegisterOrEvict("/tmp/new1", "k2", "/s2")
	timeSleep(t, 10)
	ev := r.RegisterOrEvict("/tmp/new2", "k3", "/s3")
	if ev != "/tmp/old1" {
		t.Errorf("evicted = %q, want %q (oldest)", ev, "/tmp/old1")
	}
	if _, ok := r.Lookup("/tmp/old1"); ok {
		t.Error("evicted entry still in registry")
	}
	if _, ok := r.Lookup("/tmp/new2"); !ok {
		t.Error("newest entry not registered")
	}
}

// TestRegistry_Forget_Idempotent: forgetting an unknown key is a
// no-op. The Umount path relies on this for safe retry after a
// partial failure.
func TestRegistry_Forget_Idempotent(t *testing.T) {
	r := NewRegistry(8)
	r.RegisterOrEvict("/tmp/x", "k", "/s")
	r.Forget("/tmp/x")
	r.Forget("/tmp/x") // must not panic
	if _, ok := r.Lookup("/tmp/x"); ok {
		t.Error("entry still in registry after Forget")
	}
}

// TestRegistry_SweepOrphans_EmptyNoOp: SweepOrphans on an empty
// registry returns 0 and does not panic. Pinned because cmd/vmmd
// runs the sweep on a 5-minute ticker that fires before any
// imaged child could register.
func TestRegistry_SweepOrphans_EmptyNoOp(t *testing.T) {
	r := NewRegistry(8)
	if n := r.SweepOrphans(slog.Default()); n != 0 {
		t.Errorf("empty sweep = %d, want 0", n)
	}
}

// TestUmountExt4_RefusesForeignPath: defence-in-depth guard. The
// prefix check stops a caller from handing back a path outside
// vmmd's parent-mnt scratch dir. Pinned because silently
// umounting an unrelated mount would be a security incident.
func TestUmountExt4_RefusesForeignPath(t *testing.T) {
	for _, bad := range []string{"/", "/home/foo", "/etc/passwd", "/var/log/faas"} {
		err := UmountExt4(bad)
		if err == nil {
			t.Errorf("UmountExt4(%q) = nil, want prefix-check error", bad)
		}
		if !strings.Contains(err.Error(), "outside vmmd's parent-mnt scratch") {
			t.Errorf("UmountExt4(%q) = %v, want prefix-check error", bad, err)
		}
	}
}

// TestUmountExt4_EmptyReturnsUnknown: empty mountpoint surfaces
// ErrUnknownMountpoint so the gRPC handler lifts to
// InvalidArgument (NOT NotFound — the asymmetry with
// MountParentExt4ReadOnly's NotFound is load-bearing).
func TestUmountExt4_EmptyReturnsUnknown(t *testing.T) {
	err := UmountExt4("")
	if !errors.Is(err, ErrUnknownMountpoint) {
		t.Errorf("UmountExt4(\"\") = %v, want ErrUnknownMountpoint", err)
	}
}

// TestUmountExt4_NonexistentReturnsUnknown: a never-issued path
// under the scratch dir (no parent dir at all) returns
// ErrUnknownMountpoint so imaged's defer-after-error pattern is
// idempotent. The kernel umount syscall would surface EINVAL for
// a non-mountpoint path, which we want to mask behind
// ErrUnknownMountpoint — the gRPC handler lifts it to
// InvalidArgument, which is the right wire code for "I never
// gave you this path".
func TestUmountExt4_NonexistentReturnsUnknown(t *testing.T) {
	missing := filepath.Join(os.TempDir(), ParentMountPrefix+"nonexistent-99999")
	err := UmountExt4(missing)
	if !errors.Is(err, ErrUnknownMountpoint) {
		t.Errorf("UmountExt4(%q) = %v, want ErrUnknownMountpoint", missing, err)
	}
}

// timeSleep is a tiny helper so the load-shed test can register
// entries with a measurable mountedAt gap (the sweep / load-shed
// code compares time.Now() deltas, and sub-millisecond
// registrations all read as "same instant").
func timeSleep(t *testing.T, ms int) {
	t.Helper()
	time.Sleep(time.Duration(ms) * time.Millisecond)
}
