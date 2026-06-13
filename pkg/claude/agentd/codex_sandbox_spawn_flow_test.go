package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Scenario (JOH-192 acceptance): a daemon-spawned Codex agent runs under
// Codex's OS-native sandbox by default, so it gets the guardrail-integrity
// property (can't forge identity / rewrite the daemon DB) for free — and a
// spawn whose cwd would expose $HOME to that sandbox is refused.
//
// These pin the two halves the ticket calls out:
//   - the emitted Codex spawn carries `--sandbox workspace-write` by
//     default (observed via the simSpawner's recorded sandbox, the same way
//     effort/model are asserted), and
//   - a $HOME-rooted Codex spawn is refused with a 400 rather than launched
//     with a false sense of containment.

// TestCodexSpawn_DefaultsToWorkspaceWriteSandbox: a plain Codex spawn
// resolves to the secure default (workspace-write) and threads it through
// the spawn path.
func TestCodexSpawn_DefaultsToWorkspaceWriteSandbox(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("codex-crew")

	spawn := f.AsHuman().SpawnHarness("codex-crew", "codex-worker", "codex")

	got, ok := f.World.SpawnSandbox(spawn.ConvID)
	require.True(t, ok, "the codex spawn should have been observed by the sim spawner")
	assert.Equal(t, "workspace-write", got,
		"a daemon-spawned Codex agent must default to the workspace-write sandbox")
}

// TestCodexSpawn_RefusedWhenCwdIsHome: spawning a Codex agent rooted at
// $HOME under the (default) workspace-write sandbox would make
// ~/.tclaude / ~/.codex / ~/.claude writable — the daemon refuses with a
// 400 instead of launching it.
func TestCodexSpawn_RefusedWhenCwdIsHome(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("codex-crew")

	resp := f.AsHuman().SpawnWith("codex-crew", map[string]any{
		"name":    "rooted-at-home",
		"harness": "codex",
		"cwd":     f.World.HomeDir, // $HOME — the unsafe cwd
	})
	require.Equal(t, http.StatusBadRequest, resp.Code,
		"a $HOME-rooted Codex spawn must be refused; body=%s", resp.Raw)
	assert.Contains(t, string(resp.Raw), "invalid_cwd",
		"the refusal should be the cwd-safety guard; body=%s", resp.Raw)
}

// TestCodexSpawn_DangerFullAccessBypassesCwdGuard: the danger-full-access
// opt-out disables the sandbox, so the cwd-safety guard does not apply —
// a $HOME-rooted spawn is allowed (the caller explicitly accepted full
// access) and the mode is threaded through verbatim.
func TestCodexSpawn_DangerFullAccessBypassesCwdGuard(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("codex-crew")

	resp := f.AsHuman().SpawnWith("codex-crew", map[string]any{
		"name":    "full-access",
		"harness": "codex",
		"sandbox": "danger-full-access",
		"cwd":     f.World.HomeDir,
	})
	require.Equal(t, http.StatusOK, resp.Code,
		"danger-full-access opts out of the sandbox, so the cwd guard must not fire; body=%s", resp.Raw)

	got, ok := f.World.SpawnSandbox(resp.ConvID)
	require.True(t, ok, "the codex spawn should have been observed by the sim spawner")
	assert.Equal(t, "danger-full-access", got,
		"an explicit sandbox mode must thread through unchanged")
}
