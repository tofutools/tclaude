package agentd

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	platformexec "github.com/tofutools/tclaude/pkg/claude/platform/execution"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestResumeReconcile_PostReleaseCancellationUsesExactStop(t *testing.T) {
	w := testharness.New(t)
	prevTmux := clcommon.Default
	clcommon.Default = w.Tmux
	t.Cleanup(func() { clcommon.Default = prevTmux })
	const (
		convID      = "resume-cancel-exact-conv"
		sessionID   = "resume-cancel-exact-session"
		tmuxSession = "resume-cancel-exact-tmux"
		generation  = "83838383838383838383838383838383"
	)
	require.NoError(t, db.SaveSession(&db.SessionRow{ID: sessionID, ConvID: convID,
		TmuxSession: tmuxSession, Status: "working", CreatedAt: time.Now()}))
	require.NoError(t, db.SetSessionExitLaunchGeneration(sessionID, generation))
	w.Tmux.MarkAlive(tmuxSession)
	opID := platformexec.NewOperationID()
	secret := []byte("post-release-cancel")
	eID := platformexec.ID(generation)
	require.NoError(t, db.CreateResumeOperation(db.ResumeOperationRow{
		ID: opID, Kind: "manual_resume", ConvID: convID,
		Attempt: platformexec.AttemptRef{ExecutionID: eID}, ClaimHash: db.ResumeClaimHash(secret),
		State: platformexec.ResumeRequested, LaunchPhase: "requested", Revision: 1,
	}))
	require.NoError(t, db.TransitionResumeOperation(opID, 1, platformexec.ResumeAccepted, "accepted", ""))
	claimed, err := db.ClaimResumeOperation(opID, eID, secret, convID, sessionID, 999999, "dead-process")
	require.NoError(t, err)
	require.True(t, claimed)
	gate := filepath.Join(config.DataDir(), "exit-launch", "barrier-reconcile-test")
	registered, err := db.RegisterResumeLaunch(opID, eID, convID, sessionID, tmuxSession, "%83", gate, 999999, "dead-process")
	require.NoError(t, err)
	require.True(t, registered)
	granted, err := db.GrantResumeRelease(opID, eID, sessionID, tmuxSession, "%83", gate)
	require.NoError(t, err)
	require.True(t, granted)
	started, err := db.MarkResumeReleased(opID, eID, sessionID, tmuxSession, "%83")
	require.NoError(t, err)
	require.True(t, started)

	result := managedExecutionRuntime.stop(convID, true, db.AgentExitActionForceStop, "", stopWaitForExit(0))
	assert.Equal(t, platformexec.StopCompleted, result.stop.State)
	assert.False(t, w.Tmux.IsAlive(tmuxSession))
	op, err := db.GetResumeOperation(opID)
	require.NoError(t, err)
	require.NotNil(t, op)
	assert.Equal(t, platformexec.ResumeCancelled, op.State)
	assert.Equal(t, "cleanup_proven", op.LaunchPhase)
}

func TestResumeReconcile_UnknownNoEffectRevokesThenFails(t *testing.T) {
	setupTestDB(t)
	opID := platformexec.NewOperationID()
	eID := platformexec.ID("84848484848484848484848484848484")
	require.NoError(t, db.CreateResumeOperation(db.ResumeOperationRow{
		ID: opID, Kind: "manual_resume", ConvID: "resume-unknown-no-effect",
		Attempt: platformexec.AttemptRef{ExecutionID: eID}, ClaimHash: db.ResumeClaimHash([]byte("unused")),
		State: platformexec.ResumeRequested, LaunchPhase: "requested", Revision: 1,
	}))
	require.NoError(t, db.TransitionResumeOperation(opID, 1, platformexec.ResumeAccepted, "accepted", ""))
	require.NoError(t, db.TransitionResumeOperation(opID, 2, platformexec.ResumeUnknown, "dispatch_unknown", "fork outcome unknown"))
	reconcileResumeOperations(time.Now(), false)
	op, err := db.GetResumeOperation(opID)
	require.NoError(t, err)
	require.NotNil(t, op)
	assert.Equal(t, platformexec.ResumeFailed, op.State)
	assert.Equal(t, "cleanup_proven", op.LaunchPhase)
}

