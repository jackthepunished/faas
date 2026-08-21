# Release manifest anchor

The signed daemon bundle must be tied to the exact deployment manifest that
was rendered and applied to the production fleet. The example manifest under
`deploy/manifest/examples/` is illustrative only and must never be used as a
release anchor.

Before pushing a release tag:

1. Render and validate the real fleet manifest through the normal deployment
   workflow. Keep the canonical YAML bytes unchanged after hashing.
2. Compute its digest:

   ```sh
   sha256sum /path/to/production-manifest.yaml
   ```

3. Set the repository variable `GREGALE_RELEASE_MANIFEST_HASH` to the result
   with the `sha256:` prefix. With `gh`:

   ```sh
   gh variable set GREGALE_RELEASE_MANIFEST_HASH \
     --body "sha256:<64 lowercase hex characters>"
   ```

The daemon release job fails closed when this variable is missing or has the
wrong shape. The resulting `release-manifest.json` carries that same digest;
`gregalectl release install` and `gregalectl doctor --deep` compare it with
the node's recorded release membership.

This variable is configuration, not a secret. Do not substitute the hash of
`deploy/manifest/examples/splitbox.example.yaml`, a temporary `live-e2e-*`
release, or a hand-edited copy of the production manifest.
