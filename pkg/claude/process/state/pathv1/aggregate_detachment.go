package pathv1

import (
	"cmp"
	"slices"
)

func (i *aggregateIndex) validateDetachments() {
	for _, id := range sortedMapKeys(i.view.Routing.Detachments) {
		d := i.view.Routing.Detachments[id]
		path := "detachments." + id
		if d.Key.ID != id || d.Key.ReservationID != d.ReservationID || d.Key.CandidateID != d.CandidateID {
			i.c.add("detachment_key_fields", path, "map key and persisted key tuple differ")
		}
		wantKey, err := DetachmentKeyIdentity(d.ReservationID, d.CandidateID)
		if err != nil || wantKey != id {
			i.c.add("detachment_key_identity", path, "detachment key does not recompute")
		}
		if d.EventSeq < 0 || d.ActivatedSeq != d.EventSeq {
			i.c.add("detachment_sequence", path, "activated/event sequence mismatch")
		}
		seq, err := eventUint(d.ActivatedSeq)
		want := ""
		if err == nil {
			want, err = DetachmentIdentity(d.ReservationID, d.CandidateID, d.WinnerPathID, seq)
		}
		if err != nil || want != d.ID {
			i.c.add("detachment_identity", path, "detachment identity does not recompute")
		}
		if d.ReasonCode != "any_loser" || d.Actor != "system" || d.AdminRecordID != "" || d.CommandID == "" {
			i.c.add("detachment_authority", path, "detachment must be system any_loser command authority")
		}
		i.refCommand(d.CommandID, path+".commandId")
		if cmd, ok := i.view.Commands[d.CommandID]; ok && cmd.Identity.Kind != CommandActivateGeneration {
			i.c.add("detachment_command_kind", path, "command kind %q cannot own detachment", cmd.Identity.Kind)
		}
		r, ok := i.view.Routing.Reservations[d.ReservationID]
		if !ok || r.JoinPolicy != JoinAny || r.State != ReservationActivated || r.Activation == nil {
			i.c.add("detachment_reservation", path, "detachment reservation is not an activated any")
		} else {
			if d.JoinActivation != *r.Activation || d.CommandID != r.CommandID || d.EventSeq != r.EventSeq {
				i.c.add("detachment_event_coupling", path, "detachment does not share exact winner event")
			}
			if _, ok := i.candidates[candidateKey{r.ID, d.CandidateID}]; !ok {
				i.c.add("detachment_candidate", path, "candidate is not reserved")
			}
			if a, ok := i.view.Routing.Activations[r.Activation.ID]; ok && slices.Contains(a.InputPathIDs, d.WinnerPathID) { // winner itself must never be detached
				winner := i.view.Routing.Paths[d.WinnerPathID]
				if winner.CandidateID == d.CandidateID {
					i.c.add("winner_detached", path, "winner candidate is detached")
				}
			}
		}
		if _, duplicate := i.detachmentsByID[d.ID]; duplicate {
			i.c.add("detachment_id_duplicate", path, "detachment ID %q is duplicated", d.ID)
		} else {
			i.detachmentsByID[d.ID] = d
		}
		byCandidate := i.detachmentsByReservation[d.ReservationID]
		if byCandidate == nil {
			byCandidate = map[CandidateID]DetachmentRecord{}
			i.detachmentsByReservation[d.ReservationID] = byCandidate
		}
		if previous, duplicate := byCandidate[d.CandidateID]; duplicate && previous.ID != d.ID {
			i.c.add("detachment_candidate_duplicate", path, "candidate also detached as %q", previous.ID)
		} else {
			byCandidate[d.CandidateID] = d
		}
	}
	i.indexAllDetachmentSets()
	for _, id := range sortedMapKeys(i.view.Routing.Reservations) {
		r := i.view.Routing.Reservations[id]
		if r.JoinPolicy == JoinAny && r.State == ReservationActivated {
			i.validateAnyDetachmentSet(r)
		}
	}
	for _, id := range sortedMapKeys(i.view.Routing.Paths) {
		i.validateDetachedPath(i.view.Routing.Paths[id])
	}
}

