import { existsSync } from "node:fs";
import { createAgentSession, SessionManager, SettingsManager } from "@earendil-works/pi-coding-agent";
import { PROVIDER_ID, REQUIRED_WORKFLOW_TOOLS } from "../config.mjs";
import { errorText, log } from "../diagnostics.mjs";
import { bindAgentDir } from "./agent-dir.mjs";
import { createHeadlessUi } from "./headless-ui.mjs";
import { importHistory } from "./history-import.mjs";
import { toJsonEvent } from "./json-event.mjs";
import { persistModelDefaults } from "./model-defaults.mjs";
import { createRelayModelRuntime } from "./relay-model-runtime.mjs";
import { createResourceLoader } from "./resource-loader.mjs";
import { prepareSessionDirectory, restrictSessionFile } from "./session-storage.mjs";
import { buildSystemPrompt } from "./system-prompt.mjs";

/** Refuse to run without the workflow extension: a session without it is not the product. */
function assertWorkflowExtension(loader, session) {
  const { errors } = loader.getExtensions();
  if (errors.length > 0) {
    throw new Error(`extension load failed: ${errors.map((e) => `${e.path}: ${e.error}`).join("; ")}`);
  }
  const active = session.getActiveToolNames();
  const missing = REQUIRED_WORKFLOW_TOOLS.filter((name) => !active.includes(name));
  if (missing.length > 0) throw new Error(`workflow extension did not register: ${missing.join(", ")}`);
}

/**
 * An existing session keeps the entries it was saved with; make the requested model and
 * thinking level the current ones in its history, so a resume reads back what ran last.
 */
function recordSelection(session, modelId) {
  const { model, thinkingLevel } = session.sessionManager.buildSessionContext();
  if (model?.provider !== PROVIDER_ID || model.modelId !== modelId) {
    session.sessionManager.appendModelChange(PROVIDER_ID, modelId);
  }
  if (thinkingLevel !== session.thinkingLevel) session.sessionManager.appendThinkingLevelChange(session.thinkingLevel);
}

/** Forward every session event as a wire event; restrict the session file once it exists. */
function forwardEvents(session, sessionFile, emit) {
  let restricted = false;
  return session.subscribe((event) => {
    try {
      emit({ type: "event", event: toJsonEvent(event) });
    } catch (error) {
      log(`could not forward ${event.type}: ${errorText(error)}`);
    }
    if (!restricted && event.type === "message_end") restricted = restrictSessionFile(sessionFile);
  });
}

/**
 * Build the live agent session for one `init`: relay provider, vendored workflow
 * extension and skills, persisted (or imported) history, extensions bound.
 *
 * @param {ReturnType<import("../protocol/init-config.mjs").parseInit>} config
 * @param {(record: object) => void} emit writes protocol records
 */
export async function openSession(config, emit) {
  bindAgentDir(config.agentDir);
  const sessionDir = prepareSessionDirectory(config.sessionFile);
  const isNewSession = !existsSync(config.sessionFile);

  const sessionManager = SessionManager.open(config.sessionFile, sessionDir, config.cwd);
  if (isNewSession && config.history?.length) importHistory(sessionManager, config.history, config);

  // Project resources (.pi extensions, settings, skills) are code the workspace chose;
  // they load only when the host says the workspace is trusted.
  const settingsManager = SettingsManager.create(config.cwd, config.agentDir, { projectTrusted: config.trustProject });
  await persistModelDefaults(settingsManager, config.modelId, config.thinkingLevel);

  const modelRuntime = await createRelayModelRuntime(config);
  const loader = await createResourceLoader({
    cwd: config.cwd,
    agentDir: config.agentDir,
    settingsManager,
    systemPrompt: buildSystemPrompt(config.tools),
  });

  const { session } = await createAgentSession({
    cwd: config.cwd,
    agentDir: config.agentDir,
    modelRuntime,
    model: modelRuntime.getModel(PROVIDER_ID, config.modelId),
    thinkingLevel: config.thinkingLevel,
    resourceLoader: loader,
    sessionManager,
    settingsManager,
  });

  const unsubscribe = forwardEvents(session, config.sessionFile, emit);
  const emitEvent = (event) => emit({ type: "event", event });
  let closed = false;
  const dispose = async (reason) => {
    if (closed) return;
    closed = true;
    try {
      await session.abort();
      // dispose() alone does not tell extensions the session is ending.
      await session.extensionRunner.emit({ type: "session_shutdown", reason });
    } catch (error) {
      log(`shutdown hooks failed: ${errorText(error)}`);
    }
    unsubscribe();
    session.dispose();
  };

  try {
    await session.bindExtensions({
      uiContext: createHeadlessUi(emitEvent),
      mode: "rpc",
      onError: ({ extensionPath, event, error }) =>
        emitEvent({ type: "extension_error", extensionPath, event, error }),
      // An extension asking to end the process is reported, not obeyed: the host owns its lifetime.
      shutdownHandler: () => emitEvent({ type: "extension_shutdown_request" }),
    });
    assertWorkflowExtension(loader, session);
    if (config.tools.length > 0) {
      session.setActiveToolsByName([...new Set([...session.getActiveToolNames(), ...config.tools])]);
    }
    recordSelection(session, config.modelId);
  } catch (error) {
    await dispose("quit");
    throw error;
  }

  for (const diagnostic of loader.getSkills().diagnostics) log(`skill ${diagnostic.type}: ${diagnostic.message} (${diagnostic.path})`);

  return {
    session,
    modelRuntime,
    settingsManager,
    baseUrl: config.baseUrl,
    dispose,
    ready: {
      tools: session.getActiveToolNames(),
      skills: loader.getSkills().skills.map((skill) => skill.name),
      modelId: session.model?.id,
    },
  };
}
