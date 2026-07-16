package pathv1

import (
	"container/heap"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

var (
	ErrParallelInputInvalid = errors.New("path-v1 parallel input is invalid")
	ErrParallelUnsupported  = errors.New("path-v1 parallel operation is unsupported")
)

// VerifiedParallelInput is a sealed, exact-template input for the pure
// fan-out substrate. Production reaches it only through the combined
// VerifyExecutionInput parallel-all gate.
type VerifiedParallelInput struct {
	base     *VerifiedExclusiveInput
	topology parallelTopology
}

// ParallelProjection is the detached result of one pure split event. Both
// branch arrivals are concurrently eligible/claimable after the split; this
// type makes no wall-clock scheduling or simultaneous-execution promise.
type ParallelProjection struct {
	aggregate AggregateCheckpoint
	binding   CheckpointBinding
	command   CommandRecord
	dispose   ReplayDisposition
}

func (p *ParallelProjection) Binding() CheckpointBinding {
	if p == nil {
		return CheckpointBinding{}
	}
	return p.binding
}

func (p *ParallelProjection) Command() CommandRecord {
	if p == nil {
		return CommandRecord{}
	}
	return cloneCommandRecord(p.command)
}

func (p *ParallelProjection) ReplayDisposition() ReplayDisposition {
	if p == nil {
		return ""
	}
	return p.dispose
}

func (p *ParallelProjection) Routing() RoutingState {
	if p == nil {
		return RoutingState{}
	}
	return Clone(p.aggregate.Routing)
}

// Replay proves restart/CAS idempotency against this projection's already
// durable aggregate. A strict partial post-state is rejected by MutationBatch.
func (p *ParallelProjection) Replay(command CommandRecord) (ReplayDisposition, error) {
	if p == nil || !exactExclusiveCommand(p.command, command) {
		return "", fmt.Errorf("%w: supplied split command differs from sealed projection", ErrMutationInvalid)
	}
	result, err := ReplayRoutePaths(MutationReplayView{Aggregate: p.aggregate.View(), Checkpoint: p.binding}, command)
	if err != nil {
		return "", err
	}
	return result.Disposition, nil
}

// VerifyParallelInput performs strict checkpoint, exact-source, and pinned
// template verification, then derives the complete static scope plan.
func VerifyParallelInput(ctx context.Context, checkpointBytes, templateSource []byte) (*VerifiedParallelInput, error) {
	base, err := verifyExecutionInput(ctx, checkpointBytes, templateSource)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrParallelInputInvalid, err)
	}
	topology, err := deriveParallelTopology(base.template, base.checkpoint.Initialize.Aggregate.TemplateRef)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrParallelInputInvalid, err)
	}
	if len(topology.reducers) == 0 {
		return nil, fmt.Errorf("%w: exact template has no parallel scope", ErrParallelUnsupported)
	}
	base.parallel = &topology
	return &VerifiedParallelInput{base: base, topology: topology}, nil
}

// PlanParallelSplit returns the sole canonical route_paths_v1 command for one
// instantaneous gateway split. It performs no I/O and grants no execution
// capability.
func PlanParallelSplit(ctx context.Context, input *VerifiedParallelInput, sourcePathID PathID) (CommandRecord, error) {
	draft, err := buildParallelSplitDraft(ctx, input, sourcePathID)
	if err != nil {
		return CommandRecord{}, err
	}
	return cloneCommandRecord(draft.command), nil
}

// ReduceParallelSplit applies exactly one aggregate routing event. The batch
// marks the activation output split and creates its scope, every child arrival,
// every immediate target reservation, and the unique reducer reservation.
func ReduceParallelSplit(ctx context.Context, input *VerifiedParallelInput, sourcePathID PathID, command CommandRecord) (*ParallelProjection, error) {
	draft, err := buildParallelSplitDraft(ctx, input, sourcePathID)
	if err != nil {
		return nil, err
	}
	if !exactExclusiveCommand(draft.command, command) {
		return nil, fmt.Errorf("%w: supplied split command differs from deterministic plan", ErrMutationInvalid)
	}
	result, err := ReplayRoutePaths(draft.view, command)
	if err != nil {
		return nil, err
	}
	post := draft.view.Aggregate
	post.Routing = &result.Routing
	if err := validateParallelSplitConservation(post, draft); err != nil {
		return nil, err
	}
	if report := ValidateAggregate(post); !report.Valid() {
		return nil, fmt.Errorf("%w: projected aggregate has %d diagnostics", ErrMutationInconsistent, len(report.Diagnostics)+report.Suppressed)
	}
	aggregate, err := checkpointAggregate(post)
	if err != nil {
		return nil, err
	}
	// Cardinality and mutation bounds are independent of the canonical
	// checkpoint ceiling. Prove this projection can be installed as one exact
	// schema-7 revision before exposing it to a future engine gate.
	if _, err := encodeParallelProjectionCheckpoint(input.base.checkpoint, aggregate); err != nil {
		return nil, err
	}
	return &ParallelProjection{aggregate: aggregate, binding: input.base.binding, command: cloneCommandRecord(command), dispose: result.Disposition}, nil
}

// AdvanceParallelSplit seals one pure split projection as the exact next
// schema-7 checkpoint. This is enabled only through VerifyExecutionInput's
// combined parallel-all gate.
func AdvanceParallelSplit(ctx context.Context, input *VerifiedExclusiveInput, sourcePathID PathID) (*ExecutionTransition, error) {
	if input == nil || input.parallel == nil {
		return nil, fmt.Errorf("%w: verified parallel input is required", ErrParallelInputInvalid)
	}
	parallel := &VerifiedParallelInput{base: input, topology: *input.parallel}
	command, err := PlanParallelSplit(ctx, parallel, sourcePathID)
	if err != nil {
		return nil, err
	}
	projection, err := ReduceParallelSplit(ctx, parallel, sourcePathID, command)
	if err != nil {
		return nil, err
	}
	last, err := aggregateLogicalLastSeq(projection.aggregate)
	if err != nil {
		return nil, err
	}
	next, err := advanceCheckpointV7To(input.checkpoint, projection.aggregate, CurrentRunStatus(input.checkpoint), last)
	if err != nil {
		return nil, err
	}
	return newExecutionTransition(input.checkpoint, next, "parallel_split")
}

