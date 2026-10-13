import { compactAgentHistory } from "../agent-history.js";
import { agentUsageEquals } from "../agent-usage.js";
import { subtractSessionUsageStats, usageFromSessionProgress, usageFromStats, } from "./session-usage.js";
const HISTORY_EMIT_INTERVAL_MS = 250;
const LIVE_USAGE_EMIT_INTERVAL_MS = 250;
export class TurnProgress {
    session;
    callbacks;
    /** Stats before this turn: a threaded session already holds earlier turns' usage. */
    baseline;
    lastHistoryEmit = 0;
    lastUsageEmit = 0;
    lastUsage;
    constructor(session, callbacks, baseline) {
        this.session = session;
        this.callbacks = callbacks;
        this.baseline = baseline;
    }
    /** Whether the host asked for live progress, i.e. whether onEvent needs a subscription. */
    get live() {
        return Boolean(this.callbacks.onHistory || this.callbacks.onUsageProgress);
    }
    /** Session event listener: a throttled history snapshot, then live usage. */
    onEvent = (event) => {
        this.maybeEmitHistory();
        try {
            this.emitUsageProgress(event);
        }
        catch {
            // Usage progress is best-effort; never let stats failure interrupt the agent.
        }
    };
    /** The last history snapshot, sent when the turn ends. */
    emitFinalHistory() {
        try {
            this.emitHistory();
        }
        catch {
            // History is diagnostic only; never let it mask the real result/error.
        }
    }
    /** Report this turn's exact usage. Call before disposal, which tears down the stats. */
    reportFinalUsage() {
        if (!this.callbacks.onUsage)
            return;
        try {
            const usage = usageFromStats(subtractSessionUsageStats(this.session.getSessionStats(), this.baseline));
            if (usage)
                this.callbacks.onUsage(usage);
        }
        catch {
            // Usage is best-effort; never let stats failure mask the real result/error.
        }
    }
    emitHistory() {
        this.callbacks.onHistory?.(compactAgentHistory(this.session.messages));
    }
    maybeEmitHistory() {
        if (!this.callbacks.onHistory)
            return;
        const now = Date.now();
        if (now - this.lastHistoryEmit < HISTORY_EMIT_INTERVAL_MS)
            return;
        this.lastHistoryEmit = now;
        this.emitHistory();
    }
    emitUsageProgress(event) {
        if (!this.callbacks.onUsageProgress) {
            return;
        }
        const now = Date.now();
        // Throttle streaming deltas only; message boundaries always report.
        if (event.type === "message_update" && this.lastUsage && now - this.lastUsageEmit < LIVE_USAGE_EMIT_INTERVAL_MS) {
            return;
        }
        const usage = usageFromSessionProgress(subtractSessionUsageStats(this.session.getSessionStats(), this.baseline), event);
        if (!usage || (this.lastUsage && agentUsageEquals(this.lastUsage, usage))) {
            return;
        }
        this.lastUsageEmit = now;
        this.lastUsage = usage;
        this.callbacks.onUsageProgress(usage);
    }
}
