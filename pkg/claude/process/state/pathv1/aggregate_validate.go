package pathv1

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

type candidateKey struct{ reservation, candidate string }
type treeInterval struct{ in, out, depth int }
type joinCauseKey struct {
	command string
	event   int64
}

type aggregateIndex struct {
	view AggregateView
	c    diagnosticCollector

	candidates               map[candidateKey]CandidateRecord
	candidateByClosureKey    map[CandidateClosureKey]candidateKey
	slots                    map[PossibleSlotID]PossibleSlotRecord
	pathsBySlot              map[PossibleSlotID][]PathID
	pathsByTarget            map[candidateKey][]PathID
	openDescendants          map[candidateKey]bool
	outputs                  map[ActivationID]PathID
	forkScopeByOutput        map[PathID]ScopeID
	detachmentsByID          map[DetachmentID]DetachmentRecord
	detachmentsByReservation map[ReservationID]map[CandidateID]DetachmentRecord
	joinCauses               map[joinCauseKey][]CauseID
	detachmentSetIntervals   map[DetachmentSetID]treeInterval
	detachmentMemberNodes    map[DetachmentID][]DetachmentSetID
	scopeIntervals           map[ScopeID]treeInterval
	commandRefs              map[string]struct{}
}

func sortedMapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func sortedUnique(values []string) bool {
	return slices.IsSorted(values) && len(slices.Compact(append([]string(nil), values...))) == len(values)
}

func eventUint(value int64) (uint64, error) {
	if value < 0 {
		return 0, fmt.Errorf("negative event sequence %d", value)
	}
	return uint64(value), nil
}

func ValidateAggregate(view AggregateView) InvariantReport {
	report := InvariantReport{}
	c := diagnosticCollector{report: &report}
	if view.Routing == nil {
		c.add("routing_nil", "routing", "routing state is nil")
		return report
	}
	if view.RunID == "" {
		c.add("run_id_empty", "runId", "run ID is empty")
	}
	if !validateAuthority(view, c) {
		return report
	}
	if view.Commands == nil || view.SideEffects == nil || view.AdminRecords == nil || view.AdminResolutions == nil {
		c.add("aggregate_maps_nil", "aggregate", "commands, side effects, admin records, and admin resolutions must be complete non-nil maps")
		return report
	}
	if err := validateSerializationEnvelope(view.Routing); err != nil {
		c.add("routing_envelope", "routing", "%v", err)
		return report
	}
	usage, err := MeasureAggregate(view)
	if err != nil {
		c.add("usage_overflow", "routing", "%v", err)
		return report
	}
	report.Usage = usage
	if err := usage.Validate(); err != nil {
		c.add("routing_over_budget", "routing", "%v", err)
		return report
	}
	if data, err := Encode(view.Routing); err != nil {
		c.add("checkpoint_over_budget", "routing", "%v", err)
		return report
	} else if view.CheckpointBytes == 0 {
		report.Usage.CheckpointBytes = len(data)
	}
	validateAuthorityEquality(view, c)

	i := &aggregateIndex{
		view: view, c: c,
		candidates: map[candidateKey]CandidateRecord{}, candidateByClosureKey: map[CandidateClosureKey]candidateKey{}, slots: map[PossibleSlotID]PossibleSlotRecord{},
		pathsBySlot: map[PossibleSlotID][]PathID{}, pathsByTarget: map[candidateKey][]PathID{},
		openDescendants: map[candidateKey]bool{}, outputs: map[ActivationID]PathID{},
		forkScopeByOutput: map[PathID]ScopeID{},
		detachmentsByID:   map[DetachmentID]DetachmentRecord{}, detachmentsByReservation: map[ReservationID]map[CandidateID]DetachmentRecord{}, joinCauses: map[joinCauseKey][]CauseID{},
		detachmentSetIntervals: map[DetachmentSetID]treeInterval{}, detachmentMemberNodes: map[DetachmentID][]DetachmentSetID{},
		scopeIntervals: map[ScopeID]treeInterval{},
		commandRefs:    map[string]struct{}{},
	}
	i.indexReservations()
	i.indexPaths()
	i.indexCauses()
	i.validateCommandsAndAuthority()
	i.validateScopes()
	i.validatePaths()
	i.validateActivationsAndReservations()
	i.validateGenesis()
	i.validateCausesAndClosures()
	i.validateDetachments()
	i.validatePropagation()
	return report
}

func (i *aggregateIndex) indexCauses() {
	for id, cause := range i.view.Routing.CauseRecords {
		if cause.SourcePathID == "" {
			key := joinCauseKey{command: cause.SourceCommandID, event: cause.EventSeq}
			i.joinCauses[key] = append(i.joinCauses[key], id)
		}
	}
}

func (i *aggregateIndex) validateGenesis() {
	g := i.view.Authority.Genesis
	a, ok := i.view.Routing.Activations[g.ActivationID]
	if !ok {
		i.c.add("genesis_activation_missing", "activations", "authorized genesis activation %q is missing", g.ActivationID)
	} else if len(a.InputPathIDs) != 0 || a.ReservationID != g.ReservationID || a.OutputPathID != g.OutputPathID {
		i.c.add("genesis_activation_mismatch", "activations."+g.ActivationID, "genesis activation differs from authority")
	}
	p, ok := i.view.Routing.Paths[g.OutputPathID]
	if !ok {
		i.c.add("genesis_path_missing", "paths", "authorized genesis path %q is missing", g.OutputPathID)
	} else if p.Kind != PathActivationOutput || p.SourceActivation.ID != g.ActivationID || p.SourceActivation.Generation != g.Generation || p.ScopeID != g.RootScopeID || p.BranchEdgeID != "" || p.ParentPathID != "" {
		i.c.add("genesis_path_mismatch", "paths."+g.OutputPathID, "genesis path differs from authority")
	}
	zeroInputs := 0
	for _, activation := range i.view.Routing.Activations {
		if len(activation.InputPathIDs) == 0 {
			zeroInputs++
		}
	}
	if zeroInputs != 1 {
		i.c.add("genesis_count", "activations", "has %d zero-input activations, want exactly one authorized genesis", zeroInputs)
	}
}

func (i *aggregateIndex) refCommand(id, path string) {
	if id == "" {
		i.c.add("command_authority_missing", path, "required command authority is empty")
		return
	}
	i.commandRefs[id] = struct{}{}
	if _, ok := i.view.Commands[id]; !ok {
		i.c.add("command_missing", path, "command %q is missing", id)
	}
}

func (i *aggregateIndex) indexReservations() {
	for _, id := range sortedMapKeys(i.view.Routing.Reservations) {
		r := i.view.Routing.Reservations[id]
		for _, candidate := range r.Candidates {
			key := candidateKey{id, candidate.ID}
			if _, exists := i.candidates[key]; exists {
				i.c.add("candidate_duplicate", "reservations."+id+".candidates", "candidate %q is duplicated", candidate.ID)
			} else {
				i.candidates[key] = candidate
			}
			if closureKey, err := CandidateClosureKeyIdentity(id, candidate.ID); err == nil {
				i.candidateByClosureKey[closureKey] = key
			}
		}
		for _, slot := range r.PossibleSlots {
			if previous, exists := i.slots[slot.ID]; exists && previous != slot {
				i.c.add("slot_duplicate", "reservations."+id+".possibleSlots", "slot %q has conflicting records", slot.ID)
			} else {
				i.slots[slot.ID] = slot
			}
		}
	}
}

