package pathv1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
)

func validGenesisFixture(t *testing.T) AggregateView {
	t.Helper()
	runID, templateRef, sourceHash := "run-valid", strings.Repeat("a", 64), strings.Repeat("b", 64)
	root, _ := ScopeIdentity(runID, "", "", "", "", 1)
	reservationID, _ := ReservationIdentity(runID, "start", root, "", 1)
	inputDigest, _ := InputSetIdentity(nil)
	activationID, _ := ActivationIdentity(runID, reservationID, 1, inputDigest)
	outputID, _ := ActivationOutputIdentity(activationID, 1)
	identity := CommandIdentity{RunID: runID, Kind: CommandInitializeRouting, PayloadSchema: 1, InputDigest: "legacy", PlanDigest: "routing"}
	commandID, _ := CommandIdentityDigest(identity)
	payload := json.RawMessage(`{"genesis":true}`)
	sum := sha256.Sum256(payload)
	command := CommandRecord{ID: commandID, IdempotencyKey: CommandIdempotencyKey(identity.Kind, commandID), Identity: identity, Payload: payload, PayloadHash: hex.EncodeToString(sum[:]), State: CommandObserved}
	ref := ActivationRef{ID: activationID, Generation: 1}
	receiptID, _ := ActivationReceiptIdentity(activationID, reservationID, inputDigest, outputID, commandID, 1)
	receipt := ActivationReceipt{ID: receiptID, ActivationID: activationID, ReservationID: reservationID, InputSetDigest: inputDigest, OutputPathID: outputID, ScopeID: root, JoinPolicy: JoinExclusive, Result: ReceiptActivated, CommandID: commandID, EventSeq: 1}
	state := NewRoutingState()
	state.Scopes[root] = ScopeRecord{ID: root, RunID: runID, Generation: 1, ExpectedBranchEdgeIDs: []string{}, State: ScopeOpen, EventSeq: 1}
	state.Reservations[reservationID] = ActivationReservation{ID: reservationID, RunID: runID, NodeID: "start", ScopeID: root, Generation: 1, JoinPolicy: JoinExclusive, Candidates: []CandidateRecord{}, PossibleSlots: []PossibleSlotRecord{}, State: ReservationActivated, Activation: &ref, CommandID: commandID, EventSeq: 1}
	state.Activations[activationID] = ActivationRecord{ID: activationID, RunID: runID, Ref: ref, ReservationID: reservationID, InputPathIDs: []PathID{}, InputSetDigest: inputDigest, OutputPathID: outputID, Receipt: receipt, CommandID: commandID, EventSeq: 1}
	state.Paths[outputID] = PathRecord{ID: outputID, Kind: PathActivationOutput, State: PathEnded, SourceActivation: ref, ScopeID: root, CandidateLineage: []CandidateLineageFrame{}, CreatedSeq: 1, UpdatedSeq: 1}
	authority := &AggregateAuthority{RunID: runID, TemplateRef: templateRef, TemplateSourceHash: sourceHash, Genesis: GenesisAuthority{RootScopeID: root, StartNodeID: "start", ReservationID: reservationID, ActivationID: activationID, OutputPathID: outputID, Generation: 1}, Scopes: map[string]ScopeAuthority{root: {ID: root, Generation: 1, ExpectedBranchEdgeIDs: []string{}}}, Reservations: map[string]ReservationAuthority{reservationID: {ID: reservationID, NodeID: "start", ScopeID: root, Generation: 1, JoinPolicy: JoinExclusive, Candidates: []CandidateRecord{}, PossibleSlots: []PossibleSlotRecord{}}}}
	return AggregateView{RunID: runID, TemplateRef: templateRef, TemplateSourceHash: sourceHash, Authority: authority, Routing: &state, Commands: map[string]CommandRecord{commandID: command}, SideEffects: map[string]SideEffectIdentity{}, AdminRecords: map[string]PathV1AdminRecord{}, AdminResolutions: map[string]BlockResolution{}}
}

func cloneAuthority(in *AggregateAuthority) *AggregateAuthority {
	out := *in
	out.Scopes = make(map[string]ScopeAuthority, len(in.Scopes))
	for id, s := range in.Scopes {
		s.ExpectedBranchEdgeIDs = append([]string(nil), s.ExpectedBranchEdgeIDs...)
		out.Scopes[id] = s
	}
	out.Reservations = make(map[string]ReservationAuthority, len(in.Reservations))
	for id, r := range in.Reservations {
		r.Candidates = append([]CandidateRecord(nil), r.Candidates...)
		for n := range r.Candidates {
			r.Candidates[n].PossibleSlotIDs = append([]string(nil), r.Candidates[n].PossibleSlotIDs...)
		}
		r.PossibleSlots = append([]PossibleSlotRecord(nil), r.PossibleSlots...)
		out.Reservations[id] = r
	}
	return &out
}

func addOpenAuthorityReservation(t *testing.T, view *AggregateView, node string) string {
	t.Helper()
	root := view.Authority.Genesis.RootScopeID
	edgeID, _ := EdgeIdentity(view.TemplateRef, "start", "pass", node)
	reservationID, _ := ReservationIdentity(view.RunID, node, root, "", 1)
	candidateID, _ := CandidateIdentity(reservationID, CandidateInboundEdge, edgeID)
	slotID, _ := PossibleSlotIdentity(reservationID, candidateID, "start", edgeID, root, "", 1)
	slot := PossibleSlotRecord{ID: slotID, ReservationID: reservationID, CandidateID: candidateID, SourceNodeID: "start", SourceEdgeID: edgeID, SourceScopeID: root, Generation: 1}
	candidate := CandidateRecord{ID: candidateID, Kind: CandidateInboundEdge, MemberID: edgeID, PossibleSlotIDs: []string{slotID}}
	view.Authority.Reservations[reservationID] = ReservationAuthority{ID: reservationID, NodeID: node, ScopeID: root, Generation: 1, JoinPolicy: JoinExclusive, Candidates: []CandidateRecord{candidate}, PossibleSlots: []PossibleSlotRecord{slot}}
	view.Routing.Reservations[reservationID] = ActivationReservation{ID: reservationID, RunID: view.RunID, NodeID: node, ScopeID: root, Generation: 1, JoinPolicy: JoinExclusive, Candidates: []CandidateRecord{candidate}, PossibleSlots: []PossibleSlotRecord{slot}, State: ReservationOpen}
	return reservationID
}

func validOpenArrivalFixture(t *testing.T) (AggregateView, PathID, ReservationID) {
	t.Helper()
	view := validGenesisFixture(t)
	reservationID := addOpenAuthorityReservation(t, &view, "target")
	r := view.Routing.Reservations[reservationID]
	edge := EdgeKey{TemplateRef: view.TemplateRef, FromNodeID: "start", Outcome: "pass", ToNodeID: "target"}
	edge.ID, _ = EdgeIdentity(edge.TemplateRef, edge.FromNodeID, edge.Outcome, edge.ToNodeID)
	candidate := r.Candidates[0]
	parentID := view.Authority.Genesis.OutputPathID
	parent := view.Routing.Paths[parentID]
	pathID, _ := EdgePathIdentity(parent.SourceActivation.ID, parent.ID, edge.ID, r.ID, candidate.ID)
	arrivalID, _ := ArrivalIdentity(pathID, r.ID, candidate.ID)
	lineageID, _ := CandidateLineageIdentity("", r.ID, candidate.ID)
	route := makeTestCommand(t, CommandIdentity{RunID: view.RunID, Kind: CommandRoutePaths, PayloadSchema: 1, SourceActivationID: parent.SourceActivation.ID, SourceGeneration: parent.SourceActivation.Generation, SourcePathID: parent.ID, InputDigest: "settled", CauseDigest: "cause", PlanDigest: "exclusive", ResultCode: "pass"}, CommandObserved)
	view.Commands[route.ID] = route
	parent.State = PathRouted
	parent.ProducedPathIDs = []string{pathID}
	parent.UpdatedSeq = 2
	dispositionID, _ := DispositionReceiptIdentity(parent.ID, PathLive, PathRouted, "route", route.ID, "", 2)
	parent.Disposition = &DispositionReceipt{ID: dispositionID, PathID: parent.ID, FromState: PathLive, ToState: PathRouted, ReasonCode: "route", CommandID: route.ID, EventSeq: 2}
	view.Routing.Paths[parent.ID] = parent
	view.Routing.Paths[pathID] = PathRecord{ID: pathID, Kind: PathEdge, State: PathArrived, ParentPathID: parent.ID, SourceActivation: parent.SourceActivation, Edge: &edge, TargetReservationID: r.ID, CandidateID: candidate.ID, ScopeID: parent.ScopeID, CandidateLineage: []CandidateLineageFrame{{ID: lineageID, ReservationID: r.ID, CandidateID: candidate.ID}}, CandidateLineageID: lineageID, LineageDepth: 1, ArrivalID: arrivalID, ArrivedSeq: 2, CreatedSeq: 2, UpdatedSeq: 2}
	return view, pathID, reservationID
}

func activateOpenArrival(t *testing.T, view *AggregateView, pathID PathID, reservationID ReservationID) {
	t.Helper()
	r := view.Routing.Reservations[reservationID]
	inputDigest, _ := InputSetIdentity([]string{pathID})
	command := makeTestCommand(t, CommandIdentity{RunID: view.RunID, Kind: CommandActivateGeneration, PayloadSchema: 1, TargetReservationID: r.ID, TargetGeneration: r.Generation, InputDigest: "fold", CauseDigest: "cause", PlanDigest: "activate"}, CommandObserved)
	activationID, _ := ActivationIdentity(view.RunID, r.ID, r.Generation, inputDigest)
	outputID, _ := ActivationOutputIdentity(activationID, r.Generation)
	ref := ActivationRef{ID: activationID, Generation: r.Generation}
	receiptID, _ := ActivationReceiptIdentity(activationID, r.ID, inputDigest, outputID, command.ID, 3)
	receipt := ActivationReceipt{ID: receiptID, ActivationID: activationID, ReservationID: r.ID, InputSetDigest: inputDigest, OutputPathID: outputID, ScopeID: r.ScopeID, JoinPolicy: r.JoinPolicy, Result: ReceiptActivated, CommandID: command.ID, EventSeq: 3}
	p := view.Routing.Paths[pathID]
	p.State = PathConsumed
	p.ConsumedBy = &ref
	p.UpdatedSeq = 3
	dispositionID, _ := DispositionReceiptIdentity(p.ID, PathArrived, PathConsumed, "exclusive_winner", command.ID, "", 3)
	p.Disposition = &DispositionReceipt{ID: dispositionID, PathID: p.ID, FromState: PathArrived, ToState: PathConsumed, ReasonCode: "exclusive_winner", CommandID: command.ID, EventSeq: 3}
	view.Routing.Paths[p.ID] = p
	view.Routing.Paths[outputID] = PathRecord{ID: outputID, Kind: PathActivationOutput, State: PathEnded, SourceActivation: ref, ScopeID: r.ScopeID, CreatedSeq: 3, UpdatedSeq: 3}
	view.Routing.Activations[activationID] = ActivationRecord{ID: activationID, RunID: view.RunID, Ref: ref, ReservationID: r.ID, InputPathIDs: []string{p.ID}, InputSetDigest: inputDigest, OutputPathID: outputID, Receipt: receipt, CommandID: command.ID, EventSeq: 3}
	r.State = ReservationActivated
	r.Activation = &ref
	r.CommandID = command.ID
	r.EventSeq = 3
	view.Routing.Reservations[r.ID] = r
	view.Commands[command.ID] = command
}

