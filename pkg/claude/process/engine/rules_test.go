package engine

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

func ruleProgram(next string) model.Node {
	return model.Node{Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerProgram, Run: "true"}, Next: model.Next{model.DefaultOutcome: next}}
}

// mergeTemplate: dev → ci → gate, where the gate is a rule: merge only when
// the param allows it and CI passed; CI failing sends the work back to dev.
func mergeTemplate() *model.Template {
	dev := model.Node{Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "build it"},
		Captures: []string{"risk"}, Next: model.Next{model.DefaultOutcome: "ci"}}
	ci := ruleProgram("gate")
	ci.MayFail = true
	return &model.Template{APIVersion: model.APIVersion, Kind: model.Kind, ID: "merge-rule", Start: "start",
		Params: map[string]model.Param{"merge": {Type: "string"}},
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "dev"}},
			"dev":   dev,
			"ci":    ci,
			"gate": {Type: model.NodeTypeDecision, Performer: &model.Performer{Kind: model.PerformerRule,
				Cases: []model.RuleCase{
					{When: `steps.ci.result == failed`, Then: "fix"},
					{When: `params.merge == yes and steps.ci.exitCode == 0 and outputs.risk != high`, Then: "merge"},
				}, Else: "hold"},
				Next:  model.Next{"fix": "dev", "merge": "merge", "hold": "held", "give-up": "gave-up"},
				Loops: map[string]model.LoopBound{"fix": {MaxIterations: 2, OnExhausted: "give-up"}}},
			"merge":   ruleProgram("end"),
			"end":     {Type: model.NodeTypeEnd},
			"held":    {Type: model.NodeTypeEnd, Result: "canceled"},
			"gave-up": {Type: model.NodeTypeEnd, Result: "failed"},
		}}
}

// ruleRun drives the merge template: dev reports risk, then CI exits with
// each code in turn, one per round, until the run stops planning.
func ruleRun(t *testing.T, merge string, exitCodes ...int) (Checkpoint, *Definition) {
	t.Helper()
	definition := mustPrepare(t, mergeTemplate(), map[string]string{"merge": merge})
	checkpoint, err := Initialize("run-rule", definition)
	require.NoError(t, err)
	for _, exitCode := range exitCodes {
		var command *Command
		checkpoint, command = advanceAndPlan(t, checkpoint, definition)
		require.NotNil(t, command)
		require.Equal(t, "dev", command.NodeID)
		report := observed(command, ProgramSucceeded, 0)
		report.Observation.Outputs = map[string]string{"risk": "low"}
		checkpoint, err = Apply(checkpoint, definition, report)
		require.NoError(t, err)
		checkpoint, command = advanceAndPlan(t, checkpoint, definition)
		require.NotNil(t, command)
		require.Equal(t, "ci", command.NodeID)
		outcome := ProgramSucceeded
		if exitCode != 0 {
			outcome = ProgramFailed
		}
		checkpoint, err = Apply(checkpoint, definition, observed(command, outcome, exitCode))
		require.NoError(t, err)
	}
	checkpoint, err = AdvanceUntilQuiescent(checkpoint, definition)
	require.NoError(t, err)
	return checkpoint, definition
}

func TestRuleMergesWhenTheParamAllowsAndCIPassed(t *testing.T) {
	checkpoint, definition := ruleRun(t, "yes", 0)
	require.Equal(t, NodeDone, checkpoint.Nodes["gate"])
	require.Equal(t, RuleChoice{Case: 1, Outcome: "merge", Count: 1, Read: map[string]string{
		"steps.ci.result": "succeeded", "params.merge": "yes", "steps.ci.exitCode": "0", "outputs.risk": "low",
	}}, checkpoint.Rules["gate"])
	when, ok := definition.RuleCase("gate", 1)
	require.True(t, ok)
	require.Contains(t, when, "params.merge == yes")
	// Nobody was asked.
	require.Empty(t, checkpoint.AwaitingDecisions)
	_, command := advanceAndPlan(t, checkpoint, definition)
	require.NotNil(t, command)
	require.Equal(t, "merge", command.NodeID)
}

func TestRuleTakesElseWhenNoCaseHolds(t *testing.T) {
	checkpoint, _ := ruleRun(t, "no", 0)
	require.Equal(t, -1, checkpoint.Rules["gate"].Case)
	require.Equal(t, "hold", checkpoint.Rules["gate"].Outcome)
	require.Equal(t, RunCanceled, checkpoint.Status)
}

func TestMayFailStepGoesOnAndTheRuleSendsTheWorkBack(t *testing.T) {
	checkpoint, definition := ruleRun(t, "yes", 2)
	// CI failed, yet the run goes on: the rule sent the work back to dev.
	require.Equal(t, RunRunning, checkpoint.Status)
	require.Equal(t, RuleChoice{Case: 0, Outcome: "fix", Count: 1, Read: map[string]string{"steps.ci.result": "failed"}}, checkpoint.Rules["gate"])
	require.Equal(t, 2, *checkpoint.Steps["ci"].ExitCode)
	_, command := advanceAndPlan(t, checkpoint, definition)
	require.NotNil(t, command)
	require.Equal(t, "dev", command.NodeID)

	// The second round passes and merges.
	checkpoint, _ = ruleRun(t, "yes", 2, 0)
	require.Equal(t, "merge", checkpoint.Rules["gate"].Outcome)
	require.Equal(t, 2, checkpoint.Rules["gate"].Count)
}

func TestRuleLoopGivesUpWhenItsRoundsAreUsed(t *testing.T) {
	checkpoint, _ := ruleRun(t, "yes", 1, 1)
	// The rule chose fix again, but the loop's two rounds are used up, so the
	// decision takes its onExhausted outcome instead.
	require.Equal(t, "fix", checkpoint.Rules["gate"].Outcome)
	require.Equal(t, NodeDone, checkpoint.Nodes["gave-up"])
	require.Equal(t, RunFailed, checkpoint.Status)
}

func TestARuleCheckpointRoundTrips(t *testing.T) {
	checkpoint, definition := ruleRun(t, "yes", 3)
	encoded, err := json.Marshal(checkpoint)
	require.NoError(t, err)
	decoded, err := DecodeCheckpoint(encoded, definition)
	require.NoError(t, err)
	require.Equal(t, checkpoint.Rules, decoded.Rules)
	require.Equal(t, checkpoint.Steps, decoded.Steps)
}
