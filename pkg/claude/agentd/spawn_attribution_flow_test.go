package agentd_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Background — the spawn kickoff-message misattribution
//
// The first thing a freshly-spawned agent sees in its tmux pane is the
// [system: ...] welcome built by buildSpawnWelcome. Its opening clause
// used to be hard-coded "spawned by the human" — even when the spawn
// was requested by ANOTHER agent (a PO orchestrating workers via
// `tclaude agent spawn`). That misled the new agent about who it was
// working for.
//
// The fix threads the spawn requester's conv-id (handleGroupSpawn's
// spawnerConvID, from socket peer credentials) through spawnParams →
// runSpawnPostInit, resolves it to a display name, and attributes the
// real spawner. A human-initiated spawn (conv-id "") keeps the human
// framing.

// Scenario: an agent (a PO) spawns a worker into a group it belongs to.
//
// Expected: the worker's kickoff welcome attributes the PO by its
// conversation title — "spawned by tclaude-PO" — and never claims the
// human did it.
func TestSpawn_AgentSpawner_KickoffAttributesSpawner(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	const poConv = "popo-aaaa-bbbb-cccc-111111111111"
	// The spawner's attribution name is its conversation title — the
	// same name FreshTitle resolves for every other listing surface.
	f.HaveConvWithTitle(poConv, "tclaude-PO")
	f.HaveMember("alpha", poConv)
	require.NoError(t, db.GrantAgentPermission(poConv, agentd.PermGroupsSpawn, "test"))

	spawn := f.AsAgent(poConv).Spawn("alpha", "worker")
	target := spawn.TmuxTarget()

	// The post-spawn injection renames the pane, then drops the welcome.
	// The welcome's opening clause must name the PO, not the human.
	f.AssertSentContains(target, "/rename worker", 5*time.Second)
	f.AssertSentContains(target, "spawned by tclaude-PO", 5*time.Second)

	// Airtight: with an agent spawner, the misattributing phrase must
	// appear in NO keystroke sent to the new pane.
	for _, sk := range f.World.Tmux.Sent() {
		if sk.Target != target {
			continue
		}
		assert.NotContains(t, sk.Text, "spawned by the human",
			"an agent-spawned agent must not be told the human spawned it")
	}
}

// Scenario: a human spawns a worker directly (plain `tclaude session
// new` / dashboard spawn modal — no Claude ancestor, so spawnerConvID
// resolves to "").
//
// Expected: the kickoff welcome keeps the "spawned by the human"
// framing — the fix must not regress the human case.
func TestSpawn_HumanSpawner_KickoffAttributesHuman(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	spawn := f.AsHuman().Spawn("alpha", "worker")
	target := spawn.TmuxTarget()

	f.AssertSentContains(target, "/rename worker", 5*time.Second)
	f.AssertSentContains(target, "spawned by the human", 5*time.Second)
}

// Scenario: an agent spawns a worker, but the spawner's conv-id has no
// resolvable display name (no conv_index row at all). This is a
// degenerate case for a live spawner, but the welcome must still not
// fall back to claiming the human did it.
//
// Expected: the welcome reads "spawned by another agent" — honest
// about the agent origin even when the name can't be resolved.
func TestSpawn_AgentSpawner_UnresolvableName_StillNotHuman(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	const poConv = "popo-aaaa-bbbb-cccc-222222222222"
	// Enrolled + a group member so it passes the spawn guardrails, but
	// deliberately given NO conv_index row — FreshTitle can't name it.
	f.HaveMember("alpha", poConv)
	require.NoError(t, db.GrantAgentPermission(poConv, agentd.PermGroupsSpawn, "test"))

	spawn := f.AsAgent(poConv).SpawnWith("alpha", map[string]any{"name": "worker"})
	require.Equal(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	target := spawn.TmuxTarget()

	f.AssertSentContains(target, "spawned by another agent", 5*time.Second)
	for _, sk := range f.World.Tmux.Sent() {
		if sk.Target != target {
			continue
		}
		assert.NotContains(t, sk.Text, "spawned by the human",
			"an agent-spawned agent must not be told the human spawned it")
	}
}
