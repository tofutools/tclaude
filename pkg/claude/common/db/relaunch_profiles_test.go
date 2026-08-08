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
		Harness: DefaultHarness, Status: "exited", HarnessBuiltinMode: "on",
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
	assert.Equal(t, "on", launch.HarnessBuiltinMode)
	assert.Equal(t, "bypassPermissions", launch.ApprovalPolicy)
	assert.Equal(t, "5m", mustAskTimeoutForConv(t, convID))
	assert.True(t, mustRemoteControlForConv(t, convID))
	assert.True(t, mustAutoMemoryForConv(t, convID))
}

func TestSessionProjectionPreservesExplicitFastMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode string
		want bool
	}{{"fast", "on", true}, {"standard", "off", false}} {
		t.Run(tc.name, func(t *testing.T) {
			setupTestDB(t)
			convID := "fast-mode-projection-" + tc.name
			sessionID := "fast-mode-session-" + tc.name
			require.NoError(t, SaveSession(&SessionRow{
				ID: sessionID, ConvID: convID, Cwd: "/tmp/fast-mode-projection",
				Harness: "codex", Status: "idle",
			}))
			require.NoError(t, SetSessionFastMode(sessionID, tc.mode))

			// A later status projection rebuilds the conversation fallback from
			// session columns. Fast mode has no session column, so the same
			// generation must retain the explicit launch fact already recorded.
			require.NoError(t, UpdateContextSnapshot(sessionID, 25, 10, 20, 200_000))
			profile, err := ConversationResumeProfileForConv(convID)
			require.NoError(t, err)
			require.NotNil(t, profile)
			require.NotNil(t, profile.FallbackRelaunch)
			require.NotNil(t, profile.FallbackRelaunch.FastMode)
			assert.Equal(t, tc.want, *profile.FallbackRelaunch.FastMode)
		})
	}
}

// TestSessionProjectionPreservesTheCopilotLaunchFacts is the same shape as the
// fast-mode test above, and it was NOT passing before the carry-forward it
// pins: this projection rebuilt the conversation fallback from session columns
// on every SaveSession, and neither Copilot launch fact has a session column,
// so both were dropped by the first status tick after the launch recorded them.
//
// The drive matters more than the cap. Since TCL-1058 it decides whether a
// message to this conversation is delivered over RPC or TYPED into its pane, so
// an erased record does not degrade a badge — it routes a connected agent back
// onto keystrokes. For a conversation with no stable agent profile of its own
// (every clone, every direct `session new --copilot-api`) this fallback is the
// ONLY holder, so the erasure was permanent.
//
// The second projection is driven at a NEW generation on purpose. A resume
// mints a new session row, and its first SaveSession lands before the launch
// re-asserts the posture — so a carry gated on sameSourceGeneration would leave
// the record missing across exactly that window.
func TestSessionProjectionPreservesTheCopilotLaunchFacts(t *testing.T) {
	setupTestDB(t)
	const convID = "copilot-launch-facts-conv"

	require.NoError(t, SaveSession(&SessionRow{
		ID: "copilot-launch-facts-1", ConvID: convID, Cwd: "/tmp/copilot-launch-facts",
		Harness: "copilot", Status: "idle",
	}))
	require.NoError(t, SetSessionCopilotAPI("copilot-launch-facts-1", true))
	require.NoError(t, SetSessionConfiguredContextWindowMax("copilot-launch-facts-1", 128_000))

	// A later status projection on the SAME generation — a hook tick.
	require.NoError(t, UpdateContextSnapshot("copilot-launch-facts-1", 25, 10, 20, 128_000))
	// And a projection from a NEW generation — the first write of a resume,
	// before that resume has re-asserted anything.
	require.NoError(t, SaveSession(&SessionRow{
		ID: "copilot-launch-facts-2", ConvID: convID, Cwd: "/tmp/copilot-launch-facts",
		Harness: "copilot", Status: "idle",
	}))

	profile, err := ConversationResumeProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, profile)
	require.NotNil(t, profile.FallbackRelaunch)
	require.NotNil(t, profile.FallbackRelaunch.CopilotAPI,
		"the recorded drive must survive a projection that has nothing to say about it; "+
			"a missing record reads as 'this agent chose keystrokes'")
	assert.True(t, *profile.FallbackRelaunch.CopilotAPI)
	require.NotNil(t, profile.FallbackRelaunch.ConfiguredContextWindowMax)
	assert.Equal(t, int64(128_000), *profile.FallbackRelaunch.ConfiguredContextWindowMax)
}

