import { createServer } from "node:http";

/**
 * A local stand-in for the Go relay: an OpenAI chat-completions endpoint that streams
 * scripted replies. No network, no model, no cost.
 *
 * `script(request, index)` returns the reply for the request (`index` counts from 1):
 *   { text, reasoning? }              stream text
 *   { toolCalls: [{ name, args }] }   stream tool calls
 *   { text, hang: true }              stream `text`, then stall until the client disconnects
 *   { status, error }                 fail before streaming
 *   { delayMs }                       may accompany any of the above
 *
 * Every request is recorded with its headers, parsed body and whether the client
 * disconnected before the reply finished.
 */
export async function startFakeOpenAI(script) {
  const requests = [];

  const server = createServer(async (req, res) => {
    if (req.method !== "POST" || !req.url.endsWith("/chat/completions")) {
      res.writeHead(404).end();
      return;
    }
    const raw = await new Promise((resolve) => {
      const chunks = [];
      req.on("data", (chunk) => chunks.push(chunk));
      req.on("end", () => resolve(Buffer.concat(chunks).toString("utf8")));
    });
    const record = { headers: req.headers, body: JSON.parse(raw), disconnected: false };
    requests.push(record);
    res.on("close", () => {
      record.disconnected = !res.writableEnded;
    });
    const reply = await script(record, requests.length);
    if (reply.delayMs) await new Promise((resolve) => setTimeout(resolve, reply.delayMs));
    if (reply.status) {
      res.writeHead(reply.status, { "content-type": "application/json" });
      res.end(JSON.stringify({ error: { message: reply.error ?? "failure", type: "server_error" } }));
      return;
    }
    await streamReply(res, record.body.model, reply);
  });

  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const { port } = server.address();
  return {
    baseUrl: `http://127.0.0.1:${port}/v1`,
    requests,
    close: () =>
      new Promise((resolve) => {
        server.closeAllConnections();
        server.close(resolve);
      }),
  };
}

const write = (res, payload) => res.write(`data: ${typeof payload === "string" ? payload : JSON.stringify(payload)}\n\n`);

function chunk(model, delta, finishReason = null) {
  return { id: "chatcmpl-fake", object: "chat.completion.chunk", created: 0, model, choices: [{ index: 0, delta, finish_reason: finishReason }] };
}

async function streamReply(res, model, reply) {
  res.writeHead(200, { "content-type": "text/event-stream", "cache-control": "no-cache" });
  write(res, chunk(model, { role: "assistant", content: "" }));
  if (reply.reasoning) write(res, chunk(model, { reasoning_content: reply.reasoning }));

  if (reply.toolCalls) {
    reply.toolCalls.forEach(({ name, args }, index) => {
      const json = JSON.stringify(args);
      const cut = Math.ceil(json.length / 2);
      write(res, chunk(model, { tool_calls: [{ index, id: `call_${index}_${name}`, type: "function", function: { name, arguments: json.slice(0, cut) } }] }));
      write(res, chunk(model, { tool_calls: [{ index, function: { arguments: json.slice(cut) } }] }));
    });
  } else if (reply.text) {
    const words = reply.text.split(/(?<= )/);
    for (const word of words) write(res, chunk(model, { content: word }));
  }

  if (reply.hang) {
    // Keep the stream open; the test ends it by disconnecting.
    await new Promise((resolve) => res.on("close", resolve));
    return;
  }
  write(res, chunk(model, {}, reply.toolCalls ? "tool_calls" : "stop"));
  write(res, { id: "chatcmpl-fake", object: "chat.completion.chunk", created: 0, model, choices: [], usage: { prompt_tokens: 12, completion_tokens: 5, total_tokens: 17 } });
  write(res, "[DONE]");
  res.end();
}