// encodeParallelProjectionCheckpoint uses the exact schema-7 persistence
// encoder without cloning the projected routing state a second time. The
// returned bytes are only a budget proof in TCL-445; no checkpoint capability
// is exposed until the combined gate lands.
func encodeParallelProjectionCheckpoint(checkpoint *CheckpointV7, aggregate AggregateCheckpoint) ([]byte, error) {
	if err := ValidateCheckpointV7(checkpoint); err != nil {
		return nil, err
	}
	status := CurrentRunStatus(checkpoint)
	if !runtimeStatusValid(status) {
		return nil, fmt.Errorf("%w: invalid execution status %q", ErrMutationInvalid, status)
	}
	currentSeq := CurrentLastLogSeq(checkpoint)
	if currentSeq >= math.MaxInt64 || CheckpointRevision(checkpoint) == math.MaxUint64 {
		return nil, &OverBudgetError{Limit: "execution_revision", Value: math.MaxInt64, Maximum: math.MaxInt64 - 1}
	}
	lastLogSeq := currentSeq + 1
	aggregateLast, err := aggregateLogicalLastSeq(aggregate)
	if err != nil {
		return nil, err
	}
	if aggregateLast > lastLogSeq {
		lastLogSeq = aggregateLast
	}
	if lastLogSeq-currentSeq > MaxRoutingLogEntries {
		return nil, &OverBudgetError{Limit: "log_entries", Value: int(lastLogSeq - currentSeq), Maximum: MaxRoutingLogEntries}
	}
	initialize := checkpoint.Initialize
	if aggregate.RunID != initialize.UpgradeNeeded.RunID || aggregate.TemplateRef != initialize.TemplateHash || aggregate.TemplateSourceHash != initialize.UpgradeNeeded.TemplateSourceHash {
		return nil, fmt.Errorf("%w: current aggregate differs from immutable initialization authority", ErrMutationInconsistent)
	}
	execution := &ExecutionCheckpoint{
		Revision: CheckpointRevision(checkpoint) + 1, PreviousDigest: checkpoint.Digest,
		Status: status, LogAdvanced: true, LastLogSeq: lastLogSeq, Aggregate: aggregate,
	}
	execution.LogChecksum, err = executionLogChecksum(execution)
	if err != nil {
		return nil, err
	}
	next := *checkpoint // shallow container copy; aggregate routing is not cloned
	next.Execution = execution
	genesisDigest, err := initializeEventDigest(next.Initialize)
	if err != nil {
		return nil, err
	}
	next.Digest, err = executionCheckpointDigest(genesisDigest, execution)
	if err != nil {
		return nil, err
	}
	return EncodeCheckpointV7(&next)
}

type parallelScopeFrame struct {
	fork, branch string
}

type parallelTopology struct {
	edges             map[EdgeID]EdgeKey
	incoming          map[string][]EdgeKey
	incomingSignature map[EdgeID][]parallelScopeFrame
	reducers          map[string]string
}

type parallelNodeHeap []string

func (h parallelNodeHeap) Len() int           { return len(h) }
func (h parallelNodeHeap) Less(i, j int) bool { return h[i] < h[j] }
func (h parallelNodeHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *parallelNodeHeap) Push(value any)    { *h = append(*h, value.(string)) }
func (h *parallelNodeHeap) Pop() any {
	old := *h
	last := len(old) - 1
	value := old[last]
	old[last] = ""
	*h = old[:last]
	return value
}

func validateParallelMaterializedTopology(ctx context.Context, view AggregateView, tmpl *model.Template, topology parallelTopology) error {
	if view.Routing == nil || view.Authority == nil || tmpl == nil {
		return fmt.Errorf("complete aggregate and exact template are required")
	}
	for _, path := range view.Routing.Paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		if path.Edge == nil {
			continue
		}
		edge, ok := topology.edges[path.Edge.ID]
		if !ok || edge != *path.Edge {
			return fmt.Errorf("path %q edge is outside exact parallel topology", path.ID)
		}
	}
	root := view.Authority.Genesis.RootScopeID
	for id, scope := range view.Routing.Scopes {
		if id == root {
			continue
		}
		activation, ok := view.Routing.Activations[scope.ForkActivationID]
		if !ok {
			return fmt.Errorf("scope %q fork activation is absent", id)
		}
		fork, ok := view.Routing.Reservations[activation.ReservationID]
		if !ok || tmpl.Nodes[fork.NodeID].Type != model.NodeTypeParallel || topology.reducers[fork.NodeID] != scope.JoinNodeID {
			return fmt.Errorf("scope %q fork/reducer differs from exact topology", id)
		}
		outgoing, err := exactParallelOutgoingEdges(view.TemplateRef, fork.NodeID, tmpl.Nodes[fork.NodeID].Next)
		if err != nil {
			return err
		}
		branchIDs := parallelEdgeIDs(outgoing)
		slices.Sort(branchIDs)
		if !slices.Equal(branchIDs, scope.ExpectedBranchEdgeIDs) {
			return fmt.Errorf("scope %q branch set differs from exact topology", id)
		}
		reducer, ok := view.Routing.Reservations[scope.JoinReservationID]
		if !ok || reducer.NodeID != scope.JoinNodeID || reducer.JoinPolicy != JoinAll || !reducer.IsReducing || reducer.ReducesScopeID != scope.ID {
			return fmt.Errorf("scope %q reducer reservation is not exact all authority", id)
		}
	}
	for id, reservation := range view.Routing.Reservations {
		if _, ok := tmpl.Nodes[reservation.NodeID]; !ok || reservation.JoinPolicy == JoinAny {
			return fmt.Errorf("reservation %q node/policy is outside parallel-all authority", id)
		}
		wantID, err := ReservationIdentity(view.RunID, reservation.NodeID, reservation.ScopeID, reservation.BranchEdgeID, reservation.Generation)
		if err != nil || wantID != id {
			return fmt.Errorf("reservation %q identity does not recompute", id)
		}
		if id == view.Authority.Genesis.ReservationID {
			continue
		}
		for _, candidate := range reservation.Candidates {
			wantCandidate, err := CandidateIdentity(id, candidate.Kind, candidate.MemberID)
			if err != nil || wantCandidate != candidate.ID {
				return fmt.Errorf("reservation %q candidate identity does not recompute", id)
			}
			if candidate.Kind == CandidateInboundEdge {
				edge, ok := topology.edges[candidate.MemberID]
				if !ok || edge.ToNodeID != reservation.NodeID {
					return fmt.Errorf("reservation %q inbound candidate is outside exact topology", id)
				}
			}
		}
		for _, slot := range reservation.PossibleSlots {
			edge, ok := topology.edges[slot.SourceEdgeID]
			if !ok || edge.FromNodeID != slot.SourceNodeID || edge.ToNodeID != reservation.NodeID {
				return fmt.Errorf("reservation %q possible slot is outside exact topology", id)
			}
			wantSlot, err := PossibleSlotIdentity(id, slot.CandidateID, slot.SourceNodeID, slot.SourceEdgeID, slot.SourceScopeID, slot.SourceBranchEdgeID, slot.Generation)
			if err != nil || wantSlot != slot.ID {
				return fmt.Errorf("reservation %q possible slot identity does not recompute", id)
			}
		}
	}
	return nil
}

