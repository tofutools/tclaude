package db

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedRecoverableCodexExit(t *testing.T, sessionID, conv, generation string, at time.Time, code int) string {
	t.Helper()
	agentID, _, err := EnsureAgentForConv(conv, "test")
	require.NoError(t, err)
	require.NoError(t, SaveSession(&SessionRow{ID: sessionID, TmuxSession: "tmux-" + sessionID,
		Cwd: t.TempDir(), ConvID: conv, Status: "working", Harness: "codex",
		ResumeProvenance: `{"version":1}`}))
	require.NoError(t, SetSessionExitLaunchGeneration(sessionID, generation))
	require.NoError(t, SetSessionExitLaunchBinding(sessionID, generation, strings.Repeat("a", 64), "%7"))
	require.NoError(t, MarkSessionExitLaunchReleasing(sessionID, generation))
	require.NoError(t, MarkSessionExitLaunchReleased(sessionID, generation))
	_, err = RecordAgentExitObservation(AgentExitObservation{At: at, SessionID: sessionID,
		Observer: AgentExitObserverReaper, CauseKind: AgentExitCauseNormal,
		ExitCode: &code, ExpectedGeneration: generation, ObservedState: "exited"})
	require.NoError(t, err)
	return agentID
}

func TestAgentRecovery_EligibleExitCreatesOneDurableCandidateAndLease(t *testing.T) {
	setupTestDB(t)
	now := time.Now().UTC()
	agentID := seedRecoverableCodexExit(t, "recover-1", "conv-recover-1",
		"11111111111111111111111111111111", now, 1)
	r, err := AgentRecoveryForAgent(agentID)
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.Equal(t, AgentRecoveryStatusCrashed, r.Status)
	assert.Equal(t, 1, r.ConsecutiveCrashes)
	assert.Equal(t, 5, r.BackoffSeconds)
	assert.WithinDuration(t, now.Add(5*time.Second), r.NextAttemptAt, time.Millisecond)

	first, err := ClaimAgentRecovery(agentID, r.PredecessorGeneration, now.Add(5*time.Second))
	require.NoError(t, err)
	require.NotNil(t, first)
	second, err := ClaimAgentRecovery(agentID, r.PredecessorGeneration, now.Add(5*time.Second))
	require.NoError(t, err)
	assert.Nil(t, second, "the durable launch CAS has exactly one winner")
}

func TestAgentRecovery_AuditIsCorrelatedAndBounded(t *testing.T) {
	setupTestDB(t)
	now := time.Now().UTC()
	agentID := seedRecoverableCodexExit(t, "recover-audit", "conv-recover-audit",
		"99999999999999999999999999999999", now, 17)
	recovery, err := AgentRecoveryForAgent(agentID)
	require.NoError(t, err)
	require.NotNil(t, recovery)

	for _, verb := range []string{AuditVerbAgentRecoveryEligible, AuditVerbAgentRecoveryScheduled} {
		rows, err := ListAuditLog(AuditLogFilter{Verb: verb})
		require.NoError(t, err)
		require.Len(t, rows, 1)
		row := rows[0]
		assert.Equal(t, recovery.ExitEventID, row.RelatedEventID)
		assert.Equal(t, recovery.PredecessorSessionID, row.SessionID)
		assert.Equal(t, agentID, row.TargetAgent)
		assert.Equal(t, recovery.ConvID, row.TargetConv)
		assert.Less(t, len(row.Detail), 1024)
		assert.NotContains(t, row.Detail, recovery.ConvID)
		assert.NotContains(t, row.Detail, "resume_provenance")
		assert.NotContains(t, row.Detail, t.TempDir())
	}
}

