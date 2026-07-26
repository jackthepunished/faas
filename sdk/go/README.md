# faas-go — onebox FaaS Go SDK

> **PR 2 of issue #266** — placeholder. Real README lands in PR 13 alongside the first
> tagged release. Until then, the SDK module is internal to the monorepo and is consumed
> only by the daemon's own tests.

This module mirrors the `pkg/api/*` wire surface for the onebox FaaS platform:

- typed `Client` and request/response DTOs,
- RFC 7807 problem envelope + structured errors,
- bearer-auth + auto-minted idempotency-key on mutating calls,
- cursor pagination helpers (`ListDeploymentsAll`),
- SSE streaming (`StreamAppLogs`, `StreamDeploymentLogs`, `StreamEvents`),
- multipart deploy helper (`DeployMultipart`).

The public package name will be `faas` (re-exported in PR 3) and the canonical
module path is `github.com/poyrazK/faas-go`.

## Local development

```
cd sdk/go
go build ./...
go test ./...
```

The module is a leaf: it imports only Go stdlib. The split in PR 12 trims
`pkg/api/*` (in the daemon's main module) to its server-only files; this
module then becomes the canonical home for the wire DTOs.
