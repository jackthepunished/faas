# gregale/skd-node

> Node 22 SDK for the one-box FaaS platform. Generated from
> [`api/openapi.yaml`](../../api/openapi.yaml), wrapped in a hand-written
> façade that ships retry, RFC 7807 error sentinels, idempotency, and SSE.

> **Heads-up: package name.** The manifest `name` is `gregale/skd-node`
> (preserved literally from the original spec). The likely intended name
> is `@gregale/sdk-node`. PR 13 flips this to the conventional scoped
> form in lockstep with the first release tag.

## Requirements

- Node ≥ 22.10 (uses `--experimental-strip-types` at dev-time and the
  stable global `fetch` at runtime).
- npm ≥ 10 (or `pnpm`/`yarn` compatible).

## Install

```sh
npm install gregale/skd-node
# (or once published)
npm install @gregale/sdk-node
```

## Quick start

```ts
import { FaaSClient, AppsService, ErrNotFound } from 'gregale/skd-node';

const client = new FaaSClient('https://api.example.com', {
  token: process.env.FAAS_TOKEN!,
  retry: { maxAttempts: 3, backoffMs: 100 },
});

try {
  const app = await AppsService.getApp({ slug: 'hello' });
  console.log(app.url);
} catch (err) {
  if (err instanceof ErrNotFound) {
    console.warn('app does not exist');
  } else {
    throw err;
  }
}
```

The four canonical error sentinels (`ErrNotFound`, `ErrUnauthorized`,
`ErrRateLimited`, `ErrCapacity`) all extend `FaasError` and carry the
parsed RFC 7807 `Problem` envelope, the HTTP status, and the daemon's
`tx_id` for support tickets.

## Supported surface

Every operation in `api/openapi.yaml` is reachable through the
generated services. The canonical mapping:

| OpenAPI tag | Generated service |
|---|---|
| `account` | `AccountService` |
| `apps` | `AppsService` |
| `audit` | `AuditService` |
| `auth` | `AuthService` |
| `crons` | `CronsService` |
| `delayed_tasks` | `DelayedTasksService` |
| `deployments` | `DeploymentsService` |
| `domains` | `DomainsService` |
| `instances` | `InstancesService` |
| `invocations` | `InvocationsService` |
| `keys` | `KeysService` |
| `meta` | `MetaService` |
| `mfa` | `MfaService` |
| `queues` | `QueuesService` |
| `secrets` | `SecretsService` |
| `usage` | `UsageService` |

Regenerate via `npm run gen` (committed per ADR-013; CI's
`sdk-gen-node` job is the dirty-diff gate).

## Idempotency contract

Every mutating call (POST/PUT/PATCH/DELETE) carries an `Idempotency-Key`
header. The SDK auto-mints a UUIDv4 if you don't supply one. To pin a
stable key across retries (CI deploys, batch jobs):

```ts
client.setIdempotencyKey('deploy-2026-07-26-batch-7');
await AppsService.createApp({ requestBody: { slug: 'foo' } });
```

The server replays the same response for the same key within 24h
(apid's `idempotent` middleware). GET/HEAD skip the header — the
server doesn't dedupe reads.

## SSE streaming

The OpenAPI spec has no SSE endpoints today, but `/v1/logs/{app_id}/tail`
(and a few other out-of-spec streams) expose `text/event-stream`. Use
`streamSse`:

```ts
import { streamSse } from 'gregale/skd-node';

const resp = await fetch(`${client.baseURL}/v1/logs/hello/tail`, {
  headers: { Authorization: `Bearer ${process.env.FAAS_TOKEN}` },
});
for await (const ev of streamSse(resp, signal)) {
  console.log(ev.event, ev.data);
}
```

## Zero runtime dependencies

`dependencies: {}` — the wrapper uses only Web APIs (`fetch`,
`AbortController`, `Headers`, `URL`) plus Node 22 built-ins
(`node:crypto`, `node:test`, `node:child_process`, `node:net`).

The `devDependencies` pin `openapi-typescript-codegen@0.31.0` and
`typescript@5.6.3`. Major-version bumps require an ADR.

## CI

The `sdk-gen-node` job in `.github/workflows/ci.yml` runs
`npm ci && npm run gen:check` (regen + dirty-diff assert). The
`sdk-smoke-node` job builds the fakeapid fixture, runs `npm run
test:smoke`, and tears down.

## License

Internal — see `LICENSE`.