package db

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPruneSupersededCodexNativePermissionProfilesKeepsNewestResumableGeneration(t *testing.T) {
	setupTestDB(t)
	base := time.Now().UTC().Add(-time.Minute)
	for i, runtime := range []CodexAppServerRuntime{
		{Generation: "old", LaunchID: "old-launch", AgentID: "agent-1", ConvID: "conv",
			SocketPath: "/tmp/old.sock", State: CodexAppServerDead, CreatedAt: base},
		{Generation: "new", LaunchID: "new-launch", AgentID: "agent-1", ConvID: "conv",
			SocketPath: "/tmp/new.sock", State: CodexAppServerReady, CreatedAt: base.Add(time.Second)},
		{Generation: "other", LaunchID: "other-launch", AgentID: "agent-2", ConvID: "other-conv",
			SocketPath: "/tmp/other.sock", State: CodexAppServerDead, CreatedAt: base},
	} {
		runtime.UpdatedAt = runtime.CreatedAt
		require.NoErrorf(t, UpsertCodexAppServerRuntime(runtime), "runtime %d", i)
		require.NoError(t, UpsertCodexNativePermissionProfile(CodexNativePermissionProfile{
			Generation:  runtime.Generation,
			ProfileName: fmt.Sprintf("tclaude-agent-%016x", i+1),
			ProfileTOML: "complete-" + runtime.Generation,
			CreatedAt:   runtime.CreatedAt,
		}))
	}

	pruned, err := PruneSupersededCodexNativePermissionProfiles()
	require.NoError(t, err)
	assert.EqualValues(t, 1, pruned)
	profiles, err := ListCodexNativePermissionProfiles()
	require.NoError(t, err)
	require.Len(t, profiles, 2)
	assert.ElementsMatch(t, []string{"new", "other"},
		[]string{profiles[0].Generation, profiles[1].Generation})
}
