package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// TCL-1082. A durable "take this agent off the API drive" writes copilot_api =
// false into the stable agent's relaunch profile. For that write to DO
// anything, the recorded false has to beat the group default profile tier at
// the agent's NEXT launch — and that relation was reasoned about rather than
// exercised. If a group default can still speak over a pinned false, the pin is
// theatre and the operator gets the worst possible outcome for a rollback path:
// one they believe worked.
//
// So these tests measure the relation instead of arguing it, and they measure
// it at the launch boundary — what the daemon threaded onto the forked `tclaude
// session new` — rather than at any record it consulted on the way.
//
// The arms, kept distinct on purpose (doctrine note 26: an author who probes
// one tier and reasons about three has one measurement and three predictions,
// and nothing in the green marks which is which):
//
//  1. SPAWN tier, positive control: the group default profile actually reaches
//     a launch. Without this the other arms could all be measuring a group
//     default that was never established — the silent-scenario trap of note 24.
//  2. RELAUNCH tier with a pin: resume, reincarnate and clone.
//  3. RELAUNCH tier without a pin, drive recorded ON: proves arm 2's assertion
//     discriminates rather than passing for an unrelated reason.
//  4. RELAUNCH tier with NOTHING recorded: the arm that says whether the group
//     default tier speaks at a relaunch at all.

// copilotAPIGroupProfile is a group default profile that turns the unverified
// drive ON for every member — the TCL-1090 shape this ticket's rollback exists
// to answer. trust_dir rides along because the API drive refuses an untrusted
// launch dir (TCL-1056) and these scenarios are about the drive, not trust.
const copilotAPIGroupProfile = "copilot-api-group-default"

// haveGroupDefaultAPIProfile puts the group on a default profile that selects
// the API drive, through the same daemon endpoints an operator uses.
func haveGroupDefaultAPIProfile(t *testing.T, f *testharness.Flow, group string) {
	t.Helper()
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": copilotAPIGroupProfile, "harness": harness.CopilotName,
		"copilot_api": true, "trust_dir": true,
	}).Code)
	require.Equal(t, http.StatusOK, setGroupProfile(t, f, group, copilotAPIGroupProfile).Code)

	// Assert the precondition rather than assume the setup established it: a
	// setter that no-ops leaves every later "the default did not win" reading
	// indistinguishable from "there was no default" (note 24).
	g, err := db.GetAgentGroupByName(group)
	require.NoError(t, err)
	require.NotNil(t, g)
	require.Equal(t, copilotAPIGroupProfile, g.DefaultProfile,
		"the group default profile must be stored, or every arm below measures nothing")
}

// relaunchOffline marks a conversation's live pane dead so the next resume
// actually relaunches it, and returns the tmux session it retired.
func relaunchOffline(t *testing.T, f *testharness.Flow, convID string) {
	t.Helper()
	session, err := db.FindSessionByConvID(convID)
	require.NoError(t, err)
	require.NotNil(t, session)
	f.MarkOffline(session.TmuxSession)
}

// pinDriveOff performs the durable un-choose the ticket is about: the targeted
// compare-and-set against the stable agent's relaunch profile.
func pinDriveOff(t *testing.T, convID string) {
	t.Helper()
	target, err := db.CopilotDriveTargetForConv(convID)
	require.NoError(t, err)
	require.Equal(t, db.CopilotDriveRecordAgentProfile, target.Record,
		"a spawned Copilot agent's drive must live in the agent profile")
	ok, err := db.CompareAndSetAgentCopilotAPI(target.AgentID, false, target.Raw)
	require.NoError(t, err)
	require.True(t, ok, "the pin's compare-and-set must hold")

	after, err := db.CopilotDriveTargetForConv(convID)
	require.NoError(t, err)
	require.Equal(t, db.CopilotDriveRecordAgentProfile, after.Record)
	require.False(t, after.Value, "the pin must be readable before any launch is judged by it")
}

