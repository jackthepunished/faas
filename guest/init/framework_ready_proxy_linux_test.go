//go:build linux

// Regression test for parseProxyLine (issue #470 / PR #543
// CodeQL go/integer-overflow alert). The alert flagged the
// historical `uint32(int64)` narrowing in handleFrameworkReadyConn;
// the fix bounds the parse at math.MaxUint32 via
// strconv.ParseUint(..., 10, 32). This test pins:
//
//   - the happy path (runtime only / runtime + warmup)
//   - the negative-input rejection (would otherwise wrap to a
//     huge uint32 and corrupt the wire)
//   - the math.MaxUint32-bound rejection (string of a value
//     larger than the 4-byte BE wire can hold)
//   - format errors (empty, too many fields)
//
// This is a "small picture" table so future wire extensions
// (e.g. a third field) get a single-line diff.
package main

import (
	"math"
	"strings"
	"testing"
)

func TestParseProxyLine(t *testing.T) {
	type want struct {
		runtime  string
		warmupMs uint64
		err      bool
		errSub   string
	}
	cases := []struct {
		name string
		line string
		want want
	}{
		{
			name: "runtime_only",
			line: "node22\n",
			want: want{runtime: "node22", warmupMs: 0},
		},
		{
			name: "runtime_plus_warmup_typical",
			line: "node22 350\n",
			want: want{runtime: "node22", warmupMs: 350},
		},
		{
			name: "runtime_plus_warmup_max_uint32",
			line: "node22 " + uintToString(math.MaxUint32) + "\n",
			want: want{runtime: "node22", warmupMs: math.MaxUint32},
		},
		{
			name: "negative_warmup_rejected",
			line: "node22 -1\n",
			want: want{err: true, errSub: "parse warmup_ms"},
		},
		{
			name: "overflow_warmup_rejected",
			line: "node22 " + uintToString(uint64(math.MaxUint32)+1) + "\n",
			want: want{err: true, errSub: "parse warmup_ms"},
		},
		{
			name: "way_overflow_rejected",
			line: "node22 99999999999999999999\n",
			want: want{err: true, errSub: "parse warmup_ms"},
		},
		{
			name: "non_numeric_warmup_rejected",
			line: "node22 abc\n",
			want: want{err: true, errSub: "parse warmup_ms"},
		},
		{
			name: "empty_line_rejected",
			line: "\n",
			want: want{err: true, errSub: "format"},
		},
		{
			name: "too_many_fields_rejected",
			line: "node22 350 extra\n",
			want: want{err: true, errSub: "format"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotRuntime, gotWarmupMs, err := parseProxyLine(tc.line)
			if tc.want.err {
				if err == nil {
					t.Fatalf("parseProxyLine(%q) = nil err, want %q", tc.line, tc.want.errSub)
				}
				if tc.want.errSub != "" && !strings.Contains(err.Error(), tc.want.errSub) {
					t.Errorf("err = %q, want substring %q", err.Error(), tc.want.errSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseProxyLine(%q) unexpected err: %v", tc.line, err)
			}
			if gotRuntime != tc.want.runtime {
				t.Errorf("runtime = %q, want %q", gotRuntime, tc.want.runtime)
			}
			if gotWarmupMs != tc.want.warmupMs {
				t.Errorf("warmupMs = %d, want %d", gotWarmupMs, tc.want.warmupMs)
			}
		})
	}
}

// uintToString avoids importing strconv in the test fixture
// (the production code's strconv usage is the surface under
// test). Pure fmt formatting — no parsing, no allocation.
func uintToString(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
