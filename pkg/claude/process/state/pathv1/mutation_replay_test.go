package pathv1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestMutationBatchRequiresCanonicalWholePreOrPostState(t *testing.T) {
	before := NewRoutingState()
	after := Clone(before)
	dispositionID, _ := DispositionReceiptIdentity("p", PathLive, PathEnded, "end", MutationCommandPlaceholder, "", 1)
	after.Paths["p"] = PathRecord{
		ID: "p", Kind: PathActivationOutput, State: PathEnded,
		Disposition: &DispositionReceipt{ID: dispositionID, PathID: "p", FromState: PathLive, ToState: PathEnded, ReasonCode: "end", CommandID: MutationCommandPlaceholder, EventSeq: 1},
	}
	batch, err := NewMutationBatch(&before, &after, 1)
	if err != nil {
		t.Fatal(err)
	}
	got, disposition, err := batch.replay(&before, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if disposition != ReplayApplied || got.Paths["p"].Disposition.CommandID != strings.Repeat("a", 64) {
		t.Fatalf("applied replay = %#v, %q", got.Paths["p"], disposition)
	}
	if _, disposition, err = batch.replay(&got, strings.Repeat("a", 64)); err != nil || disposition != ReplayAlreadyApplied {
		t.Fatalf("post replay = %q, %v", disposition, err)
	}

	partial := Clone(before)
	partial.CauseSets["extra"] = CauseSetRecord{Digest: "extra", CauseIDs: []CauseID{}}
	if _, _, err := batch.replay(&partial, strings.Repeat("a", 64)); !errors.Is(err, ErrMutationInconsistent) {
		t.Fatalf("extra-state error = %v", err)
	}

	duplicate := batch
	duplicate.Mutations = append(duplicate.Mutations, duplicate.Mutations[0])
	if err := duplicate.Validate(); !errors.Is(err, ErrMutationInvalid) {
		t.Fatalf("duplicate error = %v", err)
	}
	noncanonical := batch
	noncanonical.Mutations = append([]RecordMutation(nil), batch.Mutations...)
	noncanonical.Mutations[0].After = append([]byte(" "), noncanonical.Mutations[0].After...)
	if err := noncanonical.Validate(); !errors.Is(err, ErrMutationInvalid) {
		t.Fatalf("noncanonical record error = %v", err)
	}
}

func TestMutationMaterializeSingleCauseSetIsIdempotent(t *testing.T) {
	before := NewRoutingState()
	after := Clone(before)
	placeholderID, _ := CauseIdentity("", TerminalFailed, "single", "", MutationCommandPlaceholder, "", 1)
	placeholderDigest, _ := CauseSetIdentity([]CauseID{placeholderID})
	after.CauseRecords[placeholderID] = CauseRecord{
		ID: placeholderID, TerminalKind: TerminalFailed, DispositionReason: "single",
		SourceCommandID: MutationCommandPlaceholder, EventSeq: 1,
	}
	after.CauseSets[placeholderDigest] = CauseSetRecord{Digest: placeholderDigest, CauseIDs: []CauseID{placeholderID}}
	batch, err := NewMutationBatch(&before, &after, 1)
	if err != nil {
		t.Fatal(err)
	}
	commandID := strings.Repeat("a", 64)
	actualID, _ := CauseIdentity("", TerminalFailed, "single", "", commandID, "", 1)
	actualDigest, _ := CauseSetIdentity([]CauseID{actualID})

	got, disposition, err := batch.replay(&before, commandID)
	if err != nil || disposition != ReplayApplied {
		t.Fatalf("single cause-set replay = %q, %v", disposition, err)
	}
	set, ok := got.CauseSets[actualDigest]
	if !ok || set.Digest != actualDigest || !slices.Equal(set.CauseIDs, []CauseID{actualID}) {
		t.Fatalf("single materialized cause set = %#v", set)
	}
	got, disposition, err = batch.replay(&got, commandID)
	if err != nil || disposition != ReplayAlreadyApplied {
		t.Fatalf("single cause-set idempotent replay = %q, %v", disposition, err)
	}
}

func TestRoutePathsTypedReplayAppliesOnceAndRejectsDrift(t *testing.T) {
	postView, childID, reservationID := validOpenArrivalFixture(t)
	parentID := postView.Authority.Genesis.OutputPathID
	oldRouteID := postView.Routing.Paths[parentID].Disposition.CommandID
	delete(postView.Commands, oldRouteID)

	before := Clone(*postView.Routing)
	delete(before.Paths, childID)
	parent := before.Paths[parentID]
	parent.State = PathLive
	parent.ProducedPathIDs = nil
	parent.Disposition = nil
	parent.UpdatedSeq = 1
	before.Paths[parentID] = parent

	afterTemplate := Clone(*postView.Routing)
	afterParent := afterTemplate.Paths[parentID]
	afterParent.Disposition.CommandID = MutationCommandPlaceholder
	afterParent.Disposition.ReasonCode = "exclusive_route"
	afterParent.Disposition.ID, _ = DispositionReceiptIdentity(parentID, PathLive, PathRouted, "exclusive_route", MutationCommandPlaceholder, "", 2)
	afterTemplate.Paths[parentID] = afterParent
	batch, err := NewMutationBatch(&before, &afterTemplate, 2)
	if err != nil {
		t.Fatal(err)
	}

	settlement := makeTestCommand(t, CommandIdentity{
		RunID: postView.RunID, Kind: CommandSettleAttempt, PayloadSchema: 1,
		SourceActivationID: parent.SourceActivation.ID, SourceGeneration: 1, Attempt: 1,
		InputDigest: "perform-command", PlanDigest: "observed-result", ResultCode: "pass",
	}, CommandObserved)
	postView.Commands[settlement.ID] = settlement
	postView.Routing = &before
	replayView := MutationReplayView{
		Aggregate:  postView,
		Checkpoint: CheckpointBinding{Generation: 7, Digest: strings.Repeat("c", 64)},
	}
	plan := RoutePathsPlan{
		SettlementCommandID: settlement.ID,
		SourceActivationID:  parent.SourceActivation.ID,
		SourceGeneration:    1,
		SourcePathID:        parentID,
		Attempt:             1,
		CauseDigest:         "route-cause",
		ResultCode:          "exclusive/pass",
		ProducedPathIDs:     []PathID{childID},
		Batch:               batch,
	}
	payload, err := EncodeRoutePathsPayload(replayView, plan)
	if err != nil {
		t.Fatal(err)
	}
	identity := CommandIdentity{
		RunID: postView.RunID, Kind: CommandRoutePaths, PayloadSchema: 1,
		SourceActivationID: parent.SourceActivation.ID, SourceGeneration: 1, SourcePathID: parentID, Attempt: 1,
		InputDigest: settlement.ID, CauseDigest: plan.CauseDigest, PlanDigest: payloadDigest(payload), ResultCode: plan.ResultCode,
	}
	command := commandWithPayload(t, identity, CommandObserved, payload)
	replayView.Aggregate.Commands[command.ID] = command

	badAfter := Clone(afterTemplate)
	for scopeID, scope := range badAfter.Scopes {
		scope.EventSeq++
		badAfter.Scopes[scopeID] = scope
		break
	}
	badBatch, err := NewMutationBatch(&before, &badAfter, 2)
	if err != nil {
		t.Fatal(err)
	}
	badPlan := plan
	badPlan.Batch = badBatch
	badPayload, err := EncodeRoutePathsPayload(replayView, badPlan)
	if err != nil {
		t.Fatal(err)
	}
	badIdentity := identity
	badIdentity.PlanDigest = payloadDigest(badPayload)
	badCommand := commandWithPayload(t, badIdentity, CommandObserved, badPayload)
	replayView.Aggregate.Commands[badCommand.ID] = badCommand
	if err := ValidateRoutePathsCommand(replayView, badCommand); !errors.Is(err, ErrMutationInvalid) {
		t.Fatalf("route unrelated scope mutation error = %v", err)
	}
	delete(replayView.Aggregate.Commands, badCommand.ID)

	barePlan := plan
	barePlan.ResultCode = "pass"
	barePayload, err := EncodeRoutePathsPayload(replayView, barePlan)
	if err != nil {
		t.Fatal(err)
	}
	bareIdentity := identity
	bareIdentity.PlanDigest = payloadDigest(barePayload)
	bareIdentity.ResultCode = barePlan.ResultCode
	bareCommand := commandWithPayload(t, bareIdentity, CommandObserved, barePayload)
	if err := ValidateRoutePathsCommand(replayView, bareCommand); !errors.Is(err, ErrMutationInvalid) {
		t.Fatalf("exclusive route with bare result error = %v", err)
	}

	ambiguousPlan := plan
	ambiguousPlan.ResultCode = "other/pass"
	ambiguousPayload, err := EncodeRoutePathsPayload(replayView, ambiguousPlan)
	if err != nil {
		t.Fatal(err)
	}
	ambiguousIdentity := identity
	ambiguousIdentity.PlanDigest = payloadDigest(ambiguousPayload)
	ambiguousIdentity.ResultCode = ambiguousPlan.ResultCode
	ambiguousCommand := commandWithPayload(t, ambiguousIdentity, CommandObserved, ambiguousPayload)
	if err := ValidateRoutePathsCommand(replayView, ambiguousCommand); !errors.Is(err, ErrMutationInvalid) {
		t.Fatalf("route result with ambiguous prefix error = %v", err)
	}

	result, err := ReplayRoutePaths(replayView, command)
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != ReplayApplied || result.Routing.Paths[parentID].Disposition.CommandID != command.ID {
		t.Fatalf("route replay = %q, %#v", result.Disposition, result.Routing.Paths[parentID])
	}
	replayView.Aggregate.Routing = &result.Routing
	result, err = ReplayRoutePaths(replayView, command)
	if err != nil || result.Disposition != ReplayAlreadyApplied {
		t.Fatalf("route idempotent replay = %q, %v", result.Disposition, err)
	}

	drift := Clone(result.Routing)
	r := drift.Reservations[reservationID]
	r.EventSeq++
	drift.Reservations[reservationID] = r
	replayView.Aggregate.Routing = &drift
	if _, err := ReplayRoutePaths(replayView, command); !errors.Is(err, ErrMutationInconsistent) {
		t.Fatalf("extra-state drift error = %v", err)
	}
}

func TestRoutePathsTypedTerminalReplayAndStrictEnvelope(t *testing.T) {
	view := validGenesisFixture(t)
	pathID := view.Authority.Genesis.OutputPathID
	path := view.Routing.Paths[pathID]
	settlement := addTerminalAttemptCommands(t, &view, path.SourceActivation.ID, path.SourceActivation.Generation, 1, "pass", "", "observed")
	before := Clone(*view.Routing)
	beforePath := before.Paths[path.ID]
	beforePath.State = PathLive
	beforePath.UpdatedSeq = 1
	before.Paths[beforePath.ID] = beforePath
	afterTemplate := Clone(before)
	path.State = PathEnded
	path.UpdatedSeq = 2
	dispositionID, _ := DispositionReceiptIdentity(path.ID, PathLive, PathEnded, "completed", MutationCommandPlaceholder, "", 2)
	path.Disposition = &DispositionReceipt{
		ID: dispositionID, PathID: path.ID, FromState: PathLive, ToState: PathEnded,
		ReasonCode: "completed", CommandID: MutationCommandPlaceholder, EventSeq: 2,
	}
	afterTemplate.Paths[path.ID] = path
	batch, err := NewMutationBatch(&before, &afterTemplate, 2)
	if err != nil {
		t.Fatal(err)
	}
	view.Routing = &before
	replayView := MutationReplayView{Aggregate: view, Checkpoint: CheckpointBinding{Generation: 11, Digest: strings.Repeat("b", 64)}}
	plan := RoutePathsPlan{
		SettlementCommandID: settlement.ID, SourceActivationID: path.SourceActivation.ID,
		SourceGeneration: path.SourceActivation.Generation, SourcePathID: path.ID, Attempt: 1,
		CauseDigest: "terminal-route", ResultCode: "pass", ProducedPathIDs: []PathID{}, Batch: batch,
	}
	payload, err := EncodeRoutePathsPayload(replayView, plan)
	if err != nil {
		t.Fatal(err)
	}
	identity := CommandIdentity{
		RunID: view.RunID, Kind: CommandRoutePaths, PayloadSchema: 1,
		SourceActivationID: path.SourceActivation.ID, SourceGeneration: path.SourceActivation.Generation,
		SourcePathID: path.ID, Attempt: 1, InputDigest: settlement.ID,
		CauseDigest: plan.CauseDigest, PlanDigest: payloadDigest(payload), ResultCode: plan.ResultCode,
	}
	command := commandWithPayload(t, identity, CommandObserved, payload)
	replayView.Aggregate.Commands[command.ID] = command
	result, err := ReplayRoutePaths(replayView, command)
	if err != nil || result.Disposition != ReplayApplied || result.Routing.Paths[path.ID].State != PathEnded {
		t.Fatalf("typed terminal route replay = %q, %v", result.Disposition, err)
	}

	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "missing_template_source_hash", mutate: func(value map[string]any) { delete(value, "templateSourceHash") }},
		{name: "unknown_top_level_field", mutate: func(value map[string]any) { value["unexpected"] = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			var malformed map[string]any
			if err := json.Unmarshal(payload, &malformed); err != nil {
				t.Fatal(err)
			}
			test.mutate(malformed)
			malformedPayload, err := json.Marshal(malformed)
			if err != nil {
				t.Fatal(err)
			}
			forgedIdentity := identity
			forgedIdentity.PlanDigest = payloadDigest(malformedPayload)
			forged := commandWithPayload(t, forgedIdentity, CommandObserved, malformedPayload)
			aggregate := replayView.Aggregate
			aggregate.Commands = cloneMap(replayView.Aggregate.Commands)
			aggregate.Commands[forged.ID] = forged
			routing := Clone(result.Routing)
			terminal := routing.Paths[path.ID]
			terminal.Disposition.CommandID = forged.ID
			terminal.Disposition.ID, _ = DispositionReceiptIdentity(terminal.ID, terminal.Disposition.FromState, terminal.Disposition.ToState, terminal.Disposition.ReasonCode, forged.ID, "", uint64(terminal.Disposition.EventSeq))
			routing.Paths[terminal.ID] = terminal
			aggregate.Routing = &routing
			report := ValidateAggregate(aggregate)
			if !reportHasCode(report, "terminal_command_provenance") {
				t.Fatalf("malformed typed route diagnostics: %#v", report.Diagnostics)
			}
		})
	}
}