func forgeActivatedInputEdge(t *testing.T, view *AggregateView, oldPathID PathID) PathID {
	t.Helper()
	p := view.Routing.Paths[oldPathID]
	edge := *p.Edge
	edge.Outcome = "forged"
	edge.ID, _ = EdgeIdentity(edge.TemplateRef, edge.FromNodeID, edge.Outcome, edge.ToNodeID)
	newPathID, _ := EdgePathIdentity(p.SourceActivation.ID, p.ParentPathID, edge.ID, p.TargetReservationID, p.CandidateID)
	p.ID = newPathID
	p.Edge = &edge
	p.ArrivalID, _ = ArrivalIdentity(newPathID, p.TargetReservationID, p.CandidateID)
	p.Disposition.PathID = newPathID
	p.Disposition.ID, _ = DispositionReceiptIdentity(newPathID, p.Disposition.FromState, p.Disposition.ToState, p.Disposition.ReasonCode, p.Disposition.CommandID, p.Disposition.AdminRecordID, uint64(p.Disposition.EventSeq))
	oldActivation := view.Routing.Activations[p.ConsumedBy.ID]
	inputDigest, _ := InputSetIdentity([]string{newPathID})
	newActivationID, _ := ActivationIdentity(view.RunID, oldActivation.ReservationID, oldActivation.Ref.Generation, inputDigest)
	newOutputID, _ := ActivationOutputIdentity(newActivationID, oldActivation.Ref.Generation)
	newRef := ActivationRef{ID: newActivationID, Generation: oldActivation.Ref.Generation}
	p.ConsumedBy = &newRef
	delete(view.Routing.Paths, oldPathID)
	view.Routing.Paths[newPathID] = p
	parent := view.Routing.Paths[p.ParentPathID]
	parent.ProducedPathIDs = []string{newPathID}
	view.Routing.Paths[parent.ID] = parent
	oldOutput := view.Routing.Paths[oldActivation.OutputPathID]
	delete(view.Routing.Paths, oldActivation.OutputPathID)
	oldOutput.ID = newOutputID
	oldOutput.SourceActivation = newRef
	view.Routing.Paths[newOutputID] = oldOutput
	delete(view.Routing.Activations, oldActivation.ID)
	oldActivation.ID = newActivationID
	oldActivation.Ref = newRef
	oldActivation.InputPathIDs = []string{newPathID}
	oldActivation.InputSetDigest = inputDigest
	oldActivation.OutputPathID = newOutputID
	oldActivation.Receipt.ID, _ = ActivationReceiptIdentity(newActivationID, oldActivation.ReservationID, inputDigest, newOutputID, oldActivation.CommandID, uint64(oldActivation.EventSeq))
	oldActivation.Receipt.ActivationID = newActivationID
	oldActivation.Receipt.InputSetDigest = inputDigest
	oldActivation.Receipt.OutputPathID = newOutputID
	view.Routing.Activations[newActivationID] = oldActivation
	r := view.Routing.Reservations[oldActivation.ReservationID]
	r.Activation = &newRef
	view.Routing.Reservations[r.ID] = r
	return newPathID
}

func addWideOpenReservation(t *testing.T, view *AggregateView, node string, policy JoinPolicy, count int) string {
	t.Helper()
	root := view.Authority.Genesis.RootScopeID
	reservationID, _ := ReservationIdentity(view.RunID, node, root, "", 1)
	candidates := make([]CandidateRecord, 0, count)
	slots := make([]PossibleSlotRecord, 0, count)
	for n := 0; n < count; n++ {
		outcome := fmt.Sprintf("o-%04d", n)
		edgeID, _ := EdgeIdentity(view.TemplateRef, "start", outcome, node)
		candidateID, _ := CandidateIdentity(reservationID, CandidateInboundEdge, edgeID)
		slotID, _ := PossibleSlotIdentity(reservationID, candidateID, "start", edgeID, root, "", 1)
		candidates = append(candidates, CandidateRecord{ID: candidateID, Kind: CandidateInboundEdge, MemberID: edgeID, PossibleSlotIDs: []string{slotID}})
		slots = append(slots, PossibleSlotRecord{ID: slotID, ReservationID: reservationID, CandidateID: candidateID, SourceNodeID: "start", SourceEdgeID: edgeID, SourceScopeID: root, Generation: 1})
	}
	slices.SortFunc(candidates, func(a, b CandidateRecord) int { return cmpString(a.ID, b.ID) })
	slices.SortFunc(slots, func(a, b PossibleSlotRecord) int { return cmpString(a.ID, b.ID) })
	view.Authority.Reservations[reservationID] = ReservationAuthority{ID: reservationID, NodeID: node, ScopeID: root, Generation: 1, JoinPolicy: policy, Candidates: candidates, PossibleSlots: slots}
	view.Routing.Reservations[reservationID] = ActivationReservation{ID: reservationID, RunID: view.RunID, NodeID: node, ScopeID: root, Generation: 1, JoinPolicy: policy, Candidates: candidates, PossibleSlots: slots, State: ReservationOpen}
	return reservationID
}

func makeTestCommand(t *testing.T, identity CommandIdentity, state CommandState) CommandRecord {
	t.Helper()
	id, err := CommandIdentityDigest(identity)
	if err != nil {
		t.Fatal(err)
	}
	payload := json.RawMessage(`{"plan":true}`)
	sum := sha256.Sum256(payload)
	return CommandRecord{ID: id, IdempotencyKey: CommandIdempotencyKey(identity.Kind, id), Identity: identity, Payload: payload, PayloadHash: hex.EncodeToString(sum[:]), State: state}
}

