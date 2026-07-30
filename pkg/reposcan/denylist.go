package reposcan

import "strings"

// datastoreDenylist maps the datastore image's basename (without
// registry or tag) to the env-hint variable customers typically use
// to consume it. Phase 3 prints the hint in the confirm table —
// "DATABASE_URL" tells the customer exactly what shape their
// Postgres URL takes, so the SaaS hand-off is one step shorter.
//
// Match rule: imageBase(image) == key. ghcr.io/acme/postgres:15 and
// postgres:15 and library/postgres all match `postgres`. Alpine tag
// variants likewise. Tags & digests ignored.
//
// Surfaced (not provisioned) — phase 3's apid transaction refuses
// to bind a workload to a Managed. The stateless contract
// (pkg/api/limits.go's stateless_only_violation, ADR-046,
// docs/storage.md) is held forward one cycle: a customer now
// learns the carve-out at the confirm table, not at runtime.
// datastoreDenylist is the canonical image-name → env-hint map.
// Map keys are literal OCI image basenames (postgres, redis, …)
// — these are NOT magic strings repeated elsewhere in the package,
// they ARE the spec. Lifting them to named constants would obscure
// the deny-list rather than document it; we silence the lint here.
//
//nolint:goconst
var datastoreDenylist = map[string]string{
	"postgres":      hintDatabaseURL,
	"postgis":       hintDatabaseURL,
	"mysql":         hintDatabaseURL,
	"mariadb":       hintDatabaseURL,
	"redis":         hintRedisURL,
	"valkey":        hintRedisURL,
	"mongo":         hintMongoURL,
	"cassandra":     hintCassandraURL,
	"clickhouse":    hintClickhouseURL,
	"elasticsearch": hintElasticsearchURL,
	"opensearch":    hintOpensearchURL,
	"rabbitmq":      hintRabbitmqURL,
	"kafka":         hintKafkaURL,
	"nats":          hintNatsURL,
	"minio":         hintMinioURL,
	"memcached":     hintMemcachedURL,
	"etcd":          hintEtcdURL,
}

// imageBase returns the portion of an OCI image reference between
// the (optional) registry host and the (optional) tag/digest.
//
//	"postgres"                            -> "postgres"
//	"postgres:15-alpine"                  -> "postgres"
//	"library/postgres"                    -> "postgres"
//	"ghcr.io/acme/postgres:15"            -> "postgres"
//	"ghcr.io/acme/postgres@sha256:..."    -> "postgres"
//	"docker.io/library/postgres:15"       -> "postgres"
//
// A bare registry-only reference like "ghcr.io" returns the
// original string; the map miss then classifies it as non-
// datastore, falling through to the skip-warning path.
func imageBase(image string) string {
	image = strings.TrimSpace(image)
	if image == "" {
		return ""
	}
	// Drop any digest suffix first; it never affects the name.
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}
	// Drop tag (anything after the last colon that doesn't look like
	// a port number on a registry host — registries don't carry
	// colons in plain hostname form, so this is safe).
	if i := strings.LastIndex(image, ":"); i >= 0 {
		// Split on the last "/". If the colon is BEFORE that
		// slash, the colon is a registry port and the trailing
		// ":port" we found is part of the host. Treat the whole
		// thing as path-without-tag.
		lastSlash := strings.LastIndex(image, "/")
		if i > lastSlash {
			image = image[:i]
		}
	}
	// The base name is everything after the last "/".
	if i := strings.LastIndex(image, "/"); i >= 0 {
		return image[i+1:]
	}
	return image
}

// denylistKind returns (envHint, true) if the image matches the
// datastore denylist; ("", false) otherwise. The bool lets callers
// distinguish a denylist miss from an empty-string entry (the map
// never has empty entries today but it's a tripwire).
func denylistKind(image string) (string, bool) {
	base := imageBase(image)
	if base == "" {
		return "", false
	}
	hint, ok := datastoreDenylist[base]
	return hint, ok
}
