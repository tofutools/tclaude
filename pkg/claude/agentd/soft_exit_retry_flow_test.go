package agentd_test

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Flow coverage for the soft-exit re-injection retry (injectSoftExitTarget /
// scheduleSoftExitRetryTarget). Claude Code's soft exit is the keystroke-free
// signal sequence [Escape, C-c, C-c, C-c] (claudeLifecycle.SignalExitKeys,
// TCL-1137): each attempt is one lock-held send, its leading Escape clears any
// pending line or dialog, and the ctrl-c presses arm and quit. The daemon
// backgrounds a bounded retry that re-sends the sequence while the SAME pane is
// still alive, then escalates to a kill if the pane never dies.
//
// The simulator models the pane faithfully: CCSim.Receive arms on an idle C-c
// and exits on the next armed C-c (the same shutdown as /exit), while a wedged
// pane that reads the presses without quitting is modelled via
// CCSim.SetSignalExitWedged — the state the retry + escalation ladder exists to
// handle. Attempts are counted by countSoftExitAttempts (one Escape per send).

// countSoftExitAttempts returns how many soft-exit attempts the daemon made
// into target's pane. These scenarios run Claude Code, whose soft exit is now
// the keystroke-free signal sequence [Escape, C-c, C-c, C-c] (see
// claudeLifecycle.SignalExitKeys), delivered as one lock-held send per attempt.
// Every attempt leads with exactly one Escape, so counting Escape sends counts
// the distinct attempts — the signal-exit analog of the old "count typed /exit"
// tally.
func countSoftExitAttempts(f *testharness.Flow, target string) int {
	return countKeySends(f, target, "Escape")
}

// countKeySends returns how many times exactly `key` was sent into target's
// pane via send-keys.
func countKeySends(f *testharness.Flow, target, key string) int {
	n := 0
	for _, sk := range f.World.Tmux.Sent() {
		// Lifecycle soft-stop now targets the immutable pane id (%N), while
		// older harness paths retain the session target. These scenarios have
		// one pane, so count the key regardless of target spelling.
		if sk.Text == key {
			n++
		}
	}
	return n
}

// Scenario: the agent's input buffer holds a half-typed leftover when the
// daemon soft-stops it. The typed-/exit era needed a background retry here: the
// first /exit was appended to the junk and scrambled into a no-op prompt, and
// only a re-injection onto the now-clean buffer exited. The keystroke-free
// signal exit removes that failure mode entirely — its leading Escape clears
// the buffer before the ctrl-c presses land, so a single first attempt exits.
// This is the robustness win TCL-1137 buys, asserted where the old bug lived.
func TestSoftExit_SignalExitClearsJunkBufferOnFirstAttempt(t *testing.T) {
	f := newFlow(t)

	const conv = "sxja-1111-2222-3333-4444"
	const tmuxSes = "tmux-sxja"
	const target = tmuxSes + ":0.0"
	f.HaveConvWithTitle(conv, "junk-buffer-worker")
	f.HaveAliveSession(conv, "spwn-sxja", tmuxSes, f.TestCwd("sxja"))

	// Pre-existing junk in the pane's input buffer — a half-typed line left
	// unsent. NO trailing Enter, so it sits in the buffer; the signal exit's
	// Escape clears it before the ctrl-c presses arm and quit.
	cc := f.World.CCs.GetByConvID(conv)
	require.NotNil(t, cc, "CCSim for the live agent")
	cc.Receive("half-typed leftover ")

	stop := f.AsHuman().Stop(conv, false)
	f.AssertSoftStopped(stop)
	agentd.WaitForBackgroundForTest()

	assert.False(t, f.World.Tmux.IsAlive(tmuxSes),
		"the signal exit must bring down a pane despite pre-existing buffer junk")
	// No retry was needed: the Escape-led sequence handled the junk in one
	// attempt, unlike the typed /exit that used to be scrambled by it.
	assert.Equal(t, 1, countSoftExitAttempts(f, target),
		"the signal exit clears buffer junk in a single first attempt — no re-injection")
	// Pin Escape's role specifically: it must be sent, and BEFORE the first
	// C-c, so the buffer is cleared before the ctrl-c presses arm and quit.
	var idxEscape, idxFirstCC = -1, -1
	for i, sk := range f.World.Tmux.Sent() {
		if sk.Text == "Escape" && idxEscape == -1 {
			idxEscape = i
		}
		if sk.Text == "C-c" && idxFirstCC == -1 {
			idxFirstCC = i
		}
	}
	require.NotEqual(t, -1, idxEscape, "the signal exit must send an Escape")
	require.NotEqual(t, -1, idxFirstCC, "the signal exit must send a C-c")
	assert.Less(t, idxEscape, idxFirstCC,
		"Escape must precede the first C-c so buffer junk is cleared before the quit presses")
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
	f.World.Tmux.SetPaneIdentityForTest(tmuxSes, "%77", 4242)

	stop := f.AsHuman().Stop(conv, false)
	f.AssertSoftStopped(stop)
	agentd.WaitForBackgroundForTest()

	assert.False(t, f.World.Tmux.IsAlive(tmuxSes),
		"a clean /exit brings the pane down on the first attempt")
	assert.Equal(t, 1, countSoftExitAttempts(f, target),
		"a pane that exits on the first /exit must not be re-injected")
	var exitTarget string
	for _, sent := range f.World.Tmux.Sent() {
		if sent.Text == "Escape" {
			exitTarget = sent.Target
			break
		}
	}
	assert.Equal(t, "%77", exitTarget,
		"successful lifecycle send must target the exact pane id, not session name")
}

// THE LADDER AND THE WATCHDOG ARE TWO CONCURRENT ACTORS, AND NOTHING ORDERS
// THEM. Every scenario below has to reckon with that.
//
// A soft stop starts the bounded re-injection ladder AND, since TCL-1001, an
// escalation watchdog. In production the two are close: the ladder's attempts
// land at ~softExitRetryDelay × softExitMaxAttempts (plus, since TCL-1137, each
// signal-exit attempt's own lock-held key spacing — Claude Code's four-key
// [Escape, C-c, C-c, C-c] adds 3×injectSettleDelay ≈ 1.5 s per attempt), and
// the watchdog's deadline is 10 s. The final retry can therefore land at or
// just past the deadline, which is benign: a pane responsive enough to honour a
// ctrl-c quits on the FIRST attempt (a signal is handled even when the keypress
// reader is wedged — the whole premise of the signal exit), so the escalation
// only ever preempts a truly wedged pane, whose kill is the correct outcome.
// The flow fixture shrinks both — 1 ms
// retries against a 20 ms deadline — which keeps the RATIO but throws away
// the absolute headroom. Scheduler jitter is absolute: a goroutine that loses
// its P for 20 ms on a loaded runner reorders these two in a way it can never
// reorder 8 s and 10 s. That is why TCL-1028 is intermittent and why it was
// seen on macOS runners, and it bites in both directions:
//
//   - The watchdog OUTRUNS the ladder. Its deadline expires mid-ladder, it
//     kills the pane, and the ladder's next probe reads dead and returns
//     early. A scenario expecting all 3 attempts gets 2
//     (TestLifecycleStop_PaneGenerationBinding/degraded_soft_control).
//   - The watchdog STEALS the ladder's fault. It re-probes with
//     display-message on a 1 ms interval, and TmuxSim's queued faults belong
//     to the verb rather than to a caller, so a fault a scenario queued for
//     one specific retry probe is taken by whichever probe runs next. The
//     targeted probe then succeeds, the abort it was supposed to trigger
//     never happens, and the ladder runs to its full 3 attempts (TCL-1028's
//     own symptom: expected 1, got 3).
//
// Both were reproduced by shifting the relative timing a few milliseconds —
// which is all a descheduled goroutine does — and both are fixed the same
// way, by ORDERING the two actors instead of hoping. The scenarios that care
// park the watchdog on its first probe (holdSoftExitEscalation), wait for the
// ladder to reach the state under test, and only then let the watchdog run.
// Nothing about the ladder or the watchdog is disabled: the escalation still
// happens, still kills, and is still asserted — it just no longer happens at
// an arbitrary point inside the thing being measured.
//
// Do NOT "fix" a recurrence here by changing an expected attempt count or
// widening a bound. Both directions are reachable, which makes the count an
// effect; tuning it hides whichever direction is currently quiet.

// awaitLadderThenRelease is the ordering the two actors need.
//
// It waits for `reached` — the ladder state the scenario is about — and only
// then lets the parked watchdog run, so the escalation happens after the
// measurement rather than through it. It deliberately does NOT drain: a
// scenario that reads state inside the bounded observer window has to release
// the watchdog without also waiting for that window to close. Callers drain
// where they always did.
//
// `what` is reported when the ladder never gets there, which means the
// scenario has stopped driving the engine it was written for and is no longer
// proving anything.
func awaitLadderThenRelease(t *testing.T, release func(), reached func() bool, what string) {
	t.Helper()
	// Generous, because it costs nothing on the normal path (Eventually
	// returns as soon as the condition holds) and a loaded CI runner is
	// exactly the environment this ordering exists for.
	require.Eventually(t, reached, 10*time.Second, time.Millisecond, what)
	release()
}

// retryProbeFaults records which retry attempts the ladder actually reached
// and queues a single display-message fault at a chosen one.
//
// The attempt log is the point. A scenario that only counts /exit sends
// infers where the ladder stopped; this states it. When the abort under test
// regresses, "the ladder reached attempt 3" fails with the reason rather than
// with an arithmetic mismatch a reader has to decode.
type retryProbeFaults struct {
	mu       sync.Mutex
	seen     []int
	faultAt  int
	injected bool
}

func (r *retryProbeFaults) hook(sim *testharness.TmuxSim) func(int) {
	return func(attempt int) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.seen = append(r.seen, attempt)
		if attempt != r.faultAt {
			return
		}
		// Queue FIRST, publish SECOND, both under the lock. Publishing the
		// flag before queueing leaves a window where `taken` sees injected
		// AND an empty fault queue — releasing the watchdog before the fault
		// it is being held away from even exists.
		sim.FailNextCommand("display-message")
		r.injected = true
	}
}