func TestRouteMutationAuthorityImpossibleCauseClosureAndForkEvent(t *testing.T) {
	makeSource := func() RoutingState {
		state := NewRoutingState()
		state.Scopes["root"] = ScopeRecord{ID: "root", RunID: "run", Generation: 1, ExpectedBranchEdgeIDs: []EdgeID{}, State: ScopeOpen, EventSeq: 1}
		state.Paths["source"] = PathRecord{ID: "source", Kind: PathActivationOutput, State: PathLive, SourceActivation: ActivationRef{ID: "activation", Generation: 1}, ScopeID: "root", CandidateLineage: []CandidateLineageFrame{}, CreatedSeq: 1, UpdatedSeq: 1}
		return state
	}
	finishSource := func(after *RoutingState, state PathState, produced []PathID) PathRecord {
		source := after.Paths["source"]
		source.State = state
		source.ProducedPathIDs = produced
		source.UpdatedSeq = 5
		dispositionID, _ := DispositionReceiptIdentity(source.ID, PathLive, state, "route", MutationCommandPlaceholder, "", 5)
		source.Disposition = &DispositionReceipt{ID: dispositionID, PathID: source.ID, FromState: PathLive, ToState: state, ReasonCode: "route", CommandID: MutationCommandPlaceholder, EventSeq: 5}
		after.Paths[source.ID] = source
		return source
	}
	makePlan := func(t *testing.T, before, after RoutingState, produced []PathID) (RoutePathsPlan, PathRecord) {
		t.Helper()
		batch, err := NewMutationBatch(&before, &after, 5)
		if err != nil {
			t.Fatal(err)
		}
		return RoutePathsPlan{SourcePathID: "source", ProducedPathIDs: produced, Batch: batch}, after.Paths["source"]
	}

	t.Run("impossible_cause_closure", func(t *testing.T) {
		before := makeSource()
		before.Reservations["reservation"] = ActivationReservation{ID: "reservation", RunID: "run", NodeID: "target", ScopeID: "root", Generation: 1, JoinPolicy: JoinExclusive, Candidates: []CandidateRecord{}, PossibleSlots: []PossibleSlotRecord{}, State: ReservationOpen, EventSeq: 1}
		after := Clone(before)
		causeID, _ := CauseIdentity("", TerminalFailed, "impossible_edge", "", MutationCommandPlaceholder, "", 5)
		causeDigest, _ := CauseSetIdentity([]CauseID{causeID})
		edge := EdgeKey{TemplateRef: "template", FromNodeID: "source", Outcome: "missing", ToNodeID: "target"}
		edge.ID, _ = EdgeIdentity(edge.TemplateRef, edge.FromNodeID, edge.Outcome, edge.ToNodeID)
		childID, _ := ImpossibleEdgePathIdentity(causeDigest, edge.ID, "reservation")
		after.CauseRecords[causeID] = CauseRecord{ID: causeID, TerminalKind: TerminalFailed, DispositionReason: "impossible_edge", SourceCommandID: MutationCommandPlaceholder, EventSeq: 5}
		after.CauseSets[causeDigest] = CauseSetRecord{Digest: causeDigest, CauseIDs: []CauseID{causeID}}
		after.Paths[childID] = PathRecord{ID: childID, Kind: PathImpossibleEdge, State: PathImpossible, ParentPathID: "source", SourceActivation: ActivationRef{ID: "activation", Generation: 1}, Edge: &edge, TargetReservationID: "reservation", ScopeID: "root", CandidateLineage: []CandidateLineageFrame{}, ImpossibleCauseDigest: causeDigest, CreatedSeq: 5, UpdatedSeq: 5}
		finishSource(&after, PathRouted, []PathID{childID})
		plan, sourceAfter := makePlan(t, before, after, []PathID{childID})
		if err := validateRouteMutationSet(before, plan, sourceAfter); err != nil {
			t.Fatalf("valid impossible-edge route: %v", err)
		}

		aggregate, arrivedPathID, _ := validOpenArrivalFixture(t)
		otherReservationID := addOpenAuthorityReservation(t, &aggregate, "other-target")
		otherReservation := aggregate.Routing.Reservations[otherReservationID]
		otherCandidate := otherReservation.Candidates[0]
		otherEdge := EdgeKey{TemplateRef: aggregate.TemplateRef, FromNodeID: "start", Outcome: "pass", ToNodeID: "other-target"}
		otherEdge.ID, _ = EdgeIdentity(otherEdge.TemplateRef, otherEdge.FromNodeID, otherEdge.Outcome, otherEdge.ToNodeID)
		otherLineageID, _ := CandidateLineageIdentity("", otherReservationID, otherCandidate.ID)
		arrived := aggregate.Routing.Paths[arrivedPathID]
		parentID := arrived.ParentPathID
		oldRouteID := aggregate.Routing.Paths[parentID].Disposition.CommandID
		delete(aggregate.Commands, oldRouteID)
		fullBefore := Clone(*aggregate.Routing)
		delete(fullBefore.Paths, arrivedPathID)
		fullParent := fullBefore.Paths[parentID]
		fullParent.State = PathLive
		fullParent.ProducedPathIDs = nil
		fullParent.Disposition = nil
		fullParent.UpdatedSeq = 1
		fullBefore.Paths[parentID] = fullParent
		fullAfter := Clone(fullBefore)
		fullAfter.Paths[arrived.ID] = arrived
		fullCauseReasons := []string{"condition_false_a", "condition_false_b", "condition_false_c"}
		fullCauseIDs := make([]CauseID, 0, len(fullCauseReasons))
		fullCauseReasonByID := make(map[CauseID]string, len(fullCauseReasons))
		for _, reason := range fullCauseReasons {
			id, _ := CauseIdentity("", TerminalImpossible, reason, "", MutationCommandPlaceholder, "", 2)
			fullCauseIDs = append(fullCauseIDs, id)
			fullCauseReasonByID[id] = reason
			fullAfter.CauseRecords[id] = CauseRecord{ID: id, TerminalKind: TerminalImpossible, DispositionReason: reason, SourceCommandID: MutationCommandPlaceholder, EventSeq: 2}
		}
		slices.Sort(fullCauseIDs)
		fullCauseDigest, _ := CauseSetIdentity(fullCauseIDs)
		fullImpossibleID, _ := ImpossibleEdgePathIdentity(fullCauseDigest, otherEdge.ID, otherReservationID)
		impossible := PathRecord{
			ID: fullImpossibleID, Kind: PathImpossibleEdge, State: PathImpossible,
			ParentPathID: parentID, SourceActivation: arrived.SourceActivation, Edge: &otherEdge,
			TargetReservationID: otherReservationID, CandidateID: otherCandidate.ID, ScopeID: arrived.ScopeID,
			CandidateLineage:   []CandidateLineageFrame{{ID: otherLineageID, ReservationID: otherReservationID, CandidateID: otherCandidate.ID}},
			CandidateLineageID: otherLineageID, LineageDepth: 1, ImpossibleCauseDigest: fullCauseDigest,
			CreatedSeq: 2, UpdatedSeq: 2,
		}
		fullAfter.Paths[impossible.ID] = impossible
		fullParent = fullAfter.Paths[parentID]
		fullParent.State = PathRouted
		fullParent.ProducedPathIDs = []PathID{arrived.ID, impossible.ID}
		slices.Sort(fullParent.ProducedPathIDs)
		fullParent.UpdatedSeq = 2
		fullDispositionID, _ := DispositionReceiptIdentity(parentID, PathLive, PathRouted, "exclusive_route", MutationCommandPlaceholder, "", 2)
		fullParent.Disposition = &DispositionReceipt{ID: fullDispositionID, PathID: parentID, FromState: PathLive, ToState: PathRouted, ReasonCode: "exclusive_route", CommandID: MutationCommandPlaceholder, EventSeq: 2}
		fullAfter.Paths[parentID] = fullParent
		fullAfter.CauseSets[fullCauseDigest] = CauseSetRecord{Digest: fullCauseDigest, CauseIDs: fullCauseIDs}
		fullBatch, err := NewMutationBatch(&fullBefore, &fullAfter, 2)
		if err != nil {
			t.Fatal(err)
		}
		settlement := makeTestCommand(t, CommandIdentity{
			RunID: aggregate.RunID, Kind: CommandSettleAttempt, PayloadSchema: 1,
			SourceActivationID: fullParent.SourceActivation.ID, SourceGeneration: 1, Attempt: 1,
			InputDigest: "perform", PlanDigest: "observation", ResultCode: "pass",
		}, CommandObserved)
		aggregate.Commands[settlement.ID] = settlement
		aggregate.Routing = &fullBefore
		fullView := MutationReplayView{Aggregate: aggregate, Checkpoint: CheckpointBinding{Generation: 13, Digest: strings.Repeat("d", 64)}}
		fullPlan := RoutePathsPlan{
			SettlementCommandID: settlement.ID, SourceActivationID: fullParent.SourceActivation.ID,
			SourceGeneration: 1, SourcePathID: parentID, Attempt: 1, CauseDigest: fullCauseDigest,
			ResultCode: "exclusive/pass", ProducedPathIDs: fullParent.ProducedPathIDs, Batch: fullBatch,
		}
		fullPayload, err := EncodeRoutePathsPayload(fullView, fullPlan)
		if err != nil {
			t.Fatal(err)
		}
		fullIdentity := CommandIdentity{
			RunID: aggregate.RunID, Kind: CommandRoutePaths, PayloadSchema: 1,
			SourceActivationID: fullParent.SourceActivation.ID, SourceGeneration: 1, SourcePathID: parentID, Attempt: 1,
			InputDigest: settlement.ID, CauseDigest: fullCauseDigest, PlanDigest: payloadDigest(fullPayload), ResultCode: "exclusive/pass",
		}
		fullCommand := commandWithPayload(t, fullIdentity, CommandObserved, fullPayload)
		fullView.Aggregate.Commands[fullCommand.ID] = fullCommand
		result, err := ReplayRoutePaths(fullView, fullCommand)
		if err != nil || result.Disposition != ReplayApplied {
			t.Fatalf("full impossible-edge route replay = %q, %v", result.Disposition, err)
		}
		materializedIDs := make([]CauseID, 0, len(fullCauseReasons))
		for _, reason := range fullCauseReasons {
			id, _ := CauseIdentity("", TerminalImpossible, reason, "", fullCommand.ID, "", 2)
			materializedIDs = append(materializedIDs, id)
		}
		slices.Sort(materializedIDs)
		unfixedOrder := make([]CauseID, 0, len(fullCauseIDs))
		for _, placeholderID := range fullCauseIDs {
			id, _ := CauseIdentity("", TerminalImpossible, fullCauseReasonByID[placeholderID], "", fullCommand.ID, "", 2)
			unfixedOrder = append(unfixedOrder, id)
		}
		if slices.IsSorted(unfixedOrder) {
			t.Fatal("multi-cause fixture does not exercise materialized member reordering")
		}
		materializedDigest, _ := CauseSetIdentity(materializedIDs)
		materializedSet, ok := result.Routing.CauseSets[materializedDigest]
		if !ok || materializedSet.Digest != materializedDigest || !slices.IsSorted(materializedSet.CauseIDs) || !slices.Equal(materializedSet.CauseIDs, materializedIDs) {
			t.Fatalf("materialized multi-cause set = %#v, want digest %s members %v", materializedSet, materializedDigest, materializedIDs)
		}
		fullView.Aggregate.Routing = &result.Routing
		result, err = ReplayRoutePaths(fullView, fullCommand)
		if err != nil || result.Disposition != ReplayAlreadyApplied {
			t.Fatalf("full impossible-edge idempotent replay = %q, %v", result.Disposition, err)
		}

		missing := Clone(after)
		delete(missing.CauseSets, causeDigest)
		delete(missing.CauseRecords, causeID)
		missingPlan, missingSource := makePlan(t, before, missing, []PathID{childID})
		if err := validateRouteMutationSet(before, missingPlan, missingSource); !errors.Is(err, ErrMutationInvalid) {
			t.Fatalf("missing impossible cause closure error = %v", err)
		}

		extra := Clone(after)
		extraDigest, _ := CauseSetIdentity(nil)
		extra.CauseSets[extraDigest] = CauseSetRecord{Digest: extraDigest, CauseIDs: []CauseID{}}
		extraPlan, extraSource := makePlan(t, before, extra, []PathID{childID})
		if err := validateRouteMutationSet(before, extraPlan, extraSource); !errors.Is(err, ErrMutationInvalid) {
			t.Fatalf("extra impossible cause set error = %v", err)
		}
	})

	t.Run("fork_event_sequences", func(t *testing.T) {
		before := makeSource()
		makeAfter := func(scopeSeq, reservationSeq int64) RoutingState {
			after := Clone(before)
			childID := PathID("fork-child")
			after.Paths[childID] = PathRecord{ID: childID, Kind: PathEdge, State: PathLive, ParentPathID: "source", SourceActivation: ActivationRef{ID: "activation", Generation: 1}, ScopeID: "fork-scope", CandidateLineage: []CandidateLineageFrame{}, CreatedSeq: 5, UpdatedSeq: 5}
			after.Scopes["fork-scope"] = ScopeRecord{ID: "fork-scope", RunID: "run", ParentScopeID: "root", ForkOutputPathID: "source", Generation: 1, ExpectedBranchEdgeIDs: []EdgeID{}, JoinReservationID: "fork-reservation", State: ScopeOpen, EventSeq: scopeSeq}
			after.Reservations["fork-reservation"] = ActivationReservation{ID: "fork-reservation", RunID: "run", NodeID: "join", ScopeID: "root", Generation: 1, JoinPolicy: JoinExclusive, Candidates: []CandidateRecord{}, PossibleSlots: []PossibleSlotRecord{}, State: ReservationOpen, EventSeq: reservationSeq}
			finishSource(&after, PathSplit, []PathID{childID})
			return after
		}
		valid := makeAfter(5, 5)
		plan, sourceAfter := makePlan(t, before, valid, []PathID{"fork-child"})
		if err := validateRouteMutationSet(before, plan, sourceAfter); err != nil {
			t.Fatalf("valid fork route: %v", err)
		}
		for _, test := range []struct {
			name                     string
			scopeSeq, reservationSeq int64
		}{
			{name: "wrong_scope", scopeSeq: 4, reservationSeq: 5},
			{name: "negative_scope", scopeSeq: -1, reservationSeq: 5},
			{name: "wrong_reservation", scopeSeq: 5, reservationSeq: 4},
			{name: "negative_reservation", scopeSeq: 5, reservationSeq: -1},
		} {
			t.Run(test.name, func(t *testing.T) {
				after := makeAfter(test.scopeSeq, test.reservationSeq)
				plan, sourceAfter := makePlan(t, before, after, []PathID{"fork-child"})
				if err := validateRouteMutationSet(before, plan, sourceAfter); !errors.Is(err, ErrMutationInvalid) {
					t.Fatalf("fork event sequence error = %v", err)
				}
			})
		}
	})
}

