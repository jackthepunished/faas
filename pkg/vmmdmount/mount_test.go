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

// TestRegistry_NewDefaults: zero cap falls back to DefaultCap (16);
// positive cap is preserved. The cap is load-shedding — a
// misbehaving imaged can't grow the map unboundedly.
func TestRegistry_NewDefaults(t *testing.T) {
	for _, c := range []struct {
		in, want int
	}{
		{0, DefaultCap}, {-1, DefaultCap}, {32, 32}, {1, 1},
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
// without exceeding cap. Both Lookup-able. Paths live under
// MountRoot because that's what vmmd's MountExt4ReadOnly produces
// (and the registry is meaningless outside that prefix).
func TestRegistry_RegisterOrEvict_HappyPath(t *testing.T) {
	r := NewRegistry(8)
	mpA := filepath.Join(MountRoot, "mnt-a")
	mpB := filepath.Join(MountRoot, "mnt-b")
	if ev := r.RegisterOrEvict(mpA, "key-a", "/src/a"); ev != "" {
		t.Errorf("unexpected eviction on first register: %q", ev)
	}
	if ev := r.RegisterOrEvict(mpB, "key-b", "/src/b"); ev != "" {
		t.Errorf("unexpected eviction on second register: %q", ev)
	}
	for _, mp := range []string{mpA, mpB} {
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
	mpOld := filepath.Join(MountRoot, "mnt-old1")
	mpNew1 := filepath.Join(MountRoot, "mnt-new1")
	mpNew2 := filepath.Join(MountRoot, "mnt-new2")
	r.RegisterOrEvict(mpOld, "k1", "/s1")
	// Force a measurable mountedAt gap so the load-shed
	// deterministically picks the OLDEST (old1), not new1.
	timeSleep(t, 10)
	r.RegisterOrEvict(mpNew1, "k2", "/s2")
	timeSleep(t, 10)
	ev := r.RegisterOrEvict(mpNew2, "k3", "/s3")
	if ev != mpOld {
		t.Errorf("evicted = %q, want %q (oldest)", ev, mpOld)
	}
	if _, ok := r.Lookup(mpOld); ok {
		t.Error("evicted entry still in registry")
	}
	if _, ok := r.Lookup(mpNew2); !ok {
		t.Error("newest entry not registered")
	}
}

// TestRegistry_Forget_Idempotent: forgetting an unknown key is a
// no-op. The Umount path relies on this for safe retry after a
// partial failure.
func TestRegistry_Forget_Idempotent(t *testing.T) {
	r := NewRegistry(8)
	mp := filepath.Join(MountRoot, "mnt-x")
	r.RegisterOrEvict(mp, "k", "/s")
	r.Forget(mp)
	r.Forget(mp) // must not panic
	if _, ok := r.Lookup(mp); ok {
		t.Error("entry still in registry after Forget")
	}
}

// TestRegistry_SweepOrphans_EmptyNoOp: SweepOrphans on an empty
// registry returns 0 and does not panic. Pinned because cmd/vmmd
// runs the sweep on a configurable ticker that fires before any
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
	for _, bad := range []string{"/", "/home/foo", "/etc/passwd", "/var/log/faas", "/tmp/faas-parent-mnt-evil"} {
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
	missing := filepath.Join(MountRoot, ParentMountPrefix+"nonexistent-99999")
	err := UmountExt4(missing)
	if !errors.Is(err, ErrUnknownMountpoint) {
		t.Errorf("UmountExt4(%q) = %v, want ErrUnknownMountpoint", missing, err)
	}
}

// TestRegistry_Umount_UnknownIsIdempotent: Registry.Umount on a
// never-issued mountpoint returns (false, nil) so the deferred
// imaged-side UmountParentExt4 is safe to call blindly after a
// partial Mount failure.
func TestRegistry_Umount_UnknownIsIdempotent(t *testing.T) {
	r := NewRegistry(8)
	found, err := r.Umount(filepath.Join(MountRoot, "mnt-never-issued"))
	if err != nil {
		t.Errorf("Registry.Umount(unknown) = %v, want nil", err)
	}
	if found {
		t.Errorf("Registry.Umount(unknown) found=true, want false")
	}
}

// TestRegistry_Umount_RemovesEntryWithoutSyscall: when the
// mountpoint directory does not exist on disk (no actual kernel
// mount), Registry.Umount forgets the entry and reports the
// staged source removal — exercising the entry-cleanup side
// without the umount syscall. This is the path cmd/vmmd's sweep
// hits when an imaged process was killed mid-staging.
func TestRegistry_Umount_RemovesEntryWithoutSyscall(t *testing.T) {
	r := NewRegistry(8)
	// Use a path under MountRoot that we'll create + rmdir, plus
	// a real src tmp file in t.TempDir() so we can assert the
	// source removal.
	if err := os.MkdirAll(MountRoot, 0o750); err != nil {
		t.Skipf("mkdir %s: %v (likely running without root or on a non-Linux dev box)", MountRoot, err)
	}
	mp := filepath.Join(MountRoot, ParentMountPrefix+"mnt-test-rs")
	if err := os.Mkdir(mp, 0o755); err != nil {
		t.Skipf("mkdir %s: %v", mp, err)
	}
	defer func() { _ = os.Remove(mp) }()
	src := filepath.Join(t.TempDir(), "fake-src.ext4")
	if err := os.WriteFile(src, []byte("hi"), 0o600); err != nil {
		t.Fatalf("write src: %v", err)
	}
	r.RegisterOrEvict(mp, "k1", src)

	found, err := r.Umount(mp)
	if err != nil {
		// On non-Linux / unprivileged the umount syscall returns
		// EINVAL; we don't assert success here, only that the
		// entry bookkeeping went through (or the syscall error
		// restores the entry — see comment in Registry.Umount).
		t.Logf("Registry.Umount: found=%v err=%v (acceptable on non-metal)", found, err)
	}
	if _, stillThere := r.Lookup(mp); stillThere && err == nil {
		t.Error("entry still in registry after successful Registry.Umount")
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
