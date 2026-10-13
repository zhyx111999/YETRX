/** Holds one run's mutable state and the shared services that every workflow global uses. */
import { WorkflowError, WorkflowErrorCode, workflowAbortedError, } from "../errors.js";
import { CallSequence } from "./call-position.js";
import { createLimiter } from "./run-limits.js";
import { createScriptCalls } from "./script-calls.js";
export function createRuntimeState(meta, options, concurrency) {
    // The first declared phase is the default, so agents started before any
    // phase() call group under it instead of an orphan "(no phase)" bucket.
    const firstPhase = meta.phases?.[0]?.title;
    return {
        logs: [],
        phases: firstPhase ? [firstPhase] : [],
        currentPhase: firstPhase,
        scriptCalls: CallSequence.forScript(),
        journalMissed: false,
        replayedIndices: new Set(),
        limiter: createLimiter(concurrency),
        // Not seeded on resume: the script replays from call 0 and every replayed
        // call counts again, so maxAgents stays a cumulative cap on its own.
        agentCount: 0,
        // Seeded on resume because replayed calls deliberately commit no usage.
        tokenUsage: options.initialTokenUsage
            ? { ...options.initialTokenUsage }
            : { input: 0, output: 0, total: 0, cost: 0, cacheRead: 0, cacheWrite: 0 },
        agentCallbacksClosed: false,
        pendingUsageFinalizers: new Set(),
        activeCheckpointId: options.resumeCheckpoint?.checkpointId ?? null,
        activeCheckpointResponse: options.resumeCheckpoint ?? null,
        seenCheckpointIds: new Set(),
        runFatalController: new AbortController(),
        calls: createScriptCalls(),
        activeThreads: new Set(),
        resumeBarrierReached: false,
    };
}
export function createRunContext(setup) {
    const { options, state, logger, maxAgents, runId } = setup;
    const log = (message) => {
        const text = String(message);
        state.logs.push(text);
        logger.log(text);
    };
    // Both abort sources go through this one check, so no call site can forget the run-fatal seal.
    const isAborted = () => Boolean(options.signal?.aborted || state.runFatalController.signal.aborted);
    const throwIfAborted = () => {
        if (!options.signal?.aborted && state.checkpointSuspension)
            throw state.checkpointSuspension;
        if (isAborted())
            throw workflowAbortedError();
    };
    const agentLimitError = () => new WorkflowError(`Agent limit exceeded (${state.agentCount}/${maxAgents}). Raise defaultMaxAgents in the workflow settings, reload, then resume run "${runId}" — the journaled prefix replays free.`, WorkflowErrorCode.AGENT_LIMIT_EXCEEDED, { recoverable: false });
    const ensureAgentCapacity = () => {
        if (state.agentCount >= maxAgents)
            throw agentLimitError();
    };
    // In a script run the first miss ends the unchanged prefix: entries not
    // replayed by then describe calls that now run after a changed one, so the
    // caller drops them instead of keeping them for a later resume to replay.
    const noteJournalMiss = () => {
        if (state.journalMissed)
            return;
        state.journalMissed = true;
        if (!options.independentCalls && options.resumeJournal?.size) {
            options.onJournalPrefixEnd?.([...state.replayedIndices]);
        }
    };
    const replayable = (index, hash, passOver) => {
        // A thread's conversation cannot be replayed, so a thread call ends replay for the run.
        if (state.resumeBarrierReached)
            return undefined;
        const entry = options.resumeJournal?.get(index);
        if (entry?.hash !== hash) {
            noteJournalMiss();
            return undefined;
        }
        if (passOver?.(entry))
            return undefined;
        if (state.journalMissed && !options.independentCalls)
            return undefined;
        state.replayedIndices.add(index);
        return entry;
    };
    return {
        ...setup,
        log,
        isAborted,
        throwIfAborted,
        agentLimitError,
        ensureAgentCapacity,
        replayable,
    };
}