func (i *aggregateIndex) indexPaths() {
	for _, id := range sortedMapKeys(i.view.Routing.Paths) {
		path := i.view.Routing.Paths[id]
		if path.Kind == PathActivationOutput && path.SourceActivation.ID != "" {
			if previous, exists := i.outputs[path.SourceActivation.ID]; exists && previous != id {
				i.c.add("activation_output_duplicate", "paths."+id, "activation %q has outputs %q and %q", path.SourceActivation.ID, previous, id)
			} else {
				i.outputs[path.SourceActivation.ID] = id
			}
		}
		if path.TargetReservationID != "" && path.CandidateID != "" {
			key := candidateKey{path.TargetReservationID, path.CandidateID}
			i.pathsByTarget[key] = append(i.pathsByTarget[key], id)
		}
		if path.Kind == PathEdge || path.Kind == PathImpossibleEdge {
			if parent, ok := i.view.Routing.Paths[path.ParentPathID]; ok && path.Edge != nil {
				sourceScopeID, sourceBranchEdgeID := parent.ScopeID, parent.BranchEdgeID
				if parent.State == PathSplit {
					sourceScopeID, sourceBranchEdgeID = path.ScopeID, path.BranchEdgeID
				}
				slotID, err := PossibleSlotIdentity(path.TargetReservationID, path.CandidateID, path.Edge.FromNodeID, path.Edge.ID, sourceScopeID, sourceBranchEdgeID, path.SourceActivation.Generation)
				exact := PossibleSlotRecord{ID: slotID, ReservationID: path.TargetReservationID, CandidateID: path.CandidateID, SourceNodeID: path.Edge.FromNodeID, SourceEdgeID: path.Edge.ID, SourceScopeID: sourceScopeID, SourceBranchEdgeID: sourceBranchEdgeID, Generation: path.SourceActivation.Generation}
				authorized, exists := i.slots[slotID]
				if err != nil || !exists || authorized != exact {
					i.c.add("path_slot_authority", "paths."+id, "edge/impossible-edge path lacks its exact authorized possible slot")
				} else {
					i.pathsBySlot[slotID] = append(i.pathsBySlot[slotID], id)
					if len(i.pathsBySlot[slotID]) == 2 {
						i.c.add("slot_multiple_paths", "possibleSlots."+slotID, "authorized slot is materialized by multiple paths")
					}
				}
			} else {
				i.c.add("path_slot_authority", "paths."+id, "edge/impossible-edge path cannot recompute an authorized possible slot")
			}
		}
		active := path.State == PathLive || path.State == PathArrived
		if active {
			for _, frame := range path.CandidateLineage {
				if path.TargetReservationID == frame.ReservationID && path.CandidateID == frame.CandidateID {
					continue
				}
				i.openDescendants[candidateKey{frame.ReservationID, frame.CandidateID}] = true
			}
		}
	}
}

func (i *aggregateIndex) validateCommandsAndAuthority() {
	for _, id := range sortedMapKeys(i.view.Commands) {
		record := i.view.Commands[id]
		if record.ID != id {
			i.c.add("command_key_mismatch", "commands."+id, "map key differs from command ID %q", record.ID)
		}
		if record.Identity.RunID != i.view.RunID {
			i.c.add("command_run_mismatch", "commands."+id, "command run %q differs from aggregate run", record.Identity.RunID)
		}
		var err error
		if record.Identity.Kind == CommandCompleteRun {
			err = validateCompleteRunCommandPrimitive(record)
		} else {
			err = ValidateCommand(record)
		}
		if err != nil {
			i.c.add("command_invalid", "commands."+id, "%v", err)
		}
	}
	for _, id := range sortedMapKeys(i.view.SideEffects) {
		effect := i.view.SideEffects[id]
		if effect.ID != id {
			i.c.add("side_effect_key_mismatch", "sideEffects."+id, "map key differs from side-effect ID %q", effect.ID)
		}
		if effect.RunID != i.view.RunID {
			i.c.add("side_effect_run_mismatch", "sideEffects."+id, "side effect belongs to run %q", effect.RunID)
		}
		if err := ValidateSideEffect(effect); err != nil {
			i.c.add("side_effect_invalid", "sideEffects."+id, "%v", err)
		}
		if _, ok := i.view.Routing.Activations[effect.ActivationID]; !ok {
			i.c.add("side_effect_activation_missing", "sideEffects."+id, "activation %q is missing", effect.ActivationID)
		}
	}
	for _, id := range sortedMapKeys(i.view.AdminRecords) {
		record := i.view.AdminRecords[id]
		if record.ID != id {
			i.c.add("admin_key_mismatch", "adminRecords."+id, "map key differs from admin ID %q", record.ID)
		}
		if record.RunID != i.view.RunID {
			i.c.add("admin_run_mismatch", "adminRecords."+id, "admin record belongs to run %q", record.RunID)
		}
		var resolution *BlockResolution
		if value, ok := i.view.AdminResolutions[id]; ok {
			resolution = &value
		}
		if err := ValidateAdminRecord(record, false, resolution); err != nil {
			i.c.add("admin_invalid", "adminRecords."+id, "%v", err)
		}
		if record.Actor == "system" || strings.HasPrefix(record.Actor, "command:") {
			i.c.add("admin_actor_invalid", "adminRecords."+id+".actor", "user/admin authority cannot use automatic actor %q", record.Actor)
		}
	}
}

func (i *aggregateIndex) validateScopes() {
	reducerByScope := map[string]string{}
	for _, id := range sortedMapKeys(i.view.Routing.Scopes) {
		s := i.view.Routing.Scopes[id]
		path := "scopes." + id
		if s.ID != id {
			i.c.add("scope_key_mismatch", path, "map key differs from scope ID %q", s.ID)
		}
		if s.RunID != i.view.RunID {
			i.c.add("scope_run_mismatch", path, "scope belongs to run %q", s.RunID)
		}
		if s.Generation != 1 {
			i.c.add("scope_generation", path+".generation", "generation is %d, want 1", s.Generation)
		}
		if !s.State.Valid() || !s.CloseReason.Valid() {
			i.c.add("scope_state_invalid", path, "invalid state/reason %q/%q", s.State, s.CloseReason)
		}
		if !sortedUnique(s.ExpectedBranchEdgeIDs) {
			i.c.add("scope_branches_noncanonical", path+".expectedBranchEdgeIds", "expected branches are not sorted and unique")
		}
		if len(s.ExpectedBranchEdgeIDs) > MaxOutgoingOrAllCandidates {
			i.c.add("scope_branches_over_budget", path, "expected branch count %d exceeds %d", len(s.ExpectedBranchEdgeIDs), MaxOutgoingOrAllCandidates)
		}
		want, err := ScopeIdentity(s.RunID, s.ParentScopeID, s.ParentBranchEdgeID, s.ForkActivationID, s.ForkOutputPathID, s.Generation)
		if err != nil || want != s.ID {
			i.c.add("scope_identity", path, "scope identity does not recompute")
		}
		root := s.ParentScopeID == ""
		if root {
			if s.ParentBranchEdgeID != "" || s.ForkActivationID != "" || s.ForkOutputPathID != "" || len(s.ExpectedBranchEdgeIDs) != 0 || s.JoinNodeID != "" || s.JoinReservationID != "" {
				i.c.add("root_scope_shape", path, "root scope has fork/join/parent fields")
			}
		} else {
			if _, ok := i.view.Routing.Scopes[s.ParentScopeID]; !ok {
				i.c.add("scope_parent_missing", path, "parent scope %q is missing", s.ParentScopeID)
			}
			if s.ForkActivationID == "" || s.ForkOutputPathID == "" || len(s.ExpectedBranchEdgeIDs) < 2 || s.JoinNodeID == "" || s.JoinReservationID == "" {
				i.c.add("child_scope_shape", path, "non-root scope lacks complete fork/join authority")
			}
			if activation, ok := i.view.Routing.Activations[s.ForkActivationID]; !ok || activation.OutputPathID != s.ForkOutputPathID {
				i.c.add("scope_fork_missing", path, "fork activation/output does not match")
			}
			if output, ok := i.view.Routing.Paths[s.ForkOutputPathID]; !ok || output.ScopeID != s.ParentScopeID || output.BranchEdgeID != s.ParentBranchEdgeID {
				i.c.add("scope_parent_context", path, "fork output does not match recorded parent scope/branch")
			}
			if previous, exists := i.forkScopeByOutput[s.ForkOutputPathID]; exists && previous != s.ID {
				i.c.add("fork_output_multiple_scopes", path, "fork output %q creates scopes %q and %q", s.ForkOutputPathID, previous, s.ID)
			} else {
				i.forkScopeByOutput[s.ForkOutputPathID] = s.ID
			}
		}
		switch s.State {
		case ScopeOpen:
			if s.CloseReason != ScopeCloseNone || s.ClosedByCommandID != "" {
				i.c.add("open_scope_closed_fields", path, "open scope has close authority")
			}
		case ScopeClosedActivated:
			if s.CloseReason != ScopeCloseAll && s.CloseReason != ScopeCloseAny {
				i.c.add("scope_close_reason", path, "closed activated scope has reason %q", s.CloseReason)
			}
			i.refCommand(s.ClosedByCommandID, path+".closedByCommandId")
			if s.ParentScopeID == "" {
				i.validateRootScopeClosure(path, s, false)
			}
		case ScopeClosedNoActivation:
			if s.CloseReason != ScopeCloseAllImpossible && s.CloseReason != ScopeCloseCandidateNonSuccess {
				i.c.add("scope_close_reason", path, "closed-no-activation scope has reason %q", s.CloseReason)
			}
			i.refCommand(s.ClosedByCommandID, path+".closedByCommandId")
			if s.ParentScopeID == "" {
				i.validateRootScopeClosure(path, s, true)
			}
		}
	}
	for _, id := range sortedMapKeys(i.view.Routing.Reservations) {
		r := i.view.Routing.Reservations[id]
		if !r.IsReducing {
			continue
		}
		if previous, ok := reducerByScope[r.ReducesScopeID]; ok {
			i.c.add("scope_multiple_reducers", "reservations."+id, "scope %q is reduced by %q and %q", r.ReducesScopeID, previous, id)
		} else {
			reducerByScope[r.ReducesScopeID] = id
		}
	}
	i.indexScopeTree()
}

