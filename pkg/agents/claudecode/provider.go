// Package claudecode runs the canvas chat agent on a fleet runner: one
// persistent broker task per session executes the Claude Code CLI with the
// login persisted on the runner, so no Anthropic API key is required.
package claudecode

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/superplanehq/superplane/pkg/agents"
	"github.com/superplanehq/superplane/pkg/agents/agent_tools"
	runneraction "github.com/superplanehq/superplane/pkg/components/runner"
	"github.com/superplanehq/superplane/pkg/jwt"
)

const (
	ProviderName = "claude-code"

	defaultTaskTimeoutSeconds = 14400
	streamPollInterval        = time.Second
	liveLogFetchLimit         = 1000

	liveLogTypeAgentEvent  = "sp.agent.event"
	liveLogTypeAgentFailed = "sp.agent.failed"
)

// Config carries the operator settings for the runner-backed chat provider.
type Config struct {
	// Model is the Claude model id passed to the CLI; empty lets the CLI
	// choose its default.
	Model string
	// TaskTimeoutSeconds bounds one chat task; a new task (and token) is
	// created by session recovery when it expires.
	TaskTimeoutSeconds int
	// MachineType selects the runner fleet for chat tasks.
	MachineType string
}

// Provider implements agents.Provider on top of the task broker.
type Provider struct {
	config Config

	// lastEventIndex dedupes live-log records across turns of one task.
	state sync.Map
}

func New(config Config) (*Provider, error) {
	if strings.TrimSpace(os.Getenv("TASK_BROKER_BASE_URL")) == "" {
		return nil, fmt.Errorf("TASK_BROKER_BASE_URL is not set")
	}
	if strings.TrimSpace(os.Getenv("TASK_BROKER_AUTH_TOKEN")) == "" {
		return nil, fmt.Errorf("TASK_BROKER_AUTH_TOKEN is not set")
	}
	if config.TaskTimeoutSeconds <= 0 {
		config.TaskTimeoutSeconds = defaultTaskTimeoutSeconds
	}
	if strings.TrimSpace(config.MachineType) == "" {
		config.MachineType = runneraction.MachineTypeE1LargeAMD64
	}
	return &Provider{config: config}, nil
}

func (p *Provider) Name() string { return ProviderName }

// httpClientContext adapts a plain client to the interface NewBrokerClient
// expects for non-loopback broker origins.
type httpClientContext struct {
	client *http.Client
}

func (c httpClientContext) Do(req *http.Request) (*http.Response, error) {
	return c.client.Do(req)
}

func (p *Provider) brokerClient() (*runneraction.BrokerClient, error) {
	return runneraction.NewBrokerClient(httpClientContext{client: &http.Client{Timeout: 60 * time.Second}})
}

// CreateSession starts one persistent chat task on the runner. The
// ProviderSessionID is the broker task id.
func (p *Provider) CreateSession(ctx context.Context, opts agents.CreateSessionOptions) (*agents.CreateSessionResult, error) {
	if opts.OrganizationID == uuid.Nil || opts.UserID == uuid.Nil || opts.CanvasID == uuid.Nil {
		return nil, fmt.Errorf("organization, user, and canvas are required to start a claude-code chat task")
	}

	baseURL := runneraction.PublicSuperplaneBaseURL("")
	if baseURL == "" {
		return nil, fmt.Errorf("public SuperPlane URL is missing; set BASE_URL or WEBHOOKS_BASE_URL")
	}
	secret := strings.TrimSpace(os.Getenv("JWT_SECRET"))
	if secret == "" {
		return nil, fmt.Errorf("JWT_SECRET is required to mint agent session tokens")
	}

	ttl := time.Duration(p.config.TaskTimeoutSeconds) * time.Second
	if ttl > runneraction.DefaultAgentSessionTokenTTL {
		ttl = runneraction.DefaultAgentSessionTokenTTL
	}
	token, err := runneraction.MintAgentSessionToken(jwt.NewSigner(secret), runneraction.AgentSessionScope{
		OrganizationID: opts.OrganizationID,
		UserID:         opts.UserID,
		CanvasID:       opts.CanvasID,
	}, ttl)
	if err != nil {
		return nil, fmt.Errorf("mint agent session token: %w", err)
	}

	broker, err := p.brokerClient()
	if err != nil {
		return nil, err
	}

	environment := runneraction.AttachAgentSessionEnv(baseURL, token)
	if model := strings.TrimSpace(p.config.Model); model != "" {
		environment = append(environment, runneraction.BrokerEnvironmentVariable{
			Name:  "SUPERPLANE_AGENT_MODEL",
			Value: model,
		})
	}
	// Runners whose CLI must use a local interactive login (no persisted
	// file login) disable --bare with SUPERPLAE_CLAUDE_BARE=0 server-side.
	if bare := strings.TrimSpace(os.Getenv("SUPERPLANE_CLAUDE_BARE")); bare != "" {
		environment = append(environment, runneraction.BrokerEnvironmentVariable{
			Name:  "SUPERPLANE_CLAUDE_BARE",
			Value: bare,
		})
	}

	taskID, err := broker.CreateTask(runneraction.CreateTaskParams{
		MachineType:    p.config.MachineType,
		Commands:       chatTaskCommands(strings.TrimSpace(p.config.Model)),
		Files:          chatTaskFiles(),
		Environment:    environment,
		ExecutionMode:  runneraction.ExecutionModeHost,
		TimeoutSeconds: p.config.TaskTimeoutSeconds,
		Labels:         map[string]string{agentSessionTaskLabel: "claude-code-chat"},
	})
	if err != nil {
		return nil, fmt.Errorf("create chat task: %w", err)
	}
	return &agents.CreateSessionResult{ProviderSessionID: taskID}, nil
}

