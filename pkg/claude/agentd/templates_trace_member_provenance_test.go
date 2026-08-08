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

// These guard the DURABLE-RECORD → TRACED-LAUNCH boundary, which is where a
// from-group snapshot decides whether a member's launch field was CHOSEN or
// merely OBSERVED (TCL-1090).
//
// They deliberately do NOT live in templates_snapshot_merge_fields_test.go.
// Every guard in that file hand-builds the traced db.SpawnProfile and asks what
// mergeSnapshotInlineProfile does with it — so all four take the traced profile
// as GROUND TRUTH. The defect here is a lie IN the traced profile: by the time
// the merge sees copilot_api:false it is already wrong, and the merge carries it
// forward perfectly correctly. A fifth test in that file would have joined the
// blind spot rather than closed it. traceMemberLaunch had no direct test at all.
//
// For the same reason these drive the REAL minting path — relaunchProfileForSpawn
// — rather than hand-writing the durable record. The whole defect is a
// disagreement between what that function writes and what traceMemberLaunch
// reads, and a hand-written fixture is free to encode the reader's assumption
// and agree with itself.
//
// The trap being pinned: relaunchProfileForSpawn freezes CopilotAPI NON-NIL for
// every Copilot launch, deliberately and correctly — a relaunch has to replay
// "this agent is on send-keys" as a KNOWN posture rather than an unknown one a
// later profile edit could fill in differently. So non-nil means "this launch
// had a posture", while CopilotAPISet means "someone CHOSE one". Two questions,
// one spelling.

// haveTracedMember records the session/agent rows traceMemberLaunch needs. The
// harness is a parameter because each field under test is only legal on the
// harness that owns it — trace a field onto the wrong one and its validator
// rejects it, which makes a "not chosen" assertion pass for a reason that has
// nothing to do with provenance.
func haveTracedMember(t *testing.T, convID, harnessName string) string {
	t.Helper()
	setupTestDB(t)
	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, session.SaveSessionState(&session.SessionState{
		ID: convID, TmuxSession: convID, ConvID: convID,
		Harness: harnessName, Status: session.StatusIdle,
		Cwd: t.TempDir(),
	}))
	return agentID
}

func haveTracedCopilotMember(t *testing.T, convID string) string {
	t.Helper()
	return haveTracedMember(t, convID, harness.CopilotName)
}

// freezeCopilotLaunch runs the real birth-freeze for a Copilot launch and stores
// the result, so these tests read exactly what a spawn would have left behind.
func freezeCopilotLaunch(t *testing.T, agentID string, api bool, source string) {
	t.Helper()
	profile := relaunchProfileForSpawn(spawnParams{
		Harness: harness.CopilotName, CopilotAPI: api, CopilotAPISource: source,
	})
	require.NotNil(t, profile.CopilotAPI,
		"the freeze is unconditional for a Copilot launch; if this is nil the "+
			"premise of these tests has changed and they are asserting nothing")
	require.NoError(t, db.SetAgentRelaunchProfile(agentID, profile))
}

// The defect. A Copilot launch nobody steered resolves copilot_api to false at
// the harness default, and the freeze records it — correctly, for relaunch. The
// snapshot must not then re-tell that as an operator's decision.
func TestTraceMemberLaunchDoesNotReportAnObservedCopilotDefaultAsChosen(t *testing.T) {
	const convID = "ses_trace_prov_observed"
	agentID := haveTracedCopilotMember(t, convID)
	freezeCopilotLaunch(t, agentID, false, agent.ProvHarnessDefault)

	launch := traceMemberLaunch(convID)
	require.True(t, launch.Traced, "the member must be observed at all, or this asserts nothing")
	assert.False(t, launch.CopilotAPISet,
		"a copilot_api NOBODY CHOSE must not be recorded as a curated decision: the snapshot "+
			"pins it into a template-local profile, which outranks the group and global tiers, "+
			"so an observed default silently suppresses a group default saying true")
	assert.Contains(t, launch.ObservedNotChosen, "copilot_api",
		"and declining to pin it must be DISCLOSED — dropping an operator-visible fact "+
			"silently is the half of this that would ship unnoticed")
}

