package agentd_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/copilotfixture"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Copilot's hooks, driven end to end through the daemon with the payloads the
// REAL 1.0.77 binary produced (pkg/claude/harness/copilotfixture/testdata).
//
// The unit tests either side of this one prove the two halves separately: the
// installer writes a file Copilot loads, and the recorded payloads decode into
// canonical HookCallbackInput. What is left — and what an operator actually
// sees — is whether feeding those payloads through the production hook path
// moves a Copilot agent's status the way the dashboard and `agent ls` need.
//
// Before this change a Copilot session sat at its initial idle forever. The
// risk of the change is the opposite failure: an agent that enters "working"
// and never comes back, which would break the dashboard, idle notifications
// and every coordination decision built on "is this agent free". Copilot's
// Stop event is what makes that impossible, so it is what these tests pin.

const (
	copilotFlowConv  = "copi-1111-2222-3333-4444"
	copilotFlowLabel = "spwn-copi"
)

// haveCopilotSession stands up an alive session row recorded as a Copilot
// launch — the shape `tclaude session new --harness copilot` leaves behind,
// with the conv-id preset at launch (LaunchEnrollment) rather than discovered
// from a hook.
func haveCopilotSession(t *testing.T, f *testharness.Flow, conv, label, tmux, cwd string) {
	t.Helper()
	f.HaveAliveSession(conv, label, tmux, cwd)
	row, err := db.LoadSession(label)
	require.NoError(t, err, "LoadSession(%s)", label)
	require.NotNil(t, row, "session row %s should exist", label)
	row.Harness = harness.CopilotName
	require.NoError(t, db.SaveSession(row), "record the Copilot launch")
}

// copilotHook returns a recorded payload decoded into canonical input, with
// the fixture's placeholder session id and cwd rewritten to this test's own.
// Decoding the RECORDED BYTES rather than building a struct literal is the
// point: if Copilot renames a field, this test stops proving what it claims.
func copilotHook(t *testing.T, capture copilotfixture.HookCapture, event string, index int, conv, cwd string) session.HookCallbackInput {
	t.Helper()
	payloads := capture.FindAll(event)
	require.Greaterf(t, len(payloads), index, "no recorded %s payload at index %d", event, index)
	var in session.HookCallbackInput
	require.NoError(t, json.Unmarshal(
		copilotfixture.HookPayloadFor(payloads[index], conv, cwd), &in))
	return in
}

// Scenario: one complete Copilot turn, replayed in the order Copilot really
// fires it — which is NOT the order any other harness uses. UserPromptSubmit
// arrives BEFORE SessionStart, so the test also proves that the unusual order
// does not leave bad state behind.
//
// Expected: idle → working on the prompt, still working across the tool call,
// back to idle on Stop.
func TestCopilotHooks_TurnGoesWorkingThenIdle(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("squad")
	cwd := f.TestCwd("copi")
	haveCopilotSession(t, f, copilotFlowConv, copilotFlowLabel, "tmux-copi", cwd)
	f.HaveMember("squad", copilotFlowConv)

	capture := copilotfixture.LoadHookCapture(t, copilotfixture.HookScenarioClaudeDialect)
	apply := func(in session.HookCallbackInput) {
		t.Helper()
		require.NoError(t, session.ApplyHook(in, copilotFlowLabel), "ApplyHook(%s)", in.HookEventName)
	}
	member := func() *dashMember {
		t.Helper()
		m := findDashMember(fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest()), "squad", copilotFlowConv)
		require.NotNil(t, m, "agent %s missing from group squad", copilotFlowConv)
		return m
	}

	// Before any hook the dashboard can only report the row's process status:
	// a live pane with no live status of its own. That is precisely the state
	// every Copilot session was stuck in before this change.
	require.Equal(t, "running", member().State.Status,
		"a Copilot pane has no live status until its hooks report one")

	// 1) The prompt — which Copilot announces BEFORE the session.
	apply(copilotHook(t, capture, "UserPromptSubmit", 0, copilotFlowConv, cwd))
	assert.Equal(t, session.StatusWorking, member().State.Status, "the turn started")

	// 2) SessionStart lands second. It must not undo the working status the
	//    prompt just set — that ordering is exactly what makes Copilot
	//    different, and getting it wrong would show every busy agent as free.
	apply(copilotHook(t, capture, "SessionStart", 0, copilotFlowConv, cwd))
	assert.Equal(t, session.StatusWorking, member().State.Status,
		"a late SessionStart must not report a working agent as idle")

	// 3) A tool ran mid-turn.
	apply(copilotHook(t, capture, "PostToolUse", 0, copilotFlowConv, cwd))
	got := member()
	assert.Equal(t, session.StatusWorking, got.State.Status)
	assert.Equal(t, "Bash", got.State.StatusDetail,
		"the dialect's Claude tool names flow straight into the status detail")

	// 4) The turn ends. This is the whole reason the integration is worth
	//    having: without Stop the agent would stay "working" forever.
	apply(copilotHook(t, capture, "Stop", 0, copilotFlowConv, cwd))
	got = member()
	assert.Equal(t, session.StatusIdle, got.State.Status, "Stop returns the agent to idle")
	assert.Empty(t, got.State.StatusDetail)
}

