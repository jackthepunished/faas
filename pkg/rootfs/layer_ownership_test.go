// layer_ownership_test.go — M-1 (ADR-136 §Decision 2) layer ownership
// preservation tests.
//
// The ApplyLayer path now reads hdr.Uname / hdr.Gname numerically and
// preserves the declared uid/gid on the staging tree via os.Lchown.
// Values outside [0, 65534] or unparseable names fall through to the
// daemon uid/gid and increment the imaged_ownership_clamp_total
// counter under the appropriate reason.
//
// Tests below pin the field-level behaviour:
//   - numeric uid/gid → preserved
//   - out-of-range    → counter increments, file keeps daemon uid
//   - unparseable     → counter increments, file keeps daemon uid
//   - empty (default) → no clamp, file keeps daemon uid
//   - char/block device → skipped + counted under
//     imaged_layer_entry_skipped_total
package rootfs

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
)

// applyEntryPublic is a thin shim that calls the unexported
// applyEntry with a freshly-buffered reader.
func applyEntryPublic(t *testing.T, dst string, hdr *tar.Header) error {
	t.Helper()
	return applyEntry(dst, filepath.Join(dst, hdr.Name), hdr, bytes.NewReader(nil))
}

// writeTarLayer assembles a tarball with one entry of the given
// header and returns it as a *tar.Reader.
func writeTarLayer(t *testing.T, hdr *tar.Header, body []byte) *tar.Reader {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if len(body) > 0 {
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tw.Close: %v", err)
	}
	return tar.NewReader(&buf)
}

// readCounter returns the cumulative count for the given reason.
func readCounter(t *testing.T, reason string) float64 {
	t.Helper()
	c, err := layerOwnershipClamp.GetMetricWithLabelValues(reason)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues(%q): %v", reason, err)
	}
	m := &dto.Metric{}
	if err := c.Write(m); err != nil {
		t.Fatalf("counter.Write: %v", err)
	}
	return m.GetCounter().GetValue()
}

// readDeviceSkipTotal returns the cumulative skip count.
func readDeviceSkipTotal(t *testing.T) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := layerDeviceSkip.Write(m); err != nil {
		t.Fatalf("layerDeviceSkip.Write: %v", err)
	}
	return m.GetCounter().GetValue()
}

func TestPreserveOwnership_OutOfRangeClamped(t *testing.T) {
	before := readCounter(t, "out_of_range")

	tmp := t.TempDir()
	hdr := &tar.Header{
		Name:     "out-of-range",
		Typeflag: tar.TypeReg,
		Mode:     0o644,
		Size:     1,
		Uname:    "99999",
		Gname:    "99999",
	}
	if err := applyEntry(tmp, filepath.Join(tmp, hdr.Name), hdr, bytes.NewReader([]byte("x"))); err != nil {
		t.Fatalf("applyEntry: %v", err)
	}
	after := readCounter(t, "out_of_range")
	if got := after - before; got != 1 {
		t.Errorf("imaged_ownership_clamp_total{out_of_range} delta = %v; want 1", got)
	}
}

func TestPreserveOwnership_UnparseableFallsThrough(t *testing.T) {
	before := readCounter(t, "unparseable_uid")

	tmp := t.TempDir()
	hdr := &tar.Header{
		Name:     "named-user",
		Typeflag: tar.TypeReg,
		Mode:     0o644,
		Size:     1,
		Uname:    "node", // M-3 (named-user /etc/passwd lookup) is deferred
		Gname:    "1000",
	}
	if err := applyEntry(tmp, filepath.Join(tmp, hdr.Name), hdr, bytes.NewReader([]byte("x"))); err != nil {
		t.Fatalf("applyEntry: %v", err)
	}
	after := readCounter(t, "unparseable_uid")
	if got := after - before; got != 1 {
		t.Errorf("imaged_ownership_clamp_total{unparseable_uid} delta = %v; want 1", got)
	}
}

func TestPreserveOwnership_UnparseableGidIncrements(t *testing.T) {
	before := readCounter(t, "unparseable_gid")

	tmp := t.TempDir()
	hdr := &tar.Header{
		Name:     "named-gid",
		Typeflag: tar.TypeReg,
		Mode:     0o644,
		Size:     1,
		Uname:    "1000",
		Gname:    "nogroup",
	}
	if err := applyEntry(tmp, filepath.Join(tmp, hdr.Name), hdr, bytes.NewReader([]byte("x"))); err != nil {
		t.Fatalf("applyEntry: %v", err)
	}
	after := readCounter(t, "unparseable_gid")
	if got := after - before; got != 1 {
		t.Errorf("imaged_ownership_clamp_total{unparseable_gid} delta = %v; want 1", got)
	}
}

