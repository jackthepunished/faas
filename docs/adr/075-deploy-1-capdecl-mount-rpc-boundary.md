# ADR-075: pkg/capdecl + RPC-mount boundary (DEPLOY-1)

## Status

Accepted. Issue #648.

## Context

The Tier A7 deploy cycle has been losing the production box to two
distinct architectural regressions that share a root cause: **a
permissive boundary around `unix.Mount(2)` and Linux process
capabilities**. Both regressions cost the box hours of downtime and
13+ failed cd-controlplane runs.

### Regression 1 — the mount-syscall boundary

imaged's parent-ref overlay mount was the only `unix.Mount(2)` call
sitting in a non-vmmd package. It crept in over a chain of PRs
(imaged's first overlay implementation → `unix.Mount(2)` for
cleanliness → "imaged needs `cap_sys_admin`" added to the systemd
unit). Each step was reasonable in isolation; together they
moved a privileged operation out of vmmd — the only component
documented as the firecracker/jailer owner (CLAUDE.md component
ownership) — and into imaged. When the kernel rejected the
mount (overlayfs `tmpfile` constraint on ext4), imaged
crash-looped in production for hours because no architecture
review caught the boundary violation.

### Regression 2 — the capability-boundary

A misconfigured `AmbientCapabilities=` on a unit file is
invisible at the code-review stage: the daemon *starts*, the
syscall fails with `EPERM` deep inside `pkg/imaged`, and the
unit restart-loops until an operator notices. The same
problem will hit every future capability-gated call (cgroup
v2, seccomp filters, namespace operations, BPF). Without a
declarative inventory of "this daemon needs these caps," a
typo or missing `=` in a unit file is undetectable until
production fails.

### What was tried

