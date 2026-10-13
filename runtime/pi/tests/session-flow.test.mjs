import assert from "node:assert/strict";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { describe, it } from "node:test";
import { TOKEN, initCommand } from "./helpers/bridge-client.mjs";
import { prompt, textFor, textOf, transcript, withBridge } from "./helpers/scenario.mjs";

const reply = (text) => () => ({ text });

describe("init", () => {
  it("reports the real tools and exactly the three workflow skills, with the init id", async () => {
    await withBridge({ script: reply("unused") }, async ({ ready }) => {
      assert.equal(ready.type, "ready");
      assert.equal(ready.id, "init-1");
      assert.equal(ready.modelId, "profile-1");
      assert.deepEqual(ready.tools, ["read", "bash", "edit", "write", "subagents", "subagents_control"]);
      assert.deepEqual([...ready.skills].sort(), ["subagents-setup", "workflow-authoring", "workflow-patterns"]);
      for (const name of ready.tools) assert.doesNotMatch(name, /tavily|web_search|web_fetch/);
    });
  });

  it("enables grep/find/ls on request, and a second init replaces the session in the same process", async () => {
    await withBridge({ script: reply("unused") }, async ({ bridge, fake, ws, ready }) => {
      assert.ok(!ready.tools.includes("grep"));
      const second = await bridge.request(
        initCommand(ws, fake.baseUrl, { id: "init-2", tools: ["grep", "find", "ls"], sessionFile: join(ws.root, "second.jsonl") }),
      );
      assert.equal(second.type, "ready");
      assert.equal(second.id, "init-2");
      for (const name of ["read", "bash", "edit", "write", "grep", "find", "ls", "subagents", "subagents_control"]) {
        assert.ok(second.tools.includes(name), name);
      }
    });
  });

  it("fails an invalid init with ok:false carrying the id, and stays usable", async () => {
    await withBridge({ script: reply("still works"), init: false }, async ({ bridge, fake, ws }) => {
      const bad = await bridge.request(initCommand(ws, "http://example.com/v1", { id: "bad-init" }));
      assert.deepEqual({ type: bad.type, id: bad.id, ok: bad.ok }, { type: "response", id: "bad-init", ok: false });
      assert.match(bad.error, /baseUrl/);
      const early = await bridge.request({ id: "early", type: "prompt", message: "hi" });
      assert.equal(early.ok, false);
      assert.match(early.error, /not initialized/);
      const ready = await bridge.request(initCommand(ws, fake.baseUrl, { id: "init-ok" }));
      assert.equal(ready.type, "ready");
    });
  });
});