func TestTypedTerminalRouteRequiresCompleteBatchPostState(t *testing.T) {
	view := validGenesisFixture(t)
	pathID := view.Authority.Genesis.OutputPathID
	path := view.Routing.Paths[pathID]
	settlement := addTerminalAttemptCommands(t, &view, path.SourceActivation.ID, path.SourceActivation.Generation, 1, "pass", "", "observed")
	before := Clone(*view.Routing)
	beforePath := before.Paths[path.ID]
	beforePath.State = PathLive
	beforePath.UpdatedSeq = 1
	before.Paths[beforePath.ID] = beforePath
	afterTemplate := Clone(before)
	path.State = PathEnded
	path.UpdatedSeq = 2
	dispositionID, _ := DispositionReceiptIdentity(path.ID, PathLive, PathEnded, "completed", MutationCommandPlaceholder, "", 2)
	path.Disposition = &DispositionReceipt{ID: dispositionID, PathID: path.ID, FromState: PathLive, ToState: PathEnded, ReasonCode: "completed", CommandID: MutationCommandPlaceholder, EventSeq: 2}
	afterTemplate.Paths[path.ID] = path
	extraDigest, _ := CauseSetIdentity(nil)
	afterTemplate.CauseSets[extraDigest] = CauseSetRecord{Digest: extraDigest, CauseIDs: []CauseID{}}
	batch, err := NewMutationBatch(&before, &afterTemplate, 2)
	if err != nil {
		t.Fatal(err)
	}
	view.Routing = &before
	replayView := MutationReplayView{Aggregate: view, Checkpoint: CheckpointBinding{Generation: 12, Digest: strings.Repeat("c", 64)}}
	plan := RoutePathsPlan{
		SettlementCommandID: settlement.ID, SourceActivationID: path.SourceActivation.ID,
		SourceGeneration: path.SourceActivation.Generation, SourcePathID: path.ID, Attempt: 1,
		CauseDigest: "complete-route", ResultCode: "pass", ProducedPathIDs: []PathID{}, Batch: batch,
	}
	payload, err := EncodeRoutePathsPayload(replayView, plan)
	if err != nil {
		t.Fatal(err)
	}
	identity := CommandIdentity{
		RunID: view.RunID, Kind: CommandRoutePaths, PayloadSchema: 1,
		SourceActivationID: path.SourceActivation.ID, SourceGeneration: path.SourceActivation.Generation,
		SourcePathID: path.ID, Attempt: 1, InputDigest: settlement.ID,
		CauseDigest: plan.CauseDigest, PlanDigest: payloadDigest(payload), ResultCode: plan.ResultCode,
	}
	command := commandWithPayload(t, identity, CommandObserved, payload)
	replayView.Aggregate.Commands[command.ID] = command
	routing, disposition, err := batch.replay(&before, command.ID)
	if err != nil || disposition != ReplayApplied {
		t.Fatalf("full typed terminal batch application = %q, %v", disposition, err)
	}
	post := replayView.Aggregate
	post.Routing = &routing
	if report := ValidateAggregate(post); !report.Valid() {
		t.Fatalf("complete typed terminal batch diagnostics: %#v", report.Diagnostics)
	}
	for _, test := range []struct {
		name   string
		mutate func(*RoutingState)
	}{
		{name: "missing_non_source_record", mutate: func(state *RoutingState) { delete(state.CauseSets, extraDigest) }},
		{name: "different_non_source_record", mutate: func(state *RoutingState) {
			set := state.CauseSets[extraDigest]
			set.CauseIDs = []CauseID{"different"}
			state.CauseSets[extraDigest] = set
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			partial := Clone(routing)
			test.mutate(&partial)
			aggregate := post
			aggregate.Routing = &partial
			report := ValidateAggregate(aggregate)
			if !reportHasCode(report, "terminal_command_provenance") {
				t.Fatalf("partial typed terminal batch diagnostics: %#v", report.Diagnostics)
			}
		})
	}
}