// The other direction, which is what makes the unconditional carry above safe
// rather than a way to pin a stale posture: a launch that turns the drive off
// writes false over it, and false is what the record then says.
func TestALaterLaunchCanTurnTheRecordedCopilotDriveOff(t *testing.T) {
	setupTestDB(t)
	const convID = "copilot-drive-off-conv"

	require.NoError(t, SaveSession(&SessionRow{
		ID: "copilot-drive-off-1", ConvID: convID, Cwd: "/tmp/copilot-drive-off",
		Harness: "copilot", Status: "idle",
	}))
	require.NoError(t, SetSessionCopilotAPI("copilot-drive-off-1", true))

	require.NoError(t, SaveSession(&SessionRow{
		ID: "copilot-drive-off-2", ConvID: convID, Cwd: "/tmp/copilot-drive-off",
		Harness: "copilot", Status: "idle",
	}))
	require.NoError(t, SetSessionCopilotAPI("copilot-drive-off-2", false))

	profile, err := ConversationResumeProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, profile.FallbackRelaunch.CopilotAPI)
	assert.False(t, *profile.FallbackRelaunch.CopilotAPI,
		"every relaunch re-asserts this posture INCLUDING its zero value, which is why "+
			"carrying it across projections cannot make it permanent")
}

// The direction of the carry above, which is the half that is easy to get
// wrong: it must reach the CONVERSATION fallback and must not reach the stable
// agent's frozen posture.
//
// Those two records answer different questions. The agent's is what the agent
// CHOSE, frozen at birth and replayed by every daemon relaunch; the
// conversation's is what the last launch on this conversation actually did. A
// carry that flowed upward would let the second overwrite the first — and the
// launch that would do it is not exotic: `tclaude conv resume` threads no
// Copilot drive at all, so it records a truthful `false` for its own send-keys
// launch, and one such resume would permanently un-choose a managed agent's
// drive and drop its configured meter denominator.
func TestTheCopilotCarryDoesNotOverwriteTheAgentsFrozenPosture(t *testing.T) {
	setupTestDB(t)
	const convID = "copilot-frozen-posture-conv"
	agentID, _, err := EnsureAgentForConv(convID, "test")
	require.NoError(t, err)

	api, max := true, int64(128_000)
	require.NoError(t, SetAgentRelaunchProfile(agentID, AgentRelaunchProfile{
		Version: RelaunchProfileVersion, CopilotAPI: &api, ConfiguredContextWindowMax: &max,
	}))

	// A plain-CLI resume: a new session row, then the zero values that path
	// records because it never carried either field.
	require.NoError(t, SaveSession(&SessionRow{
		ID: "copilot-frozen-posture-1", ConvID: convID,
		Cwd: "/tmp/copilot-frozen-posture", Harness: "copilot", Status: "idle",
	}))
	require.NoError(t, SetSessionCopilotAPI("copilot-frozen-posture-1", false))
	require.NoError(t, SetSessionConfiguredContextWindowMax("copilot-frozen-posture-1", 0))
	// And any later projection tick, which is what would carry it upward.
	require.NoError(t, UpdateContextSnapshot("copilot-frozen-posture-1", 25, 10, 20, 128_000))

	profile, err := AgentRelaunchProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, profile)
	require.NotNil(t, profile.CopilotAPI)
	assert.True(t, *profile.CopilotAPI,
		"the agent's frozen drive is written once, at birth; a launch on its "+
			"conversation must not be able to overwrite it")
	require.NotNil(t, profile.ConfiguredContextWindowMax)
	assert.Equal(t, int64(128_000), *profile.ConfiguredContextWindowMax)
}

