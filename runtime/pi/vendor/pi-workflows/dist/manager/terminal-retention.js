// Bounds how many settled runs keep their full state in the manager's memory.
import { TERMINAL_RUN_STATUSES } from "../run-persistence.js";
/** Settled runs kept in memory by default. Their records are already on disk, so a few suffice. */
export const DEFAULT_MAX_TERMINAL_RUNS_IN_MEMORY = 20;
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
export class TerminalRunRetention {
    /** Oldest first. */
    queue = [];
    runs;
    max;
    constructor(runs, max) {
        this.runs = runs;
        this.max = max;
    }
    /** Make a settled run eviction-eligible and evict the oldest beyond the limit. */
    record(runId, run) {
        this.queue.push({ runId, run });
        while (this.queue.length > this.max) {
            const oldest = this.queue.shift();
            if (oldest === undefined)
                break;
            const { runId: oldestId, run: settled } = oldest;
            if (this.runs.get(oldestId) !== settled)
                continue;
            if (settled.lease === undefined && TERMINAL_RUN_STATUSES.has(settled.status))
                this.runs.delete(oldestId);
        }
    }
}