func (i *aggregateIndex) validateRootScopeClosure(path string, scope ScopeRecord, noActivation bool) {
	var reducer *ActivationReservation
	for _, candidate := range i.view.Routing.Reservations {
		if candidate.IsReducing && candidate.ReducesScopeID == scope.ID {
			copy := candidate
			reducer = &copy
			break
		}
	}
	if reducer == nil {
		i.c.add("root_scope_close_authority", path, "closed root scope lacks an exact reducing reservation")
		return
	}
	if reducer.CommandID != scope.ClosedByCommandID {
		i.c.add("root_scope_close_authority", path, "root scope close command does not match reducing reservation")
	}
	if noActivation && reducer.State != ReservationClosedNoActivation {
		i.c.add("root_scope_close_authority", path, "closed-no-activation root scope lacks closed reducing reservation")
	}
	if !noActivation && reducer.State != ReservationActivated {
		i.c.add("root_scope_close_authority", path, "activated root scope lacks activated reducing reservation")
	}
	command, ok := i.view.Commands[scope.ClosedByCommandID]
	if !ok || command.Identity.Kind != CommandActivateGeneration || command.Identity.TargetReservationID != reducer.ID || command.Identity.TargetGeneration != reducer.Generation {
		i.c.add("root_scope_close_authority", path, "root scope close is not owned by the reducing activation command")
	}
}

func (i *aggregateIndex) indexScopeTree() {
	parents := make(map[string]string, len(i.view.Routing.Scopes))
	for id, scope := range i.view.Routing.Scopes {
		parents[id] = scope.ParentScopeID
	}
	i.scopeIntervals = indexForest(parents, MaxLineageDepth, func(code, id, message string) { i.c.add("scope_"+code, "scopes."+id, "%s", message) })
}

func indexForest(parents map[string]string, maxDepth int, diagnose func(code, id, message string)) map[string]treeInterval {
	children := map[string][]string{}
	roots := []string{}
	for id, parent := range parents {
		if parent == "" {
			roots = append(roots, id)
		} else {
			children[parent] = append(children[parent], id)
		}
	}
	slices.Sort(roots)
	for parent := range children {
		slices.Sort(children[parent])
	}
	type frame struct {
		id    string
		exit  bool
		depth int
	}
	clock := 0
	intervals := map[string]treeInterval{}
	color := map[string]uint8{}
	walk := func(root string) {
		stack := []frame{{id: root}}
		for len(stack) > 0 {
			f := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if f.exit {
				iv := intervals[f.id]
				iv.out = clock
				clock++
				intervals[f.id] = iv
				color[f.id] = 2
				continue
			}
			if color[f.id] == 1 {
				diagnose("ancestry_cycle", f.id, "ancestry cycle detected")
				continue
			}
			if color[f.id] == 2 {
				continue
			}
			if f.depth > maxDepth {
				diagnose("ancestry_depth_over_budget", f.id, fmt.Sprintf("ancestry exceeds %d", maxDepth))
				continue
			}
			color[f.id] = 1
			intervals[f.id] = treeInterval{in: clock, depth: f.depth}
			clock++
			stack = append(stack, frame{id: f.id, exit: true, depth: f.depth})
			kids := children[f.id]
			for n := len(kids) - 1; n >= 0; n-- {
				child := kids[n]
				if color[child] == 1 {
					diagnose("ancestry_cycle", child, "ancestry cycle detected")
					continue
				}
				stack = append(stack, frame{id: child, depth: f.depth + 1})
			}
		}
	}
	for _, root := range roots {
		walk(root)
	}
	for _, id := range sortedMapKeys(parents) {
		if color[id] == 0 {
			diagnose("ancestry_cycle", id, "node is not reachable from a root")
			walk(id)
		}
	}
	return intervals
}

func within(intervals map[string]treeInterval, child, ancestor string) bool {
	c, cok := intervals[child]
	a, aok := intervals[ancestor]
	return cok && aok && a.in <= c.in && c.out <= a.out
}

func compareActivationRef(a, b *ActivationRef) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func (i *aggregateIndex) validatePaths() {
	childSeen := map[string]string{}
	inputSeen := map[string]string{}
	for activationID, activation := range i.view.Routing.Activations {
		for _, pathID := range activation.InputPathIDs {
			if previous, ok := inputSeen[pathID]; ok {
				i.c.add("path_double_consumed", "activations."+activationID, "path %q is input to %q and %q", pathID, previous, activationID)
			} else {
				inputSeen[pathID] = activationID
			}
		}
	}
	for _, id := range sortedMapKeys(i.view.Routing.Paths) {
		p := i.view.Routing.Paths[id]
		path := "paths." + id
		if p.ID != id {
			i.c.add("path_key_mismatch", path, "map key differs from path ID %q", p.ID)
		}
		if !p.Kind.Valid() || !p.State.Valid() {
			i.c.add("path_kind_state_invalid", path, "invalid kind/state %q/%q", p.Kind, p.State)
		}
		if p.SourceActivation.Generation != 1 {
			i.c.add("path_generation", path+".sourceActivation", "source generation is %d, want 1", p.SourceActivation.Generation)
		}
		if p.CreatedSeq < 0 || p.UpdatedSeq < p.CreatedSeq {
			i.c.add("path_sequence", path, "invalid created/updated sequence %d/%d", p.CreatedSeq, p.UpdatedSeq)
		}
		if err := ValidateLineage(p); err != nil {
			i.c.add("path_lineage", path+".candidateLineage", "%v", err)
		}
		if _, ok := i.view.Routing.Scopes[p.ScopeID]; !ok {
			i.c.add("path_scope_missing", path, "scope %q is missing", p.ScopeID)
		}
		if !sortedUnique(p.ProducedPathIDs) {
			i.c.add("path_children_noncanonical", path+".producedPathIds", "children are not sorted and unique")
		}
		for _, childID := range p.ProducedPathIDs {
			child, ok := i.view.Routing.Paths[childID]
			if !ok {
				i.c.add("path_child_missing", path, "child %q is missing", childID)
				continue
			}
			if child.ParentPathID != id {
				i.c.add("path_child_backlink", path, "child %q points to parent %q", childID, child.ParentPathID)
			}
			if child.CreatedSeq != p.UpdatedSeq {
				i.c.add("path_child_event_sequence", path, "child %q was created at %d, parent transitioned at %d", childID, child.CreatedSeq, p.UpdatedSeq)
			}
			if previous, ok := childSeen[childID]; ok && previous != id {
				i.c.add("path_multiple_parents", path, "child %q belongs to %q and %q", childID, previous, id)
			} else {
				childSeen[childID] = id
			}
		}
		if p.ParentPathID != "" {
			parent, ok := i.view.Routing.Paths[p.ParentPathID]
			if !ok {
				i.c.add("path_parent_missing", path, "parent %q is missing", p.ParentPathID)
			} else if !slices.Contains(parent.ProducedPathIDs, id) {
				i.c.add("path_parent_forward_link", path, "parent %q does not list child", p.ParentPathID)
			}
		}
		i.validatePathShape(path, p, inputSeen[id])
	}
}

