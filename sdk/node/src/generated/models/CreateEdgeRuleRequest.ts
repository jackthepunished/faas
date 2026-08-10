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
 * Create an edge rule on an app.
 */
export type CreateEdgeRuleRequest = {
  match_host: string;
  match_path?: string;
  match_methods?: Array<string>;
  priority?: number;
  enabled?: boolean;
  kind: 'route' | 'rewrite' | 'redirect' | 'headers' | 'cors' | 'jwt' | 'ip';
  /**
   * Kind-tagged action body — shape depends on `kind`.
   */
  action: (EdgeRuleRouteAction | EdgeRuleRewriteAction | EdgeRuleRedirectAction | EdgeRuleHeadersAction | EdgeRuleCORSAction | EdgeRuleJWTAction | EdgeRuleIPAction);
};

