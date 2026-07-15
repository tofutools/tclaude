package pathv1

import (
	"fmt"
	"math"
)

type usageCounter struct {
	paths, records, references, largest uint64
}

func (u *usageCounter) add(dst *uint64, n uint64) error {
	if math.MaxUint64-*dst < n {
		return fmt.Errorf("path-v1 usage counter overflow")
	}
	*dst += n
	return nil
}

func (u *usageCounter) list(n int) error {
	if n < 0 {
		return fmt.Errorf("negative path-v1 list length")
	}
	value := uint64(n)
	if value > u.largest {
		u.largest = value
	}
	return u.add(&u.references, value)
}

func (u *usageCounter) ids(values ...string) error {
	for _, value := range values {
		if value != "" {
			if err := u.add(&u.references, 1); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkedUsageInt(name string, value uint64) (int, error) {
	if value > uint64(math.MaxInt) {
		return 0, fmt.Errorf("path-v1 %s counter overflows int", name)
	}
	return int(value), nil
}

// MeasureAggregate counts every keyed routing record and all persisted ID
// links in one bounded pass. It intentionally errs on the conservative side:
// scalar identity links count as references as well as the contract's named
// list links, so accepted states can never exceed the true reference ceiling.
func MeasureAggregate(view AggregateView) (Usage, error) {
	if view.Routing == nil {
		return Usage{}, fmt.Errorf("nil routing state")
	}
	u := usageCounter{paths: uint64(len(view.Routing.Paths))}
	for _, n := range []int{
		len(view.Routing.Paths), len(view.Routing.Scopes), len(view.Routing.Reservations),
		len(view.Routing.Activations), len(view.Routing.CandidateClosures), len(view.Routing.CauseRecords),
		len(view.Routing.CauseSets), len(view.Routing.DetachmentSets), len(view.Routing.Detachments),
		len(view.Routing.Propagation),
	} {
		if err := u.add(&u.records, uint64(n)); err != nil {
			return Usage{}, err
		}
	}
	for _, path := range view.Routing.Paths {
		if err := u.list(len(path.ProducedPathIDs)); err != nil {
			return Usage{}, err
		}
		if err := u.list(len(path.CandidateLineage)); err != nil {
			return Usage{}, err
		}
		if err := u.ids(path.ParentPathID, path.SourceActivation.ID, path.TargetReservationID, path.CandidateID, path.ScopeID, path.BranchEdgeID, path.ArrivalID, path.DetachmentSetID, path.ImpossibleCauseDigest, path.TerminalCauseID); err != nil {
			return Usage{}, err
		}
		if path.Edge != nil {
			if err := u.ids(path.Edge.ID); err != nil {
				return Usage{}, err
			}
		}
		if path.ConsumedBy != nil {
			if err := u.ids(path.ConsumedBy.ID); err != nil {
				return Usage{}, err
			}
		}
		if path.Disposition != nil {
			if err := u.ids(path.Disposition.CommandID, path.Disposition.AdminRecordID); err != nil {
				return Usage{}, err
			}
		}
		if path.DetachedSink != nil {
			if err := u.ids(path.DetachedSink.DetachmentID, path.DetachedSink.CommandID); err != nil {
				return Usage{}, err
			}
		}
		if err := u.add(&u.references, uint64(4*len(path.CandidateLineage))); err != nil {
			return Usage{}, err
		}
	}
	for _, scope := range view.Routing.Scopes {
		if err := u.list(len(scope.ExpectedBranchEdgeIDs)); err != nil {
			return Usage{}, err
		}
		if err := u.ids(scope.ParentScopeID, scope.ParentBranchEdgeID, scope.ForkActivationID, scope.ForkOutputPathID, scope.JoinReservationID, scope.ClosedByCommandID); err != nil {
			return Usage{}, err
		}
	}
	for _, reservation := range view.Routing.Reservations {
		if err := u.list(len(reservation.Candidates)); err != nil {
			return Usage{}, err
		}
		if err := u.list(len(reservation.PossibleSlots)); err != nil {
			return Usage{}, err
		}
		if err := u.ids(reservation.ScopeID, reservation.BranchEdgeID, reservation.ReducesScopeID, reservation.CauseDigest, reservation.CommandID); err != nil {
			return Usage{}, err
		}
		for _, candidate := range reservation.Candidates {
			if err := u.list(len(candidate.PossibleSlotIDs)); err != nil {
				return Usage{}, err
			}
			if err := u.ids(candidate.ID, candidate.MemberID); err != nil {
				return Usage{}, err
			}
		}
		if err := u.add(&u.references, uint64(6*len(reservation.PossibleSlots))); err != nil {
			return Usage{}, err
		}
	}
	for _, activation := range view.Routing.Activations {
		if err := u.list(len(activation.InputPathIDs)); err != nil {
			return Usage{}, err
		}
		if err := u.ids(activation.ReservationID, activation.InputSetDigest, activation.OutputPathID, activation.CommandID); err != nil {
			return Usage{}, err
		}
	}
	for _, closure := range view.Routing.CandidateClosures {
		if err := u.ids(closure.Key.ReservationID, closure.Key.CandidateID, closure.CauseDigest, closure.CommandID); err != nil {
			return Usage{}, err
		}
	}
	for _, cause := range view.Routing.CauseRecords {
		if err := u.ids(cause.SourcePathID, cause.SourceActivationID, cause.SourceCommandID, cause.AdminRecordID); err != nil {
			return Usage{}, err
		}
	}
	for _, set := range view.Routing.CauseSets {
		if err := u.list(len(set.CauseIDs)); err != nil {
			return Usage{}, err
		}
	}
	for _, set := range view.Routing.DetachmentSets {
		if err := u.ids(set.ParentSetID, set.DetachmentID); err != nil {
			return Usage{}, err
		}
	}
	for _, detachment := range view.Routing.Detachments {
		if err := u.ids(detachment.ReservationID, detachment.CandidateID, detachment.WinnerPathID, detachment.JoinActivation.ID, detachment.CommandID); err != nil {
			return Usage{}, err
		}
	}
	for _, intent := range view.Routing.Propagation {
		if err := u.list(len(intent.Frontier)); err != nil {
			return Usage{}, err
		}
		if err := u.ids(intent.RootReservationID, intent.RootCandidateID, intent.RootCauseDigest, intent.PlanDigest, intent.CommandID); err != nil {
			return Usage{}, err
		}
	}
	// Commands, side effects, and admin records live outside RoutingState and
	// therefore do not spend its record/reference ceilings.

	paths, err := checkedUsageInt("paths", u.paths)
	if err != nil {
		return Usage{}, err
	}
	records, err := checkedUsageInt("records", u.records)
	if err != nil {
		return Usage{}, err
	}
	references, err := checkedUsageInt("references", u.references)
	if err != nil {
		return Usage{}, err
	}
	largest, err := checkedUsageInt("largest list", u.largest)
	if err != nil {
		return Usage{}, err
	}
	return Usage{Paths: paths, Records: records, References: references, LargestList: largest, CheckpointBytes: view.CheckpointBytes}, nil
}
