import assert from "node:assert/strict";
import { existsSync, mkdirSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { describe, it } from "node:test";
import { TOKEN, initCommand, makeWorkspace } from "./helpers/bridge-client.mjs";
import { prompt, transcript, withBridge } from "./helpers/scenario.mjs";

const readEntries = (file) =>
  readFileSync(file, "utf8")
    .trim()
    .split("\n")
    .map((line) => JSON.parse(line));

const usage = { input: 3, output: 2, cacheRead: 0, cacheWrite: 0, totalTokens: 5, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } };
const legacyHistory = () => [
  { role: "user", content: "legacy question", timestamp: 1000 },
  {
    role: "assistant",
    content: [{ type: "toolCall", id: "call_legacy", name: "read", arguments: { path: "a.txt" } }],
    api: "openai-completions",
    provider: "legacy",
    model: "old-model",
    usage,
    stopReason: "toolUse",
    timestamp: 1001,
  },
  { role: "toolResult", toolCallId: "call_legacy", toolName: "read", content: [{ type: "text", text: "legacy file body" }], isError: false, timestamp: 1002 },
  {
    role: "assistant",
    content: [{ type: "text", text: "legacy answer" }],
    api: "openai-completions",
    provider: "legacy",
    model: "old-model",
    usage,
    stopReason: "stop",
    timestamp: 1003,
  },
];

describe("abort", () => {
  it("stops a streaming run, answers both the abort and the prompt, and leaves the session usable", async () => {
    await withBridge(
      { script: (request, n) => (n === 1 ? { text: "partial answer ", hang: true } : { text: "second run" }) },
      async ({ bridge, fake }) => {
        const from = bridge.records.length;
        bridge.send({ id: "p1", type: "prompt", message: "think for a long time" });
        await bridge.waitFor((r) => r.event?.assistantMessageEvent?.type === "text_delta", { from, label: "first delta" });

        const aborted = await bridge.request({ id: "a1", type: "abort" });
        assert.deepEqual(aborted, { type: "response", id: "a1", ok: true });
        const answered = await bridge.waitFor((r) => r.id === "p1" && r.type === "response", { from, label: "prompt answer" });
        assert.equal(answered.ok, true, "an aborted run is not an error");

        const settled = bridge.events("agent_settled", from).at(-1);
        assert.equal(settled.aborted, true);
        const last = bridge.events("message_end", from).filter((e) => e.message.role === "assistant").at(-1);
        assert.equal(last.message.stopReason, "aborted");
        assert.equal(fake.requests[0].disconnected, true, "the relay connection was closed");

        const next = await prompt(bridge, "p2", "go again");
        assert.equal(next.response.ok, true);
        assert.equal(bridge.events("agent_settled", from).at(-1).aborted, false);
      },
    );
  });

  it("abort with nothing running still answers with the id", async () => {
    await withBridge({ script: () => ({ text: "ok" }) }, async ({ bridge }) => {
      assert.deepEqual(await bridge.request({ id: "a0", type: "abort" }), { type: "response", id: "a0", ok: true });
    });
  });
});

