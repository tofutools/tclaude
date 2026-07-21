package agentd

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestAgentRecoverySweep_ResumesSameConversationExactlyOnce(t *testing.T) {
	setupTestDB(t)
	previousResolvable := recoveryResumeTargetResolvable
	recoveryResumeTargetResolvable = func(string) bool { return true }
	t.Cleanup(func() { recoveryResumeTargetResolvable = previousResolvable })
	recorder := installRecordingResumeSpawner(t)
	conv := "codex-auto-recover-conv"
	agentID, _, err := db.EnsureAgentForConv(conv, "test")
	require.NoError(t, err)
	row := saveResumeSession(t, conv, t.TempDir(), harness.CodexName)
	row.TmuxSession = "dead-codex-pane"
	row.Status = "exited"
	require.NoError(t, db.SaveSession(row))
	const generation = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	require.NoError(t, db.SetSessionExitLaunchGeneration(row.ID, generation))
	require.NoError(t, db.SetSessionExitLaunchBinding(row.ID, generation, strings.Repeat("b", 64), "%42"))
	require.NoError(t, db.MarkSessionExitLaunchReleasing(row.ID, generation))
	require.NoError(t, db.MarkSessionExitLaunchReleased(row.ID, generation))
	now := time.Now().UTC()
	code := 1
	_, err = db.RecordAgentExitObservation(db.AgentExitObservation{At: now,
		SessionID: row.ID, Observer: db.AgentExitObserverReaper,
		CauseKind: db.AgentExitCauseNormal, ExitCode: &code,
		ExpectedGeneration: generation, ObservedState: "exited"})
	require.NoError(t, err)

	runAgentRecoverySweep(now.Add(5 * time.Second))
	runAgentRecoverySweep(now.Add(6 * time.Second))
	assert.Equal(t, 1, recorder.resumeCalls)
	assert.Equal(t, conv, recorder.convID)
	assert.Equal(t, harness.CodexName, recorder.harness)
	current, err := db.GetAgent(agentID)
	require.NoError(t, err)
	require.NotNil(t, current)
	assert.Equal(t, conv, current.CurrentConvID, "automatic resume preserves the stable actor and conversation")
	recovery, err := db.AgentRecoveryForAgent(agentID)
	require.NoError(t, err)
	require.NotNil(t, recovery)
	assert.Equal(t, db.AgentRecoveryStatusRestarting, recovery.Status)
}

func TestAgentRecoverySweep_ConfirmsSameSessionRowWithNewGeneration(t *testing.T) {
	setupTestDB(t)
	previousAlive := recoverySessionAlive
	recoverySessionAlive = func(tmuxSession string) bool { return tmuxSession == "same-row-successor-pane" }
	t.Cleanup(func() { recoverySessionAlive = previousAlive })
	const (
		conv                  = "codex-same-row-successor-conv"
		predecessorGeneration = "89898989898989898989898989898989"
		successorGeneration   = "90909090909090909090909090909090"
	)
	agentID, _, err := db.EnsureAgentForConv(conv, "test")
	require.NoError(t, err)
	row := saveResumeSession(t, conv, t.TempDir(), harness.CodexName)
	require.NoError(t, db.SetSessionExitLaunchGeneration(row.ID, predecessorGeneration))
	require.NoError(t, db.SetSessionExitLaunchBinding(row.ID, predecessorGeneration, strings.Repeat("8", 64), "%49"))
	require.NoError(t, db.MarkSessionExitLaunchReleasing(row.ID, predecessorGeneration))
	require.NoError(t, db.MarkSessionExitLaunchReleased(row.ID, predecessorGeneration))
	code := 1
	_, err = db.RecordAgentExitObservation(db.AgentExitObservation{At: time.Now().UTC().Add(-10 * time.Second),
		SessionID: row.ID, Observer: db.AgentExitObserverReaper,
		CauseKind: db.AgentExitCauseNormal, ExitCode: &code,
		ExpectedGeneration: predecessorGeneration, ObservedState: "exited"})
	require.NoError(t, err)
	recovery, err := db.AgentRecoveryForAgent(agentID)
	require.NoError(t, err)
	require.NotNil(t, recovery)
	claim, err := db.ClaimAgentRecovery(agentID, predecessorGeneration, time.Now().UTC())
	require.NoError(t, err)
	require.NotNil(t, claim)

	row.TmuxSession = "same-row-successor-pane"
	row.Status = "working"
	require.NoError(t, db.SaveSession(row))
	require.NoError(t, db.SetSessionExitLaunchGeneration(row.ID, successorGeneration))
	runAgentRecoverySweep(time.Now().UTC().Add(-2 * time.Minute))
	confirmed, err := db.AgentRecoveryForAgent(agentID)
	require.NoError(t, err)
	require.NotNil(t, confirmed)
	assert.Equal(t, db.AgentRecoveryStatusRecovered, confirmed.Status)
	assert.Equal(t, row.ID, confirmed.SuccessorSessionID)
	assert.Equal(t, successorGeneration, confirmed.SuccessorGeneration)
	assert.True(t, confirmed.RecoveredAt.After(row.UpdatedAt),
		"confirmation time must reflect the transition, not a stale ticker timestamp")
	assert.True(t, recoveryStatusVisible(*confirmed, row.UpdatedAt, true, time.Now()),
		"a hook before actual confirmation must not hide the new recovered badge")
}

