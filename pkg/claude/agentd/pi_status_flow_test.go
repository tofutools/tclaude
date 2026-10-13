package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func spawnPi(t *testing.T, f *testharness.Flow, group string, body map[string]any) (
	testharness.SpawnResp, *testharness.PiSim,
) {
	t.Helper()
	body["harness"] = harness.PiName
	var resp testharness.SpawnResp
	if _, ok := body["approval"]; ok {
		resp = f.AsHuman().SpawnWith(group, body)
		require.Equalf(t, http.StatusOK, resp.Code, "pi spawn body=%s", resp.Raw)
	} else {
		// Outside tclaude's sandbox pi's default auto posture runs
		// unconfined, so the human authorizes it in the dashboard dialog.
		body["allow_unenforced_sandbox"] = true
		served := testharness.Serve(agentd.BuildDashboardHandlerForTest(), dashboardOverrideRequest(
			t, http.MethodPost, "/api/groups/"+group+"/spawn", body))
		require.Equalf(t, http.StatusOK, served.Code, "pi spawn body=%s", served.Body.String())
		testharness.DecodeJSON(t, served, &resp)
	}
	sim := f.World.Pis.GetByConvID(resp.ConvID)
	require.NotNil(t, sim, "the spawn should have built a pi pane simulator")
	return resp, sim
}

func piMemberStatus(t *testing.T, group, convID string) string {
	t.Helper()
	m := findDashMember(fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest()), group, convID)
	require.NotNil(t, m, "agent %s missing from group %s", convID, group)
	return m.State.Status
}

func typeInto(sim *testharness.PiSim, text string) {
	sim.Receive(text)
	sim.Receive("Enter")
}

// A pi pane reports status only through tclaude's extension, so the launch
// must load it, and the extension's events must drive the dashboard through
// a whole turn: prompt, tool, a confirmation dialog, and the settled reply.
func TestPiStatus_TurnLifecycle(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	resp, sim := spawnPi(t, f, "crew", map[string]any{
		"name":            "pi-worker",
		"initial_message": "start work",
	})
	launch := sim.Launch()
	require.True(t, launch.LoadsStatusExtension(), "launch must load the status extension: %+v", launch)
	assert.Equal(t, resp.ConvID, launch.SessionID, "the daemon presets pi's session id")

	assert.Equal(t, session.StatusWorking, piMemberStatus(t, "crew", resp.ConvID), "the launch prompt starts a turn")

	require.NoError(t, sim.OpenDialog("confirm", "Allow bash?"))
	assert.Equal(t, session.StatusAwaitingPermission, piMemberStatus(t, "crew", resp.ConvID))

	require.NoError(t, sim.UseTool("bash", map[string]any{"command": "go test ./..."}))
	assert.Equal(t, session.StatusWorking, piMemberStatus(t, "crew", resp.ConvID),
		"an approved tool leaves the permission wait")

	require.NoError(t, sim.Reply("Done."))
	assert.Equal(t, session.StatusIdle, piMemberStatus(t, "crew", resp.ConvID), "the settled turn is idle")

	require.NoError(t, sim.OpenDialog("select", "Pick one"))
	assert.Equal(t, session.StatusAwaitingInput, piMemberStatus(t, "crew", resp.ConvID),
		"a non-confirmation dialog asks for input")
}

// Escape aborts a running turn (pi still settles), and a manual /compact on an
// idle pane leaves it idle.
func TestPiStatus_AbortAndCompact(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	resp, sim := spawnPi(t, f, "crew", map[string]any{"name": "pi-worker"})

	typeInto(sim, "do something long")
	assert.Equal(t, session.StatusWorking, piMemberStatus(t, "crew", resp.ConvID))
	sim.Receive("Escape")
	assert.Equal(t, session.StatusIdle, piMemberStatus(t, "crew", resp.ConvID), "an aborted turn settles idle")

	typeInto(sim, "/compact")
	assert.Equal(t, 1, sim.Compactions())
	assert.Equal(t, session.StatusIdle, piMemberStatus(t, "crew", resp.ConvID), "a manual compaction returns to idle")

	typeInto(sim, "/name renamed")
	assert.Equal(t, "renamed", sim.Name())
	assert.Equal(t, session.StatusIdle, piMemberStatus(t, "crew", resp.ConvID), "/name is not a model turn")
}

