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
// so the reused row is the newest launch even though another row was
// inserted after it, and even when that other row was updated last.
func TestFindSessionByConvID_AReusedRowIsTheNewestLaunch(t *testing.T) {
	setupTestDB(t)
	const conv = "conv-reused-row"
	base := time.Now().Add(-time.Hour)
	require.NoError(t, SaveSession(&SessionRow{
		ID: "a", TmuxSession: "a", ConvID: conv, Status: "exited", Harness: "claude",
		SandboxImplementation: "off", CreatedAt: base, UpdatedAt: base,
	}))
	b := &SessionRow{
		ID: "b", TmuxSession: "b", ConvID: conv, Status: "idle", Harness: "claude",
		SandboxImplementation: "off", CreatedAt: base.Add(time.Minute), UpdatedAt: base.Add(time.Minute),
	}
	require.NoError(t, SaveSession(b))
	now := time.Now()
	require.NoError(t, SaveSession(&SessionRow{
		ID: "a", TmuxSession: "a", ConvID: conv, Status: "running", Harness: "claude",
		SandboxImplementation: "harness-builtin", CreatedAt: now, UpdatedAt: now,
	}))
	// b, not yet marked exited, is touched last: newest by updated_at and by
	// insertion, but not the newest launch.
	b.UpdatedAt = now.Add(time.Second)
	require.NoError(t, SaveSession(b))
	row, err := FindSessionByConvID(conv)
	require.NoError(t, err)
	require.Equal(t, "a", row.ID)
	require.Equal(t, "harness-builtin", row.SandboxImplementation)
}

// An in-pane rotation (/resume B inside a running pane) rebinds the live row
// to conv B and keeps the pane's launch time, which can be older than a dead
// row of B's own. The live row is B's launch.
func TestFindSessionByConvID_ALiveRowRotatedOntoTheConvBeatsANewerDeadOne(t *testing.T) {
	setupTestDB(t)
	paneLaunched := time.Now().Add(-3 * time.Hour)
	require.NoError(t, SaveSession(&SessionRow{
		ID: "pane", TmuxSession: "pane", ConvID: "conv-a", Status: "running", Harness: "claude",
		SandboxImplementation: "harness-builtin", CreatedAt: paneLaunched, UpdatedAt: paneLaunched,
	}))
	deadLaunched := time.Now().Add(-2 * time.Hour)
	require.NoError(t, SaveSession(&SessionRow{
		ID: "conv-b", TmuxSession: "conv-b", ConvID: "conv-b", Status: "exited", Harness: "claude",
		SandboxImplementation: "off", CreatedAt: deadLaunched, UpdatedAt: deadLaunched,
	}))
	pane, err := LoadSession("pane")
	require.NoError(t, err)
	pane.ConvID = "conv-b"
	pane.UpdatedAt = time.Now()
	require.NoError(t, SaveSession(pane))

	row, err := FindSessionByConvID("conv-b")
	require.NoError(t, err)
	require.Equal(t, "pane", row.ID, "the live pane is conv-b's launch, not its newer dead row")
	require.Equal(t, "harness-builtin", row.SandboxImplementation)
	rows, err := FindSessionsByConvID("conv-b")
	require.NoError(t, err)
	require.Equal(t, "pane", rows[0].ID)
}

// The dashboard's batch loader orders each conv's rows as the single-conv
// loader does, so both pick the same launch.
func TestFindSessionsByConvIDs_OrdersAsTheSingleConvLoader(t *testing.T) {
	setupTestDB(t)
	const conv = "conv-batch"
	started := time.Now().Add(-time.Minute)
	require.NoError(t, SaveSession(&SessionRow{
		ID: "old", TmuxSession: "old", ConvID: conv, Status: "running", Harness: "claude",
		CreatedAt: started, UpdatedAt: started,
	}))
	old, err := LoadSession("old")
	require.NoError(t, err)
	relaunched := time.Now().Add(-30 * time.Second)
	require.NoError(t, SaveSession(&SessionRow{
		ID: "new", TmuxSession: "new", ConvID: conv, Status: "running", Harness: "claude",
		CreatedAt: relaunched, UpdatedAt: relaunched,
	}))
	_, err = MarkSessionExitedIfUnchanged("old", old.Status, old.UpdatedAt, "")
	require.NoError(t, err)

	single, err := FindSessionsByConvID(conv)
	require.NoError(t, err)
	batch, err := FindSessionsByConvIDs([]string{conv})
	require.NoError(t, err)
	require.Len(t, batch[conv], 2)
	for i := range single {
		require.Equal(t, single[i].ID, batch[conv][i].ID)
	}
	require.Equal(t, "new", batch[conv][0].ID)
}

