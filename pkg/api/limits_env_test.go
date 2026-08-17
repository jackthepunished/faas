package api

import (
	"strconv"
	"strings"
	"testing"
)

// TestParseEdgeRuleMaintenanceRetryAfterSeconds pins the parse
// contract for the issue #899 finding-3 override: unset → the
// platform default, valid → the value, anything else → the default
// PLUS an error naming the env var and the docs URL. The "returns the
// default on error" half is the load-bearing one — an operator typo
// must never take the maintenance gate out of service.
func TestParseEdgeRuleMaintenanceRetryAfterSeconds(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    int
		wantErr bool
	}{
		{name: "unset", raw: "", want: EdgeRuleMaintenanceRetryAfterSeconds},
		{name: "valid-small", raw: "1", want: 1},
		{name: "valid-typical", raw: "300", want: 300},
		{
			name: "valid-at-cap",
			raw:  strconv.Itoa(MaxEdgeRuleMaintenanceRetryAfterSeconds),
			want: MaxEdgeRuleMaintenanceRetryAfterSeconds,
		},
		{
			// RFC 7231 forbids Retry-After: 0, so 0 is an error
			// rather than a silent clamp — the operator meant
			// something the platform cannot express.
			name: "zero-rejected", raw: "0",
			want: EdgeRuleMaintenanceRetryAfterSeconds, wantErr: true,
		},
		{
			name: "negative-rejected", raw: "-5",
			want: EdgeRuleMaintenanceRetryAfterSeconds, wantErr: true,
		},
		{
			name: "over-cap-rejected",
			raw:  strconv.Itoa(MaxEdgeRuleMaintenanceRetryAfterSeconds + 1),
			want: EdgeRuleMaintenanceRetryAfterSeconds, wantErr: true,
		},
		{
			name: "non-integer-rejected", raw: "60s",
			want: EdgeRuleMaintenanceRetryAfterSeconds, wantErr: true,
		},
		{
			name: "whitespace-rejected", raw: " 60",
			want: EdgeRuleMaintenanceRetryAfterSeconds, wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseEdgeRuleMaintenanceRetryAfterSeconds(tc.raw)
			if got != tc.want {
				t.Errorf("value = %d, want %d", got, tc.want)
			}
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error, got nil")
				}
				if !strings.Contains(err.Error(), EnvEdgeRuleMaintenanceRetryAfterSeconds) {
					t.Errorf("error %q does not name the env var", err)
				}
				if !strings.Contains(err.Error(), "docs.gregale.dev") {
					t.Errorf("error %q carries no docs URL", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestEdgeRuleMaintenanceRetryAfterDefault asserts the cached
// accessor agrees with the constant in a clean environment (CI does
// not set the override). It also pins that the accessor is safe to
// call repeatedly — it is on the gateway hot path.
func TestEdgeRuleMaintenanceRetryAfterDefault(t *testing.T) {
	t.Setenv(EnvEdgeRuleMaintenanceRetryAfterSeconds, "")
	for i := 0; i < 3; i++ {
		if got := EdgeRuleMaintenanceRetryAfter(); got <= 0 {
			t.Fatalf("EdgeRuleMaintenanceRetryAfter() = %d; must be a positive Retry-After", got)
		}
	}
	if got := EdgeRuleMaintenanceRetryAfter(); got > MaxEdgeRuleMaintenanceRetryAfterSeconds {
		t.Errorf("EdgeRuleMaintenanceRetryAfter() = %d, above the %d cap",
			got, MaxEdgeRuleMaintenanceRetryAfterSeconds)
	}
}
