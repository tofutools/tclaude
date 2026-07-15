package pathv1

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

func TestExclusiveCanceledObservationUsesFailureEdge(t *testing.T) {
	source := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: canceled-is-failure
start: work
nodes:
  work:
    type: task
    performer: {kind: agent, prompt: work}
    next: {pass: done, fail: recover}
  recover:
    type: task
    performer: {kind: agent, prompt: recover}
    next: {pass: done}
  done: {type: end}
`)
	observedBytes := observedExclusiveAttemptForTest(t, initializedExclusiveCheckpoint(t, source), source, ExclusiveObservation{Outcome: "cancelled", Actor: "human:operator"})
	input, err := VerifyExclusiveInput(t.Context(), observedBytes, source)
	if err != nil {
		t.Fatal(err)
	}
	pending, found, err := PendingExclusiveObservation(t.Context(), input)
	if err != nil || !found || pending.Outcome != "cancelled" {
		t.Fatalf("pending canceled observation = %#v, found=%v err=%v", pending, found, err)
	}
	route, err := PlanExclusiveRoute(t.Context(), input, pending)
	if err != nil {
		t.Fatal(err)
	}
	var payload mutationPayload[RoutePathsPlan]
	if err := decodeExactPayload(route.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Plan.ProducedPathIDs) == 0 {
		t.Fatalf("produced path IDs = %#v", payload.Plan.ProducedPathIDs)
	}
	selectedRecovery := false
	for _, producedPathID := range payload.Plan.ProducedPathIDs {
		mutation, ok := findMutation(payload.Plan.Batch, MutationPath, producedPathID)
		if !ok {
			continue
		}
		var selected PathRecord
		if err := decodeExactPayload(mutation.After, &selected); err != nil {
			t.Fatal(err)
		}
		if selected.Edge != nil && selected.Edge.ToNodeID == "recover" {
			selectedRecovery = true
			break
		}
	}
	if !selectedRecovery {
		t.Fatalf("canceled task produced paths %#v without selecting the recovery failure edge", payload.Plan.ProducedPathIDs)
	}
}

func TestExclusiveOutcomeCanonicalizationSurvivesSettlementReplay(t *testing.T) {
	tests := []struct {
		name        string
		source      []byte
		outcome     string
		wantOutcome string
	}{
		{
			name: "task outcome normalized",
			source: []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: uppercase-task-outcome
start: work
nodes:
  work:
    type: task
    performer: {kind: agent, prompt: work}
    next: {pass: done}
  done: {type: end}
`),
			outcome: "PASS", wantOutcome: "pass",
		},
		{
			name: "decision verdict preserves exact case",
			source: []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: uppercase-decision-outcome
start: choose
nodes:
  choose:
    type: decision
    performer: {kind: human, ask: Choose}
    next: {SHIP: shipped, HOLD: held}
  shipped: {type: end}
  held: {type: end}
`),
			outcome: "SHIP", wantOutcome: "SHIP",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			observedBytes := observedExclusiveAttemptForTest(t, initializedExclusiveCheckpoint(t, test.source), test.source, ExclusiveObservation{Outcome: test.outcome, Actor: "human:operator"})
			input, err := VerifyExclusiveInput(t.Context(), observedBytes, test.source)
			if err != nil {
				t.Fatal(err)
			}
			pending, found, err := PendingExclusiveObservation(t.Context(), input)
			if err != nil || !found || pending.Outcome != test.wantOutcome {
				t.Fatalf("pending observation = %#v, found=%v err=%v", pending, found, err)
			}
			route, err := AdvanceExclusiveRoute(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := ValidateExecutionTransitionForAppend(t.Context(), observedBytes, test.source, route); err != nil {
				t.Fatalf("canonical settlement route replay failed: %v", err)
			}
		})
	}
}

func TestExclusiveSettlementPreservesBlockResolutionAuthority(t *testing.T) {
	source := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: preserve-block-resolution
start: work
nodes:
  work:
    type: task
    performer: {kind: agent, prompt: work}
    next: {pass: done, fail: failed}
  done: {type: end}
  failed: {type: end, result: failed}
`)
	resolvedBytes, digest := resolvedExclusiveCheckpoint(t, initializedExclusiveCheckpoint(t, source), "skip", 1)
	observedBytes := observedExclusiveAttemptForTest(t, resolvedBytes, source, ExclusiveObservation{
		Outcome: "pass", Actor: "human:operator", ResolutionDigest: digest,
	})
	input, err := VerifyExclusiveInput(t.Context(), observedBytes, source)
	if err != nil {
		t.Fatal(err)
	}
	pending, found, err := PendingExclusiveObservation(t.Context(), input)
	if !errors.Is(err, ErrExclusiveUnsupported) || found || pending != (ExclusiveObservation{}) || !strings.Contains(err.Error(), string(ExclusiveResolvedSkip)) {
		t.Fatalf("durable resolution authority was not reconstructed: pending=%#v found=%v err=%v", pending, found, err)
	}

	checkpoint, err := DecodeCheckpointV7(observedBytes)
	if err != nil {
		t.Fatal(err)
	}
	aggregate, err := CurrentAggregateCheckpoint(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range aggregate.Commands {
		if command.Identity.Kind != CommandSettleAttempt {
			continue
		}
		var payload settleAttemptObservationPayload
		if err := decodeExactPayload(command.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.ResolutionDigest != digest {
			t.Fatalf("settlement resolution digest = %q, want %q", payload.ResolutionDigest, digest)
		}
		return
	}
	t.Fatal("settlement command not found")
}

func observedExclusiveAttemptForTest(t *testing.T, checkpointBytes, source []byte, observation ExclusiveObservation) []byte {
	t.Helper()
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
	plan, err := PlanExclusiveAttempt(t.Context(), input, aggregate.Authority.Genesis.OutputPathID, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := ClaimExclusiveAttempt(t.Context(), input, plan)
	if err != nil {
		t.Fatal(err)
	}
	_, claimedBytes, _, err := ValidateExecutionTransitionForAppend(t.Context(), checkpointBytes, source, claim)
	if err != nil {
		t.Fatal(err)
	}
	claimedInput, err := VerifyExclusiveInput(t.Context(), claimedBytes, source)
	if err != nil {
		t.Fatal(err)
	}
	recovered, found, err := RecoverExclusiveAttempt(t.Context(), claimedInput)
	if err != nil || !found {
		t.Fatalf("recover claim: found=%v err=%v", found, err)
	}
	observe, err := ObserveExclusiveAttempt(t.Context(), claimedInput, recovered, observation, false)
	if err != nil {
		t.Fatal(err)
	}
	_, observedBytes, _, err := ValidateExecutionTransitionForAppend(t.Context(), claimedBytes, source, observe)
	if err != nil {
		t.Fatal(err)
	}
	return observedBytes
}

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
