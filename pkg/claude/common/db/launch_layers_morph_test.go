package db

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/launchspec"
)

// The morph's explicit entries use launchspec's field keys and request field
// names, which the relaunch resolver and the re-apply preview read.
func TestMorphExplicitValuesUseLaunchspecNames(t *testing.T) {
	values := morphExplicitValues(AgentMorphForm{Harness: "codex", HarnessBuiltinMode: morphStr("tclaude-agent"),
		Model: morphStr("opus"), Effort: morphStr("high"), Approval: morphStr("plan"),
		Tools: morphStr("strict"), AskTimeout: morphStr("5m"), AutoCompactWindow: morphStr("200k")}, AgentMorphForm{}, time.Now())
	require.Len(t, values, 8)
	for key, value := range values {
		field, ok := launchspec.Lookup(key)
		require.Truef(t, ok, "%s is not a launchspec key", key)
		for name := range value.Fields {
			assert.Truef(t, slices.Contains(field.RequestFields, name), "%s: %s is not one of its request fields", key, name)
		}
	}
	assert.Empty(t, morphExplicitValues(AgentMorphForm{}, AgentMorphForm{}, time.Now()), "an unset field is not recorded")
	unchanged := morphExplicitValues(AgentMorphForm{Approval: morphStr("plan"), Tools: morphStr("")},
		AgentMorphForm{Approval: morphStr("auto"), Tools: morphStr("")}, time.Now())
	assert.Equal(t, []string{"approval"}, slices.Collect(maps.Keys(unchanged)), "a field set to its current value is not recorded")
}

func morphStr(v string) *string { return &v }

func recordExplicit(t *testing.T, agentID string, values map[string]ExplicitLaunchValue) {
	t.Helper()
	applyMorphExplicit(t, agentID, values, false)
}

func applyMorphExplicit(t *testing.T, agentID string, values map[string]ExplicitLaunchValue, back bool) {
	t.Helper()
	d, err := Open()
	require.NoError(t, err)
	tx, err := d.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	require.NoError(t, applyMorphExplicitTx(tx, agentID, values, back))
	require.NoError(t, tx.Commit())
}

// A recorded value merges into the field's entry and keeps the other layers;
// an agent without layers stays without them.
func TestRecordExplicitLaunchValuesMerges(t *testing.T) {
	setupTestDB(t)
	agentID, err := AllocateAgent("conv-morph", "spawn")
	require.NoError(t, err)
	at := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	spawnCopy := &AgentSpawnCopy{CopiedAt: at}
	require.NoError(t, SetAgentLaunchLayers(agentID, &AgentLaunchLayers{
		SpawnCopy: spawnCopy,
		Explicit: map[string]ExplicitLaunchValue{"context_window": {
			Fields:     map[string]json.RawMessage{"ContextWindowMax": json.RawMessage(`64000`)},
			Provenance: ExplicitAtSpawn, SetAt: at}},
	}))
	later := at.Add(time.Hour)
	recordExplicit(t, agentID, morphExplicitValues(AgentMorphForm{AutoCompactWindow: morphStr("200k")}, AgentMorphForm{}, later))

	got, err := AgentLaunchLayersFor(agentID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.NotNil(t, got.SpawnCopy, "the spawn copy stays")
	window := got.Explicit["context_window"]
	assert.JSONEq(t, `64000`, string(window.Fields["ContextWindowMax"]), "the spawn's cap stays recorded")
	assert.JSONEq(t, `"200k"`, string(window.Fields["AutoCompactWindow"]))
	assert.Equal(t, ExplicitAtMorph(later), window.Provenance)

	old, err := AllocateAgent("conv-old", "spawn")
	require.NoError(t, err)
	recordExplicit(t, old, morphExplicitValues(AgentMorphForm{AutoCompactWindow: morphStr("200k")}, AgentMorphForm{}, later))
	layers, err := AgentLaunchLayersFor(old)
	require.NoError(t, err)
	assert.Nil(t, layers, "a pre-change agent stays pre-change")
}

// A morph back puts back what the morph replaced, and a second one redoes
// the morph.
func TestApplyMorphExplicitBackUndoesAndRedoes(t *testing.T) {
	setupTestDB(t)
	agentID, err := AllocateAgent("conv-back", "spawn")
	require.NoError(t, err)
	at := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	spawned := ExplicitLaunchValue{Fields: map[string]json.RawMessage{"Effort": json.RawMessage(`"low"`)}, Provenance: ExplicitAtSpawn, SetAt: at}
	require.NoError(t, SetAgentLaunchLayers(agentID, &AgentLaunchLayers{
		SpawnCopy: &AgentSpawnCopy{CopiedAt: at},
		Explicit:  map[string]ExplicitLaunchValue{"effort": spawned},
	}))
	explicit := func() map[string]ExplicitLaunchValue {
		layers, err := AgentLaunchLayersFor(agentID)
		require.NoError(t, err)
		require.NotNil(t, layers)
		return layers.Explicit
	}

	later := at.Add(time.Hour)
	recordExplicit(t, agentID, morphExplicitValues(AgentMorphForm{Effort: morphStr("high"), Approval: morphStr("plan")},
		AgentMorphForm{Effort: morphStr("low")}, later))
	morphed := explicit()
	assert.Equal(t, ExplicitAtMorph(later), morphed["effort"].Provenance)
	assert.Contains(t, morphed, "approval")

	applyMorphExplicit(t, agentID, nil, true)
	undone := explicit()
	assert.Equal(t, map[string]ExplicitLaunchValue{"effort": spawned}["effort"].Fields, undone["effort"].Fields)
	assert.Equal(t, ExplicitAtSpawn, undone["effort"].Provenance, "the spawn's value is back")
	assert.NotContains(t, undone, "approval", "a value the morph added is gone again")

	applyMorphExplicit(t, agentID, nil, true)
	redone := explicit()
	assert.Equal(t, morphed["effort"].Provenance, redone["effort"].Provenance, "a second morph back redoes the morph")
	assert.Contains(t, redone, "approval")
}
