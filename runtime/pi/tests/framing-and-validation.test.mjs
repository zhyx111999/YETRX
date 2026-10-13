import assert from "node:assert/strict";
import { rmSync } from "node:fs";
import { after, describe, it } from "node:test";
import { createLineReader } from "../src/protocol/line-reader.mjs";
import { parseInit } from "../src/protocol/init-config.mjs";
import { parseHistory } from "../src/runtime/history-messages.mjs";
import { makeWorkspace, initCommand } from "./helpers/bridge-client.mjs";

function collect(chunks) {
  const lines = [];
  const reader = createLineReader((line) => lines.push(line));
  for (const chunk of chunks) reader.push(chunk);
  reader.end();
  return lines;
}

describe("LF framing", () => {
  it("splits only on LF and strips one preceding CR", () => {
    assert.deepEqual(collect(["a\nb\r\nc"]), ["a", "b", "c"]);
  });

  it("does not treat U+2028 / U+2029 as record boundaries", () => {
    const record = JSON.stringify({ message: "x\u2028y\u2029z" });
    assert.deepEqual(collect([`${record}\n`]), [record]);
  });

  it("reassembles records and multi-byte characters split across chunks", () => {
    const bytes = Buffer.from('{"m":"héllo 世界"}\n{"m":2}\n', "utf8");
    for (let cut = 1; cut < bytes.length; cut++) {
      assert.deepEqual(collect([bytes.subarray(0, cut), bytes.subarray(cut)]), ['{"m":"héllo 世界"}', '{"m":2}'], `cut at ${cut}`);
    }
  });

  it("skips blank lines", () => {
    assert.deepEqual(collect(["\n\r\n{}\n\n"]), ["{}"]);
  });
});

describe("init validation", () => {
  const ws = makeWorkspace();
  after(() => rmSync(ws.root, { recursive: true, force: true }));
  const valid = () => initCommand(ws, "http://127.0.0.1:1234/v1");

  it("accepts the documented init and normalises it", () => {
    const config = parseInit(valid());
    assert.equal(config.modelId, "profile-1");
    assert.deepEqual(config.tools, []);
    assert.equal(config.trustProject, false);
    assert.equal(config.history, undefined);
  });

  it("only talks to a loopback relay", () => {
    for (const baseUrl of ["http://example.com/v1", "https://10.0.0.5:8443", "ftp://127.0.0.1/", "not a url"]) {
      assert.throws(() => parseInit({ ...valid(), baseUrl }), /baseUrl/, baseUrl);
    }
    for (const baseUrl of ["http://127.0.0.1:1/v1", "http://localhost:80", "http://[::1]:9/v1"]) {
      assert.doesNotThrow(() => parseInit({ ...valid(), baseUrl }), baseUrl);
    }
  });

  it("rejects tokens pi would interpret as a command or an env reference, without echoing them", () => {
    for (const apiKey of ["!rm -rf /", "$HOME/secret-token", "short", "has space in it"]) {
      assert.throws(
        () => parseInit({ ...valid(), apiKey }),
        (error) => /apiKey/.test(error.message) && !error.message.includes(apiKey),
      );
    }
  });

  it("requires modelId to be one of models, and models to be well formed", () => {
    assert.throws(() => parseInit({ ...valid(), modelId: "nope" }), /modelId/);
    assert.throws(() => parseInit({ ...valid(), models: [] }), /models/);
    assert.throws(() => parseInit({ ...valid(), models: [{ id: "a", contextWindow: 0, maxTokens: 1 }] }), /contextWindow/);
    assert.throws(
      () => parseInit({ ...valid(), models: [{ id: "a", contextWindow: 1, maxTokens: 1 }, { id: "a", contextWindow: 1, maxTokens: 1 }], modelId: "a" }),
      /duplicates/,
    );
  });

  it("validates thinkingLevel, optional tools and absolute session paths", () => {
    assert.throws(() => parseInit({ ...valid(), thinkingLevel: "extreme" }), /thinkingLevel/);
    assert.throws(() => parseInit({ ...valid(), tools: ["rm"] }), /tools/);
    assert.throws(() => parseInit({ ...valid(), sessionFile: "relative.jsonl" }), /sessionFile/);
    assert.throws(() => parseInit({ ...valid(), cwd: `${ws.root}/missing` }), /cwd/);
    assert.deepEqual(parseInit({ ...valid(), tools: ["grep", "ls", "grep"] }).tools, ["grep", "ls"]);
  });
});

describe("history validation", () => {
  const usage = { input: 1, output: 1, cacheRead: 0, cacheWrite: 0, totalTokens: 2, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } };
  const assistant = { role: "assistant", content: [{ type: "text", text: "a" }], api: "openai-completions", provider: "x", model: "m", usage, stopReason: "stop" };

  it("keeps order, fills missing timestamps and leaves given ones alone", () => {
    const parsed = parseHistory([{ role: "user", content: "q" }, { ...assistant, timestamp: 5 }, { role: "user", content: [{ type: "text", text: "r" }] }]);
    assert.equal(parsed[1].timestamp, 5);
    assert.ok(parsed[0].timestamp < parsed[2].timestamp);
  });

  it("names the first bad message and writes nothing", () => {
    assert.throws(() => parseHistory([{ role: "user", content: "q" }, { ...assistant, usage: undefined }]), /history\[1\].*usage/);
    assert.throws(() => parseHistory([{ role: "system", content: "x" }]), /history\[0\].*role/);
    assert.throws(() => parseHistory([{ role: "toolResult", toolCallId: "c", toolName: "read", content: [], isError: "no" }]), /isError/);
    assert.throws(() => parseHistory("nope"), /array/);
  });
});
