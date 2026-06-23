package agentd_test

import (
	"net/http"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// Scenario (JOH-192 / JOH-207 acceptance): a daemon-spawned Codex agent runs
// sandboxed by default, so it gets the guardrail-integrity property (can't forge
// identity / rewrite the daemon DB) for free — and a spawn whose cwd would
// expose $HOME to that sandbox is refused.
//
// These pin the halves the tickets call out:
//   - the default Codex spawn resolves to the managed-profile pseudo-mode
//     (tclaude-agent) — workspace-write containment PLUS agentd-socket access —
//     observed via the simSpawner's recorded sandbox, the same way effort/model
//     are asserted,
//   - an explicit RAW sandbox mode (e.g. workspace-write) threads through
//     unchanged, distinct from the managed-profile default, and
//   - a $HOME-rooted Codex spawn is refused with a 400 rather than launched
//     with a false sense of containment.

// TestCodexSpawn_DefaultsToManagedProfile: a plain Codex spawn resolves to the
// secure default — the managed tclaude-agent profile — and threads it through
// the spawn path.
func TestCodexSpawn_DefaultsToManagedProfile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)
		f.HaveGroup("codex-crew")

		spawn := f.AsHuman().SpawnHarness("codex-crew", "codex-worker", "codex")

		got, ok := f.World.SpawnSandbox(spawn.ConvID)
		require.True(t, ok, "the codex spawn should have been observed by the sim spawner")
		assert.Equal(t, harness.SandboxManagedProfile, got,
			"a daemon-spawned Codex agent must default to the managed tclaude-agent profile")
	})
}

// TestCodexSpawn_ExplicitWorkspaceWriteIsRaw: choosing the raw workspace-write
// mode in the spawn dialog threads it through verbatim (distinct from the
// managed-profile default), so the agent runs under Codex's native --sandbox
// without the agentd-socket allowlist.
func TestCodexSpawn_ExplicitWorkspaceWriteIsRaw(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)
		f.HaveGroup("codex-crew")

		resp := f.AsHuman().SpawnWith("codex-crew", map[string]any{
			"name":    "raw-ws",
			"harness": "codex",
			"sandbox": harness.SandboxWorkspaceWrite,
		})
		require.Equal(t, http.StatusOK, resp.Code, "explicit workspace-write spawn should succeed; body=%s", resp.Raw)

		got, ok := f.World.SpawnSandbox(resp.ConvID)
		require.True(t, ok, "the codex spawn should have been observed by the sim spawner")
		assert.Equal(t, harness.SandboxWorkspaceWrite, got,
			"an explicit raw sandbox mode must thread through unchanged, not become the managed profile")
	})
}

// TestCodexSpawn_RefusedWhenCwdIsHome: spawning a Codex agent rooted at
// $HOME under the (default) workspace-write sandbox would make
// ~/.tclaude / ~/.codex / ~/.claude writable — the daemon refuses with a
// 400 instead of launching it.
func TestCodexSpawn_RefusedWhenCwdIsHome(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
	})
}

// TestCodexSpawn_DangerFullAccessBypassesCwdGuard: the danger-full-access
// opt-out disables the sandbox, so the cwd-safety guard does not apply —
// a $HOME-rooted spawn is allowed (the caller explicitly accepted full
// access) and the mode is threaded through verbatim.
func TestCodexSpawn_DangerFullAccessBypassesCwdGuard(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
	})
}
