package cgroupstats

import (
	"bufio"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/onebox-faas/faas/pkg/fcvm"
)

// defaultRoot is the cgroup v2 unified mount; production callers use
// New(root, now) explicitly, but NewWithDefaults exists for vmmd
// wiring which has no need to substitute a fake root in tests — its
// tests inject via the fcvm cgroupRoot var instead.
const defaultRoot = "/sys/fs/cgroup"

// cpuStatField is the cgroup v2 cpu.stat key that represents the
// cumulative CPU time consumed by this scope, in microseconds. The
// poller does the delta math against its previous cumulative, so this
// package intentionally returns the raw counter — the rate belongs to
// the consumer. Other cpu.stat fields (user_usec, system_usec,
// nr_periods, nr_throttled, …) are not surfaced because schedd only
// needs the total for instance-level CPU% rollups.
const cpuStatField = "usage_usec"

// Sample is the cumulative CPU and current memory charge for one
// instance, read once from cgroup v2. CPUPct and rate are computed by
// the poller across two Samples; the package does not compute a rate
// to keep the wire-stable contract simple (a future per-cgroup split
// into user/system would change the field, not the API).
type Sample struct {
	// CPUUsageUsec is the cumulative host CPU time consumed by this
	// cgroup scope since instantiation, in microseconds. Reads
	// monotonically increase across the lifetime of the scope; on
	// regression (cgroup recreated under us) the poller detects and
	// resets its baseline.
	CPUUsageUsec uint64

	// RSSBytes is the current cgroup memory charge in bytes, read
	// from memory.current. Includes the Firecracker process and VM
	// pages charged to the scope. On non-Linux or missing files the
	// Reader returns ok=false and Sample is the zero value — the
	// caller MUST NOT default to zero resident bytes (that would
	// under-report capacity and silently look like free RAM).
	RSSBytes int64
}

// Reader reads per-instance cgroup v2 counters. Construct with
// New(root, now) when the caller needs to inject a fake root for
// tests, or with NewWithDefaults() in production vmmd wiring.
//
// The Reader is safe for concurrent use; it holds no per-instance
// state. Pollers call Sample once per Tick; the package makes no
// attempt to dedupe — if two schedd pollers targeted the same node,
// they would each get their own consistent samples.
type Reader struct {
	root string
	now  func() time.Time //nolint:unused // reserved for future freshness windows
}

// New returns a Reader that reads from the given cgroup v2 root. Pass
// "/" for the production mount; tests pass t.TempDir() via this
// argument. now is reserved for a future staleness window and is not
// yet consulted; it is plumbed today so adding it later does not
// require a signature change at every call site.
//
// Pass nil for now to use time.Now.
func New(root string, now func() time.Time) *Reader {
	if now == nil {
		now = time.Now
	}
	return &Reader{root: root, now: now}
}

// NewWithDefaults returns a Reader pointed at the production
// /sys/fs/cgroup root. Use this from cmd/vmmd wiring; tests use New.
func NewWithDefaults() *Reader { return New(defaultRoot, nil) }

// Sample reads cpu.stat and memory.current for one instance's cgroup
// scope. Returns ok=false on:
//
//   - non-Linux hosts (runtime.GOOS != "linux") — cgroup v2 is Linux-only,
//   - missing scope directory (jailer has not yet joined, or already
//     torn down),
//   - malformed cpu.stat or memory.current (partial file during
//     destroy; kernel can briefly leave a stale leaf).
//
// On ok=false the Sample is the zero value — callers MUST treat that
// as "no data", not as a real zero reading. The schedd poller uses
// ok=false to mark the row Unknown in InstanceStat.
//
// The function does not log on the not-found / malformed path: those
// are normal during the wake/destroy lifecycle and the poller
// explicitly prefers partial snapshots to error spam.
func (r *Reader) Sample(instance string) (Sample, bool) {
	if runtime.GOOS != "linux" {
		return Sample{}, false
	}
	scope := filepath.Join(r.root, fcvm.ParentCgroup, fcvm.PerInstanceScope(instance))
	if _, err := os.Stat(scope); err != nil {
		return Sample{}, false
	}
	cpu, cpuOK := readCPUUsageUsec(filepath.Join(scope, "cpu.stat"))
	if !cpuOK {
		// CPU is the more sensitive signal — if we can't read it,
		// return ok=false so the poller doesn't think it has a stale
		// counter paired with a fresh memory.current.
		return Sample{}, false
	}
	rss, rssOK := readMemoryCurrent(filepath.Join(scope, "memory.current"))
	if !rssOK {
		return Sample{}, false
	}
	return Sample{CPUUsageUsec: cpu, RSSBytes: rss}, true
}

