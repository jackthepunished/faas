// pure_helpers_mega3_test.go — Coverage Mega-PR #3 cluster F.1:
// fill pkg/oci coverage on small pure / constant-table helpers.
//
// Targets (baseline 75.2% on the package at branch time):
//   - authChallenge.String (0%): "realm|service|scope" cache key.
//   - OCIOnlyDenyCIDRsV4 (0%): defensive copy of typed DenyEntry.
//   - OCIOnlyDenyCounterLabels (0%): (CounterName, Family) tuples.
//   - EgressAllowLoopbackFromEnv (0%): exact-match env var check.
//   - short (75%): sha256 digest → first 12 hex chars.
//   - DefaultPuller 6 methods (0%): pure no-op wrappers.
//
// Family constants duplicated locally — pkg/oci_test cannot import
// pkg/netns (an import cycle: pkg/oci already imports pkg/netns via
// egress.go). pkg/netns.Family is `type Family int` with FamilyV4=0,
// FamilyV6=1; we use literal values.
//
// Whitebox `package oci`.

package oci

import (
	"context"
	"os"
	"testing"
)

const (
	familyVMega3V4 = 0 // matches netns.FamilyV4
	familyVMega3V6 = 1 // matches netns.FamilyV6
)

func TestAuthChallengeString_Mega3(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   authChallenge
		want string
	}{
		{
			name: "all three fields",
			in:   authChallenge{realm: "https://ghcr.io/token", service: "ghcr.io", scope: "repository:user/img:pull"},
			want: "https://ghcr.io/token|ghcr.io|repository:user/img:pull",
		},
		{
			name: "empty fields",
			in:   authChallenge{realm: "", service: "", scope: ""},
			want: "||",
		},
		{
			name: "scope-only",
			in:   authChallenge{scope: "repository:user/img:pull"},
			want: "||repository:user/img:pull",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.in.String(); got != c.want {
				t.Errorf("String() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestOCIOnlyDenyCIDRsV4_DefensiveCopy_Mega3(t *testing.T) {
	t.Parallel()
	a := OCIOnlyDenyCIDRsV4()
	b := OCIOnlyDenyCIDRsV4()
	if len(a) != len(b) {
		t.Errorf("len mismatch between calls: %d vs %d", len(a), len(b))
	}
	if len(a) == 0 {
		t.Skip("no deny entries configured; non-fatal")
	}
	a[0].Family = familyVMega3V6
	c := OCIOnlyDenyCIDRsV4()
	if c[0].Family == familyVMega3V6 {
		t.Error("mutating returned slice mutated the runtime union; copy contract broken")
	}
}

func TestOCIOnlyDenyCounterLabels_Mega3(t *testing.T) {
	t.Parallel()
	labels := OCIOnlyDenyCounterLabels()
	deny := OCIOnlyDenyCIDRsV4()
	if len(labels) != len(deny) {
		t.Fatalf("len(labels)=%d != len(DenyEntry)=%d", len(labels), len(deny))
	}
	for i, l := range labels {
		if l.CounterName == "" {
			t.Errorf("labels[%d].CounterName empty", i)
		}
		if l.Family == "" {
			t.Errorf("labels[%d].Family empty", i)
		}
		wantFamily := deny[i].Family.String()
		if l.Family != wantFamily {
			t.Errorf("labels[%d].Family = %q, want %q", i, l.Family, wantFamily)
		}
	}
}

func TestEgressAllowLoopbackFromEnv_Mega3(t *testing.T) {
	cases := []struct {
		envVal string
		setEnv bool
		want   bool
	}{
		{"1", true, true},
		{"", true, false},
		{"true", true, false},
		{"yes", true, false},
		{"0", true, false},
		{"", false, false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.envVal+"/set="+boolStrMega3(c.setEnv), func(t *testing.T) {
			if c.setEnv {
				t.Setenv("FAAS_EGRESS_ALLOW_LOOPBACK", c.envVal)
			} else {
				// Real unset-env path: t.Setenv always sets, so we
				// must use os.Unsetenv to exercise the os.LookupEnv
				// returning-false branch.
				if err := os.Unsetenv("FAAS_EGRESS_ALLOW_LOOPBACK"); err != nil {
					t.Fatalf("unsetenv: %v", err)
				}
				t.Cleanup(func() { _ = os.Unsetenv("FAAS_EGRESS_ALLOW_LOOPBACK") })
			}
			if got := EgressAllowLoopbackFromEnv(); got != c.want {
				t.Errorf("EgressAllowLoopbackFromEnv() = %v, want %v (env=%q setEnv=%v)",
					got, c.want, c.envVal, c.setEnv)
			}
		})
	}
}

func TestShortDigest_Mega3(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{"sha256:abcdef1234567890", "abcdef123456"},
		{"sha256:abcdef1234567890abcdef", "abcdef123456"},
		{"sha256:abc", "abc"},
		{"sha256:", ""},
		{"sha512:abcdef123456", "sha512:abcde"},
		{"plain-text-digest", "plain-text-d"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := short(c.in); got != c.want {
				t.Errorf("short(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestDefaultPuller_PurePassThrough_Mega3(t *testing.T) {
	t.Parallel()
	p := DefaultPuller{}
	ctx := context.Background()

	t.Run("PullDigest", func(t *testing.T) {
		got, err := p.PullDigest(ctx, "ref-x")
		if err != nil {
			t.Fatalf("PullDigest: %v", err)
		}
		if got != "ref-x" {
			t.Errorf("PullDigest = %q, want ref-x", got)
		}
	})
	t.Run("PullImageConfig", func(t *testing.T) {
		cfg, err := p.PullImageConfig(ctx, "ref-y")
		if err != nil {
			t.Fatalf("PullImageConfig: %v", err)
		}
		if len(cfg.Cmd) != 0 || len(cfg.Env) != 0 || cfg.WorkingDir != "" {
			t.Errorf("PullImageConfig = %+v, want zero-value", cfg)
		}
	})
	t.Run("PullLayers", func(t *testing.T) {
		res, err := p.PullLayers(ctx, "digest-z")
		if err != nil {
			t.Fatalf("PullLayers: %v", err)
		}
		if res.Digest != "digest-z" {
			t.Errorf("PullLayers.Digest = %q, want digest-z", res.Digest)
		}
	})
	t.Run("PullDigestWithAuth", func(t *testing.T) {
		got, err := p.PullDigestWithAuth(ctx, "ref-a", nil)
		if err != nil {
			t.Fatalf("PullDigestWithAuth: %v", err)
		}
		if got != "ref-a" {
			t.Errorf("PullDigestWithAuth = %q, want ref-a", got)
		}
	})
	t.Run("PullImageConfigWithAuth", func(t *testing.T) {
		cfg, err := p.PullImageConfigWithAuth(ctx, "ref-b", nil)
		if err != nil {
			t.Fatalf("PullImageConfigWithAuth: %v", err)
		}
		if len(cfg.Cmd) != 0 || len(cfg.Env) != 0 || cfg.WorkingDir != "" {
			t.Errorf("PullImageConfigWithAuth = %+v, want zero-value", cfg)
		}
	})
	t.Run("PullLayersWithAuth", func(t *testing.T) {
		res, err := p.PullLayersWithAuth(ctx, "digest-c", nil)
		if err != nil {
			t.Fatalf("PullLayersWithAuth: %v", err)
		}
		if res.Digest != "digest-c" {
			t.Errorf("PullLayersWithAuth.Digest = %q, want digest-c", res.Digest)
		}
	})
}

func boolStrMega3(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
