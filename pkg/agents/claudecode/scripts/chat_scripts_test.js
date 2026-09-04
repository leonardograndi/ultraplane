"use strict";

const fs = require("fs");
const os = require("os");
const path = require("path");
const test = require("node:test");
const assert = require("node:assert/strict");

const { buildClaudeArgs, truncateStreamLine } = require("./chat_claude.js");
const { buildPrompt, interpretWaitResponse, runLoop, writeImageFiles } = require("./chat_loop.js");
const { handleRequest, parseFrames } = require("./mcp_bridge.js");

test("truncateStreamLine truncates long tool results and tool inputs", () => {
  const long = "x".repeat(2000);
  const raw = JSON.stringify({
    type: "assistant",
    message: {
      content: [
        { type: "tool_use", id: "t1", name: "bash", input: { command: long } },
      ],
    },
  });
  const truncated = JSON.parse(truncateStreamLine(raw));
  const input = truncated.message.content[0].input;
  assert.ok(JSON.stringify(input).length <= 900, "tool input should be truncated");
  assert.ok(String(input._truncated).endsWith("[truncated]"));

  const resultRaw = JSON.stringify({
    type: "user",
    message: { content: [{ type: "tool_result", tool_use_id: "t1", content: [{ type: "text", text: long }] }] },
  });
  const resultParsed = JSON.parse(truncateStreamLine(resultRaw));
  const text = resultParsed.message.content[0].content[0].text;
  assert.ok(text.length <= 900, "tool result text should be truncated");
  assert.ok(text.endsWith("[truncated]"));
});

test("truncateStreamLine leaves plain text lines untouched", () => {
  const raw = JSON.stringify({ type: "assistant", message: { content: [{ type: "text", text: "short" }] } });
  assert.equal(truncateStreamLine(raw), raw);
  assert.equal(truncateStreamLine("not json"), "not json");
});

test("buildClaudeArgs keeps mcp config, model, and continue", () => {
  const args = buildClaudeArgs("hello", "/tmp/mcp.runtime.json", "sonnet", true);
  assert.ok(args.includes("--mcp-config"));
  assert.ok(args.includes("/tmp/mcp.runtime.json"));
  assert.deepEqual(args.slice(args.indexOf("--model"), args.indexOf("--model") + 2), ["--model", "sonnet"]);
  assert.ok(args.includes("--continue"));
  assert.ok(args.includes("--"), "prompt must come after --");
});

test("buildClaudeArgs defaults to --bare and honors the host escape", () => {
  delete process.env.SUPERPLANE_CLAUDE_BARE;
  const bareArgs = buildClaudeArgs("hello", "/tmp/mcp.runtime.json", "", false);
  assert.equal(bareArgs[0], "--bare");

  process.env.SUPERPLANE_CLAUDE_BARE = "0";
  const hostArgs = buildClaudeArgs("hello", "/tmp/mcp.runtime.json", "", false);
  assert.ok(!hostArgs.includes("--bare"), "escape must drop --bare");
  assert.deepEqual(hostArgs.slice(0, 2), ["--settings", '{"hooks":{},"plugins":{}}']);
  delete process.env.SUPERPLANE_CLAUDE_BARE;
});
test("interpretWaitResponse maps 2xx, transient failures, and hard errors", () => {
  assert.deepEqual(interpretWaitResponse(200, { status: "idle" }), { status: "idle" });
  assert.deepEqual(interpretWaitResponse(503, { message: "boom" }), { status: "idle", retry_after: 1 });
  assert.deepEqual(
    interpretWaitResponse(429, { retryable: true, retry_after: 90 }),
    { status: "idle", retry_after: 60 },
  );
  assert.throws(() => interpretWaitResponse(401, { message: "unauthorized" }), /unauthorized/);
});

test("buildPrompt appends attached image paths", () => {
  assert.equal(buildPrompt("look", []), "look");
  const prompt = buildPrompt("look", ["/task/img-1.png"]);
  assert.ok(prompt.startsWith("look\n\n"));
  assert.ok(prompt.includes("- /task/img-1.png"));
});

test("writeImageFiles decodes base64 images with their extensions", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "chat-loop-"));
  const data = Buffer.from("fake-png").toString("base64");
  const paths = writeImageFiles(dir, [
    { media_type: "image/png", data },
    { media_type: "image/jpeg", data: "" },
  ]);
  assert.deepEqual(paths, [path.join(dir, "img-1.png")]);
  assert.equal(fs.readFileSync(paths[0], "utf8"), "fake-png");
  fs.rmSync(dir, { recursive: true, force: true });
});