func TestAgentRecovery_ReaperThenCallbackPromotesSameGenerationOnce(t *testing.T) {
	setupTestDB(t)
	now := time.Now().UTC()
	const sessionID = "recover-enrich"
	const conv = "conv-recover-enrich"
	const generation = "88888888888888888888888888888888"
	agentID, _, err := EnsureAgentForConv(conv, "test")
	require.NoError(t, err)
	require.NoError(t, SaveSession(&SessionRow{ID: sessionID, TmuxSession: "tmux-enrich",
		Cwd: t.TempDir(), ConvID: conv, Status: "working", Harness: "codex",
		ResumeProvenance: `{"version":1}`}))
	require.NoError(t, SetSessionExitLaunchGeneration(sessionID, generation))
	require.NoError(t, SetSessionExitLaunchBinding(sessionID, generation, strings.Repeat("e", 64), "%9"))
	require.NoError(t, MarkSessionExitLaunchReleasing(sessionID, generation))
	require.NoError(t, MarkSessionExitLaunchReleased(sessionID, generation))
	_, err = RecordAgentExitObservation(AgentExitObservation{At: now, SessionID: sessionID,
		Observer: AgentExitObserverReaper, CauseKind: AgentExitCauseUnknown,
		ExpectedGeneration: generation, ObservedState: "exited"})
	require.NoError(t, err)
	r, err := AgentRecoveryForAgent(agentID)
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.Equal(t, AgentRecoveryStatusSuppressed, r.Status)
	code := 1
	_, err = RecordAgentExitObservation(AgentExitObservation{At: now.Add(time.Millisecond), SessionID: sessionID,
		Observer: AgentExitObserverHook, CauseKind: AgentExitCauseNormal, ExitCode: &code,
		ExpectedGeneration: generation, ObservedState: "exited"})
	require.NoError(t, err)
	r, err = AgentRecoveryForAgent(agentID)
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.Equal(t, AgentRecoveryStatusCrashed, r.Status)
	assert.Equal(t, 1, r.ConsecutiveCrashes, "enrichment is not a second crash")
	claim, err := ClaimAgentRecovery(agentID, generation, r.NextAttemptAt)
	require.NoError(t, err)
	require.NotNil(t, claim)
	loser, err := ClaimAgentRecovery(agentID, generation, r.NextAttemptAt)
	require.NoError(t, err)
	assert.Nil(t, loser)
}

func TestAgentRecovery_DelayedPredecessorCannotOverwriteNewerEpisode(t *testing.T) {
	setupTestDB(t)
	now := time.Now().UTC()
	agentID := seedRecoverableCodexExit(t, "recover-old", "conv-recover-order",
		"10101010101010101010101010101010", now, 1)
	seedRecoverableCodexExit(t, "recover-new", "conv-recover-order",
		"20202020202020202020202020202020", now.Add(time.Second), 2)
	newer, err := AgentRecoveryForAgent(agentID)
	require.NoError(t, err)
	require.NotNil(t, newer)
	require.Equal(t, "20202020202020202020202020202020", newer.PredecessorGeneration)

	code := 1
	_, err = RecordAgentExitObservation(AgentExitObservation{At: now.Add(2 * time.Second),
		SessionID: "recover-old", Observer: AgentExitObserverHook,
		CauseKind: AgentExitCauseNormal, ExitCode: &code,
		ExpectedGeneration: "10101010101010101010101010101010", ObservedState: "exited"})
	require.NoError(t, err)
	after, err := AgentRecoveryForAgent(agentID)
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.Equal(t, newer.PredecessorGeneration, after.PredecessorGeneration)
	assert.Equal(t, newer.NextAttemptAt, after.NextAttemptAt)
	assert.Equal(t, newer.Status, after.Status)
}

func TestAgentRecovery_ReaperTimestampOnPredecessorCannotOverwriteNewerEpisode(t *testing.T) {
	setupTestDB(t)
	now := time.Now().UTC()
	const (
		conv   = "conv-recover-reaper-order"
		oldGen = "41414141414141414141414141414141"
		newGen = "42424242424242424242424242424242"
		oldID  = "recover-reaper-old"
		newID  = "recover-reaper-new"
	)
	agentID, _, err := EnsureAgentForConv(conv, "test")
	require.NoError(t, err)
	require.NoError(t, SaveSession(&SessionRow{ID: oldID, TmuxSession: "tmux-old",
		Cwd: t.TempDir(), ConvID: conv, Status: "working", Harness: "codex",
		ResumeProvenance: `{"version":1}`}))
	require.NoError(t, SetSessionExitLaunchGeneration(oldID, oldGen))
	require.NoError(t, SetSessionExitLaunchBinding(oldID, oldGen, strings.Repeat("4", 64), "%48"))
	require.NoError(t, MarkSessionExitLaunchReleasing(oldID, oldGen))
	require.NoError(t, MarkSessionExitLaunchReleased(oldID, oldGen))
	observedOld, err := LoadSession(oldID)
	require.NoError(t, err)
	oldCode := 1
	_, err = RecordAgentExitObservation(AgentExitObservation{At: now, SessionID: oldID,
		Observer: AgentExitObserverHook, CauseKind: AgentExitCauseNormal, ExitCode: &oldCode,
		ExpectedGeneration: oldGen, ObservedState: "exited"})
	require.NoError(t, err)

	seedRecoverableCodexExit(t, newID, conv, newGen, now.Add(time.Second), 2)
	newer, err := AgentRecoveryForAgent(agentID)
	require.NoError(t, err)
	require.NotNil(t, newer)
	require.Equal(t, newGen, newer.PredecessorGeneration)

	ok, _, err := MarkSessionExitedAndRecordObservationIfUnchanged(oldID,
		observedOld.Status, observedOld.UpdatedAt, "unexpected", AgentExitObservation{
			At: now.Add(2 * time.Second), SessionID: oldID, Observer: AgentExitObserverReaper,
			CauseKind: AgentExitCauseNormal, ExitCode: &oldCode, ExpectedGeneration: oldGen,
		})
	require.NoError(t, err)
	require.True(t, ok, "exercise the production reaper mutation that bumps predecessor updated_at")
	after, err := AgentRecoveryForAgent(agentID)
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.Equal(t, newGen, after.PredecessorGeneration)
	assert.Equal(t, newer.NextAttemptAt, after.NextAttemptAt)
}

