// Lease-guarded edits to run records that no execution in this process owns.
import { WorkflowErrorCode } from "../errors.js";
import { settleNonTerminalPersistedAgents } from "../run-persistence.js";
import { runSummary } from "../run-record-store.js";
/** A lease for `runId`, or undefined when it is held elsewhere or the probe fails. */
export function tryAcquireRunLease(persistence, runId) {
    try {
        return persistence.acquireRunLease(runId) ?? undefined;
    }
    catch {
        return undefined;
    }
}
/**
 * At startup, a record still marked running belongs to a process that died
 * mid-run. Pause it, never fail it, so its journal survives and resume() can
 * replay the finished calls. The same applies to a paused record with agents
 * still marked active.
 */
export function recoverStaleRuns(persistence) {
    try {
        for (const p of persistence.list()) {
            const staleRunning = p.status === "running";
            const pausedWithInterruptedAgents = p.status === "paused" && runSummary(p).active > 0;
            if (!staleRunning && !pausedWithInterruptedAgents)
                continue;
            const lease = persistence.acquireRunLease(p.runId);
            if (!lease)
                continue;
            try {
                persistence.recoverInterrupted(p.runId);
            }
            finally {
                persistence.releaseRunLease(lease);
            }
        }
    }
    catch {
        // Recovery is best-effort; it must never block manager construction.
    }
}
/**
 * Mark a running or paused record aborted under a fresh lease. False when the
 * lease is held elsewhere, or the record is gone (after `onMissing`, still under
 * the lease) or no longer active.
 */
export function abortRecord(persistence, runId, onMissing) {
    const lease = persistence.acquireRunLease(runId);
    if (!lease)
        return false;
    try {
        const persisted = persistence.load(runId);
        if (!persisted) {
            onMissing();
            return false;
        }
        if (persisted.status !== "running" && persisted.status !== "paused")
            return false;
        const endedAt = new Date().toISOString();
        const errorMessage = persisted.error ?? "workflow aborted";
        const errorCode = persisted.errorCode ?? WorkflowErrorCode.WORKFLOW_ABORTED;
        persistence.save({
            ...persisted,
            status: "aborted",
            updatedAt: endedAt,
            completedAt: persisted.completedAt ?? endedAt,
            error: errorMessage,
            errorCode,
            agents: settleNonTerminalPersistedAgents(persisted.agents, "aborted", { message: errorMessage, code: errorCode }, endedAt),
        });
        return true;
    }
    finally {
        persistence.releaseRunLease(lease);
    }
}
/**
 * Merge the auto-resume counter into a record under a fresh lease. True when
 * the record now holds `attempts`. Skipped on contention, because the owner of
 * the lease writes the record itself; `onMissing` runs, under the lease, when
 * the record is gone.
 */
export function writeAutoResumeAttempts(persistence, runId, attempts, onMissing) {
    const lease = persistence.acquireRunLease(runId);
    if (!lease)
        return false;
    try {
        const current = persistence.loadPreview(runId);
        if (!current) {
            onMissing();
            return false;
        }
        if ((current.autoResumeAttempts ?? 0) === attempts)
            return true;
        return persistence.updateMetadata(runId, { autoResumeAttempts: attempts });
    }
    finally {
        persistence.releaseRunLease(lease);
    }
}
