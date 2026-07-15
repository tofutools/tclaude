package pathv1

import (
	"fmt"
	"slices"
)

func terminalKindForPath(state PathState) TerminalKind {
	switch state {
	case PathFailed:
		return TerminalFailed
	case PathCanceled:
		return TerminalCanceled
	case PathSkipped:
		return TerminalSkipped
	default:
		return ""
	}
}

func (i *aggregateIndex) validateCausesAndClosures() {
	for _, id := range sortedMapKeys(i.view.Routing.CauseRecords) {
		cause := i.view.Routing.CauseRecords[id]
		path := "causeRecords." + id
		if cause.ID != id || !cause.TerminalKind.Valid() || cause.EventSeq < 0 || cause.DispositionReason == "" {
			i.c.add("cause_shape", path, "cause key/kind/sequence/reason is invalid")
		}
		seq, err := eventUint(cause.EventSeq)
		want := ""
		if err == nil {
			want, err = CauseIdentity(cause.SourcePathID, cause.TerminalKind, cause.DispositionReason, cause.SourceActivationID, cause.SourceCommandID, cause.AdminRecordID, seq)
		}
		if err != nil || want != cause.ID {
			i.c.add("cause_identity", path, "cause identity does not recompute")
		}
		if cause.SourcePathID != "" {
			p, ok := i.view.Routing.Paths[cause.SourcePathID]
			if !ok {
				i.c.add("cause_source_missing", path, "source path %q is missing", cause.SourcePathID)
			} else if p.TerminalCauseID == cause.ID {
				if !p.State.TerminalNonSuccess() || terminalKindForPath(p.State) != cause.TerminalKind || p.Disposition == nil || p.Disposition.ReasonCode != cause.DispositionReason || p.Disposition.CommandID != cause.SourceCommandID || p.Disposition.AdminRecordID != cause.AdminRecordID || p.Disposition.EventSeq != cause.EventSeq || p.SourceActivation.ID != cause.SourceActivationID {
					i.c.add("terminal_cause_provenance", path, "cause does not exactly match terminal path disposition")
				}
			}
		} else if cause.SourceActivationID != "" || cause.AdminRecordID != "" || cause.SourceCommandID == "" {
			i.c.add("join_cause_authority", path, "join-level cause must be automatic command authority with empty source/admin")
		}
		if cause.SourceCommandID != "" {
			i.refCommand(cause.SourceCommandID, path+".sourceCommandId")
		}
		if cause.AdminRecordID != "" {
			if _, ok := i.view.AdminRecords[cause.AdminRecordID]; !ok {
				i.c.add("cause_admin_missing", path, "admin record %q is missing", cause.AdminRecordID)
			}
		}
	}
	for _, id := range sortedMapKeys(i.view.Routing.CauseSets) {
		set := i.view.Routing.CauseSets[id]
		path := "causeSets." + id
		if set.Digest != id || !sortedUnique(set.CauseIDs) {
			i.c.add("cause_set_shape", path, "cause set key/list is noncanonical")
		}
		want, err := CauseSetIdentity(set.CauseIDs)
		if err != nil || want != set.Digest {
			i.c.add("cause_set_identity", path, "cause set identity does not recompute")
		}
		for _, causeID := range set.CauseIDs {
			if _, ok := i.view.Routing.CauseRecords[causeID]; !ok {
				i.c.add("cause_set_member_missing", path, "cause %q is missing", causeID)
			}
		}
	}
	for _, id := range sortedMapKeys(i.view.Routing.Paths) {
		p := i.view.Routing.Paths[id]
		if !p.State.TerminalNonSuccess() {
			continue
		}
		cause, ok := i.view.Routing.CauseRecords[p.TerminalCauseID]
		if !ok {
			i.c.add("terminal_cause_missing", "paths."+id, "cause %q is missing", p.TerminalCauseID)
			continue
		}
		if cause.SourcePathID != id {
			i.c.add("terminal_cause_source", "paths."+id, "terminal cause belongs to path %q", cause.SourcePathID)
		}
	}
	for _, key := range sortedMapKeys(i.view.Routing.CandidateClosures) {
		i.validateCandidateClosure(key, i.view.Routing.CandidateClosures[key])
	}
}