func TestActivateAnyRealCandidateBoundaryAndMutationFormula(t *testing.T) {
	makePlan := func(count int) ActivateGenerationPlan {
		candidates := make([]CandidateRecord, count)
		losers := make([]CandidateID, 0, count-1)
		for index := range candidates {
			id := CandidateID(fmt.Sprintf("c-%04d", index))
			candidates[index] = CandidateRecord{ID: id, Kind: CandidateInboundEdge, MemberID: fmt.Sprintf("e-%04d", index), PossibleSlotIDs: []PossibleSlotID{}}
			if index > 0 {
				losers = append(losers, id)
			}
		}
		preArrived := make([]PathID, max(0, count-1))
		for index := range preArrived {
			preArrived[index] = PathID(fmt.Sprintf("p-%04d", index+1))
		}
		mutationCount := 1
		if count <= MaxAnyCandidates {
			mutationCount, _ = MutationCountAny(count, len(preArrived))
		}
		mutations := make([]RecordMutation, mutationCount)
		for index := range mutations {
			key := fmt.Sprintf("set-%04d", index)
			record, _ := json.Marshal(CauseSetRecord{Digest: key, CauseIDs: []CauseID{}})
			mutations[index] = RecordMutation{Kind: MutationCauseSet, Key: key, After: record}
		}
		return ActivateGenerationPlan{
			ReservationID: "reservation", Generation: 1, InputDigest: "fold", CauseDigest: "cause", JoinPolicy: JoinAny,
			InputPathIDs: []PathID{"winner"}, WinnerPathID: "winner", LosingCandidateIDs: losers, PreArrivedLoserPathIDs: preArrived,
			Candidates: candidates, PossibleSlots: []PossibleSlotRecord{},
			Batch: MutationBatch{EventSeq: 1, LogEntries: 1, BeforeDigest: strings.Repeat("a", 64), AfterDigest: strings.Repeat("b", 64), Mutations: mutations},
		}
	}
	maxPlan := makePlan(MaxAnyCandidates)
	if err := maxPlan.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(maxPlan.Batch.Mutations) != 4094 {
		t.Fatalf("maximum any mutations = %d", len(maxPlan.Batch.Mutations))
	}
	if err := makePlan(MaxAnyCandidates + 1).Validate(); !errors.Is(err, ErrMutationInvalid) {
		t.Fatalf("1,365-candidate error = %v", err)
	}
}