func (r *retryProbeFaults) attempts() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.seen...)
}

// taken reports that the fault was queued AND consumed. With the watchdog
// parked, the ladder's own probe is the only thing that can consume it, so
// this is a causal statement that the probe under test saw its fault — the
// thing TCL-1028 showed a scenario cannot simply assume.
func (r *retryProbeFaults) taken(sim *testharness.TmuxSim) bool {
	r.mu.Lock()
	injected := r.injected
	r.mu.Unlock()
	return injected && sim.PendingCommandFaults("display-message") == 0
}

// Scenario: a wedged pane that ignores /exit entirely. The retry must be
// BOUNDED — it cannot type /exit at the pane forever. What finishes the pane
// is the escalation ladder (TCL-1001): the soft path no longer leaves a hung
// pane running, it kills it once the bounded attempts have had their window.
func TestSoftExit_BoundedRetriesForHungPane(t *testing.T) {
	f := newFlow(t)

	const conv = "sxjc-1111-2222-3333-4444"
	const tmuxSes = "tmux-sxjc"
	const target = tmuxSes + ":0.0"
	f.HaveConvWithTitle(conv, "hung-worker")
	f.HaveAliveSession(conv, "spwn-sxjc", tmuxSes, f.TestCwd("sxjc"))

	// A pane that consumes the signal-exit ctrl-c presses without ever
	// flipping dead (CC wedged).
	cc := f.World.CCs.GetByConvID(conv)
	require.NotNil(t, cc)
	cc.SetSignalExitWedged(true)

	// The watchdog waits until the ladder has spent its attempts. That is the
	// production ordering (10 s deadline against ~8 s of retries); at fixture
	// scale it has to be stated, or the watchdog's kill lands mid-ladder and
	// this scenario measures 2 attempts instead of the bound it exists to pin.
	releaseEscalation := holdSoftExitEscalation(t)

	stop := f.AsHuman().Stop(conv, false)
	f.AssertSoftStopped(stop)
	awaitLadderThenRelease(t, releaseEscalation, func() bool {
		return countSoftExitAttempts(f, target) == 3
	}, "the bounded ladder never reached its third attempt")
	agentd.WaitForBackgroundForTest()

	// Bounded: 1 initial attempt + 2 retries (softExitMaxAttempts = 3) = 3
	// total. Guards against an unbounded re-injection loop into a wedged pane.
	// Re-asserted after the drain: the wait above proves it REACHED 3, this
	// proves nothing pushed it past 3 afterwards.
	assert.Equal(t, 3, countSoftExitAttempts(f, target),
		"soft-exit attempts must be capped (initial + retries), not infinite")
	assert.False(t, f.World.Tmux.IsAlive(tmuxSes),
		"a pane that ignored every bounded /exit must be escalated to a kill, not left running")
	d, err := db.Open()
	require.NoError(t, err)
	var exitReason string
	require.NoError(t, d.QueryRow(
		"SELECT COALESCE(exit_reason, '') FROM sessions WHERE id = ?",
		"spwn-sxjc").Scan(&exitReason))
	// An escalated kill is the daemon's own doing, so it must not reach the
	// reaper as an unexplained close. The exit REASON is what carries that,
	// and it is the durable half: nothing else writes it for this session.
	//
	// The exit INTENT is deliberately not asserted here, and the reason is
	// worth stating. The escalation re-arms it, but the superseded retry
	// engine has a bounded-window cleanup in flight whose CAS matches the
	// re-armed row exactly (same session, generation, action and event id),
	// so whichever lands last wins. In production that cleanup fires ~65s
	// after the last re-injection — a minute after the kill, and long after
	// the reaper has observed the pane and read the intent — so the ordering
	// is not observable there. Under the flow harness both delays are
	// milliseconds, which makes it a coin flip. Asserting the coin flip would
	// buy a flake; the guarantee this test is for lives in exit_reason.
	// TestSoftExitEscalation_KillsPaneWhoseInjectionFailed covers the re-arm
	// itself, on the path where no cleanup is ever scheduled to race it.
	assert.Equal(t, "daemon_kill", exitReason,
		"an escalated kill must be recorded as daemon-owned, not left to the crash fallback")
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
//
// This drives the SELECTED-TARGET retry engine (scheduleSoftExitRetryTarget),
// not the pid-keyed scheduleSoftExitRetry whose own doc comment describes this
// same resume-reuses-the-name scenario. Both guard it; they are separate
// engines and only this one carries the staging hook used below. Anyone who
// reads "live-PID guard" and greps for livePanePID lands in the other one.
func TestSoftExit_RetryDoesNotExitResumedPaneReusingTmuxName(t *testing.T) {
	f := newFlow(t)

	const conv = "sxjd-1111-2222-3333-4444"
	const tmuxSes = "tmux-sxjd"
	const target = tmuxSes + ":0.0"
	cwd := f.TestCwd("sxjd")
	f.HaveConvWithTitle(conv, "resumed-worker")
	f.HaveAliveSession(conv, "spwn-sxjd", tmuxSes, cwd)

	// A wedged pane ignores the first signal-exit attempt, so it stays alive
	// and the retry is armed to reach attempt 2 where the swap is staged.
	cc := f.World.CCs.GetByConvID(conv)
	require.NotNil(t, cc)
	cc.SetSignalExitWedged(true)

	// Stage the resume INSIDE the retry's own pre-probe hook: the original pane
	// exits and a fresh CCSim (a new process → new pane pid) re-registers under
	// the SAME tmux name, modelling `session new -r`'s conv-id-derived name
	// collision. The hook fires immediately before the retry reads the pane, so
	// the swap is ORDERED ahead of the guard rather than raced against a delay.
	//
	// This used to widen the retry delay to 150ms — over newFlow's shared 1ms —
	// purely to buy wall-clock in which to stage the swap from the test
	// goroutine. Deleting that override is the fix: the moment is now the
	// instant the guard actually runs, which is a stronger test than any
	// window, and the test sits on the same footing as its siblings below.
	//
	// Gated on attempt 2, matching those siblings. Ungated, a guard that stopped
	// working would keep looping and keep re-staging a fresh pane, and the
	// assertions below could still be satisfied — green for the wrong reason,
	// which is the failure mode this whole fixture exists to catch.
	//
	// No require/assert in here: this runs on the retry's background goroutine,
	// where FailNow is not supported and would surface as a hang or a
	// wrong-goroutine panic instead of a clean failure. The error is captured
	// and asserted after the join, for the same reason the hook restores after
	// the join — a failure has to stay legible.
	var (
		stageOnce sync.Once
		staged    bool
		stageErr  error
	)
	// Closed once the hook has run, success or failure, so the test goroutine
	// can wait for the staging WITHOUT reading `staged` across goroutines —
	// that variable is written on the ladder's goroutine and is only safe to
	// read after the join below.
	swapAttempted := make(chan struct{})
	restoreProbe := agentd.SetBeforeSoftExitTargetRetryProbeForTest(func(attempt int) {
		if attempt != 2 {
			return
		}
		stageOnce.Do(func() {
			defer close(swapAttempted)
			cc.MarkDead()
			resumed := testharness.NewCCSimWithID(t, f.World.HomeDir, conv, cwd)
			if err := resumed.Start(); err != nil {
				stageErr = err
				return
			}
			f.World.Tmux.Register(tmuxSes, cwd, resumed)
			staged = true
		})
	})
	cleanupAfterBackgroundDrain(t, restoreProbe)

	// This scenario also needs the ladder to REACH attempt 2 — that is where
	// the swap is staged — so it is exposed to the same watchdog race as its
	// siblings. Its require.True(staged) guard means a watchdog that outran
	// the ladder would fail loudly rather than pass vacuously, which is why
	// the cold review rated it low risk. Converted anyway: a loud flake is
	// still a flake, and "loud" is a property of the guard, not a reason to
	// leave the ordering to chance.
	releaseEscalation := holdSoftExitEscalation(t)

	stop := f.AsHuman().Stop(conv, false)
	f.AssertSoftStopped(stop)

	awaitLadderThenRelease(t, releaseEscalation, func() bool {
		select {
		case <-swapAttempted:
			return true
		default:
			return false
		}
	}, "the ladder never reached attempt 2, so no pane swap was staged")
	agentd.WaitForBackgroundForTest()

	require.NoError(t, stageErr, "the resumed pane must have started for this test to mean anything")
	// Pins WHICH engine ran. The staging hook exists only on the selected-target
	// retry; if a change to the stop path ever routed this scenario onto the
	// pid-keyed scheduleSoftExitRetry instead, the hook would never fire, no
	// pane swap would happen, and both assertions below would pass anyway —
	// vacuously, against a race that was never staged.
	require.True(t, staged,
		"the retry never reached its pre-probe hook: this scenario is no longer driving the selected-target engine")
	// Corroborating, not load-bearing: re-injection goes to the frozen pane id,
	// and the simulator resolves a vanished %N to nothing, so the successor
	// survives even with the guard disabled. That is production-faithful — real
	// tmux does not reuse pane ids — but it means this assertion cannot fail on
	// its own. The send count below is what proves the guard ran.
	assert.True(t, f.World.Tmux.IsAlive(tmuxSes),
		"the resumed pane must survive — the retry must not /exit a new process that reused the tmux name")
	// The only /exit to this name was the original (scrambled) attempt; the
	// retry recognised the pid change and never re-injected.
	assert.Equal(t, 1, countSoftExitAttempts(f, target),
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
	assert.Equal(t, 0, countSoftExitAttempts(f, tmuxSes+":0.0"), "successor receives zero /exit bytes")
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
	cc.SetSignalExitWedged(true)
	// This fault is aimed at the SYNCHRONOUS post-send probe, and it reaches
	// it for a structural reason rather than a lucky one: the hook runs inside
	// injectSoftExitTarget, and scheduleSoftExitEscalation is not called until
	// that returns (lifecycle.go), so no watchdog goroutine exists yet to take
	// the fault first. Moving the arming earlier — or adding any other
	// concurrent issuer of display-message on this path — gives this scenario
	// TCL-1028's bug verbatim, silently and on loaded runners only. The
	// witness below is what makes that change fail here instead.
	witness := &escalationWitness{}
	// Hook cleanup registered BEFORE the park, so LIFO runs the park's
	// release-then-drain first; the reverse order drains into a parked
	// watchdog and hangs.
	cleanupAfterBackgroundDrain(t, agentd.SetAfterSoftExitTargetSendForTest(func() {
		witness.sample()
		f.World.Tmux.FailNextCommand("display-message")
	}))
	releaseEscalation := holdSoftExitEscalationInto(t, witness)
	stop := f.AsHuman().Stop(conv, false)
	f.AssertSoftStopped(stop)
	releaseEscalation()
	agentd.WaitForBackgroundForTest()
	assert.False(t, witness.probedWhenSampled(),
		"the escalation watchdog was already probing when this fault was queued; "+
			"it is no longer aimed at the synchronous probe")
	assert.Equal(t, 1, countSoftExitAttempts(f, tmuxSes+":0.0"), "unknown probe must never trigger retry reinjection")
}

func TestSoftExit_PreSendUnknownSendsZeroAndErrors(t *testing.T) {
	f := newFlow(t)
	const conv = "sxjg-1111-2222-3333-4444"
	const tmuxSes = "tmux-sxjg"
	f.HaveConvWithTitle(conv, "pre-unknown")
	f.HaveAliveSession(conv, "spwn-sxjg", tmuxSes, f.TestCwd("sxjg"))
	// Same structural claim as the scenario above, one step earlier in the
	// synchronous injection — and pinned the same way.
	witness := &escalationWitness{}
	cleanupAfterBackgroundDrain(t, agentd.SetBeforeSoftExitTargetRevalidateForTest(func() {
		witness.sample()
		f.World.Tmux.FailNextCommand("display-message")
	}))
	// Released at the END of the body, and both halves of that matter. Not
	// earlier: a failed pre-send injection still arms the watchdog, so an
	// early release would race its kill against the "pane is still alive"
	// assertion below, which is a claim about the STOP path and nothing else.
	// Not left to cleanup either: newFlow registers its background drain LAST,
	// so LIFO runs it FIRST, and a drain into a parked watchdog never returns.
	releaseEscalation := holdSoftExitEscalationInto(t, witness)
	assert.Equal(t, "error", agentd.StopOneConvWithIntentForTest(conv, db.AgentExitActionStop))
	assert.False(t, witness.probedWhenSampled(),
		"the escalation watchdog was already probing when this fault was queued; "+
			"it is no longer aimed at the pre-send revalidate probe")
	d, err := db.Open()
	require.NoError(t, err)
	var intent string
	require.NoError(t, d.QueryRow(`SELECT exit_intent FROM sessions WHERE id = 'spwn-sxjg'`).Scan(&intent))
	assert.Empty(t, intent)
	assert.True(t, f.World.Tmux.IsAlive(tmuxSes))
	assert.Equal(t, 0, countSoftExitAttempts(f, tmuxSes+":0.0"))
	releaseEscalation()
}

// Scenario: the retry probe cannot read the pane. An unknown result must end
// the ladder and hand cleanup to the observer window — never re-inject on a
// guess about a pane whose state is unreadable.
//
// This is TCL-1028's test. The queued display-message fault is aimed at
// attempt 2's probe, and until now nothing made it land there: the escalation
// watchdog polls the same verb every millisecond, so on a runner that
// descheduled this goroutine between the hook and its probe, the watchdog took
// the fault, attempt 2 read a healthy pane, and the ladder ran on to its full
// 3 attempts. Parking the watchdog leaves the ladder's own probe as the only
// possible consumer, and `taken` then CONFIRMS the consumption instead of
// assuming it.
func TestSoftExit_RetryUnknownCleansWithoutSend(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetSoftExitRetryDelayForTest(5 * time.Millisecond))
	const conv = "sxjh-1111-2222-3333-4444"
	const tmuxSes = "tmux-sxjh"
	f.HaveConvWithTitle(conv, "retry-unknown")
	f.HaveAliveSession(conv, "spwn-sxjh", tmuxSes, f.TestCwd("sxjh"))
	cc := f.World.CCs.GetByConvID(conv)
	require.NotNil(t, cc)
	cc.SetSignalExitWedged(true)
	faults := &retryProbeFaults{faultAt: 2}
	cleanup := agentd.SetBeforeSoftExitTargetRetryProbeForTest(faults.hook(f.World.Tmux))
	cleanupAfterBackgroundDrain(t, cleanup)
	releaseEscalation := holdSoftExitEscalation(t)
	stop := f.AsHuman().Stop(conv, false)
	f.AssertSoftStopped(stop)
	awaitLadderThenRelease(t, releaseEscalation, func() bool {
		return faults.taken(f.World.Tmux)
	}, "attempt 2's probe never took the fault queued for it, so this scenario "+
		"is not measuring an unknown probe result")
	agentd.WaitForBackgroundForTest()
	assert.Equal(t, 1, countSoftExitAttempts(f, tmuxSes+":0.0"))
	assert.Equal(t, []int{2}, faults.attempts(),
		"an unknown probe must END the ladder; reaching attempt 3 means the abort did not hold")
}

// Scenario: the same unknown probe, but on the LAST attempt — the ladder is
// already at its bound, so the abort has nothing left to prevent and what is
// pinned is that the final attempt still cleans up rather than re-injecting a
// fourth time.
func TestSoftExit_FinalUnknownCleansBounded(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetSoftExitRetryDelayForTest(5 * time.Millisecond))
	const conv = "sxji-1111-2222-3333-4444"
	const tmuxSes = "tmux-sxji"
	f.HaveConvWithTitle(conv, "final-unknown")
	f.HaveAliveSession(conv, "spwn-sxji", tmuxSes, f.TestCwd("sxji"))
	cc := f.World.CCs.GetByConvID(conv)
	require.NotNil(t, cc)
	cc.SetSignalExitWedged(true)
	faults := &retryProbeFaults{faultAt: 3}
	cleanup := agentd.SetBeforeSoftExitTargetRetryProbeForTest(faults.hook(f.World.Tmux))
	cleanupAfterBackgroundDrain(t, cleanup)
	releaseEscalation := holdSoftExitEscalation(t)
	stop := f.AsHuman().Stop(conv, false)
	f.AssertSoftStopped(stop)
	awaitLadderThenRelease(t, releaseEscalation, func() bool {
		return faults.taken(f.World.Tmux)
	}, "attempt 3's probe never took the fault queued for it, so this scenario "+
		"is not measuring an unknown probe result")
	agentd.WaitForBackgroundForTest()
	assert.Equal(t, 2, countSoftExitAttempts(f, tmuxSes+":0.0"))
	assert.Equal(t, []int{2, 3}, faults.attempts(),
		"the ladder must reach its bound here; a shorter run means something aborted it early")
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

func TestLifecycleStop_PaneGenerationBinding(t *testing.T) {
	const (
		boundGeneration = "11111111111111111111111111111111"
		otherGeneration = "22222222222222222222222222222222"
	)
	tests := []struct {
		name, slug, afterGeneration, wantAction string
		force, bound                            bool
		wantSends                               int
		wantKill                                bool
	}{
		// The only soft row whose pane stays the SAME live pane throughout, so
		// it is also the only one the escalation ladder acts on: the bounded
		// re-injections are exhausted and the wedged pane is killed. The rows
		// below drift their pane identity after delivery, which stands the
		// ladder down (a successor is never ours to kill).
		{name: "degraded soft control", slug: "degraded-soft", wantAction: "soft_stopped", wantSends: 3, wantKill: true},
		{name: "degraded generation appears after delivery", slug: "degraded-appears", afterGeneration: otherGeneration, wantAction: "soft_stopped", wantSends: 1},
		{name: "bound generation disappears after delivery", slug: "bound-missing", bound: true, afterGeneration: "missing", wantAction: "soft_stopped", wantSends: 1},
		{name: "bound generation mismatches after delivery", slug: "bound-mismatch", bound: true, afterGeneration: otherGeneration, wantAction: "soft_stopped", wantSends: 1},
		{name: "degraded force control", slug: "degraded-force", force: true, wantAction: "killed", wantKill: true},
		{name: "bound generation disappears before force", slug: "bound-force-missing", force: true, bound: true, afterGeneration: "missing", wantAction: "error"},
		{name: "bound generation mismatches before force", slug: "bound-force-mismatch", force: true, bound: true, afterGeneration: otherGeneration, wantAction: "error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlow(t)
			conv := "generation-" + tc.slug
			sessionID := "spwn-generation-" + tc.slug
			tmuxSession := "tmux-generation-" + tc.slug
			f.HaveConvWithTitle(conv, tc.name)
			f.HaveAliveSession(conv, sessionID, tmuxSession, f.TestCwd(tc.slug))
			require.NoError(t, db.SetSessionExitLaunchGeneration(sessionID, boundGeneration))
			if !tc.force {
				cc := f.World.CCs.GetByConvID(conv)
				require.NotNil(t, cc)
				cc.SetSignalExitWedged(true)
			}
			if tc.bound {
				f.World.Tmux.SetPaneExitGeneration(tmuxSession, boundGeneration)
			}
			if tc.afterGeneration != "" {
				drift := func() {
					generation := tc.afterGeneration
					if generation == "missing" {
						generation = ""
					}
					f.World.Tmux.SetPaneExitGeneration(tmuxSession, generation)
				}
				if tc.force {
					t.Cleanup(agentd.SetBeforeSoftExitTargetRevalidateForTest(drift))
				} else {
					t.Cleanup(agentd.SetAfterSoftExitTargetSendForTest(drift))
				}
			}

			// Only the row whose pane stays the same live pane throughout runs
			// a full ladder against a live watchdog, and it is the row that
			// was seen failing the other way — expected 3, got 2 — when the
			// watchdog's kill arrived before the ladder's last attempt. The
			// drifting rows stand both actors down after one send, so there
			// is nothing to order.
			var releaseEscalation func()
			if tc.wantSends > 1 {
				releaseEscalation = holdSoftExitEscalation(t)
			}

			stop := f.AsHuman().Stop(conv, tc.force)
			assert.Equal(t, tc.wantAction, stop.Action)
			if releaseEscalation != nil {
				awaitLadderThenRelease(t, releaseEscalation, func() bool {
					return countSoftExitAttempts(f, tmuxSession+":0.0") == tc.wantSends
				}, "the ladder never reached its full attempt count")
			}
			// Drained on EVERY row, not just the ladder rows. The rows that
			// stand the watchdog down assert that nothing was killed, and an
			// undrained assertion there cannot fail: it would be asserting
			// that a kill has not happened YET.
			agentd.WaitForBackgroundForTest()
			assert.Equal(t, tc.wantSends, countSoftExitAttempts(f, tmuxSession+":0.0"))
			if tc.wantKill {
				assert.NotEmpty(t, f.World.Tmux.MutationTargets("kill-pane"))
			} else {
				assert.Empty(t, f.World.Tmux.MutationTargets("kill-pane"))
			}
		})
	}
}

