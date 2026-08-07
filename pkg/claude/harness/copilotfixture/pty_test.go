package copilotfixture_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness/copilotfixture"
)

// RunPTY's own control flow, measured against a FAKE cli.
//
// Everything else in this package points the harness at the real pinned binary
// and asks what Copilot does. These scenarios ask the opposite question — what
// the harness does — and they have to, because two pieces of that control flow
// decide verdicts:
//
//   - BlockedAfter ends a run early and reports it as blocked, which is a
//     finding about what a detached agent is permitted to do.
//   - The first-output gate decides whether typed Input reaches the CLI at all,
//     and when it got this wrong the symptom was a scenario passing its headline
//     assertion for entirely the wrong reason.
//
// Neither could be covered by a real-CLI scenario without asking Copilot to be
// slow on demand, and neither ran at all under plain `go test`: the smoke gate
// skips every other test in the package, so this logic was compiled and never
// executed. A shell script on PATH is enough, since RunPTY resolves "copilot"
// through PATH rather than by absolute path, and it makes the timings exact
// instead of approximate.
//
// These are NOT gated on requireSmoke, and must not be: their whole value is
// running on the machines that have no pinned binary installed.

// fakeCopilot puts a script named "copilot" at the front of PATH.
//
// t.Setenv, so these scenarios are sequential — which is also correct on the
// merits, since what they measure is timing.
func fakeCopilot(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "copilot")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// fakeRunOptions is a disposable sandbox for a fake-CLI run. The fake ignores
// the argv RunPTY builds, but cmd.Dir still has to exist.
func fakeRunOptions(t *testing.T) copilotfixture.RunOptions {
	t.Helper()
	dirs := copilotfixture.NewSandboxDirs(t)
	return copilotfixture.RunOptions{
		Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: dirs.WorkDir,
		BaseURL: "http://127.0.0.1:1/unused",
	}
}

// TestRunPTYBlockedAfterEndsAQuietRunEarly pins the early exit that replaced
// sitting out the deadline.
//
// The fake draws once and then stops forever, which is the shape of a CLI
// parked on a prompt. What the assertion is really protecting is the pair of
// flags ClassifyPermission reads: a blocked-early run must look to the
// classifier exactly like a deadline-reaching one — alive, and quiesced —
// because the whole claim is that ending sooner did not change the verdict.
func TestRunPTYBlockedAfterEndsAQuietRunEarly(t *testing.T) {
	fakeCopilot(t, "echo ready; sleep 60")

	res := copilotfixture.RunPTY(t, copilotfixture.PTYOptions{
		RunOptions:   fakeRunOptions(t),
		Deadline:     30 * time.Second,
		BlockedAfter: 3 * time.Second,
	})

	assert.True(t, res.Blocked, "a run that drew once and then went quiet is blocked")
	assert.False(t, res.Exited, "the fake is still alive; that is the blocked claim")
	assert.True(t, res.Quiesced,
		"ClassifyPermission reads Quiesced, so blocked-early must set it exactly as "+
			"the deadline path does")
	assert.False(t, res.Settled, "no evidence arrived")
	assert.Less(t, res.Elapsed, 15*time.Second,
		"the point of BlockedAfter is not paying the deadline; elapsed=%s", res.Elapsed)
	assert.Contains(t, res.TranscriptText(), "ready")
}

