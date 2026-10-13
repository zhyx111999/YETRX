// Coalesces each run's high-frequency writes into one trailing write.
/** Window for writes that come in bursts, such as finished agents' journal entries. */
export const BURST_PERSIST_MS = 400;
/**
 * Window for streaming progress (live usage and history of running agents).
 * Long, because every write appends the changed agent rows to the run's log;
 * short enough that a process that dies mid-run leaves recent spend and
 * history on disk.
 */
export const PROGRESS_PERSIST_MS = 10_000;
/**
 * At most one pending write per run. A write requested with a shorter window
 * than the pending one moves it earlier; a longer one joins it. Lifecycle
 * writes cancel the pending one and write at once, so a stale trailing write
 * can never land after them.
 */
export class PersistThrottle {
    pending = new Map();
    /** Write once within `windowMs`; the write reads live state, so one write serves every request before it. */
    schedule(runId, write, windowMs = BURST_PERSIST_MS) {
        const dueAt = Date.now() + windowMs;
        const current = this.pending.get(runId);
        if (current && current.dueAt <= dueAt)
            return;
        if (current)
            clearTimeout(current.timer);
        const timer = setTimeout(() => {
            this.pending.delete(runId);
            write();
        }, windowMs);
        // A pending progress write should never keep the process alive on its own.
        timer.unref?.();
        this.pending.set(runId, { timer, dueAt });
    }
    /** Drop the pending write: an immediate write or a deletion supersedes it. */
    cancel(runId) {
        const current = this.pending.get(runId);
        if (!current)
            return;
        clearTimeout(current.timer);
        this.pending.delete(runId);
    }
}
