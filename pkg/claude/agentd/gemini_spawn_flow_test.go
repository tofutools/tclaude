package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Daemon-spawned Gemini CLI panes, driven through the production spawn,
// rename, compact, stop and resume paths against a simulator that writes the
// CLI's own chat-file layout (pkg/testharness/gemini_sim.go).
//
// The simulator is launched from the REAL spawner's output, and the
// conversation reads go through the REAL Gemini ConvStore, so a flag respelling
// or a storage-layout drift fails here. No Gemini account was available when
// this was written: the simulator's behavior is read from the CLI source at
// harness.GeminiPinnedVersion, not measured from a live pane.

func spawnGemini(t *testing.T, f *testharness.Flow, group string, body map[string]any) (
	testharness.SpawnResp, *testharness.GeminiSim,
) {
	t.Helper()
	body["harness"] = harness.GeminiName
	resp := f.AsHuman().SpawnWith(group, body)
	require.Equalf(t, http.StatusOK, resp.Code, "gemini spawn body=%s", resp.Raw)
	sim := f.World.Geminis.GetByConvID(resp.ConvID)
	require.NotNil(t, sim, "the spawn should have built a Gemini pane simulator")
	return resp, sim
}

func geminiLaunchOf(t *testing.T, f *testharness.Flow, convID string) testharness.GeminiLaunch {
	t.Helper()
	cmd, ok := f.World.GeminiLaunchCommand(convID)
	require.Truef(t, ok, "no Gemini launch recorded for %s", convID)
	launch, err := testharness.ParseGeminiLaunch(cmd)
	require.NoErrorf(t, err, "the production spawner produced a launch the CLI would reject: %s", cmd)
	return launch
}

func geminiHarness(t *testing.T) *harness.Harness {
	t.Helper()
	h, err := harness.Resolve(harness.GeminiName)
	require.NoError(t, err)
	return h
}

// TestGeminiSpawn_LaunchEnrollmentIdentity: the daemon presets the conv id,
// the pane is launched under exactly it with the briefing in the argv, and the
// name lands in tclaude's title overlay because Gemini has no name flag.
// Nothing is typed into the pane.
func TestGeminiSpawn_LaunchEnrollmentIdentity(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	resp, sim := spawnGemini(t, f, "crew", map[string]any{
		"name":            "gemini-worker",
		"initial_message": "Investigate the flaky deploy job and report back",
		"model":           "gemini-3.1-pro-preview",
	})

	row, err := db.LoadSession(resp.Label)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, resp.ConvID, row.ConvID)
	assert.Equal(t, harness.GeminiName, row.Harness)

	launch := geminiLaunchOf(t, f, resp.ConvID)
	assert.Equal(t, resp.ConvID, launch.SessionID, "the pane must be launched under the enrolled id")
	assert.Empty(t, launch.ResumeID)
	assert.Equal(t, "gemini-3.1-pro-preview", launch.Model)
	assert.Contains(t, launch.InitialPrompt, "Investigate the flaky deploy job",
		"the briefing rides the launch argv rather than being typed into the pane")
	assert.Equal(t, resp.ConvID, sim.ConvID)

	// The production cold-read path finds the conversation in Gemini's own
	// chat files and overlays the launch name as its title. (The simulator
	// stands in for `session new` in writing that name to the index; this
	// asserts that the store reads it back, not that session new writes it.)
	h := geminiHarness(t)
	exists, err := h.Convs.Exists(resp.ConvID, sim.Cwd)
	require.NoError(t, err)
	assert.True(t, exists, "the ConvStore must find the pane's session file")
	title, err := h.Convs.Title(resp.ConvID)
	require.NoError(t, err)
	assert.Equal(t, "gemini-worker", title)

	assert.Empty(t, f.World.Tmux.Sent(), "a launch-enrolled Gemini spawn must not send-keys")
}

// TestGeminiSpawn_RenameUsesTheTitleOverlay: Gemini CLI has no rename command,
// so a rename is written to tclaude's title overlay and nothing is typed.
func TestGeminiSpawn_RenameUsesTheTitleOverlay(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	resp, _ := spawnGemini(t, f, "crew", map[string]any{
		"name":            "gemini-worker",
		"initial_message": "start work",
	})

	r := f.AsHuman().Rename(resp.ConvID, "renamed-worker")
	require.Equalf(t, http.StatusOK, r.Code, "rename body=%s", r.Raw)

	title, err := geminiHarness(t).Convs.Title(resp.ConvID)
	require.NoError(t, err)
	assert.Equal(t, "renamed-worker", title)
	for _, sk := range f.World.Tmux.Sent() {
		assert.NotContains(t, sk.Text, "rename", "Gemini has no in-pane rename; nothing may be typed")
	}
}