func (i *aggregateIndex) genesisActivation(id string) bool {
	return i.view.Authority != nil && i.view.Authority.Genesis.ActivationID == id
}
func (i *aggregateIndex) genesisReservation(id string) bool {
	return i.view.Authority != nil && i.view.Authority.Genesis.ReservationID == id
}

func (i *aggregateIndex) validatePathShape(path string, p PathRecord, inputActivation string) {
	if p.DetachmentSetID != "" {
		if _, ok := i.view.Routing.DetachmentSets[p.DetachmentSetID]; !ok {
			i.c.add("path_detachment_set_missing", path, "detachment set %q is missing", p.DetachmentSetID)
		}
	}
	switch p.Kind {
	case PathActivationOutput:
		if p.ParentPathID != "" || p.Edge != nil || p.TargetReservationID != "" || p.CandidateID != "" || p.ArrivalID != "" || p.ArrivedSeq != 0 || p.ImpossibleCauseDigest != "" {
			i.c.add("activation_output_shape", path, "activation output has edge/arrival/parent fields")
		}
		activation, ok := i.view.Routing.Activations[p.SourceActivation.ID]
		if !ok || activation.Ref != p.SourceActivation || activation.OutputPathID != p.ID {
			i.c.add("activation_output_source", path, "source activation/output backlink does not match")
		}
		want, err := ActivationOutputIdentity(p.SourceActivation.ID, p.SourceActivation.Generation)
		if err != nil || want != p.ID {
			i.c.add("activation_output_identity", path, "output identity does not recompute")
		}
		allowed := p.State == PathLive || p.State == PathRouted || p.State == PathSplit || p.State == PathEnded || p.State.TerminalNonSuccess()
		if !allowed {
			i.c.add("activation_output_state", path, "state %q is invalid for activation output", p.State)
		}
		if p.State == PathRouted {
			edges, impossible := 0, 0
			for _, id := range p.ProducedPathIDs {
				child := i.view.Routing.Paths[id]
				if child.Kind == PathEdge {
					edges++
				}
				if child.Kind == PathImpossibleEdge {
					impossible++
				}
			}
			if len(p.ProducedPathIDs) == 0 || edges != 1 || edges+impossible != len(p.ProducedPathIDs) {
				i.c.add("routed_children", path, "routed output must have exactly one edge child and only impossible siblings")
			}
		} else if p.State == PathSplit {
			if len(p.ProducedPathIDs) < 2 || len(p.ProducedPathIDs) > MaxOutgoingOrAllCandidates {
				i.c.add("split_children", path, "split child count %d is outside 2..%d", len(p.ProducedPathIDs), MaxOutgoingOrAllCandidates)
			}
			for _, id := range p.ProducedPathIDs {
				if i.view.Routing.Paths[id].Kind != PathEdge {
					i.c.add("split_child_kind", path, "split child %q is not an edge path", id)
				}
			}
		} else if len(p.ProducedPathIDs) != 0 {
			i.c.add("path_children_for_state", path, "state %q cannot own produced children", p.State)
		}
	case PathEdge, PathImpossibleEdge:
		if p.ParentPathID == "" || p.Edge == nil || p.TargetReservationID == "" || p.CandidateID == "" {
			i.c.add("edge_path_shape", path, "edge path lacks parent/edge/target/candidate")
		}
		if len(p.ProducedPathIDs) != 0 {
			i.c.add("edge_path_children", path, "edge path owns children")
		}
		parent, parentOK := i.view.Routing.Paths[p.ParentPathID]
		if parentOK && p.SourceActivation != parent.SourceActivation {
			i.c.add("edge_source_activation", path, "source activation differs from parent output")
		}
		if parentOK {
			i.validateEdgeLineage(path, parent, p)
			if sourceActivation, ok := i.view.Routing.Activations[parent.SourceActivation.ID]; ok {
				if sourceReservation, ok := i.view.Routing.Reservations[sourceActivation.ReservationID]; ok && p.Edge != nil && p.Edge.FromNodeID != sourceReservation.NodeID {
					i.c.add("edge_source_node", path, "edge source node %q differs from activation node %q", p.Edge.FromNodeID, sourceReservation.NodeID)
				}
			}
			if target, ok := i.view.Routing.Reservations[p.TargetReservationID]; ok && p.Edge != nil {
				if p.Edge.ToNodeID != target.NodeID {
					i.c.add("edge_target_node", path, "edge target node %q differs from reservation node %q", p.Edge.ToNodeID, target.NodeID)
				}
				candidate := i.candidates[candidateKey{target.ID, p.CandidateID}]
				if target.IsReducing {
					if p.ScopeID != target.ReducesScopeID || candidate.Kind != CandidateScopeBranch || candidate.MemberID != p.BranchEdgeID {
						i.c.add("reducing_edge_scope", path, "edge does not enter the exact reducing scope branch candidate")
					}
				} else if p.ScopeID != target.ScopeID || p.BranchEdgeID != target.BranchEdgeID {
					i.c.add("local_edge_scope", path, "edge scope/branch differs from local target reservation")
				}
			}
		}
		if parentOK {
			if parent.State == PathSplit {
				scopeID, found := i.forkScopeByOutput[parent.ID]
				scope := i.view.Routing.Scopes[scopeID]
				matched := found && p.ScopeID == scopeID && p.Edge != nil && p.BranchEdgeID == p.Edge.ID && slices.Contains(scope.ExpectedBranchEdgeIDs, p.BranchEdgeID)
				if !matched {
					i.c.add("split_scope_escape", path, "split child does not enter its exact fork scope/branch")
				}
			} else if p.ScopeID != parent.ScopeID || p.BranchEdgeID != parent.BranchEdgeID {
				i.c.add("route_scope_escape", path, "ordinary route changes scope/branch")
			}
		}
		if p.Edge != nil {
			wantEdge, err := EdgeIdentity(p.Edge.TemplateRef, p.Edge.FromNodeID, p.Edge.Outcome, p.Edge.ToNodeID)
			if err != nil || wantEdge != p.Edge.ID || p.Edge.TemplateRef != i.view.TemplateRef {
				i.c.add("edge_identity", path+".edge", "edge identity/template does not recompute")
			}
		}
		if _, ok := i.candidates[candidateKey{p.TargetReservationID, p.CandidateID}]; !ok {
			i.c.add("edge_candidate_missing", path, "target candidate is not reserved")
		}
		if p.Kind == PathEdge {
			if p.ImpossibleCauseDigest != "" {
				i.c.add("edge_impossible_cause", path, "ordinary edge has impossible cause")
			}
			if p.Edge != nil {
				want, err := EdgePathIdentity(p.SourceActivation.ID, p.ParentPathID, p.Edge.ID, p.TargetReservationID, p.CandidateID)
				if err != nil || want != p.ID {
					i.c.add("edge_path_identity", path, "edge path identity does not recompute")
				}
			}
			arrival, err := ArrivalIdentity(p.ID, p.TargetReservationID, p.CandidateID)
			if err != nil || arrival != p.ArrivalID || p.ArrivedSeq != p.CreatedSeq {
				i.c.add("edge_arrival_identity", path, "arrival identity/sequence does not match creation")
			}
			if p.State != PathArrived && p.State != PathConsumed && p.State != PathDetachedSink && !p.State.TerminalNonSuccess() {
				i.c.add("edge_state", path, "state %q is invalid for edge path", p.State)
			}
		} else {
			want := ""
			var err error
			if p.Edge != nil {
				want, err = ImpossibleEdgePathIdentity(p.ImpossibleCauseDigest, p.Edge.ID, p.TargetReservationID)
			}
			if p.Edge == nil || err != nil || want != p.ID || p.State != PathImpossible || p.ImpossibleCauseDigest == "" {
				i.c.add("impossible_path_identity", path, "impossible path identity/state/cause does not match")
			}
			if p.ArrivalID != "" || p.ArrivedSeq != 0 || p.ConsumedBy != nil || p.Disposition != nil || p.TerminalCauseID != "" {
				i.c.add("impossible_path_shape", path, "impossible path has arrival/consumption/disposition fields")
			}
			if _, ok := i.view.Routing.CauseSets[p.ImpossibleCauseDigest]; !ok {
				i.c.add("impossible_cause_set_missing", path, "cause set %q is missing", p.ImpossibleCauseDigest)
			}
		}
	}

	if p.State == PathConsumed {
		if p.ConsumedBy == nil {
			r, ok := i.view.Routing.Reservations[p.TargetReservationID]
			if !ok || r.State != ReservationClosedNoActivation || p.Disposition == nil || p.Disposition.CommandID != r.CommandID || p.Disposition.EventSeq != r.EventSeq {
				i.c.add("closed_consumption_authority", path, "consumerless path is not exact closed-no-activation input")
			}
		} else if inputActivation == "" || p.ConsumedBy.ID != inputActivation {
			i.c.add("consumption_backlink", path, "consumed path lacks exact activation input backlink")
		}
	} else if p.ConsumedBy != nil {
		i.c.add("consumer_on_unconsumed", path, "state %q has a consumer", p.State)
	}
	i.validateDisposition(path, p)
}

