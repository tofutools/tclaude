package pathv1

import (
	"cmp"
	"fmt"
	"slices"
)

// inheritPathDetachments links the exact path being advanced to every
// reservation-relative detachment in its immutable candidate lineage. It
// never scans or rewrites descendants: missing set nodes are interned only as
// the path itself next moves.
func inheritPathDetachments(routing *RoutingState, path PathRecord) (PathRecord, []DetachmentSetID, error) {
	if routing == nil {
		return PathRecord{}, nil, fmt.Errorf("%w: routing state is required", ErrMutationInvalid)
	}
	if err := ValidateLineage(path); err != nil {
		return PathRecord{}, nil, err
	}
	type applicableDetachment struct {
		id  DetachmentID
		seq int64
	}
	applicable := make([]applicableDetachment, 0)
	seenApplicable := map[DetachmentID]struct{}{}
	for _, frame := range path.CandidateLineage {
		key, err := DetachmentKeyIdentity(frame.ReservationID, frame.CandidateID)
		if err != nil {
			return PathRecord{}, nil, err
		}
		detachment, ok := routing.Detachments[key]
		if !ok {
			continue
		}
		if _, duplicate := seenApplicable[detachment.ID]; duplicate {
			continue
		}
		seenApplicable[detachment.ID] = struct{}{}
		applicable = append(applicable, applicableDetachment{id: detachment.ID, seq: detachment.ActivatedSeq})
	}
	slices.SortFunc(applicable, func(a, b applicableDetachment) int {
		if n := cmp.Compare(a.seq, b.seq); n != 0 {
			return n
		}
		return cmp.Compare(a.id, b.id)
	})
	existing, err := VerifyDetachmentSet(routing, path.DetachmentSetID)
	if err != nil {
		return PathRecord{}, nil, err
	}
	contained := make(map[DetachmentID]struct{}, len(existing))
	for _, id := range existing {
		contained[id] = struct{}{}
	}
	created := make([]DetachmentSetID, 0)
	parent := path.DetachmentSetID
	for _, detachment := range applicable {
		if _, ok := contained[detachment.id]; ok {
			continue
		}
		setID, err := DetachmentSetIdentity(parent, detachment.id)
		if err != nil {
			return PathRecord{}, nil, err
		}
		want := DetachmentSetRecord{ID: setID, ParentSetID: parent, DetachmentID: detachment.id}
		if current, exists := routing.DetachmentSets[setID]; exists {
			if current != want {
				return PathRecord{}, nil, fmt.Errorf("%w: detachment set %q conflicts with exact inheritance", ErrMutationInconsistent, setID)
			}
		} else {
			routing.DetachmentSets[setID] = want
			created = append(created, setID)
		}
		parent = setID
		contained[detachment.id] = struct{}{}
	}
	path.DetachmentSetID = parent
	return path, created, nil
}

func commonInputDetachmentSet(inputs []PathRecord) (DetachmentSetID, error) {
	if len(inputs) == 0 {
		return "", nil
	}
	want := inputs[0].DetachmentSetID
	for _, input := range inputs[1:] {
		if input.DetachmentSetID != want {
			return "", fmt.Errorf("%w: joined inputs have different detachment inheritance", ErrMutationInconsistent)
		}
	}
	return want, nil
}
