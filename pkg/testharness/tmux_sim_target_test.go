package testharness

import (
	"testing"

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
