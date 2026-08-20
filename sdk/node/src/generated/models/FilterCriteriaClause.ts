/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { FilterCriteriaOp } from './FilterCriteriaOp.js';
/**
 * One filter clause. The top-level FilterCriteria carries
 * `$or`, `$and`, and `payload` arrays; each clause here
 * is one primitive (eq / neq / exists / jsonpath) or a
 * nested `$or` / `$and` for compound logic.
 *
 */
export type FilterCriteriaClause = {
  op: FilterCriteriaOp;
  /**
   * Header key for eq / neq / exists on the headers map.
   */
  field?: string;
  /**
   * Equality target for eq / neq; jsonpath expected type for jsonpath.
   */
  value?: any;
  /**
   * Jsonpath expression for op=jsonpath. Evaluated against the record payload.
   */
  path?: string;
  /**
   * Nested compound clauses for op=$or / $and.
   */
  clauses?: Array<FilterCriteriaClause>;
};