test("runLoop bootstraps the cursor, runs the message, then fails hard", async () => {
  const ran = [];
  const cursors = [];
  await assert.rejects(
    runLoop({
      bootstrapCursor: async () => "m0",
      waitOnce: async (cursor) => {
        cursors.push(cursor);
        if (cursors.length === 1) {
          return { status: "message", id: "m1", content: "hello", images: [] };
        }
        throw new Error("unauthorized");
      },
      runPrompt: async (message) => {
        ran.push(message);
        return 0;
      },
      sleep: async () => {},
    }),
    /unauthorized/,
  );
  assert.deepEqual(cursors, ["m0", "m1"]);
  assert.deepEqual(ran, [{ status: "message", id: "m1", content: "hello", images: [] }]);
});

test("mcp_bridge parseFrames reads ndjson and lsp frames", () => {
  const ndjson = parseFrames(Buffer.from(`{"jsonrpc":"2.0","id":1,"method":"ping"}\n`));
  assert.equal(ndjson.messages.length, 1);
  assert.equal(ndjson.messages[0].method, "ping");

  const payload = Buffer.from(`{"jsonrpc":"2.0","id":2,"method":"ping"}`);
  const frame = Buffer.concat([
    Buffer.from(`Content-Length: ${payload.length}\r\n\r\n`),
    payload,
  ]);
  const lsp = parseFrames(frame);
  assert.equal(lsp.messages.length, 1);
  assert.equal(lsp.messages[0].id, 2);
});

function withCapturedStdout(run) {
  const original = process.stdout.write.bind(process.stdout);
  const chunks = [];
  process.stdout.write = (chunk) => {
    chunks.push(String(chunk));
    return true;
  };
  return Promise.resolve()
    .then(run)
    .finally(() => {
      process.stdout.write = original;
    })
    .then(() => chunks);
}

function stubFetch(responses) {
  const original = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (url, options) => {
    calls.push({ url: String(url), options });
    const response = responses.shift();
    return {
      ok: response.status >= 200 && response.status < 300,
      status: response.status,
      text: async () => JSON.stringify(response.body),
    };
  };
  return {
    calls,
    restore: () => {
      globalThis.fetch = original;
    },
  };
}

test("mcp_bridge lists tools and calls tools through the server", async () => {
  process.env.SUPERPLANE_BASE_URL = "https://superplane.example";
  process.env.SUPERPLANE_AGENT_SESSION_TOKEN = "token-1";
  // parseFrames flips a module-level reply format; start from ndjson.
  parseFrames(Buffer.from(`{"jsonrpc":"2.0","id":0,"method":"ping"}\n`));

  const fetchStub = stubFetch([
    { status: 200, body: { tools: [{ name: "superplane_app", description: "Run app actions", inputSchema: { type: "object" } }] } },
    { status: 200, body: { content: "patch applied", isError: false } },
  ]);
  try {
    const chunks = await withCapturedStdout(async () => {
      await handleRequest({ jsonrpc: "2.0", id: 1, method: "tools/list" });
      await handleRequest({
        jsonrpc: "2.0",
        id: 2,
        method: "tools/call",
        params: { name: "superplane_app", arguments: { action: "patch" } },
      });
    });

    assert.equal(fetchStub.calls[0].url, "https://superplane.example/api/v1/runner/agent-sessions/tools");
    const call = fetchStub.calls[1];
    assert.ok(call.url.endsWith("/api/v1/runner/agent-sessions/tools/call"));
    assert.equal(call.options.method, "POST");
    const sent = JSON.parse(call.options.body);
    assert.equal(sent.name, "superplane_app");
    assert.deepEqual(sent.input, { action: "patch" });

    const listResult = JSON.parse(chunks[0].trim());
    assert.equal(listResult.id, 1);
    assert.equal(listResult.result.tools[0].name, "superplane_app");
    const callResult = JSON.parse(chunks[1].trim());
    assert.equal(callResult.id, 2);
    assert.equal(callResult.result.content[0].text, "patch applied");
    assert.equal(callResult.result.isError, false);
  } finally {
    fetchStub.restore();
  }
});

test("buildClaudeArgs restricts MCP servers to the runtime config", () => {
  // Without --strict-mcp-config the CLI also loads the host's user-level MCP
  // servers. On a developer machine that pushed the chat prompt to ~202k
  // tokens (282 tools) and the second turn failed with "Prompt is too long".
  delete process.env.SUPERPLANE_CLAUDE_BARE;
  const bareArgs = buildClaudeArgs("hello", "/tmp/mcp.runtime.json", "", false);
  assert.ok(bareArgs.includes("--strict-mcp-config"), "--bare path must isolate MCP servers");

  process.env.SUPERPLANE_CLAUDE_BARE = "0";
  const hostArgs = buildClaudeArgs("hello", "/tmp/mcp.runtime.json", "", false);
  assert.ok(hostArgs.includes("--strict-mcp-config"), "host escape must isolate MCP servers");
  delete process.env.SUPERPLANE_CLAUDE_BARE;
});
