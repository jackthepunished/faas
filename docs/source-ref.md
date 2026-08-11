# Deploy from a GitHub source ref

A one-shot, CI-friendly deploy that pins a build to a specific
Git ref on a GitHub repo. Designed for runners that have no
browser and no GitHub App install token of their own — the
control plane resolves everything from the account's existing
`github_installations` row.

## Prerequisites

- `FAAS_API` — control plane URL.
- `FAAS_TOKEN` — a deploy-time API key (the `deploy` scope, or
  admin).
- `gregale connect` completed once on a workstation (browser
  available) so the control plane already has the GitHub App
  install row for the account. This is the only step that needs
  a browser — see `gregale connect --help`.

No `GREGALE_INSTALL_TOKEN_*` env vars are required. That env var
is for the `gregale scan --repo` local-fs decomposition path;
the source-ref deploy runs server-side.

## Worked example: pin to a SHA in CI

```bash
FAAS_API=https://api.faas.example \
FAAS_TOKEN=$FAAS_TOKEN \
gregale deploy --repo onebox-faas/hello --ref $(git rev-parse HEAD)
```

This is the canonical CI shape: a runner reads `HEAD` from the
local checkout and posts a one-shot deploy to the control
plane. The control plane resolves the durable install row,
mints an installation token, fetches the codeload archive for
the SHA, spools it, validates the tarball shape, enqueues a
build, and returns the build/deployment ids.

Output:

```
Deployed hello from onebox-faas/hello@<sha> (build <build_id>, deployment <deployment_id>)
```

## Worked example: pin to a branch in dev

```bash
gregale deploy --repo onebox-faas/hello --ref main
```

Branches and tags are resolved server-side to a 40-char SHA
before the codeload fetch starts, so a `main` ref that moves
between CI runs still produces an immutable SHA-pinned build
row.

## Failure modes

| Server response | What it means | What to do |
|---|---|---|
| `409 source_ref_unavailable` | Transient githubd or codeload blip. Server sets `Retry-After: 30`. | Back off and retry; the CLI surfaces the hint on stderr. |
| `404 github_install_not_found` | The account has no `github_installations` row. | Run `gregale connect` on a workstation once, then re-run CI. |
| `413 source_too_large` | Repo tarball exceeds the per-plan `SourceTarballMaxMB` cap (Free/Hobby 100 MB, Pro/Scale 250 MB). | Trim history (`git gc`), use a sparse checkout, or upgrade plan. |
| `400 invalid_ref` | `--ref` is not a branch, tag, or 7+/40-char SHA. | Pin to a SHA or a real branch / tag. |
| `429 plan_limit_*` | Per-plan concurrency / RAM cap reached. | Wait for a slot, or upgrade. |

CI retries of the same `gregale deploy` line mint a fresh
`Idempotency-Key` on every invocation, so each retry produces
a distinct build row. If your CI needs a true dedupe (same key
across retries folds to one row), set `Idempotency-Key` on the
SDK call directly — `Client.DeployFromSourceRef` accepts the
underlying HTTP shape.

## What it is NOT

- **Not a webhook bind.** For push-event auto-deploy use
  `gregale connect` and let the GitHub App's push events fire
  the build. The `--repo --ref` shape is one-shot.
- **Not a git deploy-key fetch.** The server uses the GitHub App
  install token (ADR-012, ADR-020); the control plane never
  sees the customer's PAT. The install token is scoped to a
  single `StreamSourceRef` RPC and discarded before the response
  is returned.

## Wire contract

- `POST /v1/apps/{slug}/deployments/source-ref`
- Body: `{"repo": "OWNER/NAME", "ref": "<branch|tag|sha>", "format": "tarball"}`
- Auth chain: `authLimited → requireMFA → requireScope(ScopesDeployWriteSurface) → idempotent → handler`
- SDK binding: `pkg/api.Client.DeployFromSourceRef` (Go) /
  `DeploymentsService.createDeploymentFromSourceRef` (Node).

## See also

- `docs/adr/092-headless-source-ref-deploy.md` — design rationale.
- `docs/cli-setup.md` — shell completion + man page install.