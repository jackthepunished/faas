# faas-go — onebox FaaS Go SDK

> **PR 2 of issue #266** — placeholder. Real README lands in PR 13 alongside the first
> tagged release. Until then, the SDK module is internal to the monorepo and is consumed
> only by the daemon's own tests. **Publishing is gated on PR 13.**

This module mirrors the `pkg/api/*` wire surface for the onebox FaaS platform:

- typed `Client` and request/response DTOs,
- RFC 7807 problem envelope + structured errors,
- bearer-auth + auto-minted idempotency-key on mutating calls,
- cursor pagination helpers (`ListDeploymentsAll`),
- SSE streaming (`StreamAppLogs`, `StreamDeploymentLogs`, `StreamEvents`),
- multipart deploy helper (`DeployMultipart`).

The public package name will be `faas` (re-exported in PR 3) and the canonical
module path is `github.com/poyrazK/faas-go`.

## Go version

The SDK targets `go 1.23` (the floor of the daemon's own toolchain at the
moment of extraction). The daemon's `go.mod` is `go 1.25.7`, but the SDK
stays on 1.23 so a customer pinned to an older Go toolchain can still
consume it. The SDK uses only Go 1.23-or-earlier features — no `iter`,
no `structs`, no `cmp.Ordered` 1.24 additions. A future toolchain bump
in the daemon does not force a bump here.

## Local development

```
cd sdk/go
go build ./...
go test ./...
go vet ./...
```

The CI gate is `.github/workflows/ci.yml::sdk-go` — a separate job that
runs `go build`, `go vet`, and `go test` inside `sdk/go/`. The daemon's
own `make test` walks only the daemon's package tree, so the SDK needs
its own gate until the PR 12 split reverses the import direction.

The module is a leaf: it imports only Go stdlib. The split in PR 12 trims
`pkg/api/*` (in the daemon's main module) to its server-only files; this
module then becomes the canonical home for the wire DTOs.
