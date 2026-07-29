package session

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestTrimOversizedHookBodyCarriesIncompletePayloadEvidence(t *testing.T) {
	req := BrokeredHookRequest{Input: HookCallbackInput{
		HookEventName: "PreToolUse",
		ToolName:      "Write",
		ToolInput:     json.RawMessage(`"` + strings.Repeat("x", hookBrokerBodyBudget) + `"`),
	}}
	body, err := json.Marshal(req)
	assert.NoError(t, err)
	assert.Greater(t, len(body), hookBrokerBodyBudget)

	trimmed := trimOversizedHookBody(req, body)
	var got BrokeredHookRequest
	assert.NoError(t, json.Unmarshal(trimmed, &got))
	assert.True(t, got.Input.PayloadTrimmed)
	assert.Empty(t, got.Input.ToolInput)
}

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
	assert.NotEmpty(t, amb.FallbackCwd(), "the working directory is the auto-registration fallback")
}

// A task signal outside CacheDir must neither be honoured as a write
// target nor relax the task-mode guard exemptions.
func TestLocalHookAmbient_RejectsOutOfBoundsTaskSignal(t *testing.T) {
	t.Setenv("TCLAUDE_TASK_SIGNAL", "/etc/passwd")
	assert.False(t, LocalHookAmbient().InTaskRunnerHook(),
		"an out-of-bounds signal path must not enable task mode")
}

// The expensive ambient lookups must stay lazy and memoised.
// GetCurrentTmuxSession execs `tmux display-message` and FindClaudePID
// walks /proc; before this seam they ran only on the two rare paths that
// needed them. Resolving them for every event would put a subprocess
// spawn on the critical path of every PreToolUse/PostToolUse of every
// harness-builtin agent — a change to the launch mode required not to
// change.
func TestLocalHookAmbient_ExpensiveLookupsAreLazyAndMemoised(t *testing.T) {
	var pidCalls, tmuxCalls, cwdCalls int
	amb := HookAmbient{
		harnessPID:  countingOnce(&pidCalls, func() int { return 7 }),
		tmuxSession: countingOnce(&tmuxCalls, func() string { return "pane" }),
		fallbackCwd: countingOnce(&cwdCalls, func() string { return "/w" }),
	}

	assert.Zero(t, pidCalls, "constructing the ambient must not resolve the harness pid")
	assert.Zero(t, tmuxCalls, "constructing the ambient must not shell out to tmux")
	assert.Zero(t, cwdCalls, "constructing the ambient must not stat the working directory")

	for range 3 {
		assert.Equal(t, 7, amb.HarnessPID())
		assert.Equal(t, "pane", amb.TmuxSession())
		assert.Equal(t, "/w", amb.FallbackCwd())
	}
	assert.Equal(t, 1, pidCalls, "repeated reads within one event must not re-walk /proc")
	assert.Equal(t, 1, tmuxCalls, "repeated reads within one event must not re-exec tmux")
	assert.Equal(t, 1, cwdCalls)
}

// A zero HookAmbient must be inert rather than a nil-dereference, so a
// caller that forgets to build one degrades the way "not in tmux, pid
// unknown" already degrades.
func TestHookAmbient_ZeroValueIsInert(t *testing.T) {
	var amb HookAmbient
	assert.Zero(t, amb.HarnessPID())
	assert.Empty(t, amb.TmuxSession())
	assert.Empty(t, amb.FallbackCwd())
	assert.False(t, amb.InTaskRunnerHook())
}

func countingOnce[T any](calls *int, fn func() T) func() T {
	var once sync.Once
	var val T
	return func() T {
		once.Do(func() { *calls++; val = fn() })
		return val
	}
}

// The brokered context takes its pane, cwd and pid from the session row
// the daemon resolved, and deliberately never enters task mode: the signal
// path is a host-side file write whose target the sandbox would otherwise
// choose. brokerHookEvents also refuses to broker at all while a task
// signal is set, so this is the second of two lines.
func TestBrokeredHookAmbient_UsesRowFactsAndNeverTaskMode(t *testing.T) {
	t.Setenv("TCLAUDE_TASK_SIGNAL", "")

	amb := BrokeredHookAmbient(BrokeredHookContext{
		RowTmuxSession:    "tmux-row",
		RowCwd:            "/home/u/proj",
		HarnessPID:        4242,
		ExitGeneration:    "gen-xyz",
		AutoCompactWindow: "200000",
	})

	assert.Equal(t, "tmux-row", amb.TmuxSession(),
		"the pane must come from the resolved row, not the daemon's own (absent) $TMUX")
	assert.Equal(t, "/home/u/proj", amb.FallbackCwd(),
		"the cwd fallback must come from the row, not the daemon's working directory")
	assert.Equal(t, 4242, amb.HarnessPID(),
		"the harness pid must come from the resolved ancestry, not the daemon's parents")
	assert.Equal(t, "gen-xyz", amb.ExitGeneration)
	assert.Equal(t, "200000", amb.AutoCompactWindow)
	assert.Empty(t, amb.TaskSignalPath, "the broker must not carry a task signal path")
	assert.False(t, amb.InTaskRunnerHook(), "brokered events are never in task mode")
}
