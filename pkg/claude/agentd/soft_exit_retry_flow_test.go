package agentd_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
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
		// Lifecycle soft-stop now targets the immutable pane id (%N), while
		// older harness paths retain the session target. These scenarios have
		// one pane, so count the command regardless of target spelling.
		if sk.Text == cmd {
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
	f.HaveAliveSession(conv, "spwn-sxja", tmuxSes, f.TestCwd("sxja"))

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
	f.HaveAliveSession(conv, "spwn-sxjb", tmuxSes, f.TestCwd("sxjb"))

	stop := f.AsHuman().Stop(conv, false)
	f.AssertSoftStopped(stop)
	agentd.WaitForBackgroundForTest()

	assert.False(t, f.World.Tmux.IsAlive(tmuxSes),
		"a clean /exit brings the pane down on the first attempt")
	assert.Equal(t, 1, countExitSends(f, target, "/exit"),
		"a pane that exits on the first /exit must not be re-injected")
	var exitTarget string
	for _, sent := range f.World.Tmux.Sent() {
		if sent.Text == "/exit" {
			exitTarget = sent.Target
			break
		}
	}
	assert.Equal(t, "%1", exitTarget,
		"successful lifecycle send must target the exact pane id, not session name")
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
	f.HaveAliveSession(conv, "spwn-sxjc", tmuxSes, f.TestCwd("sxjc"))

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
	d, err := db.Open()
	require.NoError(t, err)
	var intent, eventID string
	require.NoError(t, d.QueryRow(`SELECT exit_intent, exit_intent_event_id FROM sessions WHERE id = ?`,
		"spwn-sxjc").Scan(&intent, &eventID))
	assert.Empty(t, intent, "a successful send that never exits cannot leave reusable intent")
	assert.Empty(t, eventID)
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
	cwd := f.TestCwd("sxjd")
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

// A selected predecessor must not send bytes to a successor that reuses the
// same conversation/tmux name while the stop is between selection and its
// exact-pane revalidation.
func TestSoftExit_SelectedPaneSwapSendsZeroBytesToSuccessor(t *testing.T) {
	f := newFlow(t)
	const conv = "sxje-1111-2222-3333-4444"
	const tmuxSes = "tmux-sxje"
	cwd := f.TestCwd("sxje")
	f.HaveConvWithTitle(conv, "swap-worker")
	f.HaveAliveSession(conv, "spwn-sxje", tmuxSes, cwd)
	cc := f.World.CCs.GetByConvID(conv)
	require.NotNil(t, cc)
	cleanup := agentd.SetBeforeSoftExitTargetRevalidateForTest(func() {
		cc.MarkDead()
		resumed := testharness.NewCCSimWithID(t, f.World.HomeDir, conv, cwd)
		require.NoError(t, resumed.Start())
		f.World.Tmux.Register(tmuxSes, cwd, resumed)
	})
	t.Cleanup(cleanup)

	assert.Equal(t, "error", f.AsHuman().Stop(conv, false).Action)
	assert.True(t, f.World.Tmux.IsAlive(tmuxSes), "successor must remain alive")
	assert.Equal(t, 0, countExitSends(f, tmuxSes+":0.0", "/exit"), "successor receives zero /exit bytes")
}

func TestSoftExit_InitialProbeUnknownPreservesDeliveryWithoutRetry(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetSoftExitRetryDelayForTest(10 * time.Millisecond))
	const conv = "sxjf-1111-2222-3333-4444"
	const tmuxSes = "tmux-sxjf"
	f.HaveConvWithTitle(conv, "unknown-probe")
	f.HaveAliveSession(conv, "spwn-sxjf", tmuxSes, f.TestCwd("sxjf"))
	cc := f.World.CCs.GetByConvID(conv)
	require.NotNil(t, cc)
	cc.OnInput("/exit", func(c *testharness.CCSim, _ string) bool { return true })
	cleanup := agentd.SetAfterSoftExitTargetSendForTest(func() { f.World.Tmux.FailNextCommand("display-message") })
	t.Cleanup(cleanup)
	stop := f.AsHuman().Stop(conv, false)
	f.AssertSoftStopped(stop)
	agentd.WaitForBackgroundForTest()
	assert.Equal(t, 1, countExitSends(f, tmuxSes+":0.0", "/exit"), "unknown probe must never trigger retry reinjection")
}

