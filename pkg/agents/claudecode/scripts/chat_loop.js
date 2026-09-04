#!/usr/bin/env node
"use strict";

/**
 * Chat-session loop. Bootstraps the message cursor, then long-polls SuperPlane
 * for user messages and runs each one as a Claude Code turn (chat_claude.js,
 * --continue inside the task workspace).
 *
 * Environment: SUPERPLANE_BASE_URL, SUPERPLANE_AGENT_SESSION_TOKEN,
 * SUPERPLANE_TASK_DIR, optional SUPERPLANE_AGENT_MODEL.
 */

const fs = require("fs");
const path = require("path");
const { spawn } = require("child_process");

const HOLD_SECONDS = 45;
const MAX_RETRY_WAIT_SECONDS = 60;
const DEFAULT_RETRY_WAIT_SECONDS = 1;

const IMAGE_EXTENSIONS = {
  "image/png": "png",
  "image/jpeg": "jpg",
  "image/gif": "gif",
  "image/webp": "webp",
};

function readEnv(name) {
  const value = String(process.env[name] || "").trim();
  if (!value) {
    throw new Error(`${name} is required`);
  }
  return value;
}

function isTransientWaitFailure(status, parsed) {
  if (status === 429 || status === 502 || status === 503 || status === 504) {
    return true;
  }
  return Boolean(parsed && (parsed.retryable === true || parsed.cloudflare_error === true));
}

function retryWaitSeconds(parsed) {
  const n = Number(parsed && parsed.retry_after);
  if (!Number.isFinite(n) || n < 1) {
    return DEFAULT_RETRY_WAIT_SECONDS;
  }
  return Math.min(MAX_RETRY_WAIT_SECONDS, Math.floor(n));
}

function interpretWaitResponse(status, parsed, text) {
  if (status >= 200 && status < 300) {
    return parsed && typeof parsed === "object" ? parsed : {};
  }
  if (isTransientWaitFailure(status, parsed)) {
    return { status: "idle", retry_after: retryWaitSeconds(parsed) };
  }
  const error = new Error((parsed && (parsed.message || parsed.error)) || text || `HTTP ${status}`);
  error.status = status;
  throw error;
}

// A dropped or half-open connection must not hang the loop: the wait holds
// 45s server-side, so anything past 75s is a dead connection, retried like a
// transient failure.
const REQUEST_TIMEOUT_MS = 75000;

async function requestJSON(urlPath) {
  const baseURL = readEnv("SUPERPLANE_BASE_URL").replace(/\/$/, "");
  const token = readEnv("SUPERPLANE_AGENT_SESSION_TOKEN");
  let response;
  try {
    response = await fetch(`${baseURL}${urlPath}`, {
      method: "GET",
      headers: {
        Authorization: `Bearer ${token}`,
        Accept: "application/json",
      },
      signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
    });
  } catch (err) {
    if (err && (err.name === "TimeoutError" || err.name === "AbortError")) {
      return { status: "idle", retry_after: DEFAULT_RETRY_WAIT_SECONDS };
    }
    throw err;
  }
  const text = await response.text();
  let parsed = {};
  if (text) {
    try {
      parsed = JSON.parse(text);
    } catch {
      parsed = { message: text };
    }
  }
  return interpretWaitResponse(response.status, parsed, text);
}

async function bootstrapCursor() {
  const parsed = await requestJSON(`/api/v1/runner/agent-sessions/wait?latest=1`);
  return String((parsed && parsed.id) || "");
}

async function waitOnce(cursor) {
  return requestJSON(`/api/v1/runner/agent-sessions/wait?hold_seconds=${HOLD_SECONDS}&after=${encodeURIComponent(cursor)}`);
}

function writeImageFiles(taskDir, images) {
  const paths = [];
  (Array.isArray(images) ? images : []).forEach((image, index) => {
    const mediaType = String((image && image.media_type) || "").toLowerCase();
    const extension = IMAGE_EXTENSIONS[mediaType] || "bin";
    const data = String((image && image.data) || "");
    if (!data) {
      return;
    }
    const file = path.join(taskDir, `img-${index + 1}.${extension}`);
    fs.writeFileSync(file, Buffer.from(data, "base64"));
    paths.push(file);
  });
  return paths;
}

function buildPrompt(content, imagePaths) {
  const text = String(content || "").trim();
  if (imagePaths.length === 0) {
    return text;
  }
  const list = imagePaths.map((file) => `- ${file}`).join("\n");
  return `${text}\n\nThe user attached these image files in the task workspace:\n${list}`;
}

function writePrompt(taskDir, text) {
  const dir = path.join(taskDir, "prompts");
  fs.mkdirSync(dir, { recursive: true });
  const file = path.join(dir, `chat-${Date.now()}.txt`);
  fs.writeFileSync(file, `${text}\n`);
  return file;
}

function runPromptFile(taskDir, promptFile, model) {
  return new Promise((resolve, reject) => {
    const child = spawn(process.execPath, [path.join(taskDir, "chat_claude.js"), promptFile, model || ""], {
      stdio: "inherit",
    });
    child.on("error", reject);
    child.on("close", (code) => resolve(code == null ? 1 : code));
  });
}

function defaultSleep(ms) {
  return new Promise((resolve) => {
    setTimeout(resolve, ms);
  });
}

async function runLoop(helpers) {
  const wait = helpers.waitOnce;
  const bootstrap = helpers.bootstrapCursor;
  const runPrompt = helpers.runPrompt;
  const sleep = helpers.sleep || defaultSleep;
  let cursor = await bootstrap();
  while (true) {
    const result = await wait(cursor);
    if (result && result.status === "message" && result.id) {
      cursor = String(result.id);
      const code = await runPrompt(result);
      if (code !== 0) {
        process.stderr.write(`chat turn failed with exit ${code}; waiting for the next message\n`);
      }
      continue;
    }
    const seconds = Number(result && result.retry_after);
    if (Number.isFinite(seconds) && seconds > 0) {
      process.stderr.write(`agent wait hit a transient error; retrying in ${seconds}s\n`);
      await sleep(seconds * 1000);
    }
  }
}

async function main() {
  const taskDir = readEnv("SUPERPLANE_TASK_DIR");
  const model = String(process.env.SUPERPLANE_AGENT_MODEL || "").trim();
  await runLoop({
    bootstrapCursor,
    waitOnce,
    runPrompt: (message) => {
      const imagePaths = writeImageFiles(taskDir, message.images);
      const prompt = buildPrompt(message.content, imagePaths);
      return runPromptFile(taskDir, writePrompt(taskDir, prompt), model);
    },
  });
}

if (require.main === module) {
  main().catch((err) => {
    process.stderr.write(`${err && err.message ? err.message : err}\n`);
    process.exit(1);
  });
}

module.exports = { bootstrapCursor, buildPrompt, interpretWaitResponse, runLoop, writeImageFiles };
