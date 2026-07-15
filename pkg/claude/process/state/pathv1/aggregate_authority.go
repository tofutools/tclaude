package pathv1

import (
	"fmt"
	"slices"
)

// AggregateAuthority is ephemeral static authority derived from an exact,
// immutable template view. It is intentionally not JSON-shaped and must never
// be persisted in RoutingState. It contains no mutation, command, receipt,
// transition, or replay plan. A future TCL-469 coherent exact-template view is
// the intended supplier; this value alone never authorizes execution.
type AggregateAuthority struct {
	RunID, TemplateRef, TemplateSourceHash string
	Genesis                                GenesisAuthority
	Scopes                                 map[ScopeID]ScopeAuthority
	Reservations                           map[ReservationID]ReservationAuthority
}

type GenesisAuthority struct {
	RootScopeID   ScopeID
	StartNodeID   string
	ReservationID ReservationID
	ActivationID  ActivationID
	OutputPathID  PathID
	Generation    uint64
}

type ScopeAuthority struct {
	ID                    ScopeID
	ParentScopeID         ScopeID
	ParentBranchEdgeID    EdgeID
	ForkActivationID      ActivationID
	ForkOutputPathID      PathID
	Generation            uint64
	ExpectedBranchEdgeIDs []EdgeID
	JoinNodeID            string
	JoinReservationID     ReservationID
}

type ReservationAuthority struct {
	ID             ReservationID
	NodeID         string
	ScopeID        ScopeID
	BranchEdgeID   EdgeID
	Generation     uint64
	JoinPolicy     JoinPolicy
	IsReducing     bool
	ReducesScopeID ScopeID
	Candidates     []CandidateRecord
	PossibleSlots  []PossibleSlotRecord
}

func canonicalDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func validateAuthority(view AggregateView, c diagnosticCollector) bool {
	a := view.Authority
	if a == nil {
		c.add("authority_missing", "authority", "exact-template aggregate authority is required")
		return false
	}
	if a.RunID == "" || a.RunID != view.RunID {
		c.add("authority_run_mismatch", "authority.runId", "authority run %q differs from view run %q", a.RunID, view.RunID)
	}
	if !canonicalDigest(a.TemplateRef) || a.TemplateRef != view.TemplateRef {
		c.add("authority_template_mismatch", "authority.templateRef", "authority template ref is noncanonical or mismatched")
	}
	if !canonicalDigest(a.TemplateSourceHash) || a.TemplateSourceHash != view.TemplateSourceHash {
		c.add("authority_source_hash_mismatch", "authority.templateSourceHash", "authority source hash is noncanonical or mismatched")
	}
	if a.Scopes == nil || a.Reservations == nil {
		c.add("authority_maps_nil", "authority", "scope/reservation authority maps must be non-nil")
		return false
	}
	counter := usageCounter{records: uint64(len(a.Scopes)) + uint64(len(a.Reservations))}
	addReferences := func(n uint64) {
		if err := counter.add(&counter.references, n); err != nil {
			c.add("authority_counter_overflow", "authority", "%v", err)
		}
	}
	largest := 0
	rootCount := 0
	globalCandidates := map[string]string{}
	globalSlots := map[string]string{}
	parents := map[string]string{}
	for _, id := range sortedMapKeys(a.Scopes) {
		s := a.Scopes[id]
		parents[id] = s.ParentScopeID
		path := "authority.scopes." + id
		if id != s.ID {
			c.add("authority_scope_alias", path, "map key differs from scope ID %q", s.ID)
		}
		want, err := ScopeIdentity(a.RunID, s.ParentScopeID, s.ParentBranchEdgeID, s.ForkActivationID, s.ForkOutputPathID, s.Generation)
		if err != nil || want != s.ID {
			c.add("authority_scope_identity", path, "scope identity does not recompute")
		}
		if s.Generation != 1 {
			c.add("authority_scope_generation", path, "generation is %d, want 1", s.Generation)
		}
		if s.ParentScopeID == "" {
			rootCount++
			if s.ID != a.Genesis.RootScopeID || s.ParentBranchEdgeID != "" || s.ForkActivationID != "" || s.ForkOutputPathID != "" || len(s.ExpectedBranchEdgeIDs) != 0 || s.JoinNodeID != "" || s.JoinReservationID != "" {
				c.add("authority_root_shape", path, "root scope differs from narrow genesis root")
			}
		} else if _, ok := a.Scopes[s.ParentScopeID]; !ok {
			c.add("authority_scope_parent", path, "parent scope %q is missing", s.ParentScopeID)
		}
		if s.ParentScopeID != "" && (s.ForkActivationID == "" || s.ForkOutputPathID == "" || len(s.ExpectedBranchEdgeIDs) < 2 || s.JoinNodeID == "" || s.JoinReservationID == "") {
			c.add("authority_child_scope_shape", path, "non-root scope lacks complete fork/join authority")
		}
		if !sortedUnique(s.ExpectedBranchEdgeIDs) {
			c.add("authority_scope_branches", path, "branches are not sorted and unique")
		}
		if len(s.ExpectedBranchEdgeIDs) > MaxOutgoingOrAllCandidates {
			c.add("authority_scope_branch_bound", path, "branch count exceeds %d", MaxOutgoingOrAllCandidates)
		}
		largest = max(largest, len(s.ExpectedBranchEdgeIDs))
		addReferences(uint64(len(s.ExpectedBranchEdgeIDs)) + 6)
	}
	indexForest(parents, MaxLineageDepth, func(code, id, message string) { c.add("authority_scope_"+code, "authority.scopes."+id, "%s", message) })
	if rootCount != 1 {
		c.add("authority_root_count", "authority.scopes", "has %d roots, want exactly 1", rootCount)
	}
	if _, ok := a.Scopes[a.Genesis.RootScopeID]; !ok {
		c.add("authority_genesis_root", "authority.genesis", "root scope %q is missing", a.Genesis.RootScopeID)
	}
	if a.Genesis.Generation != 1 || a.Genesis.StartNodeID == "" || a.Genesis.ReservationID == "" || a.Genesis.ActivationID == "" || a.Genesis.OutputPathID == "" {
		c.add("authority_genesis_shape", "authority.genesis", "genesis authority is incomplete")
	}
	emptyInput, _ := InputSetIdentity(nil)
	wantActivation, _ := ActivationIdentity(a.RunID, a.Genesis.ReservationID, a.Genesis.Generation, emptyInput)
	wantOutput, _ := ActivationOutputIdentity(a.Genesis.ActivationID, a.Genesis.Generation)
	if a.Genesis.ActivationID != wantActivation || a.Genesis.OutputPathID != wantOutput {
		c.add("authority_genesis_identity", "authority.genesis", "activation/output identity does not recompute from empty input set")
	}
	for _, id := range sortedMapKeys(a.Reservations) {
		r := a.Reservations[id]
		path := "authority.reservations." + id
		if id != r.ID {
			c.add("authority_reservation_alias", path, "map key differs from reservation ID %q", r.ID)
		}
		want, err := ReservationIdentity(a.RunID, r.NodeID, r.ScopeID, r.BranchEdgeID, r.Generation)
		if err != nil || want != r.ID {
			c.add("authority_reservation_identity", path, "reservation identity does not recompute")
		}
		if _, ok := a.Scopes[r.ScopeID]; !ok {
			c.add("authority_reservation_scope", path, "scope %q is missing", r.ScopeID)
		}
		if !r.JoinPolicy.Valid() || r.Generation != 1 {
			c.add("authority_reservation_shape", path, "join policy/generation is invalid")
		}
		if r.JoinPolicy == JoinAny && (len(r.Candidates) < 2 || len(r.Candidates) > MaxAnyCandidates) {
			c.add("authority_any_candidate_bound", path, "any candidate count %d is outside 2..%d", len(r.Candidates), MaxAnyCandidates)
		}
		if r.JoinPolicy != JoinAny && len(r.Candidates) > MaxOutgoingOrAllCandidates {
			c.add("authority_candidate_bound", path, "candidate count %d exceeds %d", len(r.Candidates), MaxOutgoingOrAllCandidates)
		}
		if !slices.IsSortedFunc(r.Candidates, func(a, b CandidateRecord) int { return cmpString(a.ID, b.ID) }) || !slices.IsSortedFunc(r.PossibleSlots, func(a, b PossibleSlotRecord) int { return cmpString(a.ID, b.ID) }) {
			c.add("authority_reservation_order", path, "candidates/slots are not canonical sorted")
		}
		largest = max(largest, len(r.Candidates), len(r.PossibleSlots))
		addReferences(uint64(len(r.Candidates)) + 6*uint64(len(r.PossibleSlots)) + 5)
		candidateIDs := map[string]struct{}{}
		slotIDs := map[string]struct{}{}
		slotCandidate := map[string]string{}
		for _, candidate := range r.Candidates {
			if !candidate.Kind.Valid() || candidate.MemberID == "" || r.IsReducing && candidate.Kind != CandidateScopeBranch || !r.IsReducing && candidate.Kind != CandidateInboundEdge {
				c.add("authority_candidate_kind", path, "candidate %q kind %q conflicts with reservation scope authority", candidate.ID, candidate.Kind)
			}
			if _, duplicate := candidateIDs[candidate.ID]; duplicate {
				c.add("authority_candidate_duplicate", path, "candidate %q is duplicated", candidate.ID)
			}
			candidateIDs[candidate.ID] = struct{}{}
			want, err := CandidateIdentity(r.ID, candidate.Kind, candidate.MemberID)
			if err != nil || want != candidate.ID {
				c.add("authority_candidate_identity", path, "candidate %q identity does not recompute", candidate.ID)
			}
			if owner, exists := globalCandidates[candidate.ID]; exists && owner != r.ID {
				c.add("authority_candidate_alias", path, "candidate %q aliases reservations %q and %q", candidate.ID, owner, r.ID)
			} else {
				globalCandidates[candidate.ID] = r.ID
			}
			if !sortedUnique(candidate.PossibleSlotIDs) || len(candidate.PossibleSlotIDs) == 0 {
				c.add("authority_candidate_slots", path, "candidate %q slots are empty/noncanonical", candidate.ID)
			}
			largest = max(largest, len(candidate.PossibleSlotIDs))
			addReferences(uint64(len(candidate.PossibleSlotIDs)) + 2)
			for _, slotID := range candidate.PossibleSlotIDs {
				if _, duplicate := slotIDs[slotID]; duplicate {
					c.add("authority_slot_alias", path, "slot %q is shared/duplicated", slotID)
				}
				slotIDs[slotID] = struct{}{}
				slotCandidate[slotID] = candidate.ID
			}
		}
		if len(slotIDs) != len(r.PossibleSlots) {
			c.add("authority_slot_union", path, "candidate slot union differs from possible slots")
		}
		seenSlotRecords := map[string]struct{}{}
		for _, slot := range r.PossibleSlots {
			want, err := PossibleSlotIdentity(slot.ReservationID, slot.CandidateID, slot.SourceNodeID, slot.SourceEdgeID, slot.SourceScopeID, slot.SourceBranchEdgeID, slot.Generation)
			if err != nil || want != slot.ID || slot.ReservationID != r.ID {
				c.add("authority_slot_identity", path, "slot %q identity/owner does not recompute", slot.ID)
			}
			if _, ok := slotIDs[slot.ID]; !ok {
				c.add("authority_slot_extra", path, "slot %q is not named by a candidate", slot.ID)
			}
			if owner := slotCandidate[slot.ID]; owner != slot.CandidateID {
				c.add("authority_slot_candidate", path, "slot %q candidate %q differs from candidate list owner %q", slot.ID, slot.CandidateID, owner)
			}
			if _, duplicate := seenSlotRecords[slot.ID]; duplicate {
				c.add("authority_slot_record_duplicate", path, "slot record %q is duplicated", slot.ID)
			}
			seenSlotRecords[slot.ID] = struct{}{}
			if slot.SourceNodeID == "" || slot.SourceEdgeID == "" || slot.SourceScopeID == "" || slot.Generation != r.Generation {
				c.add("authority_slot_shape", path, "slot %q lacks exact source/generation tuple", slot.ID)
			}
			if _, ok := a.Scopes[slot.SourceScopeID]; !ok {
				c.add("authority_slot_scope", path, "slot %q source scope is missing", slot.ID)
			}
			if owner, exists := globalSlots[slot.ID]; exists && owner != r.ID {
				c.add("authority_slot_conflict", path, "slot %q aliases reservations %q and %q", slot.ID, owner, r.ID)
			} else {
				globalSlots[slot.ID] = r.ID
			}
		}
	}
	for _, id := range sortedMapKeys(a.Reservations) {
		r := a.Reservations[id]
		if !r.IsReducing {
			continue
		}
		scope, ok := a.Scopes[r.ReducesScopeID]
		if !ok || scope.JoinReservationID != r.ID || scope.JoinNodeID != r.NodeID || r.ScopeID != scope.ID {
			c.add("authority_reducer_scope", "authority.reservations."+id, "reservation is not exact named scope reducer")
			continue
		}
		members := make([]string, 0, len(r.Candidates))
		for _, candidate := range r.Candidates {
			members = append(members, candidate.MemberID)
		}
		slices.Sort(members)
		if !slices.Equal(members, scope.ExpectedBranchEdgeIDs) {
			c.add("authority_reducer_branches", "authority.reservations."+id, "candidate members differ from complete expected branches")
		}
	}
	for _, id := range sortedMapKeys(a.Scopes) {
		scope := a.Scopes[id]
		if scope.ParentScopeID == "" {
			continue
		}
		r, ok := a.Reservations[scope.JoinReservationID]
		if !ok || !r.IsReducing || r.ReducesScopeID != scope.ID {
			c.add("authority_scope_reducer", "authority.scopes."+id, "scope lacks its exact reducing reservation")
		}
	}
	genesisReservation, ok := a.Reservations[a.Genesis.ReservationID]
	if !ok || genesisReservation.NodeID != a.Genesis.StartNodeID || genesisReservation.ScopeID != a.Genesis.RootScopeID || genesisReservation.BranchEdgeID != "" || genesisReservation.Generation != a.Genesis.Generation || genesisReservation.JoinPolicy != JoinExclusive || len(genesisReservation.Candidates) != 0 || len(genesisReservation.PossibleSlots) != 0 || genesisReservation.IsReducing {
		c.add("authority_genesis_reservation", "authority.genesis", "start reservation is not the narrow zero-input exclusive genesis")
	}
	if counter.records > MaxRoutingRecords {
		c.add("authority_records_over_budget", "authority", "%d records exceed %d", counter.records, MaxRoutingRecords)
	}
	if counter.references > MaxIDReferences {
		c.add("authority_references_over_budget", "authority", "%d references exceed %d", counter.references, MaxIDReferences)
	}
	if largest > MaxRoutingList {
		c.add("authority_list_over_budget", "authority", "list length %d exceeds %d", largest, MaxRoutingList)
	}
	return len(c.report.Diagnostics) == 0
}

