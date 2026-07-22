package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDurableRelaunchProfilesSurviveSessionDeletion(t *testing.T) {
	setupTestDB(t)
	const (
		convID    = "durable-managed-conv"
		sessionID = "durable-managed-session"
	)
	agentID, _, err := EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, SaveSession(&SessionRow{
		ID: sessionID, ConvID: convID, Cwd: "/tmp/durable-managed",
		Harness: DefaultHarness, Status: "exited", SandboxMode: "on",
		ApprovalPolicy: "bypassPermissions", AskUserQuestionTimeout: "5m",
		ResumeProvenance: `{"version":1,"proof":"test"}`,
	}))
	require.NoError(t, UpdateSessionModelID(sessionID, "claude-sonnet-4-6"))
	require.NoError(t, UpdateSessionEffort(sessionID, "high"))
	require.NoError(t, UpdateContextSnapshot(sessionID, 25, 10, 20, 1_000_000))
	require.NoError(t, SetSessionRemoteControl(sessionID, true))
	require.NoError(t, SetSessionAutoMemory(sessionID, true))

	beforeAgent, err := AgentRelaunchProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, beforeAgent)
	beforeConv, err := ConversationResumeProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, beforeConv)
	require.NoError(t, DeleteSession(sessionID))

	afterAgent, err := AgentRelaunchProfileForConv(convID)
	require.NoError(t, err)
	afterConv, err := ConversationResumeProfileForConv(convID)
	require.NoError(t, err)
	assert.Equal(t, beforeAgent, afterAgent)
	assert.Equal(t, beforeConv, afterConv)
	assert.Equal(t, agentID, durableAgentIDForConv(t, convID))

	launch, err := SessionLaunchProfileForConv(convID)
	require.NoError(t, err)
	assert.Equal(t, "claude-sonnet-4-6", launch.ModelID)
	assert.Equal(t, "high", launch.Effort)
	assert.Equal(t, "on", launch.SandboxMode)
	assert.Equal(t, "bypassPermissions", launch.ApprovalPolicy)
	assert.Equal(t, "5m", mustAskTimeoutForConv(t, convID))
	assert.True(t, mustRemoteControlForConv(t, convID))
	assert.True(t, mustAutoMemoryForConv(t, convID))
}

func TestConversationFallbackPreservesUnmanagedLaunchShapeAfterPrune(t *testing.T) {
	setupTestDB(t)
	const (
		convID    = "plain-conversation"
		sessionID = "plain-session"
	)
	require.NoError(t, SaveSession(&SessionRow{
		ID: sessionID, ConvID: convID, Cwd: "/tmp/plain", Status: "exited",
		Harness: DefaultHarness, SandboxMode: "on", ApprovalPolicy: "default",
		AskUserQuestionTimeout: "10m", ResumeProvenance: "plain-proof",
	}))
	require.NoError(t, UpdateSessionModelID(sessionID, "claude-haiku-4-5"))
	require.NoError(t, UpdateSessionEffort(sessionID, "medium"))
	require.NoError(t, SetSessionRemoteControl(sessionID, true))
	require.NoError(t, DeleteSession(sessionID))

	state, err := AgentState(convID)
	require.NoError(t, err)
	assert.Equal(t, AgentStateNone, state, "plain conversation must remain unmanaged")
	profile, err := ConversationResumeProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, profile)
	require.NotNil(t, profile.FallbackRelaunch)
	assert.Equal(t, "plain-proof", profile.ResumeProvenance)

	launch, err := SessionLaunchProfileForConv(convID)
	require.NoError(t, err)
	assert.Equal(t, "claude-haiku-4-5", launch.ModelID)
	assert.Equal(t, "medium", launch.Effort)
	assert.Equal(t, "10m", mustAskTimeoutForConv(t, convID))
	assert.True(t, mustRemoteControlForConv(t, convID))
}