// TestCopilotDrivePin_GroupDefaultProfileReachesAnActualLaunch is the positive
// control every other arm here depends on. It proves the group default tier is
// live, selects the drive with nobody asking, and arrives at the launch — so a
// later "the group default did not win" is a measurement rather than an absent
// scenario.
func TestCopilotDrivePin_GroupDefaultProfileReachesAnActualLaunch(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")
	haveGroupDefaultAPIProfile(t, f, "crew")

	resp, _ := spawnCopilot(t, f, "crew", map[string]any{"name": "copilot-worker"})

	got, ok := f.World.SpawnCopilotAPI(resp.ConvID)
	require.True(t, ok, "no spawn recorded for conv %s", resp.ConvID)
	assert.True(t, got,
		"a group default profile carrying copilot_api must reach the launch of a "+
			"member who asked for nothing")

	recorded, err := db.AgentRelaunchProfileForConv(resp.ConvID)
	require.NoError(t, err)
	require.NotNil(t, recorded)
	require.NotNil(t, recorded.CopilotAPI)
	assert.True(t, *recorded.CopilotAPI, "the launch it won must be frozen on the agent")
}

// TestCopilotDrivePin_SurvivesAGroupDefaultAtEveryRelaunch is the merge-blocking
// measurement: an agent the group default put on the API drive, pinned off, and
// then relaunched through each of the three paths that recreate its pane.
//
// Resume, reincarnate and clone each assemble their own launch args, so a pin
// that holds on one says nothing about the others.
func TestCopilotDrivePin_SurvivesAGroupDefaultAtEveryRelaunch(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")
	haveGroupDefaultAPIProfile(t, f, "crew")

	resp, _ := spawnCopilot(t, f, "crew", map[string]any{"name": "copilot-worker"})
	conv := resp.ConvID
	born, ok := f.World.SpawnCopilotAPI(conv)
	require.True(t, ok)
	require.True(t, born, "the scenario needs an agent the group default actually put on the drive")

	pinDriveOff(t, conv)

	relaunchOffline(t, f, conv)
	r := f.AsHuman().Resume(conv)
	require.Equalf(t, "resumed", r.Action, "resume: %s", r.Raw)
	got, ok := f.World.SpawnCopilotAPI(conv)
	require.True(t, ok, "no resume-spawn recorded for conv %s", conv)
	assert.False(t, got, "a pinned drive must survive resume against a group default that says on")

	afterResume, err := db.CopilotDriveTargetForConv(conv)
	require.NoError(t, err)
	assert.False(t, afterResume.Value,
		"the resume must RE-RECORD the pin, or the next relaunch reads a record that lost it")

	reincarnated := f.AsHuman().Reincarnate(conv, "carry on")
	require.NotEmptyf(t, reincarnated.NewConv, "no successor: %s", reincarnated.Raw)
	got, ok = f.World.SpawnCopilotAPI(reincarnated.NewConv)
	require.True(t, ok, "no spawn recorded for successor conv %s", reincarnated.NewConv)
	assert.False(t, got, "a successor must inherit the pin, not the group default")

	cloned := f.AsHuman().CloneFresh(reincarnated.NewConv)
	require.NotEmptyf(t, cloned.NewConv, "no clone: %s", cloned.Raw)
	got, ok = f.World.SpawnCopilotAPI(cloned.NewConv)
	require.True(t, ok, "no spawn recorded for clone conv %s", cloned.NewConv)
	assert.False(t, got, "a clone is a NEW agent in the same group — the group default must not "+
		"re-acquire the drive its source was pinned off")
}

