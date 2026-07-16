package pathv1

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

func TestParallelSplitConservesTokensAndExactReservations(t *testing.T) {
	t.Parallel()
	source := parallelSplitSource(3)
	checkpoint := initializedExclusiveCheckpoint(t, source)
	input, err := VerifyParallelInput(t.Context(), checkpoint, source)
	if err != nil {
		t.Fatal(err)
	}
	sourcePathID := input.base.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID
	first, err := PlanParallelSplit(t.Context(), input, sourcePathID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := PlanParallelSplit(t.Context(), input, sourcePathID)
	if err != nil {
		t.Fatal(err)
	}
	if !exactExclusiveCommand(first, second) {
		t.Fatal("same checkpoint did not produce a byte-identical split command")
	}

	var envelope mutationPayload[RoutePathsPlan]
	if err := decodeExactPayload(first.Payload, &envelope); err != nil {
		t.Fatal(err)
	}
	plan := envelope.Plan
	if plan.Batch.LogEntries != 1 || plan.Batch.EventSeq != int64(CurrentLastLogSeq(input.base.checkpoint)+1) {
		t.Fatalf("batch event/log = %d/%d", plan.Batch.EventSeq, plan.Batch.LogEntries)
	}
	wantMutations, err := MutationCountSplit(3)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Batch.Mutations) != wantMutations {
		t.Fatalf("split mutations = %d, want exact %d", len(plan.Batch.Mutations), wantMutations)
	}

	projection, err := ReduceParallelSplit(t.Context(), input, sourcePathID, first)
	if err != nil {
		t.Fatal(err)
	}
	if projection.ReplayDisposition() != ReplayApplied {
		t.Fatalf("fresh disposition = %q", projection.ReplayDisposition())
	}
	routing := projection.Routing()
	parent := routing.Paths[sourcePathID]
	if parent.State != PathSplit || len(parent.ProducedPathIDs) != 3 || !slices.IsSorted(parent.ProducedPathIDs) {
		t.Fatalf("split parent = %#v", parent)
	}
	if len(routing.Scopes) != 2 || len(routing.Reservations) != 5 { // genesis + 3 targets + reducer
		t.Fatalf("scope/reservation counts = %d/%d", len(routing.Scopes), len(routing.Reservations))
	}
	var childScope ScopeRecord
	for id, scope := range routing.Scopes {
		if id != input.base.checkpoint.Initialize.Aggregate.Authority.Genesis.RootScopeID {
			childScope = scope
		}
	}
	if childScope.ID == "" || childScope.State != ScopeOpen || len(childScope.ExpectedBranchEdgeIDs) != 3 || !slices.IsSorted(childScope.ExpectedBranchEdgeIDs) {
		t.Fatalf("child scope = %#v", childScope)
	}
	reducer := routing.Reservations[childScope.JoinReservationID]
	if reducer.State != ReservationOpen || reducer.JoinPolicy != JoinAll || !reducer.IsReducing || reducer.ReducesScopeID != childScope.ID || len(reducer.Candidates) != 3 {
		t.Fatalf("reducer = %#v", reducer)
	}
	seenEdges := map[EdgeID]struct{}{}
	for _, childID := range parent.ProducedPathIDs {
		child := routing.Paths[childID]
		reservation := routing.Reservations[child.TargetReservationID]
		if child.Kind != PathEdge || child.State != PathArrived || child.ScopeID != childScope.ID || reservation.State != ReservationOpen || reservation.IsReducing {
			t.Fatalf("eligible child/reservation = %#v / %#v", child, reservation)
		}
		if child.Edge == nil || child.BranchEdgeID != child.Edge.ID || child.ArrivedSeq != plan.Batch.EventSeq {
			t.Fatalf("child edge/sequence = %#v", child)
		}
		seenEdges[child.Edge.ID] = struct{}{}
	}
	if len(seenEdges) != 3 {
		t.Fatalf("unique branch edges = %d", len(seenEdges))
	}

	// Already-applied replay is a no-op with the same single-event payload.
	for attempt := 0; attempt < 2; attempt++ {
		disposition, replayErr := projection.Replay(first)
		if replayErr != nil || disposition != ReplayAlreadyApplied {
			t.Fatalf("already-applied replay %d = %q, %v", attempt, disposition, replayErr)
		}
	}

	partial := projection.aggregate
	delete(partial.Routing.Paths, parent.ProducedPathIDs[0])
	if _, replayErr := ReplayRoutePaths(MutationReplayView{Aggregate: partial.View(), Checkpoint: projection.binding}, first); !errors.Is(replayErr, ErrMutationInconsistent) {
		t.Fatalf("partial post-state replay = %v", replayErr)
	}
}