func TestSoftExit_DeliveredIntentObserverWindow(t *testing.T) {
	const (
		generation = "33333333333333333333333333333333"
		eventID    = "evt_555555555555555555555555"
	)
	tests := []struct {
		name, slug string
		dualFault  bool
		wantSends  int
	}{
		{name: "dual unknown after initial delivery", slug: "dual-unknown", dualFault: true, wantSends: 1},
		{name: "final successful retry", slug: "final-retry", wantSends: 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlow(t)
			t.Cleanup(agentd.SetUnknownIntentCleanupDelayForTest(100 * time.Millisecond))
			conv := "intent-" + tc.slug
			sessionID := "spwn-intent-" + tc.slug
			tmuxSession := "tmux-intent-" + tc.slug
			f.HaveConvWithTitle(conv, tc.name)
			f.HaveAliveSession(conv, sessionID, tmuxSession, f.TestCwd(tc.slug))
			require.NoError(t, db.SetSessionExitLaunchGeneration(sessionID, generation))
			f.World.Tmux.SetPaneExitGeneration(tmuxSession, generation)
			cc := f.World.CCs.GetByConvID(conv)
			require.NotNil(t, cc)
			cc.SetSignalExitWedged(true)
			if tc.dualFault {
				// These two faults need no watchdog parking, and the reason is
				// structural rather than lucky: this hook runs inside the
				// SYNCHRONOUS injection, and the escalation watchdog is not
				// armed until that returns. There is no second issuer of
				// either verb yet, so the faults can only reach the probe they
				// are aimed at.
				t.Cleanup(agentd.SetAfterSoftExitTargetSendForTest(func() {
					f.World.Tmux.FailNextCommand("display-message")
					f.World.Tmux.FailNextCommand("list-sessions")
				}))
			}

			// The full-ladder row measures the observer window across all
			// three attempts, so the watchdog's kill must not land inside it.
			var releaseEscalation func()
			if tc.wantSends > 1 {
				releaseEscalation = holdSoftExitEscalation(t)
			}

			action := agentd.StopOneConvWithIntentForTest(conv, db.AgentExitActionStop, eventID)
			assert.Equal(t, "soft_stopped", action)
			if releaseEscalation != nil {
				// No drain here: the intent assertions below read state
				// INSIDE the bounded observer window, which the drain would
				// wait out.
				awaitLadderThenRelease(t, releaseEscalation, func() bool {
					return countSoftExitAttempts(f, tmuxSession+":0.0") == tc.wantSends
				}, "the ladder never reached its full attempt count")
			} else {
				assert.Equal(t, tc.wantSends, countSoftExitAttempts(f, tmuxSession+":0.0"))
			}

			d, err := db.Open()
			require.NoError(t, err)
			var intent, gotEventID, intentGeneration, intentAt, gotTmux string
			readIntent := func() {
				t.Helper()
				require.NoError(t, d.QueryRow(`SELECT exit_intent, exit_intent_event_id,
					exit_intent_generation, COALESCE(exit_intent_at, ''), tmux_session
					FROM sessions WHERE id = ?`, sessionID).Scan(
					&intent, &gotEventID, &intentGeneration, &intentAt, &gotTmux))
			}
			readIntent()
			assert.Equal(t, db.AgentExitActionStop, intent, "delivered command retains attribution during observer window")
			assert.Equal(t, eventID, gotEventID)
			assert.Equal(t, generation, intentGeneration)
			assert.NotEmpty(t, intentAt)
			assert.Equal(t, tmuxSession, gotTmux)

			agentd.WaitForBackgroundForTest()
			assert.Equal(t, tc.wantSends, countSoftExitAttempts(f, tmuxSession+":0.0"),
				"background retry must not add sends after the expected terminal state")
			readIntent()
			assert.Empty(t, intent, "bounded cleanup clears the exact action/event/generation owner")
			assert.Empty(t, gotEventID)
			assert.Empty(t, intentGeneration)
			assert.Empty(t, intentAt)
		})
	}
}

