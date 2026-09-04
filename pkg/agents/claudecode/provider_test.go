package claudecode

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/google/uuid"
	"github.com/superplanehq/superplane/pkg/agents"
	runneraction "github.com/superplanehq/superplane/pkg/components/runner"
)

func newTestProvider(t *testing.T, broker *httptest.Server) *Provider {
	t.Helper()

	t.Setenv("TASK_BROKER_BASE_URL", broker.URL)
	t.Setenv("TASK_BROKER_AUTH_TOKEN", "broker-token")
	t.Setenv("JWT_SECRET", "test-secret")
	t.Setenv("WEBHOOKS_BASE_URL", "https://superplane.example")

	provider, err := New(Config{})
	require.NoError(t, err)
	return provider
}

func TestNewRequiresBrokerEnv(t *testing.T) {
	t.Setenv("TASK_BROKER_BASE_URL", "")
	t.Setenv("TASK_BROKER_AUTH_TOKEN", "")

	_, err := New(Config{})
	require.Error(t, err)
}

// TestProviderCreateSessionStartsChatTask covers the happy path: the task
// ships the chat scripts, a scoped runner token, and no API key.
func TestProviderCreateSessionStartsChatTask(t *testing.T) {
	var createBody atomic.Value
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		createBody.Store(body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"chat-task-1"}`))
	}))
	defer broker.Close()

	provider := newTestProvider(t, broker)

	result, err := provider.CreateSession(t.Context(), agents.CreateSessionOptions{
		OrganizationID: uuid.New(),
		UserID:         uuid.New(),
		CanvasID:       uuid.New(),
	})
	require.NoError(t, err)
	assert.Equal(t, "chat-task-1", result.ProviderSessionID)

	raw, ok := createBody.Load().([]byte)
	require.True(t, ok)
	var request struct {
		Commands    []runneraction.BrokerCommand             `json:"commands"`
		Files       []runneraction.BrokerTaskFile            `json:"files"`
		Environment []runneraction.BrokerEnvironmentVariable `json:"environment"`
		Labels      map[string]string                        `json:"labels"`
	}
	require.NoError(t, json.Unmarshal(raw, &request))

	fileNames := make([]string, 0, len(request.Files))
	for _, file := range request.Files {
		fileNames = append(fileNames, file.Path)
	}
	assert.Contains(t, fileNames, "chat_claude.js")
	assert.Contains(t, fileNames, "chat_loop.js")
	assert.Contains(t, fileNames, "mcp_bridge.js")

	envNames := environmentNames(request.Environment)
	assert.Contains(t, envNames, runneraction.EnvSuperplaneAgentSessionToken)
	assert.Contains(t, envNames, runneraction.EnvSuperplaneBaseURL)
	assert.NotContains(t, envNames, "ANTHROPIC_API_KEY")
	assert.NotContains(t, string(raw), "ANTHROPIC_API_KEY")

	require.NotEmpty(t, request.Labels)
	assert.Equal(t, "claude-code-chat", request.Labels[agentSessionTaskLabel])
}

func TestProviderCreateSessionRequiresScope(t *testing.T) {
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"chat-task-1"}`))
	}))
	defer broker.Close()

	provider := newTestProvider(t, broker)

	_, err := provider.CreateSession(t.Context(), agents.CreateSessionOptions{})
	require.Error(t, err)
}

func TestProviderSendMessageDetectsTerminalTask(t *testing.T) {
	status := atomic.Value{}
	status.Store("running")
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chat-task-1","status":"` + status.Load().(string) + `"}`))
	}))
	defer broker.Close()

	provider := newTestProvider(t, broker)

	require.NoError(t, provider.SendMessage(t.Context(), "chat-task-1", "hello", agents.SendMessageOptions{}))

	status.Store("canceled")
	err := provider.SendMessage(t.Context(), "chat-task-1", "hello", agents.SendMessageOptions{})
	require.Error(t, err)
	assert.ErrorIs(t, err, agents.ErrProviderSessionUnavailable)
}

