// Runs one workflow subagent: picks its tools, model, and session, prompts it, and returns its answer.
import { realpathSync } from "node:fs";
import {
  type CreateAgentSessionOptions,
  createCodingTools,
  type DefaultResourceLoader,
  getAgentDir,
  type ModelRegistry,
  type SessionManager,
  type ToolDefinition,
} from "@earendil-works/pi-coding-agent";
import type { Static, TSchema } from "typebox";
import { listAvailableModels } from "./agent/available-models.js";
import { type ChildSession, openChildSession } from "./agent/child-session.js";
import { ensureFallbackRegistry } from "./agent/model-registry.js";
import { AgentModelSelector } from "./agent/model-selection.js";
import { SharedResourceLoaders } from "./agent/resource-loaders.js";
import { SessionManagers } from "./agent/session-managers.js";
import { buildSubagentTools, subagentExcludedTools } from "./agent/session-tools.js";
import { TurnProgress } from "./agent/turn-progress.js";
import { assertTurnCompleted, finalAnswer, resolveStructuredOutput } from "./agent/turn-result.js";
import { assertProcessesStartIn } from "./agent/windows-cwd.js";
import type { AgentHistoryEntry } from "./agent-history.js";
import type { AgentUsage } from "./agent-usage.js";
import { WorkflowError, WorkflowErrorCode, workflowAbortedError } from "./errors.js";
import {
  canonicalModelSpec,
  formatModelSpecWithThinking,
  type ModelThinkingLevel,
  validateThinkingLevel,
} from "./model-spec.js";
import { loadModelTierConfig, type ModelTierConfig } from "./model-tier-config.js";
import type { StructuredOutputCapture } from "./structured-output.js";

/** Child sessions load no host extensions unless explicitly opted in. */
export const DEFAULT_PROVIDER_MIDDLEWARE_EXTENSIONS: readonly string[] = Object.freeze([]);

export interface WorkflowAgentOptions {
  cwd?: string;
  /** Extra tools available to the subagent in addition to the structured output tool. */
  tools?: ToolDefinition[];
  /**
   * Extra tool names to deny in the subagent session, on top of the always-denied
   * orchestration tools. Lets the host exclude other recursive-orchestration
   * tools it registers (e.g. a pi-subagents tool) so a subagent cannot fan out
   * through them either.
   */
  excludeTools?: string[];
  /**
   * Trusted provider/auth middleware extension names allowed in child sessions.
   * Defaults to [] (no host extensions). Recursive orchestration stays excluded.
   */
  providerMiddlewareExtensions?: string[];
  /**
   * Override createAgentSession dependencies (model, settingsManager, resourceLoader, etc.).
   * An explicit per-call cwd and the computed agent identity remain authoritative.
   */
  session?: Partial<CreateAgentSessionOptions>;
  /** Extra system guidance prepended to every subagent task. */
  instructions?: string;
  /**
   * The session's main model (`provider/modelId`). Used as a fallback when
   * resolving opts.tier and no model-tiers.json config exists, and as the
   * routing target for untagged agents when `inheritMainModel` is on.
   * Without it, `{ tier: "small" }` with no saved config would fall through to
   * the session default.
   */
  mainModel?: string;
  /**
   * When true, untagged agents (no `model`, no `tier`) inherit `mainModel` —
   * the orchestrating session's model as of run start — instead of the
   * implicit medium tier (when configured) or the settings default. Mirrors
   * the inheritMainModel user setting; explicit model/tier tags are
   * unaffected, and an unavailable inherited model degrades to the settings
   * default with a run-visible warning instead of throwing.
   */
  inheritMainModel?: boolean;
  /**
   * Shared model registry from the host Pi session. When provided, subagents
   * resolve tier/model specs against the same registry the main session uses,
   * including dynamically-registered providers such as ollama-cloud. Without
   * this, the agent builds an isolated registry from disk and may miss models
   * that are only available via extension registration.
   */
  modelRegistry?: ModelRegistry;
  /** Persisted host session file used as the parent of persistent child sessions. */
  parentSessionFile?: string;
  /**
   * Persist each subagent transcript as a real pi session file under the
   * standard sessions directory (keyed by the runner's project cwd), instead
   * of an in-memory session that is discarded when the run ends. Default: false.
   */
  persistAgentSessions?: boolean;
}

