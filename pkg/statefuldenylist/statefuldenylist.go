// Package statefuldenylist is the shared OCI stateful-base-image denylist
// consulted by both the apid API gate (issue #463 / ADR-066 §Decision 4:
// pkg/api/dto.go::Sidecar.Validate) and the imaged runtime gate
// (pkg/imaged/base.go::StatefulBaseImageDenylist + StatefulDenyListMatch).
//
// Why a separate package: both `pkg/imaged` and `pkg/api` need to import
// this constant, and both packages import `pkg/api` (and `pkg/imaged`
// imports `pkg/api` — the imaged layer's surface types come from pkg/api).
// Importing `pkg/imaged` from `pkg/api` would cycle. The two-call-site
// pattern (apid API gate + imaged pull path) is the contract that
// defence-in-depth relies on; the extracted package keeps the set itself
// as the single source of truth without forcing either consumer to grow
// a dependency.
//
// The matching predicate is the same one `pkg/imaged` uses:
// strip the registry hostname (first slash-segment that looks like a
// hostname), strip any trailing `:tag` / `@sha256:...`, lower-case every
// remaining segment, and report the FIRST one whose lower-cased form
// is in the denylist. So `postgres:16`, `library/postgres:16`, and
// `ghcr.io/me/postgres-fork:v1` resolve correctly (the last one is NOT
// a match — only segments that exactly equal a denylist key count).
//
// This is a stateless package; it has no init(), no global state, and
// no I/O. The matcher is pure (input string → match/no-match).
package statefuldenylist

// Set is the Wave 0 / year-one OCI base-image denylist this platform
// refuses to deploy as a customer-facing base image or sidecar. The
// platform is stateless-only — the customer must use a managed service
// (Neon / Upstash / PlanetScale / MongoDB Atlas) for any stateful
// workload, and inject credentials via `faas secrets set`.
//
// Keys are lower-cased first path segments of the image name
// (`postgres`, `redis`, `mysql`, …). The matcher strips the registry
// hostname and tag before comparing, so `postgres:16`,
// `postgres:16-alpine`, and `library/postgres` all match; while
// `ghcr.io/me/postgres-fork` does NOT match (its first segment is
// `me`, not `postgres`).
//
// Values are short human-readable remediation hints that show up in
// the RFC 7807 Detail field so the CLI can render actionable copy.
// Both `pkg/api/errors.go::ErrSidecarStatefulDenied` and the imaged
// pull path surface this hint verbatim.
//
// Not in pkg/api/limits.go (which is numeric-only per the platform
// convention): this list is constant code at ~8 entries. If it grows
// past ~20 or needs per-plan control, move it then.
var Set = map[string]string{
	"postgres":   "use Neon (https://neon.tech) or Supabase Postgres",
	"redis":      "use Upstash Redis (https://upstash.com)",
	"mysql":      "use PlanetScale (https://planetscale.com)",
	"mariadb":    "use PlanetScale (https://planetscale.com)",
	"mongo":      "use MongoDB Atlas (https://mongodb.com/atlas)",
	"cockroach":  "use CockroachDB Cloud (https://cockroachlabs.cloud)",
	"cassandra":  "use Astra DB (https://astra.datastax.com)",
	"clickhouse": "use ClickHouse Cloud (https://clickhouse.cloud)",
}