package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Scenario: Claude Code's auto-memory system writes a per-project memory store
// that EVERY tclaude agent on that repo reads, so several agents working one
// codebase cross-pollute each other's notes. tclaude therefore resolves an
// unset auto-memory posture to OFF and has the launch inject
// CLAUDE_CODE_DISABLE_AUTO_MEMORY=1; an operator who wants memory can opt back
// in per profile or per spawn.
//
// These pin the daemon's resolution at the Spawner boundary (World.
// SpawnAutoMemory — the same surface the remote-control / trust-dir spawn flow
// tests assert), plus the recorded per-session posture a relaunch reads back.
// The env-var rendering itself is unit-tested in session.ApplyAutoMemoryEnv.

// TestClaudeSpawn_AutoMemoryDefaultsOff: the load-bearing default. A plain CC
// spawn must resolve auto memory OFF, which is what makes the forked `session
// new` inject the disable.
func TestClaudeSpawn_AutoMemoryDefaultsOff(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("cc-crew")

	spawn := f.AsHuman().SpawnHarness("cc-crew", "plain-worker", "claude")

	got, ok := f.World.SpawnAutoMemory(spawn.ConvID)
	require.True(t, ok, "the spawn should have been observed by the sim spawner")
	assert.False(t, got,
		"a plain spawn must default auto memory OFF so agents don't share one project memory store")
}

// TestClaudeSpawn_AutoMemoryOptIn: an explicit auto_memory:true survives to the
// launch, so an operator can still keep Claude Code's memory for a given agent.
func TestClaudeSpawn_AutoMemoryOptIn(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("cc-crew")

	resp := f.AsHuman().SpawnWith("cc-crew", map[string]any{
		"name":        "remembers",
		"auto_memory": true,
	})
	require.Equal(t, 200, resp.Code,
		"auto_memory opt-in on a Claude Code spawn must be accepted; body=%s", resp.Raw)

	got, ok := f.World.SpawnAutoMemory(resp.ConvID)
	require.True(t, ok, "the spawn should have been observed by the sim spawner")
	assert.True(t, got, "an explicit auto_memory opt-in must reach the launch")
}

// TestClaudeSpawn_AutoMemoryFromProfile: a profile's auto_memory default fills a
// spawn that said nothing, the same tier behaviour trust_dir / auto_review use.
func TestClaudeSpawn_AutoMemoryFromProfile(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("cc-crew")

	rec := createProfile(t, f, map[string]any{
		"name": "memory-keeper", "harness": "claude", "auto_memory": true,
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())

	resp := f.AsHuman().SpawnWith("cc-crew", map[string]any{
		"name": "inherits-memory", "profile": "memory-keeper",
	})
	require.Equal(t, 200, resp.Code, "spawn with profile; body=%s", resp.Raw)

	got, ok := f.World.SpawnAutoMemory(resp.ConvID)
	require.True(t, ok, "the spawn should have been observed by the sim spawner")
	assert.True(t, got, "a profile's auto_memory default must reach the launch")
}

// TestClaudeSpawn_ExplicitAutoMemoryOverridesProfile: an explicit per-spawn
// false beats a profile that turned memory on — the spawn form is what decides.
func TestClaudeSpawn_ExplicitAutoMemoryOverridesProfile(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("cc-crew")

	rec := createProfile(t, f, map[string]any{
		"name": "memory-keeper-2", "harness": "claude", "auto_memory": true,
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())

	resp := f.AsHuman().SpawnWith("cc-crew", map[string]any{
		"name": "opts-back-out", "profile": "memory-keeper-2", "auto_memory": false,
	})
	require.Equal(t, 200, resp.Code, "spawn body=%s", resp.Raw)

	got, ok := f.World.SpawnAutoMemory(resp.ConvID)
	require.True(t, ok, "the spawn should have been observed by the sim spawner")
	assert.False(t, got, "an explicit per-spawn auto_memory:false must override the profile default")
}

// TestCodexSpawn_RejectsAutoMemory: Codex has no auto-memory system, so an
// opt-in is a 400 at the boundary rather than a setting silently dropped.
func TestCodexSpawn_RejectsAutoMemory(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("codex-crew")

	resp := f.AsHuman().SpawnWith("codex-crew", map[string]any{
		"name":        "no-memory",
		"harness":     "codex",
		"auto_memory": true,
	})
	require.Equal(t, 400, resp.Code,
		"auto_memory on a Codex spawn must be refused with a 400; body=%s", resp.Raw)
	assert.Contains(t, string(resp.Raw), "invalid_auto_memory",
		"the refusal should name the auto-memory gate; body=%s", resp.Raw)
}

// TestCodexSpawn_AutoMemoryOffIsFine: false is valid for every harness — it is
// simply never injected for one with no auto-memory switch.
func TestCodexSpawn_AutoMemoryOffIsFine(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("codex-crew")

	resp := f.AsHuman().SpawnWith("codex-crew", map[string]any{
		"name": "codex-plain", "harness": "codex", "auto_memory": false,
	})
	require.Equal(t, 200, resp.Code, "auto_memory:false must be accepted for Codex; body=%s", resp.Raw)

	got, ok := f.World.SpawnAutoMemory(resp.ConvID)
	require.True(t, ok, "the spawn should have been observed by the sim spawner")
	assert.False(t, got)
}