func validAnyFixture(t *testing.T) AggregateView {
	t.Helper()
	view := validGenesisFixture(t)
	g := view.Authority.Genesis
	parent := view.Routing.Paths[g.OutputPathID]
	edges := []EdgeKey{{TemplateRef: view.TemplateRef, FromNodeID: "start", Outcome: "a", ToNodeID: "join"}, {TemplateRef: view.TemplateRef, FromNodeID: "start", Outcome: "b", ToNodeID: "join"}}
	for n := range edges {
		edges[n].ID, _ = EdgeIdentity(view.TemplateRef, edges[n].FromNodeID, edges[n].Outcome, edges[n].ToNodeID)
	}
	slices.SortFunc(edges, func(a, b EdgeKey) int { return cmpString(a.ID, b.ID) })
	scopeID, _ := ScopeIdentity(view.RunID, g.RootScopeID, "", g.ActivationID, g.OutputPathID, 1)
	reservationID, _ := ReservationIdentity(view.RunID, "join", scopeID, "", 1)
	type branch struct {
		edge      EdgeKey
		candidate CandidateRecord
		slot      PossibleSlotRecord
		path      PathRecord
	}
	branches := make([]branch, 0, 2)
	for _, edge := range edges {
		candidateID, _ := CandidateIdentity(reservationID, CandidateScopeBranch, edge.ID)
		slotID, _ := PossibleSlotIdentity(reservationID, candidateID, "start", edge.ID, scopeID, edge.ID, 1)
		frameID, _ := CandidateLineageIdentity("", reservationID, candidateID)
		pathID, _ := EdgePathIdentity(g.ActivationID, g.OutputPathID, edge.ID, reservationID, candidateID)
		arrivalID, _ := ArrivalIdentity(pathID, reservationID, candidateID)
		candidate := CandidateRecord{ID: candidateID, Kind: CandidateScopeBranch, MemberID: edge.ID, PossibleSlotIDs: []string{slotID}}
		slot := PossibleSlotRecord{ID: slotID, ReservationID: reservationID, CandidateID: candidateID, SourceNodeID: "start", SourceEdgeID: edge.ID, SourceScopeID: scopeID, SourceBranchEdgeID: edge.ID, Generation: 1}
		path := PathRecord{ID: pathID, Kind: PathEdge, State: PathArrived, ParentPathID: g.OutputPathID, SourceActivation: ActivationRef{ID: g.ActivationID, Generation: 1}, Edge: &edge, TargetReservationID: reservationID, CandidateID: candidateID, ScopeID: scopeID, BranchEdgeID: edge.ID, CandidateLineage: []CandidateLineageFrame{{ID: frameID, ReservationID: reservationID, CandidateID: candidateID}}, CandidateLineageID: frameID, LineageDepth: 1, ArrivalID: arrivalID, ArrivedSeq: 2, CreatedSeq: 2, UpdatedSeq: 2}
		branches = append(branches, branch{edge: edge, candidate: candidate, slot: slot, path: path})
	}
	slices.SortFunc(branches, func(a, b branch) int { return cmpString(a.candidate.ID, b.candidate.ID) })
	candidates := []CandidateRecord{branches[0].candidate, branches[1].candidate}
	slots := []PossibleSlotRecord{branches[0].slot, branches[1].slot}
	slices.SortFunc(slots, func(a, b PossibleSlotRecord) int { return cmpString(a.ID, b.ID) })
	winnerIndex := 0
	if branches[1].path.ID < branches[0].path.ID {
		winnerIndex = 1
	}
	loserIndex := 1 - winnerIndex
	winner := branches[winnerIndex].path
	loser := branches[loserIndex].path
	inputDigest, _ := InputSetIdentity([]string{winner.ID})
	activationID, _ := ActivationIdentity(view.RunID, reservationID, 1, inputDigest)
	outputID, _ := ActivationOutputIdentity(activationID, 1)
	activationRef := ActivationRef{ID: activationID, Generation: 1}
	route := makeTestCommand(t, CommandIdentity{RunID: view.RunID, Kind: CommandRoutePaths, PayloadSchema: 1, SourceActivationID: g.ActivationID, SourceGeneration: 1, SourcePathID: g.OutputPathID, InputDigest: "settled", CauseDigest: "cause", PlanDigest: "split", ResultCode: "parallel"}, CommandObserved)
	view.Commands[route.ID] = route
	activate := makeTestCommand(t, CommandIdentity{RunID: view.RunID, Kind: CommandActivateGeneration, PayloadSchema: 1, TargetReservationID: reservationID, TargetGeneration: 1, InputDigest: "fold", CauseDigest: "cause", PlanDigest: "any"}, CommandObserved)
	view.Commands[activate.ID] = activate
	parent.State = PathSplit
	parent.ProducedPathIDs = []string{branches[0].path.ID, branches[1].path.ID}
	slices.Sort(parent.ProducedPathIDs)
	parent.UpdatedSeq = 2
	dispID, _ := DispositionReceiptIdentity(parent.ID, PathLive, PathSplit, "parallel_split", route.ID, "", 2)
	parent.Disposition = &DispositionReceipt{ID: dispID, PathID: parent.ID, FromState: PathLive, ToState: PathSplit, ReasonCode: "parallel_split", CommandID: route.ID, EventSeq: 2}
	view.Routing.Paths[parent.ID] = parent
	winner.State = PathConsumed
	winner.ConsumedBy = &activationRef
	winner.UpdatedSeq = 3
	winnerDisp, _ := DispositionReceiptIdentity(winner.ID, PathArrived, PathConsumed, "any_winner", activate.ID, "", 3)
	winner.Disposition = &DispositionReceipt{ID: winnerDisp, PathID: winner.ID, FromState: PathArrived, ToState: PathConsumed, ReasonCode: "any_winner", CommandID: activate.ID, EventSeq: 3}
	detachmentKey, _ := DetachmentKeyIdentity(reservationID, loser.CandidateID)
	detachmentID, _ := DetachmentIdentity(reservationID, loser.CandidateID, winner.ID, 3)
	detachment := DetachmentRecord{ID: detachmentID, Key: DetachmentKeyRecord{ID: detachmentKey, ReservationID: reservationID, CandidateID: loser.CandidateID}, ReservationID: reservationID, CandidateID: loser.CandidateID, WinnerPathID: winner.ID, JoinActivation: activationRef, ReasonCode: "any_loser", CommandID: activate.ID, ActivatedSeq: 3, EventSeq: 3, Actor: "system"}
	setID, _ := DetachmentSetIdentity("", detachmentID)
	loser.State = PathDetachedSink
	loser.DetachmentSetID = setID
	loser.UpdatedSeq = 3
	loserDisp, _ := DispositionReceiptIdentity(loser.ID, PathArrived, PathDetachedSink, "pre_arrived_any_loser", activate.ID, "", 3)
	loser.Disposition = &DispositionReceipt{ID: loserDisp, PathID: loser.ID, FromState: PathArrived, ToState: PathDetachedSink, ReasonCode: "pre_arrived_any_loser", CommandID: activate.ID, EventSeq: 3}
	loser.DetachedSink = &DetachedSinkReceipt{DetachmentID: detachmentID, CommandID: activate.ID, ReasonCode: "pre_arrived_any_loser", EventSeq: 3}
	view.Routing.Paths[winner.ID] = winner
	view.Routing.Paths[loser.ID] = loser
	receiptID, _ := ActivationReceiptIdentity(activationID, reservationID, inputDigest, outputID, activate.ID, 3)
	receipt := ActivationReceipt{ID: receiptID, ActivationID: activationID, ReservationID: reservationID, InputSetDigest: inputDigest, OutputPathID: outputID, ScopeID: g.RootScopeID, ReducedScopeID: scopeID, JoinPolicy: JoinAny, Result: ReceiptActivated, CommandID: activate.ID, EventSeq: 3}
	view.Routing.Paths[outputID] = PathRecord{ID: outputID, Kind: PathActivationOutput, State: PathEnded, SourceActivation: activationRef, ScopeID: g.RootScopeID, CandidateLineage: []CandidateLineageFrame{}, CreatedSeq: 3, UpdatedSeq: 3}
	view.Routing.Activations[activationID] = ActivationRecord{ID: activationID, RunID: view.RunID, Ref: activationRef, ReservationID: reservationID, InputPathIDs: []string{winner.ID}, InputSetDigest: inputDigest, OutputPathID: outputID, Receipt: receipt, CommandID: activate.ID, EventSeq: 3}
	view.Routing.Reservations[reservationID] = ActivationReservation{ID: reservationID, RunID: view.RunID, NodeID: "join", ScopeID: scopeID, Generation: 1, JoinPolicy: JoinAny, IsReducing: true, ReducesScopeID: scopeID, Candidates: candidates, PossibleSlots: slots, State: ReservationActivated, Activation: &activationRef, CommandID: activate.ID, EventSeq: 3}
	branchesIDs := []string{edges[0].ID, edges[1].ID}
	slices.Sort(branchesIDs)
	view.Routing.Scopes[scopeID] = ScopeRecord{ID: scopeID, RunID: view.RunID, ParentScopeID: g.RootScopeID, ForkActivationID: g.ActivationID, ForkOutputPathID: g.OutputPathID, Generation: 1, ExpectedBranchEdgeIDs: branchesIDs, JoinNodeID: "join", JoinReservationID: reservationID, State: ScopeClosedActivated, CloseReason: ScopeCloseAny, ClosedByCommandID: activate.ID, EventSeq: 3}
	view.Routing.Detachments[detachmentKey] = detachment
	view.Routing.DetachmentSets[setID] = DetachmentSetRecord{ID: setID, DetachmentID: detachmentID}
	view.Authority = cloneAuthority(view.Authority)
	view.Authority.Scopes[scopeID] = ScopeAuthority{ID: scopeID, ParentScopeID: g.RootScopeID, ForkActivationID: g.ActivationID, ForkOutputPathID: g.OutputPathID, Generation: 1, ExpectedBranchEdgeIDs: branchesIDs, JoinNodeID: "join", JoinReservationID: reservationID}
	view.Authority.Reservations[reservationID] = ReservationAuthority{ID: reservationID, NodeID: "join", ScopeID: scopeID, Generation: 1, JoinPolicy: JoinAny, IsReducing: true, ReducesScopeID: scopeID, Candidates: candidates, PossibleSlots: slots}
	return view
}

