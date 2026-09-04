package public

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	agenttools "github.com/superplanehq/superplane/pkg/agents/agent_tools"
	runneraction "github.com/superplanehq/superplane/pkg/components/runner"
	"github.com/superplanehq/superplane/pkg/database"
	"github.com/superplanehq/superplane/pkg/jwt"
	"github.com/superplanehq/superplane/pkg/models"
	"github.com/superplanehq/superplane/test/support"
	"gorm.io/datatypes"
)

func setupAgentSessionRunner(t *testing.T, provider string) (*Server, *jwt.Signer, *support.ResourceRegistry, *models.AgentSession) {
	t.Helper()

	r := support.Setup(t)
	t.Cleanup(r.Close)

	signer := jwt.NewSigner("test")
	server, err := NewServer(
		r.Encryptor, r.Registry, signer, support.NewOIDCProvider(), r.GitProvider,
		"", "http://localhost", "http://localhost", "test", "/app/templates", r.AuthService, nil, false,
	)
	require.NoError(t, err)
	registerTestGRPCGateway(t, server, r.AuthService, r.Registry, r.Encryptor, support.NewOIDCProvider(), r.GitProvider, nil)

	canvas, _ := support.CreateCanvas(t, r.Organization.ID, r.User, nil, nil)
	session := &models.AgentSession{
		OrganizationID:    r.Organization.ID,
		UserID:            r.User,
		CanvasID:          canvas.ID,
		Provider:          provider,
		ProviderSessionID: "chat-task-1",
		Status:            models.AgentSessionStatusIdle,
	}
	require.NoError(t, models.CreateAgentSessionInTransaction(database.Conn(), session))
	return server, signer, r, session
}

func mintAgentSessionToken(t *testing.T, signer *jwt.Signer, r *support.ResourceRegistry, canvasID uuid.UUID) string {
	t.Helper()

	token, err := runneraction.MintAgentSessionToken(signer, runneraction.AgentSessionScope{
		OrganizationID: r.Organization.ID,
		UserID:         r.User,
		CanvasID:       canvasID,
	}, time.Hour)
	require.NoError(t, err)
	return token
}

func agentSessionRunnerRequest(t *testing.T, server *Server, method, path, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()

	var reader *bytes.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	server.Router.ServeHTTP(rec, req)
	return rec
}

func TestRunnerAgentSessionWaitRequiresToken(t *testing.T) {
	server, _, r, session := setupAgentSessionRunner(t, AgentSessionChatProvider)

	rec := agentSessionRunnerRequest(t, server, http.MethodGet, "/api/v1/runner/agent-sessions/wait?latest=1", "", nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	token := mintAgentSessionToken(t, jwt.NewSigner("other"), r, session.CanvasID)
	rec = agentSessionRunnerRequest(t, server, http.MethodGet, "/api/v1/runner/agent-sessions/wait?latest=1", token, nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestRunnerAgentSessionWaitLatestReturnsCursor(t *testing.T) {
	server, signer, r, session := setupAgentSessionRunner(t, AgentSessionChatProvider)

	first := &models.AgentSessionMessage{SessionID: session.ID, Role: models.AgentMessageRoleUser, Content: "first"}
	require.NoError(t, models.AppendAgentSessionMessage(first))
	second := &models.AgentSessionMessage{SessionID: session.ID, Role: models.AgentMessageRoleUser, Content: "second"}
	require.NoError(t, models.AppendAgentSessionMessage(second))

	token := mintAgentSessionToken(t, signer, r, session.CanvasID)
	rec := agentSessionRunnerRequest(t, server, http.MethodGet, "/api/v1/runner/agent-sessions/wait?latest=1", token, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var body struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, second.ID.String(), body.ID)
}

func TestRunnerAgentSessionWaitServesNextUserMessage(t *testing.T) {
	server, signer, r, session := setupAgentSessionRunner(t, AgentSessionChatProvider)

	first := &models.AgentSessionMessage{SessionID: session.ID, Role: models.AgentMessageRoleUser, Content: "first"}
	require.NoError(t, models.AppendAgentSessionMessage(first))
	assistant := &models.AgentSessionMessage{SessionID: session.ID, Role: models.AgentMessageRoleAssistant, Content: "answer"}
	require.NoError(t, models.AppendAgentSessionMessage(assistant))
	second := &models.AgentSessionMessage{
		SessionID: session.ID,
		Role:      models.AgentMessageRoleUser,
		Content:   "look at this",
		Images: datatypes.JSONSlice[models.AgentSessionImage]{
			{MediaType: "image/png", Data: base64.StdEncoding.EncodeToString([]byte("bytes"))},
		},
	}
	require.NoError(t, models.AppendAgentSessionMessage(second))

	token := mintAgentSessionToken(t, signer, r, session.CanvasID)

	// Cursor at the first user message skips the assistant row and returns
	// the next user message with its images.
	path := "/api/v1/runner/agent-sessions/wait?hold_seconds=1&after=" + first.ID.String()
	rec := agentSessionRunnerRequest(t, server, http.MethodGet, path, token, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var body struct {
		Status string                     `json:"status"`
		ID     string                     `json:"id"`
		Images []models.AgentSessionImage `json:"images"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "message", body.Status)
	assert.Equal(t, second.ID.String(), body.ID)
	require.Len(t, body.Images, 1)
	assert.Equal(t, "image/png", body.Images[0].MediaType)

	// Cursor at the newest message reports idle within the hold window.
	path = "/api/v1/runner/agent-sessions/wait?hold_seconds=1&after=" + second.ID.String()
	rec = agentSessionRunnerRequest(t, server, http.MethodGet, path, token, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"idle"`)
}

func TestRunnerAgentSessionRejectsOtherProvider(t *testing.T) {
	server, signer, r, session := setupAgentSessionRunner(t, "anthropic")

	token := mintAgentSessionToken(t, signer, r, session.CanvasID)
	rec := agentSessionRunnerRequest(t, server, http.MethodGet, "/api/v1/runner/agent-sessions/wait?latest=1", token, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestRunnerAgentSessionToolsListAndCall(t *testing.T) {
	server, signer, r, session := setupAgentSessionRunner(t, AgentSessionChatProvider)
	server.AgentToolRegistry = agenttools.NewRegistry(agenttools.Dependencies{})

	token := mintAgentSessionToken(t, signer, r, session.CanvasID)

	rec := agentSessionRunnerRequest(t, server, http.MethodGet, "/api/v1/runner/agent-sessions/tools", token, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var list struct {
		Tools []struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.NotEmpty(t, list.Tools)
	assert.NotEmpty(t, list.Tools[0].Name)
	assert.NotEmpty(t, list.Tools[0].InputSchema)

	payload, err := json.Marshal(map[string]any{
		"name":    "superplane_component_schema",
		"input":   map[string]any{},
		"call_id": "call-1",
	})
	require.NoError(t, err)
	rec = agentSessionRunnerRequest(t, server, http.MethodPost, "/api/v1/runner/agent-sessions/tools/call", token, payload)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var call struct {
		Content string `json:"content"`
		IsError bool   `json:"isError"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &call))
	assert.True(t, call.IsError)
	assert.Contains(t, call.Content, "not configured")
}
