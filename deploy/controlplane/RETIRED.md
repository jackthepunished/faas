# deploy/controlplane/ — RETIRED 2026-08-15

This directory was the v1 control-plane installer and config tree. It was
retired in [issue #911 / PR-1 (ADR-110)](../../../docs/adr/110-declarative-split-box-manifest.md)
on 2026-08-15 because every artifact it produced is now generated from a
declarative manifest + the ansible role `files/` tree.

## v2 path (replaces this directory)

1. **First-time host setup**
   ```bash
   make bootstrap                                # ansible; deploy/ansible/bootstrap.yml
   ```
2. **Edit the manifest** (`deploy/manifest/splitbox.yaml`)
   - Per-host DNS, role (single-box / control-plane / compute-only), endpoints
   - Manifest schema: `pkg/manifest/manifest.go`
3. **Validate + render + install**
   ```bash
   gregale manifest validate --manifest-file deploy/manifest/splitbox.yaml
   gregale manifest render   --manifest-file deploy/manifest/splitbox.yaml --host <name>
   gregale release install   --git-sha <sha>    --host <name>
   ```
4. **Secrets surface** (pending PR-X `gregale secrets init`)
   - host.age, session.key, deploy_ed25519, rclone.conf, box-age-key, sign-key
   - Until PR-X ships, operators must hand-bootstrap these per the comments
     left in the v2 ansible roles.

## What's still here (Phase 1 tombstone, Phase 2 deletion)

Phase 1 (this PR) keeps the v1 files on disk as a tombstone. Phase 2 (after
PR-X ships) deletes them.

| File / Dir | v1 Role | v2 Replacement |
|------------|---------|----------------|
| `bootstrap.sh` | v1 installer | `make bootstrap` + `gregale manifest {validate,render}` + `gregale release install` |
| `deploy.sh` | 12-LOC wrapper around `deployctl deploy` | direct: `/opt/faas/current/bin/deployctl deploy <id>` |
| `config/*.toml` | sed-replaced with `__DROPLET_IP__` | renderer (`pkg/renderer`) emits `/etc/faas/*.toml` |
| `systemd/faas-*.service` | hand-maintained units | ansible role `files/` (same content; diff returns empty) |
| `systemd/faas-cp.slice` | hand-maintained slice | ansible role `files/` (PR-1 copied) |
| `tmpfiles.d/faas.conf` | `/run/faas` setup | ansible role `files/faas.conf` |
| `sealed.env.example` | secrets template | PR-X `gregale secrets init` |
| `README.md` | operator narrative (this file) | `deploy/README.md` + PR-7 runbook |

## CD pipeline

The `cd-controlplane.yml` workflow rewired in PR-1 reads the systemd units +
slice + tmpfiles.d from `deploy/ansible/roles/*/files/` (the v2 tree). It no
longer touches `deploy/controlplane/`.

## Why a tombstone + later deletion (not immediate deletion)

- The renderer (PR-2) covers `/etc/faas/*.toml`, systemd units, cgroup v2
  subtree_control, and PKI leaves. It does NOT cover the secrets surface —
  that's PR-X.
- Until PR-X ships, an operator following the v2 path still has to hand-
  bootstrap host.age / session.key / deploy_ed25519 / rclone.conf /
  box-age-key / sign-key. The hand-bootstrap recipe is the v1 bootstrap.sh
  body, which we keep on disk (in this tombstone) for grep-ability.
- Phase 2 lands after PR-X merges. See the cluster status table in
  `docs/adr/110`.