// The daemon-side writer, which exists because the launched process's own write
// is best-effort and lands too late to decide routing for the window before it.
// It has to work with nothing else in the database — that is the state a freshly
// minted conv id is in.
func TestSetConversationCopilotAPISeedsAConversationWithNoProfileYet(t *testing.T) {
	setupTestDB(t)
	const convID = "copilot-seeded-conv"

	require.NoError(t, SetConversationCopilotAPI(convID, "copilot", "/tmp/copilot-seeded", true))

	profile, err := ConversationResumeProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, profile)
	assert.Equal(t, "copilot", profile.Harness)
	assert.Equal(t, "/tmp/copilot-seeded", profile.Cwd)
	require.NotNil(t, profile.FallbackRelaunch.CopilotAPI)
	assert.True(t, *profile.FallbackRelaunch.CopilotAPI)

	// And it does not clobber what a launch records afterwards.
	require.NoError(t, SaveSession(&SessionRow{
		ID: "copilot-seeded-1", ConvID: convID, Cwd: "/tmp/copilot-seeded",
		Harness: "copilot", Status: "idle", ApprovalPolicy: "never",
	}))
	after, err := ConversationResumeProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, after.FallbackRelaunch.CopilotAPI)
	assert.True(t, *after.FallbackRelaunch.CopilotAPI,
		"the launch's own projection must inherit the daemon's record, not erase it")
	require.NotNil(t, after.FallbackRelaunch.ApprovalPolicy)
	assert.Equal(t, "never", *after.FallbackRelaunch.ApprovalPolicy)
}