func cmpString(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func validateAuthorityEquality(view AggregateView, c diagnosticCollector) {
	a := view.Authority
	if len(a.Scopes) != len(view.Routing.Scopes) {
		c.add("authority_scope_set", "routing.scopes", "state has %d scopes, authority expects %d", len(view.Routing.Scopes), len(a.Scopes))
	}
	for _, id := range sortedMapKeys(a.Scopes) {
		expected := a.Scopes[id]
		actual, ok := view.Routing.Scopes[id]
		if !ok {
			c.add("authority_scope_missing", "routing.scopes", "expected scope %q is missing", id)
			continue
		}
		if expected.ID != actual.ID || expected.ParentScopeID != actual.ParentScopeID || expected.ParentBranchEdgeID != actual.ParentBranchEdgeID || expected.ForkActivationID != actual.ForkActivationID || expected.ForkOutputPathID != actual.ForkOutputPathID || expected.Generation != actual.Generation || !slices.Equal(expected.ExpectedBranchEdgeIDs, actual.ExpectedBranchEdgeIDs) || expected.JoinNodeID != actual.JoinNodeID || expected.JoinReservationID != actual.JoinReservationID {
			c.add("authority_scope_mismatch", "routing.scopes."+id, "static scope facts differ from exact authority")
		}
	}
	for _, id := range sortedMapKeys(view.Routing.Scopes) {
		if _, ok := a.Scopes[id]; !ok {
			c.add("authority_scope_extra", "routing.scopes."+id, "scope is absent from exact authority")
		}
	}
	if len(a.Reservations) != len(view.Routing.Reservations) {
		c.add("authority_reservation_set", "routing.reservations", "state has %d reservations, authority expects %d", len(view.Routing.Reservations), len(a.Reservations))
	}
	for _, id := range sortedMapKeys(a.Reservations) {
		expected := a.Reservations[id]
		actual, ok := view.Routing.Reservations[id]
		if !ok {
			c.add("authority_reservation_missing", "routing.reservations", "expected reservation %q is missing", id)
			continue
		}
		if expected.ID != actual.ID || expected.NodeID != actual.NodeID || expected.ScopeID != actual.ScopeID || expected.BranchEdgeID != actual.BranchEdgeID || expected.Generation != actual.Generation || expected.JoinPolicy != actual.JoinPolicy || expected.IsReducing != actual.IsReducing || expected.ReducesScopeID != actual.ReducesScopeID || !equalCandidates(expected.Candidates, actual.Candidates) || !slices.Equal(expected.PossibleSlots, actual.PossibleSlots) {
			c.add("authority_reservation_mismatch", "routing.reservations."+id, "static reservation/candidate/slot facts differ from exact authority")
		}
	}
	for _, id := range sortedMapKeys(view.Routing.Reservations) {
		if _, ok := a.Reservations[id]; !ok {
			c.add("authority_reservation_extra", "routing.reservations."+id, "reservation is absent from exact authority")
		}
	}
}

func equalCandidates(a, b []CandidateRecord) bool {
	if len(a) != len(b) {
		return false
	}
	for n := range a {
		if a[n].ID != b[n].ID || a[n].Kind != b[n].Kind || a[n].MemberID != b[n].MemberID || !slices.Equal(a[n].PossibleSlotIDs, b[n].PossibleSlotIDs) {
			return false
		}
	}
	return true
}

func (a AggregateAuthority) String() string {
	return fmt.Sprintf("AggregateAuthority(%s,%s)", a.RunID, a.TemplateRef)
}