func (i *aggregateIndex) indexAllDetachmentSets() {
	parents := make(map[string]string, len(i.view.Routing.DetachmentSets))
	for _, id := range sortedMapKeys(i.view.Routing.DetachmentSets) {
		set := i.view.Routing.DetachmentSets[id]
		parents[id] = set.ParentSetID
		want, err := DetachmentSetIdentity(set.ParentSetID, set.DetachmentID)
		if err != nil || set.ID != id || want != id {
			i.c.add("detachment_set_identity", "detachmentSets."+id, "identity does not recompute")
		}
		if _, ok := i.detachmentsByID[set.DetachmentID]; !ok {
			i.c.add("detachment_set_member_missing", "detachmentSets."+id, "detachment %q is missing", set.DetachmentID)
		}
		i.detachmentMemberNodes[set.DetachmentID] = append(i.detachmentMemberNodes[set.DetachmentID], id)
	}
	i.detachmentSetIntervals = indexForest(parents, MaxLineageDepth, func(code, id, message string) { i.c.add("detachment_set_"+code, "detachmentSets."+id, "%s", message) })
	for detachmentID, nodes := range i.detachmentMemberNodes {
		slices.SortFunc(nodes, func(a, b DetachmentSetID) int {
			return cmp.Compare(i.detachmentSetIntervals[a].in, i.detachmentSetIntervals[b].in)
		})
		i.detachmentMemberNodes[detachmentID] = nodes
		maxOut := -1
		maxNode := DetachmentSetID("")
		for _, node := range nodes {
			interval, ok := i.detachmentSetIntervals[node]
			if !ok {
				continue
			}
			if maxOut >= interval.out {
				i.c.add("detachment_set_duplicate", "detachmentSets."+node, "detachment %q occurs twice in one set chain (at %q and %q)", detachmentID, maxNode, node)
			}
			if interval.out > maxOut {
				maxOut = interval.out
				maxNode = node
			}
		}
	}
}

func (i *aggregateIndex) detachmentSetContains(setID string, detachmentID DetachmentID) bool {
	set, ok := i.detachmentSetIntervals[setID]
	if !ok {
		return false
	}
	nodes := i.detachmentMemberNodes[detachmentID]
	index, _ := slices.BinarySearchFunc(nodes, set.in, func(node DetachmentSetID, in int) int {
		return cmp.Compare(i.detachmentSetIntervals[node].in, in)
	})
	if index < len(nodes) && i.detachmentSetIntervals[nodes[index]].in == set.in {
		return i.detachmentSetIntervals[nodes[index]].out >= set.out
	}
	if index == 0 {
		return false
	}
	node := i.detachmentSetIntervals[nodes[index-1]]
	return node.in <= set.in && node.out >= set.out
}