export interface AgentRunOptions<TSchemaDef extends TSchema | undefined = undefined> {
  label?: string;
  /**
   * Display name recorded on the persisted session (session_info entry) when
   * `persistAgentSessions` is enabled, so transcripts are identifiable in
   * session pickers (e.g. `workflow:<runId> <label>`). Ignored for in-memory
   * sessions or when an explicit session.sessionManager override is injected.
   */
  sessionName?: string;
  schema?: TSchemaDef;
  tools?: ToolDefinition[];
  instructions?: string;
  signal?: AbortSignal;
  /**
   * Called as soon as the child SessionManager is created, before prompting.
   * The file is absent for in-memory sessions (and for a persistence fallback).
   */
  onSessionCreated?: (session: { sessionId: string; sessionFile?: string }) => void;
  /** Called once before disposal with exact cumulative provider usage, when reported. */
  onUsage?: (usage: AgentUsage) => void;
  /**
   * Called with cumulative progress while the subagent runs. The current
   * streaming response uses an output-token estimate until the provider's exact
   * terminal usage replaces it.
   */
  onUsageProgress?: (usage: AgentUsage) => void;
  /**
   * Model spec for this subagent: either `provider/modelId` (unambiguous) or a
   * bare `modelId`, parsed with the same grammar as Pi CLI's `--model`. When it
   * can't be resolved to a known model, `run()` throws MODEL_NOT_FOUND rather
   * than silently substituting the session default — a wrong-model run would
   * otherwise look successful while quietly answering with different (or
   * unauthenticated) weights. When omitted, the session default applies.
   */
  model?: string;
  /**
   * Pi thinking level. Used when `model` has no `:thinking` suffix.
   * A model-id suffix still wins.
   */
  thinking?: ModelThinkingLevel;
  /**
   * Model tier name (e.g. "small", "medium", "big"), resolved from the user's
   * model-tiers.json when no explicit `model` is given. With no
   * model-tiers.json saved, the session's main model stands in. A tier the
   * saved file does not define, or one that resolves to an unavailable spec,
   * throws MODEL_NOT_FOUND naming the tier, just like an explicit `model`.
   * Only the implicit route of an untagged agent degrades instead (see
   * onNotice), because the script never asked for that model.
   */
  tier?: string;
  /**
   * Called with the resolved model id once known (for display/telemetry). When
   * no spec resolved (settings default, implicit-route degrade, or an empty
   * tier) it fires after session creation with the session's real model, so
   * the agent never keeps displaying the pre-resolution mainModel guess.
   */
  onModelResolved?: (modelId: string) => void;
  /**
   * Called with a notice the run's own log should record instead of raw
   * console output: an untagged agent's implicit model route degrading to the
   * session default, a tier used with no model-tiers.json saved, the model
   * resolver's warning about a spec, a persisted-session warning or degrade,
   * or a schema answer recovered from prose. Each kind of model degrade and
   * the persisted-session warning are reported once per WorkflowAgent instance.
   */
  onNotice?: (message: string) => void;
  /** Called with a compact snapshot of this subagent's message/tool history. */
  onHistory?: (history: AgentHistoryEntry[]) => void;
  /**
   * Called once the child session can take instructions, with a function that
   * queues one into it (AgentSession.steer). The returned function runs before
   * the session is disposed, so a host never steers a finished session.
   */
  registerSteer?: (steer: (text: string) => Promise<void>) => () => void;
  /** Run this agent in a different working directory (e.g. an isolated worktree). */
  cwd?: string;
  /**
   * Restrict the subagent's coding tools to these names (an agentType
   * definition's `tools` allowlist). Undefined = all coding tools. The
   * structured_output tool is always added after this filter, so a schema
   * still works under a restrictive allowlist.
   */
  toolNames?: string[];
  /** Remove these coding-tool names after the allowlist (an agentType `disallowedTools` denylist). */
  disallowedToolNames?: string[];
  /**
   * With `schema`: how many extra repair turns to allow if the model finishes
   * without calling structured_output. Each retry re-prompts (tools restricted to
   * structured_output) before falling back to strict prose extraction. Default 2.
   */
  maxSchemaRetries?: number;
  /**
   * Tools that are always injected AFTER the tool-policy filter (`toolNames` /
   * `disallowedToolNames`), so they are available even under a restrictive
   * allowlist. Used by the workflow runtime to inject shared-store tools into
   * every agent regardless of its agentType definition.
   */
  systemTools?: ToolDefinition[];
  /**
   * Per-run model registry override. Takes precedence over the constructor's
   * `modelRegistry` for both model resolution and this run's session, which
   * otherwise fall back to the constructor's registry, then a lazily-built disk
   * registry.
   */
  modelRegistry?: ModelRegistry;
  /** Re-enter a named conversation retained by this WorkflowAgent instance. */
  thread?: string;
}

