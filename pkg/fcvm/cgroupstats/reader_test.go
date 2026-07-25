package cgroupstats

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/onebox-faas/faas/pkg/fcvm"
)

// withFakeRoot mirrors pkg/fcvm/cgroup_test.go's withFakeCgroupRoot
// helper. We do NOT substitute pkg/fcvm.cgroupRoot here because the
// package only consults that var for writes (memory.max); we read
// from r.root, which is a Reader-local field the test owns outright.
func withFakeRoot(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// fakeScope creates a per-instance cgroup v2 leaf under
// <root>/faas-tenant.slice/<instance>/ with the provided cpu.stat and
// memory.current bodies. Writes are byte-exact so callers can test
// malformed input.
func fakeScope(t *testing.T, root, instance, cpuStat, memoryCurrent string) {
	t.Helper()
	dir := filepath.Join(root, fcvm.ParentCgroup, fcvm.PerInstanceScope(instance))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if cpuStat != "" {
		if err := os.WriteFile(filepath.Join(dir, "cpu.stat"), []byte(cpuStat), 0o644); err != nil {
			t.Fatalf("write cpu.stat: %v", err)
		}
	}
	if memoryCurrent != "" {
		if err := os.WriteFile(filepath.Join(dir, "memory.current"), []byte(memoryCurrent), 0o644); err != nil {
			t.Fatalf("write memory.current: %v", err)
		}
	}
}

func TestSampleReadsCPUAndRSS(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cgroup v2 is Linux-only")
	}
	root := withFakeRoot(t)
	fakeScope(t, root, "vm-abc",
		"usage_usec 1234567890\nuser_usec 100\nsystem_usec 50\n",
		"134217728\n",
	)
	r := New(root, nil)
	got, ok := r.Sample("vm-abc")
	if !ok {
		t.Fatal("Sample: expected ok=true")
	}
	if got.CPUUsageUsec != 1234567890 {
		t.Errorf("CPUUsageUsec = %d, want 1234567890", got.CPUUsageUsec)
	}
	if got.RSSBytes != 134217728 {
		t.Errorf("RSSBytes = %d, want 134217728", got.RSSBytes)
	}
}

func TestSampleReturnsFalseOnMissingScope(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cgroup v2 is Linux-only")
	}
	root := withFakeRoot(t)
	// Note: no fakeScope call — directory does not exist.
	r := New(root, nil)
	_, ok := r.Sample("ghost")
	if ok {
		t.Error("Sample on missing scope: expected ok=false")
	}
}

func TestSampleReturnsFalseOnMalformedCpuStat(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cgroup v2 is Linux-only")
	}
	root := withFakeRoot(t)
	fakeScope(t, root, "vm-bad",
		"this is not a cgroup file\n",
		"42\n",
	)
	r := New(root, nil)
	_, ok := r.Sample("vm-bad")
	if ok {
		t.Error("Sample on malformed cpu.stat: expected ok=false")
	}
}

func TestSampleReturnsFalseOnMissingUsageUsecField(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cgroup v2 is Linux-only")
	}
	root := withFakeRoot(t)
	// cpu.stat present but the only key is user_usec — must not be
	// mistaken for usage_usec.
	fakeScope(t, root, "vm-no-usage",
		"user_usec 100\nsystem_usec 50\n",
		"42\n",
	)
	r := New(root, nil)
	_, ok := r.Sample("vm-no-usage")
	if ok {
		t.Error("Sample on cpu.stat without usage_usec: expected ok=false")
	}
}

func TestSampleReturnsFalseOnMalformedMemoryCurrent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cgroup v2 is Linux-only")
	}
	root := withFakeRoot(t)
	fakeScope(t, root, "vm-bad-mem",
		"usage_usec 100\n",
		"not a number\n",
	)
	r := New(root, nil)
	_, ok := r.Sample("vm-bad-mem")
	if ok {
		t.Error("Sample on malformed memory.current: expected ok=false")
	}
}

func TestSampleToleratesTrailingWhitespaceInMemoryCurrent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cgroup v2 is Linux-only")
	}
	root := withFakeRoot(t)
	// cgroup v2 memory.current normally has no trailing newline; some
	// kernels add whitespace. TrimSpace must handle it.
	fakeScope(t, root, "vm-ws",
		"usage_usec 7\n",
		"4096  \n",
	)
	r := New(root, nil)
	got, ok := r.Sample("vm-ws")
	if !ok {
		t.Fatal("Sample: expected ok=true")
	}
	if got.RSSBytes != 4096 {
		t.Errorf("RSSBytes = %d, want 4096", got.RSSBytes)
	}
}

func TestInstancesFiltersSystemdAndKernelSiblings(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cgroup v2 is Linux-only")
	}
	root := withFakeRoot(t)
	// The real per-VM scopes are bare instance ids (no '.' — jailer
	// rejects '.' in --id). The siblings below are what systemd /
	// kernel install in the same slice.
	for _, leaf := range []string{
		"vm-alpha",  // ours
		"vm-bravo",  // ours
		"init.scope",  // kernel
		"user.slice",  // systemd
		"system.slice", // systemd
		"foo.mount",  // systemd
	} {
		if err := os.MkdirAll(filepath.Join(root, fcvm.ParentCgroup, leaf), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", leaf, err)
		}
	}
	r := New(root, nil)
	got, err := r.Instances()
	if err != nil {
		t.Fatalf("Instances: %v", err)
	}
	sort.Strings(got)
	want := []string{"vm-alpha", "vm-bravo"}
	if !equalStrings(got, want) {
		t.Errorf("Instances = %v, want %v", got, want)
	}
}

func TestInstancesReturnsSortedDeterministically(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cgroup v2 is Linux-only")
	}
	root := withFakeRoot(t)
	// Insert in non-alphabetical order on purpose.
	for _, leaf := range []string{"zzz", "mmm", "aaa"} {
		if err := os.MkdirAll(filepath.Join(root, fcvm.ParentCgroup, leaf), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", leaf, err)
		}
	}
	r := New(root, nil)
	got, err := r.Instances()
	if err != nil {
		t.Fatalf("Instances: %v", err)
	}
	want := []string{"aaa", "mmm", "zzz"}
	if !equalStrings(got, want) {
		t.Errorf("Instances = %v, want %v (deterministic order)", got, want)
	}
}

func TestInstancesMissingSliceReturnsEmpty(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cgroup v2 is Linux-only")
	}
	root := withFakeRoot(t)
	// No faas-tenant.slice directory at all.
	r := New(root, nil)
	got, err := r.Instances()
	if err != nil {
		t.Fatalf("Instances on missing slice: unexpected error %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Instances on missing slice = %v, want empty", got)
	}
}

func TestNewWithDefaultsUsesSysFsCgroup(t *testing.T) {
	r := NewWithDefaults()
	if r.root != defaultRoot {
		t.Errorf("NewWithDefaults root = %q, want %q", r.root, defaultRoot)
	}
	if r.now == nil {
		t.Error("NewWithDefaults: now must default to time.Now, got nil")
	}
}

func TestNewWithNilNowUsesTimeNow(t *testing.T) {
	r := New("/tmp", nil)
	if r.now == nil {
		t.Fatal("New with nil now: now must default to time.Now, got nil")
	}
}

// equalStrings compares two string slices element-wise. Lives here so
// the package's tests don't pull in reflect.DeepEqual for trivial
// checks.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
