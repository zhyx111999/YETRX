import { claimProtocolStdout, errorText, log } from "./diagnostics.mjs";
import { createDispatcher } from "./protocol/dispatcher.mjs";
import { createLineReader } from "./protocol/line-reader.mjs";
import { createRecordWriter } from "./protocol/record-writer.mjs";
import { SessionHost } from "./runtime/session-host.mjs";

/**
 * Run the bridge: commands in on stdin, records out on stdout, diagnostics on stderr.
 * The process stays up across runs and sessions; it ends on `shutdown`, on a signal,
 * or when stdin closes, always after the live session has been shut down.
 */
export function main() {
  const writer = createRecordWriter(claimProtocolStdout());
  const host = new SessionHost(writer.send);

  let finishing;
  const finish = (code = 0) => {
    finishing ??= (async () => {
      await writer.flush();
      // Shutting down aborts in-flight HTTP streams, whose sockets close asynchronously.
      // On Windows libuv asserts (exit code 0xC0000409) if the process exits while such a
      // handle is still closing, so give them a moment before the hard exit.
      await new Promise((resolve) => setTimeout(resolve, 200));
      process.exit(code);
    })();
    return finishing;
  };
  const shutDownAndExit = async (code) => {
    await host.shutdown().catch((error) => log(`shutdown failed: ${errorText(error)}`));
    await finish(code);
  };

  const dispatch = createDispatcher({ host, send: writer.send, finish: () => finish(0) });
  const reader = createLineReader((line) => {
    dispatch(line).catch((error) => log(`dispatch failed: ${errorText(error)}`));
  });

  process.stdin.on("data", (chunk) => reader.push(chunk));
  process.stdin.on("end", () => {
    reader.end();
    void shutDownAndExit(0);
  });
  process.stdout.on("error", () => void finish(1));

  for (const signal of ["SIGINT", "SIGTERM"]) process.on(signal, () => void shutDownAndExit(0));
  // Background workflow code runs for the life of the process: a rejection there is
  // reported, not fatal. An exception thrown outside any promise leaves unknown state.
  process.on("unhandledRejection", (reason) => log(`unhandled rejection: ${errorText(reason)}`));
  process.on("uncaughtException", (error) => {
    log(`uncaught exception: ${error instanceof Error ? (error.stack ?? error.message) : String(error)}`);
    void shutDownAndExit(1);
  });
}