func (i *aggregateIndex) slotSettlement(slot PossibleSlotRecord) (SlotSettlement, bool) {
	paths := i.pathsBySlot[slot.ID]
	if len(paths) > 1 {
		i.c.add("slot_multiple_paths", "possibleSlots."+slot.ID, "slot has paths %v", paths)
		return SlotSettlement{}, false
	}
	if len(paths) == 1 {
		p := i.view.Routing.Paths[paths[0]]
		switch {
		case p.Kind == PathImpossibleEdge:
			set, ok := i.view.Routing.CauseSets[p.ImpossibleCauseDigest]
			if !ok {
				return SlotSettlement{}, false
			}
			kinds := make([]TerminalKind, 0, len(set.CauseIDs))
			for _, id := range set.CauseIDs {
				kinds = append(kinds, i.view.Routing.CauseRecords[id].TerminalKind)
			}
			return SlotSettlement{CauseIDs: append([]string(nil), set.CauseIDs...), CauseKinds: kinds}, true
		case p.State == PathArrived || p.State == PathConsumed || p.State == PathDetachedSink:
			return SlotSettlement{PathID: p.ID}, true
		case p.State.TerminalNonSuccess():
			cause, ok := i.view.Routing.CauseRecords[p.TerminalCauseID]
			if !ok {
				return SlotSettlement{}, false
			}
			return SlotSettlement{CauseIDs: []string{cause.ID}, CauseKinds: []TerminalKind{cause.TerminalKind}}, true
		default:
			return SlotSettlement{}, false
		}
	}
	sourceID, err := ReservationIdentity(i.view.RunID, slot.SourceNodeID, slot.SourceScopeID, slot.SourceBranchEdgeID, slot.Generation)
	if err != nil {
		return SlotSettlement{}, false
	}
	source, ok := i.view.Routing.Reservations[sourceID]
	if !ok {
		return SlotSettlement{}, false
	}
	if source.State == ReservationClosedNoActivation {
		set, ok := i.view.Routing.CauseSets[source.CauseDigest]
		if !ok {
			return SlotSettlement{}, false
		}
		kinds := make([]TerminalKind, 0, len(set.CauseIDs))
		for _, id := range set.CauseIDs {
			kinds = append(kinds, i.view.Routing.CauseRecords[id].TerminalKind)
		}
		return SlotSettlement{CauseIDs: append([]string(nil), set.CauseIDs...), CauseKinds: kinds}, true
	}
	if source.Activation != nil {
		activation, ok := i.view.Routing.Activations[source.Activation.ID]
		if !ok {
			return SlotSettlement{}, false
		}
		output, ok := i.view.Routing.Paths[activation.OutputPathID]
		if ok && output.State.TerminalNonSuccess() {
			cause, ok := i.view.Routing.CauseRecords[output.TerminalCauseID]
			if ok {
				return SlotSettlement{CauseIDs: []string{cause.ID}, CauseKinds: []TerminalKind{cause.TerminalKind}}, true
			}
		}
	}
	return SlotSettlement{}, false
}

func (i *aggregateIndex) validateCandidateClosure(mapKey string, closure CandidateClosure) {
	path := "candidateClosures." + mapKey
	wantKey, err := CandidateClosureKeyIdentity(closure.Key.ReservationID, closure.Key.CandidateID)
	if err != nil || mapKey != closure.Key.ID || wantKey != closure.Key.ID {
		i.c.add("closure_key_identity", path, "closure key does not recompute")
	}
	candidate, ok := i.candidates[candidateKey{closure.Key.ReservationID, closure.Key.CandidateID}]
	if !ok {
		i.c.add("closure_candidate_missing", path, "candidate is not reserved")
		return
	}
	settled := make(map[string]SlotSettlement, len(candidate.PossibleSlotIDs))
	for _, slotID := range candidate.PossibleSlotIDs {
		slot := i.slots[slotID]
		if value, ok := i.slotSettlement(slot); ok {
			settled[slotID] = value
		}
	}
	entry, causes, kind, err := FoldCandidateSlots(closure.Key.ReservationID, candidate, settled, i.openDescendants[candidateKey{closure.Key.ReservationID, closure.Key.CandidateID}])
	if err != nil {
		i.c.add("closure_fold_invalid", path, "%v", err)
		return
	}
	if entry.FoldKind == CandidateFoldOpen || entry.FoldKind == "arrived" {
		i.c.add("closure_not_proven", path, "candidate fold is %q, not closed", entry.FoldKind)
		return
	}
	if kind != closure.TerminalKind || entry.PathOrClosureID != closure.ID {
		i.c.add("closure_fold_mismatch", path, "stored closure differs from exact fold")
	}
	digest, err := CauseSetIdentity(causes)
	if err != nil || digest != closure.CauseDigest {
		i.c.add("closure_cause_digest", path, "closure cause digest is incomplete")
	}
	if set, ok := i.view.Routing.CauseSets[closure.CauseDigest]; !ok || !slices.Equal(set.CauseIDs, causes) {
		i.c.add("closure_cause_set", path, "closure does not name the exact complete cause set")
	}
	want, err := CandidateClosureIdentity(closure.Key.ReservationID, closure.Key.CandidateID, closure.TerminalKind, closure.CauseDigest)
	if err != nil || want != closure.ID {
		i.c.add("closure_identity", path, "closure identity does not recompute")
	}
	if closure.EventSeq < 0 {
		i.c.add("closure_sequence", path, "negative event sequence")
	}
	i.refCommand(closure.CommandID, path+".commandId")
	if cmd, ok := i.view.Commands[closure.CommandID]; ok && cmd.Identity.Kind != CommandPropagateCandidateClosure && cmd.Identity.Kind != CommandActivateGeneration {
		i.c.add("closure_command_authority", path, "command kind %q cannot close a candidate", cmd.Identity.Kind)
	}
}

