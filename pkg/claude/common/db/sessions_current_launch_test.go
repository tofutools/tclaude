package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A resume writes the new launch's row, and the stop path or the reaper marks
// the old launch's row exited afterwards, bumping its updated_at past the new
// row's. Readers of a conv's launch (its sandbox posture, its launch dir) must
// still get the new launch: the old row is the dead launch's posture, and a
// sandbox the operator tightened while the agent was offline would otherwise
// read as the looser one it replaced.
func TestFindSessionByConvID_IsTheNewestLaunchNotTheLastUpdatedRow(t *testing.T) {
	setupTestDB(t)
	const conv = "conv-current-launch"
	started := time.Now().Add(-time.Minute)

	old := &SessionRow{
		ID: "spwn-old", TmuxSession: "spwn-old", ConvID: conv, Cwd: "/work", Status: "running",
		Harness: "claude", HarnessBuiltinMode: "off", SandboxImplementation: "off",
		CreatedAt: started, UpdatedAt: started,
	}
	require.NoError(t, SaveSession(old))
	oldRow, err := LoadSession("spwn-old")
	require.NoError(t, err)

	// The resume: the new launch's row, tightened while the agent was offline.
	relaunched := time.Now().Add(-30 * time.Second)
	require.NoError(t, SaveSession(&SessionRow{
		ID: "rsme-new", TmuxSession: "rsme-new", ConvID: conv, Cwd: "/work", Status: "running",
		Harness: "claude", HarnessBuiltinMode: "default", SandboxImplementation: "harness-builtin",
		CreatedAt: relaunched, UpdatedAt: relaunched,
	}))

	// Then the old launch is marked exited: now the most recently updated row.
	marked, err := MarkSessionExitedIfUnchanged("spwn-old", oldRow.Status, oldRow.UpdatedAt, "")
	require.NoError(t, err)
	require.True(t, marked)
	oldNow, err := LoadSession("spwn-old")
	require.NoError(t, err)
	newNow, err := LoadSession("rsme-new")
	require.NoError(t, err)
	require.True(t, oldNow.UpdatedAt.After(newNow.UpdatedAt), "the fixture: the dead launch's row is the last updated")

	row, err := FindSessionByConvID(conv)
	require.NoError(t, err)
	require.NotNil(t, row)
	require.Equal(t, "rsme-new", row.ID, "the current launch, not the last updated row")
	require.Equal(t, "harness-builtin", row.SandboxImplementation)

	rows, err := FindSessionsByConvID(conv)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, "rsme-new", rows[0].ID, "every row, the current launch first")

	profile, err := SessionLaunchProfileForConv(conv)
	require.NoError(t, err)
	require.Equal(t, "harness-builtin", profile.SandboxImplementation,
		"the launch profile is the current launch's posture too")
}

// A resume can reuse its row (the same id): the upsert restamps created_at,
// so the reused row is the newest launch even though an older-inserted row
// exists for the conv.
func TestFindSessionByConvID_AReusedRowIsTheNewestLaunch(t *testing.T) {
	setupTestDB(t)
	const conv = "conv-reused-row"
	base := time.Now().Add(-time.Hour)
	require.NoError(t, SaveSession(&SessionRow{
		ID: "a", TmuxSession: "a", ConvID: conv, Status: "exited", Harness: "claude",
		SandboxImplementation: "off", CreatedAt: base, UpdatedAt: base,
	}))
	require.NoError(t, SaveSession(&SessionRow{
		ID: "b", TmuxSession: "b", ConvID: conv, Status: "exited", Harness: "claude",
		SandboxImplementation: "off", CreatedAt: base.Add(time.Minute), UpdatedAt: base.Add(time.Minute),
	}))
	now := time.Now()
	require.NoError(t, SaveSession(&SessionRow{
		ID: "a", TmuxSession: "a", ConvID: conv, Status: "running", Harness: "claude",
		SandboxImplementation: "harness-builtin", CreatedAt: now, UpdatedAt: now,
	}))
	row, err := FindSessionByConvID(conv)
	require.NoError(t, err)
	require.Equal(t, "a", row.ID)
	require.Equal(t, "harness-builtin", row.SandboxImplementation)
}