// A pane whose identity drifts DURING the retry window (not immediately
// post-send) has still been delivered a real /exit. The watchdog must
// mirror the synchronous post-send drift branch: stop retrying against
// the successor AND preserve the delivered exit's intent so the
// callback/reaper can attribute the predecessor's exit. Clearing here
// loses observer attribution for an exit that actually happened.
func TestSoftExit_RetryIdentityDriftPreservesDeliveredIntent(t *testing.T) {
	const (
		generation      = "44444444444444444444444444444444"
		otherGeneration = "55555555555555555555555555555555"
		eventID         = "evt_666666666666666666666666"
	)
	f := newFlow(t)
	const conv = "sxjl-1111-2222-3333-4444"
	const sessionID = "spwn-sxjl"
	const tmuxSes = "tmux-sxjl"
	f.HaveConvWithTitle(conv, "retry-drift")
	f.HaveAliveSession(conv, sessionID, tmuxSes, f.TestCwd("sxjl"))
	require.NoError(t, db.SetSessionExitLaunchGeneration(sessionID, generation))
	f.World.Tmux.SetPaneExitGeneration(tmuxSes, generation)
	cc := f.World.CCs.GetByConvID(conv)
	require.NotNil(t, cc)
	cc.SetSignalExitWedged(true)
	drifted := make(chan struct{})
	var driftOnce sync.Once
	cleanupAfterBackgroundDrain(t, agentd.SetBeforeSoftExitTargetRetryProbeForTest(func(attempt int) {
		if attempt == 2 {
			f.World.Tmux.SetPaneExitGeneration(tmuxSes, otherGeneration)
			driftOnce.Do(func() { close(drifted) })
		}
	}))

	// Without this the watchdog can kill the pane before attempt 2's probe,
	// which sends the ladder down its GONE branch instead of the identity-drift
	// branch this scenario exists to measure — and every assertion below still
	// passes, because the escalation re-arms the same action/event/generation.
	// A vacuous pass is the failure mode here, not a red test.
	releaseEscalation := holdSoftExitEscalation(t)

	assert.Equal(t, "soft_stopped",
		agentd.StopOneConvWithIntentForTest(conv, db.AgentExitActionStop, eventID))
	// Releasing once the drift is applied is safe: a watchdog that probes
	// after it sees the same mismatch and stands down rather than killing.
	awaitLadderThenRelease(t, releaseEscalation, func() bool {
		select {
		case <-drifted:
			return true
		default:
			return false
		}
	}, "the ladder never reached attempt 2, so no identity drift was applied")
	agentd.WaitForBackgroundForTest()
	assert.Empty(t, f.World.Tmux.MutationTargets("kill-pane"),
		"the drifted pane belongs to a successor and must never be killed")

	assert.Equal(t, 1, countSoftExitAttempts(f, tmuxSes+":0.0"),
		"no retry may be typed once the pane identity drifted")
	d, err := db.Open()
	require.NoError(t, err)
	var intent, gotEventID, intentGeneration string
	require.NoError(t, d.QueryRow(`SELECT exit_intent, exit_intent_event_id,
		exit_intent_generation FROM sessions WHERE id = ?`, sessionID).Scan(
		&intent, &gotEventID, &intentGeneration))
	assert.Equal(t, db.AgentExitActionStop, intent,
		"identity drift during the retry window must preserve the delivered exit's intent")
	assert.Equal(t, eventID, gotEventID)
	assert.Equal(t, generation, intentGeneration)
}

