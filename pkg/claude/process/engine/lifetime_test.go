package engine

import (
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// A spawned agent's lifetime: what the step authors, else the default, which
// for a gate's vote is the decider default.
func TestSpawnLifetimeDefaults(t *testing.T) {
	tmpl := agreementTemplate()
	decide := tmpl.Nodes["decide"]
	spawn := func(lifetime string) model.Performer {
		return model.Performer{Kind: model.PerformerAgent, Prompt: "Ship it?", Anyone: true,
			Fallback: &model.PerformerFallback{Action: model.FallbackSpawn, Lifetime: lifetime}}
	}
	decide.Deciders = []model.Performer{spawn(""), spawn(model.LifetimeRun)}
	tmpl.Nodes["decide"] = decide
	implement := tmpl.Nodes["implement"]
	implement.Performer = &model.Performer{Kind: model.PerformerAgent, Prompt: "Implement it.", Anyone: true,
		Fallback: &model.PerformerFallback{Action: model.FallbackSpawn}}
	tmpl.Nodes["implement"] = implement
	definition := mustPrepare(t, tmpl, nil)
	for nodeID, want := range map[string]string{
		"implement":                   model.DefaultLifetime(),
		model.GateVoteID("decide", 1): model.DefaultDeciderLifetime(),
		model.GateVoteID("decide", 2): model.LifetimeRun,
	} {
		if got, ok := definition.SpawnLifetime(nodeID); !ok || got != want {
			t.Errorf("SpawnLifetime(%s) = %q, %v; want %q", nodeID, got, ok, want)
		}
	}
	if _, ok := definition.SpawnLifetime("merge"); ok {
		t.Error("a program step has no spawned agent")
	}
	if model.DefaultLifetime() != model.LifetimeRun || model.DefaultDeciderLifetime() != model.LifetimeStep {
		t.Error("the agreed defaults: run for a step, step for a gate's vote")
	}
}

// A step in a loop is not finished while its loop can still send it back:
// only once the loop's decision has settled on a way out.
func TestStepFinishedWaitsForItsLoop(t *testing.T) {
	definition := mustPrepare(t, reviewLoopTemplate(model.LoopBound{MaxIterations: 3, OnExhausted: "escalate"}), nil)
	checkpoint, err := Initialize("run-life", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, _ = succeedUntilQuiet(t, checkpoint, definition)
	if checkpoint.Nodes["implement"] != NodeDone || definition.StepFinished(checkpoint, "implement") {
		t.Fatalf("implement = %q: done, but the review may send it back", checkpoint.Nodes["implement"])
	}
	checkpoint, err = Apply(checkpoint, definition, decidedAt("decide", "changes", 0))
	if err != nil {
		t.Fatal(err)
	}
	if definition.StepFinished(checkpoint, "implement") {
		t.Fatal("sent back for another round: not finished")
	}
	checkpoint, _ = succeedUntilQuiet(t, checkpoint, definition)
	checkpoint, err = Apply(checkpoint, definition, decidedAt("decide", "approved", 1))
	if err != nil {
		t.Fatal(err)
	}
	if !definition.StepFinished(checkpoint, "implement") {
		t.Fatalf("approved: the loop has exited, implement = %q, decide = %q", checkpoint.Nodes["implement"], checkpoint.Nodes["decide"])
	}
	if definition.StepFinished(checkpoint, "merge") {
		t.Fatal("merge has not run yet")
	}
}
