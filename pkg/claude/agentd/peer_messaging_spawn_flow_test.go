package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Scenario: Claude Code ships its own cross-session messaging mesh — a session
// binds an inbox socket, finds your other sessions with ListAgents, and
// delivers messages with SendMessage. Under tclaude that is a SECOND,
// unmanaged coordination channel alongside the one tclaude owns: no group
// membership, no permission slugs, no audit trail, nothing in the dashboard.
// tclaude therefore resolves an unset peer-messaging posture to OFF and has the
// launch inject the refusal; an operator who wants the native mesh can opt back
// in per profile or per spawn.
//
// These pin the daemon's resolution at the Spawner boundary (World.
// SpawnPeerMessaging — the same surface the auto-memory spawn flow tests
// assert), plus the recorded per-session posture a relaunch reads back. The
// settings.json rendering itself is unit-tested in harness.PeerMessagingSettings
// and claudeSettingsJSON.

// TestClaudeSpawn_PeerMessagingDefaultsOff: the load-bearing default. A plain CC
// spawn must resolve peer messaging OFF, which is what makes the forked
// `session new` inject crossSessionInbound=refuse and the ListAgents deny.
func TestClaudeSpawn_PeerMessagingDefaultsOff(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("cc-crew")

	spawn := f.AsHuman().SpawnHarness("cc-crew", "plain-worker", "claude")

	got, ok := f.World.SpawnPeerMessaging(spawn.ConvID)
	require.True(t, ok, "the spawn should have been observed by the sim spawner")
	assert.False(t, got,
		"a plain spawn must default peer messaging OFF so agents coordinate through tclaude, "+
			"not through Claude Code's own unmanaged mesh")
}

// TestClaudeSpawn_PeerMessagingOptIn: an explicit peer_messaging:true survives
// to the launch, so an operator can still hand a given agent the native mesh.
func TestClaudeSpawn_PeerMessagingOptIn(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("cc-crew")

	resp := f.AsHuman().SpawnWith("cc-crew", map[string]any{
		"name":           "chatty",
		"peer_messaging": true,
	})
	require.Equal(t, 200, resp.Code,
		"peer_messaging opt-in on a Claude Code spawn must be accepted; body=%s", resp.Raw)

	got, ok := f.World.SpawnPeerMessaging(resp.ConvID)
	require.True(t, ok, "the spawn should have been observed by the sim spawner")
	assert.True(t, got, "an explicit peer_messaging opt-in must reach the launch")
}

// TestClaudeSpawn_PeerMessagingFromProfile: a profile's peer_messaging default
// fills a spawn that said nothing — the same tier behaviour auto_memory uses.
func TestClaudeSpawn_PeerMessagingFromProfile(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("cc-crew")

	rec := createProfile(t, f, map[string]any{
		"name": "mesh-keeper", "harness": "claude", "peer_messaging": true,
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())

	resp := f.AsHuman().SpawnWith("cc-crew", map[string]any{
		"name": "inherits-mesh", "profile": "mesh-keeper",
	})
	require.Equal(t, 200, resp.Code, "spawn with profile; body=%s", resp.Raw)

	got, ok := f.World.SpawnPeerMessaging(resp.ConvID)
	require.True(t, ok, "the spawn should have been observed by the sim spawner")
	assert.True(t, got, "a profile's peer_messaging default must reach the launch")
}

// TestClaudeSpawn_ExplicitPeerMessagingOverridesProfile: an explicit per-spawn
// false beats a profile that opened the mesh — the spawn form is what decides.
func TestClaudeSpawn_ExplicitPeerMessagingOverridesProfile(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("cc-crew")

	rec := createProfile(t, f, map[string]any{
		"name": "mesh-keeper-2", "harness": "claude", "peer_messaging": true,
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())

	resp := f.AsHuman().SpawnWith("cc-crew", map[string]any{
		"name": "opts-back-out", "profile": "mesh-keeper-2", "peer_messaging": false,
	})
	require.Equal(t, 200, resp.Code, "spawn body=%s", resp.Raw)

	got, ok := f.World.SpawnPeerMessaging(resp.ConvID)
	require.True(t, ok, "the spawn should have been observed by the sim spawner")
	assert.False(t, got, "an explicit per-spawn peer_messaging:false must override the profile default")
}

// TestCodexSpawn_RejectsPeerMessaging: Codex has no cross-session messaging
// system, so an opt-in is a 400 at the boundary rather than a setting silently
// dropped.
func TestCodexSpawn_RejectsPeerMessaging(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("codex-crew")

	resp := f.AsHuman().SpawnWith("codex-crew", map[string]any{
		"name":           "no-mesh",
		"harness":        "codex",
		"peer_messaging": true,
	})
	require.Equal(t, 400, resp.Code,
		"peer_messaging on a Codex spawn must be refused with a 400; body=%s", resp.Raw)
	assert.Contains(t, string(resp.Raw), "invalid_peer_messaging",
		"the refusal should name the peer-messaging gate; body=%s", resp.Raw)
}

// TestCodexSpawn_PeerMessagingOffIsFine: false is valid for every harness — it
// is simply never injected for one with no messaging system.
func TestCodexSpawn_PeerMessagingOffIsFine(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("codex-crew")

	resp := f.AsHuman().SpawnWith("codex-crew", map[string]any{
		"name": "codex-plain", "harness": "codex", "peer_messaging": false,
	})
	require.Equal(t, 200, resp.Code, "peer_messaging:false must be accepted for Codex; body=%s", resp.Raw)

	got, ok := f.World.SpawnPeerMessaging(resp.ConvID)
	require.True(t, ok, "the spawn should have been observed by the sim spawner")
	assert.False(t, got)
}
