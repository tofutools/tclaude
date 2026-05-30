package agentd_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
)

// Scenario: an existing agent (or human) spawns a peer with an explicit
// --effort. The chosen level must flow end-to-end — CLI SpawnParams →
// wire SpawnRequest → daemon handleGroupSpawn → spawnParams → the
// Spawner — so the new session is launched with that effort. We assert
// at the Spawner boundary (the production seam liveSpawnNew sits on),
// which is exactly where the `--effort` flag is later built.
func TestSpawnCLI_EffortThreadsThrough(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	bridgeAgentClientToMux(t, f.Mux)
	chdirTo(t, resolveSym(t, t.TempDir()))

	stderr := new(bytes.Buffer)
	resp, rc := agent.RunSpawn(
		&agent.SpawnParams{Group: "alpha", Name: "worker", Effort: "high"},
		new(bytes.Buffer), stderr, new(bytes.Buffer),
	)
	require.Equal(t, 0, rc, "RunSpawn rc, stderr=%s", stderr.String())
	require.NotNil(t, resp, "RunSpawn resp")

	got, ok := f.World.SpawnEffort(resp.ConvID)
	require.True(t, ok, "no spawn recorded for conv %s", resp.ConvID)
	assert.Equal(t, "high", got, "effort must thread through to the spawner")
}

// Scenario: spawning WITHOUT an effort must thread "" through, so the
// production spawner omits the --effort flag and claude keeps its own
// default. This is the headline acceptance bar: unset ⇒ no flag.
func TestSpawnCLI_EffortUnsetThreadsEmpty(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	bridgeAgentClientToMux(t, f.Mux)
	chdirTo(t, resolveSym(t, t.TempDir()))

	resp, rc := agent.RunSpawn(
		&agent.SpawnParams{Group: "alpha", Name: "worker"},
		new(bytes.Buffer), new(bytes.Buffer), new(bytes.Buffer),
	)
	require.Equal(t, 0, rc, "RunSpawn rc")
	require.NotNil(t, resp, "RunSpawn resp")

	got, ok := f.World.SpawnEffort(resp.ConvID)
	require.True(t, ok, "no spawn recorded for conv %s", resp.ConvID)
	assert.Equal(t, "", got, `unset effort must thread "" (production omits --effort)`)
}

// Scenario: an invalid effort level is rejected client-side before it
// ever reaches the daemon — a clear error, no spawn.
func TestSpawnCLI_InvalidEffortRejected(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	bridgeAgentClientToMux(t, f.Mux)
	chdirTo(t, resolveSym(t, t.TempDir()))

	stderr := new(bytes.Buffer)
	resp, rc := agent.RunSpawn(
		&agent.SpawnParams{Group: "alpha", Name: "worker", Effort: "ultra"},
		new(bytes.Buffer), stderr, new(bytes.Buffer),
	)
	require.NotEqual(t, 0, rc, "invalid effort must fail")
	assert.Nil(t, resp, "no spawn response on invalid effort")
	assert.Contains(t, stderr.String(), "effort", "error should mention effort")
}
