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

	// The baseline is taken from a SEPARATE conversation built the same way.
	//
	// Reading it from the subject would destroy the scenario: durableRelaunchConfigForConv
	// backfills when the agent profile is nil, and the backfill populates BOTH the
	// agent blob and the conversation fallback. An earlier version of this test
	// took the baseline from the subject, noticed the agent blob had been
	// repopulated, re-emptied THAT — and left the fallback populated, so it
	// measured a seed in a state the hazard cannot occur in and would have stayed
	// green with the real guarantee deleted. The most innocent-looking line in a
	// test is the one that took a baseline to be careful.
	const twinConv = "ses_copilot_drive_seed_twin"
	const twinSession = "spwn-copilot-drive-seed-twin"
	_, _, err = db.EnsureAgentForConv(twinConv, "test")
	require.NoError(t, err)
	require.NoError(t, session.SaveSessionState(&session.SessionState{
		ID: twinSession, TmuxSession: twinSession, ConvID: twinConv,
		Harness: harness.CopilotName, Status: session.StatusIdle,
		Cwd: t.TempDir(),
	}))
	before, err := durableRelaunchConfigForConv(twinConv)
	require.NoError(t, err, "precondition: a conversation of this shape must be relaunchable, "+
		"or a failure on the subject afterwards would prove nothing about the seed")
	require.NotNil(t, before)

	// And the subject's own pre-pin state is asserted directly rather than
	// inferred from a resolve: this is the guarantee the seed depends on, so it is
	// read rather than assumed.
	conversation, err := db.ConversationResumeProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, conversation, "the conversation record must exist before the pin")
	require.NotNil(t, conversation.FallbackRelaunch,
		"the fallback is what answers for every field a minimal seeded profile leaves nil")
	require.NotNil(t, conversation.FallbackRelaunch.ApprovalPolicy,
		"approval policy is the one field whose absence is a HARD error at relaunch, so "+
			"this is the specific thing that keeps a seed from wedging the agent")

	// WHICH ARM IS THIS: still the empty blob, because the baseline was taken
	// elsewhere. Re-read rather than assumed — that is the check that caught the
	// earlier version of this test measuring the wrong branch.
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
	// Cwd is compared against the SUBJECT's own conversation record rather than
	// the twin's: the twin is the same shape, not the same launch, and each gets
	// its own directory.
	assert.Equal(t, conversation.Cwd, after.Cwd, "nor its launch directory")
}