func (i *aggregateIndex) validateEdgeLineage(path string, parent, edge PathRecord) {
	expected := append([]CandidateLineageFrame(nil), parent.CandidateLineage...)
	lineageID := parent.CandidateLineageID
	appendFrame := func(reservationID ReservationID, candidateID CandidateID) {
		id, err := CandidateLineageIdentity(lineageID, reservationID, candidateID)
		if err != nil {
			return
		}
		expected = append(expected, CandidateLineageFrame{ID: id, ParentLineageID: lineageID, ReservationID: reservationID, CandidateID: candidateID})
		lineageID = id
	}
	if parent.State == PathSplit {
		if scopeID, ok := i.forkScopeByOutput[parent.ID]; ok {
			scope := i.view.Routing.Scopes[scopeID]
			if reducer, ok := i.view.Routing.Reservations[scope.JoinReservationID]; ok {
				for _, candidate := range reducer.Candidates {
					if candidate.Kind == CandidateScopeBranch && candidate.MemberID == edge.BranchEdgeID {
						appendFrame(reducer.ID, candidate.ID)
						break
					}
				}
			}
		}
	}
	if len(expected) == len(parent.CandidateLineage) || expected[len(expected)-1].ReservationID != edge.TargetReservationID || expected[len(expected)-1].CandidateID != edge.CandidateID {
		appendFrame(edge.TargetReservationID, edge.CandidateID)
	}
	if !slices.Equal(expected, edge.CandidateLineage) || lineageID != edge.CandidateLineageID || int(edge.LineageDepth) != len(expected) {
		i.c.add("edge_lineage_authority", path+".candidateLineage", "edge lineage is not the exact parent/branch/target causal chain")
	}
}

func (i *aggregateIndex) validateDisposition(path string, p PathRecord) {
	requires := p.State == PathRouted || p.State == PathSplit || p.State == PathConsumed || p.State == PathDetachedSink || p.State.TerminalNonSuccess() || (p.State == PathEnded && p.UpdatedSeq != p.CreatedSeq)
	if !requires {
		if p.Disposition != nil {
			i.c.add("unexpected_disposition", path, "initial state %q has a disposition", p.State)
		}
	} else if p.Disposition == nil {
		i.c.add("disposition_missing", path, "state %q lacks disposition", p.State)
		return
	}
	if p.Disposition == nil {
		return
	}
	d := *p.Disposition
	if d.PathID != p.ID || d.ToState != p.State || d.EventSeq != p.UpdatedSeq || d.ReasonCode == "" {
		i.c.add("disposition_fields", path+".disposition", "disposition does not match path transition")
	}
	wantFrom := map[PathState]map[PathState]bool{
		PathRouted: {PathLive: true}, PathSplit: {PathLive: true}, PathConsumed: {PathArrived: true}, PathDetachedSink: {PathArrived: true}, PathEnded: {PathLive: true},
		PathFailed: {PathLive: true, PathArrived: true}, PathCanceled: {PathLive: true, PathArrived: true}, PathSkipped: {PathLive: true, PathArrived: true},
	}
	if !wantFrom[p.State][d.FromState] {
		i.c.add("disposition_transition", path+".disposition", "transition %q -> %q is illegal", d.FromState, d.ToState)
	}
	seq, err := eventUint(d.EventSeq)
	want := ""
	if err == nil {
		want, err = DispositionReceiptIdentity(p.ID, d.FromState, d.ToState, d.ReasonCode, d.CommandID, d.AdminRecordID, seq)
	}
	if err != nil || want != d.ID {
		i.c.add("disposition_identity", path+".disposition", "disposition identity does not recompute")
	}
	if d.CommandID == "" && d.AdminRecordID == "" {
		i.c.add("disposition_authority_missing", path+".disposition", "transition has neither command nor admin authority")
	}
	terminal := p.State == PathEnded || p.State.TerminalNonSuccess()
	if terminal && d.CommandID == "" {
		i.c.add("terminal_command_capability", path+".disposition", "terminal transition lacks command authority")
	}
	if d.CommandID != "" {
		i.refCommand(d.CommandID, path+".disposition.commandId")
		if command, ok := i.view.Commands[d.CommandID]; ok {
			switch p.State {
			case PathRouted, PathSplit:
				if command.Identity.Kind != CommandRoutePaths && (p.State != PathRouted || command.Identity.Kind != CommandSettleDetachedSink) {
					i.c.add("disposition_command_authority", path+".disposition", "command kind %q cannot route/split", command.Identity.Kind)
				} else if command.Identity.Kind == CommandRoutePaths {
					if command.Identity.SourceActivationID != p.SourceActivation.ID || command.Identity.SourceGeneration != p.SourceActivation.Generation || command.Identity.SourcePathID != p.ID {
						i.c.add("disposition_command_tuple", path+".disposition", "route command does not own the exact source path generation")
					}
				} else {
					child, childOK := i.view.Routing.Paths[command.Identity.SourcePathID]
					reservation, reservationOK := i.view.Routing.Reservations[command.Identity.TargetReservationID]
					if !childOK || !reservationOK || child.ParentPathID != p.ID || child.State != PathDetachedSink || child.Disposition == nil || child.Disposition.CommandID != d.CommandID || child.UpdatedSeq != d.EventSeq || command.Identity.TargetReservationID != child.TargetReservationID || command.Identity.TargetGeneration != reservation.Generation {
						i.c.add("disposition_command_tuple", path+".disposition", "detached route command does not own the exact sink child event")
					}
				}
			case PathConsumed:
				if command.Identity.Kind != CommandActivateGeneration {
					i.c.add("disposition_command_authority", path+".disposition", "command kind %q cannot consume join input", command.Identity.Kind)
				}
			case PathDetachedSink:
				want := CommandSettleDetachedSink
				if d.ReasonCode == "pre_arrived_any_loser" {
					want = CommandActivateGeneration
				}
				if command.Identity.Kind != want {
					i.c.add("disposition_command_authority", path+".disposition", "command kind %q cannot own %q", command.Identity.Kind, d.ReasonCode)
				} else if want == CommandSettleDetachedSink {
					reservation, ok := i.view.Routing.Reservations[p.TargetReservationID]
					if !ok || command.Identity.SourcePathID != p.ID || command.Identity.TargetReservationID != p.TargetReservationID || command.Identity.TargetGeneration != reservation.Generation {
						i.c.add("disposition_command_tuple", path+".disposition", "sink command does not own the exact path/reservation generation")
					}
				} else {
					reservation, ok := i.view.Routing.Reservations[p.TargetReservationID]
					if !ok || command.Identity.TargetReservationID != p.TargetReservationID || command.Identity.TargetGeneration != reservation.Generation {
						i.c.add("disposition_command_tuple", path+".disposition", "activation command does not own the exact sink reservation generation")
					}
				}
			case PathEnded:
				i.validateTerminalCommand(path, p, d, command, CommandRoutePaths)
			case PathFailed, PathCanceled, PathSkipped:
				want := CommandSettleAttempt
				if d.FromState == PathArrived {
					want = CommandActivateGeneration
				}
				i.validateTerminalCommand(path, p, d, command, want)
			}
		}
	}
	if d.AdminRecordID != "" {
		admin, ok := i.view.AdminRecords[d.AdminRecordID]
		if !ok {
			i.c.add("disposition_admin_missing", path+".disposition", "admin record %q is missing", d.AdminRecordID)
		} else if admin.EventSeq != d.EventSeq || admin.ReasonCode != d.ReasonCode {
			i.c.add("disposition_admin_mismatch", path+".disposition", "admin authority tuple differs from disposition")
		}
	}
	if p.State.TerminalNonSuccess() {
		if p.TerminalCauseID == "" {
			i.c.add("terminal_cause_missing", path, "terminal non-success path lacks cause")
		}
	} else if p.TerminalCauseID != "" {
		i.c.add("terminal_cause_unexpected", path, "successful path has terminal cause")
	}
}

