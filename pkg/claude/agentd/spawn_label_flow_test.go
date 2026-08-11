package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// Scenario: the operator has NOT opted in (the default). A spawn's session
// label — the sessions-table PK, and so the tmux session name the human
// attaches to — stays the historical opaque "spwn-XXXXXX" token regardless of
// what the agent is named.
func TestSpawn_LabelIsRandomByDefault(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	resp := f.Spawn("alpha", "code-reviewer")
	assert.Regexpf(t, `^spwn-[0-9a-f]{6}$`, resp.Label,
		"default spawn label should be the random token; body=%s", resp.Raw)
	assert.Equal(t, resp.Label, resp.TmuxSession,
		"the tmux session is named from the label")
}

// Scenario: the operator turned on agent.spawn_label_from_name. Now the agent's
// name IS its session label, so the pane is reachable as `tclaude session
// attach code-reviewer` and shows up under that name in `tmux ls`.
func TestSpawn_LabelFromNameWhenEnabled(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	// executeSpawn reads config live, so persisting it here is enough.
	require.NoError(t, config.Save(&config.Config{
		Agent: &config.AgentConfig{SpawnLabelFromName: true},
	}), "save config")

	resp := f.Spawn("alpha", "code-reviewer")
	assert.Equal(t, "code-reviewer", resp.Label, "label should be the agent's name")
	assert.Equal(t, "code-reviewer", resp.TmuxSession, "tmux session follows the label")
}

// Scenario: two agents are spawned with the SAME name while the flag is on.
// A name is not unique, so the second one is disambiguated with the "-2"
// suffix `session new` uses for a taken tmux name — it must never reuse the
// first agent's session id, which owns that agent's cost/telemetry history.
func TestSpawn_LabelFromNameDisambiguatesDuplicates(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	require.NoError(t, config.Save(&config.Config{
		Agent: &config.AgentConfig{SpawnLabelFromName: true},
	}), "save config")

	first := f.Spawn("alpha", "worker")
	second := f.Spawn("alpha", "worker")

	assert.Equal(t, "worker", first.Label)
	assert.Equal(t, "worker-2", second.Label, "a taken name gets the -N suffix")
	assert.NotEqual(t, first.Label, second.Label, "session ids must never be reused")
	assert.Equal(t, second.Label, second.TmuxSession)
}

// Scenario: the flag is on and the request omits a name. Group spawns now
// derive a readable unique name, so that effective name also supplies the
// label stem — "just give me an agent" stays recognizable and attachable.
func TestSpawn_LabelFromDerivedNameWhenRequestUnnamed(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	require.NoError(t, config.Save(&config.Config{
		Agent: &config.AgentConfig{SpawnLabelFromName: true},
	}), "save config")

	resp := f.SpawnWith("alpha", map[string]any{"name": ""})
	require.Equalf(t, 200, resp.Code, "unnamed spawn should succeed; body=%s", resp.Raw)
	assert.Regexpf(t, `^alpha-[0-9]{8}-[0-9]{4}-[A-Za-z0-9_-]{4}$`, resp.Label,
		"the derived agent name supplies the opted-in label; body=%s", resp.Raw)
	assert.Equal(t, resp.Label, resp.TmuxSession)
}