func validSlowAnyFixture(t *testing.T, failBeforeSink bool) AggregateView {
	t.Helper()
	view := validAnyFixture(t)
	g := view.Authority.Genesis
	key, d, oldLoserID := anyLoser(view)
	r := view.Routing.Reservations[d.ReservationID]
	scope := view.Routing.Scopes[r.ReducesScopeID]
	anyActivation := view.Routing.Activations[r.Activation.ID]
	winner := view.Routing.Paths[anyActivation.InputPathIDs[0]]
	oldLoser := view.Routing.Paths[oldLoserID]
	delete(view.Routing.Paths, oldLoserID)
	delete(view.Routing.Detachments, key)
	for id := range view.Routing.DetachmentSets {
		delete(view.Routing.DetachmentSets, id)
	}
	localReservationID, _ := ReservationIdentity(view.RunID, "join", scope.ID, oldLoser.Edge.ID, 1)
	localCandidateID, _ := CandidateIdentity(localReservationID, CandidateInboundEdge, oldLoser.Edge.ID)
	localSlotID, _ := PossibleSlotIdentity(localReservationID, localCandidateID, "start", oldLoser.Edge.ID, scope.ID, oldLoser.Edge.ID, 1)
	localCandidate := CandidateRecord{ID: localCandidateID, Kind: CandidateInboundEdge, MemberID: oldLoser.Edge.ID, PossibleSlotIDs: []string{localSlotID}}
	localSlot := PossibleSlotRecord{ID: localSlotID, ReservationID: localReservationID, CandidateID: localCandidateID, SourceNodeID: "start", SourceEdgeID: oldLoser.Edge.ID, SourceScopeID: scope.ID, SourceBranchEdgeID: oldLoser.Edge.ID, Generation: 1}
	outerFrame := oldLoser.CandidateLineage[0]
	localFrameID, _ := CandidateLineageIdentity(outerFrame.ID, localReservationID, localCandidateID)
	slowPathID, _ := EdgePathIdentity(g.ActivationID, g.OutputPathID, oldLoser.Edge.ID, localReservationID, localCandidateID)
	slowArrival, _ := ArrivalIdentity(slowPathID, localReservationID, localCandidateID)
	slowPath := oldLoser
	slowPath.ID = slowPathID
	slowPath.State = PathConsumed
	slowPath.TargetReservationID = localReservationID
	slowPath.CandidateID = localCandidateID
	slowPath.CandidateLineage = []CandidateLineageFrame{outerFrame, {ID: localFrameID, ParentLineageID: outerFrame.ID, ReservationID: localReservationID, CandidateID: localCandidateID}}
	slowPath.CandidateLineageID = localFrameID
	slowPath.LineageDepth = 2
	slowPath.ArrivalID = slowArrival
	slowPath.DetachmentSetID = ""
	slowPath.DetachedSink = nil
	slowPath.CreatedSeq = 2
	slowPath.ArrivedSeq = 2
	slowPath.UpdatedSeq = 3
	localInput, _ := InputSetIdentity([]string{slowPathID})
	localActivationID, _ := ActivationIdentity(view.RunID, localReservationID, 1, localInput)
	localOutputID, _ := ActivationOutputIdentity(localActivationID, 1)
	localRef := ActivationRef{ID: localActivationID, Generation: 1}
	slowPath.ConsumedBy = &localRef
	localCommand := makeTestCommand(t, CommandIdentity{RunID: view.RunID, Kind: CommandActivateGeneration, PayloadSchema: 1, TargetReservationID: localReservationID, TargetGeneration: 1, InputDigest: "fold-slow", CauseDigest: "cause", PlanDigest: "activate-slow"}, CommandObserved)
	view.Commands[localCommand.ID] = localCommand
	slowDisp, _ := DispositionReceiptIdentity(slowPath.ID, PathArrived, PathConsumed, "local_consume", localCommand.ID, "", 3)
	slowPath.Disposition = &DispositionReceipt{ID: slowDisp, PathID: slowPath.ID, FromState: PathArrived, ToState: PathConsumed, ReasonCode: "local_consume", CommandID: localCommand.ID, EventSeq: 3}
	view.Routing.Paths[slowPath.ID] = slowPath
	localReceiptID, _ := ActivationReceiptIdentity(localActivationID, localReservationID, localInput, localOutputID, localCommand.ID, 3)
	localReceipt := ActivationReceipt{ID: localReceiptID, ActivationID: localActivationID, ReservationID: localReservationID, InputSetDigest: localInput, OutputPathID: localOutputID, ScopeID: scope.ID, BranchEdgeID: oldLoser.Edge.ID, JoinPolicy: JoinExclusive, Result: ReceiptActivated, CommandID: localCommand.ID, EventSeq: 3}
	localOutput := PathRecord{ID: localOutputID, Kind: PathActivationOutput, State: PathLive, SourceActivation: localRef, ScopeID: scope.ID, BranchEdgeID: oldLoser.Edge.ID, CandidateLineage: []CandidateLineageFrame{outerFrame}, CandidateLineageID: outerFrame.ID, LineageDepth: 1, CreatedSeq: 3, UpdatedSeq: 3}
	view.Routing.Activations[localActivationID] = ActivationRecord{ID: localActivationID, RunID: view.RunID, Ref: localRef, ReservationID: localReservationID, InputPathIDs: []string{slowPath.ID}, InputSetDigest: localInput, OutputPathID: localOutputID, Receipt: localReceipt, CommandID: localCommand.ID, EventSeq: 3}
	view.Routing.Reservations[localReservationID] = ActivationReservation{ID: localReservationID, RunID: view.RunID, NodeID: "join", ScopeID: scope.ID, BranchEdgeID: oldLoser.Edge.ID, Generation: 1, JoinPolicy: JoinExclusive, Candidates: []CandidateRecord{localCandidate}, PossibleSlots: []PossibleSlotRecord{localSlot}, State: ReservationActivated, Activation: &localRef, CommandID: localCommand.ID, EventSeq: 3}
	parent := view.Routing.Paths[g.OutputPathID]
	for n, id := range parent.ProducedPathIDs {
		if id == oldLoserID {
			parent.ProducedPathIDs[n] = slowPath.ID
		}
	}
	slices.Sort(parent.ProducedPathIDs)
	view.Routing.Paths[parent.ID] = parent
	lateEdge := EdgeKey{TemplateRef: view.TemplateRef, FromNodeID: "join", Outcome: "continue", ToNodeID: "join"}
	lateEdge.ID, _ = EdgeIdentity(lateEdge.TemplateRef, lateEdge.FromNodeID, lateEdge.Outcome, lateEdge.ToNodeID)
	lateSlotID, _ := PossibleSlotIdentity(r.ID, d.CandidateID, "join", lateEdge.ID, scope.ID, oldLoser.Edge.ID, 1)
	lateSlot := PossibleSlotRecord{ID: lateSlotID, ReservationID: r.ID, CandidateID: d.CandidateID, SourceNodeID: "join", SourceEdgeID: lateEdge.ID, SourceScopeID: scope.ID, SourceBranchEdgeID: oldLoser.Edge.ID, Generation: 1}
	for n, candidate := range r.Candidates {
		if candidate.ID == d.CandidateID {
			r.Candidates[n].PossibleSlotIDs = []string{lateSlotID}
		}
	}
	for n, slot := range r.PossibleSlots {
		if slot.CandidateID == d.CandidateID {
			r.PossibleSlots[n] = lateSlot
		}
	}
	slices.SortFunc(r.PossibleSlots, func(a, b PossibleSlotRecord) int { return cmpString(a.ID, b.ID) })
	// Move the any winner event after the slow local activation.
	r.EventSeq = 4
	scope.EventSeq = 4
	anyActivation.EventSeq = 4
	anyActivation.Receipt.EventSeq = 4
	anyActivation.Receipt.ID, _ = ActivationReceiptIdentity(anyActivation.ID, r.ID, anyActivation.InputSetDigest, anyActivation.OutputPathID, r.CommandID, 4)
	winner.UpdatedSeq = 4
	winner.Disposition.EventSeq = 4
	winner.Disposition.ID, _ = DispositionReceiptIdentity(winner.ID, PathArrived, PathConsumed, winner.Disposition.ReasonCode, r.CommandID, "", 4)
	view.Routing.Paths[winner.ID] = winner
	anyOutput := view.Routing.Paths[anyActivation.OutputPathID]
	anyOutput.CreatedSeq = 4
	anyOutput.UpdatedSeq = 4
	view.Routing.Paths[anyOutput.ID] = anyOutput
	d.ID, _ = DetachmentIdentity(r.ID, d.CandidateID, winner.ID, 4)
	d.ActivatedSeq = 4
	d.EventSeq = 4
	d.JoinActivation = *r.Activation
	setID, _ := DetachmentSetIdentity("", d.ID)
	view.Routing.Detachments[key] = d
	view.Routing.DetachmentSets[setID] = DetachmentSetRecord{ID: setID, DetachmentID: d.ID}
	view.Routing.Reservations[r.ID] = r
	view.Routing.Scopes[scope.ID] = scope
	view.Routing.Activations[anyActivation.ID] = anyActivation
	view.Authority = cloneAuthority(view.Authority)
	view.Authority.Reservations[localReservationID] = ReservationAuthority{ID: localReservationID, NodeID: "join", ScopeID: scope.ID, BranchEdgeID: oldLoser.Edge.ID, Generation: 1, JoinPolicy: JoinExclusive, Candidates: []CandidateRecord{localCandidate}, PossibleSlots: []PossibleSlotRecord{localSlot}}
	anyAuthority := view.Authority.Reservations[r.ID]
	anyAuthority.Candidates = r.Candidates
	anyAuthority.PossibleSlots = r.PossibleSlots
	view.Authority.Reservations[r.ID] = anyAuthority
	if failBeforeSink {
		failure := makeTestCommand(t, CommandIdentity{RunID: view.RunID, Kind: CommandSettleAttempt, PayloadSchema: 1, SourceActivationID: localActivationID, SourceGeneration: 1, Attempt: 1, InputDigest: "attempt", PlanDigest: "observe", ResultCode: "failed"}, CommandObserved)
		view.Commands[failure.ID] = failure
		localOutput.State = PathFailed
		localOutput.UpdatedSeq = 5
		dispID, _ := DispositionReceiptIdentity(localOutput.ID, PathLive, PathFailed, "performer_failed", failure.ID, "", 5)
		localOutput.Disposition = &DispositionReceipt{ID: dispID, PathID: localOutput.ID, FromState: PathLive, ToState: PathFailed, ReasonCode: "performer_failed", CommandID: failure.ID, EventSeq: 5}
		causeID, _ := CauseIdentity(localOutput.ID, TerminalFailed, "performer_failed", localActivationID, failure.ID, "", 5)
		localOutput.TerminalCauseID = causeID
		view.Routing.CauseRecords[causeID] = CauseRecord{ID: causeID, SourcePathID: localOutput.ID, TerminalKind: TerminalFailed, DispositionReason: "performer_failed", SourceActivationID: localActivationID, SourceCommandID: failure.ID, EventSeq: 5}
	} else {
		latePathID, _ := EdgePathIdentity(localActivationID, localOutputID, lateEdge.ID, r.ID, d.CandidateID)
		sink := makeTestCommand(t, CommandIdentity{RunID: view.RunID, Kind: CommandSettleDetachedSink, PayloadSchema: 1, SourcePathID: latePathID, TargetReservationID: r.ID, TargetGeneration: 1, InputDigest: setID, ResultCode: "detached"}, CommandObserved)
		view.Commands[sink.ID] = sink
		localOutput.State = PathRouted
		localOutput.ProducedPathIDs = []string{latePathID}
		localOutput.UpdatedSeq = 5
		outDisp, _ := DispositionReceiptIdentity(localOutput.ID, PathLive, PathRouted, "late_detached_route", sink.ID, "", 5)
		localOutput.Disposition = &DispositionReceipt{ID: outDisp, PathID: localOutput.ID, FromState: PathLive, ToState: PathRouted, ReasonCode: "late_detached_route", CommandID: sink.ID, EventSeq: 5}
		lateFrameID, _ := CandidateLineageIdentity(outerFrame.ID, r.ID, d.CandidateID)
		arrivalID, _ := ArrivalIdentity(latePathID, r.ID, d.CandidateID)
		sinkDisp, _ := DispositionReceiptIdentity(latePathID, PathArrived, PathDetachedSink, "late_any_arrival", sink.ID, "", 5)
		view.Routing.Paths[latePathID] = PathRecord{ID: latePathID, Kind: PathEdge, State: PathDetachedSink, ParentPathID: localOutputID, SourceActivation: localRef, Edge: &lateEdge, TargetReservationID: r.ID, CandidateID: d.CandidateID, ScopeID: scope.ID, BranchEdgeID: oldLoser.Edge.ID, CandidateLineage: []CandidateLineageFrame{outerFrame, {ID: lateFrameID, ParentLineageID: outerFrame.ID, ReservationID: r.ID, CandidateID: d.CandidateID}}, CandidateLineageID: lateFrameID, LineageDepth: 2, ArrivalID: arrivalID, ArrivedSeq: 5, Disposition: &DispositionReceipt{ID: sinkDisp, PathID: latePathID, FromState: PathArrived, ToState: PathDetachedSink, ReasonCode: "late_any_arrival", CommandID: sink.ID, EventSeq: 5}, DetachmentSetID: setID, DetachedSink: &DetachedSinkReceipt{DetachmentID: d.ID, CommandID: sink.ID, ReasonCode: "late_any_arrival", EventSeq: 5}, CreatedSeq: 5, UpdatedSeq: 5}
	}
	view.Routing.Paths[localOutput.ID] = localOutput
	return view
}

