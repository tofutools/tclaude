package agentd_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestDashboardSnapshot_SurfacesCodexRecoveryStatesEverywhere(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("recovery-crew")
	now := time.Now().UTC()
	statuses := []string{
		db.AgentRecoveryStatusCrashed,
		db.AgentRecoveryStatusRestarting,
		db.AgentRecoveryStatusBackoff,
		db.AgentRecoveryStatusRecovered,
		db.AgentRecoveryStatusSuppressed,
	}
	convs := map[string]string{}
	recoveredAt := now
	for i, status := range statuses {
		conv := "recovery-state-" + status
		label := "recovery-session-" + status
		tmuxName := "recovery-tmux-" + status
		convs[status] = conv
		f.HaveConvWithTitle(conv, status+" worker")
		f.HaveAliveCodexSession(conv, label, tmuxName, f.TestCwd(status))
		f.HaveMember("recovery-crew", conv)
		generation := strings.Repeat(string(rune('a'+i)), 32)
		require.NoError(t, db.SetSessionExitLaunchGeneration(label, generation))
		require.NoError(t, db.SetSessionExitLaunchBinding(label, generation, strings.Repeat("f", 64), "%7"))
		require.NoError(t, db.MarkSessionExitLaunchReleasing(label, generation))
		require.NoError(t, db.MarkSessionExitLaunchReleased(label, generation))
		if status != db.AgentRecoveryStatusRecovered {
			f.MarkOffline(tmuxName)
		}
		code := 1
		_, err := db.RecordAgentExitObservation(db.AgentExitObservation{At: now,
			SessionID: label, Observer: db.AgentExitObserverReaper,
			CauseKind: db.AgentExitCauseNormal, ExitCode: &code,
			ExpectedGeneration: generation, ObservedState: "exited"})
		require.NoError(t, err)
		database, err := db.Open()
		require.NoError(t, err)
		reason := ""
		if status == db.AgentRecoveryStatusSuppressed {
			reason = "resume_provenance_missing"
		}
		recoveredAtValue := ""
		if status == db.AgentRecoveryStatusRecovered {
			recoveredAtValue = recoveredAt.Format(time.RFC3339Nano)
		}
		_, err = database.Exec(`UPDATE agent_recovery SET status=?, reason_code=?,
			consecutive_crashes=3, backoff_step=2, backoff_seconds=20,
			next_attempt_at=?, recovered_at=? WHERE conv_id=?`, status, reason,
			now.Add(20*time.Second).Format(time.RFC3339Nano), recoveredAtValue, conv)
		require.NoError(t, err)
		if status == db.AgentRecoveryStatusRecovered {
			_, err = database.Exec(`UPDATE sessions SET last_hook=? WHERE id=?`,
				recoveredAt.Add(-time.Second).Format(time.RFC3339Nano), label)
			require.NoError(t, err)
		}
	}

	snap := fetchSnapshotOnly(t, agentd.BuildDashboardHandlerForTest())
	assertState := func(surface string, conv, want string, state dashState) {
		assert.Equal(t, want, state.RecoveryStatus, "%s %s", surface, conv)
		assert.Equal(t, 3, state.RecoveryCount, "%s %s", surface, conv)
		require.NotNil(t, state.RecoveryLastExitCode, "%s %s", surface, conv)
		assert.Equal(t, 1, *state.RecoveryLastExitCode, "%s %s", surface, conv)
		assert.NotEmpty(t, state.RecoveryDetail, "%s %s", surface, conv)
	}
	for status, conv := range convs {
		var groupState, fleetState *dashState
		for _, group := range snap.Groups {
			for i := range group.Members {
				if group.Members[i].ConvID == conv {
					groupState = &group.Members[i].State
				}
			}
		}
		for i := range snap.Agents {
			if snap.Agents[i].ConvID == conv {
				fleetState = &snap.Agents[i].State
			}
		}
		require.NotNil(t, groupState, "group member %s", conv)
		require.NotNil(t, fleetState, "fleet agent %s", conv)
		assertState("group", conv, status, *groupState)
		assertState("fleet", conv, status, *fleetState)
	}

	// The durable recovered row remains for crash-loop reset/audit purposes,
	// while the operational badge clears on the first later hook.
	database, err := db.Open()
	require.NoError(t, err)
	_, err = database.Exec(`UPDATE sessions SET last_hook=? WHERE conv_id=?`,
		recoveredAt.Add(time.Second).Format(time.RFC3339Nano), convs[db.AgentRecoveryStatusRecovered])
	require.NoError(t, err)
	snap = fetchSnapshotOnly(t, agentd.BuildDashboardHandlerForTest())
	for _, group := range snap.Groups {
		for _, member := range group.Members {
			if member.ConvID == convs[db.AgentRecoveryStatusRecovered] {
				assert.Empty(t, member.State.RecoveryStatus)
			}
		}
	}
	for _, fleetAgent := range snap.Agents {
		if fleetAgent.ConvID == convs[db.AgentRecoveryStatusRecovered] {
			assert.Empty(t, fleetAgent.State.RecoveryStatus)
		}
	}
}