func TestSoftExit_PreSendUnknownSendsZeroAndErrors(t *testing.T) {
	f := newFlow(t)
	const conv = "sxjg-1111-2222-3333-4444"
	const tmuxSes = "tmux-sxjg"
	f.HaveConvWithTitle(conv, "pre-unknown")
	f.HaveAliveSession(conv, "spwn-sxjg", tmuxSes, f.TestCwd("sxjg"))
	cleanup := agentd.SetBeforeSoftExitTargetRevalidateForTest(func() { f.World.Tmux.FailNextCommand("display-message") })
	t.Cleanup(cleanup)
	assert.Equal(t, "error", agentd.StopOneConvWithIntentForTest(conv, db.AgentExitActionStop))
	d, err := db.Open()
	require.NoError(t, err)
	var intent string
	require.NoError(t, d.QueryRow(`SELECT exit_intent FROM sessions WHERE id = 'spwn-sxjg'`).Scan(&intent))
	assert.Empty(t, intent)
	assert.True(t, f.World.Tmux.IsAlive(tmuxSes))
	assert.Equal(t, 0, countExitSends(f, tmuxSes+":0.0", "/exit"))
}

func TestSoftExit_RetryUnknownCleansWithoutSend(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetSoftExitRetryDelayForTest(5 * time.Millisecond))
	const conv = "sxjh-1111-2222-3333-4444"
	const tmuxSes = "tmux-sxjh"
	f.HaveConvWithTitle(conv, "retry-unknown")
	f.HaveAliveSession(conv, "spwn-sxjh", tmuxSes, f.TestCwd("sxjh"))
	cc := f.World.CCs.GetByConvID(conv)
	require.NotNil(t, cc)
	cc.OnInput("/exit", func(c *testharness.CCSim, _ string) bool { return true })
	cleanup := agentd.SetBeforeSoftExitTargetRetryProbeForTest(func(attempt int) {
		if attempt == 2 {
			f.World.Tmux.FailNextCommand("display-message")
		}
	})
	t.Cleanup(cleanup)
	stop := f.AsHuman().Stop(conv, false)
	f.AssertSoftStopped(stop)
	agentd.WaitForBackgroundForTest()
	assert.Equal(t, 1, countExitSends(f, tmuxSes+":0.0", "/exit"))
}

func TestSoftExit_FinalUnknownCleansBounded(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetSoftExitRetryDelayForTest(5 * time.Millisecond))
	const conv = "sxji-1111-2222-3333-4444"
	const tmuxSes = "tmux-sxji"
	f.HaveConvWithTitle(conv, "final-unknown")
	f.HaveAliveSession(conv, "spwn-sxji", tmuxSes, f.TestCwd("sxji"))
	cc := f.World.CCs.GetByConvID(conv)
	require.NotNil(t, cc)
	cc.OnInput("/exit", func(c *testharness.CCSim, _ string) bool { return true })
	cleanup := agentd.SetBeforeSoftExitTargetRetryProbeForTest(func(attempt int) {
		if attempt == 3 {
			f.World.Tmux.FailNextCommand("display-message")
		}
	})
	t.Cleanup(cleanup)
	stop := f.AsHuman().Stop(conv, false)
	f.AssertSoftStopped(stop)
	agentd.WaitForBackgroundForTest()
	assert.Equal(t, 2, countExitSends(f, tmuxSes+":0.0", "/exit"))
}

func TestForceStop_SelectedPaneSwapDoesNotKillSuccessor(t *testing.T) {
	f := newFlow(t)
	const conv = "sxjk-1111-2222-3333-4444"
	const tmuxSes = "tmux-sxjk"
	cwd := f.TestCwd("sxjk")
	f.HaveConvWithTitle(conv, "force-swap")
	f.HaveAliveSession(conv, "spwn-sxjk", tmuxSes, cwd)
	cc := f.World.CCs.GetByConvID(conv)
	require.NotNil(t, cc)
	cleanup := agentd.SetBeforeSoftExitTargetRevalidateForTest(func() {
		cc.MarkDead()
		resumed := testharness.NewCCSimWithID(t, f.World.HomeDir, conv, cwd)
		require.NoError(t, resumed.Start())
		f.World.Tmux.Register(tmuxSes, cwd, resumed)
	})
	t.Cleanup(cleanup)
	stop := f.AsHuman().Stop(conv, true)
	assert.Equal(t, "error", stop.Action)
	assert.True(t, f.World.Tmux.IsAlive(tmuxSes), "successor remains alive and receives zero kill operations")
	assert.Empty(t, f.World.Tmux.MutationTargets("kill-pane"))
	assert.Empty(t, f.World.Tmux.MutationTargets("kill-session"))
}
