package egresssocket

import (
	"testing"
)

// TestResolveSocketPath covers the four-input precedence order
// documented in the package docstring:
//
//	envVal > legacyEnvVal > cfgVal > legacyCfgVal > DefaultSocketPath
//
// The table tests every interesting combination of empty/non-empty
// inputs. Empty inputs MUST be skipped; the first non-empty source
// wins.
func TestResolveSocketPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		envVal       string
		legacyEnvVal string
		cfgVal       string
		legacyCfgVal string
		want         string
	}{
		// Default fallback: every source empty → DefaultSocketPath.
		{
			name: "all empty → default",
			want: DefaultSocketPath,
		},
		// Each source wins in isolation.
		{
			name:   "envVal wins over default",
			envVal: "/run/faas/egress.custom.sock",
			want:   "/run/faas/egress.custom.sock",
		},
		{
			name:         "legacyEnvVal wins when envVal empty",
			legacyEnvVal: "/run/faas/gatewayd-egress.sock",
			want:         "/run/faas/gatewayd-egress.sock",
		},
		{
			name:   "cfgVal wins when both envs empty",
			cfgVal: "/etc/faas/egress.sock",
			want:   "/etc/faas/egress.sock",
		},
		{
			name:         "legacyCfgVal wins when all envs + cfgVal empty",
			legacyCfgVal: "/etc/faas/gatewayd-egress.sock",
			want:         "/etc/faas/gatewayd-egress.sock",
		},
		// Precedence ordering: each layer trumps the next.
		{
			name:         "envVal beats legacyEnvVal",
			envVal:       "/run/faas/A.sock",
			legacyEnvVal: "/run/faas/B.sock",
			want:         "/run/faas/A.sock",
		},
		{
			name:         "legacyEnvVal beats cfgVal",
			legacyEnvVal: "/run/faas/B.sock",
			cfgVal:       "/etc/faas/C.sock",
			want:         "/run/faas/B.sock",
		},
		{
			name:         "cfgVal beats legacyCfgVal",
			cfgVal:       "/etc/faas/C.sock",
			legacyCfgVal: "/etc/faas/D.sock",
			want:         "/etc/faas/C.sock",
		},
		// Full chain: envVal wins outright.
		{
			name:         "envVal wins in full chain",
			envVal:       "/run/faas/A.sock",
			legacyEnvVal: "/run/faas/B.sock",
			cfgVal:       "/etc/faas/C.sock",
			legacyCfgVal: "/etc/faas/D.sock",
			want:         "/run/faas/A.sock",
		},
		// Full chain sans envVal: legacyEnvVal wins second.
		{
			name:         "legacyEnvVal wins in chain without envVal",
			legacyEnvVal: "/run/faas/B.sock",
			cfgVal:       "/etc/faas/C.sock",
			legacyCfgVal: "/etc/faas/D.sock",
			want:         "/run/faas/B.sock",
		},
		// Full chain sans envvars: cfgVal wins third.
		{
			name:         "cfgVal wins in chain without envvars",
			cfgVal:       "/etc/faas/C.sock",
			legacyCfgVal: "/etc/faas/D.sock",
			want:         "/etc/faas/C.sock",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ResolveSocketPath(tc.envVal, tc.legacyEnvVal, tc.cfgVal, tc.legacyCfgVal)
			if got != tc.want {
				t.Errorf("ResolveSocketPath(%q, %q, %q, %q) = %q, want %q",
					tc.envVal, tc.legacyEnvVal, tc.cfgVal, tc.legacyCfgVal, got, tc.want)
			}
		})
	}
}

// TestResolveSocketPath_NeverReturnsEmpty is a load-bearing
// invariant: the function must NEVER return "". Every caller uses the
// return value as a path to Stat or Bind; an empty string would
// defeat the read-both-prefer-new safety net.
func TestResolveSocketPath_NeverReturnsEmpty(t *testing.T) {
	t.Parallel()

	// Cycle every combination of empty vs non-empty for the four
	// inputs, asserting none yields "". The shrinkable subset is
	// (envVal=""), but the function's contract is the blanket
	// assertion, so we exercise the full 16-cell Cartesian product.
	for a := 0; a < 2; a++ {
		for b := 0; b < 2; b++ {
			for c := 0; c < 2; c++ {
				for d := 0; d < 2; d++ {
					env := ""
					legacyEnv := ""
					cfg := ""
					legacyCfg := ""
					if a == 1 {
						env = "/x.sock"
					}
					if b == 1 {
						legacyEnv = "/y.sock"
					}
					if c == 1 {
						cfg = "/z.sock"
					}
					if d == 1 {
						legacyCfg = "/w.sock"
					}
					got := ResolveSocketPath(env, legacyEnv, cfg, legacyCfg)
					if got == "" {
						t.Errorf("ResolveSocketPath(%q,%q,%q,%q) returned empty string",
							env, legacyEnv, cfg, legacyCfg)
					}
				}
			}
		}
	}
}

