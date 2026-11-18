package agentd_test

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// spawnLifetimeRun starts a run on group crew whose first step always spawns
// a new agent with lifetime (empty: the default). With then, a second step
// follows that nobody can take, so the run stays running after the first.
// It returns the spawned agent and its conversation.
func spawnLifetimeRun(t *testing.T, runID, lifetime string, then bool) (string, string, func() *db.Agent) {
	t.Helper()
	spawned, conv, agent, _ := spawnLifetimeRunIn(t, runID, lifetime, then)
	return spawned, conv, agent
}

// spawnLifetimeFixture is the flow a spawnLifetimeRun ran in, for a test
// that starts more runs on the same group.
type spawnLifetimeFixture struct {
	f     *testharness.Flow
	root  string
	group int64
}

func spawnLifetimeRunIn(t *testing.T, runID, lifetime string, then bool) (string, string, func() *db.Agent, spawnLifetimeFixture) {
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
	return spawned, conv, agent, spawnLifetimeFixture{f: f, root: root, group: g.ID}
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
	var busy atomic.Bool
	busy.Store(true)
	agentd.SetProcessSpawnedSettledForTest(t, func(string) (bool, error) { return !busy.Load(), nil })
	_, _, agent := spawnLifetimeRun(t, "run_life_busy", "", false)
	agentd.ReapProcessSpawnedPerformersForTest()
	assert.True(t, agent().Active(), "mid-turn: not retired from under itself")
	busy.Store(false)
	agentd.ReapProcessSpawnedPerformersForTest()
	assert.False(t, agent().Active())
}

// Scenario: an agent spawned before lifetimes existed has no lifetime on
// record. It was promised to stay, so the run's end does not retire it.
func TestProcessSpawnedLifetime_ASpawnFromBeforeLifetimesStays(t *testing.T) {
	var busy atomic.Bool
	busy.Store(true)
	agentd.SetProcessSpawnedSettledForTest(t, func(string) (bool, error) { return !busy.Load(), nil })
	spawned, _, agent := spawnLifetimeRun(t, "run_life_legacy", "", false)
	// The run's end reaps on its own goroutine: let it finish (busy, so it
	// leaves the agent) before the row is changed under it.
	agentd.ReapProcessSpawnedPerformersForTest()
	rows, err := db.ListProcessAgentDispatchesForAgent(spawned, false, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, model.LifetimeRun, rows[0].SpawnLifetime, "the default is recorded at the spawn")
	require.NoError(t, db.SetProcessAgentDispatchSpawnLifetime(rows[0].RunID, rows[0].CommandID, ""))
	busy.Store(false)
	agentd.ReapProcessSpawnedPerformersForTest()
	assert.True(t, agent().Active(), "no lifetime on record: left as it always was")
}

// Scenario: a "step" agent another run has since given a step to stays
// while that step is open, and is retired once it is done.
func TestProcessSpawnedLifetime_AnAgentAnotherRunUsesStays(t *testing.T) {
	var busy atomic.Bool
	busy.Store(true)
	agentd.SetProcessSpawnedSettledForTest(t, func(string) (bool, error) { return !busy.Load(), nil })
	spawned, conv, agent, fx := spawnLifetimeRunIn(t, "run_life_shared", model.LifetimeStep, true)
	f := fx.f
	other := performerTemplate("run_life_other", model.Performer{Kind: model.PerformerAgent, Prompt: "More.", Assignee: conv}, nil)
	view := startPerformerRun(t, f, fx.root, "run_life_other", other, agentd.ProcessRunScopeForTest{Kind: "group", GroupID: fx.group})
	require.Equal(t, spawned, view.Commands[0].Performer.AgentID, "%+v", view.Commands[0].Performer)
	busy.Store(false)
	agentd.ReapProcessSpawnedPerformersForTest()
	assert.True(t, agent().Active(), "on a step of another run")
	ok := reportAs(t, f, conv, map[string]any{"outcome": "succeeded"})
	require.Equal(t, http.StatusOK, ok.Code, ok.Body.String())
	agentd.ReapProcessSpawnedPerformersForTest()
	assert.False(t, agent().Active(), "that step is done too")
}
