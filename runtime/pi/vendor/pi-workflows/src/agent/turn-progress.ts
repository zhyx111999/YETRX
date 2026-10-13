// Reports a running turn's history snapshots and token usage to the host: throttled while it streams, exact when it ends.
import type { AgentSessionEvent } from "@earendil-works/pi-coding-agent";
import { type AgentHistoryEntry, compactAgentHistory } from "../agent-history.js";
import { type AgentUsage, agentUsageEquals } from "../agent-usage.js";
import {
  type SessionUsageStats,
  subtractSessionUsageStats,
  usageFromSessionProgress,
  usageFromStats,
} from "./session-usage.js";

const HISTORY_EMIT_INTERVAL_MS = 250;
const LIVE_USAGE_EMIT_INTERVAL_MS = 250;

/** The host callbacks a turn reports to (a subset of AgentRunOptions). */
export interface TurnProgressCallbacks {
  onHistory?: (history: AgentHistoryEntry[]) => void;
  onUsageProgress?: (usage: AgentUsage) => void;
  onUsage?: (usage: AgentUsage) => void;
}

/** The session surface progress reporting reads. */
interface ProgressSession {
  readonly messages: unknown[];
  getSessionStats(): SessionUsageStats;
}

export class TurnProgress {
  private readonly session: ProgressSession;
  private readonly callbacks: TurnProgressCallbacks;
  /** Stats before this turn: a threaded session already holds earlier turns' usage. */
  private readonly baseline: SessionUsageStats | undefined;
  private lastHistoryEmit = 0;
  private lastUsageEmit = 0;
  private lastUsage: AgentUsage | undefined;

  constructor(session: ProgressSession, callbacks: TurnProgressCallbacks, baseline: SessionUsageStats | undefined) {
    this.session = session;
    this.callbacks = callbacks;
    this.baseline = baseline;
  }

  /** Whether the host asked for live progress, i.e. whether onEvent needs a subscription. */
  get live(): boolean {
    return Boolean(this.callbacks.onHistory || this.callbacks.onUsageProgress);
  }

  /** Session event listener: a throttled history snapshot, then live usage. */
  readonly onEvent = (event: AgentSessionEvent): void => {
    this.maybeEmitHistory();
    try {
      this.emitUsageProgress(event);
    } catch {
      // Usage progress is best-effort; never let stats failure interrupt the agent.
    }
  };

  /** The last history snapshot, sent when the turn ends. */
  emitFinalHistory(): void {
    try {
      this.emitHistory();
    } catch {
      // History is diagnostic only; never let it mask the real result/error.
    }
  }

  /** Report this turn's exact usage. Call before disposal, which tears down the stats. */
  reportFinalUsage(): void {
    if (!this.callbacks.onUsage) return;
    try {
      const usage = usageFromStats(subtractSessionUsageStats(this.session.getSessionStats(), this.baseline));
      if (usage) this.callbacks.onUsage(usage);
    } catch {
      // Usage is best-effort; never let stats failure mask the real result/error.
    }
  }

  private emitHistory(): void {
    this.callbacks.onHistory?.(compactAgentHistory(this.session.messages));
  }

  private maybeEmitHistory(): void {
    if (!this.callbacks.onHistory) return;
    const now = Date.now();
    if (now - this.lastHistoryEmit < HISTORY_EMIT_INTERVAL_MS) return;
    this.lastHistoryEmit = now;
    this.emitHistory();
  }

  private emitUsageProgress(event: AgentSessionEvent): void {
    if (!this.callbacks.onUsageProgress) {
      return;
    }
    const now = Date.now();
    // Throttle streaming deltas only; message boundaries always report.
    if (event.type === "message_update" && this.lastUsage && now - this.lastUsageEmit < LIVE_USAGE_EMIT_INTERVAL_MS) {
      return;
    }
    const usage = usageFromSessionProgress(
      subtractSessionUsageStats(this.session.getSessionStats(), this.baseline),
      event,
    );
    if (!usage || (this.lastUsage && agentUsageEquals(this.lastUsage, usage))) {
      return;
    }
    this.lastUsageEmit = now;
    this.lastUsage = usage;
    this.callbacks.onUsageProgress(usage);
  }
}