// The retry probe erroring because the whole session is GONE is the
// delivered /exit landing, not a failure to clean up after: the reaper
// owns attribution of the confirmed disappearance and needs the intent to
// do it. The watchdog must mirror the synchronous unknown branch's
// confirmed-disappearance case instead of instantly clearing.
func TestSoftExit_RetryUnknownAfterSessionGonePreservesReaperAttribution(t *testing.T) {
	const eventID = "evt_777777777777777777777777"
	f := newFlow(t)
	const conv = "sxjm-1111-2222-3333-4444"
	const sessionID = "spwn-sxjm"
	const tmuxSes = "tmux-sxjm"
	f.HaveConvWithTitle(conv, "retry-gone")
	f.HaveAliveSession(conv, sessionID, tmuxSes, f.TestCwd("sxjm"))
	cc := f.World.CCs.GetByConvID(conv)
	require.NotNil(t, cc)
	cc.SetSignalExitWedged(true)
	sessionGone := make(chan struct{})
	var goneOnce sync.Once
	cleanupAfterBackgroundDrain(t, agentd.SetBeforeSoftExitTargetRetryProbeForTest(func(attempt int) {
		if attempt == 2 {
			// The delivered /exit has taken effect between retries: the
			// session is gone, so the probe errors.
			_ = f.World.Tmux.Command("kill-session", "-t", "="+tmuxSes).Run()
			goneOnce.Do(func() { close(sessionGone) })
		}
	}))

	// Same vacuity hazard as the drift scenario: a watchdog kill before
	// attempt 2 reaches the same intent state by a different route, so the
	// assertions below would pass without the branch under test ever running.
	releaseEscalation := holdSoftExitEscalation(t)

	assert.Equal(t, "soft_stopped",
		agentd.StopOneConvWithIntentForTest(conv, db.AgentExitActionStop, eventID))
	awaitLadderThenRelease(t, releaseEscalation, func() bool {
		select {
		case <-sessionGone:
			return true
		default:
			return false
		}
	}, "the ladder never reached attempt 2, so the session was never made to disappear")
	agentd.WaitForBackgroundForTest()
	assert.Empty(t, f.World.Tmux.MutationTargets("kill-pane"),
		"a session that already vanished leaves the escalation nothing to kill")

	assert.Equal(t, 1, countSoftExitAttempts(f, tmuxSes+":0.0"))
	d, err := db.Open()
	require.NoError(t, err)
	var intent, gotEventID string
	require.NoError(t, d.QueryRow(`SELECT exit_intent, exit_intent_event_id
		FROM sessions WHERE id = ?`, sessionID).Scan(&intent, &gotEventID))
	assert.Equal(t, db.AgentExitActionStop, intent,
		"a confirmed disappearance must leave the intent for the reaper's attribution")
	assert.Equal(t, eventID, gotEventID)
}

