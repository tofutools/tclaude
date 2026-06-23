package agentd_test

import (
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
)

// Flow coverage for JOH-205 inc4 Part B — the opt-in dir-trust flag threading
// from the spawn body through to the forked `tclaude session new --trust-dir`.
//
// The simulator spawns in-process, so it does NOT perform the real
// ~/.codex/config.toml write (that is covered exhaustively by the harness
// package's editor unit tests). What these scenarios pin is the PLUMBING: that
// the `trust_dir` body field is gated (Codex-only) and threaded all the way to
// the spawner, captured here via World.SpawnTrustDir. They are the trust-dir
// analog of the sandbox/approval/auto-review body-contract tests.

// Scenario: a Codex spawn that ticks the dashboard's "pre-trust this dir"
// checkbox sends {"trust_dir": true}; the daemon threads it to the spawner so
// the forked session would write the trust entry before launch.
func TestCodexSpawn_TrustDirThreadsWhenOptedIn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
		f := newFlow(t)
		f.HaveGroup("squad")

		spawn := f.SpawnWith("squad", map[string]any{
			"name":      "cdx-trusted",
			"harness":   "codex",
			"trust_dir": true,
		})
		require.Equal(t, 200, spawn.Code, "spawn body=%s", string(spawn.Raw))
		require.NotEmpty(t, spawn.ConvID, "spawn returned a conv id")

		got, ok := f.World.SpawnTrustDir(spawn.ConvID)
		require.True(t, ok, "the spawner recorded a trust-dir flag for the new conv")
		assert.True(t, got, "trust_dir:true must thread `--trust-dir` to the spawner")
	})
}

// Scenario: the default — a Codex spawn that does NOT request dir-trust never
// gets it. Pins "never auto-defaulted": the flag is false unless explicitly set.
func TestCodexSpawn_TrustDirOffByDefault(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
		f := newFlow(t)
		f.HaveGroup("squad")

		spawn := f.SpawnWith("squad", map[string]any{
			"name":    "cdx-untrusted",
			"harness": "codex",
		})
		require.Equal(t, 200, spawn.Code, "spawn body=%s", string(spawn.Raw))

		got, ok := f.World.SpawnTrustDir(spawn.ConvID)
		require.True(t, ok, "the spawner recorded a trust-dir flag for the new conv")
		assert.False(t, got, "an unrequested dir-trust must default off — never auto-trusted")
	})
}

// Scenario: requesting dir-trust for a non-Codex harness (Claude Code) is a
// 400, not a silently dropped flag — Claude Code has no trust-folder modal, and
// the flag would otherwise have no meaning. Mirrors how sandbox/approval reject
// a value for a harness that doesn't take one.
func TestCodexSpawn_TrustDirRejectedForClaude(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
		f := newFlow(t)
		f.HaveGroup("squad")

		spawn := f.SpawnWith("squad", map[string]any{
			"name":      "cc-trusted",
			"trust_dir": true, // no harness → Claude Code, which has no dir-trust
		})
		assert.Equal(t, 400, spawn.Code, "trust_dir for Claude Code must be a 400; body=%s", string(spawn.Raw))
	})
}
