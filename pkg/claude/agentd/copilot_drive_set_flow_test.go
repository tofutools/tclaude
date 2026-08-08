package agentd_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// TCL-1082 end to end: `tclaude agent set-drive <selector> send-keys|api`
// through the production CLI, the daemon mux and the durable records.
//
// The command's whole value is that an operator can believe what it reports, so
// these assert on the OPERATOR-VISIBLE output as well as on the record — a write
// that lands while the report misdescribes it is the failure this series keeps
// producing.

// runSetDriveCLI drives the production command through the flow mux.
func runSetDriveCLI(t *testing.T, f *testharness.Flow, selector, drive string) (string, string, int) {
	t.Helper()
	bridgeAgentClientToMux(t, f.Mux)
	stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
	rc := agent.RunSetDrive(&agent.SetDriveParams{Agent: selector, Drive: drive}, stdout, stderr)
	return stdout.String(), stderr.String(), rc
}

// setDriveRequest POSTs the drive through the daemon endpoint, for the
// assertions whose subject is the wire contract rather than the operator view.
func setDriveRequest(
	t *testing.T, f *testharness.Flow, selector, drive string,
) *httptest.ResponseRecorder {
	t.Helper()
	r := agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/agent/"+selector+"/copilot-drive", map[string]any{"drive": drive}))
	return testharness.Serve(f.Mux, r)
}

// TestSetDrive_TakesAnAgentOffTheAPIDriveDurably is the headline case: the agent
// this ticket exists for, and the report it has to make.
func TestSetDrive_TakesAnAgentOffTheAPIDriveDurably(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")
	haveCopilotAPIProfile(t, f)

	resp, _ := runSpawnCLI(t, f, &agent.SpawnParams{
		Group: "crew", Name: "copilot-worker", Harness: harness.CopilotName,
		CopilotAPI: true, Profile: copilotAPIProfile,
	})
	conv := resp.ConvID

	stdout, stderr, rc := runSetDriveCLI(t, f, conv, "send-keys")
	require.Equalf(t, 0, rc, "set-drive failed: %s", stderr)

	target, err := db.CopilotDriveTargetForConv(conv)
	require.NoError(t, err)
	assert.Equal(t, db.CopilotDriveRecordAgentProfile, target.Record)
	assert.False(t, target.Value, "the drive must be recorded off")

	assert.Contains(t, stdout, "send-keys")
	assert.Contains(t, stdout, string(db.CopilotDriveRecordAgentProfile),
		"the report must name WHICH record it wrote; two shapes of durably-off look "+
			"identical otherwise")
	assert.Contains(t, stdout, "edited",
		"this agent HAD a recorded drive, so the report must not claim it created one")
	assert.Contains(t, stdout, "not future members",
		"an operator who pins one member and expects the group's next spawn to follow "+
			"will be wrong, and quietly")
}

// TestSetDrive_PinningAnUnrecordedDriveSaysItCreatedTheRecord is the pin rather
// than the rollback: an agent that never chose the drive, where the point of
// writing send-keys is to stop a default profile answering for it at the next
// launch.
//
// "created" is the operator-facing half of that: it says nothing was recorded
// before, which is itself the diagnostic for a lower tier having been the thing
// speaking.
func TestSetDrive_PinningAnUnrecordedDriveSaysItCreatedTheRecord(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")

	resp, _ := spawnCopilot(t, f, "crew", map[string]any{"name": "copilot-worker"})
	conv := resp.ConvID

	// Reduce to the legacy shape: a Copilot agent with no recorded drive at all.
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
	require.Equal(t, db.CopilotDriveRecordNone, target.Record, "precondition: nothing recorded")

	stdout, stderr, rc := runSetDriveCLI(t, f, conv, "send-keys")
	require.Equalf(t, 0, rc, "set-drive failed: %s", stderr)

	after, err := db.CopilotDriveTargetForConv(conv)
	require.NoError(t, err)
	assert.Equal(t, db.CopilotDriveRecordAgentProfile, after.Record,
		"the pin must land in the record the ROUTER consults first, or it would report "+
			"success while changing nothing that routes")
	assert.False(t, after.Value)
	assert.Contains(t, stdout, "CREATED")
	assert.Contains(t, stdout, "free to answer for it",
		"the operator needs to learn that a default profile had been speaking")
}

// TestSetDrive_RefusesANonCopilotAgent: the drive is Copilot-only by design, so
// a Claude agent gets a refusal rather than a stray field written into a profile
// no launch would read it from.
func TestSetDrive_RefusesANonCopilotAgent(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	spawn := f.AsHuman().Spawn("crew", "claude-worker")

	// The stable refusal CODE is the daemon's contract, so it is asserted where
	// it lives; the CLI's job is to say the same thing in words an operator can
	// act on, so that is asserted on its output. Asserting the code against the
	// human text would only pin that the two happen to share a spelling.
	rec := setDriveRequest(t, f, spawn.ConvID, "send-keys")
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Contains(t, rec.Body.String(), "copilot_drive_wrong_harness")

	_, stderr, rc := runSetDriveCLI(t, f, spawn.ConvID, "send-keys")
	assert.NotEqual(t, 0, rc, "a non-Copilot agent must be refused")
	assert.Contains(t, stderr, harness.CopilotName+"-only posture",
		"the operator must be told why, not just that it failed")

	target, err := db.CopilotDriveTargetForConv(spawn.ConvID)
	require.NoError(t, err)
	assert.Equal(t, db.CopilotDriveRecordNone, target.Record,
		"a refusal must leave no record behind")
}

