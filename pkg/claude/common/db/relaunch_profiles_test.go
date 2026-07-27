package db

import (
	"testing"
	"time"

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

func TestContextSnapshotTokenOnlyFastPathPreservesProjection(t *testing.T) {
	setupTestDB(t)
	const (
		convID    = "context-window-projection-conv"
		sessionID = "context-window-projection-session"
	)
	require.NoError(t, SaveSession(&SessionRow{
		ID: sessionID, ConvID: convID, Cwd: "/tmp/context-window-projection",
		Harness: DefaultHarness, Status: "idle",
	}))
	require.NoError(t, UpdateContextSnapshot(sessionID, 25, 10, 20, 1_000_000))
	profile, err := ConversationResumeProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, profile)
	require.NotNil(t, profile.FallbackRelaunch)
	require.NotNil(t, profile.FallbackRelaunch.ContextWindowSize)
	assert.Equal(t, int64(1_000_000), *profile.FallbackRelaunch.ContextWindowSize)

	d, err := Open()
	require.NoError(t, err)
	const sentinel = "same-window-must-not-project"
	_, err = d.Exec(`UPDATE conversation_resume_profiles SET updated_at = ? WHERE conv_id = ?`, sentinel, convID)
	require.NoError(t, err)

	updated, err := UpdateContextSnapshotIfWindowUnchanged(
		sessionID, convID, time.Time{}, 50, 100, 200, 1_000_000,
	)
	require.NoError(t, err)
	require.True(t, updated)
	snapshot, err := GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.Equal(t, float64(50), snapshot.ContextPct)
	assert.Equal(t, int64(100), snapshot.TokensInput)
	assert.Equal(t, int64(200), snapshot.TokensOutput)
	var profileUpdatedAt string
	require.NoError(t, d.QueryRow(
		`SELECT updated_at FROM conversation_resume_profiles WHERE conv_id = ?`, convID,
	).Scan(&profileUpdatedAt))
	assert.Equal(t, sentinel, profileUpdatedAt,
		"token-only context changes must not rewrite durable relaunch profiles")

	updated, err = UpdateContextSnapshotIfWindowUnchanged(
		sessionID, convID, time.Time{}, 60, 120, 240, 200_000,
	)
	require.NoError(t, err)
	assert.False(t, updated, "a changed window must fall back to full projection")
	require.NoError(t, UpdateContextSnapshot(sessionID, 60, 120, 240, 200_000))
	require.NoError(t, d.QueryRow(
		`SELECT updated_at FROM conversation_resume_profiles WHERE conv_id = ?`, convID,
	).Scan(&profileUpdatedAt))
	assert.NotEqual(t, sentinel, profileUpdatedAt,
		"a context-window change still projects durable relaunch state")
	profile, err = ConversationResumeProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, profile)
	require.NotNil(t, profile.FallbackRelaunch)
	require.NotNil(t, profile.FallbackRelaunch.ContextWindowSize)
	assert.Equal(t, int64(200_000), *profile.FallbackRelaunch.ContextWindowSize)
}