func deriveParallelTopology(tmpl *model.Template, templateRef string) (parallelTopology, error) {
	if tmpl == nil || !canonicalDigest(templateRef) {
		return parallelTopology{}, fmt.Errorf("exact template and semantic ref are required")
	}
	edges := make([]EdgeKey, 0)
	for _, edge := range model.NormalizeEdges(tmpl) {
		if edge.From == "" || model.IsPoisonEscalationRetryEdge(tmpl, edge) {
			continue
		}
		key := EdgeKey{TemplateRef: templateRef, FromNodeID: edge.From, Outcome: edge.Outcome, ToNodeID: edge.To}
		var err error
		key.ID, err = EdgeIdentity(templateRef, edge.From, edge.Outcome, edge.To)
		if err != nil {
			return parallelTopology{}, err
		}
		edges = append(edges, key)
	}
	slices.SortFunc(edges, compareParallelEdgeTuple)
	topology := parallelTopology{
		edges: make(map[EdgeID]EdgeKey, len(edges)), incoming: make(map[string][]EdgeKey, len(tmpl.Nodes)),
		incomingSignature: make(map[EdgeID][]parallelScopeFrame, len(edges)), reducers: map[string]string{},
	}
	outgoing := make(map[string][]EdgeKey, len(tmpl.Nodes))
	indegree := make(map[string]int, len(tmpl.Nodes))
	for nodeID := range tmpl.Nodes {
		indegree[nodeID] = 0
	}
	for _, edge := range edges {
		if _, duplicate := topology.edges[edge.ID]; duplicate {
			return parallelTopology{}, fmt.Errorf("duplicate normalized edge identity %q", edge.ID)
		}
		topology.edges[edge.ID] = edge
		topology.incoming[edge.ToNodeID] = append(topology.incoming[edge.ToNodeID], edge)
		outgoing[edge.FromNodeID] = append(outgoing[edge.FromNodeID], edge)
		indegree[edge.ToNodeID]++
	}
	for nodeID := range topology.incoming {
		slices.SortFunc(topology.incoming[nodeID], compareParallelEdgeTuple)
	}
	for nodeID := range outgoing {
		slices.SortFunc(outgoing[nodeID], compareParallelEdgeTuple)
	}

	ready := make(parallelNodeHeap, 0, len(indegree))
	for nodeID, degree := range indegree {
		if degree == 0 {
			ready = append(ready, nodeID)
		}
	}
	heap.Init(&ready)
	order := make([]string, 0, len(indegree))
	for ready.Len() > 0 {
		nodeID := heap.Pop(&ready).(string)
		order = append(order, nodeID)
		for _, edge := range outgoing[nodeID] {
			indegree[edge.ToNodeID]--
			if indegree[edge.ToNodeID] == 0 {
				heap.Push(&ready, edge.ToNodeID)
			}
		}
	}
	if len(order) != len(tmpl.Nodes) {
		return parallelTopology{}, fmt.Errorf("parallel topology is not a DAG")
	}

	outputs := make(map[string][]parallelScopeFrame, len(tmpl.Nodes))
	for _, nodeID := range order {
		incoming := topology.incoming[nodeID]
		signatures := make([][]parallelScopeFrame, 0, len(incoming))
		for _, edge := range incoming {
			signature := append([]parallelScopeFrame(nil), outputs[edge.FromNodeID]...)
			if tmpl.Nodes[edge.FromNodeID].Type == model.NodeTypeParallel {
				signature = append(signature, parallelScopeFrame{fork: edge.FromNodeID, branch: edge.ID})
			}
			topology.incomingSignature[edge.ID] = append([]parallelScopeFrame(nil), signature...)
			signatures = append(signatures, signature)
		}
		switch len(signatures) {
		case 0:
			outputs[nodeID] = nil
		case 1:
			outputs[nodeID] = append([]parallelScopeFrame(nil), signatures[0]...)
		default:
			if parallelSignaturesEqual(signatures) {
				outputs[nodeID] = append([]parallelScopeFrame(nil), signatures[0]...)
				continue
			}
			prefix, fork, branches, ok := reduceParallelSignatures(signatures)
			if !ok || !parallelBranchesComplete(outgoing[fork], branches) {
				return parallelTopology{}, fmt.Errorf("node %q has invalid cross-scope signatures", nodeID)
			}
			if previous := topology.reducers[fork]; previous != "" && previous != nodeID {
				return parallelTopology{}, fmt.Errorf("parallel fork %q has multiple reducers", fork)
			}
			topology.reducers[fork] = nodeID
			outputs[nodeID] = prefix
		}
	}
	for nodeID, node := range tmpl.Nodes {
		if node.Type == model.NodeTypeParallel && topology.reducers[nodeID] == "" {
			return parallelTopology{}, fmt.Errorf("parallel fork %q lacks a complete reducer", nodeID)
		}
	}
	return topology, nil
}

func parallelSignaturesEqual(values [][]parallelScopeFrame) bool {
	for index := 1; index < len(values); index++ {
		if !slices.Equal(values[0], values[index]) {
			return false
		}
	}
	return true
}