func (i *aggregateIndex) validateTerminalCommand(path string, p PathRecord, d DispositionReceipt, record CommandRecord, want CommandKindV1) {
	command := record.Identity
	if command.Kind != want {
		i.c.add("terminal_command_capability", path+".disposition", "command kind %q cannot own terminal transition %q -> %q", command.Kind, d.FromState, p.State)
		return
	}
	exact := false
	switch want {
	case CommandSettleAttempt:
		exact = command.SourceActivationID == p.SourceActivation.ID && command.SourceGeneration == p.SourceActivation.Generation
	case CommandRoutePaths:
		exact = command.SourceActivationID == p.SourceActivation.ID && command.SourceGeneration == p.SourceActivation.Generation && command.SourcePathID == p.ID
	case CommandActivateGeneration:
		reservation, ok := i.view.Routing.Reservations[p.TargetReservationID]
		exact = ok && command.TargetReservationID == p.TargetReservationID && command.TargetGeneration == reservation.Generation
	}
	if !exact {
		i.c.add("terminal_command_tuple", path+".disposition", "terminal command does not name the exact source/target path authority")
	}
	if record.State != CommandObserved && record.State != CommandReconciled {
		i.c.add("terminal_command_state", path+".disposition", "terminal command state %q is not settled authority", record.State)
	}
	if (p.State == PathCanceled || p.State == PathSkipped) && d.AdminRecordID == "" {
		i.c.add("terminal_admin_authority", path+".disposition", "%s terminal transition requires explicit admin authority", p.State)
	}
	switch want {
	case CommandSettleAttempt:
		i.validateSettleAttemptTerminal(path, p, d, record)
	case CommandRoutePaths:
		i.validateRouteTerminal(path, p, d, record)
	}
}

func (i *aggregateIndex) validateActivationsAndReservations() {
	for _, id := range sortedMapKeys(i.view.Routing.Activations) {
		i.validateActivation(id, i.view.Routing.Activations[id])
	}
	for _, id := range sortedMapKeys(i.view.Routing.Reservations) {
		i.validateReservation(id, i.view.Routing.Reservations[id])
	}
}

func (i *aggregateIndex) validateActivation(id string, a ActivationRecord) {
	path := "activations." + id
	if a.ID != id || a.Ref.ID != id || a.Ref.Generation != 1 {
		i.c.add("activation_key_ref", path, "activation key/ref does not match")
	}
	if a.RunID != i.view.RunID {
		i.c.add("activation_run_mismatch", path, "activation belongs to run %q", a.RunID)
	}
	if !sortedUnique(a.InputPathIDs) || (len(a.InputPathIDs) == 0 && !i.genesisActivation(id)) {
		i.c.add("activation_inputs_noncanonical", path+".inputPathIds", "inputs must be sorted/unique and only genesis may be empty")
	}
	if i.genesisActivation(id) && (len(a.InputPathIDs) != 0 || a.ReservationID != i.view.Authority.Genesis.ReservationID || a.OutputPathID != i.view.Authority.Genesis.OutputPathID) {
		i.c.add("genesis_activation_shape", path, "genesis activation differs from exact authority")
	}
	digest, err := InputSetIdentity(a.InputPathIDs)
	if err != nil || digest != a.InputSetDigest {
		i.c.add("activation_input_digest", path, "input digest does not recompute")
	}
	want, err := ActivationIdentity(a.RunID, a.ReservationID, a.Ref.Generation, a.InputSetDigest)
	if err != nil || want != a.ID {
		i.c.add("activation_identity", path, "activation identity does not recompute")
	}
	reservation, ok := i.view.Routing.Reservations[a.ReservationID]
	if !ok {
		i.c.add("activation_reservation_missing", path, "reservation %q is missing", a.ReservationID)
	} else if reservation.State != ReservationActivated || !compareActivationRef(reservation.Activation, &a.Ref) {
		i.c.add("activation_reservation_backlink", path, "reservation does not point to activation")
	}
	if ok && (a.Receipt.JoinPolicy != reservation.JoinPolicy || a.Receipt.CauseDigest != "") {
		i.c.add("activation_receipt_policy", path+".receipt", "receipt policy/cause differs from activated reservation")
	}
	output, ok := i.view.Routing.Paths[a.OutputPathID]
	if !ok || output.SourceActivation != a.Ref {
		i.c.add("activation_output_missing", path, "output %q is missing or has wrong source", a.OutputPathID)
	}
	for _, inputID := range a.InputPathIDs {
		input, ok := i.view.Routing.Paths[inputID]
		if !ok {
			i.c.add("activation_input_missing", path, "input %q is missing", inputID)
			continue
		}
		if input.State != PathConsumed || input.ConsumedBy == nil || *input.ConsumedBy != a.Ref {
			i.c.add("activation_input_unconsumed", path, "input %q is not consumed by activation", inputID)
		} else if input.Disposition == nil || input.Disposition.CommandID != a.CommandID || input.Disposition.EventSeq != a.EventSeq {
			i.c.add("activation_input_event", path, "input %q disposition is not coupled to activation event", inputID)
		}
	}
	i.refCommand(a.CommandID, path+".commandId")
	i.refCommand(a.Receipt.CommandID, path+".receipt.commandId")
	if command, ok := i.view.Commands[a.CommandID]; ok {
		if i.genesisActivation(id) {
			if command.Identity.Kind != CommandInitializeRouting {
				i.c.add("activation_command_authority", path, "genesis activation requires initialize_routing_v1")
			}
		} else if command.Identity.Kind != CommandActivateGeneration || command.Identity.TargetReservationID != a.ReservationID || command.Identity.TargetGeneration != a.Ref.Generation {
			i.c.add("activation_command_authority", path, "command does not authorize this activation generation")
		}
	}
	if a.EventSeq < 0 {
		i.c.add("activation_sequence", path, "negative event sequence")
	}
	if a.Receipt.ActivationID != a.ID || a.Receipt.ReservationID != a.ReservationID || a.Receipt.InputSetDigest != a.InputSetDigest || a.Receipt.OutputPathID != a.OutputPathID || a.Receipt.Result != ReceiptActivated || a.Receipt.CommandID != a.CommandID || a.Receipt.EventSeq != a.EventSeq {
		i.c.add("activation_receipt_fields", path+".receipt", "receipt is not byte-equivalent to activation")
	}
	seq, err := eventUint(a.EventSeq)
	wantReceipt := ""
	if err == nil {
		wantReceipt, err = ActivationReceiptIdentity(a.ID, a.ReservationID, a.InputSetDigest, a.OutputPathID, a.CommandID, seq)
	}
	if err != nil || wantReceipt != a.Receipt.ID {
		i.c.add("activation_receipt_identity", path+".receipt", "receipt identity does not recompute")
	}
}

