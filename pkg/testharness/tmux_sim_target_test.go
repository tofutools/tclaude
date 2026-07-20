package testharness

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// The sim models tmux's real target-session resolution (cmd-find): a bare
// -t name falls back to unique-prefix matching when no exact match exists,
// while a '='-pinned target resolves exact-only. The fallback is the
// production footgun clcommon.ExactTarget exists to avoid — a dead
// "myrepo" resolving to a live "myrepo-2" would misroute kills, attaches
// and injected keystrokes — so it must be modelled here for a dropped '='
// to fail tests instead of misrouting in production.
func TestTmuxSim_TargetResolution_ModelsPrefixFallback(t *testing.T) {
	sim := newTmuxSim()
	sim.MarkAlive("myrepo-2")

	// Bare dead name prefix-matches the live namesake — like real tmux.
	assert.NoError(t, sim.Command("has-session", "-t", "myrepo").Run(),
		"bare -t must model tmux's prefix fallback: dead 'myrepo' resolves to live 'myrepo-2'")

	// The '=' pin resolves exact-only.
	assert.Error(t, sim.Command("has-session", "-t", "=myrepo").Run(),
		"'='-pinned dead name must NOT resolve to the namesake")
	assert.NoError(t, sim.Command("has-session", "-t", "=myrepo-2").Run(),
		"'='-pinned live name resolves")

	// An ambiguous prefix resolves nothing (real tmux errors out).
	sim.MarkAlive("myrepo-3")
	assert.Error(t, sim.Command("has-session", "-t", "myrepo").Run(),
		"a prefix matching two sessions is ambiguous and must not resolve")

	// A pane-qualified '='-target binds the pin to the session part.
	assert.NoError(t, sim.Command("has-session", "-t", "=myrepo-2:0.0").Run())
}

// kill-session resolves its target like real tmux too: '='-pinned kills
// exactly the named session; a bare dead name would prefix-resolve onto a
// live namesake — the wrong-session kill the production '=' targets
// prevent.
func TestTmuxSim_KillSession_TargetResolution(t *testing.T) {
	sim := newTmuxSim()
	sim.MarkAlive("myrepo-2")
	sim.MarkAlive("myrepo-3")

	sim.Command("kill-session", "-t", "=myrepo-2")
	assert.False(t, sim.IsAlive("myrepo-2"), "'='-pinned kill removes exactly the named session")
	assert.True(t, sim.IsAlive("myrepo-3"), "the namesake survives")

	// Bare dead name now uniquely prefix-matches myrepo-3 — the sim kills
	// it, modelling the production hazard.
	sim.Command("kill-session", "-t", "myrepo")
	assert.False(t, sim.IsAlive("myrepo-3"),
		"a bare dead-name kill prefix-resolves onto the live namesake — the hazard '=' exists to prevent")
}

// send-keys targets are logged with the '=' pin stripped, so assertions
// written against raw "name:0.0" targets keep matching, and delivery
// resolves exact-only when pinned.
func TestTmuxSim_SendKeys_NormalizedLogAndExactRouting(t *testing.T) {
	sim := newTmuxSim()
	sim.MarkAlive("myrepo-2")

	sim.Command("send-keys", "-t", "=myrepo-2:0.0", "hello")
	sent := sim.Sent()
	assert.Len(t, sent, 1)
	assert.Equal(t, "myrepo-2:0.0", sent[0].Target, "the '=' resolution marker is stripped from the log")
	assert.Equal(t, "hello", sent[0].Text)
}

func TestTmuxSim_SendKeysLiteralMode(t *testing.T) {
	sim := newTmuxSim()
	sim.MarkAlive("myrepo-2")

	sim.Command("send-keys", "-l", "-t", "=myrepo-2:0.0", "Enter")
	sent := sim.Sent()
	assert.Len(t, sent, 1)
	assert.Equal(t, "myrepo-2:0.0", sent[0].Target)
	assert.Equal(t, "Enter", sent[0].Text)
}

func TestTmuxSim_SendKeysRejectsSessionExactMarkerOnPaneID(t *testing.T) {
	sim := newTmuxSim()
	sim.MarkAlive("worker")
	sim.SetPaneIdentityForTest("worker", "%77", 4242)

	assert.Error(t, sim.Command("send-keys", "-l", "-t", "=%77", "/exit").Run(),
		"real tmux rejects a session-name exact marker prefixed to a pane ID")
	assert.NoError(t, sim.Command("send-keys", "-l", "-t", "%77", "/exit").Run(),
		"a bare pane ID is already an exact target")
}

