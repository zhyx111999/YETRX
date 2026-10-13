import { spawn } from "node:child_process";
import { mkdirSync, mkdtempSync, realpathSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { createLineReader } from "../../src/protocol/line-reader.mjs";

const BRIDGE = fileURLToPath(new URL("../../bridge.mjs", import.meta.url));
export const TOKEN = "tok-test-0123456789abcdef";

/** A throwaway workspace: cwd, agent directory, session file and an isolated HOME. */
export function makeWorkspace() {
  const root = realpathSync(mkdtempSync(join(tmpdir(), "yetrx-pi-")));
  const dirs = { root, cwd: join(root, "work"), agentDir: join(root, "agent"), home: join(root, "home") };
  for (const dir of [dirs.cwd, dirs.agentDir, dirs.home]) mkdirSync(dir, { recursive: true });
  return { ...dirs, sessionFile: join(root, "sessions", "nested", "session.jsonl") };
}

/** The init command for a workspace and a relay URL. */
export function initCommand(workspace, baseUrl, overrides = {}) {
  return {
    id: "init-1",
    type: "init",
    cwd: workspace.cwd,
    agentDir: workspace.agentDir,
    sessionFile: workspace.sessionFile,
    baseUrl,
    apiKey: TOKEN,
    models: [
      { id: "profile-1", name: "Profile 1", contextWindow: 128000, maxTokens: 8192 },
      { id: "profile-2", name: "Profile 2", contextWindow: 64000, maxTokens: 4096 },
    ],
    modelId: "profile-1",
    thinkingLevel: "off",
    ...overrides,
  };
}

/** A running `node bridge.mjs` with every record it printed and everything on stderr. */
export function startBridge(workspace) {
  const child = spawn(process.execPath, [BRIDGE], {
    stdio: ["pipe", "pipe", "pipe"],
    env: {
      PATH: process.env.PATH,
      SystemRoot: process.env.SystemRoot,
      PI_CODING_AGENT_DIR: workspace.agentDir,
      HOME: workspace.home,
      USERPROFILE: workspace.home,
      PI_OFFLINE: "1",
      PI_SKIP_VERSION_CHECK: "1",
      PI_TELEMETRY: "0",
    },
  });
  const records = [];
  const waiters = new Set();
  let stderr = "";
  const exited = new Promise((resolve) => child.on("exit", (code) => resolve(code)));

  const reader = createLineReader((line) => {
    records.push(JSON.parse(line));
    for (const waiter of [...waiters]) waiter();
  });
  child.stdout.on("data", (chunk) => reader.push(chunk));
  child.stderr.on("data", (chunk) => {
    stderr += chunk;
  });

  /** Resolve with the first record at or after `from` that satisfies `predicate`. */
  const waitFor = (predicate, { from = 0, timeoutMs = 30000, label = "record" } = {}) =>
    new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        waiters.delete(check);
        reject(new Error(`timed out waiting for ${label}\nrecords: ${records.slice(from).map((r) => r.type + (r.event ? `:${r.event.type}` : "")).join(", ")}\nstderr: ${stderr}`));
      }, timeoutMs);
      const check = () => {
        const found = records.slice(from).find(predicate);
        if (!found) return;
        clearTimeout(timer);
        waiters.delete(check);
        resolve(found);
      };
      waiters.add(check);
      check();
    });

  const send = (command) => child.stdin.write(`${JSON.stringify(command)}\n`);

  return {
    records,
    send,
    sendRaw: (text) => child.stdin.write(text),
    waitFor,
    exited,
    get stderr() {
      return stderr;
    },
    /** Send a command and wait for the record answering it (`ready` or `response`). */
    request: (command, options) =>
      Promise.resolve().then(() => {
        const from = records.length;
        send(command);
        return waitFor((r) => r.id === command.id && (r.type === "response" || r.type === "ready"), { from, label: `${command.type} answer`, ...options });
      }),
    /** Events of one type, optionally only those after index `from`. */
    events: (type, from = 0) => records.slice(from).filter((r) => r.type === "event" && r.event.type === type).map((r) => r.event),
    /** End the process by closing stdin and wait for its exit code. */
    close: () => {
      child.stdin.end();
      return exited;
    },
    kill: () => child.kill(),
  };
}
