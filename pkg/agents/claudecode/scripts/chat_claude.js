#!/usr/bin/env node
"use strict";

/**
 * Run one Claude Code chat turn and forward each stream-json line as a
 * structured live-log record for the agent stream worker.
 *
 *   node chat_claude.js <prompt-file> [model]
 *
 * The CLI authenticates with the Claude Code login persisted on the runner;
 * no API key is injected into the environment.
 */

const fs = require("fs");
const path = require("path");
const readline = require("readline");
const { spawn, spawnSync } = require("child_process");

const TOOL_RESULT_MAX_CHARS = 800;
const STDERR_TAIL_MAX_CHARS = 2000;

const SYSTEM_PROMPT =
  "Write all assistant messages as plain terminal text. " +
  "Do not use Markdown: no bold/italic markers, headings, links, tables, or fenced code blocks. " +
  "Prefer plain paths, shell commands, and simple indentation.";

const CHAT_SYSTEM_PROMPT =
  " This is a SuperPlane canvas chat. Use the superplane MCP tools to inspect or change the canvas. " +
  "Answer in the language the user writes in.";

const BASE_ALLOWED_TOOLS = "Bash,Read,Edit,Write";
const MCP_ALLOWED_TOOLS = "mcp__superplane";

let seq = 0;

function claudePermissionMode() {
  return "bypassPermissions";
}

function allowedClaudeTools() {
  return [BASE_ALLOWED_TOOLS, MCP_ALLOWED_TOOLS].join(",");
}

function writeAgentEvent(message) {
  writeLiveLogRecord({ type: "sp.agent.event", index: seq++, message });
}

function writeAgentFailure(message) {
  writeLiveLogRecord({ type: "sp.agent.failed", index: seq++, message });
}

function writeLiveLogRecord(rec) {
  process.stdout.write(`${JSON.stringify(rec)}\n`);
}

function truncateText(text) {
  if (text.length <= TOOL_RESULT_MAX_CHARS) {
    return text;
  }
  return `${text.slice(0, TOOL_RESULT_MAX_CHARS)}… [truncated]`;
}

function truncateToolInput(input) {
  const raw = JSON.stringify(input);
  if (raw.length <= TOOL_RESULT_MAX_CHARS) {
    return input;
  }
  return { _truncated: truncateText(raw) };
}

function truncateToolResultContent(content) {
  if (!Array.isArray(content)) {
    return typeof content === "string" ? truncateText(content) : content;
  }
  return content.map((block) => {
    if (block && typeof block === "object" && block.type === "text" && typeof block.text === "string") {
      return { ...block, text: truncateText(block.text) };
    }
    return block;
  });
}

/** Truncate tool inputs and tool results so live-log records stay small. */
function truncateStreamLine(raw) {
  let event;
  try {
    event = JSON.parse(raw);
  } catch {
    return raw;
  }
  if (!event || typeof event !== "object") {
    return raw;
  }
  const content = event.message && Array.isArray(event.message.content) ? event.message.content : null;
  if (!content) {
    return raw;
  }
  let changed = false;
  const mapped = content.map((block) => {
    if (!block || typeof block !== "object") {
      return block;
    }
    if (block.type === "tool_use" && block.input !== undefined) {
      changed = true;
      return { ...block, input: truncateToolInput(block.input) };
    }
    if (block.type === "tool_result" && block.content !== undefined) {
      changed = true;
      return { ...block, content: truncateToolResultContent(block.content) };
    }
    return block;
  });
  if (!changed) {
    return raw;
  }
  return JSON.stringify({ ...event, message: { ...event.message, content: mapped } });
}

function commandExists(name) {
  const result = spawnSync("sh", ["-c", `command -v ${name}`], { encoding: "utf8" });
  return result.status === 0;
}

function buildClaudeArgs(prompt, mcpConfigPath, model, continueSession) {
  const args = [
    "-p",
    "--output-format",
    "stream-json",
    "--verbose",
    "--include-partial-messages",
    "--permission-mode",
    claudePermissionMode(),
    "--add-dir",
    ".",
    "--append-system-prompt",
    SYSTEM_PROMPT + CHAT_SYSTEM_PROMPT,
    "--mcp-config",
    mcpConfigPath,
    // Without this the CLI merges the host's user-level MCP servers into the
    // prompt. On a dev machine that meant 282 tools and a ~202k-token prompt,
    // so the second turn failed with "Prompt is too long".
    "--strict-mcp-config",
    "--allowedTools",
    allowedClaudeTools(),
  ];
  // --bare skips user settings and fits the runner's persisted login (bare
  // never reads OAuth/Keychain). Hosts where the CLI must use a local
  // interactive login (macOS Keychain) disable it with SUPERPLANE_CLAUDE_BARE=0;
  // host hooks and plugins are then isolated so they cannot grow the prompt.
  if (String(process.env.SUPERPLANE_CLAUDE_BARE || "") !== "0") {
    args.unshift("--bare");
  } else {
    args.unshift("--settings", '{"hooks":{},"plugins":{}}');
  }
  if (model) {
    args.push("--model", model);
  }
  if (continueSession) {
    args.push("--continue");
  }
  args.push("--", prompt);
  return args;
}