func reduceParallelSignatures(values [][]parallelScopeFrame) ([]parallelScopeFrame, string, map[string]struct{}, bool) {
	if len(values) < 2 || len(values[0]) == 0 {
		return nil, "", nil, false
	}
	depth := len(values[0])
	prefix := values[0][:depth-1]
	fork := values[0][depth-1].fork
	branches := make(map[string]struct{}, len(values))
	for _, value := range values {
		if len(value) != depth || value[depth-1].fork != fork || !slices.Equal(prefix, value[:depth-1]) {
			return nil, "", nil, false
		}
		branches[value[depth-1].branch] = struct{}{}
	}
	return append([]parallelScopeFrame(nil), prefix...), fork, branches, true
}

func parallelBranchesComplete(outgoing []EdgeKey, branches map[string]struct{}) bool {
	if len(outgoing) < 2 || len(outgoing) != len(branches) {
		return false
	}
	for _, edge := range outgoing {
		if _, ok := branches[edge.ID]; !ok {
			return false
		}
	}
	return true
}

type parallelSplitDraft struct {
	view               MutationReplayView
	command            CommandRecord
	sourcePathID       PathID
	scopeID            ScopeID
	reducerReservation ReservationID
	outgoing           []EdgeKey
	producedPaths      []PathID
	createdTargets     int
}

