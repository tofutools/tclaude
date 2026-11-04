package db

import (
	"encoding/json"
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
	values := morphExplicitValues(AgentMorphForm{Model: morphStr("opus"), Effort: morphStr("high"), Approval: morphStr("plan"),
		Tools: morphStr("strict"), AskTimeout: morphStr("5m"), AutoCompactWindow: morphStr("200k")}, time.Now())
	require.Len(t, values, 6)
	for key, value := range values {
		field, ok := launchspec.Lookup(key)
		require.Truef(t, ok, "%s is not a launchspec key", key)
		for name := range value.Fields {
			assert.Truef(t, slices.Contains(field.RequestFields, name), "%s: %s is not one of its request fields", key, name)
		}
	}
	assert.Empty(t, morphExplicitValues(AgentMorphForm{}, time.Now()), "an unset field is not recorded")
}

func morphStr(v string) *string { return &v }

func recordExplicit(t *testing.T, agentID string, values map[string]ExplicitLaunchValue) {
	t.Helper()
	d, err := Open()
	require.NoError(t, err)
	tx, err := d.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	require.NoError(t, recordExplicitLaunchValuesTx(tx, agentID, values))
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
	recordExplicit(t, agentID, morphExplicitValues(AgentMorphForm{AutoCompactWindow: morphStr("200k")}, later))

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
	recordExplicit(t, old, morphExplicitValues(AgentMorphForm{AutoCompactWindow: morphStr("200k")}, later))
	layers, err := AgentLaunchLayersFor(old)
	require.NoError(t, err)
	assert.Nil(t, layers, "a pre-change agent stays pre-change")
}
