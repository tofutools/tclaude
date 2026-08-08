package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// TCL-1082. The drive switch seeds a minimal agent relaunch profile when the
// agent's blob is EMPTY, because a targeted field edit has nothing to edit
// inside an absent record.
//
// That seed lands in the one hole a relaunch treats specially:
// durableRelaunchConfigForConv calls
// BackfillDurableRelaunchProfilesFromLatestSession only when
// AgentRelaunchProfileForConv returns nil, and it returns nil for exactly an
// empty blob. So a seed makes the profile non-nil and the backfill stops
// running — and if nothing else can answer for the approval policy, a relaunch
// fails outright.
//
// A rollback that BREAKS the agent it was protecting is worse than the gap it
// closes, so this is measured rather than argued.

// TestSeedingTheDriveLeavesTheAgentRelaunchable is the property that matters:
// after a pin into an empty profile blob, the agent must still resolve a
// relaunch configuration.
func TestSeedingTheDriveLeavesTheAgentRelaunchable(t *testing.T) {
	setupTestDB(t)

	const (
		convID    = "ses_copilot_drive_seed"
		sessionID = "spwn-copilot-drive-seed"
	)
	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, session.SaveSessionState(&session.SessionState{
		ID: sessionID, TmuxSession: sessionID, ConvID: convID,
		Harness: harness.CopilotName, Status: session.StatusIdle,
		Cwd: t.TempDir(),
	}))

	// The state the seed exists for: an agent row whose relaunch profile blob is
	// EMPTY. It has to be forced, because enrolling an agent already seeds a
	// profile from its spawn config — which is worth knowing on its own, since it
	// means the hole this seed fills is narrow rather than ordinary. Forcing it is
	// what makes this a measurement of the seed instead of a measurement of the
	// compare-and-set that runs when a profile exists.
	handle, err := db.Open()
	require.NoError(t, err)
	_, err = handle.Exec(`UPDATE agents SET relaunch_profile = '' WHERE agent_id = ?`, agentID)
	require.NoError(t, err)

	raw, err := db.AgentRelaunchProfileRaw(agentID)
	require.NoError(t, err)
	require.Empty(t, raw, "precondition: the seed path needs an EMPTY profile blob")

	before, err := durableRelaunchConfigForConv(convID)
	require.NoError(t, err, "precondition: this conversation must be relaunchable BEFORE the "+
		"pin, or a failure afterwards would prove nothing about the seed")
	require.NotNil(t, before)

	// WHICH ARM IS THIS. Resolving a relaunch above runs the backfill for a nil
	// profile, and the backfill WRITES one — measured: after the baseline read the
	// blob is populated again. So the blob has to be emptied a SECOND time here,
	// or the pin below exercises the ordinary append-into-an-existing-blob path
	// while reading as a test of the seed.
	_, err = handle.Exec(`UPDATE agents SET relaunch_profile = '' WHERE agent_id = ?`, agentID)
	require.NoError(t, err)
	rawBeforePin, err := db.AgentRelaunchProfileRaw(agentID)
	require.NoError(t, err)
	require.Empty(t, rawBeforePin,
		"the pin must run against an EMPTY blob, or this measures the append path")

	target, err := db.CopilotDriveTargetForConv(convID)
	require.NoError(t, err)
	require.Equal(t, db.CopilotDriveRecordNone, target.Record)
	created, record, err := writeCopilotDrive(convID, target, false)
	require.NoError(t, err)
	require.True(t, created, "an empty blob has nothing recorded, so this is a creation")
	require.Equal(t, db.CopilotDriveRecordAgentProfile, record)

	after, err := durableRelaunchConfigForConv(convID)
	assert.NoError(t, err,
		"MEASURED: a seeded profile must not wedge the relaunch it was written to "+
			"protect; the seed suppresses the empty-blob backfill, so everything the "+
			"relaunch needs has to still be answerable from the conversation fallback")
	require.NotNil(t, after)

	// The seed must change the drive and nothing else. A minimal profile that
	// silently replaced the resolved posture would be the whole-blob hazard the
	// targeted write exists to avoid, arriving through the one path allowed to
	// write a whole blob.
	assert.False(t, after.CopilotAPI, "the pin must be what a relaunch reads")
	assert.Equal(t, before.Harness, after.Harness)
	assert.Equal(t, before.Approval, after.Approval,
		"seeding a drive must not change the approval policy the agent relaunches under")
	assert.Equal(t, before.Sandbox, after.Sandbox,
		"nor its sandbox mode")
	assert.Equal(t, before.Model, after.Model, "nor its model")
	assert.Equal(t, before.Cwd, after.Cwd)
}