// The durable relaunch profiles follow the current launch too. After an
// in-pane rotation onto conv-b, the live pane is conv-b's launch: its posture
// is the one a relaunch of conv-b reproduces, not that of conv-b's newer dead
// row. And the stopped launch's row, marked exited after its successor was
// written, leaves the successor's projection as it is.
func TestRelaunchProfile_FollowsTheCurrentLaunch(t *testing.T) {
	setupTestDB(t)
	paneLaunched := time.Now().Add(-3 * time.Hour)
	require.NoError(t, SaveSession(&SessionRow{
		ID: "pane", TmuxSession: "pane", ConvID: "conv-a", Status: "running", Harness: "claude",
		HarnessBuiltinMode: "default", SandboxImplementation: "harness-builtin", CreatedAt: paneLaunched, UpdatedAt: paneLaunched,
	}))
	deadLaunched := time.Now().Add(-2 * time.Hour)
	require.NoError(t, SaveSession(&SessionRow{
		ID: "conv-b", TmuxSession: "conv-b", ConvID: "conv-b", Status: "exited", Harness: "claude",
		HarnessBuiltinMode: "off", SandboxImplementation: "off", CreatedAt: deadLaunched, UpdatedAt: deadLaunched,
	}))
	profile, err := SessionLaunchProfileForConv("conv-b")
	require.NoError(t, err)
	require.Equal(t, "off", profile.SandboxImplementation, "the fixture: conv-b's own dead launch first")

	pane, err := LoadSession("pane")
	require.NoError(t, err)
	pane.ConvID = "conv-b"
	pane.UpdatedAt = time.Now()
	require.NoError(t, SaveSession(pane))
	profile, err = SessionLaunchProfileForConv("conv-b")
	require.NoError(t, err)
	require.Equal(t, "harness-builtin", profile.SandboxImplementation,
		"the live pane rotated onto conv-b is its launch, though it started earlier than the dead row")

	// A late save of the dead row (a stale hook tick) does not take it back.
	dead, err := LoadSession("conv-b")
	require.NoError(t, err)
	dead.UpdatedAt = time.Now()
	require.NoError(t, SaveSession(dead))
	profile, err = SessionLaunchProfileForConv("conv-b")
	require.NoError(t, err)
	require.Equal(t, "harness-builtin", profile.SandboxImplementation, "a save of a row that is not the current launch leaves it")
}

// The rotation carries the pane's whole posture, opt-gated fields included,
// and the "project the conv's latest launch" path (head advance, enrollment,
// backfill) picks the same launch the projection accepts.
func TestRelaunchProfile_RotationCarriesTheWholePosture(t *testing.T) {
	setupTestDB(t)
	paneLaunched := time.Now().Add(-3 * time.Hour)
	require.NoError(t, SaveSession(&SessionRow{
		ID: "pane", TmuxSession: "pane", ConvID: "conv-a", Status: "running", Harness: "claude",
		SandboxImplementation: "harness-builtin", CreatedAt: paneLaunched, UpdatedAt: paneLaunched,
	}))
	require.NoError(t, SetSessionRemoteControl("pane", true))
	deadLaunched := time.Now().Add(-2 * time.Hour)
	require.NoError(t, SaveSession(&SessionRow{
		ID: "conv-b", TmuxSession: "conv-b", ConvID: "conv-b", Status: "exited", Harness: "claude",
		SandboxImplementation: "off", CreatedAt: deadLaunched, UpdatedAt: deadLaunched,
	}))
	require.NoError(t, SetSessionRemoteControl("conv-b", false))

	require.NoError(t, SetSessionConvID("pane", "conv-b"))
	profile, err := ConversationResumeProfileForConv("conv-b")
	require.NoError(t, err)
	require.NotNil(t, profile)
	require.Equal(t, "pane", profile.SourceSessionID)
	require.NotNil(t, profile.FallbackRelaunch)
	require.NotNil(t, profile.FallbackRelaunch.RemoteControl, "the pane's remote control goes with it")
	require.True(t, *profile.FallbackRelaunch.RemoteControl)

	require.NoError(t, BackfillDurableRelaunchProfilesFromLatestSession("conv-b"))
	profile, err = ConversationResumeProfileForConv("conv-b")
	require.NoError(t, err)
	require.Equal(t, "pane", profile.SourceSessionID, "the latest-launch projection picks the live pane")
}
