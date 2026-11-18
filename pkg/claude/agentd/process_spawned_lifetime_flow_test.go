package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// spawnLifetimeRun starts a run on group crew whose first step always spawns
// a new agent with lifetime (empty: the default). With then, a second step
// follows that nobody can take, so the run stays running after the first.
// It returns the spawned agent and its conversation.
func spawnLifetimeRun(t *testing.T, runID, lifetime string, then bool) (string, string, func() *db.Agent) {
	t.Helper()
	f, root := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	_, err := db.SetAgentGroupDefaultCwd("crew", t.TempDir())
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "dev", Harness: "claude"})
	require.NoError(t, err)
	performer := model.Performer{Kind: model.PerformerAgent, Prompt: "Implement it.",
		Fallback: &model.PerformerFallback{Action: model.FallbackSpawn, Profile: "dev", Lifetime: lifetime}}
	tmpl := performerTemplate(runID, performer, nil)
	if then {
		node := tmpl.Nodes[performerNode]
		node.Next = model.Next{"next": "later"}
		tmpl.Nodes[performerNode] = node
		tmpl.Nodes["later"] = model.Node{Type: model.NodeTypeTask, Next: model.Next{"next": "end"},
			Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "Later.", Role: "nobody-has-this",
				Fallback: &model.PerformerFallback{Action: model.FallbackWait}}}
	}
	view := startPerformerRun(t, f, root, runID, tmpl, agentd.ProcessRunScopeForTest{Kind: "group", GroupID: g.ID})
	require.Equal(t, "spawned", view.Commands[0].Performer.Resolution, "%+v", view.Commands[0].Performer)
	spawned := view.Commands[0].Performer.AgentID
	conv, err := db.CurrentConvForAgent(spawned)
	require.NoError(t, err)
	ok := reportAs(t, f, conv, map[string]any{"outcome": "succeeded"})
	require.Equal(t, http.StatusOK, ok.Code, ok.Body.String())
	// Its turn is over: the pane is idle again.
	setConvStatus(t, conv, "idle")
	if !then {
		require.Equal(t, engine.RunCompleted, showPerformerRun(t, f, runID).Status)
	}
	agent := func() *db.Agent {
		a, err := db.GetAgent(spawned)
		require.NoError(t, err)
		require.NotNil(t, a)
		return a
	}
	return spawned, conv, agent
}

// Scenario: a step's new agent with no lifetime set is kept for the run and
// retired when the run ends.
func TestProcessSpawnedLifetime_DefaultRetiresWhenTheRunEnds(t *testing.T) {
	_, _, agent := spawnLifetimeRun(t, "run_life_default", "", false)
	agentd.ReapProcessSpawnedPerformersForTest()
	assert.False(t, agent().Active(), "the run ended: its spawned performer is retired")
	assert.Contains(t, agent().RetireReason, "lifetime run")
}

// Scenario: "run" keeps the agent while the run goes on to later steps.
func TestProcessSpawnedLifetime_RunKeepsItWhileTheRunGoesOn(t *testing.T) {
	_, _, agent := spawnLifetimeRun(t, "run_life_run", model.LifetimeRun, true)
	agentd.ReapProcessSpawnedPerformersForTest()
	assert.True(t, agent().Active(), "the run is still running: a later step may want it")
}

// Scenario: "step" retires the agent as soon as its step is done, while the
// run goes on.
func TestProcessSpawnedLifetime_StepRetiresItOnceItsStepIsDone(t *testing.T) {
	_, _, agent := spawnLifetimeRun(t, "run_life_step", model.LifetimeStep, true)
	agentd.ReapProcessSpawnedPerformersForTest()
	assert.False(t, agent().Active(), "its step is done: retired while the run goes on")
}

// Scenario: "keep" is never retired by the run, even once it has ended.
func TestProcessSpawnedLifetime_KeepIsNeverRetired(t *testing.T) {
	_, _, agent := spawnLifetimeRun(t, "run_life_keep", model.LifetimeKeep, false)
	agentd.ReapProcessSpawnedPerformersForTest()
	assert.True(t, agent().Active())
}

// Scenario: an agent still busy when its lifetime is up is left alone, and
// retired by a later sweep once it has settled.
func TestProcessSpawnedLifetime_ABusyAgentIsRetiredOnceSettled(t *testing.T) {
	busy := true
	agentd.SetProcessSpawnedSettledForTest(t, func(string) (bool, error) { return !busy, nil })
	_, _, agent := spawnLifetimeRun(t, "run_life_busy", "", false)
	agentd.ReapProcessSpawnedPerformersForTest()
	assert.True(t, agent().Active(), "mid-turn: not retired from under itself")
	busy = false
	agentd.ReapProcessSpawnedPerformersForTest()
	assert.False(t, agent().Active())
}