// Instances enumerates the per-VM cgroup leaves under faas-tenant.slice.
// Mirrors pkg/fcvm/leakcheck.listTenantScopes' filter — no '.', no
// '..' — so systemd-installed siblings (init.scope, user.slice,
// *.mount) are excluded. Returns the bare instance names (matching
// Lease.Instance verbatim).
//
// The slice is sorted lexicographically by instance id so callers get
// deterministic ordering across calls. This matters for the poller:
// its CPU-baseline map is keyed by instance id, and a stable order
// makes the per-tick dial loop easier to reason about in logs.
func (r *Reader) Instances() ([]string, error) {
	if runtime.GOOS != "linux" {
		return nil, nil
	}
	base := filepath.Join(r.root, fcvm.ParentCgroup)
	entries, err := os.ReadDir(base)
	if err != nil {
		// Missing slice (cold-boot race, transient teardown) is
		// not an error — the poller renders an empty snapshot
		// and the wire rollup collapses. Other errors propagate
		// so the caller can log them.
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.Contains(name, ".") || strings.Contains(name, "..") {
			continue
		}
		out = append(out, name)
	}
	// Deterministic order — sort lexicographically.
	sortStrings(out)
	return out, nil
}

// readCPUUsageUsec parses one cgroup v2 cpu.stat file. The file is a
// newline-separated key-value list:
//
//	usage_usec 1234567890
//	user_usec 987654321
//	system_usec 246913569
//	...
//
// Only usage_usec is consumed (see cpuStatField rationale above).
// Malformed files (missing key, non-numeric value, no newline) return
// ok=false rather than panicking — cgroup leaves can briefly hold
// stale content during destroy.
//
// Path is vetted by the caller: it lives under
// /sys/fs/cgroup/<slice>/<instance>/, where <instance> is the
// jailer's per-VM directory name. The instance id is not
// customer-supplied (it is the cgroup directory the jailer created
// at VM boot and tore down at VM destroy), so bare os.Open is
// safe — the symlink/non-regular guard that openCustomerFile
// enforces is irrelevant on the host's cgroup v2 mount. The
// errcheck ignore below pairs with the doc: we cannot meaningfully
// act on a Close error from a /sys read.
func readCPUUsageUsec(path string) (uint64, bool) {
	f, err := os.Open(path) //nolint:forbidigo // vetted cgroup path, see comment above
	if err != nil {
		return 0, false
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	// cpu.stat lines are short; the default 64 KiB buffer is plenty.
	for sc.Scan() {
		line := sc.Text()
		key, val, ok := strings.Cut(line, " ")
		if !ok || key != cpuStatField {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(val), 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// readMemoryCurrent reads cgroup v2 memory.current — a single integer
// in bytes, no newline required (but tolerated).
func readMemoryCurrent(path string) (int64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// sortStrings is a tiny inlined sort.Strings wrapper to keep this
// file's imports list narrow. Hot path: only the instance names found
// in this tick — typically tens of entries, not millions.
func sortStrings(s []string) {
	// Insertion sort: small N, no allocation, predictable.
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
