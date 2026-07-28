/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { InstallBindRequest } from '../models/InstallBindRequest.js';
import type { InstallBindResponse } from '../models/InstallBindResponse.js';
import type { RepoResponse } from '../models/RepoResponse.js';
import type { CancelablePromise } from '../core/CancelablePromise.js';
import { OpenAPI } from '../core/OpenAPI.js';
import { request as __request } from '../core/request.js';
export class GithubService {
  /**
   * List repos the user's GitHub App installation can see.
   * Cookie-session-authenticated (NOT API-key). Hydrates the
   * dashboard bind picker's repo dropdown. Returns the repos
   * visible to the user's GitHub App installation — githubd
   * resolves the per-install token from the session's account.
   *
   * §11 anti-takeover: the handler re-runs
   * githubd.VerifyInstallation with the session's github_login
   * as `expected_login` before listing. Mismatch → 403 forged.
   *
   * @returns RepoResponse Repos the install can see.
   * @throws ApiError
   */
  public static listInstallableRepos({
    requestBody,
  }: {
    requestBody: InstallBindRequest,
  }): CancelablePromise<Array<RepoResponse>> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/v1/install/repos/list',
      body: requestBody,
      mediaType: 'application/json',
      errors: {
        400: `code: invalid_request — missing/malformed body or installation_id.`,
        403: `\`forged\` if the install's account.login differs from
        the session's github_login; \`github_login_required\` if
        the user hasn't completed /v1/auth/github. Returned for
        POST /v1/install/repos/list — the bind picker refuses
        to list repos of an install the user can't prove they own.
        `,
        502: `code: github_unreachable — githubd could not reach api.github.com; retry in a minute.`,
        503: `code: githubd_not_ready — githubd is not wired on this host (M7.5 slices 7-8).`,
      },
    });
  }
  /**
   * Persist the (account, app, installation, repo, branch) bind row.
   * Cookie-session-authenticated (NOT API-key). Persists the
   * GitHub install binding via githubd.BindAppRepo (which
   * writes through to pkg/state.PgStore.UpsertGithubInstallBinding
   * per PR-B).
   *
   * §11 anti-takeover: the handler re-runs
   * githubd.VerifyInstallation with the session's github_login
   * as `expected_login` before persisting. Mismatch → 403 forged.
   * Empty github_login → 403 github_login_required.
   *
   * On success emits `auth.install.bound` for the audit trail.
   *
   * @returns InstallBindResponse Bind persisted.
   * @throws ApiError
   */
  public static bindAppInstall({
    slug,
    requestBody,
  }: {
    /**
     * App slug. Lowercase letters, digits, hyphens; must start and end with alnum.
     */
    slug: string,
    requestBody: InstallBindRequest,
  }): CancelablePromise<InstallBindResponse> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/v1/apps/{slug}/install/bind',
      path: {
        'slug': slug,
      },
      body: requestBody,
      mediaType: 'application/json',
      errors: {
        400: `code: invalid_request — missing/malformed body, installation_id, or repo_full_name.`,
        403: `\`forged\` if the install's account.login differs from
        the session's github_login; \`github_login_required\` if
        the user hasn't completed /v1/auth/github. Returned for
        POST /v1/apps/{slug}/install/bind — the bind refuses to
        persist a row for an install the user can't prove they own.
        `,
        404: `code: not_found`,
        502: `code: github_unreachable — githubd could not reach api.github.com when persisting the bind; retry in a minute.`,
      },
    });
  }
}
