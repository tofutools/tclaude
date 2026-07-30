package db

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Session exit is a known-zero boundary for every ledger of turn-outliving
// work: sub-agents and monitors run inside the harness process and
// background shells are its children, so none can survive it.
//
// The in-memory MarkStateExited / SessionEnd arms nil these ledgers, but
// neither persists through SaveSessionState — the exit-audit UPDATEs below
// are what actually write the columns. A ledger cleared only in memory
// therefore survives into the resumed conversation, where a `/resume`
// SessionStart is NOT treated as known-zero, and badges work that ended
// before the previous process died. These tests pin the SQL.

func seedSessionWithLiveLedgers(t *testing.T, sessionID, conv, generation string) *SessionRow {
	t.Helper()
	now := time.Now()
	_, _, err := EnsureAgentForConv(conv, "test")
	require.NoError(t, err)

	row := &SessionRow{
		ID: sessionID, TmuxSession: "tmux-" + sessionID, Cwd: t.TempDir(),
		ConvID: conv, Status: "working", Harness: "claude",
		SubagentCount: 1,
		SubagentsJSON: SubagentSet(nil).Add("agent-1", "explore", now).Encode(),
		BgShellsJSON:  BgShellSet(nil).Add("shell-1", "npm run dev", now).Encode(),
		// A persistent websocket watch: no deadline and no process, so
		// nothing but this boundary can ever retire it inside MonitorTTL.
		MonitorsJSON: MonitorSet(nil).
			Add("mon-1", "", "wss://events.example.com/stream", true, now, time.Time{}).
			Encode(),
	}
	require.NoError(t, SaveSession(row))
	require.NoError(t, SetSessionExitLaunchGeneration(sessionID, generation))
	require.NoError(t, SetSessionExitLaunchBinding(sessionID, generation, strings.Repeat("a", 64), "%7"))

	stored, err := LoadSession(sessionID)
	require.NoError(t, err)
	require.NotEmpty(t, stored.MonitorsJSON, "the fixture must start with a live monitor ledger")
	return stored
}

func assertLedgersCleared(t *testing.T, sessionID string) {
	t.Helper()
	got, err := LoadSession(sessionID)
	require.NoError(t, err)
	assert.Equal(t, "exited", got.Status)
	assert.Equal(t, "", got.SubagentsJSON, "sub-agents die with the process")
	assert.Equal(t, "", got.BgShellsJSON, "background shells die with the process")
	assert.Equal(t, "", got.MonitorsJSON,
		"monitors run inside the process and must not survive it into a resume")
}

// The reaper's path: it observes a dead pane and applies the state CAS
// through MarkSessionExitedAndRecordObservationIfUnchanged.
func TestMarkSessionExitedAndRecordObservation_ClearsEveryBackgroundLedger(t *testing.T) {
	setupTestDB(t)
	const generation = "11111111111111111111111111111111"
	stored := seedSessionWithLiveLedgers(t, "exit-ledgers-1", "conv-exit-ledgers-1", generation)

	code := 0
	ok, _, err := MarkSessionExitedAndRecordObservationIfUnchanged(
		stored.ID, stored.Status, stored.UpdatedAt, "unexpected",
		AgentExitObservation{
			At: time.Now().UTC(), SessionID: stored.ID,
			Observer: AgentExitObserverReaper, CauseKind: AgentExitCauseNormal,
			ExitCode: &code, ExpectedGeneration: generation, ObservedState: "exited",
		})
	require.NoError(t, err)
	require.True(t, ok)

	assertLedgersCleared(t, "exit-ledgers-1")
}

func TestRecordSessionEndExitObservation_ClearsEveryBackgroundLedger(t *testing.T) {
	setupTestDB(t)
	const generation = "22222222222222222222222222222222"
	seedSessionWithLiveLedgers(t, "exit-ledgers-2", "conv-exit-ledgers-2", generation)

	accepted, _, err := RecordSessionEndExitObservation(AgentExitObservation{
		At: time.Now().UTC(), SessionID: "exit-ledgers-2",
		Observer: AgentExitObserverHook, CauseKind: AgentExitCauseNormal,
		Reason: "prompt_input_exit", ObservedState: "exited",
		ExpectedGeneration: generation,
	})
	require.NoError(t, err)
	require.True(t, accepted)

	assertLedgersCleared(t, "exit-ledgers-2")
}

func TestMarkSessionExitedIfUnchanged_ClearsEveryBackgroundLedger(t *testing.T) {
	setupTestDB(t)
	const generation = "33333333333333333333333333333333"
	stored := seedSessionWithLiveLedgers(t, "exit-ledgers-3", "conv-exit-ledgers-3", generation)

	marked, err := MarkSessionExitedIfUnchanged(
		stored.ID, stored.Status, stored.UpdatedAt, "process gone")
	require.NoError(t, err)
	require.True(t, marked)

	assertLedgersCleared(t, "exit-ledgers-3")
}