func TestAgentRecovery_StaleGenerationCancelCannotCancelNewerEpisode(t *testing.T) {
	setupTestDB(t)
	now := time.Now().UTC()
	agentID := seedRecoverableCodexExit(t, "recover-cas-old", "conv-recover-cas",
		"30303030303030303030303030303030", now, 1)
	old, err := AgentRecoveryForAgent(agentID)
	require.NoError(t, err)
	require.NotNil(t, old)
	seedRecoverableCodexExit(t, "recover-cas-new", "conv-recover-cas",
		"40404040404040404040404040404040", now.Add(time.Second), 1)

	changed, err := CancelAgentRecoveryGeneration(*old, "stale_sweep", now.Add(2*time.Second))
	require.NoError(t, err)
	assert.False(t, changed)
	current, err := AgentRecoveryForAgent(agentID)
	require.NoError(t, err)
	require.NotNil(t, current)
	assert.Equal(t, "40404040404040404040404040404040", current.PredecessorGeneration)
	assert.NotEqual(t, AgentRecoveryStatusCancelled, current.Status)
}

func TestAgentRecovery_BackoffScheduleIsExactAndUnbounded(t *testing.T) {
	want := []time.Duration{5, 10, 20, 40, 80, 160, 300, 600, 600, 600}
	for i, seconds := range want {
		assert.Equal(t, seconds*time.Second, AgentRecoveryBackoff(i+1), "attempt %d", i+1)
	}
}

func TestAgentRecovery_CleanExitAndLifecycleIntentFailClosed(t *testing.T) {
	setupTestDB(t)
	now := time.Now().UTC()
	agentID := seedRecoverableCodexExit(t, "recover-clean", "conv-recover-clean",
		"22222222222222222222222222222222", now, 0)
	r, err := AgentRecoveryForAgent(agentID)
	require.NoError(t, err)
	assert.Nil(t, r, "clean /quit-style exit is never recovered")

	const generation = "33333333333333333333333333333333"
	require.NoError(t, SetSessionExitLaunchGeneration("recover-clean", generation))
	require.NoError(t, SetSessionExitLaunchBinding("recover-clean", generation, strings.Repeat("b", 64), "%8"))
	require.NoError(t, MarkSessionExitLaunchReleasing("recover-clean", generation))
	require.NoError(t, MarkSessionExitLaunchReleased("recover-clean", generation))
	_, err = SetSessionExitIntent("recover-clean", AgentExitActionStop, "", now)
	require.NoError(t, err)
	code := 1
	_, err = RecordAgentExitObservation(AgentExitObservation{At: now, SessionID: "recover-clean",
		Observer: AgentExitObserverReaper, CauseKind: AgentExitCauseNormal, ExitCode: &code,
		ExpectedGeneration: generation, ObservedState: "exited"})
	require.NoError(t, err)
	r, err = AgentRecoveryForAgent(agentID)
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.Equal(t, AgentRecoveryStatusSuppressed, r.Status)
	assert.Equal(t, "lifecycle_intent", r.ReasonCode)
}

