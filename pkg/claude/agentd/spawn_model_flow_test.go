package agentd_test

import (
	"bytes"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
)

// Scenario: an existing agent (or human) spawns a peer with an explicit
// --model. The chosen alias must flow end-to-end — CLI SpawnParams →
// wire SpawnRequest → daemon handleGroupSpawn → spawnParams → the
// Spawner — so the new session is launched with that model. We assert
// at the Spawner boundary (the production seam liveSpawnNew sits on),
// which is exactly where the `--model` flag is later built.
func TestSpawnCLI_ModelThreadsThrough(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)
		f.HaveGroup("alpha")
		bridgeAgentClientToMux(t, f.Mux)
		chdirTo(t, resolveSym(t, t.TempDir()))

		stderr := new(bytes.Buffer)
		resp, rc := agent.RunSpawn(
			&agent.SpawnParams{Group: "alpha", Name: "worker", Model: "sonnet[1m]"},
			new(bytes.Buffer), stderr, new(bytes.Buffer),
		)
		require.Equal(t, 0, rc, "RunSpawn rc, stderr=%s", stderr.String())
		require.NotNil(t, resp, "RunSpawn resp")

		got, ok := f.World.SpawnModel(resp.ConvID)
		require.True(t, ok, "no spawn recorded for conv %s", resp.ConvID)
		assert.Equal(t, "sonnet[1m]", got, "model must thread through to the spawner")
	})
}

// Scenario: spawning WITHOUT a model must thread "" through, so the
// production spawner omits the --model flag and claude keeps its own
// default. This is the headline acceptance bar: unset ⇒ no flag.
func TestSpawnCLI_ModelUnsetThreadsEmpty(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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

		got, ok := f.World.SpawnModel(resp.ConvID)
		require.True(t, ok, "no spawn recorded for conv %s", resp.ConvID)
		assert.Equal(t, "", got, `unset model must thread "" (production omits --model)`)
	})
}

// Scenario: an invalid model alias is rejected client-side before it
// ever reaches the daemon — a clear error, no spawn.
func TestSpawnCLI_InvalidModelRejected(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)
		f.HaveGroup("alpha")
		bridgeAgentClientToMux(t, f.Mux)
		chdirTo(t, resolveSym(t, t.TempDir()))

		stderr := new(bytes.Buffer)
		resp, rc := agent.RunSpawn(
			&agent.SpawnParams{Group: "alpha", Name: "worker", Model: "gpt"},
			new(bytes.Buffer), stderr, new(bytes.Buffer),
		)
		require.NotEqual(t, 0, rc, "invalid model must fail")
		assert.Nil(t, resp, "no spawn response on invalid model")
		assert.Contains(t, stderr.String(), "model", "error should mention model")
	})
}
