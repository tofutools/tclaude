package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Flow coverage for Copilot's soft-exit submit contract (TCL-1001 part A).
//
// Measured against the pinned 1.0.77 CLI in a real tmux pane: typing "/exit"
// and pressing Enter is NOT enough. Whenever the TUI is not sitting idle at its
// input prompt it refuses the command — mid-turn it renders the typed text and
// then silently discards it on Enter (no exit, no queued message, no line in
// the transcript, which is exactly the "no visible attempt" the operator
// reported after reattaching), and with a permission dialog open that Enter
// lands on the DIALOG, accepting its default entry and approving the pending
// command instead of exiting.
//
// So the harness contributes a cancel keystroke ahead of the command
// (harness.Lifecycle.SoftExitPrefixKeys). These scenarios pin that the daemon
// actually sends it, in the right order, and that it is what makes the exit
// land on a pane that would otherwise swallow it.

// Scenario: an ordinary idle Copilot pane. The cancel keystroke precedes the
// exit command — on an idle pane it is a measured no-op, so it costs the
// graceful path nothing and the pane still exits on its own without any kill.
func TestCopilotSoftExit_SendsCancelBeforeExitCommand(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")

	resp, sim := spawnCopilot(t, f, "crew", map[string]any{
		"trust_dir": true,
		"name":      "copilot-worker",
	})

	f.AssertSoftStopped(f.AsHuman().Stop(resp.ConvID, false))
	agentd.WaitForBackgroundForTest()

	texts := sentTexts(f.World.Tmux.Sent())
	cancelAt, exitAt := -1, -1
	for i, text := range texts {
		if text == "C-c" && cancelAt < 0 {
			cancelAt = i
		}
		if text == "/exit" && exitAt < 0 {
			exitAt = i
		}
	}
	require.GreaterOrEqual(t, cancelAt, 0, "the Copilot soft exit must send a cancel; sent=%v", texts)
	require.GreaterOrEqual(t, exitAt, 0, "the Copilot soft exit must send /exit; sent=%v", texts)
	assert.Less(t, cancelAt, exitAt,
		"the cancel must precede the command it exists to make acceptable; sent=%v", texts)

	assert.Equal(t, 1, sim.Cancels(), "an idle pane needs exactly one cancel, not a burst")
	assert.False(t, sim.IsAlive(), "the exit still lands: a cancel on an idle pane is a no-op")
	assert.Empty(t, killTargets(f), "a graceful Copilot exit is never escalated to a kill")
}

// Scenario: the failure that motivated the contract. The pane is parked on a
// permission dialog, where a bare "/exit" + Enter is swallowed by the dialog.
// The cancel aborts the dialog first — refusing the pending command rather
// than approving it — so the exit reaches the input prompt and the pane closes
// gracefully, with no kill needed.
func TestCopilotSoftExit_CancelUnparksAPaneHeldByAPermissionDialog(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")

	resp, sim := spawnCopilot(t, f, "crew", map[string]any{
		"trust_dir":       true,
		"name":            "copilot-worker",
		"initial_message": "clean up the build",
		"approval":        harness.CopilotApprovalInherit,
	})

	// Park the pane on a permission dialog: the turn can never end and the
	// pane owns the keyboard, which is where a bare /exit disappears.
	require.Equal(t, testharness.CopilotToolBlocked,
		sim.RequestTool(testharness.CopilotToolCall{
			Kind: testharness.CopilotToolShell, Command: "rm -rf ./build"}))
	blocked, _ := sim.Blocked()
	require.True(t, blocked, "the pane must actually be parked for this scenario to mean anything")

	f.AssertSoftStopped(f.AsHuman().Stop(resp.ConvID, false))
	agentd.WaitForBackgroundForTest()

	assert.False(t, sim.IsAlive(),
		"the cancel must unpark the dialog so the exit command lands")
	assert.False(t, f.World.Tmux.IsAlive(resp.TmuxSession))
	assert.Empty(t, killTargets(f),
		"unparking is what the cancel is for — the ladder should not have been needed")
}