func (i *aggregateIndex) validatePropagation() {
	owners := map[string]string{}
	for _, id := range sortedMapKeys(i.view.Routing.Propagation) {
		intent := i.view.Routing.Propagation[id]
		path := "propagation." + id
		if intent.ID != id || !intent.State.Valid() || int(intent.Cursor) > len(intent.Frontier) {
			i.c.add("propagation_shape", path, "intent key/state/cursor is invalid")
		}
		if len(intent.Frontier) == 0 || len(intent.Frontier) > MaxRoutingList {
			i.c.add("propagation_frontier_bound", path, "frontier length %d is outside 1..%d", len(intent.Frontier), MaxRoutingList)
		}
		if intent.Shard >= MaxPropagationShards {
			i.c.add("propagation_shard_bound", path, "shard %d exceeds %d", intent.Shard, MaxPropagationShards-1)
		}
		owner := fmt.Sprintf("%s/%d", intent.RootCauseDigest, intent.Shard)
		if previous, duplicate := owners[owner]; duplicate {
			i.c.add("propagation_shard_duplicate", path, "root/shard also owned by %q", previous)
		} else {
			owners[owner] = id
		}
		seen := map[string]struct{}{}
		for _, key := range intent.Frontier {
			if _, ok := seen[key]; ok {
				i.c.add("propagation_frontier_duplicate", path, "frontier key %q is duplicated", key)
			}
			seen[key] = struct{}{}
		}
		if intent.State == PropagationComplete && int(intent.Cursor) != len(intent.Frontier) {
			i.c.add("propagation_complete_cursor", path, "complete intent cursor is %d of %d", intent.Cursor, len(intent.Frontier))
		}
		if intent.State == PropagationPending && int(intent.Cursor) == len(intent.Frontier) {
			i.c.add("propagation_pending_cursor", path, "pending intent has exhausted frontier")
		}
		plan, err := PropagationPlanIdentity(intent.RootReservationID, intent.RootCandidateID, intent.RootCauseDigest, uint64(intent.Shard), intent.Frontier)
		if err != nil || plan != intent.PlanDigest {
			i.c.add("propagation_plan_identity", path, "plan digest does not recompute")
		}
		want, err := PropagationIntentIdentity(intent.RootCauseDigest, uint64(intent.Shard), intent.PlanDigest)
		if err != nil || want != intent.ID {
			i.c.add("propagation_identity", path, "intent identity does not recompute")
		}
		if _, ok := i.candidates[candidateKey{intent.RootReservationID, intent.RootCandidateID}]; !ok {
			i.c.add("propagation_root_missing", path, "root candidate is not reserved")
		}
		if _, ok := i.view.Routing.CauseSets[intent.RootCauseDigest]; !ok {
			i.c.add("propagation_cause_missing", path, "root cause set is missing")
		}
		for _, key := range intent.Frontier {
			if _, found := i.candidateByClosureKey[key]; !found {
				i.c.add("propagation_frontier_unknown", path, "frontier key %q is unknown", key)
			}
		}
		i.refCommand(intent.CommandID, path+".commandId")
		if cmd, ok := i.view.Commands[intent.CommandID]; ok && cmd.Identity.Kind != CommandPropagateCandidateClosure && cmd.Identity.Kind != CommandActivateGeneration {
			i.c.add("propagation_command_authority", path, "command kind %q cannot own propagation", cmd.Identity.Kind)
		}
	}
}

func foldReservationCandidates(i *aggregateIndex, r ActivationReservation) map[string]string {
	result := map[string]string{}
	for _, candidate := range r.Candidates {
		key := candidateKey{r.ID, candidate.ID}
		paths := i.pathsByTarget[key]
		arrivals := make([]PathRecord, 0)
		for _, id := range paths {
			p := i.view.Routing.Paths[id]
			if p.Kind == PathEdge && (p.State == PathArrived || p.State == PathConsumed || p.State == PathDetachedSink) {
				arrivals = append(arrivals, p)
			}
		}
		if len(arrivals) == 1 {
			result[candidate.ID] = "arrived"
			continue
		}
		closureKey, _ := CandidateClosureKeyIdentity(r.ID, candidate.ID)
		if closure, ok := i.view.Routing.CandidateClosures[closureKey]; ok {
			result[candidate.ID] = string(closure.TerminalKind)
		} else {
			result[candidate.ID] = CandidateFoldOpen
		}
	}
	return result
}