func validAllArrivedNonSuccessFixture(t *testing.T) AggregateView {
	t.Helper()
	view := validAnyFixture(t)
	key, d, loserID := anyLoser(view)
	r := view.Routing.Reservations[d.ReservationID]
	scope := view.Routing.Scopes[r.ReducesScopeID]
	activation := view.Routing.Activations[r.Activation.ID]
	winnerID := activation.InputPathIDs[0]
	winner := view.Routing.Paths[winnerID]
	loser := view.Routing.Paths[loserID]
	delete(view.Routing.Detachments, key)
	for id := range view.Routing.DetachmentSets {
		delete(view.Routing.DetachmentSets, id)
	}
	delete(view.Routing.Activations, activation.ID)
	delete(view.Routing.Paths, activation.OutputPathID)
	winner.ConsumedBy = nil
	winner.UpdatedSeq = 3
	winnerDisp, _ := DispositionReceiptIdentity(winner.ID, PathArrived, PathConsumed, "join_non_success", r.CommandID, "", 3)
	winner.Disposition = &DispositionReceipt{ID: winnerDisp, PathID: winner.ID, FromState: PathArrived, ToState: PathConsumed, ReasonCode: "join_non_success", CommandID: r.CommandID, EventSeq: 3}
	view.Routing.Paths[winner.ID] = winner
	loser.State = PathFailed
	loser.DetachmentSetID = ""
	loser.DetachedSink = nil
	loser.UpdatedSeq = 3
	loserDisp, _ := DispositionReceiptIdentity(loser.ID, PathArrived, PathFailed, "branch_failed", r.CommandID, "", 3)
	loser.Disposition = &DispositionReceipt{ID: loserDisp, PathID: loser.ID, FromState: PathArrived, ToState: PathFailed, ReasonCode: "branch_failed", CommandID: r.CommandID, EventSeq: 3}
	leafID, _ := CauseIdentity(loser.ID, TerminalFailed, "branch_failed", loser.SourceActivation.ID, r.CommandID, "", 3)
	loser.TerminalCauseID = leafID
	view.Routing.Paths[loser.ID] = loser
	view.Routing.CauseRecords[leafID] = CauseRecord{ID: leafID, SourcePathID: loser.ID, TerminalKind: TerminalFailed, DispositionReason: "branch_failed", SourceActivationID: loser.SourceActivation.ID, SourceCommandID: r.CommandID, EventSeq: 3}
	leafDigest, _ := CauseSetIdentity([]string{leafID})
	view.Routing.CauseSets[leafDigest] = CauseSetRecord{Digest: leafDigest, CauseIDs: []string{leafID}}
	closureKey, _ := CandidateClosureKeyIdentity(r.ID, loser.CandidateID)
	closureID, _ := CandidateClosureIdentity(r.ID, loser.CandidateID, TerminalFailed, leafDigest)
	view.Routing.CandidateClosures[closureKey] = CandidateClosure{ID: closureID, Key: CandidateClosureKeyRecord{ID: closureKey, ReservationID: r.ID, CandidateID: loser.CandidateID}, TerminalKind: TerminalFailed, CauseDigest: leafDigest, CommandID: r.CommandID, EventSeq: 3}
	joinCauseID, _ := CauseIdentity("", TerminalFailed, "join_candidate_non_success", "", r.CommandID, "", 3)
	view.Routing.CauseRecords[joinCauseID] = CauseRecord{ID: joinCauseID, TerminalKind: TerminalFailed, DispositionReason: "join_candidate_non_success", SourceCommandID: r.CommandID, EventSeq: 3}
	finalIDs := []string{joinCauseID, leafID}
	slices.Sort(finalIDs)
	finalDigest, _ := CauseSetIdentity(finalIDs)
	view.Routing.CauseSets[finalDigest] = CauseSetRecord{Digest: finalDigest, CauseIDs: finalIDs}
	inputDigest, _ := InputSetIdentity([]string{winner.ID})
	receiptID, _ := ActivationReceiptIdentity("", r.ID, inputDigest, "", r.CommandID, 3)
	receipt := ActivationReceipt{ID: receiptID, ReservationID: r.ID, InputSetDigest: inputDigest, ScopeID: view.Authority.Genesis.RootScopeID, ReducedScopeID: scope.ID, JoinPolicy: JoinAll, Result: ReceiptClosedNoActivation, CauseDigest: finalDigest, CommandID: r.CommandID, EventSeq: 3}
	r.JoinPolicy = JoinAll
	r.State = ReservationClosedNoActivation
	r.Activation = nil
	r.CloseReceipt = &receipt
	r.ClosedReason = string(ScopeCloseCandidateNonSuccess)
	r.CauseDigest = finalDigest
	view.Routing.Reservations[r.ID] = r
	scope.State = ScopeClosedNoActivation
	scope.CloseReason = ScopeCloseCandidateNonSuccess
	view.Routing.Scopes[scope.ID] = scope
	view.Authority = cloneAuthority(view.Authority)
	authority := view.Authority.Reservations[r.ID]
	authority.JoinPolicy = JoinAll
	view.Authority.Reservations[r.ID] = authority
	return view
}

func reportHasCode(report InvariantReport, code string) bool {
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}

func TestMeasureAggregateActualStructureBounds(t *testing.T) {
	t.Parallel()
	state := NewRoutingState()
	for n := 0; n < MaxPathRecords; n++ {
		id := fmt.Sprintf("p-%06d", n)
		state.Paths[id] = PathRecord{ID: id}
	}
	view := AggregateView{Routing: &state}
	usage, err := MeasureAggregate(view)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Paths != MaxPathRecords || usage.Records != MaxPathRecords {
		t.Fatalf("max path structure usage = %#v", usage)
	}
	if err := usage.Validate(); err != nil {
		t.Fatalf("max path structure rejected: %v", err)
	}
	state.Paths["over"] = PathRecord{ID: "over"}
	usage, err = MeasureAggregate(view)
	if err != nil {
		t.Fatal(err)
	}
	var over *OverBudgetError
	if err := usage.Validate(); !errors.As(err, &over) || over.Limit != "paths" {
		t.Fatalf("path bound+1 = %v", err)
	}

	state = NewRoutingState()
	for n := 0; n < MaxPathRecords; n++ {
		id := fmt.Sprintf("p-%06d", n)
		state.Paths[id] = PathRecord{ID: id}
	}
	for n := 0; n < MaxRoutingRecords-MaxPathRecords; n++ {
		id := fmt.Sprintf("c-%06d", n)
		state.CauseRecords[id] = CauseRecord{ID: id}
	}
	view.Routing = &state
	usage, err = MeasureAggregate(view)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Records != MaxRoutingRecords {
		t.Fatalf("max record structure = %d", usage.Records)
	}
	if err := usage.Validate(); err != nil {
		t.Fatalf("max record structure rejected: %v", err)
	}
	state.CauseRecords["over"] = CauseRecord{ID: "over"}
	usage, _ = MeasureAggregate(view)
	if err := usage.Validate(); !errors.As(err, &over) || over.Limit != "records" {
		t.Fatalf("record bound+1 = %v", err)
	}
}

func TestMeasureAggregateActualReferenceAndListBounds(t *testing.T) {
	t.Parallel()
	state := NewRoutingState()
	remaining := MaxIDReferences
	for n := 0; remaining > 0; n++ {
		count := min(remaining, MaxRoutingList)
		ids := make([]string, count)
		for j := range ids {
			ids[j] = fmt.Sprintf("c-%03d-%04d", n, j)
		}
		id := fmt.Sprintf("p-%03d", n)
		state.Paths[id] = PathRecord{ID: id, ProducedPathIDs: ids}
		remaining -= count
	}
	view := AggregateView{Routing: &state}
	usage, err := MeasureAggregate(view)
	if err != nil {
		t.Fatal(err)
	}
	if usage.References != MaxIDReferences || usage.LargestList != MaxRoutingList {
		t.Fatalf("max reference structure = %#v", usage)
	}
	if err := usage.Validate(); err != nil {
		t.Fatalf("max reference structure rejected: %v", err)
	}
	last := state.Paths["p-097"]
	last.ProducedPathIDs = append(last.ProducedPathIDs, "over")
	state.Paths["p-097"] = last
	usage, _ = MeasureAggregate(view)
	var over *OverBudgetError
	if err := usage.Validate(); !errors.As(err, &over) || over.Limit != "references" {
		t.Fatalf("reference bound+1 = %v", err)
	}

	state = NewRoutingState()
	values := make([]string, MaxRoutingList+1)
	state.Paths["p"] = PathRecord{ProducedPathIDs: values}
	view.Routing = &state
	usage, _ = MeasureAggregate(view)
	if err := usage.Validate(); !errors.As(err, &over) || over.Limit != "list" {
		t.Fatalf("list bound+1 = %v", err)
	}
}

func TestUsageCounterCheckedOverflow(t *testing.T) {
	t.Parallel()
	u := usageCounter{references: math.MaxUint64}
	if err := u.add(&u.references, 1); err == nil {
		t.Fatal("uint64 counter overflow accepted")
	}
	if _, err := checkedUsageInt("test", uint64(math.MaxInt)+1); err == nil {
		t.Fatal("int counter overflow accepted")
	}
}

func TestCandidateClosureMaximumSlotsAndOpenDescendant(t *testing.T) {
	t.Parallel()
	slots := make([]string, MaxRoutingList)
	settled := make(map[string]SlotSettlement, MaxRoutingList)
	for n := range slots {
		id := fmt.Sprintf("slot-%04d", n)
		slots[n] = id
		settled[id] = SlotSettlement{CauseIDs: []string{fmt.Sprintf("cause-%04d", n)}, CauseKinds: []TerminalKind{TerminalImpossible}}
	}
	candidate := CandidateRecord{ID: "candidate", PossibleSlotIDs: slots}
	entry, causes, kind, err := FoldCandidateSlots("reservation", candidate, settled, false)
	if err != nil {
		t.Fatal(err)
	}
	if entry.FoldKind != string(TerminalImpossible) || kind != TerminalImpossible || len(causes) != MaxRoutingList {
		t.Fatalf("maximum closure = %#v causes=%d kind=%q", entry, len(causes), kind)
	}
	entry, _, _, err = FoldCandidateSlots("reservation", candidate, settled, true)
	if err != nil || entry.FoldKind != CandidateFoldOpen {
		t.Fatalf("open descendant closure = %#v, %v", entry, err)
	}
	extra := mapsCloneSettlements(settled)
	extra["bound+1"] = SlotSettlement{CauseIDs: []string{"x"}, CauseKinds: []TerminalKind{TerminalFailed}}
	if _, _, _, err := FoldCandidateSlots("reservation", candidate, extra, false); err == nil {
		t.Fatal("unknown bound+1 slot accepted")
	}
}

