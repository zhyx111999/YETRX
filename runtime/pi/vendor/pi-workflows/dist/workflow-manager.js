/**
 * Workflow manager: owns every run's lifecycle. A run starts in the background,
 * executes through runWorkflow, and can be paused,
 * stopped, resumed, re-homed to another session, or deleted. Each execution
 * holds a cross-process lease on its run and writes the run record as it goes.
 */
import { EventEmitter } from "node:events";
import { isDeepStrictEqual } from "node:util";
import { DEFAULT_MAX_AGENTS } from "./config.js";
import { WorkflowError, WorkflowErrorCode } from "./errors.js";
import { abortForLifecycleControl } from "./manager/abort-control.js";
import { attachCheckpointResponse, checkpointAlreadyConsumed } from "./manager/checkpoint-response.js";
import { waitForPausedExecutionSettlement } from "./manager/execution-settlement.js";
import { LiveRuns } from "./manager/live-runs.js";
import { newManagedRun } from "./manager/new-run.js";
import { abortRecord, recoverStaleRuns, tryAcquireRunLease, writeAutoResumeAttempts } from "./manager/record-ops.js";
import { resumedRun } from "./manager/resume-state.js";
import { executeRun } from "./manager/run-execution.js";
import { initialRunRecord } from "./manager/run-record.js";
import { adoptRunsToSession, moveRunSession } from "./manager/session-owner.js";
import { taskListMoves } from "./manager/task-list-reuse.js";
import { DEFAULT_MAX_TERMINAL_RUNS_IN_MEMORY } from "./manager/terminal-retention.js";
import { createRunPersistence, sanitizeAutoResumeAttempts, } from "./run-persistence.js";
export class WorkflowManager extends EventEmitter {
    /**
     * In-memory runs. Running and paused runs always stay; a finished run stays
     * until evicted (see TerminalRunRetention). A missing entry means "read the
     * record" (listRuns/findRun), and resume() always reloads from disk.
     */
    runs = new Map();
    /** Guarded writes for `runs`: record writes, lease release, events, deletion. */
    live;
    /** Executions by managed run, so pause/resume can wait for settlement before overlapping. */
    executions = new WeakMap();
    persistence;
    cwd;
    concurrency;
    agent;
    mainModel;
    modelRegistry;
    /** The current pi session id; new runs are tagged with it and listRuns() filters by it. */
    sessionId;
    /** The current host session file; new runs record it as their parent. */
    sessionFile;
    maxAgents;
    agentTimeoutMs;
    agentRetries;
    toolsets;
    excludeSubagentTools;
    providerMiddlewareExtensions;
    persistAgentSessions;
    inheritMainModel;
    constructor(options = {}) {
        super();
        this.cwd = options.cwd ?? process.cwd();
        this.concurrency = options.concurrency ?? 8;
        this.agent = options.agent;
        this.mainModel = options.mainModel;
        this.modelRegistry = options.modelRegistry;
        this.sessionId = options.sessionId;
        this.sessionFile = options.sessionFile;
        this.maxAgents = options.maxAgents ?? DEFAULT_MAX_AGENTS;
        this.agentTimeoutMs = options.agentTimeoutMs ?? null;
        this.agentRetries = options.agentRetries ?? 0;
        this.toolsets = options.toolsets;
        this.excludeSubagentTools = options.excludeSubagentTools;
        this.providerMiddlewareExtensions = options.providerMiddlewareExtensions;
        this.persistAgentSessions = options.persistAgentSessions ?? false;
        this.inheritMainModel = options.inheritMainModel ?? false;
        this.persistence = createRunPersistence(this.cwd);
        this.live = new LiveRuns(this.runs, this.persistence, this, options.maxTerminalRunsInMemory ?? DEFAULT_MAX_TERMINAL_RUNS_IN_MEMORY);
        recoverStaleRuns(this.persistence);
    }
    /**
     * Refresh host configuration after Pi reloads the extension, keeping live
     * runs, leases, and listeners. Running executions keep the options they
     * started with; later runs and resumes use the new ones.
     */
    reconfigureAfterReload(options) {
        this.concurrency = options.concurrency ?? 8;
        this.maxAgents = options.maxAgents ?? DEFAULT_MAX_AGENTS;
        this.agentTimeoutMs = options.agentTimeoutMs ?? null;
        this.agentRetries = options.agentRetries ?? 0;
        this.toolsets = options.toolsets;
        this.excludeSubagentTools = options.excludeSubagentTools;
        this.providerMiddlewareExtensions = options.providerMiddlewareExtensions;
        this.persistAgentSessions = options.persistAgentSessions ?? false;
        this.inheritMainModel = options.inheritMainModel ?? false;
    }
    // ── Session and host binding ───────────────────────────────────────────────
    /** Bind to the current pi session (on session_start): new runs belong to it, and listRuns() shows only its runs. */
    setSessionId(id, sessionFile) {
        this.sessionId = id;
        this.sessionFile = sessionFile;
    }
    getSessionId() {
        return this.sessionId;
    }
    getSessionFile() {
        return this.sessionFile;
    }
    /** Project cwd this manager was constructed for (persistence + agent tools). */
    getCwd() {
        return this.cwd;
    }
    /** Set the session's main model (provider/id). Used to auto-tier explore agents. */
    setMainModel(spec) {
        this.mainModel = spec;
    }
    /** Set the host session's model registry so subagents resolve models consistently. */
    setModelRegistry(registry) {
        this.modelRegistry = registry;
    }
    /**
     * After a session replacement keeps this manager, move the work the previous
     * session owns to `sessionId`: running and paused runs, and any undelivered
     * result. Finished, delivered runs keep their session so history stays where
     * it ran; parent lineage never moves. Runs another session owns stay with it,
     * even when this process executes them or caches a stale copy. Returns how
     * many runs moved.
     */
    adoptLiveRunsToSession(sessionId, previousSessionId) {
        if (!sessionId)
            return 0;
        const prev = previousSessionId !== undefined ? previousSessionId : this.sessionId;
        return adoptRunsToSession(this.persistence, this.runs, sessionId, prev, (run) => this.live.dropIfDeleted(run));
    }
    // ── Starting runs ──────────────────────────────────────────────────────────
    /** Start a workflow in the background. Returns the run ID at once; `promise` settles with the run. */
    startInBackground(script, args, exec = {}) {
        const managed = this.registerNewRun(script, args, exec);
        try {
            this.persistence.save(initialRunRecord(managed));
        }
        catch (err) {
            this.live.releaseLease(managed);
            this.runs.delete(managed.runId);
            throw err;
        }
        this.emit("started", { runId: managed.runId });
        const promise = this.execute(managed, script, args, exec);
        // The execution records its outcome itself; this catch only prevents an
        // unhandled rejection. Callers still get the original promise.
        promise.catch(() => { });
        return { runId: managed.runId, promise };
    }
    /** Create a run, take its lease, and add it to `runs`. Throws when the lease is taken. */
    registerNewRun(script, args, exec) {
        const managed = newManagedRun(script, args, {
            sessionId: this.sessionId,
            sessionFile: this.sessionFile,
            toolset: exec.toolset,
        });
        const lease = this.persistence.acquireRunLease(managed.runId);
        if (!lease)
            throw new Error(`Could not acquire workflow run lease for ${managed.runId}`);
        managed.lease = lease;
        this.runs.set(managed.runId, managed);
        return managed;
    }
    /** Start one execution of `managed` with the manager's current settings and track it. */
    execute(managed, script, args, exec = {}) {
        const execution = executeRun(this.live, managed, script, args, exec, {
            runtime: {
                cwd: this.cwd,
                agent: this.agent,
                mainModel: this.mainModel,
                modelRegistry: this.modelRegistry,
                persistAgentSessions: this.persistAgentSessions,
                inheritMainModel: this.inheritMainModel,
                concurrency: this.concurrency,
                agentRetries: this.agentRetries,
                maxAgents: this.maxAgents,
                agentTimeoutMs: this.agentTimeoutMs,
                excludeTools: this.excludeSubagentTools,
                providerMiddlewareExtensions: this.providerMiddlewareExtensions,
            },
            toolsets: this.toolsets,
        });
        this.executions.set(managed, execution);
        return execution;
    }
    // ── Pause, checkpoint responses, resume, stop ─────────────────────────────
    /** Pause a running workflow. */
    pause(runId) {
        const managed = this.runs.get(runId);
        if (managed?.status !== "running")
            return false;
        managed.status = "paused";
        abortForLifecycleControl(managed, "pause");
        this.emit("paused", { runId });
        // Write the new state now, but keep the lease until the execution settles
        // and writes its exact final usage.
        this.live.persistRun(managed);
        return true;
    }
    /**
     * Attach a response to a waiting checkpoint; it is on disk before this
     * resolves. resume() still waits for the paused execution to settle, so
     * hosts attach first and resume on the "paused" event.
     */
    attachCheckpointResponse(runId, checkpointId, responseValue) {
        return attachCheckpointResponse(this.live, (run) => this.executions.get(run), runId, checkpointId, responseValue);
    }
    /**
     * Resume a paused, failed, or stopped run, or rerun a finished one with an
     * edit: journaled results replay for unchanged calls and the rest run live.
     * Returns false when there is nothing to resume.
     *
     * `opts.script` resumes with an edited script: calls replay up to the first
     * changed or inserted call. `opts.tasks` edits a task-list run, where every
     * unchanged task replays wherever it moved. Without either, the recorded
     * script and args run again (auto-resume, TUI resume).
     */
    async resume(runId, opts) {
        const active = this.runs.get(runId);
        if (active?.status === "running")
            return false;
        // A stopped or paused run may still be draining its last agents.
        const settlingExecution = active ? this.executions.get(active) : undefined;
        if (settlingExecution) {
            if (!(await waitForPausedExecutionSettlement(settlingExecution)))
                return false;
            if (this.runs.get(runId) !== active)
                return false;
        }
        const persisted = this.persistence.load(runId);
        if (!persisted) {
            this.live.dropIfDeleted(active);
            return false;
        }
        if (checkpointAlreadyConsumed(persisted, opts?.checkpointId))
            return true;
        if (opts?.onlyIf && !opts.onlyIf(persisted))
            return false;
        // A finished run has nothing left to continue; it reruns only with an edit.
        const edited = opts?.script !== undefined || opts?.tasks !== undefined;
        if (!persisted.script || (persisted.status === "completed" && !edited))
            return false;
        // Validated before the lease: nothing may throw once it is held.
        const taskMoves = opts?.tasks ? taskListMoves(runId, persisted, opts) : undefined;
        const lease = this.persistence.acquireRunLease(runId);
        if (!lease)
            return false;
        // Another process may have stopped, deleted, or advanced the run since the
        // first read. Re-read under the lease and give up on any change, rather
        // than overwrite newer journal, agent, or checkpoint data.
        let fresh;
        try {
            fresh = this.persistence.load(runId);
        }
        catch (error) {
            this.persistence.releaseRunLease(lease);
            throw error;
        }
        if (!isDeepStrictEqual(fresh, persisted)) {
            this.persistence.releaseRunLease(lease);
            return false;
        }
        const script = opts?.script ?? persisted.script;
        const args = opts?.tasks ?? persisted.args;
        const { managed, journal, priorTokenUsage } = resumedRun({
            runId,
            persisted,
            script,
            args,
            taskMoves,
            lease,
        });
        // An explicit resume takes delivery here; any resume claims a run nobody
        // owns. moveRunSession() never throws, as nothing may with the lease held.
        if (this.sessionId) {
            const claim = opts?.claimDelivery === true;
            moveRunSession(this.persistence, managed, this.sessionId, (owner) => claim || owner === undefined);
        }
        this.runs.set(runId, managed);
        // Write before notifying: renderers read lifecycle status from the record.
        this.live.persistRun(managed);
        const resumeJournal = new Map(journal.map((e) => [e.index, e]));
        this.emit("resumed", { runId });
        // The agent count needs no seed: the script replays from call 0 and agent()
        // counts replayed calls too, so maxAgents stays a cumulative cap.
        const execution = this.execute(managed, script, args, {
            resumeJournal,
            resumeCheckpoint: persisted.checkpoint?.status === "resuming" ? persisted.checkpoint : undefined,
            initialTokenUsage: priorTokenUsage,
        });
        void execution.catch(() => { });
        return true;
    }
    /**
     * Stop a running or paused workflow. The execution holding the run's lease
     * is aborted and the run written as aborted. Like pause(), it keeps the
     * lease: the execution's last write comes after the drain, and releasing
     * now would let another process resume the run and then lose that write.
     * Any other run is stopped through its record (see stopRecord()).
     */
    stop(runId) {
        const managed = this.runs.get(runId);
        if (!managed?.lease)
            return this.stopRecord(runId, managed);
        if (managed.status !== "running" && managed.status !== "paused")
            return false;
        this.markStopped(managed);
        abortForLifecycleControl(managed, "stop");
        this.emit("stopped", { runId });
        this.live.persistRun(managed);
        return true;
    }
    /**
     * Stop a run no execution here owns: one from an earlier session, or one
     * whose execution here has settled. Such a copy only caches the record,
     * which another process may since have resumed, finished, or deleted, so
     * the record decides; a run another live process executes cannot be leased.
     */
    stopRecord(runId, cached) {
        // A cheap read first, so an unknown or finished run takes no lock.
        const current = this.findRun(runId)?.status;
        if (current !== "running" && current !== "paused")
            return false;
        if (!abortRecord(this.persistence, runId, () => this.live.dropIfDeleted(cached)))
            return false;
        // No execution will settle the cached copy, so it may be evicted now.
        if (cached && this.live.isCurrent(cached)) {
            this.markStopped(cached);
            this.live.recordTerminalRun(cached);
        }
        this.emit("stopped", { runId });
        return true;
    }
    markStopped(managed) {
        managed.status = "aborted";
        managed.error ??= new WorkflowError("workflow aborted", WorkflowErrorCode.WORKFLOW_ABORTED, {
            recoverable: true,
        });
    }
    /**
     * Queue an instruction into every running agent of a live run that has this
     * label. Resolves to the number of sessions that received it: 0 when the run
     * is not running here or no agent with that label is mid-session.
     */
    async steerAgent(runId, label, text) {
        const managed = this.runs.get(runId);
        if (managed?.status !== "running")
            return 0;
        const targets = [...managed.steerable.values()].filter((agent) => agent.label === label);
        await Promise.all(targets.map((agent) => agent.steer(text)));
        return targets.length;
    }
    // ── Reading runs ───────────────────────────────────────────────────────────
    /** The live in-memory run, if this process has one. */
    getRun(runId) {
        return this.runs.get(runId);
    }
    /** The live snapshot of an in-memory run, or null. */
    getSnapshot(runId) {
        return this.runs.get(runId)?.snapshot ?? null;
    }
    /**
     * Every live in-memory run, regardless of session. Cross-session recovery
     * uses this, because listRuns() hides runs that belong to another session.
     */
    listLiveRuns() {
        return [...this.runs.values()];
    }
    /**
     * Run records for the navigator and task panel: only the bound session's
     * runs, or all of them when no session is bound.
     */
    listRuns() {
        const all = this.persistence.list();
        return this.sessionId ? all.filter((r) => r.sessionId === this.sessionId) : all;
    }
    /** All persisted runs regardless of session (used by cross-session recovery). */
    listAllRuns() {
        return this.persistence.list();
    }
    /**
     * One run's record, read now rather than from the list cache, in any session;
     * undefined when it does not exist. A settled local copy of a run that another
     * process deleted is dropped here, so every caller agrees the run is gone.
     */
    findRun(runId) {
        let run;
        try {
            run = this.persistence.loadPreview(runId);
        }
        catch {
            // A damaged record is unreadable, not deleted; list() skips it as well,
            // and /workflows rm still removes it by id.
            return undefined;
        }
        if (!run)
            this.live.dropIfDeleted(this.runs.get(runId));
        return run ?? undefined;
    }
    getPersistence() {
        return this.persistence;
    }
    // ── Deleting and record maintenance ────────────────────────────────────────
    /**
     * Delete a run and its files. A live run is aborted first so it stops
     * spending; its execution winds down on its own, and its late writes are
     * dropped because it is no longer current. Returns false when another
     * process holds the run's lease or the run does not exist. Never throws on
     * a lease probe failure.
     */
    deleteRun(runId) {
        const managed = this.runs.get(runId);
        if (managed && !managed.controller.signal.aborted)
            managed.controller.abort();
        // Hold a lease across the deletion; otherwise another process could take
        // the run and recreate its record with its next write. An entry that still
        // holds its execution's lease uses that one. Deleting the files removes the
        // lock too, so the release at the end is harmless.
        let heldLease;
        if (managed?.lease) {
            heldLease = managed.lease;
            managed.lease = undefined;
        }
        else {
            heldLease = tryAcquireRunLease(this.persistence, runId);
            if (!heldLease)
                return false;
        }
        this.live.remove(runId, managed);
        try {
            const deleted = this.persistence.delete(runId);
            // Watchers (e.g. /workflows watch) need the event to release their
            // listeners, even when the files were already gone.
            if (deleted || managed)
                this.emit("deleted", { runId });
            return deleted;
        }
        catch (error) {
            // Removing the head commits a deletion. If only the cleanup after it
            // failed, the run is gone all the same and watchers must hear it now.
            if (this.recordIsGone(runId))
                this.emit("deleted", { runId });
            throw error;
        }
        finally {
            this.persistence.releaseRunLease(heldLease);
        }
    }
    /** Whether the run has no record at all; a damaged record still counts as present. */
    recordIsGone(runId) {
        try {
            return this.persistence.loadPreview(runId) === null;
        }
        catch {
            return false;
        }
    }
    /**
     * Record the usage-limit scheduler's backoff counter. A run whose execution
     * holds the lease carries it into its next write; any other run's record is
     * merged under a fresh lease, skipped on contention because the owner writes
     * the record itself. Never write the field with a raw save: the next full
     * write would erase it.
     */
    recordAutoResumeAttempts(runId, attempts) {
        // NaN or negative values would break the scheduler's give-up cap and timers.
        if (sanitizeAutoResumeAttempts(attempts) === undefined)
            return;
        const managed = this.runs.get(runId);
        if (managed?.lease) {
            if ((managed.autoResumeAttempts ?? 0) === attempts)
                return;
            managed.autoResumeAttempts = attempts;
            this.live.persistRun(managed);
            return;
        }
        // Without a lease the local entry is only a cache: update just this field.
        const recorded = writeAutoResumeAttempts(this.persistence, runId, attempts, () => this.live.dropIfDeleted(managed));
        if (recorded && managed)
            managed.autoResumeAttempts = attempts;
    }
}