// Scenario: the guard that makes the test above pass must not change any OTHER
// harness. Claude Code fires SessionStart BEFORE the first prompt, so there
// "session started" really does mean nothing is running — and settling to idle
// is what clears phantom state left by a previous process.
//
// Expected: an identical event pair against a Claude Code session still ends
// idle, and an IDLE Copilot session still stays idle. The guard preserves a
// running turn; it never invents one.
func TestCopilotHooks_LateSessionStartGuardIsHarnessScoped(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("squad")

	const ccConv, ccLabel = "clau-1111-2222-3333-4444", "spwn-clau"
	ccCwd := f.TestCwd("clau")
	f.HaveAliveSession(ccConv, ccLabel, "tmux-clau", ccCwd)
	f.HaveMember("squad", ccConv)

	cwd := f.TestCwd("copi")
	haveCopilotSession(t, f, copilotFlowConv, copilotFlowLabel, "tmux-copi", cwd)
	f.HaveMember("squad", copilotFlowConv)

	status := func(conv string) string {
		t.Helper()
		m := findDashMember(fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest()), "squad", conv)
		require.NotNil(t, m)
		return m.State.Status
	}

	// Claude Code: the ordinary ordering, where the reset is correct.
	require.NoError(t, session.ApplyHook(session.HookCallbackInput{
		HookEventName: "UserPromptSubmit", ConvID: ccConv, Cwd: ccCwd}, ccLabel))
	require.NoError(t, session.ApplyHook(session.HookCallbackInput{
		HookEventName: "SessionStart", ConvID: ccConv, Cwd: ccCwd, Source: "startup"}, ccLabel))
	assert.Equal(t, session.StatusIdle, status(ccConv),
		"a harness that announces its session first must still settle to idle")

	// Copilot with nothing running: the guard only preserves a turn that is
	// actually in flight, so an idle session is unaffected.
	capture := copilotfixture.LoadHookCapture(t, copilotfixture.HookScenarioClaudeDialect)
	require.NoError(t, session.ApplyHook(
		copilotHook(t, capture, "Stop", 0, copilotFlowConv, cwd), copilotFlowLabel))
	require.Equal(t, session.StatusIdle, status(copilotFlowConv))
	require.NoError(t, session.ApplyHook(
		copilotHook(t, capture, "SessionStart", 0, copilotFlowConv, cwd), copilotFlowLabel))
	assert.Equal(t, session.StatusIdle, status(copilotFlowConv),
		"the guard preserves a running turn; it never invents one")
}

// Scenario: SessionEnd arrives more than once.
//
// Copilot's SessionEnd is at-least-once, not once-per-session: a hook that
// steers the agent into forced continuation makes Stop and SessionEnd repeat
// for a single prompt. tclaude installs a stdout-discarding command precisely
// so it can never cause that, but a user's own hook still can — so the
// receiver has to be idempotent regardless of who triggered it.
func TestCopilotHooks_RepeatedSessionEndIsIdempotent(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("squad")
	cwd := f.TestCwd("copi")
	haveCopilotSession(t, f, copilotFlowConv, copilotFlowLabel, "tmux-copi", cwd)
	f.HaveMember("squad", copilotFlowConv)

	capture := copilotfixture.LoadHookCapture(t, copilotfixture.HookScenarioClaudeDialect)
	end := copilotHook(t, capture, "SessionEnd", 0, copilotFlowConv, cwd)

	for i := range 3 {
		require.NoErrorf(t, session.ApplyHook(end, copilotFlowLabel), "SessionEnd #%d", i+1)
	}

	row, err := db.LoadSession(copilotFlowLabel)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, harness.CopilotName, row.Harness,
		"repeated SessionEnd must not corrupt the row it lands on")

	// A repeat must also stay harmless for a turn that is genuinely underway:
	// the prompt after the duplicates still reports the agent as working.
	require.NoError(t, session.ApplyHook(
		copilotHook(t, capture, "UserPromptSubmit", 0, copilotFlowConv, cwd), copilotFlowLabel))
	m := findDashMember(fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest()), "squad", copilotFlowConv)
	require.NotNil(t, m)
	assert.Equal(t, session.StatusWorking, m.State.Status)
}

// Scenario: a RESUMED Copilot launch. `copilot --resume=<id>` keeps the
// session id, and Copilot fires SessionStart with source=resume — the same
// vocabulary Claude Code uses for an in-process conversation switch.
//
// Expected: the resumed turn drives the same working→idle cycle against the
// SAME conv-id, so a resumed agent stays the agent tclaude already tracks
// rather than becoming a second one.
func TestCopilotHooks_ResumedSessionKeepsTracking(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("squad")
	cwd := f.TestCwd("copi")
	haveCopilotSession(t, f, copilotFlowConv, copilotFlowLabel, "tmux-copi", cwd)
	f.HaveMember("squad", copilotFlowConv)

	capture := copilotfixture.LoadHookCapture(t, copilotfixture.HookScenarioClaudeDialect)
	status := func() string {
		t.Helper()
		m := findDashMember(fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest()), "squad", copilotFlowConv)
		require.NotNil(t, m)
		return m.State.Status
	}

	resumeStart := copilotHook(t, capture, "SessionStart", 1, copilotFlowConv, cwd)
	require.Equal(t, "resume", resumeStart.Source, "the second recorded SessionStart is the resume")

	require.NoError(t, session.ApplyHook(
		copilotHook(t, capture, "UserPromptSubmit", 1, copilotFlowConv, cwd), copilotFlowLabel))
	require.NoError(t, session.ApplyHook(resumeStart, copilotFlowLabel))
	assert.Equal(t, session.StatusWorking, status(), "the resumed turn is running")

	require.NoError(t, session.ApplyHook(
		copilotHook(t, capture, "Stop", 1, copilotFlowConv, cwd), copilotFlowLabel))
	assert.Equal(t, session.StatusIdle, status(), "and settles on its own Stop")

	row, err := db.LoadSession(copilotFlowLabel)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, copilotFlowConv, row.ConvID,
		"a resume must keep the tracked conv-id, not fork a second identity")
}