func TestActivateAnyReplayIsAtomicAndIdempotent(t *testing.T) {
	postView := validAnyFixture(t)
	r := firstAnyReservation(postView)
	activation := postView.Routing.Activations[r.Activation.ID]
	winnerID := activation.InputPathIDs[0]
	winner := postView.Routing.Paths[winnerID]
	_, detachment, loserID := anyLoser(postView)
	loser := postView.Routing.Paths[loserID]
	oldCommandID := r.CommandID
	delete(postView.Commands, oldCommandID)

	before := Clone(*postView.Routing)
	delete(before.Activations, activation.ID)
	delete(before.Paths, activation.OutputPathID)
	for key := range before.Detachments {
		delete(before.Detachments, key)
	}
	for key := range before.DetachmentSets {
		delete(before.DetachmentSets, key)
	}
	beforeWinner := before.Paths[winnerID]
	beforeWinner.State = PathArrived
	beforeWinner.ConsumedBy = nil
	beforeWinner.Disposition = nil
	beforeWinner.UpdatedSeq = beforeWinner.ArrivedSeq
	before.Paths[winnerID] = beforeWinner
	beforeLoser := before.Paths[loserID]
	beforeLoser.State = PathArrived
	beforeLoser.Disposition = nil
	beforeLoser.DetachedSink = nil
	beforeLoser.DetachmentSetID = ""
	beforeLoser.UpdatedSeq = beforeLoser.ArrivedSeq
	before.Paths[loserID] = beforeLoser
	beforeReservation := before.Reservations[r.ID]
	beforeReservation.State = ReservationOpen
	beforeReservation.Activation = nil
	beforeReservation.CommandID = ""
	beforeReservation.EventSeq = 0
	before.Reservations[r.ID] = beforeReservation
	beforeScope := before.Scopes[r.ReducesScopeID]
	beforeScope.State = ScopeOpen
	beforeScope.CloseReason = ScopeCloseNone
	beforeScope.ClosedByCommandID = ""
	beforeScope.EventSeq = 2
	before.Scopes[beforeScope.ID] = beforeScope

	afterTemplate := Clone(*postView.Routing)
	winner = afterTemplate.Paths[winnerID]
	winner.Disposition.CommandID = MutationCommandPlaceholder
	winner.Disposition.ID, _ = DispositionReceiptIdentity(winner.ID, winner.Disposition.FromState, winner.Disposition.ToState, winner.Disposition.ReasonCode, MutationCommandPlaceholder, "", 3)
	afterTemplate.Paths[winnerID] = winner
	loser = afterTemplate.Paths[loserID]
	loser.Disposition.CommandID = MutationCommandPlaceholder
	loser.Disposition.ID, _ = DispositionReceiptIdentity(loser.ID, loser.Disposition.FromState, loser.Disposition.ToState, loser.Disposition.ReasonCode, MutationCommandPlaceholder, "", 3)
	loser.DetachedSink.CommandID = MutationCommandPlaceholder
	afterTemplate.Paths[loserID] = loser
	for key, value := range afterTemplate.Detachments {
		value.CommandID = MutationCommandPlaceholder
		afterTemplate.Detachments[key] = value
	}
	r = afterTemplate.Reservations[r.ID]
	r.CommandID = MutationCommandPlaceholder
	afterTemplate.Reservations[r.ID] = r
	scope := afterTemplate.Scopes[r.ReducesScopeID]
	scope.ClosedByCommandID = MutationCommandPlaceholder
	afterTemplate.Scopes[scope.ID] = scope
	activation = afterTemplate.Activations[activation.ID]
	activation.CommandID = MutationCommandPlaceholder
	activation.Receipt.CommandID = MutationCommandPlaceholder
	activation.Receipt.ID, _ = ActivationReceiptIdentity(activation.ID, activation.ReservationID, activation.InputSetDigest, activation.OutputPathID, MutationCommandPlaceholder, 3)
	afterTemplate.Activations[activation.ID] = activation
	batch, err := NewMutationBatch(&before, &afterTemplate, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Mutations) != 8 {
		t.Fatalf("any batch mutations = %d, want 8", len(batch.Mutations))
	}

	postView.Routing = &before
	replayView := MutationReplayView{Aggregate: postView, Checkpoint: CheckpointBinding{Generation: 4, Digest: strings.Repeat("a", 64)}}
	losingCandidates := []CandidateID{detachment.CandidateID}
	foldDigest, _, causeDigest, err := activationFold(before, beforeReservation)
	if err != nil {
		t.Fatal(err)
	}
	plan := ActivateGenerationPlan{
		ReservationID: r.ID, Generation: 1, InputDigest: foldDigest, CauseDigest: causeDigest, JoinPolicy: JoinAny,
		InputPathIDs: []PathID{winnerID}, WinnerPathID: winnerID, LosingCandidateIDs: losingCandidates, PreArrivedLoserPathIDs: []PathID{loserID},
		Candidates: r.Candidates, PossibleSlots: r.PossibleSlots, Batch: batch,
	}
	payload, err := EncodeActivateGenerationPayload(replayView, plan)
	if err != nil {
		t.Fatal(err)
	}
	identity := CommandIdentity{RunID: postView.RunID, Kind: CommandActivateGeneration, PayloadSchema: 1, TargetReservationID: r.ID, TargetGeneration: 1, InputDigest: plan.InputDigest, CauseDigest: plan.CauseDigest, PlanDigest: payloadDigest(payload)}
	command := commandWithPayload(t, identity, CommandObserved, payload)
	replayView.Aggregate.Commands[command.ID] = command
	result, err := ReplayActivateGeneration(replayView, command)
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != ReplayApplied || result.Routing.Paths[loserID].State != PathDetachedSink || result.Routing.Paths[loserID].DetachedSink.CommandID != command.ID {
		t.Fatalf("any replay = %q, %#v", result.Disposition, result.Routing.Paths[loserID])
	}
	materialized, err := batch.materialize(command.ID)
	if err != nil {
		t.Fatal(err)
	}
	for mask := 1; mask < (1<<len(materialized))-1; mask++ {
		partial := Clone(before)
		for index, mutation := range materialized {
			if mask&(1<<index) != 0 {
				if err := applyRecordMutation(&partial, mutation); err != nil {
					t.Fatal(err)
				}
			}
		}
		replayView.Aggregate.Routing = &partial
		if _, err := ReplayActivateGeneration(replayView, command); !errors.Is(err, ErrMutationInconsistent) {
			t.Fatalf("partial any mask %#x error = %v", mask, err)
		}
	}
	replayView.Aggregate.Routing = &result.Routing
	result, err = ReplayActivateGeneration(replayView, command)
	if err != nil || result.Disposition != ReplayAlreadyApplied {
		t.Fatalf("any idempotent replay = %q, %v", result.Disposition, err)
	}

	partial := Clone(result.Routing)
	delete(partial.DetachmentSets, loser.DetachmentSetID)
	replayView.Aggregate.Routing = &partial
	if _, err := ReplayActivateGeneration(replayView, command); !errors.Is(err, ErrMutationInconsistent) {
		t.Fatalf("partial any replay error = %v", err)
	}
}

func TestActivateExclusiveRejectsBatchEventSeqDrift(t *testing.T) {
	postView, inputPathID, reservationID := validOpenArrivalFixture(t)
	activateOpenArrival(t, &postView, inputPathID, reservationID)
	postReservation := postView.Routing.Reservations[reservationID]
	activation := postView.Routing.Activations[postReservation.Activation.ID]
	delete(postView.Commands, postReservation.CommandID)

	before := Clone(*postView.Routing)
	delete(before.Activations, activation.ID)
	delete(before.Paths, activation.OutputPathID)
	input := before.Paths[inputPathID]
	input.State = PathArrived
	input.ConsumedBy = nil
	input.Disposition = nil
	input.UpdatedSeq = input.ArrivedSeq
	before.Paths[input.ID] = input
	reservation := before.Reservations[reservationID]
	reservation.State = ReservationOpen
	reservation.Activation = nil
	reservation.CommandID = ""
	reservation.EventSeq = 0
	before.Reservations[reservation.ID] = reservation

	afterTemplate := Clone(*postView.Routing)
	input = afterTemplate.Paths[inputPathID]
	input.Disposition.CommandID = MutationCommandPlaceholder
	input.Disposition.ID, _ = DispositionReceiptIdentity(input.ID, input.Disposition.FromState, input.Disposition.ToState, input.Disposition.ReasonCode, MutationCommandPlaceholder, "", uint64(input.Disposition.EventSeq))
	afterTemplate.Paths[input.ID] = input
	postReservation = afterTemplate.Reservations[reservationID]
	postReservation.CommandID = MutationCommandPlaceholder
	afterTemplate.Reservations[postReservation.ID] = postReservation
	activation = afterTemplate.Activations[activation.ID]
	activation.CommandID = MutationCommandPlaceholder
	activation.Receipt.CommandID = MutationCommandPlaceholder
	activation.Receipt.ID, _ = ActivationReceiptIdentity(activation.ID, activation.ReservationID, activation.InputSetDigest, activation.OutputPathID, MutationCommandPlaceholder, uint64(activation.Receipt.EventSeq))
	afterTemplate.Activations[activation.ID] = activation
	batch, err := NewMutationBatch(&before, &afterTemplate, 3)
	if err != nil {
		t.Fatal(err)
	}
	foldDigest, arrivals, causeDigest, err := activationFold(before, reservation)
	if err != nil {
		t.Fatal(err)
	}
	plan := ActivateGenerationPlan{
		ReservationID: reservation.ID, Generation: reservation.Generation, InputDigest: foldDigest, CauseDigest: causeDigest, JoinPolicy: JoinExclusive,
		InputPathIDs: arrivals, Candidates: reservation.Candidates, PossibleSlots: reservation.PossibleSlots, Batch: batch,
	}
	postView.Routing = &before
	replayView := MutationReplayView{Aggregate: postView, Checkpoint: CheckpointBinding{Generation: 5, Digest: strings.Repeat("5", 64)}}
	makeCommand := func(plan ActivateGenerationPlan) CommandRecord {
		payload, encodeErr := EncodeActivateGenerationPayload(replayView, plan)
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		identity := CommandIdentity{
			RunID: postView.RunID, Kind: CommandActivateGeneration, PayloadSchema: 1,
			TargetReservationID: reservation.ID, TargetGeneration: reservation.Generation,
			InputDigest: plan.InputDigest, CauseDigest: plan.CauseDigest, PlanDigest: payloadDigest(payload),
		}
		return commandWithPayload(t, identity, CommandObserved, payload)
	}
	command := makeCommand(plan)
	replayView.Aggregate.Commands[command.ID] = command
	if _, err := ReplayActivateGeneration(replayView, command); err != nil {
		t.Fatalf("valid exclusive activation replay: %v", err)
	}

	driftedPlan := plan
	driftedPlan.Batch.EventSeq = 99
	driftedCommand := makeCommand(driftedPlan)
	replayView.Aggregate.Commands[driftedCommand.ID] = driftedCommand
	if _, err := ReplayActivateGeneration(replayView, driftedCommand); !errors.Is(err, ErrMutationInvalid) {
		t.Fatalf("exclusive activation batch EventSeq drift error = %v", err)
	}
}