func buildParallelSplitDraft(ctx context.Context, input *VerifiedParallelInput, sourcePathID PathID) (parallelSplitDraft, error) {
	if err := ctx.Err(); err != nil {
		return parallelSplitDraft{}, err
	}
	if input == nil || input.base == nil || input.base.checkpoint == nil || input.base.template == nil || sourcePathID == "" {
		return parallelSplitDraft{}, fmt.Errorf("%w: complete sealed split input is required", ErrParallelInputInvalid)
	}
	aggregate, err := CurrentAggregateCheckpoint(input.base.checkpoint)
	if err != nil {
		return parallelSplitDraft{}, err
	}
	view := aggregate.View()
	for _, command := range view.Commands {
		if command.State.Active() {
			return parallelSplitDraft{}, fmt.Errorf("%w: active command %q must recover before split", ErrMutationInconsistent, command.ID)
		}
	}
	source, ok := view.Routing.Paths[sourcePathID]
	if !ok || source.Kind != PathActivationOutput || source.State != PathLive {
		return parallelSplitDraft{}, fmt.Errorf("%w: source is not a live activation output", ErrMutationInconsistent)
	}
	activation, ok := view.Routing.Activations[source.SourceActivation.ID]
	if !ok || activation.Ref != source.SourceActivation {
		return parallelSplitDraft{}, fmt.Errorf("%w: source activation is missing or mismatched", ErrMutationInconsistent)
	}
	sourceReservation, ok := view.Routing.Reservations[activation.ReservationID]
	if !ok || sourceReservation.State != ReservationActivated {
		return parallelSplitDraft{}, fmt.Errorf("%w: source reservation is not activated", ErrMutationInconsistent)
	}
	node, ok := input.base.template.Nodes[sourceReservation.NodeID]
	if !ok || node.Type != model.NodeTypeParallel {
		return parallelSplitDraft{}, fmt.Errorf("%w: source node %q is not parallel", ErrParallelUnsupported, sourceReservation.NodeID)
	}
	maximum, err := MutationCountSplit(len(node.Next))
	if err != nil {
		return parallelSplitDraft{}, err
	}
	outgoing, err := exactParallelOutgoingEdges(view.TemplateRef, sourceReservation.NodeID, node.Next)
	if err != nil {
		return parallelSplitDraft{}, err
	}
	if maximum > MaxRoutingMutations {
		return parallelSplitDraft{}, &OverBudgetError{Limit: "mutations", Value: maximum, Maximum: MaxRoutingMutations}
	}
	lastLogSeq := CurrentLastLogSeq(input.base.checkpoint)
	if lastLogSeq >= math.MaxInt64 {
		return parallelSplitDraft{}, &OverBudgetError{Limit: "log_entries", Value: math.MaxInt64, Maximum: math.MaxInt64 - 1}
	}
	eventSeq := int64(lastLogSeq + 1)
	before := *view.Routing
	after := Clone(before) // one aggregate-event routing clone
	authority := cloneExclusiveAuthority(view.Authority)

	scopeID, err := ScopeIdentity(view.RunID, source.ScopeID, source.BranchEdgeID, source.SourceActivation.ID, source.ID, source.SourceActivation.Generation)
	if err != nil {
		return parallelSplitDraft{}, err
	}
	if _, exists := after.Scopes[scopeID]; exists {
		return parallelSplitDraft{}, fmt.Errorf("%w: split scope already exists", ErrMutationInconsistent)
	}
	reducerNodeID := input.topology.reducers[sourceReservation.NodeID]
	if reducerNodeID == "" {
		return parallelSplitDraft{}, fmt.Errorf("%w: fork lacks exact reducer", ErrParallelInputInvalid)
	}
	branchIDs := make([]EdgeID, len(outgoing))
	for index := range outgoing {
		branchIDs[index] = outgoing[index].ID
	}
	slices.Sort(branchIDs)
	reducer, err := deriveParallelReducerReservation(input, view.RunID, view.TemplateRef, sourceReservation.NodeID, reducerNodeID, scopeID, branchIDs, eventSeq)
	if err != nil {
		return parallelSplitDraft{}, err
	}
	if _, exists := after.Reservations[reducer.ID]; exists {
		return parallelSplitDraft{}, fmt.Errorf("%w: reducer reservation already exists", ErrMutationInconsistent)
	}
	scope := ScopeRecord{
		ID: scopeID, RunID: view.RunID, ParentScopeID: source.ScopeID, ParentBranchEdgeID: source.BranchEdgeID,
		ForkActivationID: source.SourceActivation.ID, ForkOutputPathID: source.ID, Generation: source.SourceActivation.Generation,
		ExpectedBranchEdgeIDs: branchIDs, JoinNodeID: reducerNodeID, JoinReservationID: reducer.ID,
		State: ScopeOpen, EventSeq: eventSeq,
	}
	after.Scopes[scopeID] = scope
	after.Reservations[reducer.ID] = reducer
	authority.Scopes[scopeID] = ScopeAuthority{
		ID: scope.ID, ParentScopeID: scope.ParentScopeID, ParentBranchEdgeID: scope.ParentBranchEdgeID,
		ForkActivationID: scope.ForkActivationID, ForkOutputPathID: scope.ForkOutputPathID, Generation: scope.Generation,
		ExpectedBranchEdgeIDs: cloneSlice(scope.ExpectedBranchEdgeIDs), JoinNodeID: scope.JoinNodeID, JoinReservationID: scope.JoinReservationID,
	}
	authority.Reservations[reducer.ID] = reservationAuthority(reducer)

	targets := make(map[EdgeID]ActivationReservation, len(outgoing))
	createdTargets := 0
	for _, edge := range outgoing {
		if edge.ToNodeID == reducerNodeID {
			targets[edge.ID] = reducer
			continue
		}
		reservation, deriveErr := deriveParallelBranchReservation(input, view.RunID, view.TemplateRef, sourceReservation.NodeID, edge, scopeID, eventSeq)
		if deriveErr != nil {
			return parallelSplitDraft{}, deriveErr
		}
		if existing, exists := after.Reservations[reservation.ID]; exists {
			if !canonicalEqual(reservationAuthority(existing), reservationAuthority(reservation)) {
				return parallelSplitDraft{}, fmt.Errorf("%w: target reservation conflicts with exact fork authority", ErrMutationInconsistent)
			}
			reservation = existing
		} else {
			after.Reservations[reservation.ID] = reservation
			authority.Reservations[reservation.ID] = reservationAuthority(reservation)
			createdTargets++
		}
		targets[edge.ID] = reservation
	}

	produced := make([]PathID, 0, len(outgoing))
	for _, edge := range outgoing {
		if err := ctx.Err(); err != nil {
			return parallelSplitDraft{}, err
		}
		reservation := targets[edge.ID]
		candidate, ok := parallelCandidateForEdge(reservation, edge.ID)
		if !ok {
			return parallelSplitDraft{}, fmt.Errorf("%w: target reservation lacks branch edge candidate", ErrParallelInputInvalid)
		}
		reducerCandidate, reducerOK := parallelCandidateForEdge(reducer, edge.ID)
		if !reducerOK {
			return parallelSplitDraft{}, fmt.Errorf("%w: reducer lacks exact branch candidate", ErrParallelInputInvalid)
		}
		lineage, lineageID, lineageErr := AppendCandidateLineage(source, reducer.ID, reducerCandidate.ID)
		if lineageErr != nil {
			return parallelSplitDraft{}, lineageErr
		}
		if reservation.ID != reducer.ID {
			lineageParent := source
			lineageParent.CandidateLineage = lineage
			lineageParent.CandidateLineageID = lineageID
			lineageParent.LineageDepth = uint32(len(lineage))
			lineage, lineageID, lineageErr = AppendCandidateLineage(lineageParent, reservation.ID, candidate.ID)
			if lineageErr != nil {
				return parallelSplitDraft{}, lineageErr
			}
		}
		pathID, pathErr := EdgePathIdentity(source.SourceActivation.ID, source.ID, edge.ID, reservation.ID, candidate.ID)
		if pathErr != nil {
			return parallelSplitDraft{}, pathErr
		}
		arrivalID, arrivalErr := ArrivalIdentity(pathID, reservation.ID, candidate.ID)
		if arrivalErr != nil {
			return parallelSplitDraft{}, arrivalErr
		}
		after.Paths[pathID] = PathRecord{
			ID: pathID, Kind: PathEdge, State: PathArrived, ParentPathID: source.ID,
			SourceActivation: source.SourceActivation, Edge: cloneEdge(&edge), TargetReservationID: reservation.ID,
			CandidateID: candidate.ID, ScopeID: scopeID, BranchEdgeID: edge.ID,
			CandidateLineage: lineage, CandidateLineageID: lineageID, LineageDepth: uint32(len(lineage)),
			ArrivalID: arrivalID, ArrivedSeq: eventSeq, CreatedSeq: eventSeq, UpdatedSeq: eventSeq,
		}
		produced = append(produced, pathID)
	}
	slices.Sort(produced)
	parent := after.Paths[source.ID]
	parent.State = PathSplit
	parent.ProducedPathIDs = cloneSlice(produced)
	parent.UpdatedSeq = eventSeq
	dispositionID, err := DispositionReceiptIdentity(parent.ID, PathLive, PathSplit, "parallel_split", MutationCommandPlaceholder, "", uint64(eventSeq))
	if err != nil {
		return parallelSplitDraft{}, err
	}
	parent.Disposition = &DispositionReceipt{
		ID: dispositionID, PathID: parent.ID, FromState: PathLive, ToState: PathSplit,
		ReasonCode: "parallel_split", CommandID: MutationCommandPlaceholder, EventSeq: eventSeq,
	}
	after.Paths[parent.ID] = parent

	batch, err := NewMutationBatch(&before, &after, eventSeq)
	if err != nil {
		return parallelSplitDraft{}, err
	}
	wantMutations := 1 + len(outgoing) + 1 + createdTargets + 1
	if len(batch.Mutations) != wantMutations || len(batch.Mutations) > maximum {
		return parallelSplitDraft{}, fmt.Errorf("%w: split mutation count %d, want %d and at most %d", ErrMutationInconsistent, len(batch.Mutations), wantMutations, maximum)
	}

	view.Authority = authority
	view.Commands = cloneCommands(view.Commands)
	view.SideEffects = cloneMap(view.SideEffects)
	perform, settle, effect, err := observedParallelCommands(view, sourceReservation.NodeID, source)
	if err != nil {
		return parallelSplitDraft{}, err
	}
	if err := insertExactCommand(view.Commands, perform); err != nil {
		return parallelSplitDraft{}, err
	}
	if err := insertExactCommand(view.Commands, settle); err != nil {
		return parallelSplitDraft{}, err
	}
	if err := insertExactParallelSideEffect(view.SideEffects, effect); err != nil {
		return parallelSplitDraft{}, err
	}
	view.Routing = &before
	replayView := MutationReplayView{Aggregate: view, Checkpoint: input.base.binding}
	causeDigest, err := CauseSetIdentity(nil)
	if err != nil {
		return parallelSplitDraft{}, err
	}
	plan := RoutePathsPlan{
		SettlementCommandID: settle.ID, SourceActivationID: source.SourceActivation.ID,
		SourceGeneration: source.SourceActivation.Generation, SourcePathID: source.ID, Attempt: 1,
		CauseDigest: causeDigest, ResultCode: "parallel", SelectedEdgeIDs: parallelEdgeIDs(outgoing),
		ProducedPathIDs: produced, Batch: batch,
	}
	payload, err := EncodeRoutePathsPayload(replayView, plan)
	if err != nil {
		return parallelSplitDraft{}, err
	}
	identity := CommandIdentity{
		RunID: view.RunID, Kind: CommandRoutePaths, PayloadSchema: 1,
		SourceActivationID: source.SourceActivation.ID, SourceGeneration: source.SourceActivation.Generation,
		SourcePathID: source.ID, Attempt: 1, InputDigest: settle.ID, CauseDigest: causeDigest,
		PlanDigest: payloadDigest(payload), ResultCode: plan.ResultCode,
	}
	command, err := observedCommand(identity, payload)
	if err != nil {
		return parallelSplitDraft{}, err
	}
	if err := insertExactCommand(replayView.Aggregate.Commands, command); err != nil {
		return parallelSplitDraft{}, err
	}
	if _, err := ReplayRoutePaths(replayView, command); err != nil {
		return parallelSplitDraft{}, err
	}
	return parallelSplitDraft{
		view: replayView, command: command, sourcePathID: source.ID, scopeID: scopeID,
		reducerReservation: reducer.ID, outgoing: outgoing, producedPaths: produced, createdTargets: createdTargets,
	}, nil
}

