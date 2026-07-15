package pathv1

import (
	"fmt"
	"slices"
)

const CandidateFoldOpen = "open"

type SlotSettlement struct {
	PathID     PathID
	CauseIDs   []CauseID
	CauseKinds []TerminalKind
}

// FoldCandidateSlots is the pure authoritative possible-slot fold used by
// reducers before they construct a CandidateClosure. Missing slots and open
// descendants stay open; absence is never treated as impossible.
func FoldCandidateSlots(reservationID ReservationID, candidate CandidateRecord, settled map[PossibleSlotID]SlotSettlement, openDescendant bool) (CandidateFoldEntry, []CauseID, TerminalKind, error) {
	var arrival string
	var causeIDs []string
	var kinds []TerminalKind
	causeKinds := map[string]TerminalKind{}
	expected := map[string]struct{}{}
	for _, slotID := range candidate.PossibleSlotIDs {
		expected[slotID] = struct{}{}
		slot, ok := settled[slotID]
		if !ok {
			return CandidateFoldEntry{CandidateID: candidate.ID, FoldKind: CandidateFoldOpen}, nil, "", nil
		}
		if slot.PathID != "" {
			if len(slot.CauseIDs) > 0 {
				return CandidateFoldEntry{}, nil, "", fmt.Errorf("slot %q has both arrival and causes", slotID)
			}
			if arrival != "" {
				return CandidateFoldEntry{}, nil, "", fmt.Errorf("multiple arrivals for candidate %q", candidate.ID)
			}
			arrival = slot.PathID
		}
		if len(slot.CauseIDs) != len(slot.CauseKinds) {
			return CandidateFoldEntry{}, nil, "", fmt.Errorf("slot %q cause id/kind length mismatch", slotID)
		}
		if slot.PathID == "" && len(slot.CauseIDs) == 0 {
			return CandidateFoldEntry{CandidateID: candidate.ID, FoldKind: CandidateFoldOpen}, nil, "", nil
		}
		for i, id := range slot.CauseIDs {
			if previous, ok := causeKinds[id]; ok && previous != slot.CauseKinds[i] {
				return CandidateFoldEntry{}, nil, "", fmt.Errorf("cause %q has conflicting terminal kinds", id)
			}
			causeKinds[id] = slot.CauseKinds[i]
		}
		causeIDs = append(causeIDs, slot.CauseIDs...)
		kinds = append(kinds, slot.CauseKinds...)
	}
	for slotID := range settled {
		if _, ok := expected[slotID]; !ok {
			return CandidateFoldEntry{}, nil, "", fmt.Errorf("unknown possible slot %q", slotID)
		}
	}
	if arrival != "" {
		return CandidateFoldEntry{CandidateID: candidate.ID, FoldKind: "arrived", PathOrClosureID: arrival}, nil, "", nil
	}
	if openDescendant {
		return CandidateFoldEntry{CandidateID: candidate.ID, FoldKind: CandidateFoldOpen}, nil, "", nil
	}
	if len(kinds) == 0 {
		return CandidateFoldEntry{CandidateID: candidate.ID, FoldKind: CandidateFoldOpen}, nil, "", nil
	}
	fold, err := FoldTerminalKinds(kinds)
	if err != nil {
		return CandidateFoldEntry{}, nil, "", err
	}
	causeIDs = sortedUniqueStrings(causeIDs)
	causeIDs = slices.Compact(causeIDs)
	digest, err := CauseSetIdentity(causeIDs)
	if err != nil {
		return CandidateFoldEntry{}, nil, "", err
	}
	closureID, err := CandidateClosureIdentity(reservationID, candidate.ID, fold, digest)
	if err != nil {
		return CandidateFoldEntry{}, nil, "", err
	}
	return CandidateFoldEntry{CandidateID: candidate.ID, FoldKind: string(fold), PathOrClosureID: closureID}, causeIDs, fold, nil
}
