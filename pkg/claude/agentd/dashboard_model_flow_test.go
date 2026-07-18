package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// Scenario: the LLM model an agent runs on — written to the sessions
// row by the statusline hook (UpdateSessionModel, fed from Claude Code's
// model.display_name) — must surface on /api/snapshot so the dashboard
// can show "CC · Opus 4.8" under the per-row controls and in the
// status-dot tooltip. Rides on the same row read as the context meter;
// no new poller, no new data source.
//
// Asserts the model appears on BOTH the Agents[] roster and the group
// Members[] row (the two places memberRowHTML / the agents table draw
// it).
func TestDashboardSnapshot_ModelSurfaced(t *testing.T) {
	const conv = "modl-1111-2222-3333-4444"
	const label = "spwn-modl"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveGroup("squad")
	f.HaveAliveSession(conv, label, "tmux-modl", f.TestCwd("modl"))
	f.HaveMember("squad", conv)

	// The statusline hook's write path: the model + effort level land on
	// the sessions row keyed by tclaude session ID (the label).
	require.NoError(t, db.UpdateSessionModel(label, "Opus 4.8"), "UpdateSessionModel")
	require.NoError(t, db.UpdateSessionEffort(label, "high"), "UpdateSessionEffort")

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	agentRow := findDashAgent(snap, conv)
	require.NotNil(t, agentRow, "agent %s missing from snapshot Agents[]", conv)
	assert.Equal(t, "Opus 4.8", agentRow.State.Model, "Agents[] model")
	assert.Equal(t, "high", agentRow.State.EffortLevel, "Agents[] effort level")

	memberRow := findDashMember(snap, "squad", conv)
	require.NotNil(t, memberRow, "agent %s missing from group squad members", conv)
	assert.Equal(t, "Opus 4.8", memberRow.State.Model, "Members[] model")
	assert.Equal(t, "high", memberRow.State.EffortLevel, "Members[] effort level")
}

// Scenario: a freshly-spawned agent whose statusline hook has not yet
// fired has no model recorded. /api/snapshot must report an empty model
// rather than garbage; the dashboard then omits the harness line
// entirely (harnessLine returns an empty string) so the row stays clean.
func TestDashboardSnapshot_ModelEmptyWhenNotReported(t *testing.T) {
	const conv = "modu-1111-2222-3333-4444"
	const label = "spwn-modu"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveAliveSession(conv, label, "tmux-modu", f.TestCwd("modu"))
	f.HaveEnrolledAgent(conv)

	// No UpdateSessionModel call — the statusline hook never fired.

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	agentRow := findDashAgent(snap, conv)
	require.NotNil(t, agentRow, "agent %s missing from snapshot Agents[]", conv)
	assert.Equal(t, "", agentRow.State.Model, "no-tick agent should report empty model")
}

// Scenario: Codex has no command-backed statusline, but its hook payloads
// carry the current model and tclaude's launch path knows the explicit
// reasoning effort it passed to Codex. Those values should land in the same
// session columns the dashboard already renders for Claude Code.
func TestDashboardSnapshot_CodexModelAndEffortSurfaced(t *testing.T) {
	const conv = "019ec004-4250-79b1-9ade-ebaea41590aa"
	const label = "spwn-codx-model"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveGroup("squad")
	cx := f.HaveAliveCodexSession(conv, label, "tmux-codx-model", f.TestCwd("codx-model"))
	cx.Effort = "high"
	require.NoError(t, cx.WriteUserInput("start"))
	f.HaveMember("squad", conv)

	require.NoError(t, session.ApplyHook(session.HookCallbackInput{
		HookEventName:  "Stop",
		ConvID:         conv,
		Cwd:            f.TestCwd("codx-model"),
		Model:          "gpt-5-codex",
		TranscriptPath: cx.RolloutPath,
	}, label))

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	agentRow := findDashAgent(snap, conv)
	require.NotNil(t, agentRow, "agent %s missing from snapshot Agents[]", conv)
	assert.Equal(t, "gpt-5-codex", agentRow.State.Model, "Agents[] Codex model")
	assert.Equal(t, "high", agentRow.State.EffortLevel, "Agents[] Codex effort level")

	memberRow := findDashMember(snap, "squad", conv)
	require.NotNil(t, memberRow, "agent %s missing from group squad members", conv)
	assert.Equal(t, "gpt-5-codex", memberRow.State.Model, "Members[] Codex model")
	assert.Equal(t, "high", memberRow.State.EffortLevel, "Members[] Codex effort level")
}

