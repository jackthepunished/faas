# deploy/controlplane/ — RETIRED 2026-08-15 (Phase 2: tombstone deletion)

This directory was the v1 control-plane installer and config tree. It was
retired in [issue #911 / PR-1 (ADR-110)](../../docs/adr/110-declarative-split-box-manifest.md)
on 2026-08-15.

The historical operator narrative that used to live here is preserved
in the git history of `deploy/controlplane/bootstrap.sh` (deleted in
Phase 2 on 2026-08-15 by issue #911 / PR-1) and the v1 config files.
The v2 path is `make bootstrap` + `gregale manifest {validate,render}`
+ `gregale release install` + `gregale secrets init`; see the umbrella
ADR for the cluster PR decomposition.