func parallelEdgeIDs(edges []EdgeKey) []EdgeID {
	ids := make([]EdgeID, len(edges))
	for index := range edges {
		ids[index] = edges[index].ID
	}
	return ids
}

func exactParallelOutgoingEdges(templateRef, nodeID string, next model.Next) ([]EdgeKey, error) {
	if len(next) < 2 || len(next) > MaxOutgoingOrAllCandidates {
		return nil, fmt.Errorf("%w: outgoing degree %d", ErrParallelInputInvalid, len(next))
	}
	edges := make([]EdgeKey, 0, len(next))
	for outcome, target := range next {
		edge := EdgeKey{TemplateRef: templateRef, FromNodeID: nodeID, Outcome: outcome, ToNodeID: target}
		var err error
		edge.ID, err = EdgeIdentity(templateRef, nodeID, outcome, target)
		if err != nil {
			return nil, err
		}
		edges = append(edges, edge)
	}
	// The fan-out selection/materialization order is the canonical EdgeKey
	// tuple. Opaque hashes order stored identity sets only; they do not choose
	// the branch traversal order.
	slices.SortFunc(edges, compareParallelEdgeTuple)
	for index := 1; index < len(edges); index++ {
		if compareParallelEdgeTuple(edges[index-1], edges[index]) == 0 || edges[index-1].ID == edges[index].ID {
			return nil, fmt.Errorf("%w: duplicate outgoing edge", ErrParallelInputInvalid)
		}
	}
	return edges, nil
}

func compareParallelEdgeTuple(a, b EdgeKey) int {
	if value := strings.Compare(a.FromNodeID, b.FromNodeID); value != 0 {
		return value
	}
	if value := strings.Compare(a.Outcome, b.Outcome); value != 0 {
		return value
	}
	return strings.Compare(a.ToNodeID, b.ToNodeID)
}