describe("session persistence", () => {
  it("creates the storage directory, keeps the token out of the file, and restores history in a new process", async () => {
    const workspace = makeWorkspace();
    try {
      await withBridge(
        { workspace, script: (request, n) => ({ text: n === 1 ? "Pineapple noted." : "x" }) },
        async ({ bridge }) => {
          assert.ok(!existsSync(workspace.sessionFile), "nothing is written before the first message");
          await prompt(bridge, "p1", "remember: pineapple");
          assert.ok(existsSync(workspace.sessionFile), "nested storage directory was created");
          if (process.platform !== "win32") {
            assert.equal(statSync(workspace.sessionFile).mode & 0o777, 0o600);
            assert.equal(statSync(join(workspace.sessionFile, "..")).mode & 0o777, 0o700);
          }
          assert.equal((await bridge.request({ id: "bye", type: "shutdown" })).ok, true);
          assert.equal(await bridge.exited, 0);
        },
      );
      const saved = readFileSync(workspace.sessionFile, "utf8");
      assert.ok(!saved.includes(TOKEN), "the relay token is never written to the session");

      await withBridge(
        { workspace, script: () => ({ text: "You said pineapple." }) },
        async ({ bridge, fake, ready }) => {
          assert.equal(ready.type, "ready");
          await prompt(bridge, "p2", "what did I say?");
          const seen = transcript(fake.requests[0]).join("\n");
          assert.match(seen, /user: remember: pineapple/);
          assert.match(seen, /assistant: Pineapple noted\./);
          assert.match(seen, /user: what did I say\?/);
        },
      );
      const entries = readEntries(workspace.sessionFile);
      assert.equal(entries.filter((e) => e.type === "session").length, 1, "one header: the file was reopened, not recreated");
      assert.equal(entries.filter((e) => e.type === "message" && e.message.role === "user" && JSON.stringify(e.message.content).includes("pineapple")).length, 1);
    } finally {
      rmSync(workspace.root, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 });
    }
  });

  it("imports init.history once into a new session and never again for an existing file", async () => {
    const workspace = makeWorkspace();
    try {
      await withBridge(
        { workspace, init: { history: legacyHistory() }, script: () => ({ text: "continuing" }) },
        async ({ bridge, fake }) => {
          await prompt(bridge, "p1", "and now?");
          const seen = transcript(fake.requests[0]).join("\n");
          assert.match(seen, /user: legacy question/);
          assert.match(seen, /tool: legacy file body/);
          assert.match(seen, /assistant: legacy answer/);
          assert.match(seen, /user: and now\?/);
        },
      );
      const afterFirst = readEntries(workspace.sessionFile);
      const roles = afterFirst.filter((e) => e.type === "message").map((e) => e.message.role);
      assert.equal(roles.filter((role) => role === "toolResult").length, 1);

      await withBridge(
        { workspace, init: { history: legacyHistory() }, script: () => ({ text: "again" }) },
        async ({ bridge, fake }) => {
          await prompt(bridge, "p2", "once more");
          const seen = transcript(fake.requests[0]).join("\n");
          assert.equal(seen.split("legacy question").length - 1, 1, "history appears once in the model context");
        },
      );
      const afterSecond = readEntries(workspace.sessionFile);
      const legacyCopies = afterSecond.filter((e) => e.type === "message" && JSON.stringify(e.message).includes("legacy question"));
      assert.equal(legacyCopies.length, 1, "the file holds the imported history exactly once");
    } finally {
      rmSync(workspace.root, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 });
    }
  });

  it("rejects a malformed history before writing anything", async () => {
    await withBridge({ script: () => ({ text: "x" }), init: false }, async ({ bridge, fake, ws }) => {
      const bad = [{ role: "user", content: "q" }, { role: "assistant", content: [{ type: "text", text: "a" }] }];
      const answer = await bridge.request(initCommand(ws, fake.baseUrl, { id: "i", history: bad }));
      assert.deepEqual({ type: answer.type, id: answer.id, ok: answer.ok }, { type: "response", id: "i", ok: false });
      assert.match(answer.error, /history\[1\]/);
      assert.ok(!existsSync(ws.sessionFile));
    });
  });
});

describe("isolation from the host machine", () => {
  it("does not load extensions or settings from an untrusted workspace's .pi directory", async () => {
    await withBridge(
      {
        script: () => ({ text: "ok" }),
        prepare: (ws) => {
          const extensions = join(ws.cwd, ".pi", "extensions");
          mkdirSync(extensions, { recursive: true });
          writeFileSync(
            join(extensions, "canary.js"),
            `import { writeFileSync } from "node:fs";\nexport default function () { writeFileSync(${JSON.stringify(join(ws.root, "canary-ran"))}, "yes"); }\n`,
          );
        },
      },
      async ({ bridge, ws }) => {
        await prompt(bridge, "p1", "hello");
        assert.ok(!existsSync(join(ws.root, "canary-ran")), "a workspace extension ran without trust");
      },
    );
  });

  it("writes the active model into the agent directory so subagents default to it, and nowhere else", async () => {
    await withBridge({ script: () => ({ text: "ok" }) }, async ({ bridge, ws }) => {
      await bridge.request({ id: "m", type: "set_model", modelId: "profile-2", thinkingLevel: "low" });
      const settings = JSON.parse(readFileSync(join(ws.agentDir, "settings.json"), "utf8"));
      assert.equal(settings.defaultProvider, "yetrx");
      assert.equal(settings.defaultModel, "profile-2");
      assert.equal(settings.defaultThinkingLevel, "low");
      assert.ok(!existsSync(join(ws.agentDir, "auth.json")), "no credentials file");
      assert.ok(!existsSync(join(ws.agentDir, "models.json")), "no models file");
      assert.ok(!existsSync(join(ws.agentDir, "models-store.json")), "no catalog cache");
      assert.ok(!readFileSync(join(ws.agentDir, "settings.json"), "utf8").includes(TOKEN));
    });
  });
});