func TestManualResumeCancelsPendingAutomaticRetry(t *testing.T) {
	setupTestDB(t)
	recorder := installRecordingResumeSpawner(t)
	conv := "codex-manual-wins-conv"
	_, _, err := db.EnsureAgentForConv(conv, "test")
	require.NoError(t, err)
	row := saveResumeSession(t, conv, t.TempDir(), harness.CodexName)
	const generation = "cccccccccccccccccccccccccccccccc"
	require.NoError(t, db.SetSessionExitLaunchGeneration(row.ID, generation))
	require.NoError(t, db.SetSessionExitLaunchBinding(row.ID, generation, strings.Repeat("d", 64), "%43"))
	require.NoError(t, db.MarkSessionExitLaunchReleasing(row.ID, generation))
	require.NoError(t, db.MarkSessionExitLaunchReleased(row.ID, generation))
	now := time.Now().UTC()
	code := 1
	_, err = db.RecordAgentExitObservation(db.AgentExitObservation{At: now,
		SessionID: row.ID, Observer: db.AgentExitObserverReaper,
		CauseKind: db.AgentExitCauseNormal, ExitCode: &code,
		ExpectedGeneration: generation, ObservedState: "exited"})
	require.NoError(t, err)

	result := resumeOneConvLocked(conv, false, false)
	assert.Equal(t, "resumed", result.Action)
	runAgentRecoverySweep(now.Add(10 * time.Second))
	assert.Equal(t, 1, recorder.resumeCalls, "manual and automatic recovery share one launch mutex/winner")
	recovery, err := db.AgentRecoveryForConv(conv)
	require.NoError(t, err)
	require.NotNil(t, recovery)
	assert.Equal(t, db.AgentRecoveryStatusRestarting, recovery.Status,
		"manual retry owns the durable attempt until its successor is confirmed")
}

func TestOfflineStopCancelsPendingAutomaticRetry(t *testing.T) {
	setupTestDB(t)
	const conv = "codex-offline-stop-conv"
	_, _, err := db.EnsureAgentForConv(conv, "test")
	require.NoError(t, err)
	row := saveResumeSession(t, conv, t.TempDir(), harness.CodexName)
	const generation = "12121212121212121212121212121212"
	require.NoError(t, db.SetSessionExitLaunchGeneration(row.ID, generation))
	require.NoError(t, db.SetSessionExitLaunchBinding(row.ID, generation, strings.Repeat("1", 64), "%45"))
	require.NoError(t, db.MarkSessionExitLaunchReleasing(row.ID, generation))
	require.NoError(t, db.MarkSessionExitLaunchReleased(row.ID, generation))
	code := 1
	_, err = db.RecordAgentExitObservation(db.AgentExitObservation{At: time.Now().UTC(),
		SessionID: row.ID, Observer: db.AgentExitObserverReaper,
		CauseKind: db.AgentExitCauseNormal, ExitCode: &code,
		ExpectedGeneration: generation, ObservedState: "exited"})
	require.NoError(t, err)

	res := stopOneConvWithIntent(conv, false, db.AgentExitActionStop, "")
	assert.Equal(t, "skipped:already_offline", res.Action)
	recovery, err := db.AgentRecoveryForConv(conv)
	require.NoError(t, err)
	require.NotNil(t, recovery)
	assert.Equal(t, db.AgentRecoveryStatusCancelled, recovery.Status)
	runAgentRecoverySweep(time.Now().UTC().Add(time.Hour))
	recovery, err = db.AgentRecoveryForConv(conv)
	require.NoError(t, err)
	require.NotNil(t, recovery)
	assert.Equal(t, db.AgentRecoveryStatusCancelled, recovery.Status)
}