func TestContextSnapshotWriteBatchCommitsFullAndFastUpdatesTogether(t *testing.T) {
	setupTestDB(t)
	const (
		fullConv    = "context-batch-full-conv"
		fullSession = "context-batch-full-session"
		fastConv    = "context-batch-fast-conv"
		fastSession = "context-batch-fast-session"
	)
	require.NoError(t, SaveSession(&SessionRow{
		ID: fullSession, ConvID: fullConv, Cwd: "/tmp/context-batch-full",
		Harness: DefaultHarness, Status: "idle",
	}))
	require.NoError(t, SaveSession(&SessionRow{
		ID: fastSession, ConvID: fastConv, Cwd: "/tmp/context-batch-fast",
		Harness: DefaultHarness, Status: "idle",
	}))
	require.NoError(t, UpdateContextSnapshot(fastSession, 20, 20, 2, 200_000))

	batch := NewContextSnapshotWriteBatch()
	fullIndex := batch.UpdateContextSnapshot(
		fullSession, fullConv, time.Time{}, 40, 400, 40, 1_000_000,
	)
	fastIndex := batch.UpdateContextSnapshotIfWindowUnchanged(
		fastSession, fastConv, time.Time{}, 30, 300, 30, 200_000,
	)

	before, err := GetContextSnapshot(fullSession)
	require.NoError(t, err)
	assert.Zero(t, before.ContextWindowSize, "enqueue must not open or execute the transaction")

	result, err := batch.Commit()
	require.NoError(t, err)
	require.Len(t, result.Applied, 2)
	assert.True(t, result.Applied[fullIndex])
	assert.True(t, result.Applied[fastIndex])
	assert.Positive(t, result.Project.Total)
	assert.Positive(t, result.Fast.Total)

	fullSnapshot, err := GetContextSnapshot(fullSession)
	require.NoError(t, err)
	assert.Equal(t, float64(40), fullSnapshot.ContextPct)
	assert.Equal(t, int64(1_000_000), fullSnapshot.ContextWindowSize)
	fastSnapshot, err := GetContextSnapshot(fastSession)
	require.NoError(t, err)
	assert.Equal(t, float64(30), fastSnapshot.ContextPct)
	assert.Equal(t, int64(300), fastSnapshot.TokensInput)

	profile, err := ConversationResumeProfileForConv(fullConv)
	require.NoError(t, err)
	require.NotNil(t, profile)
	require.NotNil(t, profile.FallbackRelaunch)
	require.NotNil(t, profile.FallbackRelaunch.ContextWindowSize)
	assert.Equal(t, int64(1_000_000), *profile.FallbackRelaunch.ContextWindowSize)
}

