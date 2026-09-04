package claudecode

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/superplanehq/superplane/pkg/agents"
)

func TestMapStreamLineAssistantText(t *testing.T) {
	t.Parallel()

	events := mapStreamLine(`{"type":"assistant","message":{"id":"msg_01","content":[{"type":"text","text":"Hello there"}]}}`)
	require.Len(t, events, 1)

	event := events[0]
	assert.Equal(t, agents.ProviderEventAssistantMessage, event.Type)
	assert.Equal(t, "ast-msg_01", event.ProviderEventID)
	assert.Equal(t, "Hello there", event.Text)
}

func TestMapStreamLineAssistantToolUse(t *testing.T) {
	t.Parallel()

	events := mapStreamLine(`{"type":"assistant","message":{"id":"msg_02","content":[` +
		`{"type":"tool_use","id":"toolu_01","name":"mcp__superplane__patch_staging","input":{"command":"ls"}}]}}`)
	require.Len(t, events, 1)

	event := events[0]
	assert.Equal(t, agents.ProviderEventToolUseStarted, event.Type)
	assert.Equal(t, "mcp__superplane__patch_staging", event.ToolName)
	assert.Equal(t, "toolu_01", event.ToolCallID)
	assert.Equal(t, "ls", event.ToolInput)
}

func TestMapStreamLineUserToolResult(t *testing.T) {
	t.Parallel()

	events := mapStreamLine(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_01","is_error":false}]}}`)
	require.Len(t, events, 1)
	assert.Equal(t, agents.ProviderEventToolUseFinished, events[0].Type)
	assert.Equal(t, "toolu_01", events[0].ToolCallID)
	assert.Empty(t, events[0].ErrorMessage)

	events = mapStreamLine(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_02","is_error":true}]}}`)
	require.Len(t, events, 1)
	assert.Equal(t, "tool failed", events[0].ErrorMessage)
}

func TestMapStreamLineResultUsage(t *testing.T) {
	t.Parallel()

	events := mapStreamLine(`{"type":"result","subtype":"success","model":"claude-sonnet-4-6",` +
		`"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":2,"cache_creation_input_tokens":1}}`)
	require.Len(t, events, 1)

	event := events[0]
	assert.Equal(t, agents.ProviderEventTurnCompleted, event.Type)
	assert.Equal(t, "claude-sonnet-4-6", event.Model)
	require.NotNil(t, event.Usage)
	assert.Equal(t, int64(10), event.Usage.InputTokens)
	assert.Equal(t, int64(5), event.Usage.OutputTokens)
	assert.Equal(t, int64(2), event.Usage.CacheReadTokens)
	assert.Equal(t, int64(1), event.Usage.CacheWriteTokens)
	assert.Equal(t, int64(18), event.Usage.TotalTokens)
}

func TestMapStreamLineResultError(t *testing.T) {
	t.Parallel()

	events := mapStreamLine(`{"type":"result","subtype":"error_during_execution","is_error":true}`)
	require.Len(t, events, 1)
	assert.Equal(t, agents.ProviderEventSessionFailed, events[0].Type)
}

func TestMapStreamLineSkipsDeltas(t *testing.T) {
	t.Parallel()

	assert.Empty(t, mapStreamLine(`{"type":"stream_event","event":{"type":"content_block_delta"}}`))
	assert.Empty(t, mapStreamLine(`{"type":"system","subtype":"init"}`))
	assert.Empty(t, mapStreamLine("not json at all"))
}

func TestMapStreamLineRedactsJWTs(t *testing.T) {
	t.Parallel()

	events := mapStreamLine(`{"type":"assistant","message":{"id":"msg_03","content":[{"type":"text",` +
		`"text":"token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjMifQ.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c leaked"}]}}`)
	require.Len(t, events, 1)
	assert.Contains(t, events[0].Text, "<redacted>")
	assert.NotContains(t, events[0].Text, "SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c")
}

func TestMapStreamLineResultErrorKeepsProviderText(t *testing.T) {
	t.Parallel()

	// The CLI reports why the turn died in `result` (e.g. "Prompt is too
	// long"). Dropping it left users with an opaque failure in the chat.
	events := mapStreamLine(`{"type":"result","subtype":"success","is_error":true,"result":"Prompt is too long"}`)
	require.Len(t, events, 1)
	assert.Equal(t, agents.ProviderEventSessionFailed, events[0].Type)
	assert.Equal(t, "Prompt is too long", events[0].ErrorMessage)
}

func TestMapStreamLineResultErrorRedactsAndFallsBack(t *testing.T) {
	t.Parallel()

	events := mapStreamLine(`{"type":"result","is_error":true,"result":"  "}`)
	require.Len(t, events, 1)
	assert.Equal(t, "claude code turn failed", events[0].ErrorMessage)

	leaked := mapStreamLine(`{"type":"result","is_error":true,"result":` +
		`"boom eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjMifQ.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"}`)
	require.Len(t, leaked, 1)
	assert.Contains(t, leaked[0].ErrorMessage, "<redacted>")
}