// Regression: changing a running Codex thread's model does not fire a
// dedicated model-change hook. Codex does, however, put the active model slug
// on every later lifecycle hook. The first accepted main-thread event must
// advance both sessions.model (dashboard display) and sessions.model_id (resume /
// reincarnate inheritance). Reasoning effort is not in Codex's hook payload,
// so it deliberately converges from the rollout at the next Stop instead of
// introducing a dashboard file poller.
func TestDashboardSnapshot_CodexRuntimeModelAndEffortChangesConvergeFromHooks(t *testing.T) {
	const conv = "019ec004-4250-79b1-9ade-ebaea41590ab"
	const label = "spwn-codx-switch"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveGroup("squad")
	cx := f.HaveAliveCodexSession(conv, label, "tmux-codx-switch", f.TestCwd("codx-switch"))
	f.HaveMember("squad", conv)

	// Establish the original runtime configuration through a completed turn.
	cx.Model = "gpt-5.4"
	cx.Effort = "medium"
	require.NoError(t, cx.WriteUserInput("first turn"))
	require.NoError(t, session.ApplyHook(session.HookCallbackInput{
		HookEventName:  "Stop",
		ConvID:         conv,
		Cwd:            f.TestCwd("codx-switch"),
		Model:          "gpt-5.4",
		TranscriptPath: cx.RolloutPath,
	}, label))

	original := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	originalAgent := findDashAgent(original, conv)
	require.NotNil(t, originalAgent)
	assert.Equal(t, "gpt-5.4", originalAgent.State.Model)
	assert.Equal(t, "medium", originalAgent.State.EffortLevel)

	// The operator switches model + effort. A subsequent PostCompact carries
	// the new active model but no effort field. PostCompact is important here:
	// it returns early from the status machine, so model capture must happen
	// before that return.
	cx.Model = "gpt-5.5"
	cx.Effort = "xhigh"
	require.NoError(t, cx.WriteUserInput("second turn"))
	require.NoError(t, session.ApplyHook(session.HookCallbackInput{
		HookEventName:  "PostCompact",
		ConvID:         conv,
		Cwd:            f.TestCwd("codx-switch"),
		Model:          "gpt-5.5",
		TranscriptPath: cx.RolloutPath,
	}, label))

	afterCompact := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	compactAgent := findDashAgent(afterCompact, conv)
	require.NotNil(t, compactAgent)
	assert.Equal(t, "gpt-5.5", compactAgent.State.Model,
		"the next accepted Codex hook advances the dashboard model")
	assert.Equal(t, "medium", compactAgent.State.EffortLevel,
		"effort waits for the turn-ending rollout read")

	dbSnap, err := db.GetContextSnapshot(label)
	require.NoError(t, err)
	assert.Equal(t, "gpt-5.5", dbSnap.ModelID,
		"the same hook atomically advances the resume-safe model id")

	// Stop is the explicit, event-driven convergence point for effort. There
	// is still no dashboard rollout polling for model or reasoning effort.
	require.NoError(t, session.ApplyHook(session.HookCallbackInput{
		HookEventName:  "Stop",
		ConvID:         conv,
		Cwd:            f.TestCwd("codx-switch"),
		Model:          "gpt-5.5",
		TranscriptPath: cx.RolloutPath,
	}, label))

	afterStop := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	stoppedAgent := findDashAgent(afterStop, conv)
	require.NotNil(t, stoppedAgent)
	assert.Equal(t, "gpt-5.5", stoppedAgent.State.Model)
	assert.Equal(t, "xhigh", stoppedAgent.State.EffortLevel,
		"Stop converges the changed Codex reasoning effort")

	member := findDashMember(afterStop, "squad", conv)
	require.NotNil(t, member)
	assert.Equal(t, "gpt-5.5", member.State.Model, "group member model")
	assert.Equal(t, "xhigh", member.State.EffortLevel, "group member effort")
}