// TestRunPTYStopsDescendantsBeforeReturning pins the cleanup behind TCL-1045.
//
// Copilot starts helpers that inherit its PTY session. Killing only the CLI
// parent left those helpers free to keep writing into the fixture's t.TempDir;
// on macOS, testing's concurrent RemoveAll then failed with "directory not
// empty" even though every permission assertion had passed. The heartbeat is
// the deterministic form of that race: it must stop before RunPTY returns.
func TestRunPTYStopsDescendantsBeforeReturning(t *testing.T) {
	heartbeat := filepath.Join(t.TempDir(), "descendant-heartbeat")
	fakeCopilot(t, `
(
  while :; do
    printf x >> "$HEARTBEAT"
    sleep 0.02
  done
) &
echo "descendant=$!"
echo ready
sleep 60`)

	run := fakeRunOptions(t)
	run.ExtraEnv = []string{"HEARTBEAT=" + heartbeat}
	res := copilotfixture.RunPTY(t, copilotfixture.PTYOptions{
		RunOptions:   run,
		Deadline:     30 * time.Second,
		BlockedAfter: 3 * time.Second,
	})

	var descendantPID int
	for _, field := range strings.Fields(res.TranscriptText()) {
		if value, ok := strings.CutPrefix(field, "descendant="); ok {
			descendantPID, _ = strconv.Atoi(strings.TrimSpace(value))
			break
		}
	}
	if descendantPID > 0 {
		t.Cleanup(func() {
			process, err := os.FindProcess(descendantPID)
			if err == nil {
				_ = process.Kill()
			}
		})
	}

	require.True(t, res.Blocked, "the fake must exercise the early-stop path")
	require.NotZero(t, descendantPID, "the fake must disclose its background writer pid")
	before, err := os.Stat(heartbeat)
	require.NoError(t, err)
	require.Positive(t, before.Size(), "the descendant must have written before teardown")
	time.Sleep(250 * time.Millisecond)
	after, err := os.Stat(heartbeat)
	require.NoError(t, err)
	assert.Equal(t, before.Size(), after.Size(),
		"RunPTY returned while a descendant could still mutate fixture state")
}

// TestRunPTYSettledBeatsBlockedAfter pins the precedence between the two early
// exits, on a run that satisfies both at the same instant.
//
// Getting this backwards would be the worst failure the file can have: a
// scenario whose evidence HAD arrived, recorded as blocked.
func TestRunPTYSettledBeatsBlockedAfter(t *testing.T) {
	fakeCopilot(t, "echo ready; sleep 60")

	res := copilotfixture.RunPTY(t, copilotfixture.PTYOptions{
		RunOptions:   fakeRunOptions(t),
		Deadline:     30 * time.Second,
		BlockedAfter: 3 * time.Second,
		// Already true, so the only thing either exit waits for is quiescence.
		SettledWhen: func() bool { return true },
	})

	assert.True(t, res.Settled, "evidence present at the tick that ended the run")
	assert.False(t, res.Blocked, "a settled run is never also blocked")
}

// TestRunPTYDoesNotCallASilentStartupBlocked is the guard that keeps a loaded
// machine from being reported as a permission finding.
//
// The fake says nothing for well past BlockedAfter and then produces its
// evidence. A blank screen is quiet by exactly the test a finished prompt is,
// so without the first-output gate this run ends as "blocked" — a false
// finding produced by a busy runner rather than by the CLI.
func TestRunPTYDoesNotCallASilentStartupBlocked(t *testing.T) {
	fakeCopilot(t, "sleep 4; echo ready; sleep 60")

	res := copilotfixture.RunPTY(t, copilotfixture.PTYOptions{
		RunOptions:   fakeRunOptions(t),
		Deadline:     30 * time.Second,
		BlockedAfter: 2 * time.Second,
		SettledWhen:  func() bool { return true },
	})

	assert.True(t, res.Settled, "the run must survive its own silent startup")
	assert.False(t, res.Blocked, "silence before the first byte is startup, not a prompt")
	assert.Greater(t, res.FirstOutput, 3*time.Second,
		"the fake was silent for 4s; that belongs in FirstOutput")
	assert.Less(t, res.MaxOutputGap, 3*time.Second,
		"startup silence must stay OUT of the gap that sizes BlockedAfter, or a future "+
			"tightening argues from a number no working turn produced")
}

// TestRunPTYWaitsForOutputBeforeTyping pins the fix for a race that cost a real
// CI run: Input delivered into a terminal whose TUI had not started.
//
// The fake is silent past PTYQuiescence, then echoes whatever it is handed. The
// unguarded version typed at the first quiet sample — before the reader existed
// to receive it — and the bytes were gone. Asserting the echo is what
// distinguishes "typed at the right moment" from "typed at all".
func TestRunPTYWaitsForOutputBeforeTyping(t *testing.T) {
	fakeCopilot(t, "sleep 4; echo ready; head -n 1")

	res := copilotfixture.RunPTY(t, copilotfixture.PTYOptions{
		RunOptions: fakeRunOptions(t),
		Deadline:   30 * time.Second,
		Input:      []string{"typed-after-startup"},
	})

	assert.Contains(t, res.TranscriptText(), "typed-after-startup",
		"input typed before the CLI drew anything is discarded, which is how this "+
			"failed in production; transcript=%q", res.TranscriptText())
}
