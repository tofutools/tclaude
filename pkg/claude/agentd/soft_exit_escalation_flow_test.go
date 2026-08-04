package agentd_test

import (
	"sync"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Flow coverage for the soft-exit escalation ladder (TCL-1001).
//
// The bug it closes was reported from production: two Copilot agents were
// retired, their panes kept running, and 60 s later the daemon logged
// "agent-owned directories kept because agent did not exit within grace" —
// because a soft exit that never lands had nothing following it. The ladder
// now converges every stop: bounded re-injection, then tmux kill, then
// SIGTERM, then SIGKILL, each step guarded by the frozen pane identity.
//
// These scenarios drive it through the daemon mux with the tmux simulator, and
// deliberately assert on the tmux MUTATIONS (which kill hit which target) and
// the recorded attribution rather than on internal state.

// killTargets returns every target the daemon asked tmux to kill.
func killTargets(f *testharness.Flow) []string {
	targets := f.World.Tmux.MutationTargets("kill-pane")
	return append(targets, f.World.Tmux.MutationTargets("kill-session")...)
}

// Scenario: the ordinary case. A pane that honours its soft exit is never
// escalated — no kill of any kind reaches tmux, so a healthy agent keeps its
// graceful shutdown (and, for a real harness, whatever end-of-session state it
// writes on the way out).
func TestSoftExitEscalation_LandedExitIsNeverEscalated(t *testing.T) {
	f := newFlow(t)

	const conv = "esca-1111-2222-3333-4444"
	const tmuxSes = "tmux-esca"
	f.HaveConvWithTitle(conv, "clean-worker")
	f.HaveAliveSession(conv, "spwn-esca", tmuxSes, f.TestCwd("esca"))

	f.AssertSoftStopped(f.AsHuman().Stop(conv, false))
	agentd.WaitForBackgroundForTest()

	assert.False(t, f.World.Tmux.IsAlive(tmuxSes), "the soft exit ends the pane on its own")
	assert.Empty(t, killTargets(f),
		"a pane that honoured /exit must never be killed on top of it")

	d, err := db.Open()
	require.NoError(t, err)
	var exitReason string
	require.NoError(t, d.QueryRow("SELECT COALESCE(exit_reason, '') FROM sessions WHERE id = ?",
		"spwn-esca").Scan(&exitReason))
	assert.NotEqual(t, "daemon_kill", exitReason,
		"a graceful exit must not be recorded as a daemon kill")
}

// Scenario: the exit is delivered, the pane ignores it. At the deadline the
// ladder's first rung kills the pane — targeting the exact pane id it froze,
// never the session name, so a resume that reused the name cannot be hit.
func TestSoftExitEscalation_KillsPaneThatNeverExits(t *testing.T) {
	f := newFlow(t)

	const conv = "escb-1111-2222-3333-4444"
	const tmuxSes = "tmux-escb"
	f.HaveConvWithTitle(conv, "wedged-worker")
	f.HaveAliveSession(conv, "spwn-escb", tmuxSes, f.TestCwd("escb"))
	f.World.Tmux.SetPaneIdentityForTest(tmuxSes, "%91", 9191)

	cc := f.World.CCs.GetByConvID(conv)
	require.NotNil(t, cc)
	cc.OnInput("/exit", func(c *testharness.CCSim, _ string) bool {
		_ = c.WriteUserTurn("[wedged: /exit ignored]")
		return true // consume; never MarkDead
	})

	f.AssertSoftStopped(f.AsHuman().Stop(conv, false))
	agentd.WaitForBackgroundForTest()

	assert.False(t, f.World.Tmux.IsAlive(tmuxSes),
		"a delivered exit that never closes the pane must be escalated to a kill")
	assert.Contains(t, killTargets(f), "%91",
		"the escalated kill must target the frozen pane id, not the session name")
}

// Scenario: the injection itself failed (send-keys error), so there is no
// delivered exit to wait for. The stop still reports the send failure — the
// established contract — but the pane no longer survives it.
func TestSoftExitEscalation_KillsPaneWhoseInjectionFailed(t *testing.T) {
	f := newFlow(t)

	const conv = "escc-1111-2222-3333-4444"
	const tmuxSes = "tmux-escc"
	f.HaveConvWithTitle(conv, "unreachable-worker")
	f.HaveAliveSession(conv, "spwn-escc", tmuxSes, f.TestCwd("escc"))
	f.World.Tmux.FailNextCommand("send-keys")

	stop := f.AsHuman().Stop(conv, false)
	assert.Equal(t, "error", stop.Action, "a failed send-keys still reports the failure")
	agentd.WaitForBackgroundForTest()

	assert.False(t, f.World.Tmux.IsAlive(tmuxSes),
		"a stop whose exit command never reached the pane must still end the pane")
	assert.NotEmpty(t, killTargets(f))

	d, err := db.Open()
	require.NoError(t, err)
	var intent, exitReason string
	require.NoError(t, d.QueryRow(
		"SELECT exit_intent, COALESCE(exit_reason, '') FROM sessions WHERE id = ?",
		"spwn-escc").Scan(&intent, &exitReason))
	assert.Equal(t, db.AgentExitActionStop, intent,
		"the escalation re-arms the attribution the failed injection cleared")
	assert.Equal(t, "daemon_kill", exitReason)
}

// Scenario: tmux reports the kill succeeded and the process is still there —
// the case the signal rungs exist for. SIGTERM goes first and is given its
// grace; only when the process outlives that does SIGKILL follow.
func TestSoftExitEscalation_SignalsProcessGroupWhenTmuxKillIsInsufficient(t *testing.T) {
	var (
		mu      sync.Mutex
		signals []syscall.Signal
		pids    []int
	)
	f := newFlow(t)

	const conv = "escd-1111-2222-3333-4444"
	const tmuxSes = "tmux-escd"
	f.HaveConvWithTitle(conv, "unkillable-worker")
	f.HaveAliveSession(conv, "spwn-escd", tmuxSes, f.TestCwd("escd"))
	f.World.Tmux.SetPaneIdentityForTest(tmuxSes, "%92", 9292)
	f.World.Tmux.SetKillResistantForTest(tmuxSes, true)

	// The process survives everything up to and including SIGTERM; SIGKILL
	// ends it, and takes the tmux session with it exactly as a real pane
	// process's death does.
	restoreProcess := agentd.SetSoftExitEscalationProcessForTest(
		func(pid int) bool {
			mu.Lock()
			defer mu.Unlock()
			for _, s := range signals {
				if s == syscall.SIGKILL {
					return false
				}
			}
			return true
		},
		func(pid int, sig syscall.Signal) error {
			mu.Lock()
			signals = append(signals, sig)
			pids = append(pids, pid)
			mu.Unlock()
			if sig == syscall.SIGKILL {
				f.World.Tmux.KillBySignalForTest(tmuxSes)
			}
			return nil
		},
	)
	t.Cleanup(func() { restoreAfterBackgroundDrain(restoreProcess) })

	cc := f.World.CCs.GetByConvID(conv)
	require.NotNil(t, cc)
	cc.OnInput("/exit", func(c *testharness.CCSim, _ string) bool {
		_ = c.WriteUserTurn("[unkillable: /exit ignored]")
		return true
	})

	f.AssertSoftStopped(f.AsHuman().Stop(conv, false))
	agentd.WaitForBackgroundForTest()

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL}, signals,
		"the ladder must give SIGTERM its turn before SIGKILL")
	assert.Equal(t, []int{9292, 9292}, pids,
		"signals go to the frozen pane process, not to whatever pid is current")
	assert.False(t, f.World.Tmux.IsAlive(tmuxSes))
}

