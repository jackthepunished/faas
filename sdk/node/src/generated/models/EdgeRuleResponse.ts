/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { EdgeRuleCORSAction } from './EdgeRuleCORSAction.js';
import type { EdgeRuleHeadersAction } from './EdgeRuleHeadersAction.js';
import type { EdgeRuleIPAction } from './EdgeRuleIPAction.js';
import type { EdgeRuleJWTAction } from './EdgeRuleJWTAction.js';
import type { EdgeRuleRedirectAction } from './EdgeRuleRedirectAction.js';
import type { EdgeRuleRewriteAction } from './EdgeRuleRewriteAction.js';
import type { EdgeRuleRouteAction } from './EdgeRuleRouteAction.js';
/**
 * A customer-configurable edge rule. The `action` blob is a
 * kind-tagged union — the shape varies by `kind`. See
 * CreateEdgeRuleRequest.action for the per-kind shape.
 *
 */
export type EdgeRuleResponse = {
  id: string;
  account_id: string;
  app_id: string;
  /**
   * Host glob. `*` matches any; `*.example.com` matches any subdomain.
   */
  match_host: string;
  /**
   * Path glob. Trailing `*` matches anything beneath.
   */
  match_path: string;
  /**
   * Empty array = match any method.
   */
  match_methods: Array<string>;
  priority: number;
  enabled: boolean;
  kind: 'route' | 'rewrite' | 'redirect' | 'headers' | 'cors' | 'jwt' | 'ip';
  /**
   * Kind-tagged union — shape varies by `kind`.
   */
  action: (EdgeRuleRouteAction | EdgeRuleRewriteAction | EdgeRuleRedirectAction | EdgeRuleHeadersAction | EdgeRuleCORSAction | EdgeRuleJWTAction | EdgeRuleIPAction);
  created_at: string;
  updated_at: string;
};

