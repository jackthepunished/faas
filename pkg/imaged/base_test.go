// Wave 0 stateless-only deny-list tests for pkg/imaged/base.go.
//
// The deny-list is the platform's first line of defense against stateful
// base images — postgres:16, redis:7, mysql:8, mongo:7 — that would
// silently lose data on the next wake/park cycle. These tests pin the
// ref-parsing predicate (firstPathSegment) and the deny-list membership
// check (StatefulDenyListMatch) so a future refactor (e.g. moving to a
// dedicated OCI ref parser) can't silently regress one branch.

package imaged

import (
	"strings"
	"testing"
)

// TestStatefulDenyListMatch_KnownStateful: every well-known stateful
// base image is denied regardless of registry, tag, or digest format.
func TestStatefulDenyListMatch_KnownStateful(t *testing.T) {
	cases := []string{
		"postgres",                       // bare Docker Hub short-form
		"postgres:16",                    // bare + tag
		"postgres:16-alpine",             // bare + tag
		"library/postgres",               // explicit Docker Hub library path
		"docker.io/library/postgres:16",  // full Docker Hub ref
		"docker.io/postgres:16",          // Docker Hub short-form with registry
		"redis",
		"redis:7-alpine",
		"mysql:8.0",
		"mariadb:11",
		"mongo:7",
		"cockroach:v23.1",
		"cassandra:5.0",
		"clickhouse:24.1",
		"localhost:5000/myrepo/postgres:dev", // local registry + stateful image
	}
	for _, ref := range cases {
		t.Run(ref, func(t *testing.T) {
			hint, denied := StatefulDenyListMatch(ref)
			if !denied {
				t.Errorf("expected denial for %q, got pass-through", ref)
			}
			if hint == "" {
				t.Errorf("expected non-empty hint for %q", ref)
			}
		})
	}
}

// TestStatefulDenyListMatch_KnownClean: a non-stateful image name is
// not denied even when it shares a substring with a denied name
// ("postgres-fork" must NOT match "postgres" because the first path
// segment is the full directory name, not a substring match).
func TestStatefulDenyListMatch_KnownClean(t *testing.T) {
	cases := []string{
		"ghcr.io/onebox-faas/runner-node22:latest", // platform's own base
		"ghcr.io/onebox-faas/runner-python312:latest",
		"node:22-slim",                  // not in the deny-list
		"ghcr.io/me/postgres-fork:1.0",  // postgres-fork is NOT postgres
		"my-postgres-app",               // hyphenated name does not match "postgres"
		"alpine:3.20",
		"nginx:1.27",
		"python:3.12-slim",
		"",                              // empty ref → fail-open
		"docker.io/library/alpine:latest",
	}
	for _, ref := range cases {
		t.Run(ref, func(t *testing.T) {
			hint, denied := StatefulDenyListMatch(ref)
			if denied {
				t.Errorf("expected pass-through for %q, got denial (hint=%q)", ref, hint)
			}
		})
	}
}

// TestStatefulDenyListMatch_DigestForm: digest-pinned forms (the
// production default — issue #53 / M5 acceptance) still match when the
// image name is stateful. Pinned because the IndexAny(@, :) strip in
// firstPathSegment handles both formats and either branch regressing
// would silently let a stateful image through.
func TestStatefulDenyListMatch_DigestForm(t *testing.T) {
	cases := map[string]bool{
		"postgres@sha256:0000000000000000000000000000000000000000000000000000000000000000": true,
		"redis@sha256:0000000000000000000000000000000000000000000000000000000000000000":    true,
		"node@sha256:0000000000000000000000000000000000000000000000000000000000000000":     false,
	}
	for ref, wantDenied := range cases {
		t.Run(ref, func(t *testing.T) {
			_, denied := StatefulDenyListMatch(ref)
			if denied != wantDenied {
				t.Errorf("ref=%q denied=%v want=%v", ref, denied, wantDenied)
			}
		})
	}
}

// TestPathSegmentsAfterRegistry_EdgeCases: the underlying ref parser
// pins its behaviour on every shape we care about. Kept separate so a
// future refactor that splits the parser from the deny-list matcher
// (e.g. to expose it for tests in pkg/oci) has a clear acceptance test.
func TestPathSegmentsAfterRegistry_EdgeCases(t *testing.T) {
	cases := map[string][]string{
		"postgres":                         {"postgres"},
		"postgres:16":                      {"postgres"},
		"postgres@sha256:deadbeef":         {"postgres"},
		"library/postgres":                 {"library", "postgres"},
		"docker.io/library/postgres:16":    {"library", "postgres"},
		"docker.io/postgres:16":            {"postgres"},
		"ghcr.io/me/myapp:abc1234":         {"me", "myapp"},
		"localhost:5000/myrepo/myapp":      {"myrepo", "myapp"},
		"myreg.example.com/x/y/z:tag":      {"x", "y", "z"},
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			got := pathSegmentsAfterRegistry(in)
			if len(got) != len(want) {
				t.Fatalf("pathSegmentsAfterRegistry(%q) = %v, want %v", in, got, want)
			}
			for i := range got {
				if got[i] != want[i] {
					t.Errorf("pathSegmentsAfterRegistry(%q)[%d] = %q, want %q", in, i, got[i], want[i])
				}
			}
		})
	}
}

// TestStatefulBaseImageDenylist_AllKeysHaveHint: every entry in the
// deny-list MUST carry a remediation hint so the CLI can render an
// actionable message. Pinned because an empty hint silently degrades
// the customer experience to "stateless_only_violation" with no next
// step.
func TestStatefulBaseImageDenylist_AllKeysHaveHint(t *testing.T) {
	for name, hint := range StatefulBaseImageDenylist {
		if strings.TrimSpace(hint) == "" {
			t.Errorf("deny-list entry %q has empty hint", name)
		}
	}
}