func (i *aggregateIndex) validateAnyDetachmentSet(r ActivationReservation) {
	path := "reservations." + r.ID
	if r.Activation == nil {
		i.c.add("any_activation_missing", path, "activated any reservation lacks its activation reference")
		return
	}
	a, ok := i.view.Routing.Activations[r.Activation.ID]
	if !ok {
		return
	}
	if len(a.InputPathIDs) != 1 {
		i.c.add("any_input_count", path, "any activation has %d inputs, want 1", len(a.InputPathIDs))
		return
	}
	winner, ok := i.view.Routing.Paths[a.InputPathIDs[0]]
	if !ok {
		return
	}
	if winner.TargetReservationID != r.ID || winner.State != PathConsumed {
		i.c.add("any_winner_input", path, "winner is not consumed from this reservation")
	}
	losers := map[string]struct{}{}
	for _, candidate := range r.Candidates {
		if candidate.ID != winner.CandidateID {
			losers[candidate.ID] = struct{}{}
		}
	}
	actual := i.detachmentsByReservation[r.ID]
	if actual == nil {
		actual = map[CandidateID]DetachmentRecord{}
	}
	if len(actual) != len(losers) {
		i.c.add("detachment_loser_count", path, "has %d detachments, want %d losing candidates", len(actual), len(losers))
	}
	for candidate := range losers {
		d, ok := actual[candidate]
		if !ok {
			i.c.add("detachment_loser_missing", path, "loser candidate %q has no detachment", candidate)
			continue
		}
		if d.WinnerPathID != winner.ID {
			i.c.add("detachment_winner", path, "loser %q names winner %q, want %q", candidate, d.WinnerPathID, winner.ID)
		}
		root, _ := DetachmentSetIdentity("", d.ID)
		set, ok := i.view.Routing.DetachmentSets[root]
		if !ok || set.ParentSetID != "" || set.DetachmentID != d.ID {
			i.c.add("detachment_root_set", path, "loser %q lacks exact root detachment set", candidate)
		}
	}
	for candidate := range actual {
		if _, ok := losers[candidate]; !ok {
			i.c.add("detachment_extra", path, "candidate %q is not a loser", candidate)
		}
	}

	arrivals := make([]PathRecord, 0)
	for _, candidate := range r.Candidates {
		for _, id := range i.pathsByTarget[candidateKey{r.ID, candidate.ID}] {
			p := i.view.Routing.Paths[id]
			if p.Kind == PathEdge && p.ArrivedSeq <= r.EventSeq && (p.State == PathConsumed || p.State == PathDetachedSink) {
				arrivals = append(arrivals, p)
			}
		}
	}
	slices.SortFunc(arrivals, func(a, b PathRecord) int {
		if n := cmp.Compare(a.ArrivedSeq, b.ArrivedSeq); n != 0 {
			return n
		}
		return cmp.Compare(a.ID, b.ID)
	})
	if len(arrivals) == 0 || arrivals[0].ID != winner.ID {
		i.c.add("any_winner_not_minimum", path, "winner %q is not minimum committed arrival", winner.ID)
	}
	for candidate, d := range actual {
		for _, id := range i.pathsByTarget[candidateKey{r.ID, candidate}] {
			p := i.view.Routing.Paths[id]
			if p.Kind != PathEdge {
				continue
			}
			switch {
			case p.State == PathArrived:
				i.c.add("any_loser_arrived", "paths."+id, "losing arrival remains unsettled")
			case p.State == PathConsumed:
				i.c.add("any_loser_consumed", "paths."+id, "losing arrival was consumed")
			case p.State == PathDetachedSink:
				reason := "late_any_arrival"
				if p.ArrivedSeq <= d.EventSeq {
					reason = "pre_arrived_any_loser"
				}
				if p.DetachedSink == nil || p.DetachedSink.DetachmentID != d.ID || p.DetachedSink.ReasonCode != reason || p.Disposition == nil || p.Disposition.ReasonCode != reason {
					i.c.add("any_loser_sink_receipt", "paths."+id, "sink receipt/reason does not match arrival ordering")
				}
				if p.DetachmentSetID == "" {
					i.c.add("any_loser_sink_set", "paths."+id, "sink lacks detachment-set link")
				} else if !i.detachmentSetContains(p.DetachmentSetID, d.ID) {
					i.c.add("any_loser_sink_set", "paths."+id, "sink set does not contain loser detachment")
				}
				if reason == "pre_arrived_any_loser" && p.UpdatedSeq != d.EventSeq {
					i.c.add("pre_arrived_sink_sequence", "paths."+id, "pre-arrived sink sequence differs from winner event")
				}
				if reason == "late_any_arrival" && p.UpdatedSeq < p.ArrivedSeq {
					i.c.add("late_sink_sequence", "paths."+id, "late sink precedes arrival")
				}
			case p.State.TerminalNonSuccess():
				if p.ArrivedSeq <= d.EventSeq {
					i.c.add("failure_before_sink_order", "paths."+id, "pre-arrived loser became non-success instead of atomic sink")
				}
			}
		}
	}
}