export type AgentRunResult<TSchemaDef extends TSchema | undefined> = TSchemaDef extends TSchema
  ? Static<TSchemaDef>
  : string;

/**
 * Runs subagents for one workflow invocation. Production creates one instance
 * per run, so per-instance caches (tier config, resource loaders, threads) live
 * exactly as long as the run.
 */
export class WorkflowAgent {
  private readonly cwd: string;
  private readonly baseTools: ToolDefinition[];
  /** Extra subagent tool-name denylist, merged with the always-on defaults. */
  private readonly excludeTools: string[];
  private readonly sessionOptions: Partial<CreateAgentSessionOptions>;
  private readonly persistAgentSessions: boolean;
  private readonly instructions?: string;
  /** Shared registry from the host session, when provided. */
  private readonly sharedRegistry?: ModelRegistry;
  /** Lazily built once; shares the SDK's agentDir/auth so resolved models are authed. */
  private registry?: ModelRegistry;
  /** Memoized model-tiers snapshot, boxed so a missing config (null) differs from "not read yet". */
  private tierConfigBox?: { value: ModelTierConfig | null };
  private readonly models: AgentModelSelector;
  private readonly resourceLoaders: SharedResourceLoaders;
  private readonly sessionManagers: SessionManagers;
  private readonly activeThreads = new Set<string>();
  /** Unique per-instance identity: agent ids must never collide across WorkflowAgent instances. */

  constructor(options: WorkflowAgentOptions = {}) {
    this.cwd = options.cwd ?? process.cwd();
    this.baseTools = options.tools ?? createCodingTools(this.cwd);
    this.excludeTools = options.excludeTools ?? [];
    this.sessionOptions = options.session ?? {};
    this.persistAgentSessions = options.persistAgentSessions ?? false;
    this.instructions = options.instructions;
    this.sharedRegistry = options.modelRegistry;
    this.models = new AgentModelSelector({
      mainModel: options.mainModel,
      inheritMainModel: options.inheritMainModel ?? false,
      loadTierConfig: () => this.loadTierConfig(),
      listModels: listAvailableModels,
    });
    this.resourceLoaders = new SharedResourceLoaders(
      [...(options.providerMiddlewareExtensions ?? DEFAULT_PROVIDER_MIDDLEWARE_EXTENSIONS)],
      this.sessionOptions,
    );
    this.sessionManagers = new SessionManagers({
      cwd: this.cwd,
      persist: this.persistAgentSessions,
      parentSessionFile: options.parentSessionFile,
    });
  }

  async run<TSchemaDef extends TSchema | undefined = undefined>(
    prompt: string,
    options: AgentRunOptions<TSchemaDef> = {},
  ): Promise<AgentRunResult<TSchemaDef>> {
    validateThinkingLevel(options.thinking);
    const thread = options.thread;
    if (thread && this.activeThreads.has(thread)) {
      throw new WorkflowError(
        `agent thread "${thread}" is already running; same-thread calls must be sequential`,
        WorkflowErrorCode.SCRIPT_VALIDATION_ERROR,
        { recoverable: false, agentLabel: options.label },
      );
    }
    if (thread) this.activeThreads.add(thread);
    try {
      return await this.runTurn(prompt, options);
    } finally {
      if (thread) this.activeThreads.delete(thread);
    }
  }

