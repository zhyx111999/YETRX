import { randomUUID } from "node:crypto";

/** Stands in for `ui.theme`: every styling call returns its text unchanged. */
const plainTheme = new Proxy(
  {},
  { get: () => (...args) => (typeof args.at(-1) === "string" ? args.at(-1) : "") },
);

/**
 * The `ctx.ui` extensions see. There is no terminal, so:
 *  - fire-and-forget calls (notify, status, text widgets, title) become
 *    `extension_ui_request` events with the shapes of pi's RPC extension-UI protocol,
 *    so the host sees them instead of the extension silently talking to nothing;
 *  - dialogs cannot be answered, so they resolve as cancelled at once and say so in
 *    an event (`autoCancelled: true`) rather than hanging a run on a prompt nobody sees;
 *  - everything that needs a real terminal (components, editor, themes) is inert.
 *
 * @param {(event: object) => void} emitEvent
 */
export function createHeadlessUi(emitEvent) {
  const request = (method, fields) => emitEvent({ type: "extension_ui_request", id: randomUUID(), method, ...fields });
  const cancelled = (method, fields, result) => {
    request(method, { ...fields, autoCancelled: true });
    return Promise.resolve(result);
  };

  return {
    select: (title, options) => cancelled("select", { title, options }, undefined),
    confirm: (title, message) => cancelled("confirm", { title, message }, false),
    input: (title, placeholder) => cancelled("input", { title, placeholder }, undefined),
    editor: (title, prefill) => cancelled("editor", { title, prefill }, undefined),
    notify: (message, notifyType = "info") => request("notify", { message, notifyType }),
    setStatus: (statusKey, statusText) => request("setStatus", { statusKey, statusText }),
    setTitle: (title) => request("setTitle", { title }),
    // Component factories are TUI-only; only plain text lines can be shown.
    setWidget: (widgetKey, content, options) => {
      if (content === undefined || Array.isArray(content)) {
        request("setWidget", { widgetKey, widgetLines: content, widgetPlacement: options?.placement ?? "aboveEditor" });
      }
    },
    onTerminalInput: () => () => {},
    setWorkingMessage: () => {},
    setWorkingVisible: () => {},
    setWorkingIndicator: () => {},
    setHiddenThinkingLabel: () => {},
    setFooter: () => {},
    setHeader: () => {},
    custom: async () => undefined,
    pasteToEditor: () => {},
    setEditorText: () => {},
    getEditorText: () => "",
    addAutocompleteProvider: () => {},
    setEditorComponent: () => {},
    getEditorComponent: () => undefined,
    theme: plainTheme,
    getAllThemes: () => [],
    getTheme: () => undefined,
    setTheme: () => ({ success: false, error: "Theme switching is not available in the bridge" }),
    getToolsExpanded: () => false,
    setToolsExpanded: () => {},
  };
}