func TestProductionRetireInvalidatesClaimedRecoveryBeforeWaitingForLaunchLock(t *testing.T) {
	setupTestDB(t)
	const conv = "codex-retire-claim-conv"
	agentID, _, err := db.EnsureAgentForConv(conv, "test")
	require.NoError(t, err)
	row := saveResumeSession(t, conv, t.TempDir(), harness.CodexName)
	const generation = "34343434343434343434343434343434"
	require.NoError(t, db.SetSessionExitLaunchGeneration(row.ID, generation))
	require.NoError(t, db.SetSessionExitLaunchBinding(row.ID, generation, strings.Repeat("3", 64), "%47"))
	require.NoError(t, db.MarkSessionExitLaunchReleasing(row.ID, generation))
	require.NoError(t, db.MarkSessionExitLaunchReleased(row.ID, generation))
	code := 1
	_, err = db.RecordAgentExitObservation(db.AgentExitObservation{At: time.Now().UTC(),
		SessionID: row.ID, Observer: db.AgentExitObserverReaper,
		CauseKind: db.AgentExitCauseNormal, ExitCode: &code,
		ExpectedGeneration: generation, ObservedState: "exited"})
	require.NoError(t, err)
	recovery, err := db.AgentRecoveryForAgent(agentID)
	require.NoError(t, err)
	require.NotNil(t, recovery)
	claim, err := db.ClaimAgentRecovery(agentID, generation, recovery.NextAttemptAt)
	require.NoError(t, err)
	require.NotNil(t, claim)

	outcome, _, err := retireAgentConv(conv, "human", "done")
	require.NoError(t, err)
	require.True(t, outcome.Retired)
	current, err := db.AgentRecoveryClaimCurrent(*claim)
	require.NoError(t, err)
	assert.False(t, current)
}

func TestManualRecoverySuccessorEarnsHealthyReset(t *testing.T) {
	setupTestDB(t)
	previousAlive := recoverySessionAlive
	recoverySessionAlive = func(tmuxSession string) bool { return tmuxSession == "manual-successor-pane" }
	t.Cleanup(func() { recoverySessionAlive = previousAlive })
	installRecordingResumeSpawner(t)
	const (
		conv                  = "codex-manual-healthy-conv"
		predecessorGeneration = "56565656565656565656565656565656"
		autoGeneration        = "67676767676767676767676767676767"
		successorGeneration   = "78787878787878787878787878787878"
	)
	agentID, _, err := db.EnsureAgentForConv(conv, "test")
	require.NoError(t, err)
	predecessor := saveResumeSession(t, conv, t.TempDir(), harness.CodexName)
	require.NoError(t, db.SetSessionExitLaunchGeneration(predecessor.ID, predecessorGeneration))
	require.NoError(t, db.SetSessionExitLaunchBinding(predecessor.ID, predecessorGeneration, strings.Repeat("5", 64), "%46"))
	require.NoError(t, db.MarkSessionExitLaunchReleasing(predecessor.ID, predecessorGeneration))
	require.NoError(t, db.MarkSessionExitLaunchReleased(predecessor.ID, predecessorGeneration))
	code := 1
	crashedAt := time.Now().UTC()
	_, err = db.RecordAgentExitObservation(db.AgentExitObservation{At: crashedAt,
		SessionID: predecessor.ID, Observer: db.AgentExitObserverReaper,
		CauseKind: db.AgentExitCauseNormal, ExitCode: &code,
		ExpectedGeneration: predecessorGeneration, ObservedState: "exited"})
	require.NoError(t, err)
	prior, err := db.AgentRecoveryForAgent(agentID)
	require.NoError(t, err)
	require.NotNil(t, prior)
	claim, err := db.ClaimAgentRecovery(agentID, predecessorGeneration, prior.NextAttemptAt)
	require.NoError(t, err)
	require.NotNil(t, claim)
	autoSuccessor := *predecessor
	autoSuccessor.ID += "-auto-successor"
	autoSuccessor.TmuxSession = "dead-auto-successor-pane"
	require.NoError(t, db.SaveSession(&autoSuccessor))
	require.NoError(t, db.SetSessionExitLaunchGeneration(autoSuccessor.ID, autoGeneration))
	changed, err := db.ConfirmAgentRecovery(*claim, autoSuccessor.ID, autoGeneration, time.Now().UTC())
	require.NoError(t, err)
	require.True(t, changed)

	result := resumeOneConvLocked(conv, false, false)
	require.Equal(t, "resumed", result.Action)

	successor := autoSuccessor
	successor.ID += "-manual-successor"
	successor.TmuxSession = "manual-successor-pane"
	require.NoError(t, db.SaveSession(&successor))
	require.NoError(t, db.SetSessionExitLaunchGeneration(successor.ID, successorGeneration))
	confirmedAt := time.Now().UTC()
	runAgentRecoverySweep(confirmedAt)
	recovery, err := db.AgentRecoveryForAgent(agentID)
	require.NoError(t, err)
	require.NotNil(t, recovery)
	require.Equal(t, db.AgentRecoveryStatusRecovered, recovery.Status)

	runAgentRecoverySweep(confirmedAt.Add(db.AgentRecoveryHealthyReset + time.Second))
	reset, err := db.AgentRecoveryForAgent(agentID)
	require.NoError(t, err)
	assert.Nil(t, reset)
}