func mapsCloneSettlements(in map[string]SlotSettlement) map[string]SlotSettlement {
	out := make(map[string]SlotSettlement, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
}

func TestReservationFoldArrivedPlusNonSuccessCloses(t *testing.T) {
	t.Parallel()
	state := NewRoutingState()
	r := ActivationReservation{ID: "r", JoinPolicy: JoinAll, State: ReservationClosedNoActivation, ClosedReason: string(ScopeCloseCandidateNonSuccess), Candidates: []CandidateRecord{{ID: "arrived"}, {ID: "failed"}}}
	state.Reservations[r.ID] = r
	state.Paths["p"] = PathRecord{ID: "p", Kind: PathEdge, State: PathConsumed, TargetReservationID: "r", CandidateID: "arrived"}
	key, _ := CandidateClosureKeyIdentity("r", "failed")
	state.CandidateClosures[key] = CandidateClosure{Key: CandidateClosureKeyRecord{ID: key, ReservationID: "r", CandidateID: "failed"}, TerminalKind: TerminalFailed}
	i := &aggregateIndex{view: AggregateView{Routing: &state}, pathsByTarget: map[candidateKey][]PathID{{"r", "arrived"}: {"p"}}}
	if err := validateClosedReservationFold(i, r); err != nil {
		t.Fatalf("arrived + failed rejected: %v", err)
	}
	r.ClosedReason = string(ScopeCloseAllImpossible)
	if err := validateClosedReservationFold(i, r); err == nil {
		t.Fatal("arrived + failed accepted as all-impossible")
	}
}

func TestAggregateValidationCapsAdversarialDiagnostics(t *testing.T) {
	t.Parallel()
	view := validGenesisFixture(t)
	for n := 0; n < 2_000; n++ {
		id := fmt.Sprintf("p-%04d", n)
		view.Routing.Paths[id] = PathRecord{ID: "wrong", Kind: "bad", State: "bad", CreatedSeq: -1}
	}
	report := ValidateAggregate(view)
	if len(report.Diagnostics) != MaxInvariantDiagnostics || report.Suppressed == 0 {
		t.Fatalf("diagnostic cap = %d, suppressed=%d", len(report.Diagnostics), report.Suppressed)
	}
}

func TestAggregateCompletionFailsClosedOnIllegalEdgeState(t *testing.T) {
	t.Parallel()
	view := validGenesisFixture(t)
	view.Routing.Paths["edge"] = PathRecord{ID: "edge", Kind: PathEdge, State: PathLive}
	_, err := AssessAggregateCompletion(view)
	if !errors.Is(err, ErrAggregateInvalid) {
		t.Fatalf("illegal live edge completion = %v", err)
	}
}

func TestAggregateCompletionRejectsEveryUnsettledOwner(t *testing.T) {
	t.Parallel()
	t.Run("open reservation", func(t *testing.T) {
		view := validGenesisFixture(t)
		addOpenAuthorityReservation(t, &view, "target")
		if report := ValidateAggregate(view); !report.Valid() {
			t.Fatalf("fixture invalid: %#v", report.Diagnostics)
		}
		if _, err := AssessAggregateCompletion(view); !errors.Is(err, ErrAggregateUnsettled) {
			t.Fatalf("open reservation completion = %v", err)
		}
	})
	t.Run("active side effect", func(t *testing.T) {
		view := validGenesisFixture(t)
		effect := SideEffectIdentity{Kind: SideEffectWait, RunID: view.RunID, ActivationID: view.Authority.Genesis.ActivationID, Attempt: 1, WaitKind: "human", State: "pending"}
		effect.ID, _ = WaitIdentity(effect.RunID, effect.ActivationID, effect.Attempt, effect.WaitKind)
		view.SideEffects[effect.ID] = effect
		if report := ValidateAggregate(view); !report.Valid() {
			t.Fatalf("fixture invalid: %#v", report.Diagnostics)
		}
		if _, err := AssessAggregateCompletion(view); !errors.Is(err, ErrAggregateUnsettled) {
			t.Fatalf("active effect completion = %v", err)
		}
	})
}

func TestValidFullyLinkedGenesisTerminates(t *testing.T) {
	t.Parallel()
	view := validGenesisFixture(t)
	report := ValidateAggregate(view)
	if !report.Valid() {
		t.Fatalf("valid genesis diagnostics: %#v", report.Diagnostics)
	}
	completion, err := AssessAggregateCompletion(view)
	if err != nil {
		t.Fatal(err)
	}
	empty, _ := CauseSetIdentity(nil)
	if completion.Result != "completed" || completion.TerminalCauseDigest != empty {
		t.Fatalf("completion = %#v", completion)
	}
}

func TestValidAnyPreArrivedLoserTerminates(t *testing.T) {
	t.Parallel()
	view := validAnyFixture(t)
	report := ValidateAggregate(view)
	if !report.Valid() {
		t.Fatalf("valid any diagnostics: %#v", report.Diagnostics)
	}
	completion, err := AssessAggregateCompletion(view)
	if err != nil {
		t.Fatal(err)
	}
	if completion.Result != "completed" {
		t.Fatalf("completion = %#v", completion)
	}
}

func TestSlowAnyLateSinkAndFailureBeforeSink(t *testing.T) {
	t.Parallel()
	t.Run("late sink", func(t *testing.T) {
		view := validSlowAnyFixture(t, false)
		report := ValidateAggregate(view)
		if !report.Valid() {
			t.Fatalf("late sink diagnostics: %#v", report.Diagnostics)
		}
		completion, err := AssessAggregateCompletion(view)
		if err != nil || completion.Result != "completed" {
			t.Fatalf("completion=%#v err=%v", completion, err)
		}
	})
	t.Run("failure before sink", func(t *testing.T) {
		view := validSlowAnyFixture(t, true)
		report := ValidateAggregate(view)
		if !report.Valid() {
			t.Fatalf("failure diagnostics: %#v", report.Diagnostics)
		}
		completion, err := AssessAggregateCompletion(view)
		if err != nil || completion.Result != "failed" {
			t.Fatalf("completion=%#v err=%v", completion, err)
		}
	})
}

func anyLoser(view AggregateView) (DetachmentKey, DetachmentRecord, PathID) {
	for key, d := range view.Routing.Detachments {
		for _, id := range view.Routing.Paths {
			if id.CandidateID == d.CandidateID && id.TargetReservationID == d.ReservationID {
				return key, d, id.ID
			}
		}
	}
	return "", DetachmentRecord{}, ""
}

func TestAnyDetachmentRejectsPartialWrongAndUnsettledPostStates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, code string
		mutate     func(*AggregateView)
	}{
		{"missing loser detachment", "detachment_loser_missing", func(v *AggregateView) { key, _, _ := anyLoser(*v); delete(v.Routing.Detachments, key) }},
		{"sequence drift", "detachment_event_coupling", func(v *AggregateView) { key, d, _ := anyLoser(*v); d.EventSeq++; v.Routing.Detachments[key] = d }},
		{"loser left arrived", "any_loser_arrived", func(v *AggregateView) {
			_, _, id := anyLoser(*v)
			p := v.Routing.Paths[id]
			p.State = PathArrived
			p.Disposition = nil
			p.DetachedSink = nil
			p.DetachmentSetID = ""
			p.UpdatedSeq = p.CreatedSeq
			v.Routing.Paths[id] = p
		}},
		{"wrong pre-arrived reason", "any_loser_sink_receipt", func(v *AggregateView) {
			_, _, id := anyLoser(*v)
			p := v.Routing.Paths[id]
			p.DetachedSink.ReasonCode = "late_any_arrival"
			v.Routing.Paths[id] = p
		}},
		{"failure instead of pre-arrived sink", "failure_before_sink_order", func(v *AggregateView) {
			_, _, id := anyLoser(*v)
			p := v.Routing.Paths[id]
			p.State = PathFailed
			p.DetachedSink = nil
			p.DetachmentSetID = ""
			p.Disposition.ToState = PathFailed
			p.TerminalCauseID = "missing"
			v.Routing.Paths[id] = p
		}},
		{"non-minimum winner", "any_winner_not_minimum", func(v *AggregateView) {
			rID := ""
			for _, d := range v.Routing.Detachments {
				rID = d.ReservationID
			}
			r := v.Routing.Reservations[rID]
			a := v.Routing.Activations[r.Activation.ID]
			winner := v.Routing.Paths[a.InputPathIDs[0]]
			winner.ArrivedSeq = winner.ArrivedSeq + 1
			winner.CreatedSeq = winner.ArrivedSeq
			v.Routing.Paths[winner.ID] = winner
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			view := validAnyFixture(t)
			test.mutate(&view)
			report := ValidateAggregate(view)
			if !reportHasCode(report, test.code) {
				t.Fatalf("diagnostics = %#v", report.Diagnostics)
			}
		})
	}
}

func TestDetachedCandidateCrossScopeEscapeAndReactivationRejected(t *testing.T) {
	t.Parallel()
	view := validAnyFixture(t)
	_, d, loserID := anyLoser(view)
	loser := view.Routing.Paths[loserID]
	escape := loser
	escape.ID = "escape"
	escape.Kind = PathActivationOutput
	escape.State = PathFailed
	escape.ParentPathID = ""
	escape.Edge = nil
	escape.TargetReservationID = ""
	escape.CandidateID = ""
	escape.ScopeID = view.Authority.Genesis.RootScopeID
	escape.ArrivalID = ""
	escape.ArrivedSeq = 0
	escape.DetachedSink = nil
	escape.DetachmentSetID = ""
	escape.Disposition = nil
	escape.TerminalCauseID = "missing"
	view.Routing.Paths[escape.ID] = escape
	report := ValidateAggregate(view)
	if !reportHasCode(report, "detached_scope_escape") {
		t.Fatalf("escape diagnostics = %#v", report.Diagnostics)
	}
	delete(view.Routing.Paths, escape.ID)
	loser.State = PathArrived
	loser.DetachedSink = nil
	loser.DetachmentSetID = ""
	loser.Disposition = nil
	loser.UpdatedSeq = loser.CreatedSeq
	view.Routing.Paths[loser.ID] = loser
	report = ValidateAggregate(view)
	if !reportHasCode(report, "detached_reactivation") || !reportHasCode(report, "any_loser_arrived") {
		t.Fatalf("reactivation diagnostics = %#v detachment=%#v", report.Diagnostics, d)
	}
}

func TestReducingActivationCannotPopWrongScope(t *testing.T) {
	t.Parallel()
	view := validAnyFixture(t)
	var r ActivationReservation
	for _, candidate := range view.Routing.Reservations {
		if candidate.JoinPolicy == JoinAny {
			r = candidate
		}
	}
	a := view.Routing.Activations[r.Activation.ID]
	output := view.Routing.Paths[a.OutputPathID]
	output.ScopeID = r.ScopeID
	view.Routing.Paths[output.ID] = output
	report := ValidateAggregate(view)
	if !reportHasCode(report, "scope_escape") {
		t.Fatalf("wrong pop diagnostics = %#v", report.Diagnostics)
	}
}

func TestAuthorityRequiredAndBoundToRunTemplateSource(t *testing.T) {
	t.Parallel()
	base := validGenesisFixture(t)
	tests := []struct {
		name   string
		mutate func(*AggregateView)
		code   string
	}{
		{"missing", func(v *AggregateView) { v.Authority = nil }, "authority_missing"},
		{"run", func(v *AggregateView) { v.Authority = cloneAuthority(v.Authority); v.Authority.RunID = "other" }, "authority_run_mismatch"},
		{"template", func(v *AggregateView) {
			v.Authority = cloneAuthority(v.Authority)
			v.Authority.TemplateRef = strings.Repeat("c", 64)
		}, "authority_template_mismatch"},
		{"source", func(v *AggregateView) {
			v.Authority = cloneAuthority(v.Authority)
			v.Authority.TemplateSourceHash = strings.Repeat("d", 64)
		}, "authority_source_hash_mismatch"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			view := base
			test.mutate(&view)
			report := ValidateAggregate(view)
			if !reportHasCode(report, test.code) {
				t.Fatalf("diagnostics = %#v", report.Diagnostics)
			}
		})
	}
}