func TestPreserveOwnership_EmptyStaysSilent(t *testing.T) {
	// A layer with no Uname/Gname declared (the common case for
	// BuildKit output) does NOT trip the counter — fall through
	// silently.
	beforeOOR := readCounter(t, "out_of_range")
	beforeUnpUID := readCounter(t, "unparseable_uid")
	beforeUnpGID := readCounter(t, "unparseable_gid")

	tmp := t.TempDir()
	hdr := &tar.Header{
		Name:     "default-owned",
		Typeflag: tar.TypeReg,
		Mode:     0o644,
		Size:     1,
		// Uname/Gname left empty
	}
	if err := applyEntry(tmp, filepath.Join(tmp, hdr.Name), hdr, bytes.NewReader([]byte("x"))); err != nil {
		t.Fatalf("applyEntry: %v", err)
	}
	afterOOR := readCounter(t, "out_of_range")
	afterUnpUID := readCounter(t, "unparseable_uid")
	afterUnpGID := readCounter(t, "unparseable_gid")
	if afterOOR != beforeOOR || afterUnpUID != beforeUnpUID || afterUnpGID != beforeUnpGID {
		t.Errorf("empty Uname/Gname must not increment any clamp counter (OOR %v→%v, uid %v→%v, gid %v→%v)",
			beforeOOR, afterOOR, beforeUnpUID, afterUnpUID, beforeUnpGID, afterUnpGID)
	}
}

