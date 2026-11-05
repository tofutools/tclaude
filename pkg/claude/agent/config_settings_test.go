package agent

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

func clearManagedAgentEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{agentipc.AgentHintEnvVar, agentipc.SocketEnv, agentipc.CodexPermissionProfileEnv} {
		t.Setenv(name, "")
	}
}

func TestConfigGetSetAutoJoin(t *testing.T) {
	clearManagedAgentEnv(t)
	t.Setenv("HOME", t.TempDir())

	var out, errb bytes.Buffer
	require.Equal(t, rcOK, runConfigGet(&configGetParams{Key: "session.auto_join_group"}, &out, &errb), errb.String())
	assert.Equal(t, "session.auto_join_group = true (default)\n", out.String())

	out.Reset()
	require.Equal(t, rcOK, runConfigSet(&configSetParams{Key: "session.auto_join_group", Value: "false"}, &out, &errb), errb.String())
	assert.Equal(t, "session.auto_join_group = false (set)\n", out.String())
	cfg, err := config.Load()
	require.NoError(t, err)
	assert.False(t, cfg.AutoJoinGroupEnabled(), "the launch path reads the stored value")

	out.Reset()
	require.Equal(t, rcOK, runConfigSet(&configSetParams{Key: "session.auto_join_group", Value: "default"}, &out, &errb), errb.String())
	assert.Equal(t, "session.auto_join_group = true (default)\n", out.String())

	out.Reset()
	require.Equal(t, rcOK, runConfigGet(&configGetParams{}, &out, &errb))
	assert.Contains(t, out.String(), "session.auto_join_group = true (default)")
	assert.Contains(t, out.String(), "session.auto_join_or_create_group = false (default)")
}

func TestConfigSetRejectsBadInput(t *testing.T) {
	clearManagedAgentEnv(t)
	t.Setenv("HOME", t.TempDir())
	var out, errb bytes.Buffer
	assert.Equal(t, rcInvalidArg, runConfigSet(&configSetParams{Key: "notifications.enabled", Value: "true"}, &out, &errb))
	assert.Contains(t, errb.String(), "supported: agent.spawn_label_from_name, session.auto_join_group, session.auto_join_or_create_group")
	errb.Reset()
	assert.Equal(t, rcInvalidArg, runConfigSet(&configSetParams{Key: "session.auto_join_group", Value: "maybe"}, &out, &errb))
	assert.Contains(t, errb.String(), "takes true, false, or default")
}

func TestConfigSetIsHumanOnly(t *testing.T) {
	clearManagedAgentEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv(agentipc.AgentHintEnvVar, "1")
	var out, errb bytes.Buffer
	assert.Equal(t, rcAuth, runConfigSet(&configSetParams{Key: "session.auto_join_group", Value: "false"}, &out, &errb))
	assert.Contains(t, errb.String(), "human-only")
}
