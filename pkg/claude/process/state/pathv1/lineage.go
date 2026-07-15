package pathv1

import "fmt"

func AppendCandidateLineage(parent PathRecord, reservationID ReservationID, candidateID CandidateID) ([]CandidateLineageFrame, string, error) {
	if err := ValidateLineage(parent); err != nil {
		return nil, "", err
	}
	if len(parent.CandidateLineage) >= MaxLineageDepth {
		return nil, "", &OverBudgetError{Limit: "lineage_depth", Value: len(parent.CandidateLineage) + 1, Maximum: MaxLineageDepth}
	}
	frames := append([]CandidateLineageFrame(nil), parent.CandidateLineage...)
	id, err := CandidateLineageIdentity(parent.CandidateLineageID, reservationID, candidateID)
	if err != nil {
		return nil, "", err
	}
	frames = append(frames, CandidateLineageFrame{ID: id, ParentLineageID: parent.CandidateLineageID, ReservationID: reservationID, CandidateID: candidateID})
	return frames, id, nil
}

func PopConsumedLineage(inputs []PathRecord, reservationID ReservationID) ([]CandidateLineageFrame, string, error) {
	if len(inputs) == 0 {
		return nil, "", fmt.Errorf("no consumed inputs")
	}
	var common []CandidateLineageFrame
	commonID := ""
	for i, input := range inputs {
		if err := ValidateLineage(input); err != nil {
			return nil, "", err
		}
		if len(input.CandidateLineage) == 0 {
			return nil, "", fmt.Errorf("input %d has empty lineage", i)
		}
		last := input.CandidateLineage[len(input.CandidateLineage)-1]
		if last.ReservationID != reservationID {
			return nil, "", fmt.Errorf("input %d final reservation %q, want %q", i, last.ReservationID, reservationID)
		}
		remainder := input.CandidateLineage[:len(input.CandidateLineage)-1]
		remainderID := last.ParentLineageID
		if i == 0 {
			common = append([]CandidateLineageFrame(nil), remainder...)
			commonID = remainderID
		} else {
			if len(common) != len(remainder) {
				return nil, "", fmt.Errorf("consumed input lineage remainders differ")
			}
			for j := range common {
				if common[j] != remainder[j] {
					return nil, "", fmt.Errorf("consumed input lineage remainders differ")
				}
			}
			if commonID != remainderID {
				return nil, "", fmt.Errorf("consumed input lineage ids differ")
			}
		}
	}
	return common, commonID, nil
}

func ValidateLineage(path PathRecord) error {
	if int(path.LineageDepth) != len(path.CandidateLineage) {
		return fmt.Errorf("lineage depth %d does not match %d frames", path.LineageDepth, len(path.CandidateLineage))
	}
	if len(path.CandidateLineage) > MaxLineageDepth {
		return &OverBudgetError{Limit: "lineage_depth", Value: len(path.CandidateLineage), Maximum: MaxLineageDepth}
	}
	parent := ""
	for i, frame := range path.CandidateLineage {
		if frame.ParentLineageID != parent {
			return fmt.Errorf("lineage frame %d parent mismatch", i)
		}
		id, err := CandidateLineageIdentity(parent, frame.ReservationID, frame.CandidateID)
		if err != nil {
			return err
		}
		if frame.ID != id {
			return fmt.Errorf("lineage frame %d identity mismatch", i)
		}
		parent = frame.ID
	}
	if path.CandidateLineageID != parent {
		return fmt.Errorf("final lineage identity mismatch")
	}
	return nil
}

func CandidateLineage(path PathRecord, reservationID, candidateID string) (bool, error) {
	if err := ValidateLineage(path); err != nil {
		return false, err
	}
	for _, frame := range path.CandidateLineage {
		if frame.ReservationID == reservationID && frame.CandidateID == candidateID {
			return true, nil
		}
	}
	return false, nil
}

func DetachedFrom(st *RoutingState, path PathRecord, reservationID string) (bool, error) {
	if st == nil {
		return false, fmt.Errorf("nil routing state")
	}
	if err := ValidateLineage(path); err != nil {
		return false, err
	}
	for _, frame := range path.CandidateLineage {
		if frame.ReservationID != reservationID {
			continue
		}
		key, err := DetachmentKeyIdentity(reservationID, frame.CandidateID)
		if err != nil {
			return false, err
		}
		if _, ok := st.Detachments[key]; ok {
			return true, nil
		}
	}
	return false, nil
}

func VerifyDetachmentSet(st *RoutingState, setID string) ([]DetachmentID, error) {
	if setID == "" {
		return nil, nil
	}
	if st == nil {
		return nil, fmt.Errorf("nil routing state")
	}
	seen := map[string]struct{}{}
	var out []DetachmentID
	for setID != "" {
		if len(seen) >= MaxLineageDepth {
			return nil, &OverBudgetError{Limit: "detachment_set_depth", Value: len(seen) + 1, Maximum: MaxLineageDepth}
		}
		if _, ok := seen[setID]; ok {
			return nil, fmt.Errorf("detachment set cycle at %q", setID)
		}
		seen[setID] = struct{}{}
		set, ok := st.DetachmentSets[setID]
		if !ok {
			return nil, fmt.Errorf("missing detachment set %q", setID)
		}
		want, err := DetachmentSetIdentity(set.ParentSetID, set.DetachmentID)
		if err != nil {
			return nil, err
		}
		if set.ID != setID || want != set.ID {
			return nil, fmt.Errorf("detachment set identity mismatch at %q", setID)
		}
		out = append(out, set.DetachmentID)
		setID = set.ParentSetID
	}
	return out, nil
}
