package copilotfixture_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness/copilotfixture"
)

// TCL-1001: what an injected soft exit actually does to Copilot 1.0.77's TUI.
//
// tclaude retires an agent by typing the harness's exit command into its pane
// and waiting. In production two retired Copilot agents kept running with — as
// the operator saw on reattach — NO trace in the TUI that anything had been
// typed at all. These scenarios are the measurement that explains it, and they
// are why the Copilot lifecycle contributes a cancel keystroke ahead of its
// exit command (harness.copilotLifecycle.SoftExitPrefixKeys).
//
// The finding, in one line: the TUI accepts a slash command only when it is
// idle at its input prompt. Mid-turn it renders the typed "/exit" and then
// DISCARDS it on Enter — no exit, no queued message, nothing in the
// transcript. A cancel first returns the TUI to the state where the command is
// accepted, and then the same injection exits cleanly.
//
// Why a PTY and raw byte writes: a CLI with no terminal draws no TUI, so a
// headless run cannot observe this at all, and the bytes below ("\x03", the
// text, "\r") are exactly what tmux send-keys delivers — which is what makes a
// result here evidence about tclaude's own pane injection rather than about
// this test's typing.

// softExitDeadline bounds one scenario. The mock streams for well over this,
// so a run that reaches the deadline is a run whose exit never landed — the
// blocked arm's whole claim — while an arm that exits ends the moment the
// process does.
//
// 18s, down from 25s, and the margins are the argument rather than the number:
// the turn streams for ~28s, so the deadline still lands with it comfortably in
// flight, and the keystroke sequence finishes by ~9.5s, so an exit that was
// going to land has ~8.5s to do it. The control arm takes about one second.
const softExitDeadline = 18 * time.Second

// softExitBusyTurn holds the TUI mid-turn for the whole scenario: ~40 deltas
// at 700ms is far longer than the deadline, so the turn is still in flight
// whenever the keystrokes below arrive.
func softExitBusyTurn() []copilotfixture.Turn {
	return []copilotfixture.Turn{{
		Text:       "one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen seventeen eighteen nineteen twenty twenty-one twenty-two twenty-three twenty-four twenty-five twenty-six twenty-seven twenty-eight twenty-nine thirty thirty-one thirty-two thirty-three thirty-four thirty-five thirty-six thirty-seven thirty-eight thirty-nine forty",
		ChunkDelay: 700 * time.Millisecond,
	}}
}

// softExitRun launches a trusted, credential-free interactive pane whose first
// turn is already running, and delivers keystrokes into it on a schedule.
func softExitRun(t *testing.T, keys []copilotfixture.Keystroke) copilotfixture.PTYResult {
	t.Helper()
	mock := copilotfixture.NewMockProvider(t, softExitBusyTurn())
	dirs := copilotfixture.NewSandboxDirs(t)
	copilotfixture.TrustFolder(t, dirs.Home, dirs.WorkDir)
	return copilotfixture.RunPTY(t, copilotfixture.PTYOptions{
		RunOptions: copilotfixture.RunOptions{
			Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: dirs.WorkDir,
			BaseURL: mock.BaseURL(),
			Prompt:  "Count slowly to forty.",
		},
		Deadline:   softExitDeadline,
		Keystrokes: keys,
	})
}

// softExitKeys renders the injection tclaude performs: optional prefix keys,
// then the command text, then the two settled Enters. The 500ms gaps are the
// production settle (paneinput.defaultSettleDelay); the 8s lead-in is what
// puts the whole sequence inside the running turn.
func softExitKeys(prefix string) []copilotfixture.Keystroke {
	keys := []copilotfixture.Keystroke{}
	lead := 8 * time.Second
	if prefix != "" {
		keys = append(keys, copilotfixture.Keystroke{After: lead, Bytes: prefix})
		lead = 500 * time.Millisecond
	}
	return append(keys,
		copilotfixture.Keystroke{After: lead, Bytes: "/exit"},
		copilotfixture.Keystroke{After: 500 * time.Millisecond, Bytes: "\r"},
		copilotfixture.Keystroke{After: 500 * time.Millisecond, Bytes: "\r"},
	)
}

// TestCopilotSoftExitBareExitIsDiscardedMidTurn is the bug, reproduced: the
// exact injection tclaude used to perform, typed into a busy TUI, does
// nothing. The CLI is still running at the deadline.
//
// The negative claim is only worth as much as its control, which is the next
// scenario: the same rig, the same bytes, one cancel keystroke in front.
func TestCopilotSoftExitBareExitIsDiscardedMidTurn(t *testing.T) {
	// Sequential, and for the same reason as the pane-injection scenario in
	// permission_smoke_test.go: this one is ABOUT timing. Its keystrokes are
	// scheduled on a wall clock from launch, and the 8s lead exists to put them
	// inside a turn that is still running — an assumption about how far the CLI
	// has got by then, which is exactly the assumption CPU contention breaks.
	//
	// It broke, on CI, and the way it broke is worth recording: the run still
	// reported "did not exit", so the headline assertion passed, and only the
	// positive control caught it. The transcript showed a COMPLETED turn with
	// no "/exit" on screen anywhere — the keystrokes had been typed during a
	// startup that had not finished yet and were discarded, so the scenario was
	// one assertion away from reporting the bug it exists to reproduce on
	// evidence that had nothing to do with the bug.
	requireSmoke(t)

	res := softExitRun(t, softExitKeys(""))

	assert.False(t, res.Exited,
		"1.0.77 discarded a mid-turn /exit; a run that EXITED means the CLI changed "+
			"and the Copilot soft-exit prefix should be re-measured")
	// The command was typed and visibly echoed — the pane received the
	// keystrokes; it simply refused to act on them. That distinction is the
	// whole diagnosis: the injection was not lost in transit.
	assert.True(t, res.Contains("/exit"),
		"the typed command should still be visible on screen; transcript=%q",
		lastLines(res.TranscriptText(), 12))
}

// TestCopilotSoftExitCancelFirstExitsMidTurn is the fix, measured: one cancel
// keystroke ahead of the identical sequence and the busy pane exits cleanly.
func TestCopilotSoftExitCancelFirstExitsMidTurn(t *testing.T) {
	// Sequential, for the reason spelled out on the scenario above: the same
	// wall-clock keystroke schedule, so the same assumption about how far the
	// CLI has got by 8s. A control that raced for CPU while the arm it controls
	// did not would not be a control.
	requireSmoke(t)

	// "\x03" is the byte tmux send-keys C-c delivers.
	res := softExitRun(t, softExitKeys("\x03"))

	require.True(t, res.Exited,
		"a cancel before /exit must let a busy pane exit; transcript=%q",
		lastLines(res.TranscriptText(), 12))
	assert.Equal(t, 0, res.ExitCode,
		"the escalation ladder's whole premise is that this layer exits GRACEFULLY")
}

// lastLines trims a transcript for failure messages: the tail is where the
// prompt, the footer and any error live, and the head is a fixed banner.
func lastLines(text string, n int) string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
