package testharness

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// JOH-191: the TmuxSim answers has-session with an exit-0 / exit-1
// process, and that truthiness must be hermetic — independent of $PATH.
// A bare exec.Command("true") PATH-resolves at Run() time, so under
// `env -i` (empty PATH) LookPath fails, every live session reads dead,
// and the whole sim suite collapses. These tests pin that the sim execs
// ABSOLUTE-path true/false and that its has-session verdict survives an
// emptied environment.

// On any normal Unix box the coreutils resolve to absolute paths; the
// bare-name fallback only fires on a system without /usr/bin|/bin
// true/false, which is exactly where PATH-resolution would already be
// the lesser evil. Guard the property we actually rely on in CI.
func TestTmuxSim_CoreutilsResolveAbsolute(t *testing.T) {
	require.True(t, filepath.IsAbs(trueBin),
		"true must resolve to an absolute path (got %q) so has-session needs no PATH lookup", trueBin)
	require.True(t, filepath.IsAbs(falseBin),
		"false must resolve to an absolute path (got %q) so has-session needs no PATH lookup", falseBin)
}

// The has-session cmd must carry an absolute Path and no LookPath error,
// so Run() execs directly without consulting $PATH — the regression
// guard for "TmuxSim no longer execs a PATH-resolved true/false".
func TestTmuxSim_HasSessionCmdIsPathIndependent(t *testing.T) {
	tm := newTmuxSim()
	tm.MarkAlive("live")

	alive := tm.Command("has-session", "-t", "live")
	require.NoError(t, alive.Err, "has-session cmd must not carry a LookPath error")
	require.True(t, filepath.IsAbs(alive.Path),
		"alive has-session must exec an absolute path, got %q", alive.Path)

	dead := tm.Command("has-session", "-t", "missing")
	require.NoError(t, dead.Err, "has-session cmd must not carry a LookPath error")
	require.True(t, filepath.IsAbs(dead.Path),
		"dead has-session must exec an absolute path, got %q", dead.Path)
}

// End-to-end hermeticity: run the has-session cmd with an emptied
// environment (no PATH at all) and assert the alive/dead verdict still
// matches IsAlive. With a bare-name true/false this Run() would fail
// LookPath and read every session as dead — the bug JOH-191 fixes.
func TestTmuxSim_HasSessionSurvivesEmptyEnv(t *testing.T) {
	if !filepath.IsAbs(trueBin) || !filepath.IsAbs(falseBin) {
		t.Skip("coreutils not at absolute paths on this host; PATH-independence not assertable")
	}
	tm := newTmuxSim()
	tm.MarkAlive("live")

	alive := tm.Command("has-session", "-t", "live")
	alive.Env = []string{} // env -i: no PATH
	assert.NoError(t, alive.Run(),
		"a live session's has-session must exit 0 even with an empty environment")

	dead := tm.Command("has-session", "-t", "missing")
	dead.Env = []string{} // env -i: no PATH
	assert.Error(t, dead.Run(),
		"a dead session's has-session must exit non-zero even with an empty environment")
}
