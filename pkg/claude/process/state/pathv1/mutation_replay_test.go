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
	afterParent.Disposition.ID, _ = DispositionReceiptIdentity(parentID, PathLive, PathRouted, "route", MutationCommandPlaceholder, "", 2)
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
	afterTemplate := Clone(*postView.Routing)
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
		InputDigest: late.DetachmentSetID, ResultCode: "detached",
	}
	command := commandWithPayload(t, identity, CommandObserved, payload)
	replayView.Aggregate.Commands[command.ID] = command
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
	planned, err := PlanCompleteRun(pre)
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := RecoverCompleteRun(pre)
	if err != nil || recovery.Phase != CompletionReadyToClaim || !canonicalEqual(recovery.Command, planned) {
		t.Fatalf("preclaim recovery = %#v, %v", recovery, err)
	}

	claimed := pre
	claimed.Aggregate.Commands = cloneMap(pre.Aggregate.Commands)
	claimed.Aggregate.Commands[planned.ID] = planned
	claimed.CheckpointJSON = completionCheckpoint(t, "running", 2, "sum-2", map[string]CommandRecord{planned.ID: planned})
	claimed.LastLogSeq = 2
	claimed.LogChecksum = "sum-2"
	recovery, err = RecoverCompleteRun(claimed)
	if err != nil || recovery.Phase != CompletionReadyToObserve {
		t.Fatalf("claim recovery = %#v, %v", recovery, err)
	}

	observedCommand := planned
	observedCommand.State = CommandObserved
	observed := claimed
	observed.Aggregate.Commands = cloneMap(claimed.Aggregate.Commands)
	observed.Aggregate.Commands[planned.ID] = observedCommand
	observed.RunStatus = planned.Identity.ResultCode
	observed.LastLogSeq = 3
	observed.LogChecksum = "sum-3"
	observed.CheckpointJSON = completionCheckpoint(t, observed.RunStatus, 3, "sum-3", map[string]CommandRecord{planned.ID: observedCommand})
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