// Pane-typed commands (send-keys, display-message, capture-pane) parse a
// COLON-LESS target into the pane slot, where real tmux never strips the
// '=' — "=name" hunts a pane literally named "=name" and matches nothing.
// The sim models that so a production ExactTarget(name) used without a
// ":"/":0.0" qualifier on a pane-typed verb fails tests instead of dying
// (or acting on the wrong pane) in the field. Session-typed verbs parse
// the same colon-less form as an exact session name.
func TestTmuxSim_PaneTypedTargets_RejectColonLessExactPin(t *testing.T) {
	sim := newTmuxSim()
	sim.MarkAlive("myrepo-2")

	// display-message is pane-typed: colon-less '=' never resolves; the
	// ":"-qualified form pins the session and uses its active pane.
	assert.Error(t, sim.Command("display-message", "-p", "-t", "=myrepo-2", "#{pane_pid}").Run(),
		"colon-less '=' on a pane-typed verb must model tmux's literal-pane lookup failure")
	assert.NoError(t, sim.Command("display-message", "-p", "-t", "=myrepo-2:", "#{pane_pid}").Run(),
		"the ExactTarget(name)+\":\" form resolves the session exactly")

	// send-keys likewise: colon-less '=' is dropped, qualified forms route.
	sim.Command("send-keys", "-t", "=myrepo-2", "lost")
	sim.Command("send-keys", "-t", "=myrepo-2:", "kept")
	sent := sim.Sent()
	assert.Len(t, sent, 2, "both sends are logged")
	assert.Equal(t, "myrepo-2:", sent[1].Target)

	// has-session is session-typed: the same colon-less '=' form is the
	// correct exact pin there.
	assert.NoError(t, sim.Command("has-session", "-t", "=myrepo-2").Run())

	// set-option and list-panes route through the same pane-typed rules, so
	// the production ExactTarget(name)+":" sites (window titles, scrollback
	// mouse mode, ParsePIDFromTmux) are exercised rather than falling
	// through to an unconditional success.
	assert.Error(t, sim.Command("set-option", "-t", "=myrepo-2", "mouse", "on").Run())
	assert.NoError(t, sim.Command("set-option", "-t", "=myrepo-2:", "mouse", "on").Run())
	assert.Error(t, sim.Command("list-panes", "-t", "=myrepo-2", "-F", "#{pane_pid}").Run())
	out, err := sim.Command("list-panes", "-t", "=myrepo-2:", "-F", "#{pane_pid}").Output()
	assert.NoError(t, err)
	assert.NotEmpty(t, strings.TrimSpace(string(out)), "list-panes echoes the pane pid on a resolved target")
}

// The production probes go through '='-pinned targets: a dead name whose
// live "-N" namesake would prefix-match must read as DEAD, and
// UniqueTmuxSessionName must therefore hand the freed base back out. This
// pins the clcommon.ExactTarget usage end-to-end through the session
// package — dropping the '=' from IsTmuxSessionAlive fails this test.
func TestSession_LivenessProbe_IsExact(t *testing.T) {
	sim := newTmuxSim()
	sim.MarkAlive("myrepo-2")
	sim.MarkAlive("myrepo-3")
	prev := clcommon.Default
	clcommon.Default = sim
	t.Cleanup(func() { clcommon.Default = prev })

	assert.False(t, session.IsTmuxSessionAlive("myrepo"),
		"a dead name must not read alive via its live '-N' namesakes")
	assert.True(t, session.IsTmuxSessionAlive("myrepo-2"))

	assert.Equal(t, "myrepo", session.UniqueTmuxSessionName("myrepo"),
		"the freed base is handed back out even while '-N' namesakes live")
}

// WaitForSendKeys must never treat two unresolvable targets as the same
// pane. Send-keys deliveries are logged even when the target no longer
// resolves (the session may legitimately have exited before the
// assertion runs), and unresolvable targets all resolve to "" — the old
// comparison matched on "" == "", so a wait against a target production
// code never touched could pass because SOME other dead target had
// received the text.
func TestTmuxSim_WaitForSendKeys_UnresolvedTargetsDoNotCrossMatch(t *testing.T) {
	sim := newTmuxSim()
	sim.MarkAlive("live")
	sim.Command("send-keys", "-t", "gone-a:0.0", "/exit")

	assert.True(t, sim.WaitForSendKeys("gone-a:0.0", "/exit", 10*time.Second),
		"the literal logged target still matches after its session is gone")
	assert.False(t, sim.WaitForSendKeys("gone-b:0.0", "/exit", 50*time.Millisecond),
		"a different unresolvable target must not cross-match on \"\" == \"\"")
	assert.False(t, sim.WaitForSendKeys("live:0.0", "/exit", 50*time.Millisecond),
		"a live session that never received the text must not match")
}

// Parity guard for the remaining '='-pinned pane-ID sinks: real tmux
// rejects `-t =%N` for kill-pane ("can't find pane"), kill-session
// ("can't find session") and paste-buffer ("can't find pane") exactly as
// it does for send-keys, so the sim must fail them too instead of
// silently no-oping — otherwise a production regression that re-mangles
// pane IDs (TCL-589) would pass flow tests on those paths. Verified
// against real tmux 3.7b.
func TestTmuxSim_ExactPinnedPaneTargetsFailBeyondSendKeys(t *testing.T) {
	sim := newTmuxSim()
	sim.MarkAlive("par")
	sim.SetPaneIdentityForTest("par", "%88", 4242)

	assert.Error(t, sim.Command("kill-pane", "-t", "=%88").Run(),
		"kill-pane must reject a '='-pinned pane ID like real tmux")
	assert.Error(t, sim.Command("kill-session", "-t", "=%88").Run(),
		"kill-session must reject a '='-pinned pane ID like real tmux")
	sim.Command("set-buffer", "-b", "b1", "hello")
	assert.Error(t, sim.Command("paste-buffer", "-d", "-p", "-r", "-b", "b1", "-t", "=%88").Run(),
		"paste-buffer must reject a '='-pinned pane ID like real tmux")
	assert.True(t, sim.IsAlive("par"), "rejected targets must not tear the session down")
	for _, sk := range sim.Sent() {
		assert.NotEqual(t, "hello", sk.Text, "rejected paste must not deliver any bytes")
	}

	// The plain pane ID keeps working on every path (no aliasing regressed).
	assert.NoError(t, sim.Command("paste-buffer", "-d", "-p", "-r", "-b", "b1", "-t", "%88").Run())
	assert.True(t, sim.WaitForSendKeys("%88", "hello", 10*time.Second),
		"the un-pinned paste reaches the exact pane")
	assert.NoError(t, sim.Command("kill-pane", "-t", "%88").Run())
	assert.False(t, sim.IsAlive("par"))
}