func TestAuthorityRejectsZeroTwoRootsAndNonGenesisEmptyInput(t *testing.T) {
	t.Parallel()
	view := validGenesisFixture(t)
	view.Authority = cloneAuthority(view.Authority)
	delete(view.Authority.Scopes, view.Authority.Genesis.RootScopeID)
	if report := ValidateAggregate(view); !reportHasCode(report, "authority_root_count") {
		t.Fatalf("zero roots: %#v", report.Diagnostics)
	}
	view = validGenesisFixture(t)
	view.Authority = cloneAuthority(view.Authority)
	second, _ := ScopeIdentity(view.RunID, "", "", "fork", "path", 1)
	view.Authority.Scopes[second] = ScopeAuthority{ID: second, ForkActivationID: "fork", ForkOutputPathID: "path", Generation: 1, ExpectedBranchEdgeIDs: []string{}}
	if report := ValidateAggregate(view); !reportHasCode(report, "authority_root_count") {
		t.Fatalf("two roots: %#v", report.Diagnostics)
	}
	view = validGenesisFixture(t)
	extraID, _ := ActivationIdentity(view.RunID, view.Authority.Genesis.ReservationID, 1, "other")
	view.Routing.Activations[extraID] = ActivationRecord{ID: extraID, RunID: view.RunID, Ref: ActivationRef{ID: extraID, Generation: 1}, ReservationID: view.Authority.Genesis.ReservationID, InputPathIDs: []string{}, InputSetDigest: "other"}
	if report := ValidateAggregate(view); !reportHasCode(report, "activation_inputs_noncanonical") || !reportHasCode(report, "genesis_count") {
		t.Fatalf("non-genesis exception: %#v", report.Diagnostics)
	}
}

func TestAuthorityExactCandidateSlotBranchAndReservationSets(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, code string
		mutate     func(*AggregateView, string)
	}{
		{"state missing reservation", "authority_reservation_missing", func(v *AggregateView, id string) { delete(v.Routing.Reservations, id) }},
		{"state extra reservation", "authority_reservation_extra", func(v *AggregateView, id string) { delete(v.Authority.Reservations, id) }},
		{"state missing candidate", "authority_reservation_mismatch", func(v *AggregateView, id string) {
			r := v.Routing.Reservations[id]
			r.Candidates = []CandidateRecord{}
			v.Routing.Reservations[id] = r
		}},
		{"state extra candidate", "authority_reservation_mismatch", func(v *AggregateView, id string) {
			r := v.Routing.Reservations[id]
			r.Candidates = append(r.Candidates, r.Candidates[0])
			v.Routing.Reservations[id] = r
		}},
		{"state missing slot", "authority_reservation_mismatch", func(v *AggregateView, id string) {
			r := v.Routing.Reservations[id]
			r.PossibleSlots = []PossibleSlotRecord{}
			v.Routing.Reservations[id] = r
		}},
		{"state extra slot", "authority_reservation_mismatch", func(v *AggregateView, id string) {
			r := v.Routing.Reservations[id]
			r.PossibleSlots = append(r.PossibleSlots, r.PossibleSlots[0])
			v.Routing.Reservations[id] = r
		}},
		{"authority duplicate candidate", "authority_candidate_duplicate", func(v *AggregateView, id string) {
			r := v.Authority.Reservations[id]
			r.Candidates = append(r.Candidates, r.Candidates[0])
			v.Authority.Reservations[id] = r
		}},
		{"authority duplicate slot", "authority_slot_alias", func(v *AggregateView, id string) {
			r := v.Authority.Reservations[id]
			r.Candidates = append(r.Candidates, r.Candidates[0])
			v.Authority.Reservations[id] = r
		}},
		{"state extra branch", "authority_scope_mismatch", func(v *AggregateView, _ string) {
			root := v.Authority.Genesis.RootScopeID
			s := v.Routing.Scopes[root]
			s.ExpectedBranchEdgeIDs = []string{"extra"}
			v.Routing.Scopes[root] = s
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			view := validGenesisFixture(t)
			id := addOpenAuthorityReservation(t, &view, "target")
			test.mutate(&view, id)
			report := ValidateAggregate(view)
			if !reportHasCode(report, test.code) {
				t.Fatalf("diagnostics = %#v", report.Diagnostics)
			}
		})
	}
}

func TestBoundedIndexedAuthorityStress(t *testing.T) {
	t.Parallel()
	view := validGenesisFixture(t)
	for n := 0; n < MaxRoutingList; n++ {
		addOpenAuthorityReservation(t, &view, fmt.Sprintf("target-%04d", n))
	}
	report := ValidateAggregate(view)
	if !report.Valid() {
		t.Fatalf("stress diagnostics first=%#v count=%d suppressed=%d", report.Diagnostics[:min(3, len(report.Diagnostics))], len(report.Diagnostics), report.Suppressed)
	}
}

func TestActualMaximumAnyAndAllCandidateStructures(t *testing.T) {
	t.Parallel()
	t.Run("any maximum", func(t *testing.T) {
		view := validGenesisFixture(t)
		addWideOpenReservation(t, &view, "any", JoinAny, MaxAnyCandidates)
		if report := ValidateAggregate(view); !report.Valid() {
			t.Fatalf("max any diagnostics: %#v", report.Diagnostics)
		}
	})
	t.Run("any bound+1", func(t *testing.T) {
		view := validGenesisFixture(t)
		addWideOpenReservation(t, &view, "any", JoinAny, MaxAnyCandidates+1)
		if report := ValidateAggregate(view); !reportHasCode(report, "authority_any_candidate_bound") {
			t.Fatalf("diagnostics: %#v", report.Diagnostics)
		}
	})
	t.Run("all maximum", func(t *testing.T) {
		view := validGenesisFixture(t)
		addWideOpenReservation(t, &view, "all", JoinAll, MaxOutgoingOrAllCandidates)
		if report := ValidateAggregate(view); !report.Valid() {
			t.Fatalf("max all diagnostics: %#v", report.Diagnostics)
		}
	})
	t.Run("all bound+1", func(t *testing.T) {
		view := validGenesisFixture(t)
		addWideOpenReservation(t, &view, "all", JoinAll, MaxOutgoingOrAllCandidates+1)
		if report := ValidateAggregate(view); !reportHasCode(report, "authority_candidate_bound") {
			t.Fatalf("diagnostics: %#v", report.Diagnostics)
		}
	})
}

func TestAllArrivedPlusNonSuccessExactCloseCause(t *testing.T) {
	t.Parallel()
	view := validAllArrivedNonSuccessFixture(t)
	report := ValidateAggregate(view)
	if !report.Valid() {
		t.Fatalf("all non-success diagnostics: %#v", report.Diagnostics)
	}
	completion, err := AssessAggregateCompletion(view)
	if err != nil || completion.Result != "failed" {
		t.Fatalf("completion=%#v err=%v", completion, err)
	}
	for id, set := range view.Routing.CauseSets {
		if len(set.CauseIDs) == 2 {
			set.CauseIDs = set.CauseIDs[:1]
			view.Routing.CauseSets[id] = set
			break
		}
	}
	report = ValidateAggregate(view)
	if !reportHasCode(report, "reservation_close_cause_set") {
		t.Fatalf("incomplete cause diagnostics: %#v", report.Diagnostics)
	}
}

func TestDetachmentSetIndexAllowsRepeatedMemberAcrossDistinctChains(t *testing.T) {
	t.Parallel()
	first, second := DetachmentID("detachment-first"), DetachmentID("detachment-second")
	firstRoot, _ := DetachmentSetIdentity("", first)
	secondRoot, _ := DetachmentSetIdentity("", second)
	chain, _ := DetachmentSetIdentity(firstRoot, second)
	state := NewRoutingState()
	state.DetachmentSets[firstRoot] = DetachmentSetRecord{ID: firstRoot, DetachmentID: first}
	state.DetachmentSets[secondRoot] = DetachmentSetRecord{ID: secondRoot, DetachmentID: second}
	state.DetachmentSets[chain] = DetachmentSetRecord{ID: chain, ParentSetID: firstRoot, DetachmentID: second}

	newIndex := func() (*aggregateIndex, *InvariantReport) {
		report := &InvariantReport{}
		return &aggregateIndex{
			view: AggregateView{Routing: &state}, c: diagnosticCollector{report: report},
			detachmentsByID:        map[DetachmentID]DetachmentRecord{first: {ID: first}, second: {ID: second}},
			detachmentSetIntervals: map[DetachmentSetID]treeInterval{},
			detachmentMemberNodes:  map[DetachmentID][]DetachmentSetID{},
		}, report
	}
	index, report := newIndex()
	index.indexAllDetachmentSets()
	if !report.Valid() {
		t.Fatalf("distinct-chain diagnostics: %#v", report.Diagnostics)
	}
	if !index.detachmentSetContains(chain, first) || !index.detachmentSetContains(chain, second) {
		t.Fatal("chain does not contain both causal detachments")
	}
	if index.detachmentSetContains(secondRoot, first) {
		t.Fatal("unrelated root contains first detachment")
	}

	duplicate, _ := DetachmentSetIdentity(chain, first)
	state.DetachmentSets[duplicate] = DetachmentSetRecord{ID: duplicate, ParentSetID: chain, DetachmentID: first}
	index, report = newIndex()
	index.indexAllDetachmentSets()
	if !reportHasCode(*report, "detachment_set_duplicate") {
		t.Fatalf("duplicate-chain diagnostics: %#v", report.Diagnostics)
	}
}

func TestEdgeArrivalRequiresExactAuthorizedSlot(t *testing.T) {
	t.Parallel()
	view, oldPathID, _ := validOpenArrivalFixture(t)
	if report := ValidateAggregate(view); !report.Valid() {
		t.Fatalf("valid arrival diagnostics: %#v", report.Diagnostics)
	}
	p := view.Routing.Paths[oldPathID]
	edge := *p.Edge
	edge.Outcome = "forged"
	edge.ID, _ = EdgeIdentity(edge.TemplateRef, edge.FromNodeID, edge.Outcome, edge.ToNodeID)
	newPathID, _ := EdgePathIdentity(p.SourceActivation.ID, p.ParentPathID, edge.ID, p.TargetReservationID, p.CandidateID)
	p.ID = newPathID
	p.Edge = &edge
	p.ArrivalID, _ = ArrivalIdentity(newPathID, p.TargetReservationID, p.CandidateID)
	delete(view.Routing.Paths, oldPathID)
	view.Routing.Paths[newPathID] = p
	parent := view.Routing.Paths[p.ParentPathID]
	parent.ProducedPathIDs = []string{newPathID}
	view.Routing.Paths[parent.ID] = parent
	report := ValidateAggregate(view)
	if !reportHasCode(report, "path_slot_authority") {
		t.Fatalf("forged slot diagnostics: %#v", report.Diagnostics)
	}
}

