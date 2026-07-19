package pathv1

import (
	"fmt"
	"math"
)

type usageCounter struct {
	paths, records, references, largest uint64
}

const usageSaturation = uint64(MaxIDReferences + 1)

func (u *usageCounter) add(dst *uint64, n uint64) error {
	if math.MaxUint64-*dst < n {
		return fmt.Errorf("path-v1 usage counter overflow")
	}
	*dst += n
	return nil
}

func (u *usageCounter) reference(n uint64) {
	if u.references >= usageSaturation {
		return
	}
	if n >= usageSaturation-u.references {
		u.references = usageSaturation
		return
	}
	u.references += n
}

func (u *usageCounter) referenceProduct(n uint64, factor uint64) {
	if factor != 0 && n > (usageSaturation-1)/factor {
		u.references = usageSaturation
		return
	}
	u.reference(n * factor)
}

func (u *usageCounter) list(n int) error {
	if n < 0 {
		return fmt.Errorf("negative path-v1 list length")
	}
	value := uint64(n)
	if value > u.largest {
		u.largest = value
	}
	u.reference(value)
	return nil
}

func (u *usageCounter) ids(values ...string) error {
	for _, value := range values {
		if value != "" {
			u.reference(1)
		}
	}
	return nil
}

func (u *usageCounter) referencesOverBudget() bool {
	return u.references > MaxIDReferences
}

func boundedRegistryCount(maximum int, lengths ...int) int {
	total := 0
	for _, length := range lengths {
		if length > maximum-total {
			return maximum + 1
		}
		total += length
	}
	return total
}

func checkedUsageInt(name string, value uint64) (int, error) {
	if value > uint64(math.MaxInt) {
		return 0, fmt.Errorf("path-v1 %s counter overflows int", name)
	}
	return int(value), nil
}

