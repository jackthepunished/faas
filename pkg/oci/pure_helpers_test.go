// pure_helpers_test.go — fill pkg/oci coverage of the tiny pure
// helpers reachable without a real Docker registry.
//
// Targets:
//   - authChallenge.String (the canonical cache-key tuple)
//   - OCIOnlyDenyCIDRsV4 (defensive copy)
//   - OCIOnlyDenyCounterLabels (the projection)
//   - EgressAllowLoopbackFromEnv (env-var SSRF opt-out)
//   - NewEgressHTTPClientAllowLoopback (nil vs. real client)
//
// Whitebox `package oci`.
package oci

import (
	"net/http"
	"net/netip"
	"os"
	"strings"
	"testing"
)

// --- authChallenge.String -------------------------------------

func TestAuthChallenge_String_RoundTrip(t *testing.T) {
	c := authChallenge{
		realm:   "https://auth.example.com/token",
		service: "registry.example.com",
		scope:   "repository:foo/bar:pull",
	}
	got := c.String()
	// The tuple is "realm|service|scope" — pipe-separated so
	// callers can split on it for sub-key access without
	// canonicalising further.
	for _, want := range []string{c.realm, c.service, c.scope} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, want substring %q", got, want)
		}
	}
	// Pin the separator contract — a future refactor to "/"
	// (path-style) would change the cache-key shape.
	if strings.Count(got, "|") != 2 {
		t.Errorf("String() = %q, want exactly 2 pipe separators", got)
	}
}

// --- OCIOnlyDenyCIDRsV4 ---------------------------------------

// OCIOnlyDenyCIDRsV4 returns a defensive copy — mutating the
// returned slice (in particular the underlying array) MUST NOT
// mutate the internal catalog.
func TestOCIOnlyDenyCIDRsV4_DefensiveCopy(t *testing.T) {
	got1 := OCIOnlyDenyCIDRsV4()
	if len(got1) == 0 {
		t.Fatal("OCIOnlyDenyCIDRsV4 returned empty")
	}
	// Capture the original prefix for the first entry.
	orig := got1[0].Prefix
	got1[0].Prefix = netip.MustParsePrefix("0.0.0.0/0") // poison the copy
	got2 := OCIOnlyDenyCIDRsV4()
	if got2[0].Prefix != orig {
		t.Errorf("mutation leaked: got %v, want %v", got2[0].Prefix, orig)
	}
}

// --- OCIOnlyDenyCounterLabels ---------------------------------

func TestOCIOnlyDenyCounterLabels_NonEmptyAndMatchesCIDRs(t *testing.T) {
	labels := OCIOnlyDenyCounterLabels()
	cidrs := OCIOnlyDenyCIDRsV4()
	if len(labels) != len(cidrs) {
		t.Errorf("labels = %d, cidrs = %d (must match 1:1)", len(labels), len(cidrs))
	}
	for i, l := range labels {
		if l.CounterName == "" {
			t.Errorf("[%d] empty CounterName", i)
		}
		if l.Family == "" {
			t.Errorf("[%d] empty Family", i)
		}
	}
}

// --- EgressAllowLoopbackFromEnv -------------------------------

func TestEgressAllowLoopbackFromEnv_DefaultFalse(t *testing.T) {
	t.Setenv("FAAS_EGRESS_ALLOW_LOOPBACK", "")
	if EgressAllowLoopbackFromEnv() {
		t.Error("unset: got true, want false")
	}
}

func TestEgressAllowLoopbackFromEnv_TruthyValue(t *testing.T) {
	t.Setenv("FAAS_EGRESS_ALLOW_LOOPBACK", "1")
	if !EgressAllowLoopbackFromEnv() {
		t.Error("\"1\": got false, want true")
	}
}

// "true" / "yes" / anything other than the exact string "1"
// must NOT opt-out the SSRF guard (the safe fallback).
func TestEgressAllowLoopbackFromEnv_NonExactMatch(t *testing.T) {
	for _, v := range []string{"true", "yes", "1 ", " 1"} {
		t.Setenv("FAAS_EGRESS_ALLOW_LOOPBACK", v)
		if EgressAllowLoopbackFromEnv() {
			t.Errorf("%q: got true, want false (only exact \"1\" matches)", v)
		}
	}
}

// --- NewEgressHTTPClientAllowLoopback -------------------------

func TestNewEgressHTTPClientAllowLoopback_NilWhenNotOptedIn(t *testing.T) {
	t.Setenv("FAAS_EGRESS_ALLOW_LOOPBACK", "")
	if got := NewEgressHTTPClientAllowLoopback(); got != nil {
		t.Errorf("not opted in: client = %v, want nil", got)
	}
}

func TestNewEgressHTTPClientAllowLoopback_RealClientWhenOptedIn(t *testing.T) {
	t.Setenv("FAAS_EGRESS_ALLOW_LOOPBACK", "1")
	got := NewEgressHTTPClientAllowLoopback()
	if got == nil {
		t.Fatal("opted in: client = nil, want non-nil")
	}
	if got.Transport == nil {
		t.Error("opted in: Transport = nil, want non-nil")
	}
}

// Sanity: the opted-in client is a usable *http.Client (not a
// stub that the rest of the codebase's http.Client.Do would
// reject).
func TestNewEgressHTTPClientAllowLoopback_UsableAsHTTPClient(t *testing.T) {
	t.Setenv("FAAS_EGRESS_ALLOW_LOOPBACK", "1")
	var c *http.Client = NewEgressHTTPClientAllowLoopback()
	if c == nil {
		t.Fatal("expected non-nil")
	}
	// Don't actually fire a request — the test exists to pin
	// the static-type contract. We just need c.Do to be
	// callable.
	_ = c.Do
}

// Defensive: even with an unrelated env var set, the helper
// must not opt-in by accident.
func TestNewEgressHTTPClientAllowLoopback_UnrelatedEnvIgnored(t *testing.T) {
	t.Setenv("FAAS_EGRESS_ALLOW_LOOPBACK", "")
	os.Setenv("OTHER_ENV", "1") //nolint:errcheck
	t.Cleanup(func() { os.Unsetenv("OTHER_ENV") })
	if got := NewEgressHTTPClientAllowLoopback(); got != nil {
		t.Errorf("unrelated env: client = %v, want nil", got)
	}
}
