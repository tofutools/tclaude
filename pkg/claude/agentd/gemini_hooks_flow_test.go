package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// One Gemini turn delivered as Gemini's own hook payloads, in the order the
// CLI can produce them for a launch with an `-i` first turn: the prompt's
// BeforeAgent may arrive before SessionStart (the app marks its config ready
// before it awaits the SessionStart hook). The payloads use Gemini's event
// names and go through the callback's real decode, so the name translation is
// exercised end to end, and the status is read back from the dashboard.
func TestGeminiHooks_TurnGoesWorkingThenIdle(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	resp, sim := spawnGemini(t, f, "crew", map[string]any{
		"name":            "gemini-worker",
		"initial_message": "start work",
		"trust_dir":       true,
	})
	member := func() *dashMember {
		t.Helper()
		m := findDashMember(fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest()), "crew", resp.ConvID)
		require.NotNil(t, m, "agent %s missing from group crew", resp.ConvID)
		return m
	}
	fire := func(event string, fields map[string]any) {
		t.Helper()
		require.NoError(t, sim.FireHook(event, fields), "hook %s", event)
	}

	fire("BeforeAgent", map[string]any{"prompt": "start work"})
	assert.Equal(t, session.StatusWorking, member().State.Status, "BeforeAgent starts the turn")

	fire("SessionStart", map[string]any{"source": "startup"})
	assert.Equal(t, session.StatusWorking, member().State.Status,
		"a SessionStart that lands after the first prompt must not report the agent idle")

	fire("Notification", map[string]any{
		"notification_type": "ToolPermission",
		"message":           "Allow run_shell_command?",
		"details":           map[string]any{"type": "exec"},
	})
	assert.Equal(t, session.StatusAwaitingPermission, member().State.Status,
		"a tool-confirmation dialog is a human-attention state")

	fire("AfterTool", map[string]any{
		"tool_name":     "run_shell_command",
		"tool_input":    map[string]any{"command": "go test ./..."},
		"tool_response": map[string]any{"llmContent": "ok"},
	})
	assert.Equal(t, session.StatusWorking, member().State.Status, "the tool ran: back to working")

	fire("AfterAgent", map[string]any{
		"prompt": "start work", "prompt_response": "Done.", "stop_hook_active": false,
	})
	assert.Equal(t, session.StatusIdle, member().State.Status, "AfterAgent ends the turn")
}
