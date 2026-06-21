package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Scenario (JOH-258): a spawn can arm Claude Code's built-in Remote Access at
// launch via --remote-control, so the new agent is reachable from the Claude
// app from turn one. The daemon threads the opt-in to the forked `tclaude
// session new` (→ `claude --remote-control`) AND tags the new session row's
// best-known remote_control state out-of-band, so the dashboard indicator + the
// toggle's direction logic start armed. A plain spawn leaves it off; a Codex
// spawn — which has no Remote Access — rejects the opt-in at the boundary.
//
// These pin both halves: the threaded flag (via the simSpawner's recorded
// RemoteControl, the same surface the sandbox/approval/auto-review flow tests
// assert) and the out-of-band row tag (via db.RemoteControlForConv, the source
// the dashboard + CLI read).

// TestClaudeSpawn_RemoteControlDefaultsOff: a plain CC spawn does not arm Remote
// Access, and the row's best-known state stays off.
func TestClaudeSpawn_RemoteControlDefaultsOff(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("cc-crew")

	spawn := f.AsHuman().SpawnHarness("cc-crew", "plain-worker", "claude")

	got, ok := f.World.SpawnRemoteControl(spawn.ConvID)
	require.True(t, ok, "the spawn should have been observed by the sim spawner")
	assert.False(t, got, "a plain spawn must default remote-control OFF (no --remote-control)")

	rc, err := db.RemoteControlForConv(spawn.ConvID)
	require.NoError(t, err)
	assert.False(t, rc, "a plain spawn must leave the row's best-known remote_control off")
}

// TestClaudeSpawn_RemoteControlOptInArmsAtLaunch: an explicit remote_control:true
// threads --remote-control through the spawn path AND tags the new row enabled,
// so the agent boots phone-reachable and the dashboard shows it armed.
func TestClaudeSpawn_RemoteControlOptInArmsAtLaunch(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("cc-crew")

	resp := f.AsHuman().SpawnWith("cc-crew", map[string]any{
		"name":           "phone-reachable",
		"remote_control": true,
	})
	require.Equal(t, 200, resp.Code,
		"remote_control opt-in on a Claude Code spawn must be accepted; body=%s", resp.Raw)

	got, ok := f.World.SpawnRemoteControl(resp.ConvID)
	require.True(t, ok, "the spawn should have been observed by the sim spawner")
	assert.True(t, got,
		"an explicit remote_control opt-in must thread --remote-control through the spawn path")

	rc, err := db.RemoteControlForConv(resp.ConvID)
	require.NoError(t, err)
	assert.True(t, rc,
		"the spawn must tag the new row's best-known remote_control on, so the dashboard + toggle start armed")
}

// TestCodexSpawn_RejectsRemoteControl: Codex has no built-in Remote Access, so a
// remote_control opt-in is a 400 at the boundary, not a flag silently dropped
// onto a harness that can't honour it.
func TestCodexSpawn_RejectsRemoteControl(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("codex-crew")

	resp := f.AsHuman().SpawnWith("codex-crew", map[string]any{
		"name":           "no-remote",
		"harness":        "codex",
		"remote_control": true,
	})
	require.Equal(t, 400, resp.Code,
		"remote_control on a Codex spawn must be refused with a 400; body=%s", resp.Raw)
	assert.Contains(t, string(resp.Raw), "invalid_remote_control",
		"the refusal should name the remote-control gate; body=%s", resp.Raw)
}