func TestContextSnapshotTokenOnlyFastPathPreservesProjection(t *testing.T) {
	setupTestDB(t)
	const (
		convID    = "context-window-projection-conv"
		sessionID = "context-window-projection-session"
	)
	row := &SessionRow{
		ID: sessionID, ConvID: convID, Cwd: "/tmp/context-window-projection",
		Harness: DefaultHarness, Status: "idle",
	}
	require.NoError(t, SaveSession(row))
	require.NoError(t, UpdateContextSnapshot(sessionID, 25, 10, 20, 1_000_000))
	profile, err := ConversationResumeProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, profile)
	require.NotNil(t, profile.FallbackRelaunch)
	require.NotNil(t, profile.FallbackRelaunch.ContextWindowSize)
	assert.Equal(t, int64(1_000_000), *profile.FallbackRelaunch.ContextWindowSize)

	d, err := Open()
	require.NoError(t, err)
	sentinel := time.Date(2025, 1, 2, 3, 4, 5, 6, time.UTC)
	_, err = d.Exec(`UPDATE conversation_resume_profiles SET updated_at = ? WHERE conv_id = ?`, dbTime(sentinel), convID)
	require.NoError(t, err)

	updated, err := UpdateContextSnapshotIfWindowUnchanged(
		sessionID, convID, row.CreatedAt, 50, 100, 200, 1_000_000,
	)
	require.NoError(t, err)
	require.True(t, updated)
	snapshot, err := GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.Equal(t, float64(50), snapshot.ContextPct)
	assert.Equal(t, int64(100), snapshot.TokensInput)
	assert.Equal(t, int64(200), snapshot.TokensOutput)
	var profileUpdatedAt dbTimestamp
	require.NoError(t, d.QueryRow(
		`SELECT updated_at FROM conversation_resume_profiles WHERE conv_id = ?`, convID,
	).Scan(&profileUpdatedAt))
	assert.Equal(t, sentinel, profileUpdatedAt.Time(),
		"token-only context changes must not rewrite durable relaunch profiles")

	updated, err = UpdateContextSnapshotIfWindowUnchanged(
		sessionID, convID, row.CreatedAt, 60, 120, 240, 200_000,
	)
	require.NoError(t, err)
	assert.False(t, updated, "a changed window must fall back to full projection")
	require.NoError(t, UpdateContextSnapshot(sessionID, 60, 120, 240, 200_000))
	require.NoError(t, d.QueryRow(
		`SELECT updated_at FROM conversation_resume_profiles WHERE conv_id = ?`, convID,
	).Scan(&profileUpdatedAt))
	assert.NotEqual(t, sentinel, profileUpdatedAt.Time(),
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
	fullRow := &SessionRow{
		ID: fullSession, ConvID: fullConv, Cwd: "/tmp/context-batch-full",
		Harness: DefaultHarness, Status: "idle",
	}
	require.NoError(t, SaveSession(fullRow))
	fastRow := &SessionRow{
		ID: fastSession, ConvID: fastConv, Cwd: "/tmp/context-batch-fast",
		Harness: DefaultHarness, Status: "idle",
	}
	require.NoError(t, SaveSession(fastRow))
	require.NoError(t, UpdateContextSnapshot(fastSession, 20, 20, 2, 200_000))

	batch := NewContextSnapshotWriteBatch()
	fullIndex := batch.UpdateContextSnapshot(
		fullSession, fullConv, fullRow.CreatedAt, 40, 400, 40, 1_000_000,
	)
	fastIndex := batch.UpdateContextSnapshotIfWindowUnchanged(
		fastSession, fastConv, fastRow.CreatedAt, 30, 300, 30, 200_000,
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
	badRow := &SessionRow{
		ID: badSession, ConvID: badConv, Cwd: "/tmp/context-batch-bad",
		Harness: DefaultHarness, Status: "idle",
	}
	require.NoError(t, SaveSession(badRow))
	goodRow := &SessionRow{
		ID: goodSession, ConvID: goodConv, Cwd: "/tmp/context-batch-good",
		Harness: DefaultHarness, Status: "idle",
	}
	require.NoError(t, SaveSession(goodRow))
	require.NoError(t, UpdateContextSnapshot(goodSession, 20, 20, 2, 200_000))
	d, err := Open()
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE conversation_resume_profiles
		SET profile_json = '{"version":99}', updated_at = 1767225600000000000
		WHERE conv_id = ?`, badConv)
	require.NoError(t, err)

	batch := NewContextSnapshotWriteBatch()
	badIndex := batch.UpdateContextSnapshot(
		badSession, badConv, badRow.CreatedAt, 40, 400, 40, 1_000_000,
	)
	goodIndex := batch.UpdateContextSnapshotIfWindowUnchanged(
		goodSession, goodConv, goodRow.CreatedAt, 30, 300, 30, 200_000,
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
		Harness: DefaultHarness, HarnessBuiltinMode: "on", ApprovalPolicy: "default",
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

func TestTemporaryHarnessBuiltinModeIsAgentKeyedAcrossRotationAndProjection(t *testing.T) {
	setupTestDB(t)
	const (
		oldConv = "sandbox-override-old"
		newConv = "sandbox-override-new"
	)
	agentID, _, err := EnsureAgentForConv(oldConv, "test")
	require.NoError(t, err)
	normalMode, normalSource := "on", "group default profile \"confined\""
	normalImplementation := "tclaude-layer"
	require.NoError(t, SetAgentRelaunchProfile(agentID, AgentRelaunchProfile{
		Version: RelaunchProfileVersion, HarnessBuiltinMode: &normalMode,
		HarnessBuiltinModeSource: &normalSource,
	}))

	override := "off"
	require.NoError(t, SetTemporaryHarnessBuiltinModeForConv(
		oldConv, normalMode, normalImplementation, normalSource, &override,
	))
	mode, active, err := TemporaryHarnessBuiltinModeForAgent(agentID)
	require.NoError(t, err)
	assert.True(t, active)
	assert.Equal(t, override, mode)

	// Process telemetry from the unlocked launch must not promote "off" to the
	// stable normal posture.
	require.NoError(t, SaveSession(&SessionRow{
		ID: "sandbox-override-session", ConvID: oldConv, Cwd: "/tmp/unlocked",
		Harness: DefaultHarness, Status: "idle", HarnessBuiltinMode: override,
		SandboxImplementation: "harness-builtin",
	}))
	profile, err := AgentRelaunchProfileForConv(oldConv)
	require.NoError(t, err)
	require.NotNil(t, profile)
	require.NotNil(t, profile.HarnessBuiltinMode)
	assert.Equal(t, normalMode, *profile.HarnessBuiltinMode)
	require.NotNil(t, profile.SandboxImplementation)
	assert.Equal(t, normalImplementation, *profile.SandboxImplementation,
		"process-only implementation must not replace the exact normal implementation")
	launch, err := SessionLaunchProfileForConv(oldConv)
	require.NoError(t, err)
	assert.Equal(t, override, launch.HarnessBuiltinMode)
	assert.Equal(t, "harness-builtin", launch.SandboxImplementation,
		"non-daemon relaunches must not re-enable the durable outer layer while temporarily off")
	assert.Equal(t, TemporaryHarnessBuiltinModeSource, launch.HarnessBuiltinModeSource)

	_, err = RotateAgentConv(oldConv, newConv, "clear")
	require.NoError(t, err)
	mode, active, err = TemporaryHarnessBuiltinModeForConv(newConv)
	require.NoError(t, err)
	assert.True(t, active, "the override follows the stable agent, not a conversation generation")
	assert.Equal(t, override, mode)

	require.NoError(t, SetTemporaryHarnessBuiltinModeForConv(newConv, "", "", "", nil))
	_, active, err = TemporaryHarnessBuiltinModeForAgent(agentID)
	require.NoError(t, err)
	assert.False(t, active)
	profile, err = AgentRelaunchProfileForConv(newConv)
	require.NoError(t, err)
	require.NotNil(t, profile)
	assert.Equal(t, normalMode, *profile.HarnessBuiltinMode)
	assert.Equal(t, normalSource, *profile.HarnessBuiltinModeSource)
	assert.Equal(t, normalImplementation, *profile.SandboxImplementation)
	launch, err = SessionLaunchProfileForConv(newConv)
	require.NoError(t, err)
	assert.Equal(t, normalMode, launch.HarnessBuiltinMode)
	assert.Equal(t, normalSource, launch.HarnessBuiltinModeSource)
	assert.Equal(t, normalImplementation, launch.SandboxImplementation)
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
		Harness: "codex", Status: "exited", HarnessBuiltinMode: "read-only",
		ApprovalPolicy: "untrusted", CreatedAt: oldCreated,
	}))
	require.NoError(t, SaveSession(&SessionRow{
		ID: "same-conv-new", ConvID: convID, Cwd: "/tmp/same-new",
		Harness: "codex", Status: "running", HarnessBuiltinMode: "workspace-write",
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
		Harness: "opencode", Status: "idle", HarnessBuiltinMode: "off", AskUserQuestionTimeout: "5m",
		RemoteControl: true, AutoMemory: true,
	}))
	require.NoError(t, SetSessionRemoteControl("server-authoritative-session", true))
	require.NoError(t, SetSessionAutoMemory("server-authoritative-session", true))

	conversation, err := ConversationResumeProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, conversation)
	require.NotNil(t, conversation.FallbackRelaunch)
	require.NotNil(t, conversation.FallbackRelaunch.HarnessBuiltinMode)
	assert.Equal(t, "off", *conversation.FallbackRelaunch.HarnessBuiltinMode)
	require.NotNil(t, conversation.FallbackRelaunch.ApprovalPolicy)
	assert.Empty(t, *conversation.FallbackRelaunch.ApprovalPolicy)

	agent, err := AgentRelaunchProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, agent)
	require.NotNil(t, agent.HarnessBuiltinMode)
	assert.Equal(t, "off", *agent.HarnessBuiltinMode)
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
		Harness: DefaultHarness, Status: "idle", HarnessBuiltinMode: "on", ApprovalPolicy: "auto",
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
	require.NotNil(t, profile.HarnessBuiltinMode)
	assert.Equal(t, "on", *profile.HarnessBuiltinMode)
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
		VALUES (?, '{"version":99}', 1767225600000000000)`, convID)
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
