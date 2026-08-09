package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Flow coverage for Copilot's soft-exit contract (TCL-1001 part A, revised
// after the 2026-08-09 stdin-wedge incident).
//
// Typing "/exit" is not a reliable exit. Measured against the pinned 1.0.77
// CLI, the TUI silently discards a typed slash command whenever it is not
// sitting idle at its prompt; and observed live on 1.0.78, a wedged keypress
// reader drops typed text outright for tens of seconds while the pane keeps
// rendering — three /exit injections ignored until the escalation ladder
// SIGKILLed the pane, costing the session its durable session.shutdown state.
// Throughout both failure modes ctrl-c handling demonstrably keeps working.
//
// So the managed stop types nothing: it sends three C-c presses. The first is
// spent cancelling whatever is in flight (a running turn, a permission
// dialog, a half-typed line) or, on an idle pane, arms 1.0.78's "ctrl+c again
// to exit"; the press after the arming one exits the CLI cleanly (status 0)
// through its designed quit path. These scenarios pin that the daemon sends
// exactly that sequence and that it closes the pane without the kill ladder.

// Scenario: an ordinary idle Copilot pane. No text is typed at all — the
// first press arms, the second exits, and the third lands on a dead pane
// (tolerated). The pane closes on its own without any kill.
func TestCopilotSoftExit_SignalExitClosesAnIdlePane(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")

	resp, sim := spawnCopilot(t, f, "crew", map[string]any{
		"trust_dir": true,
		"name":      "copilot-worker",
	})

	f.AssertSoftStopped(f.AsHuman().Stop(resp.ConvID, false))

	// Assert on the SYNCHRONOUS attempt alone, before the background retry
	// watchdog can add its own presses: the contract is that one attempt is a
	// burst of C-c presses and nothing else. The pane dies on the second
	// press, so the third lands on a dead pane and may not be recorded.
	attemptTexts := sentTexts(f.World.Tmux.Sent())
	ccSent := 0
	for _, text := range attemptTexts {
		require.Equal(t, "C-c", text,
			"the Copilot soft exit must send only C-c presses — no typed command, no Enter; sent=%v", attemptTexts)
		ccSent++
	}
	assert.GreaterOrEqual(t, ccSent, 2,
		"one attempt needs at least the arming press and the exiting press; sent=%v", attemptTexts)
	assert.GreaterOrEqual(t, sim.Cancels(), 2,
		"an idle pane exits on the second consecutive press (the surplus third may still be swallowed by the dead pane)")
	assert.False(t, sim.IsAlive(), "the second consecutive C-c on an idle pane exits the CLI")

	agentd.WaitForBackgroundForTest()
	assert.Empty(t, killTargets(f), "a graceful Copilot exit is never escalated to a kill")
}

// Scenario: the pane is parked on a permission dialog, which owns the
// keyboard. The first press ABORTS the dialog — refusing the pending command
// rather than approving it — the second arms, and the third exits. Three
// presses is exactly enough; no kill is needed.
func TestCopilotSoftExit_SignalExitClosesAPaneHeldByAPermissionDialog(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")

	resp, sim := spawnCopilot(t, f, "crew", map[string]any{
		"trust_dir":       true,
		"name":            "copilot-worker",
		"initial_message": "clean up the build",
		"approval":        harness.CopilotApprovalInherit,
	})

	// Park the pane on a permission dialog: the turn can never end and the
	// pane owns the keyboard, which is where typed text disappears.
	require.Equal(t, testharness.CopilotToolBlocked,
		sim.RequestTool(testharness.CopilotToolCall{
			Kind: testharness.CopilotToolShell, Command: "rm -rf ./build"}))
	blocked, _ := sim.Blocked()
	require.True(t, blocked, "the pane must actually be parked for this scenario to mean anything")

	f.AssertSoftStopped(f.AsHuman().Stop(resp.ConvID, false))

	assert.Equal(t, 3, sim.Cancels(),
		"press one aborts the dialog and ends its turn, press two arms, press three exits — one attempt is exactly enough")
	assert.False(t, sim.IsAlive(),
		"press one aborts the dialog, press two arms, press three exits")

	agentd.WaitForBackgroundForTest()
	assert.False(t, f.World.Tmux.IsAlive(resp.TmuxSession))
	assert.Empty(t, killTargets(f),
		"the dialog abort is what the first press is for — the ladder should not have been needed")
}
