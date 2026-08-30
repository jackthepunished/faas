# ADR-136 · OCI image-config correctness for the registry pull path

- **Status:** accepted
- **Date:** 2026-08-29
- **Deciders:** Gregale platform team
- **Forks:** [Epic #1186 — make Gregale a first-class OCI container platform](#1186), sub-task A.1-A.3 + file-ownership preservation (a small piece of cross-cutting workstream E + I).

## Context

Gregale's OCI registry pull path silently drops three load-bearing fields
from the OCI image-config spec, and the layer writer unconditionally
forgets the tar header `Uname`/`Gid`:

1. **`Entrypoint`** — `pkg/imaged/handler.go::manifestFromImageConfig` reads
   only `Cmd` and discards `Entrypoint`. A Docker image with
   `ENTRYPOINT ["/x"]` and no `CMD` is invoked with an empty argv.
2. **`User`** — `pkg/oci/registry.go::parseImageConfig` does not read the
   `User` field at all. Two parallel parsers exist (`parseImageConfig` vs
   `oci.ParseConfig` in `pkg/oci/image.go:54`); they disagree on which OCI
   fields they consume, and the registry path consumes the loser.
3. **`HEALTHCHECK`, `StopSignal`, `StopGracePeriod`** — never parsed on
   either path.

A separate, latent bug:

4. **`pkg/rootfs/layer.go::applyEntry`** opens every regular file with
   `os.FileMode(hdr.Mode)&os.ModePerm` and writes under the imaged-daemon
   UID. `hdr.Uname` and `hdr.Gid` are never read. Files owned by UID 1001
   inside the image appear as UID 0 inside the published ext4.

The OCI/Docker image-config schema is open-ended and adds fields every
major release; the spec allows either **flat fields** (`Cmd`, `Env`,
`WorkingDir` at the top level — Docker v2) or a **nested `config`
envelope** (OCI image-config). The codebase has, for historical reasons,
two parallel decoders that prefer flat-or-nested inconsistently and
each only reads a subset of fields.

## Decision

### 1. One canonical OCI image-config parser

`oci.ParseConfig` (package `pkg/oci`, file `image.go`) becomes the single
canonical decoder for OCI/Docker image-config blobs. `parseImageConfig`
(in `registry.go:385`) is rewritten to delegate to `ParseConfig` after
extracting the same raw bytes; the private struct lives in a new
`pkg/oci/oci.go` and is shared by both functions. **Both Docker v2 flat
and OCI nested-`config` inputs are accepted**, with flat preferred when
both are present (preserves today's behaviour — single-source-of-truth for
the preference; no customer-facing change).

`oci.Config` is the rich struct (Entrypoint, Cmd, Env, WorkingDir, User,
DiffIDs). `oci.ImageConfig` is the consumer-facing projection that maps
Env slice → map and is the seam `pkg/imaged` consumes. Both widen in the
same commit (see Decision 3).

### 2. Env flattening bug fix

`pkg/oci/registry.go::parseImageConfig` today splits each
`"KEY=VALUE"` env entry by walking bytes until the first `=` (lines
412-420 today). Entries with no `=` are dropped silently — including
`"=VALUE"`-style entries where the key is the empty string. The new
parser uses `strings.Cut(kv, "=")` which preserves the value for an
empty-key entry (today's `=PATH` in some Distroless images has been
silently lost). Entries with no `=` at all still drop to `key=""`. The
spec allows either; we choose the more permissive interpretation and
document it.

### 3. Surface the missing OCI fields end-to-end

`oci.ImageConfig` widens to include: `Entrypoint []string`, `User string`,
`Healthcheck *ImageHealthcheck`, `StopSignal string`, `StopGracePeriod
int`. New exported type `oci.ImageHealthcheck` carries
`{Test []string, Interval, Timeout, Retries, StartPeriod time.Duration}`.
The wire shape (JSON-on-disk) is unchanged for any caller that ignores
unknown fields, which the platform does today.

`oci.ManifestFromConfig` projects these onto `api.AppManifest` (Decision
4). The order of preference per OCI/Docker semantics is
**Entrypoint || Cmd** for argv, and **the user's `User` field** when
non-empty (numeric only — see Decision 5).

### 4. AppManifest widens additively (no version bump)

`pkg/api/appmanifest.go::AppManifest` gains three optional fields:
`Healthcheck *api.AppManifestHealthcheck`, `StopSignal string`,
`StopGracePeriod time.Duration`. `Validate()` extends to
`StopGracePeriod ∈ [0, 5 * time.Minute]` (a conservative cap; per-plan
tightening lands in M-2 alongside the runtime wiring of these fields).

The wire format `/etc/faas/app.json` round-trips these fields as plain
JSON. **No `AppManifestV1_5` type, no migration.** Existing guest-init
ignores unknown JSON keys (idempotent `encoding/json.Unmarshal` on a
typed struct), and new guest-init reads the new fields when present.

### 5. Layer-entry ownership preservation (numeric uid/gid only)

`pkg/rootfs/layer.go::applyEntry` reads `hdr.Uname`/`hdr.Gid` after
`os.OpenFile` succeeds. The hard rule is:

- If both fields parse as numeric AND `0 ≤ uid ≤ 65534` AND
  `0 ≤ gid ≤ 65534`: `os.Lchown(target, uid, gid)`.
- Out of range, non-numeric, or empty: leave the file under the
  imaged-daemon UID. Emit `imaged_ownership_clamp_total{reason}`
  counter where `reason ∈ {"out_of_range", "non_numeric"}`. The layer
  build does not fail (an image that does not declare `User` will leave
  both fields empty under the existing today path; preserving that
  default).

The 0-65534 ceiling is **load-bearing for §11**: it guarantees a
customer-pulled image cannot see the tenant-uid range (ADR-019 keeps
jail uids at 20000-29999), cannot write a uid that overlaps with the
daemon-jailer surface, and cannot acquire UID 65535 (Linux
"nobody"/overflow sentinel). A user declaring `USER 0` continues to
work; `USER 1001` works; `USER 99999` is clamped with a counter
increment and surface-log.

`tar.TypeChar` / `tar.TypeBlock` device entries (currently silently
dropped — they have no consumer in the two-drive scheme) are explicitly
**rejected** in the layer-ownership path: the `applyEntry` switch
returns an error on `TypeChar`/`TypeBlock` so a malicious image cannot
publish a device node to the guest ext4. The ADR-040 symlink policy is
the precedent for the strictness; a 7-bit block/char device exposed in
the guest filesystem is a privilege-escape primitive that the §11
checklist explicitly forbids.

### 6. Naming the cross-cutting seam

The cluster of files touched (commit 8 = pkg/rootfs/layer.go +
pkg/rootfs/build.go) is **not a new package**, **not a new table**, **no
migration**. Slot reservations 00525-00527 are claimed for any
follow-up schema work (none required for M-1 but the slot bank keeps
the cross-PR slot precheck green).

## Consequences

### Positive

- **Alpine, Distroless, Debian, Ubuntu, Busybox, Node, Python, Go
  images** all respect the spec on first deploy. Today's silent
  capability drift between source-build and registry-build paths
  disappears; a customer can pull `gcr.io/distroless/static-debian12`
  with `ENTRYPOINT ["/app"]` and get `["/app"]` argv.
- **A registry image's `User` actually takes effect** for numeric
  UIDs, closing a latent §11-style regression (any image with
  `USER != root` was running as root in the guest).
- **HEALTHCHECK fields are surfaced** for downstream consumers
  (M-2's ADR-X5 wires the runtime poll-loop). Out-of-scope for M-1 to
  honour those fields at runtime — that requires ADR-X3 (container-
  native lifecycle contract) and a supervisor change.
- **One decoder** instead of two. New OCI fields (the spec keeps
  adding them) are added in one place.
- **Ownership changes are scoped** by the [0, 65534] cap; the
  tenant-jail-uid surface (ADR-019) remains unreachable from
  customer-supplied data.
- **M-1 is grounded in fixtures**, not just unit tests. The
  acceptance criterion for issue #1186 ("Alpine, Debian, Ubuntu,
  Distroless, Node, Python, Go, Gregale-based images are tested") is
  closed by the commit-7 fixture harness.

### Negative

- **`pkg/rootfs` gains filesystem-semantics surface area.** The
  ownership-preservation path changes what `os.Stat` returns on
  staging files; the pgtest parity suite and the metal-lima
  acceptance suite must catch any unintended behaviour shift. Commit
  8 is sequenced last precisely so the prior 7 commits are isolated
  by the time this lands.
- **Named users (`USER node`, `USER postgres`) are explicitly
  out-of-scope.** Today's behaviour for them is "the customer image
  runs as the default user". Documented in the ADR; addressed in M-3
  via a guest-side `/etc/passwd` map keyed by `(image_digest, user)`.
- **Cross-PR slot reservation against PR #1185** (slots 517-524).
  We fence 525-527, leaving 517-524 free for PR #1185. If PR #1185
  lands first, our slots stay clean. If our PR lands first and
  #1185 rebases later, #1185 hits the cross-PR fence precheck and
  picks new slots — that's the established pattern (memory entry:
  `cross-pr-rebase-fence-deletion-hazard.md`).
- **Old guest-init cannot read the new fields** — but it ignores
  them, by JSON-decode semantics. There is no wire-format
  incompatibility.
- **Layer-owner policy is fixed at numeric-with-cap.** A future
  ADR may widen to named users (M-3) or relax the cap (permitted by
  §11 only if the tenant-uid range is also widened; out of scope for
  M-1).

### Neutral

- `parseImageConfig`'s address doesn't change; callers keep
  importing the same symbol. Same for `ParseConfig`.
- The OCI registry pull path's surface to `pkg/imaged` widens by
  five fields. Any test fake that satisfies `Puller` /
  `AuthPuller` (cmd/e2e/fakevmm, etc.) compiles unchanged because
  the additional fields are additive.

## Decision matrix

| Surface | Today | M-1 outcome |
|---|---|---|
| Entrypoint on registry | dropped | surfaced |
| Cmd on registry | surfaced | surfaced (unchanged) |
| User (numeric) on registry | dropped | surfaced |
| User (named) on registry | dropped (clamped to "app") | dropped (deferred to M-3) |
| HEALTHCHECK on registry | dropped | surfaced (runtime wiring = M-2) |
| StopSignal on registry | dropped | surfaced (runtime wiring = M-2) |
| StopGracePeriod on registry | dropped | surfaced (runtime wiring = M-2) |
| =VALUE env keys | dropped | preserved |
| `=KEY` env entries | dropped | preserved |
| tar TypeChar / TypeBlock | silently dropped | rejected with error |
| hdr.Uname/Gid on layer | always daemon UID | numeric-only chown, [0, 65534] cap |

## Rejected alternatives

- **Cross-drive whiteouts** (overlayfs char-device to hide base-dir
  victims). Rejected for M-1 — addresses the `layer.go:21-24` TODO
  but the underlying two-drive scheme is unchanged. M-3 problem;
  M-3 lands full-rootfs assembly which subsumes the issue.
- **Named-user `/etc/passwd` resolution**. Rejected for M-1 —
  requires either bundling a guest-side map keyed by image digest,
  or shipping a runtime resolver. Both pull in M-3 work (full-rootfs
  assembly makes a guest-side map straightforward; without it,
  the map would have to be persisted in `/etc` of the shared base,
  contaminating every app).
- **A new `AppManifestV1_5` struct / dual-write window**. Rejected —
  additive widening is sufficient (`json.Unmarshal` ignores unknown
  keys). The cost of dual-write is two extra migrations and a
  per-deployment round-trip flag; we don't earn that complexity for
  a dormant-field set whose runtime effects land in M-2.
- **Range cap at `[0, 65535]`**. Rejected — UID 65535 is the Linux
  "nobody"/overflow sentinel and is unsafe to honour from a customer
  image. The `[0, 65534]` cap is the conservative ceiling that
  matches Linux user-id ranges per `man 5 shadow`.
- **No upper cap** (`uid, gid := hdr.Uname, hdr.Gid; chown`). Rejected
  — would let a customer image declare a UID overlapping with the
  tenant-uid space (ADR-019 owns 20000-29999) or the docker-jailer
  range. Violates §11.
- **Per-app scope of the cap** (cap by plan). Rejected for M-1 —
  uniform [0, 65534] is the safest default; per-plan tightening lands
  in M-2 alongside the lifecycle contract work.
- **Including `Volumes` from the OCI config**. Rejected — ADR-069
  §Decision 7 and ADR-047 §"Why" both forbid customer-controlled
  volume mounts at the workload-set level. Parsing Volumes from
  image config without using them would be misleading.
- **Silent clamping** (no counter, no log). Rejected — operations
  needs to see clamp events to detect adversarial images. The
  `imaged_ownership_clamp_total{reason}` counter and a per-build
  structured log entry are mandatory.
- **Keep ownership preservation as a follow-up to a separate ADR**.
  Rejected — the bug is concrete and load-bearing for any non-root
  image; deferring it to M-3 would ship the parser unification on
  top of a silent root-on-mount regression for every customer who
  deploys a non-root image today.
- **Re-derive the parser merging earlier than this PR**. Rejected —
  attempting to broaden the parser scope (OCI indexes, multi-arch
  selection) in the same commit would explode blast radius. C and
  the structural index handling are M-3 / post-merge work.

## Cross-references

- **Forced by PR #1 of M-1** (Mega-PR #1 of issue #1186):
  - `pkg/oci/oci.go` (new) — shared raw decoder
  - `pkg/oci/registry.go::parseImageConfig` — delegates to ParseConfig
  - `pkg/oci/image.go::ParseConfig` — consumes rawConfig
  - `pkg/oci/puller.go::ImageConfig` — widens additively
  - `pkg/oci/image.go::ImageHealthcheck` (new)
  - `pkg/oci/image.go::ManifestFromConfig` — surfaces new fields
  - `pkg/imaged/handler.go::manifestFromImageConfig` — converges with local-OCI path
  - `pkg/imaged/handler.go::buildImageLayer` — fails fast when Entrypoint and Cmd both empty
  - `pkg/api/appmanifest.go::AppManifest` — adds Healthcheck, StopSignal, StopGracePeriod
  - `pkg/api/dto.go::DeploymentHealthcheck` — adds Test/Interval/Timeout/Retries/StartPeriod
  - `pkg/imaged/apply_overrides.go` — extends overrides
  - `pkg/rootfs/layer.go::applyEntry` — ownership preservation
  - `pkg/rootfs/build.go::Build` — clamp counter integration
  - `migrations/00525_reserve_slot.sql`, `00526_reserve_slot.sql`,
    `00527_reserve_slot.sql` (slot bank only — no schema change in M-1)
  - `pkg/imaged/testdata/oci-fixtures/{alpine_3_19,distroless_static,...}.json`
    — fixture harness

- **Loading constraints** (existing ADRs this PR must not violate):
  - ADR-040 (OCI layer symlink policy) — symlink `Linkname` stored
    verbatim, path-traversal defence, whiteout semantics. Commit 8
    preserves all of this verbatim; no symlink-touching change.
  - ADR-053 (deploy-overrides) — the DEPLOY-level override layer.
    M-1's image-level semantics and ADR-053's deploy-level override
    coexist with clear precedence: customer override wins.
  - ADR-057 (runtime healthcheck probe) — the HOST-side `vmm.waitReady`
    HTTP probe runtime. **Orthogonal to this ADR's image-level
    HEALTHCHECK field surface**; M-2 will wire ADR-057 to consume
    the surfaced field.
  - ADR-058 (cosign deploy-time enforcement) — out of scope for M-1;
    references the `apps.require_signed` field that PR/issue #197
    builds against. M-1's ownership surface is independent.
  - ADR-062 (per-app private-registry Basic Auth) — out of scope;
    the auth path goes through `oci.AuthPuller` unchanged.
  - ADR-019 (jailer invocation) — owns the **20000-29999** tenant-uid
    range. M-1's cap at `[0, 65534]` is the layer ABOVE that range
    that customer images can see; it never overlaps.
  - ADR-041 (migration slot reservation convention) — three fences
    at 00525-00527 follow this pattern.

- **Issue / PR relationships**:
  - **#1186** (parent epic) — M-1 of the four-Mega-PR plan
    (M-1..M-4). This ADR closes sub-tasks A.1, A.2 (parser
    unification + semantics) and a small piece of I (file
    ownership).
  - **PR #1185** (durable async jobs, draft) — owns slots
    517-524 + ADR-134+135. We claim 525-527; sidestep
    friction. No blocker relationship.
  - **#1182** (zero-config first deploy) — M-1 doesn't ship the
    `--runtime` / `--handler` forwarder; M-2 follows.
  - **#600** (digest-pinned OCI base refs) — informational;
    M-1's "validate before pulling all layers" posture is the same
    shape but doesn't duplicate the rule.
  - **#474** (guest-init supervisor split) — pre-requisite for M-2,
    not M-1. M-1 doesn't touch `guest/init/`.
  - **#1184** (broader stateless compute) — parent of #1186;
    M-2 will converge execution-mode taxonomy with #1184
    Workstream C and ADR-050.

- **Spec sections**:
  - §4.6 (imaged — image and snapshot service) — the seam M-1
    fixes is the imaged/rootfs boundary; no spec change.
  - §11 (security hardening, ship-blocking) — M-1's ownership-cap
    and device-rejection policy are both §11 hardening moves; both
    are required for §11 sign-off on the registry-pulled image path.
  - §14 (delivery plan) — M-1 ships as part of an M7/M8 milestone
    unblock; M-2 onward closes the M8 surface.

- **Tests pinning this ADR**:
  - `pkg/oci/parse_unified_test.go` — both flat Docker and nested
    OCI configs parse through the same decoder
  - `pkg/oci/parse_unified_test.go` — `=VALUE` env keys preserved
  - `pkg/imaged/manifest_from_image_config_test.go` — entrypoint,
    cmd, user, healthcheck, stopsignal fixtures round-trip to
    stable `AppManifest`
  - `pkg/rootfs/layer_ownership_test.go` — numeric chown,
    out-of-range clamp counter, named fall-through
  - `pkg/imaged/testdata/oci-fixtures/*.json` (commit 7) —
    Alpine, Distroless, Debian, Busybox, Node, Python, Go,
    Gregale-based acceptance
  - `cmd/apid/spec_compliance_test.go` — extension covers the
    new fields in the OpenAPI consumer surface