describe("a prompt", () => {
  it("streams delta-only events with a complete message_end, and settles before it answers", async () => {
    await withBridge({ script: reply("Hello from the relay.") }, async ({ bridge, fake }) => {
      const { response, records } = await prompt(bridge, "p1", "say hello");
      assert.deepEqual(response, { type: "response", id: "p1", ok: true });

      const updates = records.filter((r) => r.event?.type === "message_update");
      assert.ok(updates.length >= 3);
      for (const { event } of updates) {
        assert.deepEqual(Object.keys(event).sort(), ["assistantMessageEvent", "type", "usage"], "no cumulative message on the wire");
        assert.ok(!("partial" in event.assistantMessageEvent), "no cumulative partial on the wire");
      }
      const streamed = updates
        .map((r) => r.event.assistantMessageEvent)
        .filter((e) => e.type === "text_delta")
        .map((e) => e.delta)
        .join("");
      assert.equal(streamed, "Hello from the relay.");

      const assistantEnd = records.find((r) => r.event?.type === "message_end" && r.event.message.role === "assistant");
      assert.equal(assistantEnd.event.message.content[0].text, "Hello from the relay.");
      assert.equal(assistantEnd.event.message.stopReason, "stop");
      assert.equal(assistantEnd.event.message.provider, "yetrx");
      assert.equal(assistantEnd.event.message.model, "profile-1");

      const types = records.map((r) => r.event?.type ?? r.type);
      assert.ok(types.indexOf("agent_settled") < types.indexOf("response"), "agent_settled arrives before the response");
      assert.equal(fake.requests.length, 1);
    });
  });

  it("goes through the relay with the bearer token, the raw model id and streaming tool/usage options", async () => {
    await withBridge({ script: reply("ok"), init: { thinkingLevel: "medium" } }, async ({ bridge, fake }) => {
      await prompt(bridge, "p1", "hello");
      const [{ headers, body }] = fake.requests;
      assert.equal(headers.authorization, `Bearer ${TOKEN}`);
      assert.equal(body.model, "profile-1");
      assert.equal(body.stream, true);
      assert.equal(body.stream_options?.include_usage, true);
      assert.equal(body.reasoning_effort, "medium");
      assert.ok(body.max_tokens > 0 && !("max_completion_tokens" in body));
      assert.ok(!("store" in body));
      assert.deepEqual(body.tools.map((tool) => tool.function.name), ["read", "bash", "edit", "write", "subagents", "subagents_control"]);
      assert.ok(!JSON.stringify(bridge.records).includes(TOKEN));
      assert.ok(!bridge.stderr.includes(TOKEN));
    });
  });

  it("sees a concise system prompt: no pi persona or docs, no SYSTEM/APPEND from disk, but cwd context and skills", async () => {
    await withBridge(
      {
        script: reply("ok"),
        prepare: (ws) => {
          writeFileSync(join(ws.agentDir, "APPEND_SYSTEM.md"), "CANARY_APPEND persona text\n");
          writeFileSync(join(ws.agentDir, "SYSTEM.md"), "CANARY_SYSTEM persona text\n");
          writeFileSync(join(ws.cwd, "AGENTS.md"), "CANARY_PROJECT_RULE keep this\n");
          const personal = join(ws.home, ".agents", "skills", "personal-skill");
          mkdirSync(personal, { recursive: true });
          writeFileSync(join(personal, "SKILL.md"), "---\nname: personal-skill\ndescription: PRIVATE_HOME_SKILL must not load\n---\nbody\n");
        },
      },
      async ({ bridge, fake, ready }) => {
        await prompt(bridge, "p1", "hello");
        const system = textFor(fake.requests[0], "system");
        assert.match(system, /read: read a file/);
        assert.match(system, /subagents: start background subagents/);
        assert.match(system, /CANARY_PROJECT_RULE/, "cwd context files are still loaded");
        for (const name of ["workflow-authoring", "workflow-patterns", "subagents-setup"]) assert.ok(system.includes(name), name);
        for (const absent of ["CANARY_APPEND", "CANARY_SYSTEM", "PRIVATE_HOME_SKILL", "Pi documentation", "expert coding assistant", "pi, a coding agent harness"]) {
          assert.ok(!system.includes(absent), `system prompt must not contain ${absent}`);
        }
        assert.ok(!ready.skills.includes("personal-skill"));
      },
    );
  });

  it("applies the latest host systemPrompt in one live session while preserving resources and history", async () => {
    const initial = "HOST_PROMPT_INITIAL: initial instructions.";
    const first = "HOST_PROMPT_FIRST: first instructions.";
    const second = "HOST_PROMPT_SECOND: replacement instructions.";
    await withBridge(
      {
        script: reply("noted"),
        init: { systemPrompt: initial, tools: ["grep", "find", "ls"] },
        prepare: (ws) => {
          writeFileSync(join(ws.agentDir, "SYSTEM.md"), "PRIVATE_SYSTEM_CANARY");
          writeFileSync(join(ws.agentDir, "APPEND_SYSTEM.md"), "PRIVATE_APPEND_CANARY");
          writeFileSync(join(ws.cwd, "AGENTS.md"), "PROJECT_CONTEXT_CANARY");
        },
      },
      async ({ bridge, fake, ws, ready }) => {
        const tools = ["read", "bash", "edit", "write", "grep", "find", "ls", "subagents", "subagents_control"];
        assert.deepEqual([...ready.tools].sort(), [...tools].sort());
        const check = (expected, absent = []) => {
          const request = fake.requests.at(-1);
          const system = textFor(request, "system");
          assert.ok(system.includes(expected), "the relay receives the latest host prompt");
          assert.ok(system.includes(ws.cwd.replace(/\\/g, "/")), "cwd survives");
          assert.match(system, /PROJECT_CONTEXT_CANARY/);
          for (const name of ["workflow-authoring", "workflow-patterns", "subagents-setup"]) {
            assert.ok(system.includes(name), `skill metadata survives: ${name}`);
          }
          assert.deepEqual(request.body.tools.map((tool) => tool.function.name).sort(), [...tools].sort());
          const allMessages = JSON.stringify(request.body.messages);
          for (const text of [...absent, "PRIVATE_SYSTEM_CANARY", "PRIVATE_APPEND_CANARY"]) {
            assert.ok(!allMessages.includes(text), `must not inject ${text}`);
          }
          assert.match(allMessages, /多代理模式已启动/, "the workflow parent hook still runs");
        };

        assert.equal((await prompt(bridge, "initial", "remember this turn")).response.ok, true);
        check(initial);
        for (const [id, systemPrompt, absent] of [["p1", first, [initial]], ["p2", second, [initial, first]]]) {
          assert.equal((await bridge.request({ id, type: "prompt", message: id, systemPrompt })).ok, true);
          check(systemPrompt, absent);
        }
        assert.equal(fake.requests.length, 3);
        assert.match(textFor(fake.requests.at(-1), "user"), /remember this turn/, "history survives the updates");
        assert.match(textFor(fake.requests.at(-1), "assistant"), /noted/);

        for (const systemPrompt of [null, false, 123, [], {}]) {
          const invalidInit = await bridge.request(initCommand(ws, fake.baseUrl, { id: "invalid-init", systemPrompt }));
          assert.equal(invalidInit.ok, false);
          assert.equal(invalidInit.error, "systemPrompt must be a string");
          const invalidPrompt = await bridge.request({ id: "invalid-prompt", type: "prompt", message: "invalid", systemPrompt });
          assert.equal(invalidPrompt.ok, false);
          assert.equal(invalidPrompt.error, "systemPrompt must be a string");
        }
        assert.equal(fake.requests.length, 3, "invalid values never reach the model or replace the session");
        assert.equal((await prompt(bridge, "omitted", "keep current instructions")).response.ok, true);
        check(second, [initial, first]);

        assert.equal((await bridge.request({ id: "empty", type: "prompt", message: "use default instructions", systemPrompt: "" })).ok, true);
        check("read: read a file", [initial, first, second, "expert coding assistant", "Pi documentation"]);
        assert.match(textFor(fake.requests.at(-1), "system"), /ls: list a directory/);
        assert.match(textFor(fake.requests.at(-1), "user"), /remember this turn/);
        assert.equal(bridge.records.filter((record) => record.type === "ready").length, 1);
        assert.equal(bridge.events("extension_error").length, 0);
      },
    );
  });

  it("injects the workflow extension's hidden parent note after the typed message", async () => {
    await withBridge({ script: reply("ok") }, async ({ bridge, fake }) => {
      const { records } = await prompt(bridge, "p1", "refactor this");
      const hidden = records.find((r) => r.event?.type === "message_end" && r.event.message.customType === "workflows-parent-prompt");
      assert.ok(hidden, "the vendored extension's parent prompt ran");
      assert.match(transcript(fake.requests[0]).join("\n"), /user: refactor this[\s\S]*多代理模式已启动/);
    });
  });

  it("runs a tool call and feeds the result back to the model", async () => {
    await withBridge(
      {
        script: (request) =>
          request.body.messages.some((m) => m.role === "tool")
            ? { text: "The file says: secret-42" }
            : { toolCalls: [{ name: "read", args: { path: "note.txt" } }] },
        prepare: (ws) => writeFileSync(join(ws.cwd, "note.txt"), "secret-42\n"),
      },
      async ({ bridge, fake }) => {
        const { response, records } = await prompt(bridge, "p1", "read note.txt");
        assert.equal(response.ok, true);
        const start = records.find((r) => r.event?.type === "tool_execution_start");
        const end = records.find((r) => r.event?.type === "tool_execution_end");
        assert.equal(start.event.toolName, "read");
        assert.deepEqual(start.event.args, { path: "note.txt" });
        assert.equal(end.event.isError, false);
        assert.match(end.event.result.content[0].text, /secret-42/);

        assert.equal(fake.requests.length, 2);
        const toolMessage = fake.requests[1].body.messages.find((m) => m.role === "tool");
        assert.match(textOf(toolMessage.content), /secret-42/);
        const finalAnswer = records.filter((r) => r.event?.type === "message_end" && r.event.message.role === "assistant").at(-1);
        assert.equal(finalAnswer.event.message.content[0].text, "The file says: secret-42");
      },
    );
  });

  it("keeps history across two turns", async () => {
    await withBridge(
      { script: (request, n) => ({ text: n === 1 ? "Noted, Zed." : "You are Zed." }) },
      async ({ bridge, fake }) => {
        await prompt(bridge, "p1", "My name is Zed");
        await prompt(bridge, "p2", "What is my name?");
        const second = transcript(fake.requests[1]).join("\n");
        assert.match(second, /user: My name is Zed/);
        assert.match(second, /assistant: Noted, Zed\./);
        assert.match(second, /user: What is my name\?/);
      },
    );
  });

  it("answers a provider failure with ok:false and the error, and recovers on the next prompt", async () => {
    await withBridge(
      {
        script: (request, n) => (n === 1 ? { status: 400, error: "relay says no" } : { text: "back" }),
        // pi retries failed requests by default, which would hide the failure behind the second reply.
        prepare: (ws) => writeFileSync(join(ws.agentDir, "settings.json"), JSON.stringify({ retry: { enabled: false } })),
      },
      async ({ bridge }) => {
        const failed = await prompt(bridge, "p1", "first");
        assert.deepEqual({ id: failed.response.id, ok: failed.response.ok }, { id: "p1", ok: false });
        assert.match(failed.response.error, /relay says no/);
        assert.ok(failed.records.some((r) => r.event?.type === "agent_settled"));
        const next = await prompt(bridge, "p2", "second");
        assert.equal(next.response.ok, true);
      },
    );
  });
});