func TestPropagationDuplicateShardDeterminism(t *testing.T) {
	frontier := []CandidateClosureKey{"key-a", "key-b"}
	planDigest, _ := PropagationPlanIdentity("reservation", "candidate", "cause", 0, frontier)
	id, _ := PropagationIntentIdentity("cause", 0, planDigest)
	intent := PropagationIntent{
		ID: id, RootReservationID: "reservation", RootCandidateID: "candidate", RootCauseDigest: "cause",
		Shard: 0, Cursor: 0, Frontier: frontier, PlanDigest: planDigest, State: PropagationPending,
		CommandID: MutationCommandPlaceholder, EventSeq: 1,
	}
	normalized, err := normalizePropagationIntents([]PropagationIntent{intent, intent})
	if err != nil || len(normalized) != 1 {
		t.Fatalf("identical duplicates = %#v, %v", normalized, err)
	}
	conflict := intent
	conflict.Cursor = 1
	if _, err := normalizePropagationIntents([]PropagationIntent{conflict, intent}); !errors.Is(err, ErrMutationInconsistent) {
		t.Fatalf("different-byte duplicate error = %v", err)
	}
}

func TestPropagateClosureReplayBindsRootFrontierAndIntent(t *testing.T) {
	view, sourcePathID, reservationID := validOpenArrivalFixture(t)
	reservation := view.Routing.Reservations[reservationID]
	candidate := reservation.Candidates[0]
	causeDigest, _ := CauseSetIdentity(nil)
	view.Routing.CauseSets[causeDigest] = CauseSetRecord{Digest: causeDigest, CauseIDs: []CauseID{}}
	before := Clone(*view.Routing)
	frontierKey, _ := CandidateClosureKeyIdentity(reservationID, candidate.ID)
	frontier := []CandidateClosureKey{frontierKey}
	intentPlan, _ := PropagationPlanIdentity(reservationID, candidate.ID, causeDigest, 0, frontier)
	intentID, _ := PropagationIntentIdentity(causeDigest, 0, intentPlan)
	intent := PropagationIntent{
		ID: intentID, RootReservationID: reservationID, RootCandidateID: candidate.ID, RootCauseDigest: causeDigest,
		Shard: 0, Cursor: 1, Frontier: frontier, PlanDigest: intentPlan, State: PropagationComplete,
		CommandID: MutationCommandPlaceholder, EventSeq: 3,
	}
	afterTemplate := Clone(before)
	afterTemplate.Propagation[intentID] = intent
	batch, err := NewMutationBatch(&before, &afterTemplate, 3)
	if err != nil {
		t.Fatal(err)
	}
	view.Routing = &before
	replayView := MutationReplayView{Aggregate: view, Checkpoint: CheckpointBinding{Generation: 9, Digest: strings.Repeat("9", 64)}}
	plan := PropagateClosurePlan{
		SourcePathID: sourcePathID, TargetReservationID: reservationID, TargetGeneration: 1,
		InputDigest: intentPlan, CauseDigest: causeDigest, RootReservationID: reservationID, RootCandidateID: candidate.ID, RootCauseDigest: causeDigest,
		Intents: []PropagationIntent{intent, intent}, Batch: batch,
	}
	payload, err := EncodePropagateClosurePayload(replayView, plan)
	if err != nil {
		t.Fatal(err)
	}
	identity := CommandIdentity{
		RunID: view.RunID, Kind: CommandPropagateCandidateClosure, PayloadSchema: 1,
		SourcePathID: sourcePathID, TargetReservationID: reservationID, TargetGeneration: 1,
		InputDigest: intentPlan, CauseDigest: causeDigest, PlanDigest: payloadDigest(payload),
	}
	command := commandWithPayload(t, identity, CommandObserved, payload)
	replayView.Aggregate.Commands[command.ID] = command

	badAfter := Clone(afterTemplate)
	for scopeID, scope := range badAfter.Scopes {
		scope.EventSeq++
		badAfter.Scopes[scopeID] = scope
		break
	}
	badBatch, err := NewMutationBatch(&before, &badAfter, 3)
	if err != nil {
		t.Fatal(err)
	}
	badPlan := plan
	badPlan.Batch = badBatch
	badPayload, err := EncodePropagateClosurePayload(replayView, badPlan)
	if err != nil {
		t.Fatal(err)
	}
	badIdentity := identity
	badIdentity.PlanDigest = payloadDigest(badPayload)
	badCommand := commandWithPayload(t, badIdentity, CommandObserved, badPayload)
	replayView.Aggregate.Commands[badCommand.ID] = badCommand
	if err := ValidatePropagateClosureCommand(replayView, badCommand); !errors.Is(err, ErrMutationInvalid) {
		t.Fatalf("propagation unrelated scope mutation error = %v", err)
	}
	delete(replayView.Aggregate.Commands, badCommand.ID)

	badPathAfter := Clone(afterTemplate)
	unrelatedArrival := badPathAfter.Paths[sourcePathID]
	unrelatedArrival.UpdatedSeq = 3
	badPathAfter.Paths[sourcePathID] = unrelatedArrival
	badPathBatch, err := NewMutationBatch(&before, &badPathAfter, 3)
	if err != nil {
		t.Fatal(err)
	}
	badPathPlan := plan
	badPathPlan.Batch = badPathBatch
	badPathPayload, err := EncodePropagateClosurePayload(replayView, badPathPlan)
	if err != nil {
		t.Fatal(err)
	}
	badPathIdentity := identity
	badPathIdentity.PlanDigest = payloadDigest(badPathPayload)
	badPathCommand := commandWithPayload(t, badPathIdentity, CommandObserved, badPathPayload)
	replayView.Aggregate.Commands[badPathCommand.ID] = badPathCommand
	if err := ValidatePropagateClosureCommand(replayView, badPathCommand); !errors.Is(err, ErrMutationInvalid) {
		t.Fatalf("propagation unrelated arrived-path rewrite error = %v", err)
	}
	delete(replayView.Aggregate.Commands, badPathCommand.ID)

	result, err := ReplayPropagateClosure(replayView, command)
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != ReplayApplied || result.Routing.Propagation[intentID].CommandID != command.ID {
		t.Fatalf("propagation replay = %q, %#v", result.Disposition, result.Routing.Propagation[intentID])
	}
	replayView.Aggregate.Routing = &result.Routing
	result, err = ReplayPropagateClosure(replayView, command)
	if err != nil || result.Disposition != ReplayAlreadyApplied {
		t.Fatalf("propagation idempotent replay = %q, %v", result.Disposition, err)
	}
}

func TestPropagateClosurePreservesExistingClosureAuthority(t *testing.T) {
	view := validAllArrivedNonSuccessFixture(t)
	var closure CandidateClosure
	for _, candidateClosure := range view.Routing.CandidateClosures {
		closure = candidateClosure
		break
	}
	if closure.ID == "" {
		t.Fatal("existing candidate closure missing")
	}
	reservation := view.Routing.Reservations[closure.Key.ReservationID]
	before := Clone(*view.Routing)
	frontier := []CandidateClosureKey{closure.Key.ID}
	intentPlan, _ := PropagationPlanIdentity(reservation.ID, closure.Key.CandidateID, closure.CauseDigest, 0, frontier)
	intentID, _ := PropagationIntentIdentity(closure.CauseDigest, 0, intentPlan)
	intent := PropagationIntent{
		ID: intentID, RootReservationID: reservation.ID, RootCandidateID: closure.Key.CandidateID, RootCauseDigest: closure.CauseDigest,
		Shard: 0, Cursor: 1, Frontier: frontier, PlanDigest: intentPlan, State: PropagationComplete,
		CommandID: MutationCommandPlaceholder, EventSeq: 99,
	}
	positiveAfter := Clone(before)
	positiveAfter.Propagation[intent.ID] = intent
	positiveBatch, err := NewMutationBatch(&before, &positiveAfter, 99)
	if err != nil {
		t.Fatal(err)
	}
	replayView := MutationReplayView{Aggregate: view, Checkpoint: CheckpointBinding{Generation: 10, Digest: strings.Repeat("a", 64)}}
	makeCommand := func(batch MutationBatch) CommandRecord {
		plan := PropagateClosurePlan{
			TargetReservationID: reservation.ID, TargetGeneration: reservation.Generation,
			InputDigest: intentPlan, CauseDigest: closure.CauseDigest,
			RootReservationID: reservation.ID, RootCandidateID: closure.Key.CandidateID, RootCauseDigest: closure.CauseDigest,
			Intents: []PropagationIntent{intent}, Batch: batch,
		}
		payload, encodeErr := EncodePropagateClosurePayload(replayView, plan)
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		identity := CommandIdentity{
			RunID: view.RunID, Kind: CommandPropagateCandidateClosure, PayloadSchema: 1,
			TargetReservationID: reservation.ID, TargetGeneration: reservation.Generation,
			InputDigest: intentPlan, CauseDigest: closure.CauseDigest, PlanDigest: payloadDigest(payload),
		}
		return commandWithPayload(t, identity, CommandObserved, payload)
	}
	positiveCommand := makeCommand(positiveBatch)
	positiveView := replayView
	positiveView.Aggregate.Commands = cloneMap(view.Commands)
	positiveView.Aggregate.Commands[positiveCommand.ID] = positiveCommand
	result, err := ReplayPropagateClosure(positiveView, positiveCommand)
	if err != nil || result.Disposition != ReplayApplied || !canonicalEqual(result.Routing.CandidateClosures[closure.Key.ID], closure) {
		t.Fatalf("valid propagation over existing closure = %q, %v", result.Disposition, err)
	}

	badAfter := Clone(positiveAfter)
	rewritten := badAfter.CandidateClosures[closure.Key.ID]
	rewritten.CommandID = MutationCommandPlaceholder
	rewritten.EventSeq = 17
	badAfter.CandidateClosures[closure.Key.ID] = rewritten
	badBatch, err := NewMutationBatch(&before, &badAfter, 99)
	if err != nil {
		t.Fatal(err)
	}
	badCommand := makeCommand(badBatch)
	badView := replayView
	badView.Aggregate.Commands = cloneMap(view.Commands)
	badView.Aggregate.Commands[badCommand.ID] = badCommand
	if _, err := ReplayPropagateClosure(badView, badCommand); !errors.Is(err, ErrMutationInvalid) {
		t.Fatalf("existing closure authority rewrite error = %v", err)
	}
}

