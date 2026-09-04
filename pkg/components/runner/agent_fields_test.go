package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/superplanehq/superplane/pkg/configuration"
)

func credentialsSourceOptions(t *testing.T, field configuration.Field) []configuration.FieldOption {
	t.Helper()

	require.NotNil(t, field.TypeOptions)
	require.NotNil(t, field.TypeOptions.Object)
	for _, sub := range field.TypeOptions.Object.Schema {
		if sub.Name != "source" {
			continue
		}
		require.NotNil(t, sub.TypeOptions)
		require.NotNil(t, sub.TypeOptions.Select)
		return sub.TypeOptions.Select.Options
	}
	t.Fatal("credentials source field not found")
	return nil
}

func TestAgentCredentialsFieldIncludesRunnerLoginOnlyWhenAllowed(t *testing.T) {
	t.Parallel()

	field := AgentCredentialsField(AgentCredentialsOptions{AllowRunnerLogin: true})
	assert.Contains(t, credentialsSourceOptions(t, field), configuration.FieldOption{
		Label: "Runner login",
		Value: CredentialsSourceRunner,
	})
	assert.Contains(t, field.Description, "Claude Code login on the runner")

	field = AgentCredentialsField(AgentCredentialsOptions{})
	for _, option := range credentialsSourceOptions(t, field) {
		assert.NotEqual(t, CredentialsSourceRunner, option.Value)
	}
	assert.NotContains(t, field.Description, "runner")
}
