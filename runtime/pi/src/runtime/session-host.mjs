import { PROVIDER_ID } from "../config.mjs";
import { persistModelDefaults } from "./model-defaults.mjs";
import { openSession } from "./open-session.mjs";
import { updateModels } from "./update-models.mjs";

/** The failure of a run that ended in a provider error, as pi records it on the last message. */
function runFailure(session) {
  const last = session.messages.at(-1);
  if (last?.role === "assistant" && last.stopReason === "error") return last.errorMessage ?? "model request failed";
  return undefined;
}

/**
 * Owns the one live agent session of this process and the order of operations on it.
 * `init`, `setModel` and `shutdown` change the session and run one at a time; `prompt`
 * and `abort` wait for those to finish but run alongside an active run, so an abort can
 * always reach a prompt that is still streaming.
 */
export class SessionHost {
  #emit;
  #active = null;
  #lifecycle = Promise.resolve();

  /** @param {(record: object) => void} emit writes protocol records */
  constructor(emit) {
    this.#emit = emit;
  }

  #serialize(task) {
    const run = this.#lifecycle.then(task);
    this.#lifecycle = run.catch(() => {});
    return run;
  }

  async #current() {
    await this.#lifecycle;
    if (!this.#active) throw new Error("not initialized: send init first");
    return this.#active;
  }

  async #close(reason) {
    const active = this.#active;
    this.#active = null;
    await active?.dispose(reason);
  }

  /** Replace the live session (if any) with a freshly opened one. Returns the ready payload. */
  init(config) {
    return this.#serialize(async () => {
      // A new init always ends the old session: its background runs are paused, not handed over.
      await this.#close("quit");
      this.#active = await openSession(config, this.#emit);
      return this.#active.ready;
    });
  }

  /**
   * Run one prompt to completion. While a run is already streaming (a background result
   * started one, say) the message is queued behind it instead of being rejected.
   */
  async prompt({ message, streamingBehavior }) {
    const { session } = await this.#current();
    let disposition;
    await session.prompt(message, {
      expandPromptTemplates: false,
      ...(session.isStreaming ? { streamingBehavior } : {}),
      preflightResult: (value) => {
        disposition = value;
      },
    });
    const failure = disposition === "started" ? runFailure(session) : undefined;
    if (failure) throw new Error(failure);
  }

  async abort() {
    const { session } = await this.#current();
    await session.abort();
  }

  setModel({ modelId, thinkingLevel }) {
    return this.#serialize(async () => {
      if (!this.#active) throw new Error("not initialized: send init first");
      const { session, modelRuntime, settingsManager } = this.#active;
      const model = modelRuntime.getModel(PROVIDER_ID, modelId);
      if (!model) throw new Error(`unknown model "${modelId}"`);
      await session.setModel(model);
      if (thinkingLevel !== undefined) session.setThinkingLevel(thinkingLevel);
      await persistModelDefaults(settingsManager, modelId, thinkingLevel);
    });
  }

  /** Replace the relay's model list in place; the session and its background runs keep going. */
  setModels(update) {
    return this.#serialize(async () => {
      if (!this.#active) throw new Error("not initialized: send init first");
      await updateModels(this.#active, update);
    });
  }

  /** End the live session; a no-op when there is none. */
  shutdown() {
    return this.#serialize(() => this.#close("quit"));
  }
}
