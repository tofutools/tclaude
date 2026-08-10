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

func TestDeleteAgentByConvIDAtomicallySchedulesNativeProfileCleanup(t *testing.T) {
	setupTestDB(t)
	const convID = "native-delete-conv"
	agentID, _, err := EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, UpsertCodexAppServerRuntime(CodexAppServerRuntime{
		Generation: "native-delete-generation", LaunchID: "native-delete-launch",
		AgentID: agentID, ConvID: convID, SocketPath: "/tmp/native-delete.sock",
		State: CodexAppServerReady,
	}))
	require.NoError(t, UpsertCodexNativePermissionProfile(CodexNativePermissionProfile{
		Generation: "native-delete-generation", ProfileName: "tclaude-agent-1234567890abcdef",
		ProfileTOML: "complete-native-delete",
	}))

	_, err = DeleteAgentByConvID(convID)
	require.NoError(t, err)
	profile, err := GetCodexNativePermissionProfile("native-delete-generation")
	require.NoError(t, err)
	require.NotNil(t, profile)
	assert.True(t, profile.CleanupPending,
		"conversation deletion and durable cleanup intent must share one commit")
}

func TestPruneOrdinaryNativeProfileWaitsForReadySuccessor(t *testing.T) {
	setupTestDB(t)
	base := time.Now().UTC().Add(-time.Minute)
	for _, profile := range []CodexNativePermissionProfile{
		{Generation: "ordinary-old", ProfileName: "tclaude-agent-0000000000000031",
			ProfileTOML: "old", OwnerAgentID: "agent", OwnerConvID: "conv",
			LaunchID: "old-launch", LaunchReady: true, CreatedAt: base},
		{Generation: "ordinary-new", ProfileName: "tclaude-agent-0000000000000032",
			ProfileTOML: "new", OwnerAgentID: "agent", OwnerConvID: "conv",
			LaunchID: "new-launch", LaunchReady: false, CreatedAt: base.Add(time.Second)},
	} {
		require.NoError(t, UpsertCodexNativePermissionProfile(profile))
	}
	pruned, err := PruneSupersededCodexNativePermissionProfiles()
	require.NoError(t, err)
	assert.Zero(t, pruned)
	marked, err := MarkCodexNativePermissionProfileLaunchReady("ordinary-new")
	require.NoError(t, err)
	require.True(t, marked)
	pruned, err = PruneSupersededCodexNativePermissionProfiles()
	require.NoError(t, err)
	assert.EqualValues(t, 1, pruned)
	old, err := GetCodexNativePermissionProfile("ordinary-old")
	require.NoError(t, err)
	assert.Nil(t, old)
}

func TestMarkCodexNativePermissionProfileLaunchReadyReportsMissingGeneration(t *testing.T) {
	setupTestDB(t)
	marked, err := MarkCodexNativePermissionProfileLaunchReady("missing-generation")
	require.NoError(t, err)
	assert.False(t, marked)
}

func TestNativeProfileOwnershipFollowsSessionDiscoveryAndEnrollment(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, UpsertCodexNativePermissionProfile(CodexNativePermissionProfile{
		Generation: "launch-owner", ProfileName: "tclaude-agent-0000000000000041",
		ProfileTOML: "owned", LaunchID: "session-owner",
	}))
	require.NoError(t, SaveSession(&SessionRow{ID: "session-owner", Harness: "codex"}))
	require.NoError(t, SetSessionConvID("session-owner", "conv-owner"))

	profile, err := GetCodexNativePermissionProfile("launch-owner")
	require.NoError(t, err)
	require.NotNil(t, profile)
	assert.Equal(t, "conv-owner", profile.OwnerConvID)
	assert.Empty(t, profile.OwnerAgentID)

	agentID, _, err := EnsureAgentForConv("conv-owner", "test")
	require.NoError(t, err)
	profile, err = GetCodexNativePermissionProfile("launch-owner")
	require.NoError(t, err)
	require.NotNil(t, profile)
	assert.Equal(t, agentID, profile.OwnerAgentID)
}
