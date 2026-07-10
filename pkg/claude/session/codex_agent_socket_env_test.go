package session

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestApplyCodexAgentSocketEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	env := map[string]string{}
	ApplyCodexAgentSocketEnv("", env)
	assert.NotContains(t, env, agentipc.SocketEnv)

	ApplyCodexAgentSocketEnv(harness.CodexAgentProfile, env)
	assert.Equal(t, filepath.Join(home, ".tclaude-agentd.sock"), env[agentipc.SocketEnv])
}
