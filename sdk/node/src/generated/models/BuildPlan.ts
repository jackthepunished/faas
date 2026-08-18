/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * Auto-detected build plan surfaced on DeploymentResponse (issue #961 / Mega-A PR-2). Same shape the CLI's pre-ship `Detected:` line prints; populated by apid via `pkg/markers.DetectFromTarball` against the spooled source tarball. Embedded on DeploymentResponse; never returned by a dedicated route.
 */
export type BuildPlan = {
  /**
   * Framework detected from the source tarball's top-level markers. `unknown` means no marker was found (monorepo / custom build); the wire renders this as `Detected: …, framework=unknown` rather than dropping the response.
   */
  framework: 'node' | 'python' | 'go' | 'docker' | 'unknown';
  /**
   * Runtime the app is pinned to (eg `node22`, `python312`). Echoed from app.Runtime. nil for apps without a runtime set (image deploys).
   */
  runtime?: string | null;
  /**
   * Framework version extracted from the detected marker (eg `package.json` `engines.node`, `requirements.txt` head pin). nil when the marker has no version or framework is `unknown`.
   */
  version?: string | null;
  /**
   * Entrypoint override (create-time only). nil when the customer did not supply one.
   */
  entrypoint?: string | null;
  /**
   * Listen-port override (create-time only). nil when the customer did not supply one.
   */
  port?: number | null;
  /**
   * App class from `app.Type` — `app` for plain apps, `function` for function rewrites (spec §4.2).
   */
  class?: 'app' | 'function';
};

