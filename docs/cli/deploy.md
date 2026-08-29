# Deploying from the CLI

`gregale deploy` ships a directory, tarball, OCI image, or pinned
GitHub ref to an app on the control plane. The path that runs is
inferred from flags + the cwd's git state; this page captures the
non-obvious pieces (which files actually get shipped, what the
`--json` envelope looks like, when a "deploy this subdir only"
intent needs a different command).

## Source-root semantics

When cwd is inside a git repository whose `origin` is a recognised
GitHub URL, `gregale deploy` ships the **committed tree at HEAD from
the repo root**, not the cwd subdir. `git archive HEAD` is run with
gitDir = the enclosing repo root (see `gitArchiveHEAD` at
`cmd/gregale/git_local.go:345`), so a `cd apps/services/api && gregale
deploy` from inside a monorepo uploads the **entire** committed tree.

Two reasons:

1. The control plane's builder reads the framework hint from the
   top of the tree (Railpack's `package.json`, `requirements.txt`,
   `go.mod`, `Dockerfile`, etc.). Shipping only the subdir loses
   monorepo-hosting files (lockfiles at root, shared config) and
   the build silently misdetects the framework.
2. Reproducibility is anchored at HEAD's full tree hash. A
   subdir-only upload can't be replayed deterministically against
   the upstream remote.

For a subdir-only deploy, `cd` to the subdir and use the cwd-auto-pack
fallback (no git, no origin — the CLI packs the cwd):

```bash
cd monorepo/packages/api
rm -rf .git                   # strip any nested git dirs first
unset GREGALE_GIT_DEPTH_CHECK  # or pass --no-git if running interactively
gregale deploy --tarball /tmp/api.tar.gz
```

For a decomposed monorepo deploy (one CLI invocation, N apps), use
`gregale scan --path .` and the project-plan apply path; see the
decomposition PR (issue #791 / ADR-090).

## Monorepo / nested-project detection

When the cwd contains **monorepo workspace markers** in a nested
subdir (e.g. `apps/web/package.json`, `apps/services/api/package.json`),
the CLI prints a hint and exits with a "no deployable source here"
error rather than guessing:

```
note: detected nested project marker(s) at apps/web/services/api/package.json
hint: this looks like a monorepo subdir; run `gregale scan --path .` to
      decompose into per-app plans and deploy each one.
```

The detection walks depth 2 from the cwd (so `apps/web/package.json`
and `apps/services/api/package.json` both trigger it). Depth 4+
remains intentionally out of scope — a `pkg/billing/internal/lib/`
marker is too deep for the CLI to act on without explicit operator
intent. See `detectNestedMarkerHint` at `cmd/gregale/pack.go:691`.

## `--json` receipt shape

`gregale deploy --json` emits a single JSON document with the
`DeploymentResponse` shape promoted to top-level (via embedding)
plus four provenance-only fields:

| Field            | Type   | Source                                                          |
|------------------|--------|-----------------------------------------------------------------|
| `id`             | string | `api.DeploymentResponse.ID` (server-issued)                      |
| `app_id`         | string | `api.DeploymentResponse.AppID`                                  |
| `status`         | string | `api.DeploymentResponse.Status` — `"pending"` at deploy time    |
| (all other `DeploymentResponse` fields) | — | see [`pkg/api`](../../pkg/api)                                 |
| `app_url`        | string | `deployedAppURL(slug)` — `https://<slug>.<FAAS_APPS_DOMAIN>` (default `gregale.dev`). Slug comes from `--name` / cwd-derived name (CLI input), NOT from `app_id` on the response — the wire's `app_id` is the 32-char hex primary key and the gateway routes on slug. |
| `commit_sha`     | string | `git rev-parse HEAD^{commit}` from the zero-config branch; empty on image / source-ref / non-git fallback paths |
| `dirty`          | bool   | `git status --porcelain` is non-empty; omitempty so a clean repo renders no key |
| `source_sha256`  | string | lower-case hex sha256 of the tarball bytes just shipped; empty on image and source-ref (server pulls) paths |

The receipt is consumed by CI / GitHub Actions tooling that needs to
pin a deploy to a specific upstream artifact. Parse with any JSON
decoder that accepts extra top-level keys: existing SDK clients that
unmarshal into `api.DeploymentResponse` keep working — the extra
fields are silently dropped.

For the source-ref CI path, commit pinning is captured server-side
rather than in the receipt (the CLI never sees the tarball bytes).
See [`docs/source-ref.md`](../source-ref.md) for the
`--repo OWNER/NAME --ref $SHA` shape and the install-token trust
boundary.

## Reproducibility note

Three flavors of "what was deployed", pinned differently:

- **Zero-config (`gregale deploy` from a git repo)**: pinned at
  `commit_sha` (HEAD) + the committed tree of HEAD. Re-runs of the
  same SHA are byte-identical assuming the working tree was clean at
  deploy time; `dirty:true` in the receipt flags a deploy whose
  working tree had uncommitted changes (the SHA is still pinned;
  the uncommitted bytes were NOT shipped).
- **Tarball (`gregale deploy --tarball foo.tar.gz`)**: pinned at
  `source_sha256` (the bytes shipped). Re-runs of the same SHA
  are byte-identical. No `commit_sha` because no git detection ran.
- **Image (`gregale deploy --image registry.x/app@sha256:...`)**:
  pinned at the OCI digest in `--image`. `dep.ImageDigest` on the
  response carries the same value.
- **Source-ref (`gregale deploy --repo OWNER/NAME --ref SHA`)**:
  pinned at the GitHub ref. SHA-pinned refs (`--ref
  $(git rev-parse HEAD)`) are byte-identical upstream; branch refs
  are not. See [`docs/source-ref.md`](../source-ref.md).