// TestProviderStreamEventsCompletesTurn feeds live-log records through a fake
// broker and expects the mapped provider events.
func TestProviderStreamEventsCompletesTurn(t *testing.T) {
	records := `{"type":"sp.agent.event","index":1,"message":"{\"type\":\"assistant\",\"message\":{\"id\":\"m1\",\"content\":[{\"type\":\"text\",\"text\":\"hi\"}]}}"}
{"type":"sp.agent.event","index":2,"message":"{\"type\":\"result\",\"subtype\":\"success\",\"model\":\"sonnet\",\"usage\":{\"input_tokens\":3,\"output_tokens\":4}}"}`

	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/live-logs"):
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = w.Write([]byte(records))
		case strings.Contains(r.URL.Path, "/v1/tasks/"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"chat-task-1","status":"running"}`))
		default:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"chat-task-1"}`))
		}
	}))
	defer broker.Close()

	provider := newTestProvider(t, broker)

	var collected []agents.ProviderEvent
	err := provider.StreamEvents(t.Context(), "chat-task-1", func(event agents.ProviderEvent) error {
		collected = append(collected, event)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, collected, 2)
	assert.Equal(t, agents.ProviderEventAssistantMessage, collected[0].Type)
	assert.Equal(t, "hi", collected[0].Text)
	assert.Equal(t, agents.ProviderEventTurnCompleted, collected[1].Type)
	require.NotNil(t, collected[1].Usage)
	assert.Equal(t, int64(7), collected[1].Usage.TotalTokens)
}

// TestProviderCreateSessionForwardsBareOverride covers the host-login escape:
// SUPERPLAE_CLAUDE_BARE set server-side reaches the task environment.
func TestProviderCreateSessionForwardsBareOverride(t *testing.T) {
	var createBody atomic.Value
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		createBody.Store(body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"chat-task-9"}`))
	}))
	defer broker.Close()

	provider := newTestProvider(t, broker)
	t.Setenv("SUPERPLANE_CLAUDE_BARE", "0")

	_, err := provider.CreateSession(t.Context(), agents.CreateSessionOptions{
		OrganizationID: uuid.New(),
		UserID:         uuid.New(),
		CanvasID:       uuid.New(),
	})
	require.NoError(t, err)

	raw, ok := createBody.Load().([]byte)
	require.True(t, ok)
	var request struct {
		Environment []runneraction.BrokerEnvironmentVariable `json:"environment"`
	}
	require.NoError(t, json.Unmarshal(raw, &request))
	found := false
	for _, item := range request.Environment {
		if item.Name == "SUPERPLANE_CLAUDE_BARE" {
			found = true
			assert.Equal(t, "0", item.Value)
		}
	}
	assert.True(t, found, "task environment must carry SUPERPLAE_CLAUDE_BARE")
}

// TestProviderStreamEventsReportsCLIFailure covers the sp.agent.failed record
// the chat script writes when the CLI dies without a result.
func TestProviderStreamEventsReportsCLIFailure(t *testing.T) {
	records := `{"type":"sp.agent.failed","index":1,"message":"claude exited with code 1"}`

	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/live-logs"):
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = w.Write([]byte(records))
		case strings.Contains(r.URL.Path, "/v1/tasks/"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"chat-task-1","status":"failed"}`))
		default:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"chat-task-1"}`))
		}
	}))
	defer broker.Close()

	provider := newTestProvider(t, broker)

	var collected []agents.ProviderEvent
	err := provider.StreamEvents(t.Context(), "chat-task-1", func(event agents.ProviderEvent) error {
		collected = append(collected, event)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, collected, 1)
	assert.Equal(t, agents.ProviderEventSessionFailed, collected[0].Type)
	assert.Contains(t, collected[0].ErrorMessage, "claude exited with code 1")
}

// TestProviderStreamEventsTerminalWithoutResult expects session-unavailable
// when the task died before producing a turn result.
func TestProviderStreamEventsTerminalWithoutResult(t *testing.T) {
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/live-logs"):
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = w.Write([]byte(`{"type":"sp.agent.event","index":1,"message":"{\"type\":\"assistant\",\"message\":{\"id\":\"m1\",\"content\":[{\"type\":\"text\",\"text\":\"hi\"}]}}"}`))
		case strings.Contains(r.URL.Path, "/v1/tasks/"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"chat-task-1","status":"canceled"}`))
		default:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"chat-task-1"}`))
		}
	}))
	defer broker.Close()

	provider := newTestProvider(t, broker)

	err := provider.StreamEvents(t.Context(), "chat-task-1", func(agents.ProviderEvent) error { return nil })
	require.Error(t, err)
	assert.ErrorIs(t, err, agents.ErrProviderSessionUnavailable)
}

func TestProviderDefineOutcomeUnsupported(t *testing.T) {
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer broker.Close()

	provider := newTestProvider(t, broker)

	err := provider.DefineOutcome(t.Context(), "task", agents.DefineOutcomeOptions{})
	require.Error(t, err)
	assert.ErrorIs(t, err, agents.ErrInvalidRequest)
}

func environmentNames(environment []runneraction.BrokerEnvironmentVariable) []string {
	names := make([]string, 0, len(environment))
	for _, item := range environment {
		names = append(names, item.Name)
	}
	return names
}