// TestSetDrive_RefusesEscalationWhileTheAgentIsRunning: --ui-server is a LAUNCH
// flag. A pane that came up without one has no server, so recording "api" for it
// would route this conversation's mail into a channel that does not exist — and
// an API-driven conversation HOLDS its mail rather than falling back to
// keystrokes, so the agent would silently stop receiving messages.
func TestSetDrive_RefusesEscalationWhileTheAgentIsRunning(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")

	resp, _ := spawnCopilot(t, f, "crew", map[string]any{"name": "copilot-worker"})

	rec := setDriveRequest(t, f, resp.ConvID, "api")
	require.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "copilot_drive_needs_relaunch")

	_, stderr, rc := runSetDriveCLI(t, f, resp.ConvID, "api")
	assert.NotEqual(t, 0, rc, "escalating a running agent must be refused")
	assert.Contains(t, stderr, "Stop the agent",
		"the refusal must name the remedy, or it converts a silent bad state into a "+
			"loud dead end")

	target, err := db.CopilotDriveTargetForConv(resp.ConvID)
	require.NoError(t, err)
	assert.False(t, target.Value, "the refused escalation must not have been recorded")
}

// TestSetDrive_ReportsANoOpAsUnchanged: an operator who runs the rollback twice
// must not be told the second run changed something. A no-op reported as a
// change is a small lie that costs a debugging session later.
func TestSetDrive_ReportsANoOpAsUnchanged(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")
	haveCopilotAPIProfile(t, f)

	// Spawned ON the drive deliberately. A plain Copilot spawn already freezes
	// copilot_api=false, so the FIRST run would land on the no-op branch too and
	// the test would assert "unchanged" after "unchanged" — never observing the
	// change-then-no-op sequence it claims. That is what the first version of this
	// test did, and it passed.
	resp, _ := runSpawnCLI(t, f, &agent.SpawnParams{
		Group: "crew", Name: "copilot-worker", Harness: harness.CopilotName,
		CopilotAPI: true, Profile: copilotAPIProfile,
	})

	stdout, stderr, rc := runSetDriveCLI(t, f, resp.ConvID, "send-keys")
	require.Equalf(t, 0, rc, "first run failed: %s", stderr)
	require.Contains(t, stdout, "→ send-keys",
		"the first run must be a real CHANGE, or the second proves nothing")
	require.NotContains(t, stdout, "unchanged")

	stdout, stderr, rc = runSetDriveCLI(t, f, resp.ConvID, "send-keys")
	require.Equalf(t, 0, rc, "second run failed: %s", stderr)
	assert.Contains(t, stdout, "unchanged")
}

// TestSetDrive_SeedsTheConversationFallbackForAnAgentlessConversation covers the
// one branch of writeCopilotDrive nobody had run: a conversation with no stable
// agent row — a clone, or a plain `session new` — whose drive can only live in
// the conversation fallback.
//
// It is also the branch most likely to be "simplified" later, because it reads
// like the agent branch with a different setter.
func TestSetDrive_SeedsTheConversationFallbackForAnAgentlessConversation(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")
	haveCopilotAPIProfile(t, f)

	resp, _ := runSpawnCLI(t, f, &agent.SpawnParams{
		Group: "crew", Name: "copilot-worker", Harness: harness.CopilotName,
		CopilotAPI: true, Profile: copilotAPIProfile,
	})
	// A clone is a NEW agent whose drive lives only in the conversation fallback,
	// which is exactly the shape this branch exists for.
	cloned := f.AsHuman().CloneFresh(resp.ConvID)
	require.NotEmptyf(t, cloned.NewConv, "no clone: %s", cloned.Raw)

	target, err := db.CopilotDriveTargetForConv(cloned.NewConv)
	require.NoError(t, err)
	require.Equal(t, db.CopilotDriveRecordConversationFallback, target.Record,
		"precondition: a clone's drive must live in the conversation fallback, or this "+
			"measures the agent branch")
	require.True(t, target.Value)

	stdout, stderr, rc := runSetDriveCLI(t, f, cloned.NewConv, "send-keys")
	require.Equalf(t, 0, rc, "set-drive on a clone failed: %s", stderr)

	after, err := db.CopilotDriveTargetForConv(cloned.NewConv)
	require.NoError(t, err)
	assert.Equal(t, db.CopilotDriveRecordConversationFallback, after.Record)
	assert.False(t, after.Value, "the clone must be off the drive durably")
	assert.Contains(t, stdout, string(db.CopilotDriveRecordConversationFallback),
		"the operator must be told it is the CONVERSATION record that answers here — "+
			"the shape that an agent profile could later outvote")
}

// TestSetDrive_ReadBackNamesTheDeciderNotJustTheValue: the GET direction exists
// so an operator can ask "which record decides this, and what does it say"
// BEFORE writing — the question TCL-1082 says an operator should not have to
// hold in their head.
func TestSetDrive_ReadBackNamesTheDeciderNotJustTheValue(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")
	haveCopilotAPIProfile(t, f)

	resp, _ := runSpawnCLI(t, f, &agent.SpawnParams{
		Group: "crew", Name: "copilot-worker", Harness: harness.CopilotName,
		CopilotAPI: true, Profile: copilotAPIProfile,
	})

	r := agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet,
		"/v1/agent/"+resp.ConvID+"/copilot-drive", nil))
	rec := testharness.Serve(f.Mux, r)
	require.Equalf(t, http.StatusOK, rec.Code, "read body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"drive":"api"`)
	assert.Contains(t, rec.Body.String(), `"record":"agent profile"`)
}