describe("models", () => {
  it("set_model switches the model and thinking level for the next request", async () => {
    await withBridge({ script: reply("ok") }, async ({ bridge, fake }) => {
      const set = await bridge.request({ id: "m1", type: "set_model", modelId: "profile-2", thinkingLevel: "high" });
      assert.deepEqual(set, { type: "response", id: "m1", ok: true });
      await prompt(bridge, "p1", "hello");
      assert.equal(fake.requests[0].body.model, "profile-2");
      assert.equal(fake.requests[0].body.reasoning_effort, "high");

      const unknown = await bridge.request({ id: "m2", type: "set_model", modelId: "missing" });
      assert.deepEqual({ id: unknown.id, ok: unknown.ok }, { id: "m2", ok: false });
      assert.match(unknown.error, /unknown model/);
    });
  });

  it("set_models replaces the model list in place and moves the session to the chosen model", async () => {
    await withBridge({ script: reply("ok") }, async ({ bridge, fake }) => {
      await prompt(bridge, "p1", "before");
      const sessionBefore = bridge.records.find((r) => r.event?.type === "agent_start");
      assert.ok(sessionBefore);

      const models = [
        { id: "profile-2", name: "Profile 2 (edited)", contextWindow: 32000, maxTokens: 2048 },
        { id: "profile-9", name: "Profile 9", contextWindow: 200000, maxTokens: 16000 },
      ];
      const updated = await bridge.request({ id: "u1", type: "set_models", models, modelId: "profile-9", thinkingLevel: "low" });
      assert.deepEqual(updated, { type: "response", id: "u1", ok: true });

      await prompt(bridge, "p2", "after");
      assert.equal(fake.requests[1].body.model, "profile-9");
      assert.equal(fake.requests[1].body.reasoning_effort, "low");
      assert.match(transcript(fake.requests[1]).join("\n"), /user: before/, "history survives the update");

      const removed = await bridge.request({ id: "u2", type: "set_model", modelId: "profile-1" });
      assert.equal(removed.ok, false, "a model dropped from the list can no longer be selected");
      const kept = await bridge.request({ id: "u3", type: "set_model", modelId: "profile-2" });
      assert.equal(kept.ok, true);

      const invalid = await bridge.request({ id: "u4", type: "set_models", models, modelId: "not-listed" });
      assert.deepEqual({ id: invalid.id, ok: invalid.ok }, { id: "u4", ok: false });
      const stillWorks = await prompt(bridge, "p3", "again");
      assert.equal(stillWorks.response.ok, true);
      assert.equal(fake.requests[2].body.model, "profile-2");
    });
  });

  it("keeps the running model object when set_models does not change its definition", async () => {
    await withBridge({ script: reply("ok") }, async ({ bridge, ws }) => {
      const before = bridge.records.length;
      const models = [
        { id: "profile-1", name: "Profile 1", contextWindow: 128000, maxTokens: 8192 },
        { id: "profile-7", name: "Profile 7", contextWindow: 8000, maxTokens: 1000 },
      ];
      const response = await bridge.request({ id: "u1", type: "set_models", models, modelId: "profile-1" });
      assert.equal(response.ok, true);
      assert.equal(bridge.events("thinking_level_changed", before).length, 0);
      await prompt(bridge, "p1", "go");
      const entries = readFileSync(ws.sessionFile, "utf8").trim().split("\n").map((line) => JSON.parse(line));
      assert.equal(entries.filter((e) => e.type === "model_change").length, 1, "an unchanged model adds no model_change entry");
    });
  });
});

