package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
)

func TestApplyManagedAgentHint(t *testing.T) {
	t.Run("managed launch", func(t *testing.T) {
		env := map[string]string{}
		applyManagedAgentHint(true, env)
		assert.Equal(t, "1", env[agentipc.AgentHintEnvVar])
	})

	t.Run("direct human launch", func(t *testing.T) {
		env := map[string]string{}
		applyManagedAgentHint(false, env)
		assert.NotContains(t, env, agentipc.AgentHintEnvVar)
	})
}