// TestGeminiSpawn_CompactTypesCompress: compaction types the canonical
// `/compress` command into the pane.
func TestGeminiSpawn_CompactTypesCompress(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	resp, sim := spawnGemini(t, f, "crew", map[string]any{
		"name":            "gemini-worker",
		"initial_message": "start work",
	})
	sim.WriteGeminiReply("on it", "gemini-3-flash")

	c := f.AsHuman().Compact(resp.ConvID)
	require.Equalf(t, http.StatusOK, c.Code, "compact body=%s", c.Raw)
	assert.Equal(t, 1, sim.Compressions())
	assert.True(t, sim.IsAlive())
}

// TestGeminiSpawn_StopThenResumeReopensTheSameConversation: a soft stop
// closes the pane through the CLI's own exit path, and a resume relaunches it
// with `--resume <full id>` against the SAME session file.
func TestGeminiSpawn_StopThenResumeReopensTheSameConversation(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	resp, sim := spawnGemini(t, f, "crew", map[string]any{
		"name":            "gemini-worker",
		"initial_message": "start work",
	})
	sim.WriteGeminiReply("done", "gemini-3-flash")

	f.AssertSoftStopped(f.AsHuman().Stop(resp.ConvID, false))
	assert.False(t, sim.IsAlive(), "the soft exit must close the Gemini pane")

	resume := f.Resume(resp.ConvID)
	require.Equalf(t, http.StatusOK, resume.Code, "resume body=%s", resume.Raw)

	relaunch := geminiLaunchOf(t, f, resp.ConvID)
	assert.Equal(t, resp.ConvID, relaunch.ResumeID, "a relaunch must name the full conversation id")
	assert.Empty(t, relaunch.SessionID, "--resume and --session-id are mutually exclusive")

	resumed := f.World.Geminis.GetByConvID(resp.ConvID)
	require.NotNil(t, resumed)
	launchCmd, _ := f.World.GeminiLaunchCommand(resp.ConvID)
	require.NotSamef(t, sim, resumed,
		"the resumed pane never started (resume body=%s, launch=%s, spawn cwd=%s)", resume.Raw, launchCmd, sim.Cwd)
	assert.Truef(t, resumed.IsAlive(), "the resumed pane died (launch=%s, cwd=%s)", launchCmd, resumed.Cwd)

	// Still one conversation, still titled by the overlay.
	h := geminiHarness(t)
	convs, err := h.Convs.ListConvs(sim.Cwd)
	require.NoError(t, err)
	n := 0
	for _, c := range convs {
		if c.SessionID == resp.ConvID {
			n++
		}
	}
	assert.Equal(t, 1, n, "a resume must not fork the conversation")
	title, err := h.Convs.Title(resp.ConvID)
	require.NoError(t, err)
	assert.Equal(t, "gemini-worker", title)
}

// TestGeminiSpawn_TclaudeLayerForcesGeminisOwnSandboxOff: under tclaude's own
// wall the pane is launched with Gemini's sandbox forced off, so the outer
// wall is the single enforcement boundary — GEMINI_SANDBOX outranks a
// settings.json `tools.sandbox` that would otherwise re-run the CLI in a
// container outside the wall.
func TestGeminiSpawn_TclaudeLayerForcesGeminisOwnSandboxOff(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	resp, _ := spawnGemini(t, f, "crew", map[string]any{
		"name":                   "walled-gemini",
		"sandbox_implementation": "tclaude-layer",
	})
	launch := geminiLaunchOf(t, f, resp.ConvID)
	assert.Equal(t, "false", launch.Env["GEMINI_SANDBOX"])
	sandboxVar, set := launch.Env["SANDBOX"]
	assert.True(t, set && sandboxVar == "",
		"SANDBOX must be exported empty: a stray value would make Gemini believe it is already contained, "+
			"and an unset one could be refilled by a workspace .env")

	row, err := db.LoadSession(resp.Label)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, "tclaude-layer", row.SandboxImplementation)
	assert.Equal(t, harness.GeminiSandboxOff, row.HarnessBuiltinMode)

	// A resume relaunches under the recorded posture, not a re-defaulted one.
	f.AssertSoftStopped(f.AsHuman().Stop(resp.ConvID, false))
	require.Equal(t, http.StatusOK, f.Resume(resp.ConvID).Code)
	relaunch := geminiLaunchOf(t, f, resp.ConvID)
	require.Equal(t, resp.ConvID, relaunch.ResumeID)
	assert.Equal(t, "false", relaunch.Env["GEMINI_SANDBOX"], "a resumed walled pane must keep Gemini's sandbox off")

	// A plain spawn leaves the operator's Gemini sandbox posture alone.
	plain, _ := spawnGemini(t, f, "crew", map[string]any{"name": "plain-gemini"})
	plainLaunch := geminiLaunchOf(t, f, plain.ConvID)
	_, forced := plainLaunch.Env["GEMINI_SANDBOX"]
	assert.False(t, forced)
}