The original "minimum scope" plan (issue #648 / DEPLOY-1) was
to ship `pkg/capdecl` + the lint rule alone, leaving the
mount-syscall regression for a follow-up. The user explicitly
rejected that split ("Go with B. Doesnt matter how much it
takes, we need to solve failures at the end" — option B =
ship capdecl + lint rule AND fix the architectural violation
in the same PR). This ADR records that decision and the
boundary it locks in.

## Decision

### 1. `pkg/capdecl` — declarative Linux-capability contract

Every daemon ships a `cmd/<daemon>/caps.go` that exports:

```go
var capsDecl = capdecl.Declaration{
    Allow: []string{"cap_sys_admin", "cap_net_admin", ...},
    Deny:  nil, // or ["cap_dac_read_search"]
}
```

The declaration names the **minimum** capabilities the daemon
needs in production. `pkg/capdecl/runtimecheck.MustCheckOnBoot`
runs at the top of every daemon's `run` / `runWithDeps` and:

- Parses `/proc/self/status` for `CapBnd` / `CapEff` / `CapPrm`
- Fails fast with a `*runtimecheck.Violation` + `os.Exit(1)` if:
  - any `Allow` cap is **missing** from `CapBnd` (the
    `CapabilityBoundingSet` not set up in the unit file)
  - any `Deny` cap is **present** in `CapBnd` (an ambient
    capability granted to a daemon that documented it must
    NOT have it)
  - any declared cap name is unknown to the kernel
- Logs an Error line on violation, stays silent on success
  (every boot doesn't need a "validated" log line)

The declarations are checked into the repo so a unit-file
typo shows up in code review as a missing `Allow` cap on the
PR, not as a 4 AM page.

### 2. `pkg/vmmdmount` + `MountOverlayParent` RPC — the mount-syscall boundary

The `unix.Mount(2)` syscall is **only** legal in
`pkg/vmmdmount`. Every other package routes through vmmd's
`MountOverlayParent` / `UmountOverlayParent` gRPC methods,
which validate path prefixes before forwarding to
`pkg/vmmdmount`:

- `lowerdir` must start with `/srv/fc/parent/` (the
  read-only base parent mounted by `MountParentExt4`)
- `upperdir` / `workdir` / `merged` must start with
  `/dev/shm/faas-base-staging/` (tmpfs — kernel
  `tmpfile+workdir` overlayfs contract)
- empty paths → `InvalidArgument`
- paths outside the prefix → `ErrInvalidOverlayPath` →
  `InvalidArgument` (no vmmd-side mount attempt)

`MountOverlayParent` is idempotent on unknown mountpoints so
imaged's `defer-after-error` cleanup pattern is safe.

### 3. `pkg/vmmdgrpc.VmmdAPI` extension

The gRPC server interface grows two methods (the schema was
already extended by the proto regeneration in PR-K):

```go
MountOverlayParent(ctx context.Context, lowerdir, upperdir,
    workdir, merged string) error
UmountOverlayParent(ctx context.Context, merged string) error
```

Server-side validation lives in `pkg/vmmdgrpc/server.go`;
syscall-level validation + path-prefix enforcement lives in
`pkg/vmmdmount/overlay.go`. The split keeps the gRPC layer
free of host-filesystem constants.

### 4. Lint enforcement — the tripwires

Three rules in `.golangci.yml` make the boundary
compile-time-enforced:

```yaml
depguard.vmmdmount-vmmd-only:
  deny: [pkg: github.com/onebox-faas/faas/pkg/vmmdmount]
  files: ["!**/cmd/vmmd/**", "!**/pkg/vmmdgrpc/**",
          "!**/pkg/vmmdmount/**", "!**/pkg/fcvm/**"]

depguard.vmmdgrpc-not-for-control-plane:
  deny: [pkg: github.com/onebox-faas/faas/pkg/vmmdgrpc]
  files: ["**/cmd/apid/**", "**/cmd/meterd/**",
          "**/cmd/githubd/**", "**/cmd/faas/**"]

forbidigo.forbid:
  - pattern: '^Mount$', pkg: '^golang\.org/x/sys/unix$'
  - pattern: '^Unmount$', pkg: '^golang\.org/x/sys/unix$'
```

The first rule blocks any non-vmmd/non-fcvm package from
importing `pkg/vmmdmount`. The second blocks apid / meterd
/ githubd / faas CLI from dialing vmmd directly (CLAUDE.md
ownership). The third is the belt-and-suspenders: any
`unix.Mount` or `unix.Unmount` call outside `pkg/vmmdmount`
fails lint, even if the depguard rule was bypassed (e.g. via
a wildcard import or a future refactor that moves the
package).

### 5. imaged's `AmbientCapabilities=cap_sys_admin` is GONE

The interim fix (PR-F, deploy PR #637) added
`AmbientCapabilities=cap_sys_admin` to `faas-imaged.service`
so imaged could call `unix.Mount(2)` directly. This ADR
**reverts that workaround** in a follow-up cd-controlplane
PR (per the user's "stay focus on architectural" directive,
the unit-file change is in a separate PR so the diff stays
reviewable). Once imaged's mount path moves to vmmd RPC,
the cap is no longer needed and the unit file goes back to
zero ambient capabilities — the architectural goal of
imaged-as-unprivileged is achieved.

## Consequences

### Positive

- **One architectural owner for mount syscalls.** No future
  PR can move a `unix.Mount(2)` call out of vmmd without
  tripping depguard in CI. The CI tripwire is the durable
  enforcement; this ADR is the rationale.
- **One architectural owner for capabilities.** The unit-file
  cap set and the in-code cap set are now defined in the
  same place (the unit file's `AmbientCapabilities=` plus
  `cmd/<daemon>/caps.go`'s `Allow`). A drift between the
  two fails the boot gate with a `*runtimecheck.Violation`
  pointing at the missing cap, not at a deep-in-the-stack
  `EPERM`.
- **imaged reverts to unprivileged.** No AmbientCapabilities,
  no root, no cap_sys_admin. The daemon's capset matches
  the spec's "no root except vmmd" rule (§11).
- **Reusable mount RPC seam.** Future mount needs (guest
  chroot setup, builder artifact overlays, e2e harness
  filesystem construction) all use the same gRPC path,
  not their own `unix.Mount(2)`.

### Negative

- **One more round-trip per overlay mount.** imaged's parent
  staging now RPCs vmmd to mount (vs. the in-process
  `unix.Mount(2)` from PR-K). Cost: ~0.5 ms on the local
  unix socket. Negligible vs. the ~30 s of base-image pull
  and the ~10 s of overlay assembly.
- **A test seam for every daemon.** Every daemon's
  `runDeps` grows a `capCheck func() error` field that
  tests inject as `nopCapCheck()`. The default (nil field)
  resolves to `runtimecheck.MustCheckOnBoot(capsDecl, log, nil)`,
  which exits on violation in production. The injection is
  1 line per test, but the seam is the cost of separating
  "testability" from "production safety."
- **The mount RPC's path-prefix validation is load-bearing.**
  A future PR that relaxes `/srv/fc/parent/` or
  `/dev/shm/faas-base-staging/` silently expands the attack
  surface. The constants are exported
  (`vmmdmount.MountRoot`, `vmmdmount.OverlayStagingRoot`) so
  a future change is a search-replace tripwire, not a
  silent drift.

### Out of scope

- **Removing `MountParentExt4ReadOnly` / `UmountParentExt4`
  from imaged.** These are already routed through vmmd RPC
  (ADR-053). The mount-syscall boundary was the last open
  hole; the read-only mount uses a different code path
  (`mount(2)` with `MS_RDONLY`) inside `pkg/vmmdmount`'s
  sibling helper, which is the correct location.
- **Reverting the imaged `AmbientCapabilities=cap_sys_admin`
  unit-file change.** That's the follow-up cd-controlplane
  PR (mentioned above). Keeping it as a separate PR keeps
  the diff reviewable: the boundary fix is this PR; the
  unit-file cleanup is its own PR with its own deploy +
  verify cycle.
- **Builderd / schedd / gatewayd-internal ever needing
  capabilities.** Their `capsDecl` is empty (no Allow, no
  Deny). If a future PR adds a privileged op to one of them,
  the gate fails at boot — exactly the failure mode this
  ADR is designed to make loud.
- **A `pkg/daemonunit` generator.** That's DEPLOY-2 (issue
  #649). The capdecl package is structured so the future
  generator can read `cmd/<daemon>/caps.go` and emit a
  matching systemd unit fragment, but the generator itself
  is out of scope here.

## References

- PR-K (PR #644 / commit 30fae9e1) — `unix.Mount(2)` replaces
  `exec.Command("mount")` in imaged's parent-ref overlay
  path. The boundary move in this PR supersedes PR-K's
  in-imaged mount.
- PR-K.2 (PR #646) — staging dir to `/dev/shm` for the
  kernel `tmpfile+workdir` overlayfs contract. PR-K.2 is
  what makes `MountOverlayParent` work in vmmd without a
  production re-deploy of the staging-dir layout.
- PR #648 (issue tracker) — DEPLOY-1 entry point.
- CLAUDE.md "Component ownership" — the prose this ADR
  encodes as code + lint rules.
- `pkg/capdecl/capdecl.go` — Declaration type + Validate.
- `pkg/capdecl/runtimecheck/cmd.go` — `MustCheckOnBoot` boot
  gate.
- `pkg/vmmdmount/overlay.go` — `MountOverlayParent` /
  `UmountOverlayParent` path validation + syscall wrapper.
- `pkg/vmmdgrpc/server.go` — gRPC handlers for the new
  RPC pair.
- `.golangci.yml` — depguard + forbidigo rules.
