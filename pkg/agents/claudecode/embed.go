package claudecode

import _ "embed"

//go:embed scripts/chat_claude.js
var chatClaudeScript string

//go:embed scripts/chat_loop.js
var chatLoopScript string

//go:embed scripts/mcp_bridge.js
var mcpBridgeScript string
