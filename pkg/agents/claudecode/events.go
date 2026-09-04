package claudecode

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/superplanehq/superplane/pkg/agents"
)

// streamMessage mirrors the Claude Code stream-json `message` envelope for
// assistant and user events.
type streamMessage struct {
	ID      string           `json:"id"`
	Content []streamBlockAny `json:"content"`
	Model   string           `json:"model"`
	Usage   *streamUsage     `json:"usage,omitempty"`
}

type streamBlockAny struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type streamUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

type streamEvent struct {
	Type    string          `json:"type"`
	Message json.RawMessage `json:"message"`
	Model   string          `json:"model"`
	IsError bool            `json:"is_error"`
	// Result carries the CLI's own explanation of a failed turn, e.g.
	// "Prompt is too long". Surfacing it beats a generic failure message.
	Result string       `json:"result,omitempty"`
	Usage  *streamUsage `json:"usage,omitempty"`
}

// mapStreamLine translates one Claude Code stream-json line into provider
// events. Uninteresting lines (stream deltas, init) map to nothing.
func mapStreamLine(raw string) []agents.ProviderEvent {
	var event streamEvent
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		return nil
	}

	switch event.Type {
	case "assistant":
		return assistantEvents(event)
	case "user":
		return toolResultEvents(event)
	case "result":
		if event.IsError {
			return []agents.ProviderEvent{{
				Type:         agents.ProviderEventSessionFailed,
				ErrorMessage: turnFailureMessage(event.Result),
			}}
		}
		usage := tokenUsage(event.Usage)
		return []agents.ProviderEvent{{
			Type:  agents.ProviderEventTurnCompleted,
			Model: event.Model,
			Usage: usage,
		}}
	default:
		return nil
	}
}

func assistantEvents(event streamEvent) []agents.ProviderEvent {
	var message streamMessage
	if err := json.Unmarshal(event.Message, &message); err != nil {
		return nil
	}

	texts := make([]string, 0, len(message.Content))
	events := make([]agents.ProviderEvent, 0, len(message.Content))
	for _, block := range message.Content {
		switch block.Type {
		case "text":
			if block.Text != "" {
				texts = append(texts, block.Text)
			}
		case "tool_use":
			events = append(events, agents.ProviderEvent{
				ProviderEventID: block.ID,
				Type:            agents.ProviderEventToolUseStarted,
				ToolName:        block.Name,
				ToolCallID:      block.ID,
				ToolInput:       redactSensitive(renderToolInput(block.Input)),
			})
		}
	}
	if len(texts) > 0 {
		events = append(events, agents.ProviderEvent{
			ProviderEventID: assistantEventID(message.ID),
			Type:            agents.ProviderEventAssistantMessage,
			Text:            redactSensitive(strings.Join(texts, "")),
		})
	}
	return events
}

func toolResultEvents(event streamEvent) []agents.ProviderEvent {
	var message streamMessage
	if err := json.Unmarshal(event.Message, &message); err != nil {
		return nil
	}

	events := make([]agents.ProviderEvent, 0, len(message.Content))
	for _, block := range message.Content {
		if block.Type != "tool_result" {
			continue
		}
		finished := agents.ProviderEvent{
			ProviderEventID: block.ToolUseID,
			Type:            agents.ProviderEventToolUseFinished,
			ToolCallID:      block.ToolUseID,
		}
		if block.IsError {
			finished.ErrorMessage = "tool failed"
		}
		events = append(events, finished)
	}
	return events
}

// turnFailureMessage prefers the CLI's own reason for the failed turn and
// falls back to a generic message when it is empty.
func turnFailureMessage(result string) string {
	if trimmed := strings.TrimSpace(result); trimmed != "" {
		return redactSensitive(trimmed)
	}
	return "claude code turn failed"
}

func assistantEventID(messageID string) string {
	if messageID == "" {
		return ""
	}
	return "ast-" + messageID
}

func tokenUsage(raw *streamUsage) *agents.TokenUsage {
	if raw == nil {
		return nil
	}
	usage := &agents.TokenUsage{
		InputTokens:      raw.InputTokens,
		OutputTokens:     raw.OutputTokens,
		CacheReadTokens:  raw.CacheReadInputTokens,
		CacheWriteTokens: raw.CacheCreationInputTokens,
	}
	usage.TotalTokens = usage.InputTokens + usage.OutputTokens + usage.CacheReadTokens + usage.CacheWriteTokens
	if usage.TotalTokens == 0 {
		return nil
	}
	return usage
}

// renderToolInput prefers the `command` field for shell-style tools and falls
// back to compact JSON for anything else.
func renderToolInput(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var fields map[string]any
	if err := json.Unmarshal(input, &fields); err == nil {
		if cmd, ok := fields["command"].(string); ok && cmd != "" {
			return cmd
		}
	}
	return strings.TrimSpace(string(input))
}

// Defense in depth: the chat task holds a scoped runner token, but if a
// JWT-shaped secret ever surfaces in tool output or assistant text we still
// redact it rather than relay it to the user.
var jwtPattern = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)

func redactSensitive(s string) string {
	if s == "" {
		return s
	}
	return jwtPattern.ReplaceAllString(s, "<redacted>")
}