func TestParallelSplitBatchCountIsExactForSupportedDegrees(t *testing.T) {
	t.Parallel()
	for n := 2; n <= 8; n++ {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			source := parallelSplitSource(n)
			checkpoint := initializedExclusiveCheckpoint(t, source)
			input, err := VerifyParallelInput(t.Context(), checkpoint, source)
			if err != nil {
				t.Fatal(err)
			}
			pathID := input.base.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID
			command, err := PlanParallelSplit(t.Context(), input, pathID)
			if err != nil {
				t.Fatal(err)
			}
			var envelope mutationPayload[RoutePathsPlan]
			if err := decodeExactPayload(command.Payload, &envelope); err != nil {
				t.Fatal(err)
			}
			want, err := MutationCountSplit(n)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(envelope.Plan.Batch.Mutations); got != want || got > 2*n+3 {
				t.Fatalf("M_split(%d) = %d, want exact %d and <= %d", n, got, want, 2*n+3)
			}
		})
	}
}

func TestParallelSplitLimitsAndWideFrontierTopology(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		n, want int
		ok      bool
	}{{2, 7, true}, {MaxOutgoingOrAllCandidates, 4_095, true}, {MaxOutgoingOrAllCandidates + 1, 0, false}} {
		got, err := MutationCountSplit(test.n)
		if test.ok && (err != nil || got != test.want || got > MaxRoutingMutations) {
			t.Fatalf("MutationCountSplit(%d) = %d, %v", test.n, got, err)
		}
		if !test.ok && err == nil {
			t.Fatalf("MutationCountSplit(%d) unexpectedly accepted", test.n)
		}
	}

	tmpl := parallelWideTemplate(MaxOutgoingOrAllCandidates)
	topology, err := deriveParallelTopology(tmpl, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if topology.reducers["fork"] != "merge" || len(topology.incoming["merge"]) != MaxOutgoingOrAllCandidates {
		t.Fatalf("wide topology reducer/inbound = %q/%d", topology.reducers["fork"], len(topology.incoming["merge"]))
	}
}