  private async runTurn<TSchemaDef extends TSchema | undefined = undefined>(
    prompt: string,
    options: AgentRunOptions<TSchemaDef> = {},
  ): Promise<AgentRunResult<TSchemaDef>> {
    // 1. Where the agent works and which tools it gets.
    const runCwd = realpathSync(options.cwd ?? this.cwd);
    assertProcessesStartIn(runCwd, options.label);
    const capture: StructuredOutputCapture<any> = { called: false, value: undefined };
    const customTools = buildSubagentTools({
      codingTools: this.codingToolsFor(runCwd),
      tools: options.tools,
      toolNames: options.toolNames,
      disallowedToolNames: options.disallowedToolNames,
      systemTools: options.systemTools,
      schema: options.schema,
      capture,
    });

    // 2. Which model it runs on.
    const modelRegistry = await this.getRegistry(options.modelRegistry);
    const choice = this.models.select(options, modelRegistry);

    // 3. The session it runs in. Persisted sessions are keyed by the runner's
    //    project cwd, not runCwd, so short-lived worktree agents still group
    //    under the project.
    const agentDir = getAgentDir();
    const sessionManager = this.sessionManagers.forCall(options.thread, runCwd, options.onNotice);
    const effectiveSessionManager =
      options.thread || !this.sessionOptions.sessionManager ? sessionManager : this.sessionOptions.sessionManager;
    // Report the child identity before the first prompt or usage event.
    options.onSessionCreated?.({
      sessionId: effectiveSessionManager.getSessionId(),
      sessionFile: effectiveSessionManager.isPersisted() ? effectiveSessionManager.getSessionFile() : undefined,
    });
    const threadLeaf = options.thread ? sessionManager.getLeafId() : null;
    let child: ChildSession;
    try {
      child = await openChildSession({
        cwd: runCwd,
        cwdIsExplicit: options.cwd !== undefined,
        agentDir,
        sessionManager,
        keepSessionManager: Boolean(options.thread),
        customTools,
        sharedResourceLoader: () => this.getSharedResourceLoader(agentDir, runCwd),
        modelRegistry,
        model: choice.model,
        thinkingLevel: choice.thinkingLevel,
        excludeTools: subagentExcludedTools(this.excludeTools, this.sessionOptions.excludeTools),
        injected: this.sessionOptions,
      });
    } catch (error) {
      if (options.thread) this.restoreThreadLeaf(sessionManager, threadLeaf);
      throw error;
    }

    // 4. The turn itself. From here on the session is disposed however the turn
    //    ends, and no teardown step can skip that or replace the turn's own
    //    result or error.
    const { session } = child;
    // This turn's own transcript, collected from message_end events below rather
    // than sliced out of session.messages with a pre-prompt() length snapshot.
    // Auto-compaction (on by default) can rewrite session.messages inside
    // prompt() — the array is rebuilt shorter (summary + kept tail) — so any
    // index captured here goes stale and a slice from it comes back empty,
    // misclassifying a successful turn as AGENT_EMPTY_OUTPUT and rolling back a
    // threaded turn that actually landed. Events are append-only per turn, so
    // the collected window survives compaction, and it naturally spans the
    // schema path's repair re-prompts, which run on this same session.
    const turnMessages: unknown[] = [];
    let progress: TurnProgress | undefined;
    let threadTurnSucceeded = false;
    let unregisterSteer: (() => void) | undefined;
    let removeAbortListener: (() => void) | undefined;
    let removeProgressListener: (() => void) | undefined;
    let removeTurnListener: (() => void) | undefined;
    try {
      // Name a persisted session for session pickers: only when this agent owns
      // the manager, and for a thread only on its first turn. Best-effort.
      if (
        this.persistAgentSessions &&
        (!this.sessionOptions.sessionManager || options.thread) &&
        (!options.thread || !threadLeaf) &&
        options.sessionName
      ) {
        try {
          sessionManager.appendSessionInfo(options.sessionName);
        } catch {
          // Naming is best-effort; never fail the run over it.
        }
      }

      // A threaded session already holds earlier turns' usage.
      progress = new TurnProgress(session, options, options.thread ? session.getSessionStats() : undefined);
      // With no resolved spec, the display still shows the pre-resolution
      // mainModel guess; report the model the session actually bound.
      if (!choice.model && session.model) {
        options.onModelResolved?.(formatModelSpecWithThinking(canonicalModelSpec(session.model), choice.thinkingLevel));
      }
      if (options.signal?.aborted) throw workflowAbortedError();
      unregisterSteer = options.registerSteer?.(async (text) => {
        await session.steer(text);
      });
      if (options.signal) {
        const onAbort = () => void session.abort();
        options.signal.addEventListener("abort", onAbort, { once: true });
        removeAbortListener = () => options.signal?.removeEventListener("abort", onAbort);
      }
      if (progress.live) {
        removeProgressListener = session.subscribe(progress.onEvent);
      }
      removeTurnListener = session.subscribe((event) => {
        if (event.type === "message_end") turnMessages.push(event.message);
      });

      await session.prompt(this.buildPrompt(prompt, options as AgentRunOptions<any>, Boolean(options.schema)));
      // Before either result branch, so a cancelled or failed turn is never
      // read as SCHEMA_NONCOMPLIANCE, an empty answer, or a partial success.
      assertTurnCompleted(turnMessages, options);

      if (options.schema) {
        const result = (await resolveStructuredOutput(
          session,
          turnMessages,
          capture,
          options.schema,
          options,
        )) as AgentRunResult<TSchemaDef>;
        threadTurnSucceeded = true;
        return result;
      }

      // A threaded session's restored transcript never enters turnMessages, so
      // an empty turn cannot reuse an old answer.
      const text = finalAnswer(turnMessages, options);
      threadTurnSucceeded = true;
      return text as AgentRunResult<TSchemaDef>;
    } finally {
      runTeardown([
        unregisterSteer,
        removeAbortListener,
        removeTurnListener,
        removeProgressListener,
        () => progress?.emitFinalHistory(),
        options.thread && !threadTurnSucceeded ? () => this.restoreThreadLeaf(sessionManager, threadLeaf) : undefined,
        // Before disposal, which tears down the session stats.
        () => progress?.reportFinalUsage(),
      ]);
      await child.dispose();
    }
  }