// A failed RE-send during the retry window must not erase the delivered
// first /exit's attribution: the send often fails precisely because that
// exit is landing. The branch mirrors its unknown-probe sibling — the
// intent survives through the bounded observer window (for the
// callback/reaper to claim) and is then cleaned up, never cleared the
// instant the re-send errors.
func TestSoftExit_RetrySendFailurePreservesDeliveredIntentThroughWindow(t *testing.T) {
	const eventID = "evt_888888888888888888888888"
	f := newFlow(t)
	t.Cleanup(agentd.SetUnknownIntentCleanupDelayForTest(time.Second))
	const conv = "sxjn-1111-2222-3333-4444"
	const sessionID = "spwn-sxjn"
	const tmuxSes = "tmux-sxjn"
	f.HaveConvWithTitle(conv, "retry-send-fail")
	f.HaveAliveSession(conv, sessionID, tmuxSes, f.TestCwd("sxjn"))
	cc := f.World.CCs.GetByConvID(conv)
	require.NotNil(t, cc)
	cc.SetSignalExitWedged(true)
	retryReached := make(chan struct{})
	var once sync.Once
	cleanupAfterBackgroundDrain(t, agentd.SetBeforeSoftExitTargetRetryProbeForTest(func(attempt int) {
		if attempt == 2 {
			f.World.Tmux.FailNextCommand("send-keys")
			once.Do(func() { close(retryReached) })
		}
	}))

	// A watchdog kill before attempt 2's probe leaves the ladder on its
	// pane-is-gone branch, so the re-send never happens, the queued send-keys
	// fault is never consumed, and the final clear this scenario asserts is
	// never scheduled — it fails outright rather than passing vacuously.
	releaseEscalation := holdSoftExitEscalation(t)

	assert.Equal(t, "soft_stopped",
		agentd.StopOneConvWithIntentForTest(conv, db.AgentExitActionStop, eventID))
	select {
	case <-retryReached:
	case <-time.After(10 * time.Second):
		t.Fatal("retry attempt 2 never ran")
	}
	// Reaching the hook is not enough: what this scenario measures is a
	// FAILED re-send, which has not happened until the queued fault has been
	// taken. With the watchdog parked, the ladder's own send is the only
	// thing that can take it.
	awaitLadderThenRelease(t, releaseEscalation, func() bool {
		return f.World.Tmux.PendingCommandFaults("send-keys") == 0
	}, "the re-send never consumed its queued fault, so no failed re-send was measured")

	d, err := db.Open()
	require.NoError(t, err)
	readIntent := func() (intent, gotEventID string) {
		t.Helper()
		require.NoError(t, d.QueryRow(`SELECT exit_intent, exit_intent_event_id
			FROM sessions WHERE id = ?`, sessionID).Scan(&intent, &gotEventID))
		return intent, gotEventID
	}
	// The 1s observer window comfortably outlasts this 100ms probe: any
	// clear observed here is the instant-clear regression, not the bounded
	// cleanup.
	assert.Never(t, func() bool {
		intent, _ := readIntent()
		return intent == ""
	}, 100*time.Millisecond, 10*time.Millisecond,
		"a failed re-send must not instantly clear the delivered exit's intent")
	intent, gotEventID := readIntent()
	assert.Equal(t, db.AgentExitActionStop, intent)
	assert.Equal(t, eventID, gotEventID)

	agentd.WaitForBackgroundForTest()
	assert.Equal(t, 1, countSoftExitAttempts(f, tmuxSes+":0.0"),
		"the failed re-send delivers no bytes and no further retries run")
	intent, gotEventID = readIntent()
	assert.Empty(t, intent, "the bounded observer window still cleans up; retention is not a leak")
	assert.Empty(t, gotEventID)
}