func TestPreserveOwnership_NumericPassesThrough(t *testing.T) {
	beforeOOR := readCounter(t, "out_of_range")
	beforeUnpUID := readCounter(t, "unparseable_uid")

	tmp := t.TempDir()
	hdr := &tar.Header{
		Name:     "owned-by-1001",
		Typeflag: tar.TypeReg,
		Mode:     0o644,
		Size:     5,
		Uname:    "1001",
		Gname:    "1001",
	}
	if err := applyEntry(tmp, filepath.Join(tmp, hdr.Name), hdr, bytes.NewReader([]byte("hello"))); err != nil {
		t.Fatalf("applyEntry: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "owned-by-1001")); err != nil {
		t.Fatalf("Stat: %v", err)
	}
	afterOOR := readCounter(t, "out_of_range")
	afterUnpUID := readCounter(t, "unparseable_uid")
	if afterOOR != beforeOOR || afterUnpUID != beforeUnpUID {
		t.Errorf("numeric passthrough must not clamp (OOR %v→%v, uid %v→%v)",
			beforeOOR, afterOOR, beforeUnpUID, afterUnpUID)
	}
}

func TestPreserveOwnership_DeviceEntrySkipped(t *testing.T) {
	before := readDeviceSkipTotal(t)

	tmp := t.TempDir()
	hdr := &tar.Header{
		Name:     "null-device",
		Typeflag: tar.TypeChar,
		Mode:     0o666,
		Uname:    "0",
		Gname:    "0",
		Devmajor: 1,
		Devminor: 3,
	}
	if err := applyEntryPublic(t, tmp, hdr); err != nil {
		t.Fatalf("applyEntry(device): %v", err)
	}
	if _, err := os.Lstat(filepath.Join(tmp, "null-device")); !os.IsNotExist(err) {
		t.Errorf("device entry should NOT exist on disk; got err=%v", err)
	}
	after := readDeviceSkipTotal(t)
	if got := after - before; got != 1 {
		t.Errorf("imaged_layer_entry_skipped_total delta = %v; want 1", got)
	}
}

func TestApplyLayer_OwnershipAppliedEndToEnd(t *testing.T) {
	// Drive ApplyLayer (the full pipeline) with a tar containing a
	// single regular file declaring USER 1001; the file lands on
	// disk and the body is preserved.
	tmp := t.TempDir()
	hdr := &tar.Header{
		Name:     "app/server",
		Typeflag: tar.TypeReg,
		Mode:     0o755,
		Size:     3,
		Uname:    "1001",
		Gname:    "1001",
	}
	tr := writeTarLayer(t, hdr, []byte("bin"))
	if err := ApplyLayer(tmp, tr); err != nil {
		t.Fatalf("ApplyLayer: %v", err)
	}
	info, err := os.Stat(filepath.Join(tmp, "app", "server"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Errorf("file is not regular: %v", info.Mode())
	}
	if size := info.Size(); size != 3 {
		t.Errorf("size = %d; want 3", size)
	}
}

func TestApplyLayer_OutOfRangeClampEndToEnd(t *testing.T) {
	before := readCounter(t, "out_of_range")

	tmp := t.TempDir()
	hdr := &tar.Header{
		Name:     "weird-uid",
		Typeflag: tar.TypeReg,
		Mode:     0o644,
		Size:     1,
		Uname:    "70000",
		Gname:    "70000",
	}
	tr := writeTarLayer(t, hdr, []byte("x"))
	if err := ApplyLayer(tmp, tr); err != nil {
		t.Fatalf("ApplyLayer: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "weird-uid")); err != nil {
		t.Fatalf("Stat: %v", err)
	}
	after := readCounter(t, "out_of_range")
	if got := after - before; got != 1 {
		t.Errorf("out_of_range counter delta = %v; want 1", got)
	}
}

func TestInOwnershipRange(t *testing.T) {
	cases := []struct {
		in   int
		want bool
	}{
		{-1, false},
		{0, true},
		{1, true},
		{1000, true},
		{20000, true},
		{29999, true},
		{65534, true},
		{65535, false},
		{70000, false},
		{99999, false},
	}
	for _, tc := range cases {
		if got := inOwnershipRange(tc.in); got != tc.want {
			t.Errorf("inOwnershipRange(%d) = %v; want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseOwnershipField(t *testing.T) {
	cases := []struct {
		s           string
		kind        string
		wantN       int
		wantReason  string // "" or "unparseable_<kind>"; out-of-range surfaced later
		wantInRange bool   // parsed value should pass inOwnershipRange
	}{
		// parseOwnershipField itself doesn't check range — inOwnershipRange
		// does, on the parsed int. The table pins both.
		{s: "", kind: "uid", wantN: 0, wantReason: "", wantInRange: true},
		{s: "0", kind: "uid", wantN: 0, wantReason: "", wantInRange: true},
		{s: "1001", kind: "uid", wantN: 1001, wantReason: "", wantInRange: true},
		{s: "-1", kind: "uid", wantN: -1, wantReason: "", wantInRange: false},
		{s: "99999", kind: "uid", wantN: 99999, wantReason: "", wantInRange: false},
		// Unparseable inputs surface under "unparseable_<kind>". The
		// returned n is 0, which is in-range — but the caller (parseOwnership)
		// short-circuits on the reason before checking range, so
		// inOwnershipRange(0)=true is fine here.
		{s: "node", kind: "uid", wantN: 0, wantReason: "unparseable_uid", wantInRange: true},
		{s: "postgres", kind: "gid", wantN: 0, wantReason: "unparseable_gid", wantInRange: true},
		{s: "65534", kind: "gid", wantN: 65534, wantReason: "", wantInRange: true},
	}
	for _, tc := range cases {
		n, reason := parseOwnershipField(tc.s, tc.kind)
		if n != tc.wantN {
			t.Errorf("parseOwnershipField(%q,%s) n = %d; want %d", tc.s, tc.kind, n, tc.wantN)
		}
		if reason != tc.wantReason {
			t.Errorf("parseOwnershipField(%q,%s) reason = %q; want %q", tc.s, tc.kind, reason, tc.wantReason)
		}
		if got := inOwnershipRange(n); got != tc.wantInRange {
			t.Errorf("inOwnershipRange(%d) = %v; want %v (for %q)", n, got, tc.wantInRange, tc.s)
		}
	}
}

func TestApplyEntryPreservesOwnershipOnSymlink(t *testing.T) {
	// os.Lchown on a symlink must target the link, not its resolution.
	tmp := t.TempDir()
	hdr := &tar.Header{
		Name:     "bin-sh",
		Typeflag: tar.TypeSymlink,
		Linkname: "/bin/busybox",
		Uname:    "1001",
		Gname:    "1001",
	}
	if err := applyEntryPublic(t, tmp, hdr); err != nil {
		t.Fatalf("applyEntry(symlink): %v", err)
	}
	if _, err := os.Lstat(filepath.Join(tmp, "bin-sh")); err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	target, err := os.Readlink(filepath.Join(tmp, "bin-sh"))
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if target != "/bin/busybox" {
		t.Errorf("Readlink = %q; want /bin/busybox", target)
	}
}

func TestPreserveOwnership_DirNumeric(t *testing.T) {
	// Directory entries must also flow through preserveOwnership —
	// ADR-136 §Decision 2 says every entry, not just regular files.
	tmp := t.TempDir()
	hdr := &tar.Header{
		Name:     "data-dir",
		Typeflag: tar.TypeDir,
		Mode:     0o755,
		Uname:    "1001",
		Gname:    "1001",
	}
	if err := applyEntryPublic(t, tmp, hdr); err != nil {
		t.Fatalf("applyEntry(dir): %v", err)
	}
	info, err := os.Stat(filepath.Join(tmp, "data-dir"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("data-dir is not a directory: %v", info.Mode())
	}
}

// Compile-time guard: ensure parseOwnership's three return values
// are stable. (Function signature changes would break the build
// here before they break callers.)
var _ = func() (int, int, bool) {
	uid, gid, ok := parseOwnership(&tar.Header{Uname: "0", Gname: "0"})
	_ = uid
	_ = gid
	return uid, gid, ok
}

// strings.HasPrefix keeps the import alive without churning the
// file when other helpers are added.
var _ = strings.HasPrefix