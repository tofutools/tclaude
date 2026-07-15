package pathv1

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"testing/quick"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	legacyplan "github.com/tofutools/tclaude/pkg/claude/process/plan"
)

const exclusiveParityTemplate = `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: exclusive-parity-v6
start: work
nodes:
  work:
    type: task
    performer: {kind: agent, prompt: work}
    retry: {maxAttempts: 2}
    next: {pass: done, fail: failed}
  done: {type: end}
  failed: {type: end, result: failed}
`

func TestPureExclusiveMatchesV6TaskAndRetrySemantics(t *testing.T) {
	source := []byte(exclusiveParityTemplate)
	parsed, err := model.Parse(source)
	if err != nil || parsed.Diagnostics.HasErrors() {
		t.Fatalf("parse parity template: %v, %#v", err, parsed.Diagnostics)
	}
	checkpoint := initializedExclusiveCheckpoint(t, source)
	input, err := VerifyExclusiveInput(t.Context(), checkpoint, source)
	if err != nil {
		t.Fatal(err)
	}
	pathID := input.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID
	node := parsed.Template.Nodes[parsed.Template.Start]
	for _, test := range []struct {
		outcome string
		attempt uint64
		want    string
	}{
		{outcome: "pass", attempt: 1, want: legacyplan.ResolvePassEdge(node.Next, "pass")},
		{outcome: "fail", attempt: 2, want: legacyplan.ResolveFailEdge(node.Next)},
	} {
		observation := ExclusiveObservation{SourcePathID: pathID, Attempt: test.attempt, Outcome: test.outcome}
		command, err := PlanExclusiveRoute(t.Context(), input, observation)
		if err != nil {
			t.Fatal(err)
		}
		projection, err := ReduceExclusiveRoute(t.Context(), input, observation, command)
		if err != nil {
			t.Fatal(err)
		}
		if target := selectedExclusiveTarget(projection.Routing(), pathID); target != test.want {
			t.Fatalf("%s target = %q, want v6 %q", test.outcome, target, test.want)
		}
	}
	if got := legacyplan.SettleNodeStatus("fail", 1, node.Retry); string(got) != "ready" {
		t.Fatalf("v6 retry fixture status = %q", got)
	}
	pending := ExclusiveObservation{SourcePathID: pathID, Attempt: 1, Outcome: "fail"}
	if got, err := ClassifyExclusiveObservation(t.Context(), input, pending); err != nil || got != ExclusiveRetryPending {
		t.Fatalf("pure retry parity = %q, %v", got, err)
	}
}

func TestPureExclusiveTokenConservationProperty(t *testing.T) {
	source := []byte(exclusiveParityTemplate)
	checkpoint := initializedExclusiveCheckpoint(t, source)
	property := func(pass bool) bool {
		input, err := VerifyExclusiveInput(context.Background(), checkpoint, source)
		if err != nil {
			return false
		}
		outcome, attempt := "pass", uint64(1)
		if !pass {
			outcome, attempt = "fail", 2
		}
		pathID := input.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID
		observation := ExclusiveObservation{SourcePathID: pathID, Attempt: attempt, Outcome: outcome}
		command, err := PlanExclusiveRoute(context.Background(), input, observation)
		if err != nil {
			return false
		}
		projection, err := ReduceExclusiveRoute(context.Background(), input, observation, command)
		if err != nil {
			return false
		}
		routing := projection.Routing()
		parent := routing.Paths[pathID]
		arrived, impossible := 0, 0
		for _, childID := range parent.ProducedPathIDs {
			switch routing.Paths[childID].State {
			case PathArrived:
				arrived++
			case PathImpossible:
				impossible++
			}
		}
		return arrived == 1 && impossible == 1 && arrived+impossible == len(parent.ProducedPathIDs)
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 100}); err != nil {
		t.Fatal(err)
	}
}

func TestPureExclusiveBoundsAndCancellationFailBeforePlanning(t *testing.T) {
	if _, err := VerifyExclusiveInput(t.Context(), bytes.Repeat([]byte{'x'}, MaxCheckpointBytes+1), []byte("x")); err == nil {
		t.Fatal("oversized checkpoint accepted")
	}
	if _, err := VerifyExclusiveInput(t.Context(), []byte("{}"), bytes.Repeat([]byte{'x'}, MaxCheckpointBytes+1)); err == nil {
		t.Fatal("oversized template accepted")
	}
	next := make(model.Next, MaxOutgoingOrAllCandidates+1)
	for index := 0; index < MaxOutgoingOrAllCandidates+1; index++ {
		next[string(rune(index+1))] = "target"
	}
	if _, err := exactOutgoingEdges("template", "source", next); err == nil {
		t.Fatal("over-budget outgoing topology accepted")
	}

	source := []byte(exclusiveParityTemplate)
	input, err := VerifyExclusiveInput(t.Context(), initializedExclusiveCheckpoint(t, source), source)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	observation := ExclusiveObservation{SourcePathID: input.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID, Attempt: 1, Outcome: "pass"}
	if _, err := PlanExclusiveRoute(canceled, input, observation); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled plan error = %v", err)
	}
}

func TestPureExclusiveConcurrentPlanningIsDeterministic(t *testing.T) {
	source := []byte(exclusiveParityTemplate)
	input, err := VerifyExclusiveInput(t.Context(), initializedExclusiveCheckpoint(t, source), source)
	if err != nil {
		t.Fatal(err)
	}
	observation := ExclusiveObservation{SourcePathID: input.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID, Attempt: 1, Outcome: "pass"}
	want, err := PlanExclusiveRoute(t.Context(), input, observation)
	if err != nil {
		t.Fatal(err)
	}
	const workers = 32
	results := make(chan CommandRecord, workers)
	errs := make(chan error, workers)
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			got, err := PlanExclusiveRoute(context.Background(), input, observation)
			if err != nil {
				errs <- err
				return
			}
			results <- got
		}()
	}
	group.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for got := range results {
		if !reflect.DeepEqual(got, want) {
			t.Fatal("concurrent plan bytes differ")
		}
	}
}

func FuzzPureExclusiveRejectsCommandMutation(f *testing.F) {
	f.Add([]byte(" "))
	f.Add([]byte(`{"drift":true}`))
	f.Fuzz(func(t *testing.T, mutation []byte) {
		if len(mutation) > 256 {
			t.Skip()
		}
		source := []byte(exclusiveParityTemplate)
		input, err := VerifyExclusiveInput(t.Context(), initializedExclusiveCheckpoint(t, source), source)
		if err != nil {
			t.Fatal(err)
		}
		observation := ExclusiveObservation{SourcePathID: input.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID, Attempt: 1, Outcome: "pass"}
		command, err := PlanExclusiveRoute(t.Context(), input, observation)
		if err != nil {
			t.Fatal(err)
		}
		if len(mutation) == 0 {
			mutation = []byte(" ")
		}
		command.Payload = append(command.Payload, mutation...)
		if _, err := ReduceExclusiveRoute(t.Context(), input, observation, command); !errors.Is(err, ErrMutationInvalid) {
			t.Fatalf("mutated command error = %v", err)
		}
	})
}

func selectedExclusiveTarget(routing RoutingState, parentID PathID) string {
	for _, childID := range routing.Paths[parentID].ProducedPathIDs {
		child := routing.Paths[childID]
		if child.Kind == PathEdge && child.State == PathArrived {
			return child.Edge.ToNodeID
		}
	}
	return ""
}
