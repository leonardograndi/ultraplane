package runner

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/superplanehq/superplane/pkg/jwt"
)

const (
	AgentSessionTokenPurpose = "agent_session"
	// EnvSuperplaneAgentSessionToken carries the JWT the chat loop uses to
	// poll messages and call tools on the public runner endpoints.
	EnvSuperplaneAgentSessionToken = "SUPERPLANE_AGENT_SESSION_TOKEN"
	// DefaultAgentSessionTokenTTL bounds one chat task; recovery mints a new
	// task (and token) when the session is replaced.
	DefaultAgentSessionTokenTTL = 24 * time.Hour
)

// AgentSessionScope identifies the chat session a runner task serves.
type AgentSessionScope struct {
	OrganizationID uuid.UUID
	UserID         uuid.UUID
	CanvasID       uuid.UUID
}

func MintAgentSessionToken(signer *jwt.Signer, scope AgentSessionScope, ttl time.Duration) (string, error) {
	if signer == nil {
		return "", fmt.Errorf("jwt signer is required")
	}
	if ttl <= 0 {
		ttl = DefaultAgentSessionTokenTTL
	}
	if scope.OrganizationID == uuid.Nil || scope.UserID == uuid.Nil || scope.CanvasID == uuid.Nil {
		return "", fmt.Errorf("agent session scope is incomplete")
	}
	return signer.GenerateWithClaims(ttl, map[string]string{
		"purpose":   AgentSessionTokenPurpose,
		"org_id":    scope.OrganizationID.String(),
		"user_id":   scope.UserID.String(),
		"canvas_id": scope.CanvasID.String(),
	})
}

func ParseAgentSessionToken(signer *jwt.Signer, token string) (*AgentSessionScope, error) {
	if signer == nil {
		return nil, fmt.Errorf("jwt signer is required")
	}
	claims, err := signer.ValidateAndGetClaims(token)
	if err != nil {
		return nil, err
	}
	purpose, _ := claims["purpose"].(string)
	if purpose != AgentSessionTokenPurpose {
		return nil, fmt.Errorf("invalid agent session token purpose")
	}
	scope := AgentSessionScope{}
	if scope.OrganizationID, err = parsePlanningClaimUUID(claims, "org_id"); err != nil {
		return nil, err
	}
	if scope.UserID, err = parsePlanningClaimUUID(claims, "user_id"); err != nil {
		return nil, err
	}
	if scope.CanvasID, err = parsePlanningClaimUUID(claims, "canvas_id"); err != nil {
		return nil, err
	}
	return &scope, nil
}

// AttachAgentSessionEnv returns the env vars that point a chat task at the
// public runner endpoints, with the bearer token for the session.
func AttachAgentSessionEnv(baseURL, token string) []BrokerEnvironmentVariable {
	return []BrokerEnvironmentVariable{
		{Name: EnvSuperplaneBaseURL, Value: strings.TrimRight(strings.TrimSpace(baseURL), "/")},
		{Name: EnvSuperplaneAgentSessionToken, Value: token},
	}
}
