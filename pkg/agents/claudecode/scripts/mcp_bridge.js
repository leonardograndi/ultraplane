#!/usr/bin/env node
"use strict";

/**
 * Stdio MCP server for canvas chat. Forwards tools/list and tools/call to
 * the public SuperPlane runner endpoints, so every agent tool runs on the
 * server. Talks to SuperPlane with SUPERPLANE_BASE_URL +
 * SUPERPLANE_AGENT_SESSION_TOKEN.
 */

function readEnv(name) {
  const value = String(process.env[name] || "").trim();
  if (!value) {
    throw new Error(`${name} is required`);
  }
  return value;
}
// Tool calls should answer fast; a dead connection surfaces as a tool error
// instead of hanging the CLI.
const REQUEST_TIMEOUT_MS = 30000;

async function requestJSON(method, urlPath, body) {
  const baseURL = readEnv("SUPERPLANE_BASE_URL").replace(/\/$/, "");
  const token = readEnv("SUPERPLANE_AGENT_SESSION_TOKEN");
  const headers = {
    Authorization: `Bearer ${token}`,
    Accept: "application/json",
  };
  const options = { method, headers, signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS) };
  if (body !== undefined) {
    headers["Content-Type"] = "application/json";
    options.body = JSON.stringify(body);
  }
  const response = await fetch(`${baseURL}${urlPath}`, options);
  const text = await response.text();
  let parsed = {};
  if (text) {
    try {
      parsed = JSON.parse(text);
    } catch {
      parsed = { message: text };
    }
  }
  if (!response.ok) {
    const message = parsed.message || parsed.error || text || `HTTP ${response.status}`;
    const error = new Error(message);
    error.status = response.status;
    throw error;
  }
  return parsed;
}

async function listTools() {
  const parsed = await requestJSON("GET", "/api/v1/runner/agent-sessions/tools");
  const tools = Array.isArray(parsed && parsed.tools) ? parsed.tools : [];
  return tools
    .filter((tool) => tool && tool.name)
    .map((tool) => ({
      name: String(tool.name),
      description: String(tool.description || ""),
      inputSchema: tool.inputSchema && typeof tool.inputSchema === "object" ? tool.inputSchema : { type: "object" },
    }));
}

async function callTool(name, input, callId) {
  return requestJSON("POST", "/api/v1/runner/agent-sessions/tools/call", {
    name,
    input: input && typeof input === "object" ? input : {},
    call_id: callId || "",
  });
}

let replyFormat = "ndjson";

function writeMessage(message) {
  const encoded = JSON.stringify(message);
  if (replyFormat === "lsp") {
    const payload = Buffer.from(encoded, "utf8");
    process.stdout.write(`Content-Length: ${payload.length}\r\n\r\n`);
    process.stdout.write(payload);
    return;
  }
  process.stdout.write(`${encoded}\n`);
}

function sendResult(id, result) {
  writeMessage({ jsonrpc: "2.0", id, result });
}

function sendError(id, code, message) {
  writeMessage({ jsonrpc: "2.0", id, error: { code, message } });
}

async function handleRequest(message) {
  const { id, method, params } = message;
  if (method === "initialize") {
    sendResult(id, {
      protocolVersion: "2024-11-05",
      capabilities: { tools: {} },
      serverInfo: { name: "superplane", version: "1.0.0" },
    });
    return;
  }
  if (method === "notifications/initialized" || method === "notifications/cancelled") {
    return;
  }
  if (method === "ping") {
    sendResult(id, {});
    return;
  }
  if (method === "tools/list") {
    try {
      sendResult(id, { tools: await listTools() });
    } catch (err) {
      sendError(id, -32603, err && err.message ? err.message : String(err));
    }
    return;
  }
  if (method === "tools/call") {
    const name = params && params.name;
    const args = (params && params.arguments) || {};
    try {
      const result = await callTool(name, args);
      sendResult(id, {
        content: [{ type: "text", text: String(result && result.content) || "" }],
        isError: Boolean(result && result.isError),
      });
    } catch (err) {
      sendResult(id, {
        content: [{ type: "text", text: err && err.message ? err.message : String(err) }],
        isError: true,
      });
    }
    return;
  }
  if (id != null) {
    sendError(id, -32601, `Unknown method: ${method}`);
  }
}

function parseFrames(buffer) {
  const messages = [];
  let rest = skipASCIIWhitespace(buffer);
  while (rest.length > 0) {
    const peek = rest.toString("utf8", 0, Math.min(rest.length, 16));
    if (/^content-length:/i.test(peek)) {
      const parsed = parseContentLengthFrame(rest);
      if (!parsed) {
        break;
      }
      replyFormat = "lsp";
      if (parsed.message) {
        messages.push(parsed.message);
      }
      rest = skipASCIIWhitespace(parsed.rest);
      continue;
    }
    if (rest[0] === 0x7b) {
      const parsed = parseNDJSONFrame(rest);
      if (!parsed) {
        break;
      }
      replyFormat = "ndjson";
      if (parsed.message) {
        messages.push(parsed.message);
      }
      rest = skipASCIIWhitespace(parsed.rest);
      continue;
    }
    rest = rest.slice(1);
  }
  return { messages, rest };
}

function skipASCIIWhitespace(buffer) {
  let index = 0;
  while (index < buffer.length && (buffer[index] === 0x09 || buffer[index] === 0x0a || buffer[index] === 0x0d || buffer[index] === 0x20)) {
    index += 1;
  }
  return index === 0 ? buffer : buffer.slice(index);
}

function parseContentLengthFrame(buffer) {
  const headerEnd = buffer.indexOf("\r\n\r\n");
  if (headerEnd < 0) {
    return null;
  }
  const header = buffer.slice(0, headerEnd).toString("utf8");
  const match = header.match(/Content-Length:\s*(\d+)/i);
  if (!match) {
    return { message: null, rest: buffer.slice(headerEnd + 4) };
  }
  const length = Number(match[1]);
  const bodyStart = headerEnd + 4;
  if (buffer.length < bodyStart + length) {
    return null;
  }
  const body = buffer.slice(bodyStart, bodyStart + length).toString("utf8");
  let message = null;
  try {
    message = JSON.parse(body);
  } catch {
    // Ignore malformed frames.
  }
  return { message, rest: buffer.slice(bodyStart + length) };
}

function parseNDJSONFrame(buffer) {
  const newline = buffer.indexOf(0x0a);
  if (newline < 0) {
    return null;
  }
  const line = buffer.slice(0, newline).toString("utf8").trim();
  let message = null;
  if (line) {
    try {
      message = JSON.parse(line);
    } catch {
      // Ignore malformed frames.
    }
  }
  return { message, rest: buffer.slice(newline + 1) };
}

async function main() {
  let buffer = Buffer.alloc(0);
  process.stdin.on("data", (chunk) => {
    buffer = Buffer.concat([buffer, chunk]);
    const parsed = parseFrames(buffer);
    buffer = parsed.rest;
    for (const message of parsed.messages) {
      Promise.resolve(handleRequest(message)).catch((err) => {
        if (message && message.id != null) {
          sendError(message.id, -32603, err && err.message ? err.message : String(err));
        }
      });
    }
  });
}

if (require.main === module) {
  main();
}

module.exports = { handleRequest, parseFrames };