func TestContextSnapshotWriteBatchIsolatesProjectionFailure(t *testing.T) {
	setupTestDB(t)
	const (
		badConv     = "context-batch-bad-conv"
		badSession  = "context-batch-bad-session"
		goodConv    = "context-batch-good-conv"
		goodSession = "context-batch-good-session"
	)
	require.NoError(t, SaveSession(&SessionRow{
		ID: badSession, ConvID: badConv, Cwd: "/tmp/context-batch-bad",
		Harness: DefaultHarness, Status: "idle",
	}))
	require.NoError(t, SaveSession(&SessionRow{
		ID: goodSession, ConvID: goodConv, Cwd: "/tmp/context-batch-good",
		Harness: DefaultHarness, Status: "idle",
	}))
	require.NoError(t, UpdateContextSnapshot(goodSession, 20, 20, 2, 200_000))
	d, err := Open()
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE conversation_resume_profiles
		SET profile_json = '{"version":99}', updated_at = 'now'
		WHERE conv_id = ?`, badConv)
	require.NoError(t, err)

	batch := NewContextSnapshotWriteBatch()
	badIndex := batch.UpdateContextSnapshot(
		badSession, badConv, time.Time{}, 40, 400, 40, 1_000_000,
	)
	goodIndex := batch.UpdateContextSnapshotIfWindowUnchanged(
		goodSession, goodConv, time.Time{}, 30, 300, 30, 200_000,
	)
	result, err := batch.Commit()
	require.NoError(t, err, "an operation error must not abort the outer transaction")
	require.Len(t, result.OperationErrors, 1)
	assert.ErrorContains(t, result.OperationErrors[0], "unsupported conversation resume profile version 99")
	assert.False(t, result.Applied[badIndex])
	assert.True(t, result.Applied[goodIndex])

	badSnapshot, err := GetContextSnapshot(badSession)
	require.NoError(t, err)
	assert.Zero(t, badSnapshot.ContextWindowSize,
		"the failed operation's session update must roll back to its savepoint")
	goodSnapshot, err := GetContextSnapshot(goodSession)
	require.NoError(t, err)
	assert.Equal(t, float64(30), goodSnapshot.ContextPct)
	assert.Equal(t, int64(300), goodSnapshot.TokensInput)
}

func TestContextSnapshotWriteBatchGuardsGenerationAndAllZeroSnapshot(t *testing.T) {
	setupTestDB(t)
	const (
		oldConv     = "context-batch-old-generation"
		newConv     = "context-batch-new-generation"
		sessionID   = "context-batch-reused-session"
		zeroConv    = "context-batch-zero-guard-conv"
		zeroSession = "context-batch-zero-guard-session"
	)
	require.NoError(t, SaveSession(&SessionRow{
		ID: sessionID, ConvID: oldConv, Cwd: "/tmp/context-batch-old",
		Harness: DefaultHarness, Status: "idle",
	}))
	staleBatch := NewContextSnapshotWriteBatch()
	staleUpdate := staleBatch.UpdateContextSnapshot(
		sessionID, oldConv, time.Time{}, 70, 700, 70, 1_000_000,
	)
	staleReset := staleBatch.ResetCompact(sessionID, oldConv, time.Time{})
	require.NoError(t, DeleteSession(sessionID))
	require.NoError(t, SaveSession(&SessionRow{
		ID: sessionID, ConvID: newConv, Cwd: "/tmp/context-batch-new",
		Harness: DefaultHarness, Status: "idle",
	}))
	staleResult, err := staleBatch.Commit()
	require.NoError(t, err)
	assert.False(t, staleResult.Applied[staleUpdate])
	assert.False(t, staleResult.Applied[staleReset])
	recreated, err := GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.Zero(t, recreated.ContextWindowSize,
		"deferred operations must not modify a replacement session generation")

	require.NoError(t, SaveSession(&SessionRow{
		ID: zeroSession, ConvID: zeroConv, Cwd: "/tmp/context-batch-zero",
		Harness: DefaultHarness, Status: "idle",
	}))
	require.NoError(t, UpdateContextSnapshot(zeroSession, 25, 250, 25, 200_000))
	zeroBatch := NewContextSnapshotWriteBatch()
	zeroIndex := zeroBatch.UpdateContextSnapshot(
		zeroSession, zeroConv, time.Time{}, 0, 0, 0, 0,
	)
	zeroResult, err := zeroBatch.Commit()
	require.NoError(t, err)
	assert.True(t, zeroResult.Applied[zeroIndex])
	preserved, err := GetContextSnapshot(zeroSession)
	require.NoError(t, err)
	assert.Equal(t, float64(25), preserved.ContextPct)
	assert.Equal(t, int64(200_000), preserved.ContextWindowSize)
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
	require.NoError(t, SetSessionAutoMemory(sessionID, true))
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
	assert.True(t, mustAutoMemoryForConv(t, convID))
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

func TestTemporarySandboxModeIsAgentKeyedAcrossRotationAndProjection(t *testing.T) {
	setupTestDB(t)
	const (
		oldConv = "sandbox-override-old"
		newConv = "sandbox-override-new"
	)
	agentID, _, err := EnsureAgentForConv(oldConv, "test")
	require.NoError(t, err)
	normalMode, normalSource := "on", "group default profile \"confined\""
	require.NoError(t, SetAgentRelaunchProfile(agentID, AgentRelaunchProfile{
		Version: RelaunchProfileVersion, SandboxMode: &normalMode,
		SandboxModeSource: &normalSource,
	}))

	override := "off"
	require.NoError(t, SetTemporarySandboxModeForConv(
		oldConv, normalMode, normalSource, &override,
	))
	mode, active, err := TemporarySandboxModeForAgent(agentID)
	require.NoError(t, err)
	assert.True(t, active)
	assert.Equal(t, override, mode)

	// Process telemetry from the unlocked launch must not promote "off" to the
	// stable normal posture.
	require.NoError(t, SaveSession(&SessionRow{
		ID: "sandbox-override-session", ConvID: oldConv, Cwd: "/tmp/unlocked",
		Harness: DefaultHarness, Status: "idle", SandboxMode: override,
	}))
	profile, err := AgentRelaunchProfileForConv(oldConv)
	require.NoError(t, err)
	require.NotNil(t, profile)
	require.NotNil(t, profile.SandboxMode)
	assert.Equal(t, normalMode, *profile.SandboxMode)
	launch, err := SessionLaunchProfileForConv(oldConv)
	require.NoError(t, err)
	assert.Equal(t, override, launch.SandboxMode)
	assert.Equal(t, TemporarySandboxModeSource, launch.SandboxModeSource)

	_, err = RotateAgentConv(oldConv, newConv, "clear")
	require.NoError(t, err)
	mode, active, err = TemporarySandboxModeForConv(newConv)
	require.NoError(t, err)
	assert.True(t, active, "the override follows the stable agent, not a conversation generation")
	assert.Equal(t, override, mode)

	require.NoError(t, SetTemporarySandboxModeForConv(newConv, "", "", nil))
	_, active, err = TemporarySandboxModeForAgent(agentID)
	require.NoError(t, err)
	assert.False(t, active)
	profile, err = AgentRelaunchProfileForConv(newConv)
	require.NoError(t, err)
	require.NotNil(t, profile)
	assert.Equal(t, normalMode, *profile.SandboxMode)
	assert.Equal(t, normalSource, *profile.SandboxModeSource)
	launch, err = SessionLaunchProfileForConv(newConv)
	require.NoError(t, err)
	assert.Equal(t, normalMode, launch.SandboxMode)
	assert.Equal(t, normalSource, launch.SandboxModeSource)
}

func TestOlderSameConversationSessionCannotRollBackDurableIntent(t *testing.T) {
	setupTestDB(t)
	const convID = "same-conversation-generations"
	_, _, err := EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	oldCreated := time.Now().Add(-time.Hour)
	newCreated := time.Now()
	require.NoError(t, SaveSession(&SessionRow{
		ID: "same-conv-old", ConvID: convID, Cwd: "/tmp/same-old",
		Harness: "codex", Status: "exited", SandboxMode: "read-only",
		ApprovalPolicy: "untrusted", CreatedAt: oldCreated,
	}))
	require.NoError(t, SaveSession(&SessionRow{
		ID: "same-conv-new", ConvID: convID, Cwd: "/tmp/same-new",
		Harness: "codex", Status: "running", SandboxMode: "workspace-write",
		ApprovalPolicy: "never", CreatedAt: newCreated,
	}))
	require.NoError(t, SetSessionRemoteControl("same-conv-old", true))
	require.NoError(t, UpdateSessionModelID("same-conv-old", "stale-model"))

	conversation, err := ConversationResumeProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, conversation)
	assert.Equal(t, "/tmp/same-new", conversation.Cwd)
	require.NotNil(t, conversation.FallbackRelaunch)
	require.NotNil(t, conversation.FallbackRelaunch.ApprovalPolicy)
	assert.Equal(t, "never", *conversation.FallbackRelaunch.ApprovalPolicy)
	assert.Equal(t, "same-conv-new", conversation.SourceSessionID)
	agent, err := AgentRelaunchProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, agent)
	require.NotNil(t, agent.ApprovalPolicy)
	assert.Equal(t, "never", *agent.ApprovalPolicy)
	assert.Nil(t, agent.ModelID, "stale model telemetry must not reach stable intent")
}

func TestNonCodexSessionProjectionDoesNotInventCodexApproval(t *testing.T) {
	setupTestDB(t)
	const convID = "server-authoritative-conversation"
	_, _, err := EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, SaveSession(&SessionRow{
		ID: "server-authoritative-session", ConvID: convID, Cwd: "/tmp/server-authoritative",
		Harness: "opencode", Status: "idle", SandboxMode: "off", AskUserQuestionTimeout: "5m",
		RemoteControl: true, AutoMemory: true,
	}))
	require.NoError(t, SetSessionRemoteControl("server-authoritative-session", true))
	require.NoError(t, SetSessionAutoMemory("server-authoritative-session", true))

	conversation, err := ConversationResumeProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, conversation)
	require.NotNil(t, conversation.FallbackRelaunch)
	require.NotNil(t, conversation.FallbackRelaunch.SandboxMode)
	assert.Equal(t, "off", *conversation.FallbackRelaunch.SandboxMode)
	require.NotNil(t, conversation.FallbackRelaunch.ApprovalPolicy)
	assert.Empty(t, *conversation.FallbackRelaunch.ApprovalPolicy)

	agent, err := AgentRelaunchProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, agent)
	require.NotNil(t, agent.SandboxMode)
	assert.Equal(t, "off", *agent.SandboxMode)
	require.NotNil(t, agent.ApprovalPolicy)
	assert.Empty(t, *agent.ApprovalPolicy)
	require.NotNil(t, agent.AskUserQuestionTimeout)
	assert.Empty(t, *agent.AskUserQuestionTimeout)
	require.NotNil(t, agent.RemoteControl)
	assert.False(t, *agent.RemoteControl)
	require.NotNil(t, agent.AutoMemory)
	assert.False(t, *agent.AutoMemory)
}

func TestBlankInitialSessionProjectionPreservesExactAgentBirthIntent(t *testing.T) {
	setupTestDB(t)
	const convID = "birth-profile-before-telemetry"
	agentID, _, err := EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	model := "claude-opus-4-8"
	effort := "high"
	window := int64(1_000_000)
	remoteControl := true
	autoMemory := true
	require.NoError(t, SetAgentRelaunchProfile(agentID, AgentRelaunchProfile{
		Version: RelaunchProfileVersion, ModelID: &model, Effort: &effort,
		ContextWindowSize: &window, RemoteControl: &remoteControl, AutoMemory: &autoMemory,
	}))
	require.NoError(t, SaveSession(&SessionRow{
		ID: "birth-profile-session", ConvID: convID, Cwd: "/tmp/birth-profile",
		Harness: DefaultHarness, Status: "idle", SandboxMode: "on", ApprovalPolicy: "auto",
	}))

	profile, err := AgentRelaunchProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, profile)
	require.NotNil(t, profile.ModelID)
	assert.Equal(t, model, *profile.ModelID)
	require.NotNil(t, profile.Effort)
	assert.Equal(t, effort, *profile.Effort)
	require.NotNil(t, profile.ContextWindowSize)
	assert.Equal(t, window, *profile.ContextWindowSize)
	require.NotNil(t, profile.RemoteControl)
	assert.True(t, *profile.RemoteControl)
	require.NotNil(t, profile.AutoMemory)
	assert.True(t, *profile.AutoMemory)
	require.NotNil(t, profile.SandboxMode)
	assert.Equal(t, "on", *profile.SandboxMode)
}

func TestNewUnmanagedGenerationDoesNotInheritPreviousToggles(t *testing.T) {
	setupTestDB(t)
	const convID = "plain-toggle-generations"
	require.NoError(t, SaveSession(&SessionRow{
		ID: "plain-toggle-old", ConvID: convID, Cwd: "/tmp/plain-toggle",
		Harness: DefaultHarness, CreatedAt: time.Now().Add(-time.Hour),
	}))
	require.NoError(t, SetSessionRemoteControl("plain-toggle-old", true))
	require.NoError(t, SetSessionAutoMemory("plain-toggle-old", true))
	assert.True(t, mustRemoteControlForConv(t, convID))
	assert.True(t, mustAutoMemoryForConv(t, convID))

	require.NoError(t, SaveSession(&SessionRow{
		ID: "plain-toggle-new", ConvID: convID, Cwd: "/tmp/plain-toggle",
		Harness: DefaultHarness, CreatedAt: time.Now(),
	}))
	require.NoError(t, DeleteSession("plain-toggle-old"))
	require.NoError(t, DeleteSession("plain-toggle-new"))
	assert.False(t, mustRemoteControlForConv(t, convID))
	assert.False(t, mustAutoMemoryForConv(t, convID))
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

// TestObservedAutoCompactWindowReachesTheAgentSpine is the regression guard for
// the agents-spine merge. relaunchProfileForSpawn freezes a non-nil
// AutoCompactWindow at birth — INCLUDING ptr("") for an agent that pinned
// nothing — and AutoCompactWindowForConv short-circuits on the spine value. So a
// spine merge that never accepts a freshly projected window would freeze that
// birth "" forever: an operator who later exports
// CLAUDE_CODE_AUTO_COMPACT_WINDOW would see the live bar re-base correctly while
// every reincarnate / clone / resume silently reverted the successor to the
// model's full window — exactly what UpdateSessionAutoCompactWindow's
// "observers may only ADD a pin" contract promises cannot happen.
func TestObservedAutoCompactWindowReachesTheAgentSpine(t *testing.T) {
	setupTestDB(t)
	const (
		convID    = "acw-spine-conv"
		sessionID = "acw-spine-session"
	)
	agentID, _, err := EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, SaveSession(&SessionRow{
		ID: sessionID, ConvID: convID, Cwd: "/tmp/acw-spine",
		Harness: DefaultHarness, Status: "idle",
	}))

	// Birth: nothing pinned, recorded as KNOWN intent (the empty string).
	require.NoError(t, SetAgentRelaunchProfile(agentID, AgentRelaunchProfile{
		Version: RelaunchProfileVersion, AutoCompactWindow: stringPtr(""),
	}))
	require.NoError(t, SetSessionAutoCompactWindow(sessionID, ""))
	window, err := AutoCompactWindowForConv(convID)
	require.NoError(t, err)
	require.Empty(t, window, "birth state: nothing pinned")

	// The operator exports the variable and restarts the pane; the status line
	// records what it observes.
	require.NoError(t, UpdateSessionAutoCompactWindow(sessionID, "450000"))
	// A later hook tick projects it (the plain UPDATE does not project itself).
	require.NoError(t, SaveSession(&SessionRow{
		ID: sessionID, ConvID: convID, Cwd: "/tmp/acw-spine",
		Harness: DefaultHarness, Status: "working",
	}))

	spine, err := AgentRelaunchProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, spine)
	require.NotNil(t, spine.AutoCompactWindow)
	assert.Equal(t, "450000", *spine.AutoCompactWindow,
		"the agents-spine merge must accept an observed window, or every relaunch loses the pin")

	window, err = AutoCompactWindowForConv(convID)
	require.NoError(t, err)
	assert.Equal(t, "450000", window,
		"the relaunch readers must see the observed window")
}

// TestLaunchCanAssertNothingPinnedOverAnObservedWindow: the other direction —
// only the LAUNCH path may say "nothing pinned", and it must win. The observer
// is a no-op on the empty string precisely so the two can share a column.
func TestLaunchCanAssertNothingPinnedOverAnObservedWindow(t *testing.T) {
	setupTestDB(t)
	const (
		convID    = "acw-clear-conv"
		sessionID = "acw-clear-session"
	)
	_, _, err := EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, SaveSession(&SessionRow{
		ID: sessionID, ConvID: convID, Cwd: "/tmp/acw-clear",
		Harness: DefaultHarness, Status: "idle",
	}))

	require.NoError(t, SetSessionAutoCompactWindow(sessionID, "450000"))
	window, err := AutoCompactWindowForConv(convID)
	require.NoError(t, err)
	require.Equal(t, "450000", window)

	// An observer cannot erase it...
	require.NoError(t, UpdateSessionAutoCompactWindow(sessionID, ""))
	window, err = AutoCompactWindowForConv(convID)
	require.NoError(t, err)
	assert.Equal(t, "450000", window, "an observer must never erase a recorded pin")

	// ...but a relaunch that deliberately pins nothing can.
	require.NoError(t, SetSessionAutoCompactWindow(sessionID, ""))
	window, err = AutoCompactWindowForConv(convID)
	require.NoError(t, err)
	assert.Empty(t, window, "the launch path may assert nothing pinned")
}