// Scenario: the pane exited and a resume relaunched the conversation under the
// same tmux name before the deadline — the very race the ladder must not lose.
// A watchdog that killed on name alone would execute the fresh agent.
func TestSoftExitEscalation_StandsDownForASuccessorPane(t *testing.T) {
	f := newFlow(t)

	const conv = "esce-1111-2222-3333-4444"
	const tmuxSes = "tmux-esce"
	f.HaveConvWithTitle(conv, "replaced-worker")
	f.HaveAliveSession(conv, "spwn-esce", tmuxSes, f.TestCwd("esce"))
	f.World.Tmux.SetPaneIdentityForTest(tmuxSes, "%93", 9393)

	cc := f.World.CCs.GetByConvID(conv)
	require.NotNil(t, cc)
	cc.OnInput("/exit", func(c *testharness.CCSim, _ string) bool {
		_ = c.WriteUserTurn("[wedged: /exit ignored]")
		return true
	})

	// Between the stop and the deadline the pane identity changes, which is
	// what a resume reusing the conv-id-derived tmux name looks like from the
	// watchdog's side.
	var once sync.Once
	restorePoll := agentd.SetSoftExitEscalationPollForTest(func() {
		once.Do(func() {
			f.World.Tmux.SetPaneIdentityForTest(tmuxSes, "%94", 9494)
		})
	})
	t.Cleanup(func() { restoreAfterBackgroundDrain(restorePoll) })

	f.AssertSoftStopped(f.AsHuman().Stop(conv, false))
	agentd.WaitForBackgroundForTest()

	assert.True(t, f.World.Tmux.IsAlive(tmuxSes),
		"the successor pane must survive its predecessor's escalation")
	assert.Empty(t, killTargets(f),
		"a changed pane identity stands the ladder down instead of killing a stranger")
}