// MeasureAggregate counts every keyed aggregate record and its persisted
// identity links. Empty scalar links contribute zero. List entries contribute
// one reference each in addition to the element fields documented below.
//
// Records are the ten RoutingState registries plus Commands, SideEffects,
// Contacts, AdminRecords, and AdminResolutions. The exact reference table is:
//
//   - path: ProducedPathIDs and CandidateLineage list entries; ParentPathID,
//     SourceActivation.ID, TargetReservationID, CandidateID, ScopeID,
//     BranchEdgeID, ArrivalID, DetachmentSetID, ImpossibleCauseDigest, and
//     TerminalCauseID; Edge.ID; ConsumedBy.ID; disposition command/admin;
//     detached-sink detachment/command; plus four identity fields per lineage
//     frame (ID, parent, reservation, candidate);
//   - scope: ExpectedBranchEdgeIDs entries; parent scope/branch, fork
//     activation/output, join reservation, and closing command;
//   - reservation: Candidates and PossibleSlots entries; scope, branch,
//     reduced scope, cause digest, and command; each candidate's
//     PossibleSlotIDs entries, ID, and member; six identity fields per possible
//     slot (ID, reservation, candidate, source edge, source scope/branch);
//   - activation: InputPathIDs entries; reservation, input digest, output, and
//     command;
//   - candidate closure: reservation, candidate, cause digest, and command;
//   - cause: source path, source activation, source command, and admin record;
//   - cause set: CauseIDs entries;
//   - detachment set: parent set and detachment;
//   - detachment: reservation, candidate, winner path, join activation, and
//     command;
//   - propagation: Frontier entries; root reservation/candidate/cause, plan,
//     and command;
//   - command: idempotency key, payload hash, source activation, source path,
//     target reservation, input digest, cause digest, and plan digest;
//   - side effect: activation and source command;
//   - contact: activation and source command;
//   - admin record: evidence and resolution digest;
//   - admin resolution: owning admin-record map key, node, and evidence.
//
// Registry cardinality is saturated at MaxRoutingRecords+1 before any record
// is scanned. The reference pass is therefore record-bounded and itself stops
// at MaxIDReferences+1. This makes exact-limit acceptance and bound+1 rejection
// deterministic without allowing malformed input to force unbounded work.
func MeasureAggregate(view AggregateView) (Usage, error) {
	if view.Routing == nil {
		return Usage{}, fmt.Errorf("nil routing state")
	}
	records := boundedRegistryCount(MaxRoutingRecords,
		len(view.Routing.Paths), len(view.Routing.Scopes), len(view.Routing.Reservations),
		len(view.Routing.Activations), len(view.Routing.CandidateClosures), len(view.Routing.CauseRecords),
		len(view.Routing.CauseSets), len(view.Routing.DetachmentSets), len(view.Routing.Detachments),
		len(view.Routing.Propagation), len(view.Commands), len(view.SideEffects), len(view.Contacts),
		len(view.AdminRecords), len(view.AdminResolutions),
	)
	u := usageCounter{paths: uint64(len(view.Routing.Paths)), records: uint64(records)}
	if records > MaxRoutingRecords {
		return aggregateUsage(u, view.CheckpointBytes)
	}
	for _, id := range sortedMapKeys(view.Routing.Paths) {
		path := view.Routing.Paths[id]
		if err := u.list(len(path.ProducedPathIDs)); err != nil {
			return Usage{}, err
		}
		if u.referencesOverBudget() {
			return aggregateUsage(u, view.CheckpointBytes)
		}
		if err := u.list(len(path.CandidateLineage)); err != nil {
			return Usage{}, err
		}
		if u.referencesOverBudget() {
			return aggregateUsage(u, view.CheckpointBytes)
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
		u.referenceProduct(uint64(len(path.CandidateLineage)), 4)
		if u.referencesOverBudget() {
			return aggregateUsage(u, view.CheckpointBytes)
		}
	}
	for _, id := range sortedMapKeys(view.Routing.Scopes) {
		scope := view.Routing.Scopes[id]
		if err := u.list(len(scope.ExpectedBranchEdgeIDs)); err != nil {
			return Usage{}, err
		}
		if u.referencesOverBudget() {
			return aggregateUsage(u, view.CheckpointBytes)
		}
		if err := u.ids(scope.ParentScopeID, scope.ParentBranchEdgeID, scope.ForkActivationID, scope.ForkOutputPathID, scope.JoinReservationID, scope.ClosedByCommandID); err != nil {
			return Usage{}, err
		}
		if u.referencesOverBudget() {
			return aggregateUsage(u, view.CheckpointBytes)
		}
	}
	for _, id := range sortedMapKeys(view.Routing.Reservations) {
		reservation := view.Routing.Reservations[id]
		if err := u.list(len(reservation.Candidates)); err != nil {
			return Usage{}, err
		}
		if u.referencesOverBudget() {
			return aggregateUsage(u, view.CheckpointBytes)
		}
		if err := u.list(len(reservation.PossibleSlots)); err != nil {
			return Usage{}, err
		}
		if u.referencesOverBudget() {
			return aggregateUsage(u, view.CheckpointBytes)
		}
		if err := u.ids(reservation.ScopeID, reservation.BranchEdgeID, reservation.ReducesScopeID, reservation.CauseDigest, reservation.CommandID); err != nil {
			return Usage{}, err
		}
		if u.referencesOverBudget() {
			return aggregateUsage(u, view.CheckpointBytes)
		}
		for _, candidate := range reservation.Candidates {
			if err := u.list(len(candidate.PossibleSlotIDs)); err != nil {
				return Usage{}, err
			}
			if u.referencesOverBudget() {
				return aggregateUsage(u, view.CheckpointBytes)
			}
			if err := u.ids(candidate.ID, candidate.MemberID); err != nil {
				return Usage{}, err
			}
			if u.referencesOverBudget() {
				return aggregateUsage(u, view.CheckpointBytes)
			}
		}
		u.referenceProduct(uint64(len(reservation.PossibleSlots)), 6)
		if u.referencesOverBudget() {
			return aggregateUsage(u, view.CheckpointBytes)
		}
	}
	for _, id := range sortedMapKeys(view.Routing.Activations) {
		activation := view.Routing.Activations[id]
		if err := u.list(len(activation.InputPathIDs)); err != nil {
			return Usage{}, err
		}
		if u.referencesOverBudget() {
			return aggregateUsage(u, view.CheckpointBytes)
		}
		if err := u.ids(activation.ReservationID, activation.InputSetDigest, activation.OutputPathID, activation.CommandID); err != nil {
			return Usage{}, err
		}
		if u.referencesOverBudget() {
			return aggregateUsage(u, view.CheckpointBytes)
		}
	}
	for _, id := range sortedMapKeys(view.Routing.CandidateClosures) {
		closure := view.Routing.CandidateClosures[id]
		if err := u.ids(closure.Key.ReservationID, closure.Key.CandidateID, closure.CauseDigest, closure.CommandID); err != nil {
			return Usage{}, err
		}
		if u.referencesOverBudget() {
			return aggregateUsage(u, view.CheckpointBytes)
		}
	}
	for _, id := range sortedMapKeys(view.Routing.CauseRecords) {
		cause := view.Routing.CauseRecords[id]
		if err := u.ids(cause.SourcePathID, cause.SourceActivationID, cause.SourceCommandID, cause.AdminRecordID); err != nil {
			return Usage{}, err
		}
		if u.referencesOverBudget() {
			return aggregateUsage(u, view.CheckpointBytes)
		}
	}
	for _, id := range sortedMapKeys(view.Routing.CauseSets) {
		set := view.Routing.CauseSets[id]
		if err := u.list(len(set.CauseIDs)); err != nil {
			return Usage{}, err
		}
		if u.referencesOverBudget() {
			return aggregateUsage(u, view.CheckpointBytes)
		}
	}
	for _, id := range sortedMapKeys(view.Routing.DetachmentSets) {
		set := view.Routing.DetachmentSets[id]
		if err := u.ids(set.ParentSetID, set.DetachmentID); err != nil {
			return Usage{}, err
		}
		if u.referencesOverBudget() {
			return aggregateUsage(u, view.CheckpointBytes)
		}
	}
	for _, id := range sortedMapKeys(view.Routing.Detachments) {
		detachment := view.Routing.Detachments[id]
		if err := u.ids(detachment.ReservationID, detachment.CandidateID, detachment.WinnerPathID, detachment.JoinActivation.ID, detachment.CommandID); err != nil {
			return Usage{}, err
		}
		if u.referencesOverBudget() {
			return aggregateUsage(u, view.CheckpointBytes)
		}
	}
	for _, id := range sortedMapKeys(view.Routing.Propagation) {
		intent := view.Routing.Propagation[id]
		if err := u.list(len(intent.Frontier)); err != nil {
			return Usage{}, err
		}
		if u.referencesOverBudget() {
			return aggregateUsage(u, view.CheckpointBytes)
		}
		if err := u.ids(intent.RootReservationID, intent.RootCandidateID, intent.RootCauseDigest, intent.PlanDigest, intent.CommandID); err != nil {
			return Usage{}, err
		}
		if u.referencesOverBudget() {
			return aggregateUsage(u, view.CheckpointBytes)
		}
	}
	for _, id := range sortedMapKeys(view.Commands) {
		command := view.Commands[id]
		if err := u.ids(command.IdempotencyKey, command.PayloadHash,
			command.Identity.SourceActivationID, command.Identity.SourcePathID,
			command.Identity.TargetReservationID, command.Identity.InputDigest,
			command.Identity.CauseDigest, command.Identity.PlanDigest); err != nil {
			return Usage{}, err
		}
		if u.referencesOverBudget() {
			return aggregateUsage(u, view.CheckpointBytes)
		}
	}
	for _, id := range sortedMapKeys(view.SideEffects) {
		effect := view.SideEffects[id]
		if err := u.ids(effect.ActivationID, effect.SourceCommandID); err != nil {
			return Usage{}, err
		}
		if u.referencesOverBudget() {
			return aggregateUsage(u, view.CheckpointBytes)
		}
	}
	for _, id := range sortedMapKeys(view.Contacts) {
		contact := view.Contacts[id]
		if err := u.ids(contact.ActivationID, contact.SourceCommandID); err != nil {
			return Usage{}, err
		}
		if u.referencesOverBudget() {
			return aggregateUsage(u, view.CheckpointBytes)
		}
	}
	for _, id := range sortedMapKeys(view.AdminRecords) {
		record := view.AdminRecords[id]
		if err := u.ids(record.EvidenceRef, record.ResolutionDigest); err != nil {
			return Usage{}, err
		}
		if u.referencesOverBudget() {
			return aggregateUsage(u, view.CheckpointBytes)
		}
	}
	for _, id := range sortedMapKeys(view.AdminResolutions) {
		resolution := view.AdminResolutions[id]
		if err := u.ids(id, resolution.NodeID, resolution.EvidenceRef); err != nil {
			return Usage{}, err
		}
		if u.referencesOverBudget() {
			return aggregateUsage(u, view.CheckpointBytes)
		}
	}

	return aggregateUsage(u, view.CheckpointBytes)
}

func aggregateUsage(u usageCounter, checkpointBytes int) (Usage, error) {
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
	return Usage{Paths: paths, Records: records, References: references, LargestList: largest, CheckpointBytes: checkpointBytes}, nil
}