func (i *aggregateIndex) validateReservation(id string, r ActivationReservation) {
	path := "reservations." + id
	if r.ID != id {
		i.c.add("reservation_key_mismatch", path, "map key differs from reservation ID %q", r.ID)
	}
	if r.RunID != i.view.RunID || r.Generation != 1 {
		i.c.add("reservation_run_generation", path, "run/generation does not match aggregate")
	}
	if _, ok := i.view.Routing.Scopes[r.ScopeID]; !ok {
		i.c.add("reservation_scope_missing", path, "scope %q is missing", r.ScopeID)
	}
	want, err := ReservationIdentity(r.RunID, r.NodeID, r.ScopeID, r.BranchEdgeID, r.Generation)
	if err != nil || want != r.ID {
		i.c.add("reservation_identity", path, "reservation identity does not recompute")
	}
	if !r.JoinPolicy.Valid() || !r.State.Valid() {
		i.c.add("reservation_state_policy", path, "invalid state/policy %q/%q", r.State, r.JoinPolicy)
	}
	if !slices.IsSortedFunc(r.Candidates, func(a, b CandidateRecord) int { return cmp.Compare(a.ID, b.ID) }) {
		i.c.add("reservation_candidates_order", path+".candidates", "candidates are not sorted by ID")
	}
	if !slices.IsSortedFunc(r.PossibleSlots, func(a, b PossibleSlotRecord) int { return cmp.Compare(a.ID, b.ID) }) {
		i.c.add("reservation_slots_order", path+".possibleSlots", "possible slots are not sorted by ID")
	}
	if len(r.Candidates) == 0 && !i.genesisReservation(id) {
		i.c.add("reservation_candidates_empty", path, "reservation has no candidates")
	}
	if i.genesisReservation(id) && (r.NodeID != i.view.Authority.Genesis.StartNodeID || r.ScopeID != i.view.Authority.Genesis.RootScopeID || r.BranchEdgeID != "" || r.JoinPolicy != JoinExclusive || r.IsReducing || len(r.Candidates) != 0 || len(r.PossibleSlots) != 0) {
		i.c.add("genesis_reservation_shape", path, "genesis reservation differs from exact authority")
	}
	if r.JoinPolicy == JoinAny && (len(r.Candidates) < 2 || len(r.Candidates) > MaxAnyCandidates) {
		i.c.add("any_candidate_bound", path, "any candidate count %d is outside 2..%d", len(r.Candidates), MaxAnyCandidates)
	}
	if r.JoinPolicy != JoinAny && len(r.Candidates) > MaxOutgoingOrAllCandidates {
		i.c.add("candidate_bound", path, "candidate count %d exceeds %d", len(r.Candidates), MaxOutgoingOrAllCandidates)
	}
	if r.IsReducing {
		scope, ok := i.view.Routing.Scopes[r.ReducesScopeID]
		if !ok || scope.JoinReservationID != r.ID || scope.JoinNodeID != r.NodeID || r.ScopeID != r.ReducesScopeID {
			i.c.add("reducing_authority", path, "reservation is not the named reducer for its scope")
		}
		if r.JoinPolicy == JoinExclusive {
			i.c.add("reducing_exclusive", path, "exclusive reservation cannot reduce a scope")
		}
	} else if r.ReducesScopeID != "" {
		i.c.add("local_reduces_scope", path, "local reservation names a reduced scope")
	}

	memberSeen, slotSeen := map[string]struct{}{}, map[string]struct{}{}
	for _, candidate := range r.Candidates {
		if !candidate.Kind.Valid() || (r.IsReducing && candidate.Kind != CandidateScopeBranch) || (!r.IsReducing && candidate.Kind != CandidateInboundEdge) {
			i.c.add("candidate_kind", path, "candidate %q has invalid kind %q", candidate.ID, candidate.Kind)
		}
		want, err := CandidateIdentity(r.ID, candidate.Kind, candidate.MemberID)
		if err != nil || want != candidate.ID {
			i.c.add("candidate_identity", path, "candidate %q identity does not recompute", candidate.ID)
		}
		if _, ok := memberSeen[candidate.MemberID]; ok {
			i.c.add("candidate_member_duplicate", path, "candidate member %q is duplicated", candidate.MemberID)
		}
		memberSeen[candidate.MemberID] = struct{}{}
		if !sortedUnique(candidate.PossibleSlotIDs) || len(candidate.PossibleSlotIDs) == 0 {
			i.c.add("candidate_slots_noncanonical", path, "candidate %q slots must be nonempty, sorted, and unique", candidate.ID)
		}
		for _, slotID := range candidate.PossibleSlotIDs {
			slot, ok := i.slots[slotID]
			if !ok || slot.ReservationID != r.ID || slot.CandidateID != candidate.ID {
				i.c.add("candidate_slot_missing", path, "candidate %q slot %q is missing/mismatched", candidate.ID, slotID)
			}
			if _, duplicate := slotSeen[slotID]; duplicate {
				i.c.add("slot_shared", path, "slot %q belongs to multiple candidates", slotID)
			}
			slotSeen[slotID] = struct{}{}
		}
	}
	if r.IsReducing {
		if scope, ok := i.view.Routing.Scopes[r.ReducesScopeID]; ok {
			members := make([]string, 0, len(memberSeen))
			for member := range memberSeen {
				members = append(members, member)
			}
			slices.Sort(members)
			if !slices.Equal(members, scope.ExpectedBranchEdgeIDs) {
				i.c.add("reducing_branch_set", path, "candidate members differ from complete expected scope branches")
			}
		}
	}
	if len(slotSeen) != len(r.PossibleSlots) {
		i.c.add("reservation_slots_incomplete", path, "candidate slot union has %d entries, reservation stores %d", len(slotSeen), len(r.PossibleSlots))
	}
	for _, slot := range r.PossibleSlots {
		want, err := PossibleSlotIdentity(slot.ReservationID, slot.CandidateID, slot.SourceNodeID, slot.SourceEdgeID, slot.SourceScopeID, slot.SourceBranchEdgeID, slot.Generation)
		if err != nil || slot.ID != want || slot.ReservationID != r.ID || slot.Generation != r.Generation {
			i.c.add("slot_identity", path, "slot %q identity/owner does not recompute", slot.ID)
		}
		if _, ok := i.view.Routing.Scopes[slot.SourceScopeID]; !ok {
			i.c.add("slot_source_scope_missing", path, "slot %q source scope is missing", slot.ID)
		}
	}

	switch r.State {
	case ReservationOpen:
		if r.Activation != nil || r.CloseReceipt != nil || r.ClosedReason != "" || r.CauseDigest != "" || r.CommandID != "" {
			i.c.add("open_reservation_closed_fields", path, "open reservation has terminal authority fields")
		}
	case ReservationActivated:
		if r.Activation == nil || r.CloseReceipt != nil || r.ClosedReason != "" || r.CauseDigest != "" {
			i.c.add("activated_reservation_fields", path, "activated reservation has incomplete/conflicting terminal fields")
		}
		if r.Activation != nil {
			if a, ok := i.view.Routing.Activations[r.Activation.ID]; !ok || a.Ref != *r.Activation || a.ReservationID != r.ID || a.CommandID != r.CommandID || a.EventSeq != r.EventSeq {
				i.c.add("reservation_activation_mismatch", path, "activation record does not exactly match")
			}
		}
		i.refCommand(r.CommandID, path+".commandId")
		i.validateReservationCommand(path, r)
	case ReservationClosedNoActivation:
		if r.Activation != nil || r.CloseReceipt == nil || (r.ClosedReason != string(ScopeCloseAllImpossible) && r.ClosedReason != string(ScopeCloseCandidateNonSuccess)) || r.CauseDigest == "" {
			i.c.add("closed_reservation_fields", path, "closed-no-activation reservation has incomplete/conflicting fields")
		}
		if _, ok := i.view.Routing.CauseSets[r.CauseDigest]; !ok {
			i.c.add("reservation_cause_set_missing", path, "cause set %q is missing", r.CauseDigest)
		}
		i.refCommand(r.CommandID, path+".commandId")
		i.validateReservationCommand(path, r)
		if r.CloseReceipt != nil {
			receipt := r.CloseReceipt
			i.refCommand(receipt.CommandID, path+".closeReceipt.commandId")
			if receipt.ActivationID != "" || receipt.ReservationID != r.ID || receipt.OutputPathID != "" || receipt.Result != ReceiptClosedNoActivation || receipt.JoinPolicy != r.JoinPolicy || receipt.CauseDigest != r.CauseDigest || receipt.CommandID != r.CommandID || receipt.EventSeq != r.EventSeq {
				i.c.add("close_receipt_fields", path+".closeReceipt", "close receipt does not match reservation")
			}
			seq, err := eventUint(r.EventSeq)
			want := ""
			if err == nil {
				want, err = ActivationReceiptIdentity("", r.ID, receipt.InputSetDigest, "", r.CommandID, seq)
			}
			if err != nil || want != receipt.ID {
				i.c.add("close_receipt_identity", path+".closeReceipt", "close receipt identity does not recompute")
			}
			if r.IsReducing {
				s := i.view.Routing.Scopes[r.ReducesScopeID]
				if receipt.ScopeID != s.ParentScopeID || receipt.BranchEdgeID != s.ParentBranchEdgeID || receipt.ReducedScopeID != s.ID {
					i.c.add("close_receipt_scope", path+".closeReceipt", "receipt does not pop exact reducing scope")
				}
			} else if receipt.ScopeID != r.ScopeID || receipt.BranchEdgeID != r.BranchEdgeID || receipt.ReducedScopeID != "" {
				i.c.add("close_receipt_scope", path+".closeReceipt", "local close receipt changes scope")
			}
			consumed := make([]string, 0)
			for _, candidate := range r.Candidates {
				for _, pathID := range i.pathsByTarget[candidateKey{r.ID, candidate.ID}] {
					p := i.view.Routing.Paths[pathID]
					if p.State == PathConsumed && p.ConsumedBy == nil {
						consumed = append(consumed, p.ID)
					}
				}
			}
			digest, err := InputSetIdentity(consumed)
			if err != nil || digest != receipt.InputSetDigest {
				i.c.add("close_receipt_input_digest", path+".closeReceipt", "input digest does not name all and only consumed arrivals")
			}
		}
	}
	if r.State != ReservationOpen {
		if err := validateClosedReservationFold(i, r); err != nil {
			i.c.add("reservation_fold_mismatch", path, "%v", err)
		}
		i.validateClosedReservationCause(r)
	}
	if r.IsReducing {
		s := i.view.Routing.Scopes[r.ReducesScopeID]
		if r.State == ReservationOpen && s.State != ScopeOpen {
			i.c.add("scope_reservation_state", path, "open reducer has closed scope")
		}
		if r.State == ReservationActivated && (s.State != ScopeClosedActivated || s.ClosedByCommandID != r.CommandID || s.EventSeq != r.EventSeq) {
			i.c.add("scope_reservation_state", path, "activated reducer does not exactly close scope")
		}
		if r.State == ReservationClosedNoActivation && (s.State != ScopeClosedNoActivation || s.ClosedByCommandID != r.CommandID || s.EventSeq != r.EventSeq || string(s.CloseReason) != r.ClosedReason) {
			i.c.add("scope_reservation_state", path, "closed reducer does not exactly close scope")
		}
	}
	i.validateActivationScope(r)
}

