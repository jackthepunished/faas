/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * ADR-038 / Tier 3 / issue #197 B3.10-read half: the
 * post-mortem "what ran?" record for a single successful build.
 * Field names mirror the `build_provenance` table columns. Empty
 * strings indicate a column the populator hasn't filled yet —
 * `buildkit_version`, `railpack_version`, `base_digest`,
 * `runner_digest`, and `sbom_storage_key` are populated by
 * Phase 3 (cosign signer + syft SBOM), but the columns exist
 * today so Phase 3 is a zero-cost schema change.
 *
 */
export type BuildProvenanceResponse = {
  id: string;
  build_id: string;
  buildkit_version?: string;
  railpack_version?: string;
  base_digest?: string;
  /**
   * sha256 of the customer's source tarball (the cache lookup key).
   */
  source_sha256: string;
  source_url?: string;
  commit_sha?: string;
  /**
   * free / hobby / pro / scale — copied from the account at claim time.
   */
  plan: string;
  runner_digest?: string;
  /**
   * compute_node name (default `default-local` on the one-box).
   */
  builder_node_id: string;
  started_at: string;
  finished_at: string;
  /**
   * Phase 3 populator fills this from `syft` output. Empty string when not yet populated.
   */
  sbom_storage_key?: string | null;
};

