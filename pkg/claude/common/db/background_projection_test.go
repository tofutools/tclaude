package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	platformexec "github.com/tofutools/tclaude/pkg/claude/platform/execution"
)

func seedBackgroundProjectionSession(
	t *testing.T,
	id string,
	generation platformexec.ID,
	created time.Time,
) *SessionRow {
	t.Helper()
	now := time.Now()
	row := &SessionRow{
		ID: id, ConvID: "conv-" + id, TmuxSession: "tmux-" + id,
		PID: 4242, Status: "main_agent_idle", CreatedAt: created,
		BgShellsJSON: BgShellSet(nil).Add("shell-old", "npm run dev", now).Encode(),
		MonitorsJSON: MonitorSet(nil).
			Add("monitor-old", "gh pr checks --watch", "checks", false, now, time.Time{}).
			Encode(),
		ExitLaunchGeneration: generation.String(),
	}
	require.NoError(t, SaveSession(row))
	stored, err := LoadSession(id)
	require.NoError(t, err)
	require.Equal(t, generation, stored.ExecutionID)
	return stored
}

func backgroundProjectionRef(row *SessionRow) BackgroundProjectionRef {
	return BackgroundProjectionRef{
		Attempt: platformexec.AttemptRef{
			ExecutionID: row.ExecutionID, LegacySessionID: row.ID,
		},
		SessionCreatedAt: row.CreatedAt,
		MainPID:          row.PID,
	}
}

func TestProjectSessionBackgroundLedgersCommitsBothInputsAtomically(t *testing.T) {
	setupTestDB(t)
	const generation = platformexec.ID("11111111111111111111111111111111")
	row := seedBackgroundProjectionSession(t, "background-project", generation, time.Now())

	ok, err := ProjectSessionBackgroundLedgers(
		backgroundProjectionRef(row),
		row.BgShellsJSON, row.MonitorsJSON,
		"", row.MonitorsJSON,
	)
	require.NoError(t, err)
	require.True(t, ok)

	stored, err := LoadSession(row.ID)
	require.NoError(t, err)
	assert.Empty(t, stored.BgShellsJSON)
	assert.Equal(t, row.MonitorsJSON, stored.MonitorsJSON)
}

func TestProjectSessionBackgroundLedgersRefusesConcurrentHookAdditionToEitherLedger(t *testing.T) {
	setupTestDB(t)
	const generation = platformexec.ID("22222222222222222222222222222222")
	row := seedBackgroundProjectionSession(t, "background-hook-race", generation, time.Now())

	// Simulate the upstream hook writer adding a monitor after collection.
	currentMonitors := ParseMonitorSet(row.MonitorsJSON).
		Add("monitor-new", "tail -f deploy.log", "deploy", false, time.Now(), time.Time{}).
		Encode()
	d, err := Open()
	require.NoError(t, err)
	_, err = d.Exec("UPDATE sessions SET monitors_json = ? WHERE id = ?", currentMonitors, row.ID)
	require.NoError(t, err)

	ok, err := ProjectSessionBackgroundLedgers(
		backgroundProjectionRef(row),
		row.BgShellsJSON, row.MonitorsJSON,
		"", "",
	)
	require.NoError(t, err)
	assert.False(t, ok, "a change to either input rejects the whole projection")

	stored, err := LoadSession(row.ID)
	require.NoError(t, err)
	assert.Equal(t, row.BgShellsJSON, stored.BgShellsJSON,
		"the shell half must not commit when the monitor input raced")
	assert.Equal(t, currentMonitors, stored.MonitorsJSON,
		"the concurrent hook addition must win")
}

func TestProjectSessionBackgroundLedgersRefusesSameRowSuccessor(t *testing.T) {
	setupTestDB(t)
	const (
		predecessor = platformexec.ID("33333333333333333333333333333333")
		successor   = platformexec.ID("44444444444444444444444444444444")
	)
	old := seedBackgroundProjectionSession(t, "background-row-reuse", predecessor, time.Now())

	replacement := &SessionRow{
		ID: old.ID, ConvID: "conv-successor", TmuxSession: old.TmuxSession,
		PID: old.PID, Status: "main_agent_idle", CreatedAt: old.CreatedAt.Add(time.Second),
		BgShellsJSON: BgShellSet(nil).
			Add("shell-successor", "serve successor", time.Now()).Encode(),
		MonitorsJSON:         old.MonitorsJSON,
		ExitLaunchGeneration: successor.String(),
	}
	require.NoError(t, SaveSession(replacement))

	ok, err := ProjectSessionBackgroundLedgers(
		backgroundProjectionRef(old),
		old.BgShellsJSON, old.MonitorsJSON,
		"", "",
	)
	require.NoError(t, err)
	assert.False(t, ok)

	stored, err := LoadSession(old.ID)
	require.NoError(t, err)
	assert.Equal(t, successor, stored.ExecutionID)
	assert.Contains(t, stored.BgShellsJSON, "shell-successor")
}

func TestSetSessionStatusFromBackgroundProjectionRefusesHookAfterProjection(t *testing.T) {
	setupTestDB(t)
	const generation = platformexec.ID("55555555555555555555555555555555")
	row := seedBackgroundProjectionSession(t, "background-status-hook-race", generation, time.Now())
	ref := backgroundProjectionRef(row)

	projected, err := ProjectSessionBackgroundLedgers(
		ref, row.BgShellsJSON, row.MonitorsJSON, "", "")
	require.NoError(t, err)
	require.True(t, projected)

	// A hook starts new work after the ledger projection and bumps updated_at.
	current, err := LoadSession(row.ID)
	require.NoError(t, err)
	current.Status = "working"
	current.BgShellsJSON = BgShellSet(nil).
		Add("shell-new", "serve new work", time.Now()).Encode()
	require.NoError(t, SaveSession(current))

	settled, err := SetSessionStatusFromBackgroundProjection(
		ref, row.Status, row.UpdatedAt, "", "",
		"idle", "", time.Now())
	require.NoError(t, err)
	assert.False(t, settled)

	stored, err := LoadSession(row.ID)
	require.NoError(t, err)
	assert.Equal(t, "working", stored.Status)
	assert.Contains(t, stored.BgShellsJSON, "shell-new")
}

func TestSetSessionStatusFromBackgroundProjectionRefusesSameRowSuccessor(t *testing.T) {
	setupTestDB(t)
	const (
		predecessor = platformexec.ID("66666666666666666666666666666666")
		successor   = platformexec.ID("77777777777777777777777777777777")
	)
	old := seedBackgroundProjectionSession(t, "background-status-row-reuse", predecessor, time.Now())
	ref := backgroundProjectionRef(old)

	replacement := &SessionRow{
		ID: old.ID, ConvID: "conv-status-successor", TmuxSession: old.TmuxSession,
		PID: old.PID, Status: "working", CreatedAt: old.CreatedAt.Add(time.Second),
		BgShellsJSON: BgShellSet(nil).
			Add("shell-successor", "successor work", time.Now()).Encode(),
		ExitLaunchGeneration: successor.String(),
	}
	require.NoError(t, SaveSession(replacement))

	settled, err := SetSessionStatusFromBackgroundProjection(
		ref, old.Status, old.UpdatedAt, old.BgShellsJSON, old.MonitorsJSON,
		"idle", "", time.Now())
	require.NoError(t, err)
	assert.False(t, settled)

	stored, err := LoadSession(old.ID)
	require.NoError(t, err)
	assert.Equal(t, successor, stored.ExecutionID)
	assert.Equal(t, "working", stored.Status)
	assert.Contains(t, stored.BgShellsJSON, "shell-successor")
}
