# rest-api-postgres

A minimal port-8080 Node.js REST API (`GET /notes`, `POST /notes`)
backed by a managed PostgreSQL provider. **This is a scaffold, not a
production API** — it auto-creates a `notes` table on first boot so
the customer can `curl` it without running migrations, and uses a
small connection pool sized for cold-boot wake latency.

## Managed service

Plug in any managed PostgreSQL:

- **Neon** — serverless, free tier. URL looks like
  `postgres://user:pass@ep-xxx.us-east-1.aws.neon.tech/db?sslmode=require`.
- **Supabase** — `postgres://postgres:pass@db.xxx.supabase.co:5432/postgres?sslmode=require`.
- **PlanetScale (Postgres beta)** — provider-supplied URL.
- **CockroachDB Cloud** — `postgres://user:pass@free-tier.gcp-us-central1.cockroachlabs.cloud:26257/db?sslmode=require`.

**Don't** run `postgres:16` as your base image — the platform's
deny-list rejects it at accept time (Wave 0 PR-A).

## Set the secrets

```sh
faas secrets set --app <slug> DATABASE_URL='postgres://user:pass@host:port/db?sslmode=require'
```

If `DATABASE_URL` is missing, the handler exits at startup with the
exact `faas secrets set` command.

## Deploy

From this directory:

```sh
faas deploy
```

## Try it

```sh
curl https://<slug>.<DOMAIN>/healthz       # → {"ok":true,"db":"ok"}
curl -X POST -H 'content-type: application/json' \
     -d '{"body":"hello from faas"}' \
     https://<slug>.<DOMAIN>/notes          # → {"ok":true,"note":{...}}
curl https://<slug>.<DOMAIN>/notes          # → {"ok":true,"notes":[{...}]}
```

## Re-deploy after edits

Edit `handler.js`, then `faas deploy` from this directory.