func deriveParallelReducerReservation(input *VerifiedParallelInput, runID, templateRef, forkNodeID, reducerNodeID string, scopeID ScopeID, branchIDs []EdgeID, eventSeq int64) (ActivationReservation, error) {
	node, ok := input.base.template.Nodes[reducerNodeID]
	if !ok {
		return ActivationReservation{}, fmt.Errorf("%w: reducer node %q is absent", ErrParallelInputInvalid, reducerNodeID)
	}
	policy := JoinAll
	if node.Join == model.JoinAny {
		policy = JoinAny
		if len(branchIDs) > MaxAnyCandidates {
			return ActivationReservation{}, fmt.Errorf("%w: any reducer candidate count %d exceeds %d", ErrParallelInputInvalid, len(branchIDs), MaxAnyCandidates)
		}
	}
	reservationID, err := ReservationIdentity(runID, reducerNodeID, scopeID, "", 1)
	if err != nil {
		return ActivationReservation{}, err
	}
	candidates := make([]CandidateRecord, 0, len(branchIDs))
	slots := make([]PossibleSlotRecord, 0, len(input.topology.incoming[reducerNodeID]))
	for _, branchID := range branchIDs {
		candidateID, candidateErr := CandidateIdentity(reservationID, CandidateScopeBranch, branchID)
		if candidateErr != nil {
			return ActivationReservation{}, candidateErr
		}
		candidateSlots := make([]PossibleSlotID, 0)
		for _, edge := range input.topology.incoming[reducerNodeID] {
			signature := input.topology.incomingSignature[edge.ID]
			if !signatureContainsBranch(signature, forkNodeID, branchID) {
				continue
			}
			slotID, slotErr := PossibleSlotIdentity(reservationID, candidateID, edge.FromNodeID, edge.ID, scopeID, branchID, 1)
			if slotErr != nil {
				return ActivationReservation{}, slotErr
			}
			candidateSlots = append(candidateSlots, slotID)
			slots = append(slots, PossibleSlotRecord{
				ID: slotID, ReservationID: reservationID, CandidateID: candidateID,
				SourceNodeID: edge.FromNodeID, SourceEdgeID: edge.ID, SourceScopeID: scopeID,
				SourceBranchEdgeID: branchID, Generation: 1,
			})
		}
		slices.Sort(candidateSlots)
		if len(candidateSlots) == 0 {
			return ActivationReservation{}, fmt.Errorf("%w: reducer branch %q has no exact possible slot", ErrParallelInputInvalid, branchID)
		}
		candidates = append(candidates, CandidateRecord{ID: candidateID, Kind: CandidateScopeBranch, MemberID: branchID, PossibleSlotIDs: candidateSlots})
	}
	slices.SortFunc(candidates, func(a, b CandidateRecord) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(slots, func(a, b PossibleSlotRecord) int { return strings.Compare(a.ID, b.ID) })
	return ActivationReservation{
		ID: reservationID, RunID: runID, NodeID: reducerNodeID, ScopeID: scopeID, Generation: 1,
		JoinPolicy: policy, IsReducing: true, ReducesScopeID: scopeID,
		Candidates: candidates, PossibleSlots: slots, State: ReservationOpen, EventSeq: eventSeq,
	}, nil
}

func deriveParallelBranchReservation(input *VerifiedParallelInput, runID, templateRef, forkNodeID string, branch EdgeKey, scopeID ScopeID, eventSeq int64) (ActivationReservation, error) {
	node, ok := input.base.template.Nodes[branch.ToNodeID]
	if !ok {
		return ActivationReservation{}, fmt.Errorf("%w: branch target %q is absent", ErrParallelInputInvalid, branch.ToNodeID)
	}
	reservationID, err := ReservationIdentity(runID, branch.ToNodeID, scopeID, branch.ID, 1)
	if err != nil {
		return ActivationReservation{}, err
	}
	wantSignature := input.topology.incomingSignature[branch.ID]
	candidates := make([]CandidateRecord, 0)
	slots := make([]PossibleSlotRecord, 0)
	for _, edge := range input.topology.incoming[branch.ToNodeID] {
		if !slices.Equal(wantSignature, input.topology.incomingSignature[edge.ID]) {
			continue
		}
		candidateID, candidateErr := CandidateIdentity(reservationID, CandidateInboundEdge, edge.ID)
		if candidateErr != nil {
			return ActivationReservation{}, candidateErr
		}
		slotID, slotErr := PossibleSlotIdentity(reservationID, candidateID, edge.FromNodeID, edge.ID, scopeID, branch.ID, 1)
		if slotErr != nil {
			return ActivationReservation{}, slotErr
		}
		candidates = append(candidates, CandidateRecord{ID: candidateID, Kind: CandidateInboundEdge, MemberID: edge.ID, PossibleSlotIDs: []PossibleSlotID{slotID}})
		slots = append(slots, PossibleSlotRecord{
			ID: slotID, ReservationID: reservationID, CandidateID: candidateID,
			SourceNodeID: edge.FromNodeID, SourceEdgeID: edge.ID, SourceScopeID: scopeID,
			SourceBranchEdgeID: branch.ID, Generation: 1,
		})
	}
	if len(candidates) == 0 {
		return ActivationReservation{}, fmt.Errorf("%w: branch target has no exact candidate", ErrParallelInputInvalid)
	}
	slices.SortFunc(candidates, func(a, b CandidateRecord) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(slots, func(a, b PossibleSlotRecord) int { return strings.Compare(a.ID, b.ID) })
	policy := JoinExclusive
	if len(candidates) > 1 {
		policy = JoinAll
		if node.Join == model.JoinAny {
			policy = JoinAny
			if len(candidates) > MaxAnyCandidates {
				return ActivationReservation{}, fmt.Errorf("%w: local any candidate count %d exceeds %d", ErrParallelInputInvalid, len(candidates), MaxAnyCandidates)
			}
		}
	}
	return ActivationReservation{
		ID: reservationID, RunID: runID, NodeID: branch.ToNodeID, ScopeID: scopeID, BranchEdgeID: branch.ID,
		Generation: 1, JoinPolicy: policy, Candidates: candidates, PossibleSlots: slots,
		State: ReservationOpen, EventSeq: eventSeq,
	}, nil
}

func signatureContainsBranch(signature []parallelScopeFrame, forkNodeID string, branchID EdgeID) bool {
	for index := len(signature) - 1; index >= 0; index-- {
		if signature[index].fork == forkNodeID {
			return signature[index].branch == branchID
		}
	}
	return false
}

func parallelCandidateForEdge(reservation ActivationReservation, edgeID EdgeID) (CandidateRecord, bool) {
	for _, candidate := range reservation.Candidates {
		if candidate.MemberID == edgeID && (candidate.Kind == CandidateInboundEdge || candidate.Kind == CandidateScopeBranch) {
			return candidate, true
		}
	}
	return CandidateRecord{}, false
}

func observedParallelCommands(view AggregateView, nodeID string, source PathRecord) (CommandRecord, CommandRecord, SideEffectIdentity, error) {
	const attempt uint64 = 1
	var existingPerform, existingSettle CommandRecord
	for _, candidate := range view.Commands {
		identity := candidate.Identity
		if identity.SourceActivationID != source.SourceActivation.ID || identity.SourceGeneration != source.SourceActivation.Generation {
			continue
		}
		switch identity.Kind {
		case CommandPerformAttempt:
			if identity.Attempt != attempt {
				return CommandRecord{}, CommandRecord{}, SideEffectIdentity{}, fmt.Errorf("%w: parallel activation has later perform attempt %d", ErrMutationInconsistent, identity.Attempt)
			}
			if existingPerform.ID != "" {
				return CommandRecord{}, CommandRecord{}, SideEffectIdentity{}, fmt.Errorf("%w: parallel attempt has multiple source commands", ErrMutationInconsistent)
			}
			existingPerform = cloneCommandRecord(candidate)
		case CommandSettleAttempt:
			if identity.Attempt != attempt {
				return CommandRecord{}, CommandRecord{}, SideEffectIdentity{}, fmt.Errorf("%w: parallel activation has later settlement attempt %d", ErrMutationInconsistent, identity.Attempt)
			}
			if existingSettle.ID != "" {
				return CommandRecord{}, CommandRecord{}, SideEffectIdentity{}, fmt.Errorf("%w: parallel attempt has multiple settlement commands", ErrMutationInconsistent)
			}
			existingSettle = cloneCommandRecord(candidate)
		}
	}
	for _, candidate := range view.SideEffects {
		if candidate.Kind == SideEffectAttempt && candidate.ActivationID == source.SourceActivation.ID && candidate.Attempt != attempt {
			return CommandRecord{}, CommandRecord{}, SideEffectIdentity{}, fmt.Errorf("%w: parallel activation has later side-effect attempt %d", ErrMutationInconsistent, candidate.Attempt)
		}
	}
	performPayload, err := json.Marshal(performAttemptPayload{
		TemplateRef: view.TemplateRef, TemplateSourceHash: view.TemplateSourceHash, NodeID: nodeID,
		SourceActivationID: source.SourceActivation.ID, SourceGeneration: source.SourceActivation.Generation, Attempt: attempt,
	})
	if err != nil {
		return CommandRecord{}, CommandRecord{}, SideEffectIdentity{}, err
	}
	expectedPerform, err := observedCommand(CommandIdentity{
		RunID: view.RunID, Kind: CommandPerformAttempt, PayloadSchema: 1,
		SourceActivationID: source.SourceActivation.ID, SourceGeneration: source.SourceActivation.Generation,
		Attempt: attempt, PlanDigest: payloadDigest(performPayload),
	}, performPayload)
	if err != nil {
		return CommandRecord{}, CommandRecord{}, SideEffectIdentity{}, err
	}
	perform := expectedPerform
	if existingPerform.ID != "" {
		if !exactParallelSettledCommand(existingPerform, expectedPerform) {
			return CommandRecord{}, CommandRecord{}, SideEffectIdentity{}, fmt.Errorf("%w: existing parallel perform command differs from deterministic observation", ErrMutationInconsistent)
		}
		perform = existingPerform
	}
	settlePayload, err := json.Marshal(settleAttemptObservationPayload{
		TemplateRef: view.TemplateRef, SourceCommandID: perform.ID, SourceActivationID: source.SourceActivation.ID,
		SourceGeneration: source.SourceActivation.Generation, Attempt: attempt, ResultCode: "parallel", Actor: "system:parallel",
	})
	if err != nil {
		return CommandRecord{}, CommandRecord{}, SideEffectIdentity{}, err
	}
	expectedSettle, err := observedCommand(CommandIdentity{
		RunID: view.RunID, Kind: CommandSettleAttempt, PayloadSchema: 1,
		SourceActivationID: source.SourceActivation.ID, SourceGeneration: source.SourceActivation.Generation,
		Attempt: attempt, InputDigest: perform.ID, PlanDigest: payloadDigest(settlePayload), ResultCode: "parallel",
	}, settlePayload)
	if err != nil {
		return CommandRecord{}, CommandRecord{}, SideEffectIdentity{}, err
	}
	settle := expectedSettle
	if existingSettle.ID != "" {
		if !exactParallelSettledCommand(existingSettle, expectedSettle) {
			return CommandRecord{}, CommandRecord{}, SideEffectIdentity{}, fmt.Errorf("%w: existing parallel settlement differs from deterministic observation", ErrMutationInconsistent)
		}
		settle = existingSettle
	}
	effect := SideEffectIdentity{Kind: SideEffectAttempt, RunID: view.RunID, ActivationID: source.SourceActivation.ID, Attempt: attempt, State: "observed"}
	effect.ID, err = AttemptIdentity(effect.RunID, effect.ActivationID, effect.Attempt)
	if err != nil {
		return CommandRecord{}, CommandRecord{}, SideEffectIdentity{}, err
	}
	if existing, ok := view.SideEffects[effect.ID]; ok && existing != effect {
		return CommandRecord{}, CommandRecord{}, SideEffectIdentity{}, fmt.Errorf("%w: existing parallel attempt side effect differs from deterministic observation", ErrMutationInconsistent)
	}
	return perform, settle, effect, nil
}

func exactParallelSettledCommand(existing, expected CommandRecord) bool {
	if existing.State != CommandObserved && existing.State != CommandReconciled {
		return false
	}
	existing.State = expected.State
	return exactExclusiveCommand(existing, expected)
}

func insertExactParallelSideEffect(effects map[string]SideEffectIdentity, effect SideEffectIdentity) error {
	if existing, ok := effects[effect.ID]; ok && existing != effect {
		return fmt.Errorf("%w: side-effect identity collision", ErrMutationInconsistent)
	}
	effects[effect.ID] = effect
	return nil
}

func validateParallelSplitConservation(post AggregateView, draft parallelSplitDraft) error {
	parent, ok := post.Routing.Paths[draft.sourcePathID]
	if !ok || parent.State != PathSplit || parent.Disposition == nil || parent.Disposition.ReasonCode != "parallel_split" || !slices.Equal(parent.ProducedPathIDs, materializedProducedIDs(post, parent.ID)) {
		return fmt.Errorf("%w: split parent does not own exact materialized children", ErrMutationInconsistent)
	}
	if len(parent.ProducedPathIDs) != len(draft.outgoing) {
		return fmt.Errorf("%w: split child count differs from exact outgoing set", ErrMutationInconsistent)
	}
	scope, ok := post.Routing.Scopes[draft.scopeID]
	if !ok || scope.State != ScopeOpen || scope.JoinReservationID != draft.reducerReservation || scope.ForkOutputPathID != parent.ID {
		return fmt.Errorf("%w: split scope/reducer authority is incomplete", ErrMutationInconsistent)
	}
	seenEdges := make(map[EdgeID]struct{}, len(draft.outgoing))
	for _, pathID := range parent.ProducedPathIDs {
		child, exists := post.Routing.Paths[pathID]
		if !exists || child.Kind != PathEdge || child.State != PathArrived || child.ParentPathID != parent.ID || child.Edge == nil || child.ScopeID != scope.ID || child.BranchEdgeID != child.Edge.ID {
			return fmt.Errorf("%w: split child %q is not one exact arrived branch", ErrMutationInconsistent, pathID)
		}
		if _, duplicate := seenEdges[child.Edge.ID]; duplicate {
			return fmt.Errorf("%w: split edge %q is duplicated", ErrMutationInconsistent, child.Edge.ID)
		}
		seenEdges[child.Edge.ID] = struct{}{}
		reservation, reservationOK := post.Routing.Reservations[child.TargetReservationID]
		if !reservationOK || reservation.State != ReservationOpen {
			return fmt.Errorf("%w: split child target reservation is not open", ErrMutationInconsistent)
		}
		candidate, candidateOK := parallelCandidateForEdge(reservation, child.Edge.ID)
		if !candidateOK || candidate.ID != child.CandidateID {
			return fmt.Errorf("%w: split child candidate differs from exact reservation", ErrMutationInconsistent)
		}
	}
	if len(seenEdges) != len(draft.outgoing) {
		return fmt.Errorf("%w: split token count is not conserved", ErrMutationInconsistent)
	}
	return nil
}