// TestGeminiSpawn_ApprovalModeIsRenderedAndRecorded: an unchosen posture
// resolves to the nonblocking `yolo`, an explicit one threads through, and
// `inherit` emits no flag — each recorded on the session row so a relaunch
// reproduces it.
func TestGeminiSpawn_ApprovalModeIsRenderedAndRecorded(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	for _, tc := range []struct {
		name, approval, wantFlag, wantRow string
	}{
		{"default-gemini", "", harness.GeminiApprovalYolo, harness.GeminiApprovalYolo},
		{"edit-gemini", harness.GeminiApprovalAutoEdit, harness.GeminiApprovalAutoEdit, harness.GeminiApprovalAutoEdit},
		{"plan-gemini", harness.GeminiApprovalPlan, harness.GeminiApprovalPlan, harness.GeminiApprovalPlan},
		{"inherit-gemini", harness.GeminiApprovalInherit, "", harness.GeminiApprovalInherit},
	} {
		body := map[string]any{"name": tc.name}
		if tc.approval != "" {
			body["approval"] = tc.approval
		}
		resp, _ := spawnGemini(t, f, "crew", body)
		assert.Equalf(t, tc.wantFlag, geminiLaunchOf(t, f, resp.ConvID).ApprovalMode, "%s: rendered flag", tc.name)
		row, err := db.LoadSession(resp.Label)
		require.NoError(t, err)
		require.NotNil(t, row)
		assert.Equalf(t, tc.wantRow, row.ApprovalPolicy, "%s: recorded policy", tc.name)
	}

	bad := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "bad-gemini", "harness": harness.GeminiName, "approval": "never",
	})
	assert.NotEqual(t, http.StatusOK, bad.Code, "a Codex token is not a Gemini approval mode")
}

// TestGeminiSpawn_DashboardShowsUsageFromTheSessionFile: the per-call usage
// Gemini stamps on each model message reaches the dashboard's context meter —
// the latest call's prompt size against Gemini's own model limit, and output
// summed over the conversation.
func TestGeminiSpawn_DashboardShowsUsageFromTheSessionFile(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	resp, sim := spawnGemini(t, f, "crew", map[string]any{
		"name":            "metered-gemini",
		"initial_message": "start work",
	})
	sim.WriteGeminiReplyWithTokens("first", "gemini-3-flash", 10_000, 300, 200)
	sim.WriteGeminiReplyWithTokens("second", "gemini-3-flash", 104_857, 500, 0)

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	row := findDashAgent(snap, resp.ConvID)
	require.NotNil(t, row, "the Gemini agent must be on the dashboard")
	assert.Equal(t, int64(1_048_576), row.State.ContextWindowSize)
	assert.InDelta(t, 10.0, row.State.ContextPct, 0.01)

	stored, err := db.GetContextSnapshot(resp.Label)
	require.NoError(t, err)
	assert.Equal(t, int64(104_857), stored.TokensInput, "context occupancy is the LATEST call's prompt")
	assert.Equal(t, int64(1_000), stored.TokensOutput, "output and thinking tokens summed over the conversation")
}

// TestGeminiSpawn_DashboardShowsTheModelSwitchedInPane: a /model switch made
// inside the pane shows as the agent's model, read from the model Gemini
// stamps on each call — not the one it was launched with.
func TestGeminiSpawn_DashboardShowsTheModelSwitchedInPane(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	resp, sim := spawnGemini(t, f, "crew", map[string]any{
		"name":            "switching-gemini",
		"model":           "gemini-3-flash",
		"initial_message": "start work",
	})
	sim.WriteGeminiReplyWithTokens("first", "gemini-3-flash", 10_000, 300, 0)
	sim.WriteGeminiReplyWithTokens("after /model", "gemini-3.1-pro-preview", 12_000, 300, 0)

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	row := findDashAgent(snap, resp.ConvID)
	require.NotNil(t, row, "the Gemini agent must be on the dashboard")
	assert.Equal(t, "gemini-3.1-pro-preview", row.State.Model, "the latest call's model")
}

// TestGeminiSpawn_WhatIfCostFromTheSessionFile: a priced model's calls show
// up as the agent's WHAT-IF cost, priced at Gemini API rates.
func TestGeminiSpawn_WhatIfCostFromTheSessionFile(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f.HaveGroup("crew")
	resp, sim := spawnGemini(t, f, "crew", map[string]any{
		"name":            "priced-gemini",
		"initial_message": "start work",
	})
	sim.WriteGeminiReplyWithTokens("first", "gemini-3.5-flash", 100_000, 4_000, 6_000)
	sim.WriteGeminiReplyWithTokens("unpriced", "gemini-3-flash", 100_000, 4_000, 0)

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	row := findDashAgent(snap, resp.ConvID)
	require.NotNil(t, row, "the Gemini agent must be on the dashboard")
	// 100k input at $1.50/M plus 10k output+thinking at $9.00/M; the call on a
	// model with no published rate adds nothing.
	assert.InDelta(t, 0.15+0.09, row.State.VirtualCostUSD, 1e-9)
	assert.Zero(t, row.State.CostUSD, "a what-if estimate is never a real charge")
}