func TestAuthorizedSlotCannotMaterializeAsEdgeAndImpossibleEdge(t *testing.T) {
	t.Parallel()
	view, pathID, _ := validOpenArrivalFixture(t)
	p := view.Routing.Paths[pathID]
	commandID := view.Routing.Paths[p.ParentPathID].Disposition.CommandID
	causeID, _ := CauseIdentity("", TerminalImpossible, "condition_false", "", commandID, "", 2)
	view.Routing.CauseRecords[causeID] = CauseRecord{ID: causeID, TerminalKind: TerminalImpossible, DispositionReason: "condition_false", SourceCommandID: commandID, EventSeq: 2}
	causeDigest, _ := CauseSetIdentity([]string{causeID})
	view.Routing.CauseSets[causeDigest] = CauseSetRecord{Digest: causeDigest, CauseIDs: []string{causeID}}
	impossibleID, _ := ImpossibleEdgePathIdentity(causeDigest, p.Edge.ID, p.TargetReservationID)
	view.Routing.Paths[impossibleID] = PathRecord{ID: impossibleID, Kind: PathImpossibleEdge, State: PathImpossible, ParentPathID: p.ParentPathID, SourceActivation: p.SourceActivation, Edge: p.Edge, TargetReservationID: p.TargetReservationID, CandidateID: p.CandidateID, ScopeID: p.ScopeID, BranchEdgeID: p.BranchEdgeID, CandidateLineage: p.CandidateLineage, CandidateLineageID: p.CandidateLineageID, LineageDepth: p.LineageDepth, ImpossibleCauseDigest: causeDigest, CreatedSeq: 2, UpdatedSeq: 2}
	parent := view.Routing.Paths[p.ParentPathID]
	parent.ProducedPathIDs = append(parent.ProducedPathIDs, impossibleID)
	slices.Sort(parent.ProducedPathIDs)
	view.Routing.Paths[parent.ID] = parent
	report := ValidateAggregate(view)
	if !reportHasCode(report, "slot_multiple_paths") {
		t.Fatalf("duplicate slot diagnostics: %#v", report.Diagnostics)
	}
}

func TestActivationInputRequiresExactAuthorizedSlot(t *testing.T) {
	t.Parallel()
	view, pathID, reservationID := validOpenArrivalFixture(t)
	activateOpenArrival(t, &view, pathID, reservationID)
	if report := ValidateAggregate(view); !report.Valid() {
		t.Fatalf("valid activation diagnostics: %#v", report.Diagnostics)
	}
	forgedID := forgeActivatedInputEdge(t, &view, pathID)
	report := ValidateAggregate(view)
	if !reportHasCode(report, "path_slot_authority") {
		t.Fatalf("forged activation input %q diagnostics: %#v", forgedID, report.Diagnostics)
	}
}

func rebindTerminalPathCommand(t *testing.T, view *AggregateView, pathID PathID, commandID string) {
	t.Helper()
	p := view.Routing.Paths[pathID]
	oldCause := view.Routing.CauseRecords[p.TerminalCauseID]
	delete(view.Routing.CauseRecords, oldCause.ID)
	p.Disposition.CommandID = commandID
	p.Disposition.ID, _ = DispositionReceiptIdentity(p.ID, p.Disposition.FromState, p.Disposition.ToState, p.Disposition.ReasonCode, commandID, p.Disposition.AdminRecordID, uint64(p.Disposition.EventSeq))
	oldCause.SourceCommandID = commandID
	oldCause.ID, _ = CauseIdentity(oldCause.SourcePathID, oldCause.TerminalKind, oldCause.DispositionReason, oldCause.SourceActivationID, commandID, oldCause.AdminRecordID, uint64(oldCause.EventSeq))
	p.TerminalCauseID = oldCause.ID
	view.Routing.Paths[pathID] = p
	view.Routing.CauseRecords[oldCause.ID] = oldCause
}

func failedPathID(view AggregateView) PathID {
	for id, path := range view.Routing.Paths {
		if path.State == PathFailed {
			return id
		}
	}
	return ""
}

func TestTerminalDispositionRejectsUnrelatedCommandAtCompletion(t *testing.T) {
	t.Parallel()
	view := validSlowAnyFixture(t, true)
	pathID := failedPathID(view)
	initializeID := ""
	for id, command := range view.Commands {
		if command.Identity.Kind == CommandInitializeRouting {
			initializeID = id
		}
	}
	rebindTerminalPathCommand(t, &view, pathID, initializeID)
	report := ValidateAggregate(view)
	if !reportHasCode(report, "terminal_command_capability") {
		t.Fatalf("unrelated terminal command diagnostics: %#v", report.Diagnostics)
	}
	if _, err := AssessAggregateCompletion(view); !errors.Is(err, ErrAggregateInvalid) {
		t.Fatalf("completion error = %v, want ErrAggregateInvalid", err)
	}
}

func TestTerminalDispositionRequiresExactSourceTuple(t *testing.T) {
	t.Parallel()
	view := validSlowAnyFixture(t, true)
	pathID := failedPathID(view)
	wrong := makeTestCommand(t, CommandIdentity{RunID: view.RunID, Kind: CommandSettleAttempt, PayloadSchema: 1, SourceActivationID: view.Authority.Genesis.ActivationID, SourceGeneration: 1, Attempt: 1, InputDigest: "attempt", PlanDigest: "observe", ResultCode: "failed"}, CommandObserved)
	view.Commands[wrong.ID] = wrong
	rebindTerminalPathCommand(t, &view, pathID, wrong.ID)
	report := ValidateAggregate(view)
	if !reportHasCode(report, "terminal_command_tuple") {
		t.Fatalf("wrong terminal tuple diagnostics: %#v", report.Diagnostics)
	}
	if _, err := AssessAggregateCompletion(view); !errors.Is(err, ErrAggregateInvalid) {
		t.Fatalf("completion error = %v, want ErrAggregateInvalid", err)
	}
}

func TestEndedDispositionRequiresRouteCapability(t *testing.T) {
	t.Parallel()
	view := validGenesisFixture(t)
	pathID := view.Authority.Genesis.OutputPathID
	p := view.Routing.Paths[pathID]
	p.UpdatedSeq = 2
	initializeID := view.Authority.Genesis.ReservationID
	for id, command := range view.Commands {
		if command.Identity.Kind == CommandInitializeRouting {
			initializeID = id
		}
	}
	dispositionID, _ := DispositionReceiptIdentity(p.ID, PathLive, PathEnded, "completed", initializeID, "", 2)
	p.Disposition = &DispositionReceipt{ID: dispositionID, PathID: p.ID, FromState: PathLive, ToState: PathEnded, ReasonCode: "completed", CommandID: initializeID, EventSeq: 2}
	view.Routing.Paths[p.ID] = p
	report := ValidateAggregate(view)
	if !reportHasCode(report, "terminal_command_capability") {
		t.Fatalf("ended command diagnostics: %#v", report.Diagnostics)
	}
	if _, err := AssessAggregateCompletion(view); !errors.Is(err, ErrAggregateInvalid) {
		t.Fatalf("completion error = %v, want ErrAggregateInvalid", err)
	}
}

func TestAggregateRejectsCommandlessMaterializedActivation(t *testing.T) {
	t.Parallel()
	view := validGenesisFixture(t)
	activationID := view.Authority.Genesis.ActivationID
	reservationID := view.Authority.Genesis.ReservationID
	a := view.Routing.Activations[activationID]
	commandID := a.CommandID
	a.CommandID = ""
	a.Receipt.CommandID = ""
	a.Receipt.ID, _ = ActivationReceiptIdentity(a.ID, a.ReservationID, a.InputSetDigest, a.OutputPathID, "", uint64(a.EventSeq))
	view.Routing.Activations[a.ID] = a
	r := view.Routing.Reservations[reservationID]
	r.CommandID = ""
	view.Routing.Reservations[r.ID] = r
	delete(view.Commands, commandID)
	report := ValidateAggregate(view)
	if !reportHasCode(report, "command_authority_missing") {
		t.Fatalf("commandless activation diagnostics: %#v", report.Diagnostics)
	}
	if _, err := AssessAggregateCompletion(view); !errors.Is(err, ErrAggregateInvalid) {
		t.Fatalf("completion error = %v, want ErrAggregateInvalid", err)
	}
}

func TestAggregateRejectsCommandlessReceiptClosureAndPropagation(t *testing.T) {
	t.Parallel()
	t.Run("receipt", func(t *testing.T) {
		view := validGenesisFixture(t)
		a := view.Routing.Activations[view.Authority.Genesis.ActivationID]
		a.Receipt.CommandID = ""
		a.Receipt.ID, _ = ActivationReceiptIdentity(a.ID, a.ReservationID, a.InputSetDigest, a.OutputPathID, "", uint64(a.EventSeq))
		view.Routing.Activations[a.ID] = a
		if report := ValidateAggregate(view); !reportHasCode(report, "command_authority_missing") {
			t.Fatalf("commandless receipt diagnostics: %#v", report.Diagnostics)
		}
	})
	t.Run("closure", func(t *testing.T) {
		view := validAllArrivedNonSuccessFixture(t)
		for key, closure := range view.Routing.CandidateClosures {
			closure.CommandID = ""
			view.Routing.CandidateClosures[key] = closure
			break
		}
		if report := ValidateAggregate(view); !reportHasCode(report, "command_authority_missing") {
			t.Fatalf("commandless closure diagnostics: %#v", report.Diagnostics)
		}
	})
	t.Run("propagation", func(t *testing.T) {
		view := validGenesisFixture(t)
		reservationID := addOpenAuthorityReservation(t, &view, "target")
		r := view.Routing.Reservations[reservationID]
		candidate := r.Candidates[0]
		commandID := view.Routing.Activations[view.Authority.Genesis.ActivationID].CommandID
		causeID, _ := CauseIdentity("", TerminalImpossible, "condition_false", "", commandID, "", 2)
		view.Routing.CauseRecords[causeID] = CauseRecord{ID: causeID, TerminalKind: TerminalImpossible, DispositionReason: "condition_false", SourceCommandID: commandID, EventSeq: 2}
		causeDigest, _ := CauseSetIdentity([]string{causeID})
		view.Routing.CauseSets[causeDigest] = CauseSetRecord{Digest: causeDigest, CauseIDs: []string{causeID}}
		closureKey, _ := CandidateClosureKeyIdentity(r.ID, candidate.ID)
		frontier := []string{closureKey}
		plan, _ := PropagationPlanIdentity(r.ID, candidate.ID, causeDigest, 0, frontier)
		intentID, _ := PropagationIntentIdentity(causeDigest, 0, plan)
		view.Routing.Propagation[intentID] = PropagationIntent{ID: intentID, RootReservationID: r.ID, RootCandidateID: candidate.ID, RootCauseDigest: causeDigest, Cursor: 1, Frontier: frontier, PlanDigest: plan, State: PropagationComplete, EventSeq: 2}
		if report := ValidateAggregate(view); !reportHasCode(report, "command_authority_missing") {
			t.Fatalf("commandless propagation diagnostics: %#v", report.Diagnostics)
		}
	})
}
