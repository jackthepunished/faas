/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * One execution of a cron: when it fired, how long it ran, and how it ended. Backed by the underlying invocations row, projected so callers need not compute a duration or interpret raw state.
 */
export type CronRun = {
  id: string;
  /**
   * When the cron fired (the invocation's created_at), not when the app began executing.
   */
  started_at: string;
  /**
   * When the run reached a terminal state; null while still in flight.
   */
  completed_at?: string | null;
  /**
   * completed_at - started_at in milliseconds, computed server-side. Null while the run is still in flight.
   */
  duration_ms?: number | null;
  /**
   * Normalized result. `timeout` means the dispatch exceeded its deadline; `dead_letter` means the retry budget was exhausted; `running` means the run has not reached a terminal state yet. Branch on this, never on `error`.
   */
  outcome: 'success' | 'failed' | 'timeout' | 'dead_letter' | 'running';
  /**
   * Dispatch attempts for this run; greater than 1 means it was retried.
   */
  attempts: number;
  /**
   * The instance that served the run; null if the fire never reached one.
   */
  instance_id?: string | null;
  /**
   * Operator-facing failure text. Unstructured and unversioned — do not parse it.
   */
  error?: string | null;
};

