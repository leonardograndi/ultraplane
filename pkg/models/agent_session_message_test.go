package models

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/superplanehq/superplane/pkg/database"
)

func TestAppendAgentSessionMessageInTransaction_PreservesToolNameOnNamelessResult(t *testing.T) {
	session := &AgentSession{
		ID:                uuid.New(),
		OrganizationID:    uuid.New(),
		UserID:            uuid.New(),
		CanvasID:          uuid.New(),
		Provider:          "anthropic",
		ProviderSessionID: "sesn_test",
		Status:            AgentSessionStatusStreaming,
	}
	require.NoError(t, CreateAgentSessionInTransaction(database.Conn(), session))
	t.Cleanup(func() {
		_ = database.Conn().Delete(&AgentSession{}, "id = ?", session.ID).Error
	})

	require.NoError(t, AppendAgentSessionMessage(&AgentSessionMessage{
		SessionID:       session.ID,
		ProviderEventID: "toolu_1",
		Role:            AgentMessageRoleTool,
		Content:         `{"file_path":"/tmp/spec.md"}`,
		ToolCallID:      "toolu_1",
		ToolName:        "read",
		ToolStatus:      AgentToolStatusStarted,
	}))

	require.NoError(t, AppendAgentSessionMessage(&AgentSessionMessage{
		SessionID:       session.ID,
		ProviderEventID: "toolu_1",
		Role:            AgentMessageRoleTool,
		ToolCallID:      "toolu_1",
		ToolStatus:      AgentToolStatusFinished,
	}))

	var stored AgentSessionMessage
	require.NoError(t, database.Conn().
		Where("session_id = ? AND provider_event_id = ?", session.ID, "toolu_1").
		First(&stored).Error)

	assert.Equal(t, "read", stored.ToolName)
	assert.Equal(t, AgentToolStatusFinished, stored.ToolStatus)
	assert.Equal(t, `{"file_path":"/tmp/spec.md"}`, stored.Content)
}

func TestAppendAgentSessionMessageInTransaction_DoesNotDowngradeFinishedToolOnReplay(t *testing.T) {
	session := &AgentSession{
		ID:                uuid.New(),
		OrganizationID:    uuid.New(),
		UserID:            uuid.New(),
		CanvasID:          uuid.New(),
		Provider:          "anthropic",
		ProviderSessionID: "sesn_test",
		Status:            AgentSessionStatusStreaming,
	}
	require.NoError(t, CreateAgentSessionInTransaction(database.Conn(), session))
	t.Cleanup(func() {
		_ = database.Conn().Delete(&AgentSession{}, "id = ?", session.ID).Error
	})

	require.NoError(t, AppendAgentSessionMessage(&AgentSessionMessage{
		SessionID:       session.ID,
		ProviderEventID: "toolu_1",
		Role:            AgentMessageRoleTool,
		Content:         `{"ok":true}`,
		ToolCallID:      "toolu_1",
		ToolName:        "superplane_app",
		ToolStatus:      AgentToolStatusFinished,
	}))

	require.NoError(t, AppendAgentSessionMessage(&AgentSessionMessage{
		SessionID:       session.ID,
		ProviderEventID: "toolu_1",
		Role:            AgentMessageRoleTool,
		Content:         `{"action":"read"}`,
		ToolCallID:      "toolu_1",
		ToolName:        "superplane_app",
		ToolStatus:      AgentToolStatusStarted,
	}))

	var stored AgentSessionMessage
	require.NoError(t, database.Conn().
		Where("session_id = ? AND provider_event_id = ?", session.ID, "toolu_1").
		First(&stored).Error)

	assert.Equal(t, "superplane_app", stored.ToolName)
	assert.Equal(t, AgentToolStatusFinished, stored.ToolStatus)
	assert.Equal(t, `{"ok":true}`, stored.Content)
}

func TestFindLatestAgentSessionMessageIDAndNextUserMessage(t *testing.T) {
	session := &AgentSession{
		ID:                uuid.New(),
		OrganizationID:    uuid.New(),
		UserID:            uuid.New(),
		CanvasID:          uuid.New(),
		Provider:          "claude-code",
		ProviderSessionID: "task-1",
		Status:            AgentSessionStatusIdle,
	}
	require.NoError(t, CreateAgentSessionInTransaction(database.Conn(), session))
	t.Cleanup(func() {
		_ = database.Conn().Where("session_id = ?", session.ID).Delete(&AgentSessionMessage{}).Error
		_ = database.Conn().Delete(&AgentSession{}, "id = ?", session.ID).Error
	})

	empty, err := FindLatestAgentSessionMessageID(database.Conn(), session.ID)
	require.NoError(t, err)
	assert.Equal(t, uuid.Nil, empty)

	first := &AgentSessionMessage{SessionID: session.ID, Role: AgentMessageRoleUser, Content: "first"}
	require.NoError(t, AppendAgentSessionMessage(first))
	require.NoError(t, AppendAgentSessionMessage(&AgentSessionMessage{SessionID: session.ID, Role: AgentMessageRoleAssistant, Content: "answer"}))
	second := &AgentSessionMessage{SessionID: session.ID, Role: AgentMessageRoleUser, Content: "second"}
	require.NoError(t, AppendAgentSessionMessage(second))

	latest, err := FindLatestAgentSessionMessageID(database.Conn(), session.ID)
	require.NoError(t, err)
	assert.Equal(t, second.ID, latest)

	// Without a cursor the oldest user message comes first, skipping the
	// assistant row.
	next, err := FindNextAgentUserMessageAfter(database.Conn(), session.ID, uuid.Nil)
	require.NoError(t, err)
	require.NotNil(t, next)
	assert.Equal(t, first.ID, next.ID)

	// After the first user message only the second remains.
	next, err = FindNextAgentUserMessageAfter(database.Conn(), session.ID, first.ID)
	require.NoError(t, err)
	require.NotNil(t, next)
	assert.Equal(t, second.ID, next.ID)

	// At the newest message nothing is pending.
	next, err = FindNextAgentUserMessageAfter(database.Conn(), session.ID, second.ID)
	require.NoError(t, err)
	assert.Nil(t, next)
}
