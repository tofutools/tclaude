package pathv1

func Clone(st RoutingState) RoutingState {
	out := st
	if st.Paths != nil {
		out.Paths = make(map[string]PathRecord, len(st.Paths))
		for k, v := range st.Paths {
			out.Paths[k] = clonePath(v)
		}
	}
	out.Scopes = make(map[string]ScopeRecord, len(st.Scopes))
	if st.Scopes == nil {
		out.Scopes = nil
	} else {
		for k, v := range st.Scopes {
			v.ExpectedBranchEdgeIDs = cloneSlice(v.ExpectedBranchEdgeIDs)
			out.Scopes[k] = v
		}
	}
	if st.Reservations != nil {
		out.Reservations = make(map[string]ActivationReservation, len(st.Reservations))
		for k, v := range st.Reservations {
			out.Reservations[k] = cloneReservation(v)
		}
	}
	if st.Activations != nil {
		out.Activations = make(map[string]ActivationRecord, len(st.Activations))
		for k, v := range st.Activations {
			v.InputPathIDs = cloneSlice(v.InputPathIDs)
			out.Activations[k] = v
		}
	}
	out.CandidateClosures = cloneMap(st.CandidateClosures)
	out.CauseRecords = cloneMap(st.CauseRecords)
	if st.CauseSets != nil {
		out.CauseSets = make(map[string]CauseSetRecord, len(st.CauseSets))
		for k, v := range st.CauseSets {
			v.CauseIDs = cloneSlice(v.CauseIDs)
			out.CauseSets[k] = v
		}
	}
	out.DetachmentSets = cloneMap(st.DetachmentSets)
	out.Detachments = cloneMap(st.Detachments)
	if st.Propagation != nil {
		out.Propagation = make(map[string]PropagationIntent, len(st.Propagation))
		for k, v := range st.Propagation {
			v.Frontier = cloneSlice(v.Frontier)
			out.Propagation[k] = v
		}
	}
	return out
}

func cloneMap[T any](in map[string]T) map[string]T {
	if in == nil {
		return nil
	}
	out := make(map[string]T, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneSlice[T any](in []T) []T {
	if in == nil {
		return nil
	}
	out := make([]T, len(in))
	copy(out, in)
	return out
}

func clonePath(v PathRecord) PathRecord {
	v.ProducedPathIDs = cloneSlice(v.ProducedPathIDs)
	v.CandidateLineage = cloneSlice(v.CandidateLineage)
	if v.Edge != nil {
		x := *v.Edge
		v.Edge = &x
	}
	if v.ConsumedBy != nil {
		x := *v.ConsumedBy
		v.ConsumedBy = &x
	}
	if v.Disposition != nil {
		x := *v.Disposition
		v.Disposition = &x
	}
	if v.DetachedSink != nil {
		x := *v.DetachedSink
		v.DetachedSink = &x
	}
	return v
}
func cloneReservation(v ActivationReservation) ActivationReservation {
	v.Candidates = cloneSlice(v.Candidates)
	for i := range v.Candidates {
		v.Candidates[i].PossibleSlotIDs = cloneSlice(v.Candidates[i].PossibleSlotIDs)
	}
	v.PossibleSlots = cloneSlice(v.PossibleSlots)
	if v.Activation != nil {
		x := *v.Activation
		v.Activation = &x
	}
	if v.CloseReceipt != nil {
		x := *v.CloseReceipt
		v.CloseReceipt = &x
	}
	return v
}

func NewRoutingState() RoutingState {
	return RoutingState{Protocol: Protocol, Encoding: Encoding, Paths: map[string]PathRecord{}, Scopes: map[string]ScopeRecord{}, Reservations: map[string]ActivationReservation{}, Activations: map[string]ActivationRecord{}, CandidateClosures: map[string]CandidateClosure{}, CauseRecords: map[string]CauseRecord{}, CauseSets: map[string]CauseSetRecord{}, DetachmentSets: map[string]DetachmentSetRecord{}, Detachments: map[string]DetachmentRecord{}, Propagation: map[string]PropagationIntent{}}
}
