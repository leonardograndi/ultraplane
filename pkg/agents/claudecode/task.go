package claudecode

import (
	"fmt"

	runneraction "github.com/superplanehq/superplane/pkg/components/runner"
)

// agentSessionTaskLabel marks broker tasks that back canvas chat sessions, so
// operators can find chat loops among runner tasks.
const agentSessionTaskLabel = "sp_agent_session"

// chatTaskFiles are materialized under SUPERPLANE_TASK_DIR before execution.
func chatTaskFiles() []runneraction.BrokerTaskFile {
	return []runneraction.BrokerTaskFile{
		{Path: "chat_claude.js", Content: chatClaudeScript, Mode: "0644"},
		{Path: "chat_loop.js", Content: chatLoopScript, Mode: "0644"},
		{Path: "mcp_bridge.js", Content: mcpBridgeScript, Mode: "0644"},
	}
}

// chatTaskCommands checks the claude CLI and node up front, then runs the
// long-poll chat loop.
func chatTaskCommands(model string) []runneraction.BrokerCommand {
	prepare := runneraction.NodePrepareScript("claude", "claude CLI not found on PATH; install Claude Code on the runner and log in", "")
	loop := fmt.Sprintf(`node "$SUPERPLANE_TASK_DIR/chat_loop.js" %s`, runneraction.ShellSingleQuote(model))
	return []runneraction.BrokerCommand{
		{Name: "Prepare Claude Code", Command: prepare},
		{Name: "Chat loop", Command: loop, Kind: runneraction.LiveLogKindPrompt, Preview: "Wait for chat messages"},
	}
}
