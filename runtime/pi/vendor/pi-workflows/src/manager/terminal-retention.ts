// Bounds how many settled runs keep their full state in the manager's memory.

import { type RunStatus, TERMINAL_RUN_STATUSES } from "../run-persistence.js";

/** Settled runs kept in memory by default. Their records are already on disk, so a few suffice. */
export const DEFAULT_MAX_TERMINAL_RUNS_IN_MEMORY = 20;

/** What eviction needs to know about an in-memory run. */
export interface RetainedRun {
  status: RunStatus;
  /** Held while an execution still owns the run; such a run is never evicted. */
  lease?: unknown;
}

/**
 * FIFO eviction of settled (completed/failed/aborted) runs from the manager's
 * run map. A run is recorded only after its execution has persisted its final
 * state and released its lease; running and paused runs are never recorded.
 * The newest few stay so listeners of the final event still see the live
 * object. Evicted runs are read from disk like any run this process never had.
 *
 * Each queue entry names the exact in-memory run that settled. A resume puts a
 * new run object under the same id, so an older entry no longer matches and can
 * never evict the newer execution, even one that a stop() already marked aborted
 * while it still drains and holds the lease.
 */
export class TerminalRunRetention<R extends RetainedRun> {
  /** Oldest first. */
  private readonly queue: Array<{ runId: string; run: R }> = [];
  private readonly runs: Map<string, R>;
  private readonly max: number;

  constructor(runs: Map<string, R>, max: number) {
    this.runs = runs;
    this.max = max;
  }

  /** Make a settled run eviction-eligible and evict the oldest beyond the limit. */
  record(runId: string, run: R): void {
    this.queue.push({ runId, run });
    while (this.queue.length > this.max) {
      const oldest = this.queue.shift();
      if (oldest === undefined) break;
      const { runId: oldestId, run: settled } = oldest;
      if (this.runs.get(oldestId) !== settled) continue;
      if (settled.lease === undefined && TERMINAL_RUN_STATUSES.has(settled.status)) this.runs.delete(oldestId);
    }
  }
}