func TestParallelSplitUsesEdgeTupleOrderBeforeOpaqueHashOrder(t *testing.T) {
	t.Parallel()
	labels := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel"}
	var input *VerifiedParallelInput
	var edges []EdgeKey
	for i := 0; i < len(labels) && input == nil; i++ {
		for j := i + 1; j < len(labels); j++ {
			candidateSource := parallelTupleOrderSource(labels[i], labels[j])
			checkpoint := initializedExclusiveCheckpoint(t, candidateSource)
			candidate, err := VerifyParallelInput(t.Context(), checkpoint, candidateSource)
			if err != nil {
				t.Fatal(err)
			}
			templateRef := candidate.base.checkpoint.Initialize.Aggregate.Authority.TemplateRef
			candidateEdges, err := exactParallelOutgoingEdges(templateRef, "fork", candidate.base.template.Nodes["fork"].Next)
			if err != nil {
				t.Fatal(err)
			}
			if candidateEdges[0].ID > candidateEdges[1].ID {
				input, edges = candidate, candidateEdges
				break
			}
		}
	}
	if input == nil {
		t.Fatal("fixed template set has no tuple/hash-order reversal")
	}
	if edges[0].ID < edges[1].ID {
		t.Fatalf("tuple edge order = %#v", edges)
	}
	pathID := input.base.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID
	command, err := PlanParallelSplit(t.Context(), input, pathID)
	if err != nil {
		t.Fatal(err)
	}
	var envelope mutationPayload[RoutePathsPlan]
	if err := decodeExactPayload(command.Payload, &envelope); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(envelope.Plan.SelectedEdgeIDs, []EdgeID{edges[0].ID, edges[1].ID}) {
		t.Fatalf("materialized selected edges = %v, want tuple order %v", envelope.Plan.SelectedEdgeIDs, []EdgeID{edges[0].ID, edges[1].ID})
	}
	reversed := envelope.Plan
	reversed.SelectedEdgeIDs = slices.Clone(reversed.SelectedEdgeIDs)
	slices.Reverse(reversed.SelectedEdgeIDs)
	if err := reversed.Validate(); !errors.Is(err, ErrMutationInvalid) {
		t.Fatalf("hash-ordered selected edges validation = %v", err)
	}
	projection, err := ReduceParallelSplit(t.Context(), input, pathID, command)
	if err != nil {
		t.Fatal(err)
	}
	for _, edgeID := range envelope.Plan.SelectedEdgeIDs {
		found := false
		for _, child := range projection.aggregate.Routing.Paths {
			if child.ParentPathID == pathID && child.Edge != nil && child.Edge.ID == edgeID {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("tuple-ordered selected edge %q was not materialized", edgeID)
		}
	}
}

func TestParallelSplitMaximumDegreeEndToEnd(t *testing.T) {
	source := parallelSplitSource(MaxOutgoingOrAllCandidates)
	checkpoint := initializedExclusiveCheckpoint(t, source)
	input, err := VerifyParallelInput(t.Context(), checkpoint, source)
	if err != nil {
		t.Fatal(err)
	}
	pathID := input.base.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID
	command, err := PlanParallelSplit(t.Context(), input, pathID)
	if err != nil {
		t.Fatal(err)
	}
	var envelope mutationPayload[RoutePathsPlan]
	if err := decodeExactPayload(command.Payload, &envelope); err != nil {
		t.Fatal(err)
	}
	if got := len(envelope.Plan.Batch.Mutations); got != 4_095 || got > MaxRoutingMutations || envelope.Plan.Batch.LogEntries != 1 {
		t.Fatalf("maximum split mutations/log = %d/%d", got, envelope.Plan.Batch.LogEntries)
	}
	projection, err := ReduceParallelSplit(t.Context(), input, pathID, command)
	if err != nil {
		t.Fatal(err)
	}
	routing := projection.aggregate.Routing
	parent := routing.Paths[pathID]
	if len(parent.ProducedPathIDs) != MaxOutgoingOrAllCandidates || len(routing.Scopes) != 2 || len(routing.Reservations) != MaxOutgoingOrAllCandidates+2 {
		t.Fatalf("maximum split paths/scopes/reservations = %d/%d/%d", len(parent.ProducedPathIDs), len(routing.Scopes), len(routing.Reservations))
	}
	if disposition, replayErr := projection.Replay(command); replayErr != nil || disposition != ReplayAlreadyApplied {
		t.Fatalf("maximum split replay = %q, %v", disposition, replayErr)
	}

	// The degree guard runs before the routing clone or mutation materializer.
	input.base.template.Nodes["fork"].Next["over-cap"] = "work-0000"
	if _, err := PlanParallelSplit(t.Context(), input, pathID); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("2,047-way split error = %v", err)
	}
}

func parallelSplitSource(n int) []byte {
	var out strings.Builder
	out.WriteString("apiVersion: tclaude.dev/v1alpha1\nkind: ProcessTemplate\nid: parallel-split\nstart: fork\nnodes:\n  fork:\n    type: parallel\n    next:\n")
	for index := 0; index < n; index++ {
		fmt.Fprintf(&out, "      branch-%04d: work-%04d\n", index, index)
	}
	for index := 0; index < n; index++ {
		fmt.Fprintf(&out, "  work-%04d:\n    type: task\n    performer: {kind: agent, prompt: work}\n    next: merge\n", index)
	}
	out.WriteString("  merge:\n    type: end\n    join: all\n")
	return []byte(out.String())
}

func parallelTupleOrderSource(firstOutcome, secondOutcome string) []byte {
	return []byte(fmt.Sprintf(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: tuple-order
start: fork
nodes:
  fork:
    type: parallel
    next:
      %s: left
      %s: right
  left:
    type: task
    performer: {kind: agent, prompt: left}
    next: merge
  right:
    type: task
    performer: {kind: agent, prompt: right}
    next: merge
  merge:
    type: end
    join: all
`, firstOutcome, secondOutcome))
}

func parallelWideTemplate(n int) *model.Template {
	nodes := make(map[string]model.Node, n+2)
	next := make(model.Next, n)
	for index := 0; index < n; index++ {
		nodeID := fmt.Sprintf("work-%04d", index)
		next[fmt.Sprintf("branch-%04d", index)] = nodeID
		nodes[nodeID] = model.Node{Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "work"}, Next: model.Next{"next": "merge"}}
	}
	nodes["fork"] = model.Node{Type: model.NodeTypeParallel, Next: next}
	nodes["merge"] = model.Node{Type: model.NodeTypeEnd, Join: model.JoinAll}
	return &model.Template{APIVersion: model.APIVersion, Kind: model.Kind, ID: "wide", Start: "fork", Nodes: nodes}
}