// TestCopilotDrivePin_WithoutThePinTheRecordedDriveWins is the discriminator for
// the arm above. Same group, same default, same relaunches — only the pin
// removed. If this also came up on send-keys, the previous test would be
// passing for a reason that has nothing to do with the pin.
func TestCopilotDrivePin_WithoutThePinTheRecordedDriveWins(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")
	haveGroupDefaultAPIProfile(t, f, "crew")

	resp, _ := spawnCopilot(t, f, "crew", map[string]any{"name": "copilot-worker"})
	conv := resp.ConvID

	relaunchOffline(t, f, conv)
	r := f.AsHuman().Resume(conv)
	require.Equalf(t, "resumed", r.Action, "resume: %s", r.Raw)
	got, ok := f.World.SpawnCopilotAPI(conv)
	require.True(t, ok, "no resume-spawn recorded for conv %s", conv)
	assert.True(t, got, "an unpinned agent must still resume onto the drive it was born with")

	reincarnated := f.AsHuman().Reincarnate(conv, "carry on")
	require.NotEmptyf(t, reincarnated.NewConv, "no successor: %s", reincarnated.Raw)
	got, ok = f.World.SpawnCopilotAPI(reincarnated.NewConv)
	require.True(t, ok, "no spawn recorded for successor conv %s", reincarnated.NewConv)
	assert.True(t, got, "an unpinned successor must inherit the drive")
}

// TestCopilotDrivePin_UnrecordedDriveAtRelaunch answers the question the pin
// rests on, in the one shape where the answer is not already determined by a
// record: nothing durable says anything about this conversation's drive, and the
// group default profile says ON.
//
// Whichever way it falls, it is load-bearing. If the group default wins here,
// then a relaunch does consult that tier and the pin is what stands between an
// operator's rollback and a member the group re-enrols on the unverified drive.
// If it does not win, then the pin cannot be beaten by a group default at
// relaunch at all — the protection is structural, and the pin's job is
// narrower than "beat the lower tier".
func TestCopilotDrivePin_UnrecordedDriveAtRelaunch(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")
	haveGroupDefaultAPIProfile(t, f, "crew")

	resp, _ := spawnCopilot(t, f, "crew", map[string]any{"name": "copilot-worker"})
	conv := resp.ConvID

	// Reduce the agent to the legacy shape: born before the field existed, so
	// no record answers for its drive. Both records are stripped, because the
	// resolve consults them in order.
	agentID, err := db.AgentIDForConv(conv)
	require.NoError(t, err)
	require.NotEmpty(t, agentID)
	profile, err := db.AgentRelaunchProfileForConv(conv)
	require.NoError(t, err)
	require.NotNil(t, profile)
	profile.CopilotAPI = nil
	require.NoError(t, db.SetAgentRelaunchProfile(agentID, *profile))
	conversation, err := db.ConversationResumeProfileForConv(conv)
	require.NoError(t, err)
	require.NotNil(t, conversation)
	if conversation.FallbackRelaunch != nil {
		conversation.FallbackRelaunch.CopilotAPI = nil
		require.NoError(t, db.SetConversationResumeProfile(conv, *conversation))
	}

	// Prove the scenario before measuring it: nothing records a drive now.
	target, err := db.CopilotDriveTargetForConv(conv)
	require.NoError(t, err)
	require.Equal(t, db.CopilotDriveRecordNone, target.Record,
		"the arm is only meaningful with NOTHING recorded; got %s", target.Record)

	relaunchOffline(t, f, conv)
	r := f.AsHuman().Resume(conv)
	require.Equalf(t, "resumed", r.Action, "resume: %s", r.Raw)
	got, ok := f.World.SpawnCopilotAPI(conv)
	require.True(t, ok, "no resume-spawn recorded for conv %s", conv)
	t.Logf("MEASURED: unrecorded drive + group default profile copilot_api=true "+
		"→ relaunch threaded copilot_api=%v", got)
	assert.False(t, got,
		"a relaunch resolves the drive from the durable record alone; the group "+
			"default profile tier is a SPAWN-time tier and must not re-acquire the "+
			"drive for an agent whose record says nothing")

	reincarnated := f.AsHuman().Reincarnate(conv, "carry on")
	require.NotEmptyf(t, reincarnated.NewConv, "no successor: %s", reincarnated.Raw)
	got, ok = f.World.SpawnCopilotAPI(reincarnated.NewConv)
	require.True(t, ok, "no spawn recorded for successor conv %s", reincarnated.NewConv)
	assert.False(t, got, "nor may a successor acquire the drive from the group default")
}

