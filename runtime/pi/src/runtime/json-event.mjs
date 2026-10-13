/**
 * pi's own SDK-event to wire-event conversion, the one `--mode rpc` and `--mode json`
 * use: it turns `message_update` into a delta-only record. The SDK event carries the
 * whole accumulated message in `message` and in every `assistantMessageEvent.partial`,
 * which grows quadratically over a long answer.
 *
 * The function is not exported from the package root and `exports` forbids deep
 * imports, so it is loaded by file URL. That is safe here because the package version
 * is pinned; a version bump that moves the file fails loudly at startup.
 */
const codingAgentEntry = import.meta.resolve("@earendil-works/pi-coding-agent");
const { toJsonEvent } = await import(new URL("./modes/json-event.js", codingAgentEntry).href);

export { toJsonEvent };
