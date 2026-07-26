// seccomp_test.go — M8 §11 in-process pin for the SeccompStatus
// /proc parser. The cross-process fence is cmd/e2e/
// sec11_seccomp_e2e_test.go (metal); this file pins the kernel
// ABI the parser reads from so the wire is shrinkable from a
// procfs body without spinning up a vmmd.
//
// The kernel writes /proc/<pid>/status as human-readable text;
// parseSeccompLines walks line-by-line looking for `Seccomp:` and
// `Seccomp_filters:`. This is a stable kernel ABI; the tests pin
// the format the parser expects.

package vmmdgrpc

import (
	"strings"
	"testing"
)

// TestParseSeccompLines_AllKnownModes pins the kernel-ABI
// surface the handler reads from /proc/<pid>/status. The kernel
// writes the "Seccomp:" + "Seccomp_filters:" lines in this exact
// format (whitespace-separated, integer values); changing the
// format would break every /proc parsing toolchain upstream, but
// the test is here so a future refactor that, say, switches to
// strings.Split(line, "=") trips here.
func TestParseSeccompLines_AllKnownModes(t *testing.T) {
	cases := []struct {
		name string
		body string
		// Expected returns. filterLen is what the parser reports
		// when the kernel did NOT write Seccomp_filters (only
		// happens for Seccomp=0/1 — the kernel ABI is "line is
		// present iff Seccomp=2").
		wantMode   string
		wantFilter int
		wantErr    bool
	}{
		{
			name:       "filter+1BPF (jailer default)",
			body:       "Name:\tjailer\nUmask:\t0022\nSeccomp:\t2\nSeccomp_filters:\t1\n",
			wantMode:   "filter",
			wantFilter: 1,
		},
		{
			name:       "filter+empty (operator regression: filter mode but no BPF program)",
			body:       "Name:\tjailer\nSeccomp:\t2\nSeccomp_filters:\t0\n",
			wantMode:   "filter",
			wantFilter: 0,
		},
		{
			name:       "strict (no filter line per kernel ABI)",
			body:       "Name:\tjailer\nSeccomp:\t1\n",
			wantMode:   "strict",
			wantFilter: 0,
		},
		{
			name:       "disabled",
			body:       "Name:\tjailer\nSeccomp:\t0\n",
			wantMode:   "disabled",
			wantFilter: 0,
		},
		{
			name:    "missing Seccomp line (kernel too old)",
			body:    "Name:\tjailer\nUmask:\t0022\n",
			wantErr: true,
		},
		{
			name:    "garbage Seccomp value",
			body:    "Seccomp:\tabc\n",
			wantErr: true,
		},
		{
			name:    "unknown Seccomp value 7",
			body:    "Seccomp:\t7\n",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mode, filter, err := ParseSeccompLines(strings.NewReader(tc.body))
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error, got mode=%q filter=%d", mode, filter)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if mode != tc.wantMode {
				t.Errorf("mode = %q, want %q", mode, tc.wantMode)
			}
			if filter != int32(tc.wantFilter) {
				t.Errorf("filter = %d, want %d", filter, tc.wantFilter)
			}
		})
	}
}

// TestParseSeccompLines_RealSelfProcess — REMOVED.
//
// The cross-process e2e (cmd/e2e/sec11_seccomp_e2e_test.go, metal)
// is the authoritative gate for the /proc-based readback. An
// in-process self-test was sketched here to expose the parser
// to a real kernel ABI without spinning up a vmmd, but the
// fixture tests above already pin the same shape from canned
// /proc bodies, and the self-test adds noise (it always skips
// because the test process is mode=disabled).
