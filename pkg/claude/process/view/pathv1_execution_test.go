package view

import (
	"reflect"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	legacy "github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
)

func TestProjectCurrentPathV1ViewerV2UsesCurrentCheckpointOnly(t *testing.T) {
	source := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: current-viewer
start: work
nodes:
  work:
    type: task
    performer: {kind: agent, prompt: work}
    next: {pass: done, fail: failed}
  done: {type: end, result: completed}
  failed: {type: end, result: failed}
`)
	current := viewerInitializedCheckpoint(t, source)
	input, err := pathv1.VerifyExclusiveInput(t.Context(), current, source)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := pathv1.DecodeCheckpointV7(current)
	if err != nil {
		t.Fatal(err)
	}
	aggregate, err := pathv1.CurrentAggregateCheckpoint(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := pathv1.PlanExclusiveAttempt(t.Context(), input, aggregate.Authority.Genesis.OutputPathID, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := pathv1.ClaimExclusiveAttempt(t.Context(), input, plan)
	if err != nil {
		t.Fatal(err)
	}
	_, current, _, err = pathv1.ValidateExecutionTransitionForAppend(t.Context(), current, source, claim)
	if err != nil {
		t.Fatal(err)
	}
	input, err = pathv1.VerifyExclusiveInput(t.Context(), current, source)
	if err != nil {
		t.Fatal(err)
	}
	recovered, found, err := pathv1.RecoverExclusiveAttempt(t.Context(), input)
	if err != nil || !found {
		t.Fatalf("recover claim: found=%v err=%v", found, err)
	}
	observed, err := pathv1.ObserveExclusiveAttempt(t.Context(), input, recovered, pathv1.ExclusiveObservation{Outcome: "pass", Actor: "agent:agt_test1"}, false)
	if err != nil {
		t.Fatal(err)
	}
	_, current, _, err = pathv1.ValidateExecutionTransitionForAppend(t.Context(), current, source, observed)
	if err != nil {
		t.Fatal(err)
	}
	input, err = pathv1.VerifyExclusiveInput(t.Context(), current, source)
	if err != nil {
		t.Fatal(err)
	}
	pending, found, err := pathv1.PendingExclusiveObservation(t.Context(), input)
	if err != nil || !found {
		t.Fatalf("pending observation: found=%v err=%v", found, err)
	}
	routed, err := pathv1.AdvanceExclusiveRoute(t.Context(), input, pending)
	if err != nil {
		t.Fatal(err)
	}
	_, current, _, err = pathv1.ValidateExecutionTransitionForAppend(t.Context(), current, source, routed)
	if err != nil {
		t.Fatal(err)
	}

	report, err := ProjectCurrentPathV1ViewerV2(t.Context(), current, source)
	if err != nil {
		t.Fatal(err)
	}
	if !report.RoutingAvailable || report.Routing == nil || len(report.Routing.Edges) == 0 {
		t.Fatalf("current checkpoint routing unavailable: %#v", report)
	}
	if reflect.TypeOf(ProjectCurrentPathV1ViewerV2).NumIn() != 3 {
		t.Fatal("viewer-v2 execution entrypoint gained a non-checkpoint/template authority input")
	}
}

func viewerInitializedCheckpoint(t *testing.T, source []byte) []byte {
	t.Helper()
	parsed, err := model.Parse(source)
	if err != nil || parsed.Diagnostics.HasErrors() {
		t.Fatalf("parse fixture: %v %#v", err, parsed.Diagnostics)
	}
	inits := make([]legacy.NodeInit, 0, len(parsed.Template.Nodes))
	for id, node := range parsed.Template.Nodes {
		status := legacy.NodeStatusPending
		if id == parsed.Template.Start {
			status = legacy.NodeStatusReady
		}
		inits = append(inits, legacy.NodeInit{ID: id, Type: node.Type, Status: status})
	}
	state := legacy.New("run-current-viewer", parsed.Ref, parsed.Ref, inits)
	state.Status = legacy.RunStatusRunning
	legacyBytes, err := legacy.Encode(&state)
	if err != nil {
		t.Fatal(err)
	}
	needed, err := pathv1.AssessUpgradeNeeded(t.Context(), legacyBytes, &state, parsed.Ref, parsed.SourceHash, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := pathv1.BuildInitialization(t.Context(), needed, parsed.Template)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := pathv1.EncodeCheckpointV7(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
