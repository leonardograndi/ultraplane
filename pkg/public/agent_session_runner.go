package public

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	runneraction "github.com/superplanehq/superplane/pkg/components/runner"
	"github.com/superplanehq/superplane/pkg/database"
	"github.com/superplanehq/superplane/pkg/models"
	"gorm.io/gorm"

	"github.com/superplanehq/superplane/pkg/agents"
)

const (
	minAgentSessionHoldSeconds = 1
	maxAgentSessionHoldSeconds = 45

	// AgentSessionChatProvider is the only provider whose runner tasks may use
	// these endpoints; the anthropic provider has no runner loop.
	AgentSessionChatProvider = "claude-code"
)

type agentSessionToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type agentSessionToolCallRequest struct {
	Name   string          `json:"name"`
	Input  json.RawMessage `json:"input"`
	CallID string          `json:"call_id"`
}

// authenticateAgentSessionRunner validates the agent-session JWT and resolves
// the chat session row for the token scope. The token is single-purpose and
// scoped to one (organization, user, canvas) chat.
func (s *Server) authenticateAgentSessionRunner(w http.ResponseWriter, r *http.Request) (*models.AgentSession, bool) {
	token := bearerToken(r.Header.Get("Authorization"))
	if token == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return nil, false
	}
	scope, err := runneraction.ParseAgentSessionToken(s.jwt, token)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return nil, false
	}

	session, err := models.FindAgentSessionByCanvasInTransaction(
		database.DB(r.Context()),
		scope.OrganizationID,
		scope.UserID,
		scope.CanvasID,
	)
	if err != nil {
		writeAgentSessionRunnerError(w, err)
		return nil, false
	}
	if session.Provider != AgentSessionChatProvider {
		http.Error(w, "agent session not found", http.StatusNotFound)
		return nil, false
	}
	return session, true
}

func (s *Server) handleRunnerAgentSessionWait(w http.ResponseWriter, r *http.Request) {
	session, ok := s.authenticateAgentSessionRunner(w, r)
	if !ok {
		return
	}

	if r.URL.Query().Get("latest") == "1" {
		id, err := models.FindLatestAgentSessionMessageID(database.DB(r.Context()), session.ID)
		if err != nil {
			writeAgentSessionRunnerError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id})
		return
	}

	afterID, err := parseAgentMessageCursor(r.URL.Query().Get("after"))
	if err != nil {
		http.Error(w, "Invalid after parameter", http.StatusBadRequest)
		return
	}

	hold := clampAgentSessionHoldSeconds(r.URL.Query().Get("hold_seconds"))
	deadline := time.Now().Add(time.Duration(hold) * time.Second)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		message, err := models.FindNextAgentUserMessageAfter(database.DB(r.Context()), session.ID, afterID)
		if err != nil {
			writeAgentSessionRunnerError(w, err)
			return
		}
		if message != nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"status":  "message",
				"id":      message.ID,
				"content": message.Content,
				"images":  message.Images,
			})
			return
		}
		if !time.Now().Before(deadline) {
			writeJSON(w, http.StatusOK, map[string]any{"status": "idle"})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) handleRunnerAgentSessionTools(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authenticateAgentSessionRunner(w, r); !ok {
		return
	}
	if s.AgentToolRegistry == nil {
		http.Error(w, "agent tools unavailable", http.StatusServiceUnavailable)
		return
	}

	definitions := s.AgentToolRegistry.Definitions()
	tools := make([]agentSessionToolDefinition, 0, len(definitions))
	for _, definition := range definitions {
		tools = append(tools, agentSessionToolDefinition{
			Name:        definition.Name(),
			Description: definition.Description(),
			InputSchema: definition.InputSchema().Map(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tools": tools})
}

func (s *Server) handleRunnerAgentSessionToolCall(w http.ResponseWriter, r *http.Request) {
	session, ok := s.authenticateAgentSessionRunner(w, r)
	if !ok {
		return
	}
	if s.AgentToolRegistry == nil {
		http.Error(w, "agent tools unavailable", http.StatusServiceUnavailable)
		return
	}

	var req agentSessionToolCallRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "Tool name is required", http.StatusBadRequest)
		return
	}

	result := s.AgentToolRegistry.ExecuteCustomTool(r.Context(), agentSessionContextFor(session), agents.CustomToolUse{
		ID:    req.CallID,
		Name:  req.Name,
		Input: string(req.Input),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"content": result.Content,
		"isError": result.IsError,
	})
}

func writeAgentSessionRunnerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, models.ErrAgentSessionNotFound), errors.Is(err, gorm.ErrRecordNotFound):
		http.Error(w, "agent session not found", http.StatusNotFound)
	default:
		http.Error(w, "Lookup failed", http.StatusInternalServerError)
	}
}

func clampAgentSessionHoldSeconds(raw string) int {
	hold := 45
	if raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err == nil {
			hold = parsed
		}
	}
	if hold < minAgentSessionHoldSeconds {
		return minAgentSessionHoldSeconds
	}
	if hold > maxAgentSessionHoldSeconds {
		return maxAgentSessionHoldSeconds
	}
	return hold
}

func parseAgentMessageCursor(raw string) (uuid.UUID, error) {
	if raw == "" {
		return uuid.Nil, nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

func agentSessionContextFor(session *models.AgentSession) agents.AgentSessionContext {
	return agents.AgentSessionContext{
		SessionID:         session.ID.String(),
		ProviderSessionID: session.ProviderSessionID,
		OrganizationID:    session.OrganizationID.String(),
		UserID:            session.UserID.String(),
		CanvasID:          session.CanvasID.String(),
	}
}