func TestAgentRecovery_RepeatedLaunchFailuresPersistExactCappedSchedule(t *testing.T) {
	setupTestDB(t)
	now := time.Now().UTC()
	agentID := seedRecoverableCodexExit(t, "recover-loop", "conv-recover-loop",
		"44444444444444444444444444444444", now, 1)
	want := []time.Duration{5, 10, 20, 40, 80, 160, 300, 600, 600}
	for attempt, delay := range want {
		r, err := AgentRecoveryForAgent(agentID)
		require.NoError(t, err)
		require.NotNil(t, r)
		assert.Equal(t, delay*time.Second, time.Duration(r.BackoffSeconds)*time.Second, "attempt %d", attempt+1)
		if attempt == len(want)-1 {
			break
		}
		due := r.NextAttemptAt
		claim, err := ClaimAgentRecovery(agentID, r.PredecessorGeneration, due)
		require.NoError(t, err)
		require.NotNil(t, claim)
		changed, err := FailAgentRecoveryLaunch(*claim, due, "launch_failed")
		require.NoError(t, err)
		require.True(t, changed)
	}
	// Closing and reopening the singleton models an agentd restart: the exact
	// due timestamp/backoff remains database authority, not an in-memory timer.
	before, err := AgentRecoveryForAgent(agentID)
	require.NoError(t, err)
	ResetForTest()
	after, err := AgentRecoveryForAgent(agentID)
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.Equal(t, before.NextAttemptAt, after.NextAttemptAt)
	assert.Equal(t, before.BackoffSeconds, after.BackoffSeconds)
}

func TestAgentRecovery_HealthyRuntimeResetsLaterCrashToInitialDelay(t *testing.T) {
	setupTestDB(t)
	now := time.Now().UTC()
	agentID := seedRecoverableCodexExit(t, "recover-healthy-1", "conv-recover-healthy",
		"55555555555555555555555555555555", now.Add(-time.Hour), 1)
	database, err := Open()
	require.NoError(t, err)
	_, err = database.Exec(`UPDATE agent_recovery SET status=?, consecutive_crashes=7,
		healthy_since=? WHERE agent_id=?`, AgentRecoveryStatusRecovered,
		now.Add(-AgentRecoveryHealthyReset-time.Second).Format(time.RFC3339Nano), agentID)
	require.NoError(t, err)
	beforeReset, err := AgentRecoveryForAgent(agentID)
	require.NoError(t, err)
	require.NotNil(t, beforeReset)
	reset, err := ResetHealthyAgentRecovery(*beforeReset, now)
	require.NoError(t, err)
	require.True(t, reset)

	seedRecoverableCodexExit(t, "recover-healthy-2", "conv-recover-healthy",
		"66666666666666666666666666666666", now, 1)
	r, err := AgentRecoveryForAgent(agentID)
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.Equal(t, 1, r.ConsecutiveCrashes)
	assert.Equal(t, 5, r.BackoffSeconds)
}

func TestRetireAgentCancelsPendingRecoveryAtomically(t *testing.T) {
	setupTestDB(t)
	agentID := seedRecoverableCodexExit(t, "recover-retire", "conv-recover-retire",
		"77777777777777777777777777777777", time.Now().UTC(), 1)
	retired, err := RetireAgentByID(agentID, "human", "done")
	require.NoError(t, err)
	require.True(t, retired)
	r, err := AgentRecoveryForAgent(agentID)
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.Equal(t, AgentRecoveryStatusCancelled, r.Status)
}

func TestRetireInvalidatesAlreadyClaimedRecoveryBeforeSpawn(t *testing.T) {
	setupTestDB(t)
	now := time.Now().UTC()
	agentID := seedRecoverableCodexExit(t, "recover-retire-claim", "conv-recover-retire-claim",
		"abababababababababababababababab", now, 1)
	r, err := AgentRecoveryForAgent(agentID)
	require.NoError(t, err)
	require.NotNil(t, r)
	claim, err := ClaimAgentRecovery(agentID, r.PredecessorGeneration, r.NextAttemptAt)
	require.NoError(t, err)
	require.NotNil(t, claim)
	current, err := AgentRecoveryClaimCurrent(*claim)
	require.NoError(t, err)
	require.True(t, current)

	retired, err := RetireAgentByID(agentID, "human", "done")
	require.NoError(t, err)
	require.True(t, retired)
	current, err = AgentRecoveryClaimCurrent(*claim)
	require.NoError(t, err)
	assert.False(t, current)
}
