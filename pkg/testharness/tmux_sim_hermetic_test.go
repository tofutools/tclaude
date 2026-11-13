package testharness

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// JOH-191: the simulator's answers must be hermetic, independent of $PATH and
// of anything else in the process environment. It used to answer has-session
// by running true/false, and a PATH-resolved binary made every live session
// read dead under an emptied environment. It now starts no process at all;
// this pins that the verdict cannot depend on the environment.
func TestTmuxSim_HasSessionSurvivesEmptyEnv(t *testing.T) {
	t.Setenv("PATH", "")
	tm := newTmuxSim()
	tm.MarkAlive("live")

	_, err := tm.Exec("has-session", "-t", "live")
	assert.NoError(t, err, "a live session's has-session must succeed with no PATH")

	_, err = tm.Exec("has-session", "-t", "missing")
	assert.Error(t, err, "a dead session's has-session must fail with no PATH")
}

// The one process the simulator ever names, the stand-in for an attaching
// tmux client, is an absolute path for the same reason.
func TestTmuxSim_AttachStandInIsPathIndependent(t *testing.T) {
	argv, err := newTmuxSim().AttachArgv([]string{"attach-session", "-t", "=live"})
	require.NoError(t, err)
	require.Len(t, argv, 1)
	require.True(t, filepath.IsAbs(argv[0]), "got %q", argv[0])
}

func TestTmuxSim_ParsesClientOptionsBeforeCommand(t *testing.T) {
	tm := newTmuxSim()

	// -N is the currently-used external-runtime client option. Keep a
	// documented value-taking option and an unknown valueless option in front
	// of it too: the simulator should dispatch the real command positionally.
	cmd := tm.Command("-L", "future-socket", "-x", "-N", "new-session", "-d", "-s", "bridge", "sh", "-c", "sleep 1")
	require.NoError(t, cmd.Run())
	assert.True(t, tm.IsAlive("bridge"))
	assert.Equal(t, 1, tm.CommandCount("new-session"))
}

func TestTmuxSim_RejectsAmbiguousUnknownClientOption(t *testing.T) {
	tm := newTmuxSim()

	// An unknown option followed by a value may be a future value-taking
	// client option. Failing closed is safer than treating that value as the
	// command and accidentally interpreting a later payload as new-session.
	cmd := tm.Command("-future", "option-value", "new-session", "-d", "-s", "payload")
	err := cmd.Run()
	assert.EqualError(t, err, "tmux simulator: cannot determine whether unknown client option \"-future\" takes a value; add it to tmuxClientOptions or tmuxClientOptionsWithValue")
	assert.False(t, tm.IsAlive("payload"))
	assert.Equal(t, 0, tm.CommandCount("new-session"))
}

func TestTmuxSim_DoesNotParsePayloadAsCommand(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "run-shell", args: []string{"run-shell", "new-session", "-d", "-s", "payload"}},
		{name: "send-keys", args: []string{"send-keys", "-t", "payload", "new-session"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tm := newTmuxSim()
			cmd := tm.Command(tc.args...)
			require.NoError(t, cmd.Run())
			assert.False(t, tm.IsAlive("payload"))
			assert.Equal(t, 0, tm.CommandCount("new-session"))
		})
	}
}
