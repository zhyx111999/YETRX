import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { isParentRequest, prompt, textFor, transcript, withBridge } from "./helpers/scenario.mjs";

const DELEGATE = { toolCalls: [{ name: "subagents", args: { tasks: [{ label: "worker", task: "say hello" }] } }] };

/**
 * A relay that plays a parent session delegating one task and a child that answers
 * `childReply`. The parent's replies are decided by what its request already contains.
 */
function delegatingRelay({ childReply, deliveredReply = { text: "ack: got the result" } }) {
  return (request) => {
    if (!isParentRequest(request)) return childReply;
    const everything = JSON.stringify(request.body.messages);
    if (everything.includes("what next")) return { text: "answered: what next" };
    if (everything.includes("Background workflow")) return deliveredReply;
    if (request.body.messages.some((message) => message.role === "tool")) return { text: "started it" };
    return DELEGATE;
  };
}

const childRequests = (fake) => fake.requests.filter((request) => !isParentRequest(request));

async function waitForChildRequest(fake) {
  for (let waited = 0; waited < 20000; waited += 50) {
    if (childRequests(fake).length > 0) return childRequests(fake)[0];
    await new Promise((resolve) => setTimeout(resolve, 50));
  }
  throw new Error("the subagent never reached the relay");
}

describe("the built-in deep-research workflow", () => {
  it("runs on the workflow package's own web_search/web_fetch tools; no Tavily anywhere", async () => {
    const tool = (name, args) => ({ toolCalls: [{ name, args }] });
    await withBridge(
      {
        script: (request) => {
          const everything = JSON.stringify(request.body.messages);
          if (isParentRequest(request)) {
            if (everything.includes("Background workflow")) return { text: "ack" };
            if (request.body.messages.some((message) => message.role === "tool")) return { text: "started" };
            return tool("subagents", { name: "deep-research", args: { question: "What is Pi?", angles: 1 } });
          }
          // The model never calls web_search, so this test makes no network request.
          return everything.includes("You are planning web research")
            ? tool("structured_output", { queries: ["what is pi"] })
            : tool("structured_output", { sources: [] });
        },
      },
      async ({ bridge, fake }) => {
        const { response } = await prompt(bridge, "p1", "research Pi");
        assert.equal(response.ok, true);
        const delivered = await bridge.waitFor(
          (r) => r.event?.type === "message_end" && r.event.message.customType === "workflow-result",
          { label: "deep-research result", timeoutMs: 45000 },
        );
        assert.match(JSON.stringify(delivered.event.message.content), /what is pi/);

        const researcher = childRequests(fake).find((request) => JSON.stringify(request.body.messages).includes("Research this query using the web_search"));
        const names = researcher.body.tools.map((t) => t.function.name);
        assert.ok(names.includes("web_search") && names.includes("web_fetch"), `researcher tools: ${names.join(", ")}`);
        assert.ok(!JSON.stringify(fake.requests.map((request) => request.body)).match(/tavily/i), "Tavily appears in no request");

        const parentTools = fake.requests.find(isParentRequest).body.tools.map((t) => t.function.name);
        assert.ok(!parentTools.some((name) => /web_|search/.test(name)), "the main agent has no web tools of its own");
      },
    );
  });
});

describe("background workflow results", () => {
  it("come back into the parent session after the prompt was answered, with no new command", async () => {
    await withBridge(
      { script: delegatingRelay({ childReply: { delayMs: 1500, text: "child says hello" } }) },
      async ({ bridge, fake }) => {
        assert.equal((await bridge.request({ id: "m1", type: "set_model", modelId: "profile-2" })).ok, true);
        const { response } = await prompt(bridge, "p1", "please delegate this");
        assert.deepEqual(response, { type: "response", id: "p1", ok: true });
        const afterResponse = bridge.records.length;

        // The subagent is still running and inherited the parent's current model.
        const child = await waitForChildRequest(fake);
        assert.equal(child.body.model, "profile-2", "a subagent without its own model uses the main model");
        assert.ok(!child.body.tools.some((tool) => tool.function.name === "subagents"), "subagents cannot spawn subagents");

        // The model list changes while the run is in flight; nothing about the run may break.
        const models = [
          { id: "profile-2", name: "Profile 2", contextWindow: 64000, maxTokens: 4096 },
          { id: "profile-4", name: "Profile 4", contextWindow: 100000, maxTokens: 8000 },
        ];
        const updated = await bridge.request({ id: "u1", type: "set_models", models, modelId: "profile-4" });
        assert.equal(updated.ok, true);

        const delivered = await bridge.waitFor(
          (r) => r.event?.type === "message_end" && r.event.message.customType === "workflow-result",
          { from: afterResponse, label: "delivered workflow result", timeoutMs: 45000 },
        );
        assert.match(JSON.stringify(delivered.event.message.content), /child says hello/, "the real subagent output is delivered");
        await bridge.waitFor((r) => r.event?.type === "agent_settled", { from: bridge.records.indexOf(delivered), label: "follow-up turn settled" });

        const parentRequests = fake.requests.filter(isParentRequest);
        const last = parentRequests.at(-1);
        assert.match(textFor(last, "user"), /Background workflow/);
        assert.equal(last.body.model, "profile-4", "the delivered turn runs on the updated model");
        assert.ok(
          !bridge.records.slice(afterResponse).some((r) => r.type === "response" && r.id === "p1"),
          "no second answer for the prompt: the follow-up turn is spontaneous",
        );
        const finalText = bridge.events("message_end", afterResponse).filter((e) => e.message.role === "assistant").at(-1);
        assert.equal(finalText.message.content[0].text, "ack: got the result");
      },
    );
  });

  it("queue a user prompt that arrives while the delivered turn is running", async () => {
    await withBridge(
      {
        script: delegatingRelay({
          childReply: { delayMs: 200, text: "child says hello" },
          deliveredReply: { delayMs: 2500, text: "ack: got the result" },
        }),
      },
      async ({ bridge, fake }) => {
        await prompt(bridge, "p1", "please delegate this");
        const afterResponse = bridge.records.length;
        const started = await bridge.waitFor((r) => r.event?.type === "agent_start", { from: afterResponse, label: "spontaneous run", timeoutMs: 45000 });

        const queued = await bridge.request({ id: "p2", type: "prompt", message: "what next" });
        assert.deepEqual(queued, { type: "response", id: "p2", ok: true });
        const answered = await bridge.waitFor(
          (r) => r.event?.type === "message_end" && r.event.message.role === "assistant" && r.event.message.content[0]?.text === "answered: what next",
          { from: bridge.records.indexOf(started), label: "queued prompt answered", timeoutMs: 45000 },
        );
        assert.ok(answered);
        assert.match(transcript(fake.requests.filter(isParentRequest).at(-1)).join("\n"), /user: what next/);
      },
    );
  });

  it("are paused, not left hanging, when the host shuts down mid-run", async () => {
    await withBridge(
      { script: delegatingRelay({ childReply: { text: "still working ", hang: true } }) },
      async ({ bridge, fake }) => {
        await prompt(bridge, "p1", "please delegate this");
        await waitForChildRequest(fake);
        const answer = await bridge.request({ id: "bye", type: "shutdown" }, { timeoutMs: 30000 });
        assert.deepEqual(answer, { type: "response", id: "bye", ok: true });
        assert.equal(await bridge.exited, 0);
      },
    );
  });
});