// The positive control, and the reason this pair is stated as a pair: the VALUE
// is identical to the case above. Only the provenance differs. A fix that simply
// stopped reporting copilot_api, or always reported it, satisfies exactly one of
// these two and fails the other.
func TestTraceMemberLaunchReportsACuratedCopilotOptOutAsChosen(t *testing.T) {
	const convID = "ses_trace_prov_curated"
	agentID := haveTracedCopilotMember(t, convID)
	freezeCopilotLaunch(t, agentID, false, agent.ProvGroupProfileSource("copilot-send-keys"))

	launch := traceMemberLaunch(convID)
	require.True(t, launch.Traced)
	assert.True(t, launch.CopilotAPISet,
		"a curated send-keys opt-out is exactly the per-agent decision this feature exists "+
			"for; it must still speak and still suppress a group default")
	assert.False(t, launch.CopilotAPI, "and it must carry the value that was chosen")
	assert.NotContains(t, launch.ObservedNotChosen, "copilot_api",
		"nothing was dropped, so nothing should be disclosed as dropped")
}

// A record written before the attribution existed means UNKNOWN, never
// "unchosen" — and the direction is not a wash.
//
// Reading unknown as unchosen would drop a genuine send-keys opt-out made before
// this field shipped, and a group default saying true would then move that agent
// onto the API drive the operator had deliberately kept it off. Reading it as
// chosen merely leaves the pre-existing wart in place for old records. One of
// those errors silently acquires an unverified drive; the other is the status quo.
func TestTraceMemberLaunchTreatsALegacyRecordWithNoAttributionAsChosen(t *testing.T) {
	const convID = "ses_trace_prov_legacy"
	agentID := haveTracedCopilotMember(t, convID)

	// Hand-written on purpose: this shape is what a PREVIOUS BINARY wrote, and no
	// current code path can produce it.
	sendKeys := false
	require.NoError(t, db.SetAgentRelaunchProfile(agentID, db.AgentRelaunchProfile{
		Version: db.RelaunchProfileVersion, CopilotAPI: &sendKeys,
	}))

	launch := traceMemberLaunch(convID)
	require.True(t, launch.Traced)
	assert.True(t, launch.CopilotAPISet,
		"nil attribution is a legacy record, not evidence that nobody chose: treating it as "+
			"unchosen would drop a real pre-existing opt-out and let a group default move "+
			"that agent onto the unverified API drive")
	assert.NotContains(t, launch.ObservedNotChosen, "copilot_api")
}

// The same contract for the sibling field, which shares the defect exactly:
// relaunchProfileForSpawn freezes SSHWorkaround non-nil for EVERY launch, not
// even harness-gated.
func TestTraceMemberLaunchDoesNotReportAnObservedSSHWorkaroundDefaultAsChosen(t *testing.T) {
	const convID = "ses_trace_prov_ssh"
	// Codex, because ssh_workaround is a Codex capability: on any other harness
	// ResolveSSHWorkaround rejects it and the "not chosen" assertion below would
	// pass without the provenance gate ever being consulted.
	agentID := haveTracedMember(t, convID, harness.CodexName)

	profile := relaunchProfileForSpawn(spawnParams{
		Harness: harness.CodexName, SSHWorkaround: true,
		SSHWorkaroundSource: agent.ProvHarnessDefault,
	})
	require.NotNil(t, profile.SSHWorkaround, "the freeze is unconditional for every launch")
	require.NoError(t, db.SetAgentRelaunchProfile(agentID, profile))

	// Positive control on the harness itself: prove this member's ssh_workaround
	// IS traceable here, so the assertion below can only be about provenance.
	require.True(t, traceMemberLaunch(convID).Traced)

	launch := traceMemberLaunch(convID)
	assert.False(t, launch.SSHWorkaroundSet,
		"an ssh_workaround nobody chose must not become a template specification line")
	assert.Contains(t, launch.ObservedNotChosen, "ssh_workaround")
}
