// Provides the SessionManager behind each subagent call: one-shot in memory or on disk, or retained per named thread.
import { randomUUID } from "node:crypto";
import { realpathSync, unlinkSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { SessionManager } from "@earendil-works/pi-coding-agent";
import { errorMessage, WorkflowError, WorkflowErrorCode } from "../errors.js";
/**
 * Named threads live as long as this object. Production creates one per
 * workflow invocation; embedders that reuse a WorkflowAgent deliberately choose
 * a longer thread lifetime.
 */
export class SessionManagers {
    options;
    threads = new Map();
    persistNoticeSent = false;
    constructor(options) {
        this.options = options;
    }
    /**
     * The manager for one call. A named thread reuses its manager, so the
     * conversation continues, and must keep the canonical cwd it started in.
     * Without a thread every call gets a fresh one-shot manager.
     */
    forCall(thread, cwd, onNotice) {
        if (!thread)
            return this.createOneShot(onNotice);
        // WorkflowAgent.run passes an already canonical cwd. Without one, keep the
        // runner cwd as given so SessionManager can still create it lazily.
        const threadCwd = cwd === undefined ? this.options.cwd : realpathSync(cwd);
        const existing = this.threads.get(thread);
        if (existing) {
            if (existing.cwd !== threadCwd) {
                throw new WorkflowError(`agent thread "${thread}" cannot change cwd from "${existing.cwd}" to "${threadCwd}"`, WorkflowErrorCode.SCRIPT_VALIDATION_ERROR, { recoverable: false });
            }
            return existing.manager;
        }
        const manager = this.createOneShot(onNotice);
        this.threads.set(thread, { manager, cwd: threadCwd });
        return manager;
    }
    /**
     * In memory by default. With persistence on, a file-backed session keyed by
     * the runner's project cwd (never a per-call worktree cwd), so worktree agents
     * still group under the project.
     *
     * SessionManager.create() only creates the directory; the SDK writes the file
     * lazily, with uncaught synchronous fs calls, on the first assistant message
     * deep inside session.prompt(). Probing writability up front lets a failure
     * (permissions, disk full) degrade this one agent to an in-memory session
     * instead of aborting it mid-run. Both that degrade and the first persisted
     * session, which writes full transcripts that may hold secrets, are reported
     * through onNotice so the run's log shows them.
     */
    createOneShot(onNotice) {
        if (!this.options.persist)
            return SessionManager.inMemory();
        try {
            const manager = SessionManager.create(this.options.cwd);
            // create() starts a session without lineage. Restart it with the host
            // session as parent before createAgentSession() writes the header.
            if (this.options.parentSessionFile) {
                manager.newSession({ parentSession: this.options.parentSessionFile });
            }
            assertDirWritable(manager.getSessionDir());
            if (!this.persistNoticeSent) {
                this.persistNoticeSent = true;
                onNotice?.(`persistAgentSessions is on: full subagent transcripts, which may include secrets, are written under ${manager.getSessionDir()}`);
            }
            return manager;
        }
        catch (error) {
            onNotice?.(`persistAgentSessions: could not persist this agent's session (${errorMessage(error)}); continuing in memory`);
            return SessionManager.inMemory();
        }
    }
}
/** Best-effort write probe: throws if the directory isn't actually writable. */
function assertDirWritable(dir) {
    const probePath = join(dir, `.write-probe-${randomUUID()}`);
    writeFileSync(probePath, "");
    unlinkSync(probePath);
}
