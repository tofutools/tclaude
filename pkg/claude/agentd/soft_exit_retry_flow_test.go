package agentd_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Flow coverage for the soft-exit re-injection retry (injectSoftExit /
// scheduleSoftExitRetry). A single /exit can be silently lost when the
// pane's input buffer wasn't empty: send-keys appends the command to
// whatever junk was sitting there, so the trailing Enter submits
// "<junk>/exit" as one ordinary prompt instead of an exit, and the pane
// keeps running. The daemon now backgrounds a retry that re-injects /exit
// while the SAME pane is still alive — the submit above cleared the
// buffer, so the second attempt lands clean and takes.
//
// The simulator models the bug faithfully: CCSim.Receive buffers
// keystrokes until an Enter arrives, so a junk fragment fed WITHOUT a
// trailing Enter sits in the input buffer exactly as a half-typed line
// would in a real pane. The default /exit handler matches on the "/exit"
// prefix, so "<junk>/exit" misses it and falls through to the user-turn
// catch-all — the pane stays alive — while a clean "/exit" triggers
// MarkDead.

// countExitSends returns how many times exactly `cmd` was typed into
// target's pane via send-keys. Each soft-exit injection sends the command
// text as one send-keys call (followed by two Enters), so this counts the
// distinct soft-exit attempts the daemon made into that pane.
func countExitSends(f *testharness.Flow, target, cmd string) int {
	n := 0
	for _, sk := range f.World.Tmux.Sent() {
		if sk.Target == target && sk.Text == cmd {
			n++
		}
	}
	return n
}

// Scenario: the agent's input buffer holds a half-typed leftover when the
// daemon soft-stops it. The first /exit is scrambled into a no-op prompt;
// the background retry re-injects onto the now-clean buffer and the pane
// finally exits. This is the bug the retry exists to recover from.
func TestSoftExit_RetryRecoversJunkScrambledExit(t *testing.T) {
	f := newFlow(t)

	const conv = "sxja-1111-2222-3333-4444"
	const tmuxSes = "tmux-sxja"
	const target = tmuxSes + ":0.0"
	f.HaveConvWithTitle(conv, "junk-buffer-worker")
	f.HaveAliveSession(conv, "spwn-sxja", tmuxSes, "/tmp/sxja")

	// Pre-existing junk in the pane's input buffer — a half-typed line left
	// unsent. NO trailing Enter, so it sits in the buffer; the daemon's
	// /exit gets appended to it.
	cc := f.World.CCs.GetByConvID(conv)
	require.NotNil(t, cc, "CCSim for the live agent")
	cc.Receive("half-typed leftover ")

	stop := f.AsHuman().Stop(conv, false)
	f.AssertSoftStopped(stop)

	// Drain the background retry goroutine, then assert the recovery landed.
	agentd.WaitForBackgroundForTest()

	assert.False(t, f.World.Tmux.IsAlive(tmuxSes),
		"the soft-exit retry must bring down a pane whose first /exit was scrambled by buffer junk")

	// Proof the RETRY did it, not the first attempt: /exit was typed more
	// than once. The first landed on "<junk>/exit" (a no-op prompt); the
	// second on a clean buffer (the real exit).
	assert.GreaterOrEqual(t, countExitSends(f, target, "/exit"), 2,
		"daemon must have re-injected /exit after the first was lost to buffer junk")
}

// Scenario: a pane with an empty input buffer honours the very first
// /exit. The retry must NOT pile on extra /exit injections once the pane
// is already gone — a clean stop stays a single attempt.
func TestSoftExit_NoRetryWhenFirstExitSucceeds(t *testing.T) {
	f := newFlow(t)

	const conv = "sxjb-1111-2222-3333-4444"
	const tmuxSes = "tmux-sxjb"
	const target = tmuxSes + ":0.0"
	f.HaveConvWithTitle(conv, "clean-worker")
	f.HaveAliveSession(conv, "spwn-sxjb", tmuxSes, "/tmp/sxjb")

	stop := f.AsHuman().Stop(conv, false)
	f.AssertSoftStopped(stop)
	agentd.WaitForBackgroundForTest()

	assert.False(t, f.World.Tmux.IsAlive(tmuxSes),
		"a clean /exit brings the pane down on the first attempt")
	assert.Equal(t, 1, countExitSends(f, target, "/exit"),
		"a pane that exits on the first /exit must not be re-injected")
}

// Scenario: a wedged pane that ignores /exit entirely. The retry must be
// BOUNDED — it cannot type /exit at the pane forever; the force-kill
// fallback (escalateShutdown, covered in power_flow_test) owns finishing a
// genuinely hung pane.
func TestSoftExit_BoundedRetriesForHungPane(t *testing.T) {
	f := newFlow(t)

	const conv = "sxjc-1111-2222-3333-4444"
	const tmuxSes = "tmux-sxjc"
	const target = tmuxSes + ":0.0"
	f.HaveConvWithTitle(conv, "hung-worker")
	f.HaveAliveSession(conv, "spwn-sxjc", tmuxSes, "/tmp/sxjc")

	// A pane that consumes /exit without ever flipping dead (CC wedged).
	cc := f.World.CCs.GetByConvID(conv)
	require.NotNil(t, cc)
	cc.OnInput("/exit", func(c *testharness.CCSim, _ string) bool {
		_ = c.WriteUserTurn("[hung: /exit ignored]")
		return true // consume; never MarkDead
	})

	stop := f.AsHuman().Stop(conv, false)
	f.AssertSoftStopped(stop)
	agentd.WaitForBackgroundForTest()

	assert.True(t, f.World.Tmux.IsAlive(tmuxSes),
		"a pane that ignores /exit stays alive — the soft path can't force it")
	// Bounded: 1 initial attempt + 2 retries (softExitMaxAttempts = 3) = 3
	// total. Guards against an unbounded re-injection loop into a wedged pane.
	assert.Equal(t, 3, countExitSends(f, target, "/exit"),
		"soft-exit attempts must be capped (initial + retries), not infinite")
}

// Scenario: the regression the live-PID guard exists to prevent. After a
// soft-stop whose first /exit was scrambled, the original pane exits and a
// brand-new agent process comes up REUSING the same tmux name — exactly
// what a production resume does, since `session new -r` derives the tmux
// name from the conv-id ([:8]) with no --label. The pending retry must NOT
// type /exit at that innocent, freshly-resumed pane (which would kill it
// and drop its input). The guard keys on the tmux pane's live OS pid, which
// is fresh for the new process, so the retry recognises "not my pane" and
// aborts.
func TestSoftExit_RetryDoesNotExitResumedPaneReusingTmuxName(t *testing.T) {
	f := newFlow(t)
	// Give the retry a real (if short) window so the test can stage the
	// resume-reuses-the-name race deterministically before the retry fires.
	t.Cleanup(agentd.SetSoftExitRetryDelayForTest(150 * time.Millisecond))

	const conv = "sxjd-1111-2222-3333-4444"
	const tmuxSes = "tmux-sxjd"
	const target = tmuxSes + ":0.0"
	const cwd = "/tmp/sxjd"
	f.HaveConvWithTitle(conv, "resumed-worker")
	f.HaveAliveSession(conv, "spwn-sxjd", tmuxSes, cwd)

	// Junk in the buffer scrambles the first /exit, so the pane stays alive
	// and the retry is armed.
	cc := f.World.CCs.GetByConvID(conv)
	require.NotNil(t, cc)
	cc.Receive("half-typed leftover ")

	stop := f.AsHuman().Stop(conv, false)
	f.AssertSoftStopped(stop)

	// Stage the resume: the original pane exits and a fresh CCSim (a new
	// process → new pane pid) re-registers under the SAME tmux name, modelling
	// `session new -r`'s conv-id-derived name collision.
	cc.MarkDead()
	resumed := testharness.NewCCSimWithID(t, f.World.HomeDir, conv, cwd)
	require.NoError(t, resumed.Start())
	f.World.Tmux.Register(tmuxSes, cwd, resumed)

	agentd.WaitForBackgroundForTest()

	assert.True(t, f.World.Tmux.IsAlive(tmuxSes),
		"the resumed pane must survive — the retry must not /exit a new process that reused the tmux name")
	// The only /exit to this name was the original (scrambled) attempt; the
	// retry recognised the pid change and never re-injected.
	assert.Equal(t, 1, countExitSends(f, target, "/exit"),
		"retry must abort once a different process owns the tmux name")
}
