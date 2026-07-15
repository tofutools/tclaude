package pathv1

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

func TestObserveExclusiveAttemptRejectsDecisionTypoBeforeTransition(t *testing.T) {
	source := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: reject-decision-typo
start: choose
nodes:
  choose:
    type: decision
    performer: {kind: human, ask: Choose}
    next: {ship: shipped, hold: held}
  shipped: {type: end}
  held: {type: end}
`)
	genesisBytes := initializedExclusiveCheckpoint(t, source)
	input, err := VerifyExclusiveInput(t.Context(), genesisBytes, source)
	if err != nil {
		t.Fatal(err)
	}
	genesis, err := DecodeCheckpointV7(genesisBytes)
	if err != nil {
		t.Fatal(err)
	}
	aggregate, err := CurrentAggregateCheckpoint(genesis)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanExclusiveAttempt(t.Context(), input, aggregate.Authority.Genesis.OutputPathID, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := ClaimExclusiveAttempt(t.Context(), input, plan)
	if err != nil {
		t.Fatal(err)
	}
	_, claimedBytes, claimed, err := ValidateExecutionTransitionForAppend(t.Context(), genesisBytes, source, claim)
	if err != nil {
		t.Fatal(err)
	}
	beforeBytes := bytes.Clone(claimedBytes)
	beforeBinding := CurrentCheckpointBinding(claimed)
	claimedInput, err := VerifyExclusiveInput(t.Context(), claimedBytes, source)
	if err != nil {
		t.Fatal(err)
	}
	recovered, found, err := RecoverExclusiveAttempt(t.Context(), claimedInput)
	if err != nil || !found {
		t.Fatalf("recover claim: found=%v err=%v", found, err)
	}
	transition, err := ObserveExclusiveAttempt(t.Context(), claimedInput, recovered, ExclusiveObservation{Outcome: "shpi", Actor: "human:operator"}, false)
	if !errors.Is(err, ErrExclusiveUnsupported) || transition != nil {
		t.Fatalf("typo observation transition=%#v error=%v", transition, err)
	}
	after, err := DecodeCheckpointV7(claimedBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(claimedBytes, beforeBytes) || CurrentCheckpointBinding(after) != beforeBinding {
		t.Fatal("invalid verdict changed checkpoint bytes or binding")
	}
}

func TestAdvanceExclusiveRouteCannotSynthesizeTaskObservation(t *testing.T) {
	source := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: reject-synthetic-route
start: work
nodes:
  work:
    type: task
    performer: {kind: agent, prompt: work}
    next: {pass: done}
  done: {type: end}
`)
	checkpointBytes := initializedExclusiveCheckpoint(t, source)
	input, err := VerifyExclusiveInput(t.Context(), checkpointBytes, source)
	if err != nil {
		t.Fatal(err)
	}
	transition, err := AdvanceExclusiveRoute(t.Context(), input)
	if !errors.Is(err, ErrExclusiveNotRoutable) || transition != nil {
		t.Fatalf("fresh task synthesized route transition=%#v error=%v", transition, err)
	}
	checkpoint, err := DecodeCheckpointV7(checkpointBytes)
	if err != nil {
		t.Fatal(err)
	}
	aggregate, err := CurrentAggregateCheckpoint(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range aggregate.Commands {
		if command.Identity.Kind == CommandPerformAttempt || command.Identity.Kind == CommandSettleAttempt {
			t.Fatalf("fresh task gained performer authority: %#v", command)
		}
	}
}

func TestExclusiveAttemptPlanPerformerIsDeeplyDetached(t *testing.T) {
	source := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: detached-performer
start: work
nodes:
  work:
    type: task
    performer:
      kind: human
      ask: Ship it?
      choices: [ship, hold]
      choiceOutcomes: {ship: pass, hold: fail}
      contact: {cadence: 5m, budget: 2, escalationTarget: release-lead}
    next: {pass: done, fail: failed}
  done: {type: end}
  failed: {type: end, result: failed}
`)
	checkpointBytes := initializedExclusiveCheckpoint(t, source)
	input, err := VerifyExclusiveInput(t.Context(), checkpointBytes, source)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := DecodeCheckpointV7(checkpointBytes)
	if err != nil {
		t.Fatal(err)
	}
	aggregate, err := CurrentAggregateCheckpoint(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanExclusiveAttempt(t.Context(), input, aggregate.Authority.Genesis.OutputPathID, 1, map[string]string{"release": "stable"})
	if err != nil {
		t.Fatal(err)
	}

	performer := plan.Performer()
	performer.Choices[0] = "mutated"
	performer.ChoiceOutcomes["ship"] = "fail"
	performer.Contact.Cadence = "mutated"
	params := plan.Params()
	params["release"] = "mutated"

	want := &model.Performer{
		Kind: model.PerformerHuman, Ask: "Ship it?", Choices: []string{"ship", "hold"},
		ChoiceOutcomes: map[string]string{"ship": "pass", "hold": "fail"},
		Contact:        &model.ContactSchedule{Cadence: "5m", Budget: 2, EscalationTarget: "release-lead"},
	}
	if got := plan.Performer(); !reflect.DeepEqual(got, want) {
		t.Fatalf("sealed performer was mutated through accessor: got %#v want %#v", got, want)
	}
	if got := plan.Params()["release"]; got != "stable" {
		t.Fatalf("sealed params were mutated through accessor: got %q", got)
	}
}

func TestCloneExclusivePerformerCopiesEveryNestedField(t *testing.T) {
	original := &model.Performer{
		Choices: []string{"one"}, Args: []string{"arg"}, ChoiceOutcomes: map[string]string{"one": "pass"},
		Contact: &model.ContactSchedule{Cadence: "1m", Budget: 1, EscalationTarget: "owner"},
	}
	clone := cloneExclusivePerformer(original)
	original.Choices[0] = "changed"
	original.Args[0] = "changed"
	original.ChoiceOutcomes["one"] = "fail"
	original.Contact.Cadence = "changed"
	if clone.Choices[0] != "one" || clone.Args[0] != "arg" || clone.ChoiceOutcomes["one"] != "pass" || clone.Contact.Cadence != "1m" {
		t.Fatalf("nested performer fields alias source: %#v", clone)
	}
}

func TestPlanExclusiveAttemptRejectsNonAdapterNodes(t *testing.T) {
	tests := map[string][]byte{
		"instantaneous start": []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: reject-start
start: begin
nodes:
  begin: {type: start, next: {pass: done}}
  done: {type: end}
`),
		"wait": []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: reject-wait
start: pause
nodes:
  pause: {type: wait, wait: {duration: 1m}, next: {pass: done}}
  done: {type: end}
`),
		"end": []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: reject-end
start: done
nodes:
  done: {type: end}
`),
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			checkpointBytes := initializedExclusiveCheckpoint(t, source)
			input, err := VerifyExclusiveInput(t.Context(), checkpointBytes, source)
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, err := DecodeCheckpointV7(checkpointBytes)
			if err != nil {
				t.Fatal(err)
			}
			aggregate, err := CurrentAggregateCheckpoint(checkpoint)
			if err != nil {
				t.Fatal(err)
			}
			_, err = PlanExclusiveAttempt(t.Context(), input, aggregate.Authority.Genesis.OutputPathID, 1, nil)
			if !errors.Is(err, ErrExclusiveUnsupported) {
				t.Fatalf("PlanExclusiveAttempt error = %v, want unsupported", err)
			}
		})
	}
}
