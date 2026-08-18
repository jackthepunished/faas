# GHCR publish and retention policy

The builder base is published as `ghcr.io/poyrazk/builder-base` by
`.github/workflows/images.yml`.

Tag policy:

- `latest` is a convenience tag only and may move on every successful main
  build;
- `sha-<full 40-character commit>` is the immutable deployment tag and must
  never be overwritten or deleted while a release or manifest can reference
  it;
- pullers should resolve the deployment tag to a platform-specific digest and
  record that digest in the release manifest / `FAAS_BUILDER_BASE_REF`.

Retention policy:

- retain all `sha-*` tags and their manifests for the lifetime of any release;
- pruning automation may remove only unreferenced convenience/preview tags;
- package visibility must be explicitly public before a public multi-box
  release, and the package must remain in the `poyrazk` namespace;
- the repository `GITHUB_TOKEN` is sufficient for the main-branch workflow;
  a personal access token is not required in CI.

Credential incident procedure:

1. Revoke any GHCR PAT that was pasted into a terminal, issue, PR, chat, or
   transcript. Treat it as compromised even if it was not used.
2. Create a replacement with the minimum package scope needed for the
   operator action, store it only in the GitHub secret/registry credential
   store, and never put it in workflow YAML or shell history.
3. Re-run the public E2E pull using the immutable `sha-<full-sha>` tag and
   record the resulting platform digest in the release evidence.
4. Confirm the old token no longer authenticates and remove any local copies.

The repository can codify the tag and retention contract, but revocation,
package visibility, and registry retention are account-level controls and
must be checked in GitHub Packages before release.