func exactFoldCauseIDs(i *aggregateIndex, r ActivationReservation, fold map[string]string) []string {
	var ids []string
	for _, candidate := range r.Candidates {
		kind := fold[candidate.ID]
		if kind == CandidateFoldOpen || kind == "arrived" {
			continue
		}
		key, _ := CandidateClosureKeyIdentity(r.ID, candidate.ID)
		if closure, ok := i.view.Routing.CandidateClosures[key]; ok {
			if set, ok := i.view.Routing.CauseSets[closure.CauseDigest]; ok {
				ids = append(ids, set.CauseIDs...)
			}
		}
	}
	return sortedUniqueStrings(ids)
}

func requireFoldClosed(r ActivationReservation, fold map[string]string) (hasArrival, hasOpen, hasFailed, hasSkipped, hasCanceled bool) {
	for _, candidate := range r.Candidates {
		switch fold[candidate.ID] {
		case "arrived":
			hasArrival = true
		case CandidateFoldOpen:
			hasOpen = true
		case string(TerminalFailed):
			hasFailed = true
		case string(TerminalSkipped):
			hasSkipped = true
		case string(TerminalCanceled):
			hasCanceled = true
		}
	}
	return
}

func validateClosedReservationFold(i *aggregateIndex, r ActivationReservation) error {
	fold := foldReservationCandidates(i, r)
	arrived, open, failed, skipped, canceled := requireFoldClosed(r, fold)
	if r.State == ReservationActivated {
		if r.JoinPolicy == JoinAny {
			if !arrived {
				return fmt.Errorf("any activation has no arrival")
			}
			return nil
		}
		if open {
			return fmt.Errorf("candidate fold remains open")
		}
		if r.JoinPolicy == JoinAll && !arrived {
			return fmt.Errorf("all activation has no arrival")
		}
		if r.JoinPolicy == JoinAll && (failed || skipped || canceled) {
			return fmt.Errorf("all activation includes non-success")
		}
		return nil
	}
	if open {
		return fmt.Errorf("candidate fold remains open")
	}
	if r.ClosedReason == string(ScopeCloseAllImpossible) && (arrived || failed || skipped || canceled) {
		return fmt.Errorf("all-impossible close has other fold kinds")
	}
	if r.ClosedReason == string(ScopeCloseCandidateNonSuccess) && !failed && !skipped && !canceled {
		return fmt.Errorf("candidate-non-success close lacks non-success")
	}
	return nil
}

func (i *aggregateIndex) validateClosedReservationCause(r ActivationReservation) {
	if r.State != ReservationClosedNoActivation {
		return
	}
	path := "reservations." + r.ID
	fold := foldReservationCandidates(i, r)
	leafIDs := exactFoldCauseIDs(i, r, fold)
	kinds := make([]TerminalKind, 0, len(leafIDs))
	for _, id := range leafIDs {
		if cause, ok := i.view.Routing.CauseRecords[id]; ok {
			kinds = append(kinds, cause.TerminalKind)
		}
	}
	wantKind, err := FoldTerminalKinds(kinds)
	if err != nil {
		i.c.add("reservation_close_cause", path, "cannot fold candidate causes: %v", err)
		return
	}
	wantReason := "join_all_impossible"
	if r.ClosedReason == string(ScopeCloseCandidateNonSuccess) {
		wantReason = "join_candidate_non_success"
	}
	joinIDs := []string{}
	for _, id := range i.joinCauses[joinCauseKey{command: r.CommandID, event: r.EventSeq}] {
		cause := i.view.Routing.CauseRecords[id]
		if cause.TerminalKind == wantKind && cause.DispositionReason == wantReason && cause.SourceActivationID == "" && cause.AdminRecordID == "" {
			joinIDs = append(joinIDs, id)
		} else {
			i.c.add("join_cause_mismatch", path, "join cause %q has wrong provenance", id)
		}
	}
	if len(joinIDs) != 1 {
		i.c.add("join_cause_count", path, "has %d exact join causes, want 1", len(joinIDs))
		return
	}
	expected := append(leafIDs, joinIDs[0])
	expected = sortedUniqueStrings(expected)
	set, ok := i.view.Routing.CauseSets[r.CauseDigest]
	if !ok || !slices.Equal(set.CauseIDs, expected) {
		i.c.add("reservation_close_cause_set", path, "cause set is not the exact leaf union plus join cause")
	}
}
