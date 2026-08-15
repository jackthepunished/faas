# deploy/controlplane/ — RETIRED 2026-08-15

This directory was the v1 control-plane installer and config tree. It was
retired in [issue #911 / PR-1 (ADR-110)](../../docs/adr/110-declarative-split-box-manifest.md)
on 2026-08-15. See [RETIRED.md](./RETIRED.md) for the v2 path mapping.

The historical operator narrative that used to live here has been preserved
in the git history of `deploy/controlplane/bootstrap.sh` and the v1 config
files. Phase 2 (after PR-X `gregale secrets init` ships) deletes this
directory.
