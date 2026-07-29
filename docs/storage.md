# Storage on this platform

This platform is stateless. Your code runs in an ephemeral microVM
that wakes, executes, parks, and forgets. Bring your own state.

## Why stateless

Scale-to-zero economics is the load-bearing reason: an instance
that holds state would either need to stay warm forever (defeats
the model) or write state somewhere that survives a wake/park
cycle (adds a write-amplification layer we can't afford on a
one-box build). MicroVMs are fungible — every wake boots from the
same snapshot, every park destroys local state — and snapshot
reuse only works because instances are interchangeable. Local
filesystem state is ephemeral by design; every wake is a fresh
boot.

The platform's deny-list (see `pkg/imaged/base.go` and the
`stateless_only_violation` 422) rejects stateful base images at
accept time: `postgres:16`, `mysql:8`, `redis:7`, `mongo:7`, and
the rest. Tarballs with `VOLUME` directives or top-level
`data/` / `db/` directories are rejected for the same reason.

## Recommended providers

The platform doesn't ship a managed-storage product; it integrates
with the providers customers already use. Pick the category that
matches the workload, plug in the URL, and `faas secrets set` the
env vars the runtime injects at wake.

| Category        | Provider                              | Env vars |
|-----------------|---------------------------------------|----------|
| Object storage  | AWS S3, Cloudflare R2, Backblaze B2   | `S3_BUCKET`, `S3_REGION`, `S3_ACCESS_KEY_ID`, `S3_SECRET_ACCESS_KEY`, optional `S3_ENDPOINT` (R2/B2) |
| Managed SQL     | Neon, Supabase, PlanetScale, CockroachDB Cloud | `DATABASE_URL` |
| KV / cache      | Upstash Redis, Upstash KV             | `UPSTASH_REDIS_REST_URL`, `UPSTASH_REDIS_REST_TOKEN` |
| Document        | MongoDB Atlas, Turso (libSQL)         | provider-specific |
| Queue / scheduler | Upstash QStash, AWS SQS             | `QSTASH_TOKEN`, etc. |

## Wiring it up

`faas secrets set` writes the value to sealed secrets at rest; at
wake time the runner injects every secret as a plain environment
variable inside the guest. No SDK lock-in, no special API surface,
no extra headers — `process.env.DATABASE_URL` is what your code
reads.

```sh
faas secrets set --app <slug> DATABASE_URL='postgres://user:pass@host/db?sslmode=require'
```

## Don't

- **Don't** add a `VOLUME` directive to your Dockerfile. The
  accept-time tarball scan rejects it.
- **Don't** run `postgres:16`, `mysql:8`, `redis:7`, `mongo:7`,
  `mariadb`, `cockroach`, `cassandra`, `clickhouse` as your base
  image. The deny-list rejects the deployment before the guest
  ever wakes.
- **Don't** write to database-shaped local paths
  (`/var/lib/postgresql`, `/data`, `/db`). The guest-init advisory
  (Wave 0 PR-C, ships separately) flags these writes at runtime.

## Templates

`faas init` scaffolds a working project that uses the right
provider. Each template fails clearly at startup (or in the first
invocation, for function handlers) if the secrets aren't set.

- `faas init --template=s3-uploader` — port-8080 Node app, multipart
  upload to S3 / R2 / B2.
- `faas init --template=slack-bot` — port-8080 Node app, Slack
  Events with HMAC-SHA256 signature verification.
- `faas init --template=rest-api-postgres` — port-8080 Node app,
  Express + `pg` against a managed PostgreSQL.
- `faas init --template=cron-worker` — exported handler for Upstash
  QStash invocations, with a Redis-backed progress counter.

See `faas init --help` for the full flag surface and the
`--deploy` chain (materialize + deploy in one command).
