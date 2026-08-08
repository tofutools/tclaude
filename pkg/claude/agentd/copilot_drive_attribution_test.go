package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// TCL-1082. A pin moves CopilotAPI. CopilotAPISource names the tier that CHOSE
// CopilotAPI and is documented as travelling WITH it, so an edit that moved one
// and left the other would leave a record naming a tier that chose the opposite
// posture.
//
// That is a WRONG attribution rather than a missing one, and the difference
// matters here because it is not only rendered. templateAgentLaunchFromConv asks
// launchValueWasChosen(CopilotAPISource) to decide whether a from-group snapshot
// carries the drive as a SPEC LINE or demotes it to an observation it declines
// to carry. So a stale attribution silently reclassifies an operator's decision:
//
//   - source left at "harness default" -> the pin is dropped from the snapshot as
//     something nobody chose
//   - source left naming a group-default profile -> a pin that CONTRADICTS that
//     profile is recorded as having come from it
//
// Both arms are measured below rather than argued, because "the field is stale"
// and "the staleness changes an outcome" are different claims and only the
// second one makes it a defect.

func TestPinningTheDriveMovesItsAttributionToo(t *testing.T) {
	setupTestDB(t)

	const convID = "ses_copilot_drive_attribution"
	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, session.SaveSessionState(&session.SessionState{
		ID: "spwn-drive-attribution", TmuxSession: "spwn-drive-attribution", ConvID: convID,
		Harness: harness.CopilotName, Status: session.StatusIdle, Cwd: t.TempDir(),
	}))

	// Born ON the drive, attributed to the tier that put it there — the shape a
	// group default produces.
	onDrive := true
	chosenBy := "default profile copilot-api-crew"
	require.NoError(t, db.SetAgentRelaunchProfile(agentID, db.AgentRelaunchProfile{
		Version:          db.RelaunchProfileVersion,
		CopilotAPI:       &onDrive,
		CopilotAPISource: &chosenBy,
	}))

	target, err := db.CopilotDriveTargetForConv(convID)
	require.NoError(t, err)
	require.Equal(t, db.CopilotDriveRecordAgentProfile, target.Record)
	require.True(t, target.Value,
		"precondition: the agent must start ON the drive, or the pin below rolls back "+
			"nothing and the attribution would have no reason to move")

	_, _, err = writeCopilotDrive(convID, target, false)
	require.NoError(t, err)

	after, err := db.AgentRelaunchProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, after)
	require.NotNil(t, after.CopilotAPI)
	assert.False(t, *after.CopilotAPI, "the pin must land")

	require.NotNil(t, after.CopilotAPISource,
		"a drive with no source reads as un-chosen, and a from-group snapshot "+
			"declines to carry an un-chosen value as a spec line")
	assert.Equal(t, agent.ProvExplicit, *after.CopilotAPISource,
		"the operator chose this, so the record must say so; leaving the previous "+
			"tier's name here would attribute a pin that CONTRADICTS that profile to "+
			"the very profile it contradicts")
	assert.NotEqual(t, chosenBy, *after.CopilotAPISource)
}

// TestPinningADriveNobodyChoseBecomesAChoice is the arm that costs an operator
// something when it is wrong, and it fails in the opposite direction from the
// one above: here the stale source UNDERSTATES rather than misattributes.
//
// An agent whose drive was merely what the resolver landed on carries the
// harness-default attribution. launchValueWasChosen returns false for it, so a
// from-group snapshot lists the axis as observed-not-chosen and leaves it out of
// the spec. If a pin does not move the attribution, the operator's deliberate
// decision inherits that silence and is dropped from every future deploy.
func TestPinningADriveNobodyChoseBecomesAChoice(t *testing.T) {
	setupTestDB(t)

	const convID = "ses_copilot_drive_attribution_unchosen"
	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, session.SaveSessionState(&session.SessionState{
		ID: "spwn-drive-attr-unchosen", TmuxSession: "spwn-drive-attr-unchosen",
		ConvID: convID, Harness: harness.CopilotName, Status: session.StatusIdle,
		Cwd: t.TempDir(),
	}))

	onDrive := true
	nobodyChose := agent.ProvHarnessDefault
	require.NoError(t, db.SetAgentRelaunchProfile(agentID, db.AgentRelaunchProfile{
		Version:          db.RelaunchProfileVersion,
		CopilotAPI:       &onDrive,
		CopilotAPISource: &nobodyChose,
	}))

	// The precondition IS the hazard: before the pin, this axis is one a snapshot
	// would decline to carry.
	before, err := db.AgentRelaunchProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, before)
	require.False(t, launchValueWasChosen(before.CopilotAPISource),
		"precondition: nothing chose this drive yet, or the test is not measuring the "+
			"promotion it claims to")

	target, err := db.CopilotDriveTargetForConv(convID)
	require.NoError(t, err)
	_, _, err = writeCopilotDrive(convID, target, false)
	require.NoError(t, err)

	after, err := db.AgentRelaunchProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.True(t, launchValueWasChosen(after.CopilotAPISource),
		"an operator's pin IS a choice, and a snapshot that treats it as an observed "+
			"default drops it from the spec — the decision would survive in the record "+
			"and vanish from every deploy built out of it")
}

// TestTheDriveAttributionVocabularyIsShared guards the one seam the compare-and-set
// could not close by construction.
//
// The db package cannot import pkg/claude/agent — agent imports db — so the
// provenance term arrives as a caller-supplied string and db's own tests spell it
// as a literal. This asserts the literal and the constant have not drifted apart,
// which is the failure that would leave every db-side arm green while production
// wrote a term launchValueWasChosen does not recognise.
func TestTheDriveAttributionVocabularyIsShared(t *testing.T) {
	assert.Equal(t, "explicit", agent.ProvExplicit,
		"db's tests pin this spelling as a literal because they cannot import the "+
			"constant; changing the constant alone would leave them passing while "+
			"production wrote a term the snapshot's chosen-check does not know")
	explicit := agent.ProvExplicit
	assert.True(t, launchValueWasChosen(&explicit),
		"whatever the term is, the chosen-check has to accept it, or every operator "+
			"pin is classified as something nobody chose")
}