function main() {
  const args = process.argv.slice(2);
  if (args.length < 1) {
    console.error("usage: node chat_claude.js <prompt-file> [model]");
    process.exit(2);
  }
  runTurn(args[0], args[1] || "")
    .then((code) => process.exit(code))
    .catch((err) => {
      console.error(err && err.message ? err.message : err);
      process.exit(1);
    });
}

async function runTurn(promptFile, model) {
  const taskDir = process.env.SUPERPLANE_TASK_DIR;
  if (!taskDir) {
    throw new Error("SUPERPLANE_TASK_DIR is required");
  }

  const prompt = fs.readFileSync(promptFile, "utf8");
  const promptCountPath = path.join(taskDir, "prompt_count");
  const promptCount = fs.existsSync(promptCountPath)
    ? Number.parseInt(fs.readFileSync(promptCountPath, "utf8").trim(), 10) || 0
    : 0;

  const mcpConfigPath = path.join(taskDir, "mcp.runtime.json");
  fs.writeFileSync(
    mcpConfigPath,
    `${JSON.stringify({
      mcpServers: {
        superplane: {
          command: "node",
          args: [path.join(taskDir, "mcp_bridge.js")],
        },
      },
    })}\n`,
  );

  const claudeArgs = buildClaudeArgs(prompt, mcpConfigPath, model, promptCount > 0);
  let command = "claude";
  let args = claudeArgs;
  if (commandExists("stdbuf")) {
    command = "stdbuf";
    args = ["-oL", "-eL", "claude", ...claudeArgs];
  }

  let stderrTail = "";
  let sawResult = false;
  let resultLine = "";
  const child = spawn(command, args, { stdio: ["ignore", "pipe", "pipe"] });
  child.stderr.on("data", (chunk) => {
    stderrTail = (stderrTail + chunk.toString("utf8")).slice(-STDERR_TAIL_MAX_CHARS);
  });

  const rl = readline.createInterface({ input: child.stdout, crlfDelay: Infinity });
  rl.on("line", (raw) => {
    if (!sawResult) {
      try {
        const event = JSON.parse(raw);
        if (event && event.type === "result") {
          sawResult = true;
          resultLine = raw;
        }
      } catch {
        // Non-JSON lines are forwarded as they are.
      }
    }
    writeAgentEvent(truncateStreamLine(raw));
  });

  const exitCode = await Promise.all([
    new Promise((resolve, reject) => {
      child.on("error", reject);
      child.on("close", (code) => resolve(code == null ? 1 : code));
    }),
    new Promise((resolve) => rl.on("close", resolve)),
  ]).then(([code]) => code);

  if (exitCode !== 0 && !sawResult) {
    writeAgentFailure(truncateText(stderrTail.trim() || `claude exited with code ${exitCode}`));
  }

  const resultFile = process.env.SUPERPLANE_RESULT_FILE;
  if (resultFile) {
    fs.writeFileSync(resultFile, `${resultLine || "{}"}\n`);
  }
  accumulateLLMUsage(resultLine, model);
  fs.writeFileSync(promptCountPath, `${promptCount + 1}\n`);
  return exitCode;
}

function accumulateLLMUsage(raw, fallbackModel) {
  const taskDir = process.env.SUPERPLANE_TASK_DIR;
  if (!taskDir || !raw) {
    return;
  }
  const script = path.join(taskDir, "llm_usage.js");
  if (!fs.existsSync(script)) {
    return;
  }
  let parsed = {};
  try {
    parsed = JSON.parse(raw);
  } catch {
    return;
  }
  require(script).accumulate(taskDir, {
    model: parsed.model || fallbackModel,
    usage: parsed.usage,
    total_cost_usd: parsed.total_cost_usd,
  });
}

if (require.main === module) {
  main();
}

module.exports = { buildClaudeArgs, truncateStreamLine, truncateToolInput, truncateToolResultContent };
