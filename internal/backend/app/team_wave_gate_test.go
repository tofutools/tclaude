package app

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func TestTeamWaveGateRequiresWorkedThenSimultaneouslySettled(t *testing.T) {
	now := time.Date(2026, 9, 8, 20, 0, 0, 0, time.UTC)
	deadline := now.Add(defaultTeamWaveMaxWait)
	ids := []model.ExecutionID{"execution_a", "execution_b"}
	activated := map[model.ExecutionID]bool{}
	observations := map[model.ExecutionID]ports.Observation{
		ids[0]: {Workload: ports.WorkloadRunning, AgentActivity: ports.AgentActivityIdle},
		ids[1]: {Workload: ports.WorkloadRunning, AgentActivity: ports.AgentActivityIdle},
	}
	released, newly := teamWaveGate(now, deadline, ids, activated, observations)
	require.False(t, released, "initial idle must not skip the first turn")
	require.Empty(t, newly)
	observations[ids[0]] = ports.Observation{Workload: ports.WorkloadRunning, AgentActivity: ports.AgentActivityActive}
	released, newly = teamWaveGate(now, deadline, ids, activated, observations)
	require.False(t, released)
	require.Equal(t, []model.ExecutionID{ids[0]}, newly)
	require.Empty(t, activated, "sampling must not silently mutate durable progress")
	activated[ids[0]] = true
	observations[ids[0]] = ports.Observation{Workload: ports.WorkloadRunning, AgentActivity: ports.AgentActivityIdle}
	observations[ids[1]] = ports.Observation{Workload: ports.WorkloadRunning, AgentActivity: ports.AgentActivityActive}
	released, newly = teamWaveGate(now, deadline, ids, activated, observations)
	require.False(t, released)
	require.Equal(t, []model.ExecutionID{ids[1]}, newly)
	activated[ids[1]] = true
	observations[ids[0]] = ports.Observation{Workload: ports.WorkloadRunning, AgentActivity: ports.AgentActivityActive}
	observations[ids[1]] = ports.Observation{Workload: ports.WorkloadRunning, AgentActivity: ports.AgentActivityIdle}
	released, _ = teamWaveGate(now, deadline, ids, activated, observations)
	require.False(t, released, "an earlier idle sample is not permanent settlement")
	observations[ids[0]] = ports.Observation{Workload: ports.WorkloadRunning, AgentActivity: ports.AgentActivityAwaitingInput}
	released, _ = teamWaveGate(now, deadline, ids, activated, observations)
	require.True(t, released)
}

func TestTeamWaveGateDeadUnknownAndMaximumWait(t *testing.T) {
	now := time.Now()
	id := model.ExecutionID("execution_gate")
	ids := []model.ExecutionID{id}
	for _, observation := range []ports.Observation{{}, {Workload: ports.WorkloadStarting}, {Workload: ports.WorkloadRunning, AgentActivity: ports.AgentActivityUnknown}} {
		released, _ := teamWaveGate(now, now.Add(time.Minute), ids, map[model.ExecutionID]bool{id: true}, map[model.ExecutionID]ports.Observation{id: observation})
		require.False(t, released)
	}
	released, _ := teamWaveGate(now, now.Add(time.Minute), ids, nil, nil)
	require.False(t, released, "a missing observation is not a dead member")
	released, _ = teamWaveGate(now, now, ids, nil, nil)
	require.True(t, released, "the deadline backstops unavailable activity evidence")
	released, _ = teamWaveGate(now, time.Time{}, ids, nil, map[model.ExecutionID]ports.Observation{id: {Workload: ports.WorkloadExited}})
	require.True(t, released, "a dead member cannot hold the next wave")
	released, _ = teamWaveGate(now, time.Time{}, nil, nil, nil)
	require.True(t, released)
}

func TestTeamWaveBudgetAndFinalWave(t *testing.T) {
	team := model.TeamDefinition{Waves: []model.TeamWave{{ID: "first", MemberKeys: []string{"a"}, WaitForIdle: true}, {ID: "last", MemberKeys: []string{"b"}, DependsOn: []string{"first"}, WaitForIdle: true}}}
	budget, err := teamWaveRunBudget(team)
	require.NoError(t, err)
	require.Greater(t, budget, defaultTeamWaveMaxWait)
	graph := teamDeploymentGraph(team, model.TeamDeployment{Members: map[string]model.AgentID{"a": "agent_a", "b": "agent_b"}})
	var gates []model.WorkNodeID
	for _, node := range graph.Nodes {
		if node.Kind == model.WorkNodeWait {
			gates = append(gates, node.ID)
		}
	}
	require.Equal(t, []model.WorkNodeID{teamWaveGateID("first")}, gates, "the final wave does not delay deployment completion")
	team.Waves[0].MaxWaitSeconds = -1
	_, err = teamWaveRunBudget(team)
	require.ErrorIs(t, err, ErrInvalid)
	team.Waves[0].MaxWaitSeconds = 9223372036
	_, err = teamWaveRunBudget(team)
	require.ErrorIs(t, err, ErrInvalid, "aggregate duration must not wrap")
}
