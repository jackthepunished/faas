package reposcan

import "testing"

// TestImageBase_RegistryAndTagAndDigest covers the four
// canonical compose image-reference forms. All four must yield
// the same basename ("postgres") so the denylist match sees one
// canonical key.
func TestImageBase_RegistryAndTagAndDigest(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"postgres", "postgres"},
		{"postgres:15-alpine", "postgres"},
		{"library/postgres", "postgres"},
		{"ghcr.io/acme/postgres:15", "postgres"},
		{"ghcr.io/acme/postgres@sha256:abcdef", "postgres"},
		{"docker.io/library/postgres:15", "postgres"},
		{"nginx:1.25-alpine", "nginx"},
		{"ghcr.io/acme/api", "api"},
		// Registry with port — must NOT strip "5000" from the
		// host; the port is part of the registry.
		{"registry.local:5000/postgres:15", "postgres"},
	}
	for _, tc := range cases {
		got := imageBase(tc.in)
		if got != tc.want {
			t.Errorf("imageBase(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestDenylist_RecognisedDatabases pins the §3 denylist match.
// Every key in datastoreDenylist must yield a non-empty hint.
func TestDenylist_RecognisedDatabases(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"postgres":      "DATABASE_URL",
		"redis":         "REDIS_URL",
		"mongo":         "MONGODB_URL",
		"kafka":         "KAFKA_URL",
		"clickhouse":    "CLICKHOUSE_URL",
		"rabbitmq":      "RABBITMQ_URL",
		"elasticsearch": "ELASTICSEARCH_URL",
		"nats":          "NATS_URL",
	}
	for img, wantHint := range want {
		gotHint, ok := denylistKind(img)
		if !ok {
			t.Errorf("denylistKind(%q) miss; want hint=%q", img, wantHint)
			continue
		}
		if gotHint != wantHint {
			t.Errorf("denylistKind(%q) hint=%q, want %q", img, gotHint, wantHint)
		}
	}
}

// TestDenylist_RegistryAndTagVariants — imageBase match must
// ignore registry+tag, so denylist entry is hit regardless.
func TestDenylist_RegistryAndTagVariants(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{
		"postgres:15-alpine",
		"library/postgres",
		"ghcr.io/acme/postgres:15",
		"ghcr.io/acme/postgres@sha256:abcdef",
		"docker.io/library/postgres:latest",
	} {
		hint, ok := denylistKind(ref)
		if !ok || hint != "DATABASE_URL" {
			t.Errorf("denylistKind(%q) = (%q, %v); want (DATABASE_URL, true)", ref, hint, ok)
		}
	}
}

// TestDenylist_NonDatastoreRef — a non-denylisted image returns
// (hint, false), signalling caller to the skip-warning path.
func TestDenylist_NonDatastoreRef(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{"nginx", "nginx:1.25", "ghcr.io/acme/api", "alpine"} {
		hint, ok := denylistKind(ref)
		if ok || hint != "" {
			t.Errorf("denylistKind(%q) = (%q, %v); want (\"\", false)", ref, hint, ok)
		}
	}
}

// TestDenylist_EmptyImage — empty string is a no-match, not a panic.
func TestDenylist_EmptyImage(t *testing.T) {
	t.Parallel()
	hint, ok := denylistKind("")
	if ok || hint != "" {
		t.Errorf("denylistKind(\"\") = (%q, %v); want (\"\", false)", hint, ok)
	}
}
