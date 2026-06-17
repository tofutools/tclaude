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
	f.HaveAliveSession(conv, label, "tmux-modl", "/tmp/modl")
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
// entirely (harnessLine returns '') so the row stays clean.
func TestDashboardSnapshot_ModelEmptyWhenNotReported(t *testing.T) {
	const conv = "modu-1111-2222-3333-4444"
	const label = "spwn-modu"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveAliveSession(conv, label, "tmux-modu", "/tmp/modu")
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
	cx := f.HaveAliveCodexSession(conv, label, "tmux-codx-model", "/tmp/codx-model")
	f.HaveMember("squad", conv)

	require.NoError(t, db.UpdateSessionEffort(label, "high"), "seed Codex launch effort")
	require.NoError(t, session.ApplyHook(session.HookCallbackInput{
		HookEventName:  "SessionStart",
		ConvID:         conv,
		Cwd:            "/tmp/codx-model",
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
