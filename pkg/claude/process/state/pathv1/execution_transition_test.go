package pathv1

import (
	"errors"
	"reflect"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

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
	plan, err := PlanExclusiveAttempt(t.Context(), input, aggregate.Authority.Genesis.OutputPathID, 1)
	if err != nil {
		t.Fatal(err)
	}

	performer := plan.Performer()
	performer.Choices[0] = "mutated"
	performer.ChoiceOutcomes["ship"] = "fail"
	performer.Contact.Cadence = "mutated"

	want := &model.Performer{
		Kind: model.PerformerHuman, Ask: "Ship it?", Choices: []string{"ship", "hold"},
		ChoiceOutcomes: map[string]string{"ship": "pass", "hold": "fail"},
		Contact:        &model.ContactSchedule{Cadence: "5m", Budget: 2, EscalationTarget: "release-lead"},
	}
	if got := plan.Performer(); !reflect.DeepEqual(got, want) {
		t.Fatalf("sealed performer was mutated through accessor: got %#v want %#v", got, want)
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
			_, err = PlanExclusiveAttempt(t.Context(), input, aggregate.Authority.Genesis.OutputPathID, 1)
			if !errors.Is(err, ErrExclusiveUnsupported) {
				t.Fatalf("PlanExclusiveAttempt error = %v, want unsupported", err)
			}
		})
	}
}
