package db

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInvalidateCodexAppServerRuntimesAfterRestartClearsLiveClaims(t *testing.T) {
	setupTestDB(t)
	for _, tc := range []struct {
		generation, state string
	}{
		{"warming-generation", CodexAppServerWarming},
		{"ready-generation", CodexAppServerReady},
		{"dead-generation", CodexAppServerDead},
	} {
		require.NoError(t, UpsertCodexAppServerRuntime(CodexAppServerRuntime{
			Generation: tc.generation, LaunchID: "launch-" + tc.generation,
			AgentID: "agent", SocketPath: filepath.Join(t.TempDir(), "app.sock"), State: tc.state,
		}))
	}

	require.NoError(t, InvalidateCodexAppServerRuntimesAfterRestart())
	for _, generation := range []string{"warming-generation", "ready-generation"} {
		runtime, err := GetCodexAppServerRuntime(generation)
		require.NoError(t, err)
		require.NotNil(t, runtime)
		assert.Equal(t, CodexAppServerUnavailable, runtime.State)
		assert.Contains(t, runtime.Detail, "agentd restarted")
	}
	dead, err := GetCodexAppServerRuntime("dead-generation")
	require.NoError(t, err)
	require.NotNil(t, dead)
	assert.Equal(t, CodexAppServerDead, dead.State)
}