func TestResumeReconcile_CancellationRetriesChildRevisionRace(t *testing.T) {
	setupTestDB(t)
	const convID = "resume-cancellation-revision-race"
	opID := platformexec.NewOperationID()
	eID := platformexec.ID("85858585858585858585858585858585")
	secret := []byte("claim-during-cancellation")
	require.NoError(t, db.CreateResumeOperation(db.ResumeOperationRow{
		ID: opID, Kind: "manual_resume", ConvID: convID,
		Attempt: platformexec.AttemptRef{ExecutionID: eID}, ClaimHash: db.ResumeClaimHash(secret),
		State: platformexec.ResumeRequested, LaunchPhase: "requested", Revision: 1,
	}))
	require.NoError(t, db.TransitionResumeOperation(opID, 1, platformexec.ResumeAccepted, "accepted", ""))

	previous := resumeCancellationBeforeCAS
	traced := false
	resumeCancellationBeforeCAS = func(op db.ResumeOperationRow) {
		if traced || op.ID != opID {
			return
		}
		traced = true
		claimed, err := db.ClaimResumeOperation(opID, eID, secret, convID, "resume-race-session", 901, "claim-start")
		require.NoError(t, err)
		require.True(t, claimed, "child wins the first revision exactly between parent read and CAS")
	}
	t.Cleanup(func() { resumeCancellationBeforeCAS = previous })

	require.NoError(t, cancelManagedResumes(convID, "stop raced child claim"))
	require.True(t, traced)
	op, err := db.GetResumeOperation(opID)
	require.NoError(t, err)
	require.NotNil(t, op)
	assert.Equal(t, platformexec.ResumeCancelling, op.State)
	assert.Equal(t, "revoked", op.LaunchPhase)
	registered, err := db.RegisterResumeLaunch(opID, eID, convID, "resume-race-session", "tmux-race", "%85", "/private/race-gate", 901, "claim-start")
	require.NoError(t, err)
	assert.False(t, registered, "revoked child cannot register or release after Stop")
}

func TestResumeReconcile_StaleRequestedUnblocksLaterResume(t *testing.T) {
	setupTestDB(t)
	const convID = "resume-stale-requested"
	oldID := platformexec.NewOperationID()
	require.NoError(t, db.CreateResumeOperation(db.ResumeOperationRow{
		ID: oldID, Kind: "manual_resume", ConvID: convID,
		Attempt:   platformexec.AttemptRef{ExecutionID: platformexec.ID("86868686868686868686868686868686")},
		ClaimHash: db.ResumeClaimHash([]byte("unused-old-claim")), State: platformexec.ResumeRequested,
		LaunchPhase: "requested", Revision: 1, RequestedAt: time.Now().Add(-time.Hour),
	}))

	reconcileResumeOperations(time.Now(), false)
	old, err := db.GetResumeOperation(oldID)
	require.NoError(t, err)
	require.NotNil(t, old)
	assert.Equal(t, platformexec.ResumeFailed, old.State)
	assert.Equal(t, "cleanup_proven", old.LaunchPhase)

	newID := platformexec.NewOperationID()
	err = db.CreateResumeOperation(db.ResumeOperationRow{
		ID: newID, Kind: "manual_resume", ConvID: convID,
		Attempt:   platformexec.AttemptRef{ExecutionID: platformexec.ID("87878787878787878787878787878787")},
		ClaimHash: db.ResumeClaimHash([]byte("new-claim")), State: platformexec.ResumeRequested,
		LaunchPhase: "requested", Revision: 1,
	})
	require.NoError(t, err, "stale crash-left request must not permanently block a legitimate Resume")
}

func TestResumeReconcile_StopReportsCancellationStoreFailure(t *testing.T) {
	setupTestDB(t)
	previous := activeResumeOperationsForCancellation
	activeResumeOperationsForCancellation = func() ([]db.ResumeOperationRow, error) {
		return nil, errors.New("injected store failure")
	}
	t.Cleanup(func() { activeResumeOperationsForCancellation = previous })

	result := managedExecutionRuntime.stop(
		"resume-store-failure", true, db.AgentExitActionForceStop, "", stopWaitForExit(0))
	assert.Equal(t, "error", result.legacy.Action)
	assert.Contains(t, result.legacy.Detail, "could not revoke pending Resume")
	assert.Equal(t, platformexec.StopUnresolved, result.stop.State)
}