// SendMessage validates that the chat task is still alive. The message is
// persisted by the service before the stream worker starts, and the task loop
// long-polls it from the runner agent-session endpoint.
func (p *Provider) SendMessage(ctx context.Context, providerSessionID, message string, opts agents.SendMessageOptions) error {
	broker, err := p.brokerClient()
	if err != nil {
		return err
	}
	task, err := broker.FetchTaskStatus(providerSessionID)
	if err != nil {
		return fmt.Errorf("%w: fetch chat task: %w", agents.ErrProviderSessionUnavailable, err)
	}
	if task.IsInTerminalState() {
		return fmt.Errorf("%w: chat task is %s", agents.ErrProviderSessionUnavailable, task.Status)
	}
	return nil
}

// StreamEvents polls the chat task live logs until the turn's result event
// arrives, the task dies without one, or ctx is cancelled.
func (p *Provider) StreamEvents(ctx context.Context, providerSessionID string, onEvent func(agents.ProviderEvent) error) error {
	broker, err := p.brokerClient()
	if err != nil {
		return err
	}

	lastIndex := p.loadLastIndex(providerSessionID)
	ticker := time.NewTicker(streamPollInterval)
	defer ticker.Stop()
	for {
		if err := p.streamOnce(ctx, broker, providerSessionID, &lastIndex, onEvent); err != nil {
			if errors.Is(err, errTurnCompleted) {
				return nil
			}
			return err
		}

		select {
		case <-ctx.Done():
			p.storeLastIndex(providerSessionID, lastIndex)
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// streamOnce fetches new live-log records and maps them. It returns nil to
// continue polling, or a terminal error that ends the stream.
func (p *Provider) streamOnce(ctx context.Context, broker *runneraction.BrokerClient, taskID string, lastIndex *int64, onEvent func(agents.ProviderEvent) error) error {
	result, err := runneraction.FetchLiveLogRecords(ctx, taskID, runneraction.LiveLogFetchOptions{Limit: liveLogFetchLimit})
	if err != nil {
		// The broker may restart or prune tasks; treat lookup failure as an
		// unavailable session so the service recovers with a new task.
		return fmt.Errorf("%w: fetch live logs: %w", agents.ErrProviderSessionUnavailable, err)
	}

	turnCompleted := false
	for _, record := range result.Records {
		if record.Index == nil || int64(*record.Index) <= *lastIndex {
			continue
		}
		*lastIndex = int64(*record.Index)

		if record.Type == liveLogTypeAgentFailed {
			if err := onEvent(agents.ProviderEvent{
				Type:         agents.ProviderEventSessionFailed,
				ErrorMessage: redactSensitive(record.Message),
			}); err != nil {
				return err
			}
			// The CLI died without a result; the turn is over.
			turnCompleted = true
			continue
		}
		if record.Type != liveLogTypeAgentEvent {
			continue
		}

		for _, event := range mapStreamLine(record.Message) {
			if err := onEvent(event); err != nil {
				return err
			}
			if event.Type == agents.ProviderEventTurnCompleted || event.Type == agents.ProviderEventSessionFailed {
				turnCompleted = true
			}
		}
	}

	if turnCompleted {
		p.storeLastIndex(taskID, *lastIndex)
		return errTurnCompleted
	}

	// The loop only ends a turn with a result event; a terminal task without
	// one lost the turn (timeout, cancel) and the session must recover.
	task, err := broker.FetchTaskStatus(taskID)
	if err != nil {
		return fmt.Errorf("%w: fetch chat task: %w", agents.ErrProviderSessionUnavailable, err)
	}
	if task.IsInTerminalState() {
		return fmt.Errorf("%w: chat task ended without a result (%s)", agents.ErrProviderSessionUnavailable, task.Status)
	}
	return nil
}

// errTurnCompleted is an internal sentinel that ends StreamEvents for this
// turn; the stream worker starts a new stream per turn.
var errTurnCompleted = fmt.Errorf("turn completed")

func (p *Provider) loadLastIndex(taskID string) int64 {
	if value, ok := p.state.Load(taskID); ok {
		if index, ok := value.(int64); ok {
			return index
		}
	}
	return 0
}

func (p *Provider) storeLastIndex(taskID string, index int64) {
	p.state.Store(taskID, index)
}

func (p *Provider) InterruptSession(ctx context.Context, providerSessionID string) error {
	broker, err := p.brokerClient()
	if err != nil {
		return err
	}
	if err := broker.CancelTask(providerSessionID); err != nil {
		return fmt.Errorf("%w: cancel chat task: %w", agents.ErrProviderSessionUnavailable, err)
	}
	p.state.Delete(providerSessionID)
	return nil
}

func (p *Provider) DefineOutcome(ctx context.Context, providerSessionID string, opts agents.DefineOutcomeOptions) error {
	return fmt.Errorf("%w: claude-code provider does not support outcomes", agents.ErrInvalidRequest)
}

// DeleteSession stops the chat task; called when a session is replaced.
func (p *Provider) DeleteSession(ctx context.Context, providerSessionID string) error {
	broker, err := p.brokerClient()
	if err != nil {
		return err
	}
	if err := broker.CancelTask(providerSessionID); err != nil {
		return fmt.Errorf("cancel chat task: %w", err)
	}
	p.state.Delete(providerSessionID)
	return nil
}

// ArchiveSession mirrors DeleteSession: the chat task holds no archive state.
func (p *Provider) ArchiveSession(ctx context.Context, providerSessionID string) error {
	return p.DeleteSession(ctx, providerSessionID)
}

func (p *Provider) ToolSchemaRevision() string {
	return agenttools.SchemaRevision()
}