func TestSettleDetachedSinkCreatesLateArrivalAtomically(t *testing.T) {
	postView := validSlowAnyFixture(t, false)
	var late PathRecord
	for _, path := range postView.Routing.Paths {
		if path.State == PathDetachedSink && path.DetachedSink != nil && path.DetachedSink.ReasonCode == "late_any_arrival" {
			late = path
			break
		}
	}
	if late.ID == "" {
		t.Fatal("late detached sink missing")
	}
	before := Clone(*postView.Routing)
	delete(before.Paths, late.ID)
	parent := before.Paths[late.ParentPathID]
	parent.State = PathLive
	parent.ProducedPathIDs = nil
	parent.Disposition = nil
	parent.UpdatedSeq = parent.CreatedSeq
	before.Paths[parent.ID] = parent
	afterTemplate := Clone(*postView.Routing)
	afterParent := afterTemplate.Paths[late.ParentPathID]
	afterParent.Disposition.CommandID = MutationCommandPlaceholder
	afterParent.Disposition.ID, _ = DispositionReceiptIdentity(afterParent.ID, PathLive, PathRouted, afterParent.Disposition.ReasonCode, MutationCommandPlaceholder, "", uint64(afterParent.UpdatedSeq))
	afterTemplate.Paths[afterParent.ID] = afterParent
	templatePath := afterTemplate.Paths[late.ID]
	templatePath.Disposition.CommandID = MutationCommandPlaceholder
	templatePath.Disposition.ID, _ = DispositionReceiptIdentity(templatePath.ID, PathArrived, PathDetachedSink, "late_any_arrival", MutationCommandPlaceholder, "", uint64(templatePath.UpdatedSeq))
	templatePath.DetachedSink.CommandID = MutationCommandPlaceholder
	afterTemplate.Paths[late.ID] = templatePath
	batch, err := NewMutationBatch(&before, &afterTemplate, late.UpdatedSeq)
	if err != nil {
		t.Fatal(err)
	}
	postView.Routing = &before
	delete(postView.Commands, late.Disposition.CommandID)
	replayView := MutationReplayView{Aggregate: postView, Checkpoint: CheckpointBinding{Generation: 8, Digest: strings.Repeat("8", 64)}}
	plan := SettleDetachedSinkPlan{
		SourcePathID: late.ID, ReservationID: late.TargetReservationID, Generation: late.SourceActivation.Generation,
		DetachmentSetID: late.DetachmentSetID, DetachmentID: late.DetachedSink.DetachmentID,
		ResultCode: "detached", Batch: batch,
	}
	payload, err := EncodeSettleDetachedSinkPayload(replayView, plan)
	if err != nil {
		t.Fatal(err)
	}
	identity := CommandIdentity{
		RunID: postView.RunID, Kind: CommandSettleDetachedSink, PayloadSchema: 1,
		SourcePathID: late.ID, TargetReservationID: late.TargetReservationID, TargetGeneration: late.SourceActivation.Generation,
		InputDigest: late.DetachmentSetID, PlanDigest: payloadDigest(payload), ResultCode: "detached",
	}
	command := commandWithPayload(t, identity, CommandObserved, payload)
	replayView.Aggregate.Commands[command.ID] = command
	alternateView := replayView
	alternateView.Checkpoint = CheckpointBinding{Generation: replayView.Checkpoint.Generation + 1, Digest: strings.Repeat("7", 64)}
	alternatePayload, err := EncodeSettleDetachedSinkPayload(alternateView, plan)
	if err != nil {
		t.Fatal(err)
	}
	alternateIdentity := identity
	alternateIdentity.PlanDigest = payloadDigest(alternatePayload)
	alternateCommand := commandWithPayload(t, alternateIdentity, CommandObserved, alternatePayload)
	if alternateCommand.ID == command.ID || alternateCommand.IdempotencyKey == command.IdempotencyKey {
		t.Fatal("distinct detached-sink checkpoint bindings collided")
	}
	result, err := ReplaySettleDetachedSink(replayView, command)
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != ReplayApplied || result.Routing.Paths[late.ID].DetachedSink.CommandID != command.ID {
		t.Fatalf("late sink replay = %q, %#v", result.Disposition, result.Routing.Paths[late.ID])
	}
	replayView.Aggregate.Routing = &result.Routing
	result, err = ReplaySettleDetachedSink(replayView, command)
	if err != nil || result.Disposition != ReplayAlreadyApplied {
		t.Fatalf("late sink idempotent replay = %q, %v", result.Disposition, err)
	}
}