func (i *aggregateIndex) validateReservationCommand(path string, r ActivationReservation) {
	command, ok := i.view.Commands[r.CommandID]
	if !ok {
		return
	}
	if i.genesisReservation(r.ID) {
		if command.Identity.Kind != CommandInitializeRouting {
			i.c.add("reservation_command_authority", path, "genesis reservation requires initialize_routing_v1")
		}
		return
	}
	if command.Identity.Kind != CommandActivateGeneration || command.Identity.TargetReservationID != r.ID || command.Identity.TargetGeneration != r.Generation {
		i.c.add("reservation_command_authority", path, "command does not own this exact activation reservation generation")
	}
}

func (i *aggregateIndex) validateActivationScope(r ActivationReservation) {
	if r.State != ReservationActivated || r.Activation == nil {
		return
	}
	a, ok := i.view.Routing.Activations[r.Activation.ID]
	if !ok {
		return
	}
	output, ok := i.view.Routing.Paths[a.OutputPathID]
	if !ok {
		return
	}
	inputs := make([]PathRecord, 0, len(a.InputPathIDs))
	for _, id := range a.InputPathIDs {
		if p, ok := i.view.Routing.Paths[id]; ok {
			inputs = append(inputs, p)
		}
	}
	if i.genesisReservation(r.ID) {
		if len(inputs) != 0 || output.ScopeID != i.view.Authority.Genesis.RootScopeID || output.BranchEdgeID != "" || len(output.CandidateLineage) != 0 || output.CandidateLineageID != "" || a.Receipt.ScopeID != output.ScopeID || a.Receipt.BranchEdgeID != "" || a.Receipt.ReducedScopeID != "" {
			i.c.add("genesis_scope_shape", "reservations."+r.ID, "genesis output does not exactly enter the root scope")
		}
		return
	}
	frames, lineageID, err := PopConsumedLineage(inputs, r.ID)
	if err != nil {
		i.c.add("activation_lineage_pop", "reservations."+r.ID, "%v", err)
		return
	}
	if !slices.Equal(frames, output.CandidateLineage) || lineageID != output.CandidateLineageID {
		i.c.add("activation_output_lineage", "reservations."+r.ID, "output lineage is not exact common input remainder")
	}
	if r.IsReducing {
		s := i.view.Routing.Scopes[r.ReducesScopeID]
		if output.ScopeID != s.ParentScopeID || output.BranchEdgeID != s.ParentBranchEdgeID || a.Receipt.ReducedScopeID != s.ID || a.Receipt.ScopeID != s.ParentScopeID || a.Receipt.BranchEdgeID != s.ParentBranchEdgeID {
			i.c.add("scope_escape", "reservations."+r.ID, "reducing activation output does not pop exactly one scope")
		}
	} else if output.ScopeID != r.ScopeID || output.BranchEdgeID != r.BranchEdgeID || a.Receipt.ReducedScopeID != "" || a.Receipt.ScopeID != r.ScopeID || a.Receipt.BranchEdgeID != r.BranchEdgeID {
		i.c.add("local_scope_change", "reservations."+r.ID, "local activation changes scope/branch")
	}
}