func (i *aggregateIndex) validateDetachedPath(p PathRecord) {
	if p.State == PathDetachedSink {
		i.validateDetachedSinkAuthority(p)
	}
	applicable := map[DetachmentID]struct{}{}
	for _, frame := range p.CandidateLineage {
		key, _ := DetachmentKeyIdentity(frame.ReservationID, frame.CandidateID)
		d, ok := i.view.Routing.Detachments[key]
		if !ok {
			continue
		}
		if d.EventSeq <= p.UpdatedSeq {
			applicable[d.ID] = struct{}{}
		}
		r := i.view.Routing.Reservations[d.ReservationID]
		base := r.ScopeID
		if r.IsReducing {
			base = r.ReducesScopeID
		}
		if p.State == PathDetachedSink && p.TargetReservationID == r.ID {
			continue
		}
		if !within(i.scopeIntervals, p.ScopeID, base) {
			i.c.add("detached_scope_escape", "paths."+p.ID, "detached candidate %q escaped scope %q", frame.CandidateID, base)
		}
		if p.TargetReservationID == r.ID && p.State != PathDetachedSink && p.State != PathFailed && p.State != PathCanceled && p.State != PathSkipped {
			i.c.add("detached_reactivation", "paths."+p.ID, "detached candidate returned to closed reservation without sink")
		}
	}
	if len(applicable) > 0 && p.DetachmentSetID == "" {
		i.c.add("detachment_set_missing", "paths."+p.ID, "path lacks its applicable causal detachment set")
	}
	if p.DetachmentSetID != "" {
		for detachmentID := range applicable {
			if !i.detachmentSetContains(p.DetachmentSetID, detachmentID) {
				i.c.add("detachment_set_lineage", "paths."+p.ID, "set omits applicable causal detachment %q", detachmentID)
			}
		}
		if interval, ok := i.detachmentSetIntervals[p.DetachmentSetID]; ok && interval.depth+1 != len(applicable) {
			i.c.add("detachment_set_lineage", "paths."+p.ID, "set chain has %d members, want %d applicable causal detachments", interval.depth+1, len(applicable))
		}
	}
}

func (i *aggregateIndex) validateDetachedSinkAuthority(p PathRecord) {
	path := "paths." + p.ID
	key, err := DetachmentKeyIdentity(p.TargetReservationID, p.CandidateID)
	d, ok := i.view.Routing.Detachments[key]
	if err != nil || !ok {
		i.c.add("detached_sink_authority", path, "detached sink has no exact reservation/candidate detachment")
		return
	}
	if p.DetachedSink == nil || p.Disposition == nil {
		i.c.add("detached_sink_receipt_missing", path, "detached sink lacks its receipt/disposition")
		return
	}
	receipt, disposition := *p.DetachedSink, *p.Disposition
	reason := "late_any_arrival"
	if p.ArrivedSeq <= d.EventSeq {
		reason = "pre_arrived_any_loser"
	}
	if receipt.DetachmentID != d.ID || receipt.CommandID != disposition.CommandID || receipt.EventSeq != disposition.EventSeq || receipt.EventSeq != p.UpdatedSeq || receipt.ReasonCode != reason || disposition.ReasonCode != reason {
		i.c.add("detached_sink_event", path, "sink receipt is not coupled to its exact disposition event/detachment")
	}
	if reason == "pre_arrived_any_loser" {
		if receipt.CommandID != d.CommandID || receipt.EventSeq != d.EventSeq {
			i.c.add("detached_sink_event", path, "pre-arrived sink is not atomic with the detachment event")
		}
	} else {
		command, commandOK := i.view.Commands[receipt.CommandID]
		reservation, reservationOK := i.view.Routing.Reservations[p.TargetReservationID]
		if !commandOK || !reservationOK || command.Identity.Kind != CommandSettleDetachedSink || command.Identity.SourcePathID != p.ID || command.Identity.TargetReservationID != p.TargetReservationID || command.Identity.TargetGeneration != reservation.Generation {
			i.c.add("detached_sink_authority", path, "late sink command does not own the exact path/reservation generation")
		}
	}
	if p.DetachmentSetID == "" || !i.detachmentSetContains(p.DetachmentSetID, d.ID) {
		i.c.add("detached_sink_set", path, "sink does not link the exact causal detachment set")
	}
}