// /clone moves the pane to a new session in-process; tclaude must follow it to
// the new conversation, as it does for Claude Code's /clear.
func TestPiStatus_CloneRotatesConversation(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	resp, sim := spawnPi(t, f, "crew", map[string]any{"name": "pi-worker"})
	oldPath := sim.Path()

	typeInto(sim, "/clone")
	require.NotEqual(t, resp.ConvID, sim.ConvID, "/clone rotates the session id")
	require.NotEqual(t, oldPath, sim.Path())
	assert.Contains(t, sim.Path(), "_"+sim.ConvID+".jsonl")

	rows, err := session.ListSessionStates()
	require.NoError(t, err)
	var tracked string
	for _, row := range rows {
		if row.ID == resp.TmuxSession {
			tracked = row.ConvID
		}
	}
	assert.Equal(t, sim.ConvID, tracked, "the session row follows the pane to the cloned conversation")
	assert.Equal(t, "pi-worker", sim.Name(), "tclaude restores the agent's title with pi's own /name")
	assert.NotEqual(t, session.StatusWorking, piMemberStatus(t, "crew", sim.ConvID),
		"restoring the title is not a model turn")

	typeInto(sim, "continue in the clone")
	assert.Equal(t, session.StatusWorking, piMemberStatus(t, "crew", sim.ConvID))
}

// A soft stop exits through pi's /quit, which the extension reports as the
// session ending.
func TestPiStatus_SoftStopQuits(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	resp, sim := spawnPi(t, f, "crew", map[string]any{"name": "pi-worker"})

	f.AssertSoftStopped(f.Stop(resp.ConvID, false))
	assert.False(t, sim.IsAlive(), "the soft stop must quit the pane")
}

// An experimental harness needs the operator's opt-in: without it the daemon
// refuses the spawn, and says how to opt in. No registered harness is
// experimental today; pi, the last to graduate, stands in.
func TestSpawn_ExperimentalHarnessRequiresOptIn(t *testing.T) {
	t.Setenv(harness.ExperimentalHarnessesEnvVar, "")
	standIn := *harness.MustGet(harness.PiName)
	standIn.Experimental = true
	t.Cleanup(harness.RegisterForTest(&standIn))
	t.Cleanup(harness.SetExperimentalHarnessEnabledForTest(harness.PiName, false))
	f := newFlow(t)
	f.HaveGroup("crew")
	resp := f.AsHuman().SpawnWith("crew", map[string]any{"name": "pi-worker", "harness": harness.PiName})
	assert.NotEqual(t, http.StatusOK, resp.Code)
	assert.Contains(t, string(resp.Raw), "experimental")
	assert.Nil(t, f.World.Pis.GetByConvID(resp.ConvID))
}

// pi has no sandbox of its own, so its unattended auto posture launches only
// inside tclaude's sandbox or with the human's explicit allowance; read-only
// acts on nothing and needs neither.
func TestPiSpawn_AutoNeedsConfinementOrHumanAllowance(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	refused := f.AsHuman().SpawnWith("crew", map[string]any{"name": "pi-auto", "harness": harness.PiName})
	assert.Equal(t, http.StatusForbidden, refused.Code, "body=%s", refused.Raw)
	assert.Contains(t, string(refused.Raw), "unconfined_approval")
	assert.Contains(t, string(refused.Raw), "Allow launch with unenforced rules")

	_, sim := spawnPi(t, f, "crew", map[string]any{"name": "pi-reader", "approval": harness.PiApprovalReadOnly})
	assert.Equal(t, "read,grep,find,ls", sim.Launch().Tools, "read-only renders pi's read tools only")

	_, sim = spawnPi(t, f, "crew", map[string]any{"name": "pi-allowed"})
	assert.Equal(t, "", sim.Launch().Tools, "auto exposes pi's full tool set")
}

// tclaude's sandbox is pi's boundary: inside it the default auto posture
// launches without the human's allowance, pi's own mode is the recorded `off`,
// and a resume relaunches under the same wall.
func TestPiSpawn_TclaudeLayerConfinesAuto(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	resp, sim := spawnPi(t, f, "crew", map[string]any{
		"name":                   "pi-walled",
		"approval":               harness.PiApprovalAuto,
		"sandbox_implementation": "tclaude-layer",
	})
	assert.Equal(t, "", sim.Launch().Tools, "auto exposes pi's full tool set")
	assert.True(t, sim.Launch().LoadsStatusExtension())
	got, ok := f.World.SpawnSandboxImplementation(resp.ConvID)
	require.True(t, ok)
	assert.Equal(t, "tclaude-layer", got)
	assertSandboxLayerCalls(t, f, testharness.SandboxLayerInteractive)

	row, err := db.LoadSession(resp.Label)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, harness.PiSandboxOff, row.HarnessBuiltinMode)

	f.AssertSoftStopped(f.AsHuman().Stop(resp.ConvID, false))
	require.Equal(t, http.StatusOK, f.Resume(resp.ConvID).Code)
	relaunched := f.World.Pis.GetByConvID(resp.ConvID)
	require.NotNil(t, relaunched)
	assert.True(t, relaunched.Launch().LoadsStatusExtension(), "a resume keeps the status extension")
}