describe("protocol robustness", () => {
  it("accepts CRLF and U+2028 inside a record, answers junk, and always echoes the id", async () => {
    await withBridge({ script: reply("ok") }, async ({ bridge, fake }) => {
      const message = "line one\u2028line two\u2029end";
      bridge.sendRaw(`${JSON.stringify({ id: "crlf", type: "prompt", message })}\r\n`);
      const answered = await bridge.waitFor((r) => r.id === "crlf" && r.type === "response", { label: "crlf answer" });
      assert.equal(answered.ok, true);
      assert.match(transcript(fake.requests[0]).join("\n"), /line one\u2028line two\u2029end/);

      const unknown = await bridge.request({ id: "x1", type: "frobnicate" });
      assert.deepEqual({ id: unknown.id, ok: unknown.ok }, { id: "x1", ok: false });
      assert.match(unknown.error, /unknown command type/);

      const from = bridge.records.length;
      bridge.sendRaw("{not json}\n[1,2]\n");
      await bridge.waitFor((r) => r.type === "response" && /JSON object/.test(r.error ?? ""), { from, label: "array rejection" });
      const parse = bridge.records.slice(from).find((r) => /invalid JSON/.test(r.error ?? ""));
      assert.equal(parse.ok, false);

      const empty = await bridge.request({ id: "x2", type: "prompt", message: "   " });
      assert.deepEqual({ id: empty.id, ok: empty.ok }, { id: "x2", ok: false });

      const after = await prompt(bridge, "p-after", "still alive");
      assert.equal(after.response.ok, true);
    });
  });

  it("exits 0 after shutdown, and when stdin closes", async () => {
    await withBridge({ script: reply("ok") }, async ({ bridge }) => {
      const answer = await bridge.request({ id: "bye", type: "shutdown" });
      assert.deepEqual(answer, { type: "response", id: "bye", ok: true });
      assert.equal(await bridge.exited, 0);
    });
    await withBridge({ script: reply("ok") }, async ({ bridge }) => {
      assert.equal(await bridge.close(), 0);
    });
  });
});