  /** Coding tools bound to `runCwd`; tools capture their cwd at construction and cannot be moved. */
  private codingToolsFor(runCwd: string): ToolDefinition[] {
    try {
      if (runCwd === realpathSync(this.cwd)) return this.baseTools;
    } catch {
      // An explicitly selected existing directory can outlive the constructor's
      // original directory. Its tools must not depend on that old path existing.
    }
    return createCodingTools(runCwd);
  }

  /** A per-run registry wins, then the host's shared registry, then the disk-backed fallback. */
  private async getRegistry(perRunRegistry?: ModelRegistry): Promise<ModelRegistry> {
    if (perRunRegistry) {
      return perRunRegistry;
    }
    if (this.sharedRegistry) {
      return this.sharedRegistry;
    }
    if (!this.registry) {
      this.registry = await ensureFallbackRegistry();
    }
    return this.registry;
  }

  /**
   * The cwd-aware model-tiers overlay, read once per instance. Every untagged
   * or tiered agent consults it, and a synchronous disk read per agent would
   * stall a large fan-out. One instance per run means each run still sees the
   * file as it was when the run started. `loader` is for tests and only matters
   * on the first call.
   */
  private loadTierConfig(loader?: () => ModelTierConfig | null): ModelTierConfig | null {
    if (!this.tierConfigBox) {
      const read = loader ?? (() => loadModelTierConfig({ cwd: this.cwd }));
      this.tierConfigBox = { value: read() };
    }
    return this.tierConfigBox.value;
  }

  private getSharedResourceLoader(agentDir: string, cwd = this.cwd): Promise<DefaultResourceLoader> {
    return this.resourceLoaders.get(agentDir, cwd);
  }

  /** Roll a thread back to its leaf before a failed turn, so the next turn continues from there. */
  private restoreThreadLeaf(sessionManager: SessionManager, leafId: string | null): void {
    if (leafId) sessionManager.branch(leafId);
    else sessionManager.resetLeaf();
  }

  private buildPrompt(prompt: string, options: AgentRunOptions<any>, structured: boolean): string {
    const parts = [this.instructions, options.instructions, prompt].filter(Boolean);
    if (structured) {
      parts.push(
        [
          "Final output contract:",
          "- Your final action MUST be a structured_output tool call.",
          "- The structured_output arguments are the return value of this subagent.",
          "- Do not emit a prose final answer instead of structured_output.",
          "- If you need to inspect files or run commands first, do so, then call structured_output exactly once.",
        ].join("\n"),
      );
    }
    return parts.join("\n\n");
  }
}

/**
 * Run every teardown step even when an earlier one throws. The steps are host
 * callbacks and bookkeeping, so none of them may skip the session's disposal or
 * replace the turn's own result or error.
 */
function runTeardown(steps: ReadonlyArray<(() => void) | undefined>): void {
  for (const step of steps) {
    try {
      step?.();
    } catch {
      // Best-effort by design: the turn's outcome is already decided.
    }
  }
}