// TestResolveFromOS_EnvWinsOverConfig covers the OS-helper wrapper:
// even when the cfg-level fields are populated, an env var beats
// them. This is the production call path in cmd/meterd/main.go and
// cmd/gatewayd-internal/egress_grpc.go.
func TestResolveFromOS_EnvWinsOverConfig(t *testing.T) {
	t.Parallel()

	env := map[string]string{
		"FAAS_EGRESS_SOCKET":         "/run/faas/from-env-new.sock",
		"FAAS_GATEWAY_EGRESS_SOCKET": "/run/faas/from-env-legacy.sock",
	}
	getenv := func(key string) string { return env[key] }

	got := ResolveFromOS(getenv, "/etc/faas/cfg-new.sock", "/etc/faas/cfg-legacy.sock")
	want := "/run/faas/from-env-new.sock"
	if got != want {
		t.Errorf("ResolveFromOS with both envs set = %q, want %q (env beats cfg)", got, want)
	}
}

// TestResolveFromOS_LegacyEnvBeatsConfig covers the case where the
// legacy env var is set but the new one is not; the legacy env must
// still win over both cfg-level fields, mirroring the in-package
// ResolveSocketPath order.
func TestResolveFromOS_LegacyEnvBeatsConfig(t *testing.T) {
	t.Parallel()

	env := map[string]string{
		"FAAS_GATEWAY_EGRESS_SOCKET": "/run/faas/from-env-legacy.sock",
		// FAAS_EGRESS_SOCKET deliberately unset.
	}
	getenv := func(key string) string { return env[key] }

	got := ResolveFromOS(getenv, "/etc/faas/cfg-new.sock", "/etc/faas/cfg-legacy.sock")
	want := "/run/faas/from-env-legacy.sock"
	if got != want {
		t.Errorf("ResolveFromOS with legacy env only = %q, want %q", got, want)
	}
}

// TestResolveFromOS_NilEnvGetterFallsBackToOSGetenv asserts the
// production-mode fallback works: when the caller passes a nil
// EnvGetter (or omits it), the function uses os.Getenv. We pin
// behaviour by setting a value via t.Setenv and asserting it flows
// through. Tests that need to inject values should NOT rely on this
// path — they should pass a literal EnvGetter.
func TestResolveFromOS_NilEnvGetterFallsBackToOSGetenv(t *testing.T) {
	// DELIBERATELY not t.Parallel() — t.Setenv mutates process env
	// and would race with sibling tests.
	t.Setenv("FAAS_EGRESS_SOCKET", "/run/faas/test-egress.sock")

	got := ResolveFromOS(nil, "", "")
	want := "/run/faas/test-egress.sock"
	if got != want {
		t.Errorf("ResolveFromOS(nil) with FAAS_EGRESS_SOCKET set = %q, want %q", got, want)
	}
}

// TestResolveFromOS_AllEmptyReturnsDefault exercises the safety
// net: nothing set anywhere, production default applies. This is the
// "fresh deploy with no env, no config" case.
func TestResolveFromOS_AllEmptyReturnsDefault(t *testing.T) {
	// DELIBERATELY not t.Parallel() — t.Setenv would race.
	t.Setenv("FAAS_EGRESS_SOCKET", "")
	t.Setenv("FAAS_GATEWAY_EGRESS_SOCKET", "")

	got := ResolveFromOS(nil, "", "")
	if got != DefaultSocketPath {
		t.Errorf("ResolveFromOS with all empty = %q, want %q (default)", got, DefaultSocketPath)
	}
}

// TestConstantsAreDistinct is a static guard: if someone ever
// accidentally aliases DefaultSocketPath to LegacySocketPath (or
// vice versa), the read-both-prefer-new fallback collapses and
// existing deployments break silently. Cheap to check at unit-test
// time.
func TestConstantsAreDistinct(t *testing.T) {
	t.Parallel()

	if DefaultSocketPath == LegacySocketPath {
		t.Fatalf("DefaultSocketPath == LegacySocketPath (%q) — the new path must be distinct from the legacy path so the read-both fallback is meaningful",
			DefaultSocketPath)
	}
	if DefaultSocketPath == "" {
		t.Fatalf("DefaultSocketPath is empty")
	}
	if LegacySocketPath == "" {
		t.Fatalf("LegacySocketPath is empty")
	}
}