// TestCopilotDrivePin_UnrecordedDriveAtRelaunchWithAGlobalDefault is the arm
// above for the OTHER ambient tier.
//
// It exists because the two tiers are resolved by the same call and it would
// cost nothing to assume they behave alike — which is exactly the assumption
// note 26 says goes wrong. One tier exercised and one argued is one
// measurement and one prediction, and the green would not say which is which.
func TestCopilotDrivePin_UnrecordedDriveAtRelaunchWithAGlobalDefault(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")

	// Born plainly, on send-keys, with no ambient tier in play yet.
	resp, _ := spawnCopilot(t, f, "crew", map[string]any{"name": "copilot-worker"})
	conv := resp.ConvID
	born, ok := f.World.SpawnCopilotAPI(conv)
	require.True(t, ok)
	require.False(t, born, "the arm needs an agent that was NOT born on the drive")

	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "copilot-api-global-default", "harness": harness.CopilotName,
		"copilot_api": true, "trust_dir": true,
	}).Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "copilot-api-global-default").Code)

	// The positive control this arm needs and the group arm gets for free. Arm 4
	// is self-discriminating — its agent is born ON the drive and must come up
	// OFF it — whereas here both the before and after readings are "send-keys",
	// so a stale or never-taken reading would look exactly like a pass. Proving
	// the GLOBAL tier reaches a launch at all is what makes the silence below a
	// measurement.
	control, _ := spawnCopilot(t, f, "crew", map[string]any{"name": "control-worker"})
	controlDrive, ok := f.World.SpawnCopilotAPI(control.ConvID)
	require.True(t, ok, "no spawn recorded for control conv %s", control.ConvID)
	require.True(t, controlDrive,
		"the global default profile must reach a fresh spawn, or this arm's send-keys "+
			"result is an absent scenario rather than a measurement")

	// The same reduction to "nothing recorded" as the group arm, so the global
	// tier is the only thing left that could speak.
	agentID, err := db.AgentIDForConv(conv)
	require.NoError(t, err)
	profile, err := db.AgentRelaunchProfileForConv(conv)
	require.NoError(t, err)
	require.NotNil(t, profile)
	profile.CopilotAPI = nil
	require.NoError(t, db.SetAgentRelaunchProfile(agentID, *profile))
	conversation, err := db.ConversationResumeProfileForConv(conv)
	require.NoError(t, err)
	require.NotNil(t, conversation)
	if conversation.FallbackRelaunch != nil {
		conversation.FallbackRelaunch.CopilotAPI = nil
		require.NoError(t, db.SetConversationResumeProfile(conv, *conversation))
	}
	target, err := db.CopilotDriveTargetForConv(conv)
	require.NoError(t, err)
	require.Equal(t, db.CopilotDriveRecordNone, target.Record,
		"the arm is only meaningful with NOTHING recorded; got %s", target.Record)

	relaunchOffline(t, f, conv)
	r := f.AsHuman().Resume(conv)
	require.Equalf(t, "resumed", r.Action, "resume: %s", r.Raw)
	got, ok := f.World.SpawnCopilotAPI(conv)
	require.True(t, ok, "no resume-spawn recorded for conv %s", conv)
	t.Logf("MEASURED: unrecorded drive + global default profile copilot_api=true "+
		"→ relaunch threaded copilot_api=%v", got)
	assert.False(t, got,
		"the global default profile is a SPAWN-time tier too, and must not move an "+
			"existing agent onto the unverified drive at a relaunch")
}
