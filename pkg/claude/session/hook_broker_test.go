package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// The routing decision is the whole reason TCL-754 needs a marker at all:
// inside the wall the database is an empty writable tmpfs, so a hook that
// tried the direct path would succeed into a throwaway database instead of
// failing. These pin that the marker decides, and that nothing else does.
func TestBrokerHookEvents_MarkerDecides(t *testing.T) {
	// No HOME → no resolvable database path → the backstop cannot fire, so
	// only the marker is under test here.
	t.Setenv("HOME", "")

	t.Run("marker set routes to the broker", func(t *testing.T) {
		t.Setenv(HookBrokerEnvVar, HookBrokerAgentd)
		assert.True(t, brokerHookEvents())
	})

	t.Run("no marker keeps the direct path", func(t *testing.T) {
		t.Setenv(HookBrokerEnvVar, "")
		assert.False(t, brokerHookEvents())
	})

	t.Run("an unrecognised marker value is not a broker instruction", func(t *testing.T) {
		t.Setenv(HookBrokerEnvVar, "yes")
		assert.False(t, brokerHookEvents(),
			"only the exact %q value routes; anything else must not silently change behaviour",
			HookBrokerAgentd)
	})
}

// LocalHookAmbient is the direct path's context. The values it carries are
// the ones ApplyHook used to read inline, so a regression here is a silent
// behaviour change for every harness-builtin agent — the one thing the
// broker work was required not to touch.
func TestLocalHookAmbient_ReadsTheLaunchEnvironment(t *testing.T) {
	t.Setenv("TCLAUDE_EXIT_GENERATION", "gen-abc123")
	t.Setenv(harness.AutoCompactWindowEnvVar, "450000")
	t.Setenv("TCLAUDE_TASK_SIGNAL", "")

	amb := LocalHookAmbient()
	assert.Equal(t, "gen-abc123", amb.ExitGeneration)
	assert.Equal(t, "450000", amb.AutoCompactWindow)
	assert.False(t, amb.InTaskRunnerHook(), "no signal path means no task mode")
	assert.NotEmpty(t, amb.FallbackCwd, "the working directory is the auto-registration fallback")
}

// A task signal outside CacheDir must neither be honoured as a write
// target nor relax the task-mode guard exemptions.
func TestLocalHookAmbient_RejectsOutOfBoundsTaskSignal(t *testing.T) {
	t.Setenv("TCLAUDE_TASK_SIGNAL", "/etc/passwd")
	assert.False(t, LocalHookAmbient().InTaskRunnerHook(),
		"an out-of-bounds signal path must not enable task mode")
}

// The brokered context takes its pane, cwd and pid from the session row
// the daemon resolved, and deliberately never enters task mode: the signal
// path is a host-side file write whose target the sandbox would otherwise
// choose, and `tclaude task run` soft-disables hooks anyway, so nothing
// legitimate reaches the broker in task mode.
func TestBrokeredHookAmbient_UsesRowFactsAndNeverTaskMode(t *testing.T) {
	t.Setenv("TCLAUDE_TASK_SIGNAL", "")

	amb := BrokeredHookAmbient(BrokeredHookContext{
		RowTmuxSession:    "tmux-row",
		RowCwd:            "/home/u/proj",
		HarnessPID:        4242,
		ExitGeneration:    "gen-xyz",
		AutoCompactWindow: "200000",
	})

	assert.Equal(t, "tmux-row", amb.TmuxSession,
		"the pane must come from the resolved row, not the daemon's own (absent) $TMUX")
	assert.Equal(t, "/home/u/proj", amb.FallbackCwd,
		"the cwd fallback must come from the row, not the daemon's working directory")
	assert.Equal(t, 4242, amb.HarnessPID,
		"the harness pid must come from the resolved ancestry, not the daemon's parents")
	assert.Equal(t, "gen-xyz", amb.ExitGeneration)
	assert.Equal(t, "200000", amb.AutoCompactWindow)
	assert.Empty(t, amb.TaskSignalPath, "the broker must not carry a task signal path")
	assert.False(t, amb.InTaskRunnerHook(), "brokered events are never in task mode")
}