func TestSupersededSessionCannotOverwriteCurrentAgentRelaunchIntent(t *testing.T) {
	setupTestDB(t)
	const oldConv = "generation-old"
	const newConv = "generation-new"
	agentID, _, err := EnsureAgentForConv(oldConv, "test")
	require.NoError(t, err)
	require.NoError(t, SaveSession(&SessionRow{
		ID: "old-session", ConvID: oldConv, Cwd: "/tmp/old", Status: "exited",
		Harness: DefaultHarness, ApprovalPolicy: "default", AskUserQuestionTimeout: "5m",
	}))
	_, err = RotateAgentConv(oldConv, newConv, "test")
	require.NoError(t, err)
	assert.Equal(t, agentID, durableAgentIDForConv(t, newConv))
	require.NoError(t, SaveSession(&SessionRow{
		ID: "new-session", ConvID: newConv, Cwd: "/tmp/new", Status: "running",
		Harness: DefaultHarness, ApprovalPolicy: "bypassPermissions", AskUserQuestionTimeout: "10m",
	}))

	// A late hook/reaper write for the predecessor remains useful session and
	// conversation history, but cannot roll back the stable actor's policy.
	old, err := LoadSession("old-session")
	require.NoError(t, err)
	require.NotNil(t, old)
	old.ApprovalPolicy = "default"
	old.AskUserQuestionTimeout = "never"
	require.NoError(t, SaveSession(old))

	agent, err := AgentRelaunchProfileForConv(newConv)
	require.NoError(t, err)
	require.NotNil(t, agent)
	require.NotNil(t, agent.ApprovalPolicy)
	require.NotNil(t, agent.AskUserQuestionTimeout)
	assert.Equal(t, "bypassPermissions", *agent.ApprovalPolicy)
	assert.Equal(t, "10m", *agent.AskUserQuestionTimeout)
	oldProfile, err := ConversationResumeProfileForConv(oldConv)
	require.NoError(t, err)
	require.NotNil(t, oldProfile)
	require.NotNil(t, oldProfile.FallbackRelaunch)
	assert.Equal(t, "never", *oldProfile.FallbackRelaunch.AskUserQuestionTimeout)
}

func TestMigrateV145BackfillsThenDecouplesLegacySession(t *testing.T) {
	setupTestDB(t)
	const convID = "legacy-v144-conv"
	_, _, err := EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, SaveSession(&SessionRow{
		ID: "legacy-v144-session", ConvID: convID, Cwd: "/tmp/legacy-v144",
		Status: "exited", Harness: DefaultHarness, ApprovalPolicy: "default",
		AskUserQuestionTimeout: "5m", ResumeProvenance: "legacy-proof",
	}))
	d, err := Open()
	require.NoError(t, err)
	_, err = d.Exec(`DROP TABLE conversation_resume_profiles`)
	require.NoError(t, err)
	_, err = d.Exec(`ALTER TABLE agents DROP COLUMN relaunch_profile`)
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE schema_version SET version = 144`)
	require.NoError(t, err)

	require.NoError(t, migrateV144toV145(d))
	agent, err := AgentRelaunchProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, agent)
	conversation, err := ConversationResumeProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, conversation)
	assert.Equal(t, "/tmp/legacy-v144", conversation.Cwd)
	assert.Equal(t, "legacy-proof", conversation.ResumeProvenance)
	require.NoError(t, DeleteSession("legacy-v144-session"))
	assert.Equal(t, "5m", mustAskTimeoutForConv(t, convID))
}

func TestDurableRelaunchProfilesRejectUnknownVersions(t *testing.T) {
	setupTestDB(t)
	const convID = "unknown-profile-version"
	agentID, _, err := EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	d, err := Open()
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE agents SET relaunch_profile = '{"version":99}' WHERE agent_id = ?`, agentID)
	require.NoError(t, err)
	_, err = d.Exec(`INSERT INTO conversation_resume_profiles (conv_id, profile_json, updated_at)
		VALUES (?, '{"version":99}', 'now')`, convID)
	require.NoError(t, err)

	_, err = AgentRelaunchProfileForConv(convID)
	assert.ErrorContains(t, err, "unsupported agent relaunch profile version 99")
	_, err = ConversationResumeProfileForConv(convID)
	assert.ErrorContains(t, err, "unsupported conversation resume profile version 99")

	_, err = d.Exec(`UPDATE conversation_resume_profiles
		SET profile_json = '{"version":1,"harness":"claude","cwd":"/tmp/test","fallback_relaunch":{"version":99}}'
		WHERE conv_id = ?`, convID)
	require.NoError(t, err)
	_, err = ConversationResumeProfileForConv(convID)
	assert.ErrorContains(t, err, "unsupported conversation fallback relaunch profile version 99")
}

func durableAgentIDForConv(t *testing.T, convID string) string {
	t.Helper()
	v, err := AgentIDForConv(convID)
	require.NoError(t, err)
	return v
}

func mustAskTimeoutForConv(t *testing.T, convID string) string {
	t.Helper()
	v, err := AskTimeoutForConv(convID)
	require.NoError(t, err)
	return v
}

func mustRemoteControlForConv(t *testing.T, convID string) bool {
	t.Helper()
	v, err := RemoteControlForConv(convID)
	require.NoError(t, err)
	return v
}

func mustAutoMemoryForConv(t *testing.T, convID string) bool {
	t.Helper()
	v, err := AutoMemoryForConv(convID)
	require.NoError(t, err)
	return v
}