func TestAgentRecoverySweep_HealthyResetRequiresExactLiveSuccessor(t *testing.T) {
	setupTestDB(t)
	previousAlive := recoverySessionAlive
	alive := false
	recoverySessionAlive = func(tmuxSession string) bool {
		return alive && tmuxSession == "live-recovered-pane"
	}
	t.Cleanup(func() { recoverySessionAlive = previousAlive })

	const (
		conv                  = "codex-healthy-reset-conv"
		predecessorGeneration = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
		successorGeneration   = "ffffffffffffffffffffffffffffffff"
	)
	agentID, _, err := db.EnsureAgentForConv(conv, "test")
	require.NoError(t, err)
	predecessor := saveResumeSession(t, conv, t.TempDir(), harness.CodexName)
	require.NoError(t, db.SetSessionExitLaunchGeneration(predecessor.ID, predecessorGeneration))
	require.NoError(t, db.SetSessionExitLaunchBinding(predecessor.ID, predecessorGeneration, strings.Repeat("e", 64), "%44"))
	require.NoError(t, db.MarkSessionExitLaunchReleasing(predecessor.ID, predecessorGeneration))
	require.NoError(t, db.MarkSessionExitLaunchReleased(predecessor.ID, predecessorGeneration))
	crashedAt := time.Now().UTC().Add(-time.Hour)
	code := 1
	_, err = db.RecordAgentExitObservation(db.AgentExitObservation{At: crashedAt,
		SessionID: predecessor.ID, Observer: db.AgentExitObserverReaper,
		CauseKind: db.AgentExitCauseNormal, ExitCode: &code,
		ExpectedGeneration: predecessorGeneration, ObservedState: "exited"})
	require.NoError(t, err)
	recovery, err := db.AgentRecoveryForAgent(agentID)
	require.NoError(t, err)
	require.NotNil(t, recovery)
	claim, err := db.ClaimAgentRecovery(agentID, predecessorGeneration, recovery.NextAttemptAt)
	require.NoError(t, err)
	require.NotNil(t, claim)

	successor := *predecessor
	successor.ID += "-successor"
	successor.Cwd = t.TempDir()
	successor.TmuxSession = "live-recovered-pane"
	require.NoError(t, db.SaveSession(&successor))
	require.NoError(t, db.SetSessionExitLaunchGeneration(successor.ID, successorGeneration))
	healthySince := time.Now().UTC().Add(-db.AgentRecoveryHealthyReset - time.Second)
	changed, err := db.ConfirmAgentRecovery(*claim, successor.ID, successorGeneration, healthySince)
	require.NoError(t, err)
	require.True(t, changed)

	runAgentRecoverySweep(time.Now().UTC())
	stillTracked, err := db.AgentRecoveryForAgent(agentID)
	require.NoError(t, err)
	require.NotNil(t, stillTracked, "elapsed time alone must not reset a dead successor")

	alive = true
	runAgentRecoverySweep(time.Now().UTC())
	reset, err := db.AgentRecoveryForAgent(agentID)
	require.NoError(t, err)
	assert.Nil(t, reset, "the exact continuously healthy successor resets the crash streak")
}