func TestCompleteRunClaimObservationAndRecovery(t *testing.T) {
	aggregate := validGenesisFixture(t)
	checkpoint := CheckpointBinding{Generation: 3, Digest: strings.Repeat("d", 64)}
	pre := CompletionReplayView{
		Aggregate: aggregate, Checkpoint: checkpoint,
		CheckpointJSON: completionCheckpoint(t, "running", 1, "sum-1", nil),
		RunStatus:      "running", LastLogSeq: 1, LogChecksum: "sum-1",
	}
	bindCompletionCheckpoint(t, &pre)
	planned, err := PlanCompleteRun(pre)
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := RecoverCompleteRun(pre)
	if err != nil || recovery.Phase != CompletionReadyToClaim || !canonicalEqual(recovery.Command, planned) {
		t.Fatalf("preclaim recovery = %#v, %v", recovery, err)
	}
	nonderived := pre
	nonderived.Checkpoint = CheckpointBinding{Generation: pre.Checkpoint.Generation + 1, Digest: strings.Repeat("e", 64)}
	if _, err := PlanCompleteRun(nonderived); !errors.Is(err, ErrMutationInvalid) {
		t.Fatalf("completion non-derived planning checkpoint error = %v", err)
	}
	ghostCommand := makeTestCommand(t, CommandIdentity{
		RunID: pre.Aggregate.RunID, Kind: CommandPerformAttempt, PayloadSchema: 1,
		SourceActivationID: pre.Aggregate.Authority.Genesis.ActivationID, SourceGeneration: 1, Attempt: 1, PlanDigest: "ghost-work",
	}, CommandIssued)
	ghost := pre
	ghost.CheckpointJSON = completionCheckpoint(t, "running", 1, "sum-1", map[string]CommandRecord{ghostCommand.ID: ghostCommand})
	bindCompletionCheckpoint(t, &ghost)
	if _, err := PlanCompleteRun(ghost); !errors.Is(err, ErrMutationInconsistent) {
		t.Fatalf("completion planned with checkpoint-only active command: %v", err)
	}
	if _, err := RecoverCompleteRun(ghost); !errors.Is(err, ErrMutationInconsistent) {
		t.Fatalf("completion recovery ignored checkpoint-only active command: %v", err)
	}
	checkpointOnlyTerminal := ghostCommand
	checkpointOnlyTerminal.State = CommandObserved
	terminalOnly := pre
	terminalOnly.CheckpointJSON = completionCheckpoint(t, "running", 1, "sum-1", map[string]CommandRecord{checkpointOnlyTerminal.ID: checkpointOnlyTerminal})
	bindCompletionCheckpoint(t, &terminalOnly)
	if _, err := PlanCompleteRun(terminalOnly); !errors.Is(err, ErrMutationInconsistent) {
		t.Fatalf("completion planned with checkpoint-only terminal command: %v", err)
	}

	driftCheckpoint := checkpointOnlyTerminal
	driftAggregate := checkpointOnlyTerminal
	driftAggregate.State = CommandReconciled
	drift := pre
	drift.Aggregate.Commands = cloneMap(pre.Aggregate.Commands)
	drift.Aggregate.Commands[driftAggregate.ID] = driftAggregate
	drift.CheckpointJSON = completionCheckpoint(t, "running", 1, "sum-1", map[string]CommandRecord{driftCheckpoint.ID: driftCheckpoint})
	bindCompletionCheckpoint(t, &drift)
	if err := validateCompletionView(drift); !errors.Is(err, ErrMutationInconsistent) {
		t.Fatalf("checkpoint/aggregate terminal command byte drift error = %v", err)
	}
	if err := validateCompletionView(pre); err != nil {
		t.Fatalf("aggregate-only terminal command history rejected: %v", err)
	}
	for _, status := range []string{"paused", "dirty", "inconsistent", "completed"} {
		unsafe := pre
		unsafe.RunStatus = status
		unsafe.CheckpointJSON = completionCheckpoint(t, status, unsafe.LastLogSeq, unsafe.LogChecksum, nil)
		if _, err := PlanCompleteRun(unsafe); !errors.Is(err, ErrMutationInconsistent) {
			t.Fatalf("completion planned from unsafe status %q: %v", status, err)
		}
	}

	claimed := pre
	claimed.Aggregate.Commands = cloneMap(pre.Aggregate.Commands)
	claimed.Aggregate.Commands[planned.ID] = planned
	claimed.CheckpointJSON = completionCheckpoint(t, "running", 1, "sum-1", map[string]CommandRecord{planned.ID: planned})
	if err := validateCompletionView(claimed); err != nil {
		t.Fatalf("reconciled checkpoint/aggregate completion command: %v", err)
	}
	recovery, err = RecoverCompleteRun(claimed)
	if err != nil || recovery.Phase != CompletionReadyToObserve {
		t.Fatalf("claim recovery = %#v, %v", recovery, err)
	}
	foreignRun := claimed
	foreignRun.Aggregate.RunID = "another-run"
	if _, err := ValidateCompleteRunCommand(foreignRun, planned); !errors.Is(err, ErrMutationInvalid) {
		t.Fatalf("foreign-run completion error = %v", err)
	}
	var forgedPayload CompleteRunCommandPayload
	if err := json.Unmarshal(planned.Payload, &forgedPayload); err != nil {
		t.Fatal(err)
	}
	forgedPayload.Checkpoint = nonderived.Checkpoint
	forgedBytes, err := json.Marshal(forgedPayload)
	if err != nil {
		t.Fatal(err)
	}
	forged := commandWithPayload(t, planned.Identity, planned.State, forgedBytes)
	forgedView := claimed
	forgedView.Aggregate.Commands = cloneMap(claimed.Aggregate.Commands)
	forgedView.Aggregate.Commands[forged.ID] = forged
	if _, err := ValidateCompleteRunCommand(forgedView, forged); !errors.Is(err, ErrMutationInvalid) {
		t.Fatalf("same-identity different completion checkpoint error = %v", err)
	}
	for _, status := range []string{"dirty", "inconsistent"} {
		unsafe := claimed
		unsafe.RunStatus = status
		unsafe.LastLogSeq = 2
		unsafe.LogChecksum = "sum-2"
		unsafe.CheckpointJSON = completionCheckpoint(t, status, 2, "sum-2", map[string]CommandRecord{planned.ID: planned})
		if _, err := RecoverCompleteRun(unsafe); !errors.Is(err, ErrMutationInconsistent) {
			t.Fatalf("completion recovered across %s durable drift: %v", status, err)
		}
	}
	drifted := claimed
	drifted.LastLogSeq = 2
	drifted.LogChecksum = "sum-2"
	drifted.CheckpointJSON = completionCheckpoint(t, "running", 2, "sum-2", map[string]CommandRecord{planned.ID: planned})
	if _, err := RecoverCompleteRun(drifted); !errors.Is(err, ErrMutationInconsistent) {
		t.Fatalf("completion recovered across log-anchor drift: %v", err)
	}

	observedCommand := planned
	observedCommand.State = CommandObserved
	observed := claimed
	observed.Aggregate.Commands = cloneMap(claimed.Aggregate.Commands)
	observed.Aggregate.Commands[planned.ID] = observedCommand
	observed.RunStatus = planned.Identity.ResultCode
	observed.CheckpointJSON = completionCheckpoint(t, observed.RunStatus, 1, "sum-1", map[string]CommandRecord{planned.ID: observedCommand})
	recovery, err = RecoverCompleteRun(observed)
	if err != nil || recovery.Phase != CompletionRecovered || recovery.Result != "completed" {
		t.Fatalf("observed recovery = %#v, %v", recovery, err)
	}

	partial := claimed
	partial.RunStatus = planned.Identity.ResultCode
	if _, err := RecoverCompleteRun(partial); !errors.Is(err, ErrMutationInconsistent) {
		t.Fatalf("terminal-with-issued error = %v", err)
	}
	partial = observed
	partial.RunStatus = "running"
	if _, err := RecoverCompleteRun(partial); !errors.Is(err, ErrMutationInconsistent) {
		t.Fatalf("observed-with-running error = %v", err)
	}

	multiple := claimed
	multiple.Aggregate.Commands = cloneMap(claimed.Aggregate.Commands)
	other := planned
	other.ID = strings.Repeat("e", 64)
	multiple.Aggregate.Commands[other.ID] = other
	if _, err := RecoverCompleteRun(multiple); !errors.Is(err, ErrMutationInconsistent) {
		t.Fatalf("multiple completion error = %v", err)
	}

	missingSelf := claimed
	missingSelf.CheckpointJSON = pre.CheckpointJSON
	if _, err := RecoverCompleteRun(missingSelf); err == nil {
		t.Fatal("claimed completion without checkpoint self accepted")
	}
	extraCheckpoint := observed
	var raw map[string]any
	if err := json.Unmarshal(observed.CheckpointJSON, &raw); err != nil {
		t.Fatal(err)
	}
	raw["unexpectedDurableState"] = true
	extraCheckpoint.CheckpointJSON, _ = json.Marshal(raw)
	if _, err := RecoverCompleteRun(extraCheckpoint); !errors.Is(err, ErrMutationInconsistent) {
		t.Fatalf("extra checkpoint state error = %v", err)
	}
	terminalWithoutCommand := pre
	terminalWithoutCommand.RunStatus = "completed"
	terminalWithoutCommand.CheckpointJSON = completionCheckpoint(t, "completed", 2, "sum-2", nil)
	if _, err := RecoverCompleteRun(terminalWithoutCommand); !errors.Is(err, ErrMutationInconsistent) {
		t.Fatalf("terminal without command error = %v", err)
	}
	active := pre
	active.Aggregate.Commands = cloneMap(pre.Aggregate.Commands)
	activeCommand := makeTestCommand(t, CommandIdentity{RunID: active.Aggregate.RunID, Kind: CommandPerformAttempt, PayloadSchema: 1, SourceActivationID: active.Aggregate.Authority.Genesis.ActivationID, SourceGeneration: 1, Attempt: 1, PlanDigest: "work"}, CommandIssued)
	active.Aggregate.Commands[activeCommand.ID] = activeCommand
	if _, err := PlanCompleteRun(active); err == nil {
		t.Fatal("completion planned with another active command")
	}
}

func commandWithPayload(t *testing.T, identity CommandIdentity, state CommandState, payload []byte) CommandRecord {
	t.Helper()
	id, err := CommandIdentityDigest(identity)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	return CommandRecord{ID: id, IdempotencyKey: CommandIdempotencyKey(identity.Kind, id), Identity: identity, Payload: payload, PayloadHash: hex.EncodeToString(sum[:]), State: state}
}

func bindCompletionCheckpoint(t *testing.T, view *CompletionReplayView) {
	t.Helper()
	basis, err := computeCompletionBasis(*view, CompletionBasis{
		BasisRunStatus:   view.RunStatus,
		BasisLastLogSeq:  view.LastLogSeq,
		BasisLogChecksum: view.LogChecksum,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	view.Checkpoint = completionBasisCheckpoint(basis)
}

func firstAnyReservation(view AggregateView) ActivationReservation {
	for _, reservation := range view.Routing.Reservations {
		if reservation.JoinPolicy == JoinAny {
			return reservation
		}
	}
	panic("any reservation missing")
}

func completionCheckpoint(t *testing.T, status string, seq uint64, checksum string, commands map[string]CommandRecord) []byte {
	t.Helper()
	if commands == nil {
		commands = map[string]CommandRecord{}
	}
	data, err := json.Marshal(map[string]any{
		"schema": 6, "status": status, "lastLogSeq": seq, "logChecksum": checksum,
		"outstandingCommands": commands, "nodes": map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestMutationPayloadAndCheckpointLimits(t *testing.T) {
	view := MutationReplayView{Aggregate: AggregateView{TemplateRef: "template", TemplateSourceHash: strings.Repeat("f", 64)}, Checkpoint: CheckpointBinding{Generation: 1, Digest: strings.Repeat("a", 64)}}
	plan := RoutePathsPlan{ResultCode: strings.Repeat("x", MaxCommandPayloadBytes)}
	if _, err := EncodeRoutePathsPayload(view, plan); err == nil {
		t.Fatal("oversized typed mutation payload accepted")
	}
	checkpoint := make([]byte, MaxCheckpointBytes+1)
	completion := CompletionReplayView{CheckpointJSON: checkpoint}
	if err := validateCompletionView(completion); err == nil {
		t.Fatal("oversized completion checkpoint accepted")
	}
}

func TestAnyPostStateSetsAreExhaustive(t *testing.T) {
	values := []int{0, 1, 4}
	for _, preArrived := range values {
		t.Run(fmt.Sprintf("pre_arrived_%d", preArrived), func(t *testing.T) {
			count := 5
			got, err := MutationCountAny(count, preArrived)
			if err != nil {
				t.Fatal(err)
			}
			if got != 2*count+preArrived+3 {
				t.Fatalf("count = %d", got)
			}
		})
	}
}

func TestSortedPlanListsRejectEveryPermutationDrift(t *testing.T) {
	plan := ActivateGenerationPlan{
		ReservationID: "r", Generation: 1, InputDigest: "i", CauseDigest: "c", JoinPolicy: JoinAll,
		InputPathIDs: []PathID{"a", "b"}, Candidates: []CandidateRecord{{ID: "a", PossibleSlotIDs: []PossibleSlotID{}}, {ID: "b", PossibleSlotIDs: []PossibleSlotID{}}}, PossibleSlots: []PossibleSlotRecord{},
		Batch: MutationBatch{EventSeq: 1, LogEntries: 1, BeforeDigest: strings.Repeat("a", 64), AfterDigest: strings.Repeat("b", 64), Mutations: []RecordMutation{{Kind: MutationCauseSet, Key: "x", After: json.RawMessage(`{"digest":"x","causeIds":[]}`)}}},
	}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	slices.Reverse(plan.InputPathIDs)
	if err := plan.Validate(); !errors.Is(err, ErrMutationInvalid) {
		t.Fatalf("permuted input error = %v", err)
	}
}
