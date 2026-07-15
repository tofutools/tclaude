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

func TestDecisionCaseCollisionPreservesExactSettlementAndRouteAuthority(t *testing.T) {
	source := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: case-distinct-decision
start: choose
nodes:
  choose:
    type: decision
    performer: {kind: human, ask: Choose}
    next: {Go: shipped, go: held}
  shipped: {type: end}
  held: {type: end}
`)
	type authority struct {
		observed []byte
		pending  ExclusiveObservation
		settle   CommandRecord
		route    CommandRecord
		edge     EdgeKey
	}
	run := func(t *testing.T, verdict string) authority {
		t.Helper()
		observed := observedExclusiveAttemptForTest(t, initializedExclusiveCheckpoint(t, source), source, ExclusiveObservation{
			Outcome: " " + verdict + " ", Actor: "human:operator",
		})
		input, err := VerifyExclusiveInput(t.Context(), observed, source)
		if err != nil {
			t.Fatal(err)
		}
		pending, found, err := PendingExclusiveObservation(t.Context(), input)
		if err != nil || !found || pending.Outcome != verdict {
			t.Fatalf("pending verdict = %#v, found=%v err=%v", pending, found, err)
		}
		planned, err := PlanExclusiveRoute(t.Context(), input, pending)
		if err != nil {
			t.Fatal(err)
		}
		observedAggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
		if err != nil {
			t.Fatal(err)
		}
		var plannedPayload mutationPayload[RoutePathsPlan]
		if err := decodeExactPayload(planned.Payload, &plannedPayload); err != nil {
			t.Fatal(err)
		}
		barePlan := plannedPayload.Plan
		barePlan.ResultCode = verdict
		barePayload, err := EncodeRoutePathsPayload(MutationReplayView{Aggregate: observedAggregate.View(), Checkpoint: input.binding}, barePlan)
		if err != nil {
			t.Fatal(err)
		}
		bareIdentity := planned.Identity
		bareIdentity.PlanDigest = payloadDigest(barePayload)
		bareIdentity.ResultCode = verdict
		bareRoute := commandWithPayload(t, bareIdentity, CommandObserved, barePayload)
		if err := ValidateRoutePathsCommand(MutationReplayView{Aggregate: observedAggregate.View(), Checkpoint: input.binding}, bareRoute); !errors.Is(err, ErrMutationInvalid) {
			t.Fatalf("exclusive route with bare verdict %q error = %v", verdict, err)
		}
		transition, err := AdvanceExclusiveRoute(t.Context(), input)
		if err != nil {
			t.Fatal(err)
		}
		_, routedBytes, checkpoint, err := ValidateExecutionTransitionForAppend(t.Context(), observed, source, transition)
		if err != nil {
			t.Fatal(err)
		}
		roundTrip, err := EncodeCheckpointV7(checkpoint)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(routedBytes, roundTrip) {
			t.Fatal("routed checkpoint audit bytes did not round trip canonically")
		}
		decoded, err := DecodeCheckpointV7(roundTrip)
		if err != nil {
			t.Fatal(err)
		}
		aggregate, err := CurrentAggregateCheckpoint(decoded)
		if err != nil {
			t.Fatal(err)
		}
		var got authority
		got.observed, got.pending = observed, pending
		for _, command := range aggregate.Commands {
			switch {
			case command.Identity.Kind == CommandSettleAttempt && command.Identity.ResultCode == verdict:
				got.settle = command
				var payload settleAttemptObservationPayload
				if err := decodeExactPayload(command.Payload, &payload); err != nil {
					t.Fatal(err)
				}
				if payload.ResultCode != verdict {
					t.Fatalf("settlement audit payload verdict = %q, want %q", payload.ResultCode, verdict)
				}
			case command.Identity.Kind == CommandRoutePaths && command.Identity.InputDigest == planned.Identity.InputDigest:
				got.route = command
			}
		}
		if got.settle.ID == "" || got.route.ID == "" || !exactExclusiveCommand(got.route, planned) {
			t.Fatalf("settlement/route parity failed: settle=%#v route=%#v planned=%#v", got.settle, got.route, planned)
		}
		var routePayload mutationPayload[RoutePathsPlan]
		if err := decodeExactPayload(got.route.Payload, &routePayload); err != nil {
			t.Fatal(err)
		}
		if got.route.Identity.InputDigest != got.settle.ID || got.route.Identity.ResultCode != "exclusive/"+verdict ||
			routePayload.Plan.SettlementCommandID != got.settle.ID || routePayload.Plan.ResultCode != "exclusive/"+verdict {
			t.Fatalf("route audit authority did not conserve %q: route=%#v plan=%#v", verdict, got.route.Identity, routePayload.Plan)
		}
		for _, path := range aggregate.Routing.Paths {
			if path.Kind == PathEdge && path.Edge != nil && path.Edge.FromNodeID == "choose" {
				if got.edge.ID != "" {
					t.Fatalf("multiple selected decision edges: %#v and %#v", got.edge, *path.Edge)
				}
				got.edge = *path.Edge
			}
		}
		if got.edge.Outcome != verdict {
			t.Fatalf("selected edge outcome = %q, want %q", got.edge.Outcome, verdict)
		}
		return got
	}

	upper := run(t, "Go")
	lower := run(t, "go")
	if upper.settle.ID == lower.settle.ID || upper.settle.PayloadHash == lower.settle.PayloadHash ||
		upper.route.ID == lower.route.ID || upper.route.PayloadHash == lower.route.PayloadHash || upper.edge.ID == lower.edge.ID {
		t.Fatalf("case-distinct verdict authority collapsed: upper=%#v lower=%#v", upper, lower)
	}
	upperInput, err := VerifyExclusiveInput(t.Context(), upper.observed, source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReduceExclusiveRoute(t.Context(), upperInput, upper.pending, lower.route); !errors.Is(err, ErrMutationInvalid) {
		t.Fatalf("cross-case route replay error = %v", err)
	}

	initial := initializedExclusiveCheckpoint(t, source)
	input, err := VerifyExclusiveInput(t.Context(), initial, source)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := DecodeCheckpointV7(initial)
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
	_, claimed, _, err := ValidateExecutionTransitionForAppend(t.Context(), initial, source, claim)
	if err != nil {
		t.Fatal(err)
	}
	claimedInput, err := VerifyExclusiveInput(t.Context(), claimed, source)
	if err != nil {
		t.Fatal(err)
	}
	recovered, found, err := RecoverExclusiveAttempt(t.Context(), claimedInput)
	if err != nil || !found {
		t.Fatalf("recover claim: found=%v err=%v", found, err)
	}
	if transition, err := ObserveExclusiveAttempt(t.Context(), claimedInput, recovered, ExclusiveObservation{Outcome: "GO", Actor: "human:operator"}, false); !errors.Is(err, ErrExclusiveUnsupported) || transition != nil {
		t.Fatalf("ambiguous normalized verdict transition=%#v error=%v", transition, err)
	}
}

func TestAdvanceExclusiveRouteClosesMultiWayLosersAtomically(t *testing.T) {
	source := exclusiveFanoutTemplate(5, false)
	observed := observedExclusiveAttemptForTest(t, initializedExclusiveCheckpoint(t, source), source, ExclusiveObservation{
		Outcome: exclusiveOutcome(3), Actor: "human:operator",
	})
	input, err := VerifyExclusiveInput(t.Context(), observed, source)
	if err != nil {
		t.Fatal(err)
	}
	transition, err := AdvanceExclusiveRoute(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	_, _, checkpoint, err := ValidateExecutionTransitionForAppend(t.Context(), observed, source, transition)
	if err != nil {
		t.Fatal(err)
	}
	aggregate, err := CurrentAggregateCheckpoint(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	closed := 0
	for _, reservation := range aggregate.Routing.Reservations {
		if reservation.State == ReservationClosedNoActivation {
			closed++
		}
	}
	if closed != 4 || len(aggregate.Routing.CandidateClosures) != 4 {
		t.Fatalf("transition closed=%d closures=%d", closed, len(aggregate.Routing.CandidateClosures))
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
