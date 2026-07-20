package epochv8

import (
	"cmp"
	"fmt"
	"reflect"
	"slices"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// VerifyCheckpointV8 reconstructs the checkpoint from the immutable epoch-zero
// anchor and ordered history. Any missing, reordered, or internally coherent
// but unauthorized final summary fails closed.
func VerifyCheckpointV8(checkpoint *CheckpointV8) error {
	if checkpoint == nil {
		return fmt.Errorf("%w: checkpoint is nil", ErrInvalid)
	}
	wire := checkpoint.wire
	if err := ensureCheckpointWireBudget(wire); err != nil {
		return err
	}
	if wire.StateSchemaVersion != StateSchemaVersion || wire.Protocol != Protocol || wire.Encoding != Encoding {
		return fmt.Errorf("%w: checkpoint protocol envelope is invalid", ErrInvalid)
	}
	if len(wire.Epochs) == 0 || len(wire.Epochs) > MaxEpochs || len(wire.History) > MaxHistoryEvents || len(wire.Authorities) > MaxAuthorities {
		return fmt.Errorf("%w: checkpoint collection bounds are invalid", ErrOverBudget)
	}
	if !validIdentifier(wire.Anchor.RunID) || !capabilitiesValid(wire.Anchor.Capabilities, true) {
		return fmt.Errorf("%w: initialization run/capability anchor is invalid", ErrInvalid)
	}
	if wire.Anchor.OriginalEpoch.Ordinal != 0 || wire.Anchor.OriginalEpoch.PredecessorEpochID != "" {
		return fmt.Errorf("%w: original epoch is not epoch zero", ErrInvalid)
	}
	if err := validateEpoch(wire.Anchor.RunID, wire.Anchor.OriginalEpoch); err != nil {
		return err
	}
	wantAnchorDigest, err := anchorDigest(wire.Anchor)
	if err != nil || wantAnchorDigest != wire.Anchor.Digest {
		return fmt.Errorf("%w: initialization anchor digest mismatch", ErrInvalid)
	}
	if !reflect.DeepEqual(wire.Epochs[0], wire.Anchor.OriginalEpoch) {
		return fmt.Errorf("%w: epoch zero differs from initialization anchor", ErrInvalid)
	}
	if err := validateAuthorities(wire.Anchor.RunID, []TemplateEpoch{wire.Anchor.OriginalEpoch}, wire.Anchor.InitialAuthorities, true); err != nil {
		return fmt.Errorf("%w: initial authorities: %v", ErrInvalid, err)
	}

	prefix := checkpointWire{
		StateSchemaVersion: StateSchemaVersion, Protocol: Protocol, Encoding: Encoding,
		Anchor: cloneWire(wire).Anchor, CurrentEpochID: wire.Anchor.OriginalEpoch.ID,
		Epochs: []TemplateEpoch{cloneEpoch(wire.Anchor.OriginalEpoch)}, History: []HistoryEvent{},
		Authorities: cloneAuthorities(wire.Anchor.InitialAuthorities),
	}
	prefix.Digest, err = checkpointDigest(prefix)
	if err != nil {
		return err
	}
	seenApplySource := make(map[OwnerIdentity]struct{})
	seenApplyTarget := make(map[OwnerIdentity]struct{})
	seenFinishIdentity := make(map[OwnerIdentity]struct{})
	for i, event := range wire.History {
		if event.Revision != uint64(i+1) {
			return fmt.Errorf("%w: history revision %d is out of order", ErrInvalid, event.Revision)
		}
		wantEventDigest, digestErr := historyEventDigest(event)
		if digestErr != nil || wantEventDigest != event.Digest {
			return fmt.Errorf("%w: history event %d digest mismatch", ErrInvalid, event.Revision)
		}
		base := prefix.Binding()
		switch event.Kind {
		case HistoryApply:
			if event.Apply == nil || event.Finish != nil {
				return fmt.Errorf("%w: apply event shape is invalid", ErrInvalid)
			}
			if event.Apply.BaseBinding != base {
				return fmt.Errorf("%w: apply event base binding is not the exact predecessor", ErrInvalid)
			}
			if err := validateApplyCoreStatic(wire.Anchor.RunID, event.Apply.applyCore); err != nil {
				return err
			}
			wantRecordDigest, recordErr := applyRecordDigest(*event.Apply)
			if recordErr != nil || wantRecordDigest != event.Apply.RecordDigest {
				return fmt.Errorf("%w: apply record digest mismatch", ErrInvalid)
			}
			if event.Apply.PredecessorEpoch != prefix.CurrentEpochID ||
				event.Apply.CandidateEpoch.Ordinal != uint64(len(prefix.Epochs)) ||
				event.Apply.CandidateEpoch.PredecessorEpochID != prefix.CurrentEpochID {
				return fmt.Errorf("%w: apply epoch chain is not append-only", ErrInvalid)
			}
			if !capabilitySubset(event.Apply.CandidateEpoch.RequiredCapabilities, wire.Anchor.Capabilities) {
				return fmt.Errorf("%w: candidate escalates initialization capabilities", ErrInvalid)
			}
			protected, closureErr := protectedClosure(prefix.Authorities)
			if closureErr != nil || !reflect.DeepEqual(protected, event.Apply.Protected) {
				return fmt.Errorf("%w: apply protected closure is incomplete", ErrInvalid)
			}
			before := prefix.Epochs[len(prefix.Epochs)-1]
			diff, diffErr := computeDiff(before, event.Apply.CandidateEpoch)
			if diffErr != nil || !reflect.DeepEqual(diff, event.Apply.Diff) {
				return fmt.Errorf("%w: apply diff is incomplete or noncanonical", ErrInvalid)
			}
			for _, handoff := range event.Apply.HandoffSet {
				if handoff.Action != HandoffTransfer || handoff.Target == nil {
					continue
				}
				if _, duplicate := seenApplySource[handoff.Source]; duplicate {
					return fmt.Errorf("%w: owner identity has a second handoff", ErrInvalid)
				}
				if _, duplicate := seenApplyTarget[handoff.Target.Identity]; duplicate {
					return fmt.Errorf("%w: handoff successor is duplicated", ErrInvalid)
				}
				seenApplySource[handoff.Source] = struct{}{}
				seenApplyTarget[handoff.Target.Identity] = struct{}{}
			}
			dependencies, dependencyErr := newAuthorityDependencyIndex(prefix.Authorities)
			if dependencyErr != nil {
				return dependencyErr
			}
			nextAuthorities, applyErr := applyHandoffSet(wire.Anchor.RunID, prefix.Authorities, event.Apply.HandoffSet, dependencies)
			if applyErr != nil {
				return applyErr
			}
			prefix.Authorities = nextAuthorities
			prefix.Epochs = append(prefix.Epochs, cloneEpoch(event.Apply.CandidateEpoch))
			prefix.CurrentEpochID = event.Apply.CandidateEpoch.ID
		case HistoryFinishClaimed:
			if event.Finish == nil || event.Apply != nil {
				return fmt.Errorf("%w: finish event shape is invalid", ErrInvalid)
			}
			receipt := *event.Finish
			if receipt.BaseBinding != base || !receipt.Result.valid() || !canonicalDigest(receipt.EvidenceDigest) {
				return fmt.Errorf("%w: finish receipt binding/result is invalid", ErrInvalid)
			}
			wantReceiptID, receiptErr := finishIdentity(receipt)
			if receiptErr != nil || wantReceiptID != receipt.ID {
				return fmt.Errorf("%w: finish receipt identity mismatch", ErrInvalid)
			}
			if _, duplicate := seenFinishIdentity[receipt.Identity]; duplicate {
				return fmt.Errorf("%w: owner identity has a second finish", ErrInvalid)
			}
			authority, ok := authorityByID(prefix.Authorities, receipt.Identity)
			if !ok || authority.Kind != AuthorityFrontier || authority.State != AuthorityClaimed || authority.EpochID != receipt.OwnerEpochID {
				return fmt.Errorf("%w: finish receipt does not settle claimed owner epoch", ErrInvalid)
			}
			dependencies, dependencyErr := newAuthorityDependencyIndex(prefix.Authorities)
			if dependencyErr != nil {
				return dependencyErr
			}
			if dependencies.hasActiveDependent(authority.Identity) {
				return fmt.Errorf("%w: finish receipt bypasses active dependent authority", ErrInvalid)
			}
			for j := range prefix.Authorities {
				if prefix.Authorities[j].Identity == receipt.Identity {
					prefix.Authorities[j].State = receipt.Result.authorityState()
					prefix.Authorities[j].TerminalRecordID = receipt.ID
				}
			}
			seenFinishIdentity[receipt.Identity] = struct{}{}
		default:
			return fmt.Errorf("%w: unknown history event kind %q", ErrInvalid, event.Kind)
		}
		prefix.History = append(prefix.History, cloneHistory([]HistoryEvent{event})[0])
		prefix.Digest, err = checkpointDigest(prefix)
		if err != nil {
			return err
		}
	}
	if err := validateAuthorities(wire.Anchor.RunID, prefix.Epochs, prefix.Authorities, false); err != nil {
		return err
	}
	if err := validateComposedGraph(prefix.Epochs, prefix.Authorities); err != nil {
		return err
	}
	if !reflect.DeepEqual(prefix.Epochs, wire.Epochs) || prefix.CurrentEpochID != wire.CurrentEpochID ||
		!reflect.DeepEqual(prefix.Authorities, wire.Authorities) || !reflect.DeepEqual(prefix.History, wire.History) {
		return fmt.Errorf("%w: checkpoint summary differs from replayed history", ErrInvalid)
	}
	wantDigest, err := checkpointDigest(wire)
	if err != nil || wantDigest != wire.Digest || prefix.Digest != wire.Digest {
		return fmt.Errorf("%w: checkpoint digest mismatch", ErrInvalid)
	}
	return nil
}

func (wire checkpointWire) Binding() Binding {
	return Binding{Revision: uint64(len(wire.History)), Digest: wire.Digest}
}

func validateEpochPrototype(epoch TemplateEpoch) error {
	if epoch.ID != "" || !canonicalTemplateRef(epoch.TemplateRef) || !canonicalDigest(epoch.TemplateSourceDigest) ||
		!capabilitiesValid(epoch.RequiredCapabilities, false) || len(epoch.RequiredCapabilities) == 0 {
		return fmt.Errorf("%w: candidate epoch artifact/capabilities are invalid", ErrInvalid)
	}
	return validateGraph(epoch.Graph, epoch.RequiredCapabilities)
}

func validateEpoch(runID string, epoch TemplateEpoch) error {
	copy := cloneEpoch(epoch)
	copy.ID = ""
	if err := validateEpochPrototype(copy); err != nil {
		return err
	}
	want, err := epochIdentity(runID, epoch)
	if err != nil || want != epoch.ID || !canonicalDigest(string(epoch.ID)) {
		return fmt.Errorf("%w: epoch identity mismatch", ErrInvalid)
	}
	return nil
}

func validateGraph(graph EpochGraph, required []Capability) error {
	if len(graph.Nodes) == 0 || len(graph.Nodes) > model.MaxNormalizedNodes || len(graph.Edges) > model.MaxNormalizedEdges {
		return fmt.Errorf("%w: epoch graph cardinality", ErrOverBudget)
	}
	if !slices.IsSortedFunc(graph.Nodes, func(a, b GraphNode) int { return cmp.Compare(a.ID, b.ID) }) ||
		!slices.IsSortedFunc(graph.Edges, compareGraphEdge) {
		return fmt.Errorf("%w: epoch graph is not canonically ordered", ErrInvalid)
	}
	nodes := make(map[string]GraphNode, len(graph.Nodes))
	capSet := map[Capability]struct{}{}
	for i, node := range graph.Nodes {
		if !validIdentifier(node.ID) || !canonicalDigest(node.SemanticDigest) || !capabilitiesValid(node.RequiredCapabilities, false) || len(node.RequiredCapabilities) == 0 {
			return fmt.Errorf("%w: graph node %d is invalid", ErrInvalid, i)
		}
		if i > 0 && graph.Nodes[i-1].ID == node.ID {
			return fmt.Errorf("%w: graph node is duplicated", ErrInvalid)
		}
		switch node.Type {
		case string(model.NodeTypeTask), string(model.NodeTypeDecision), string(model.NodeTypeWait), string(model.NodeTypeStart), string(model.NodeTypeEnd), string(model.NodeTypeParallel):
		default:
			return fmt.Errorf("%w: graph node type %q is unsupported", ErrInvalid, node.Type)
		}
		if node.Join != "" && node.Join != string(model.JoinAll) && node.Join != string(model.JoinAny) {
			return fmt.Errorf("%w: graph node join policy is invalid", ErrInvalid)
		}
		expectedCapabilities := []Capability{CapabilityFoundationV1}
		if node.Type == string(model.NodeTypeParallel) || node.Join != "" {
			expectedCapabilities = append(expectedCapabilities, CapabilityParallelAllV1)
		}
		if node.Join == string(model.JoinAny) {
			expectedCapabilities = append(expectedCapabilities, CapabilityParallelAnyV1)
		}
		if !slices.Equal(node.RequiredCapabilities, expectedCapabilities) {
			return fmt.Errorf("%w: graph node capability classification mismatch", ErrInvalid)
		}
		for _, capability := range node.RequiredCapabilities {
			capSet[capability] = struct{}{}
		}
		nodes[node.ID] = node
	}
	derived := make([]Capability, 0, len(capSet))
	for capability := range capSet {
		derived = append(derived, capability)
	}
	slices.Sort(derived)
	if !slices.Equal(derived, required) {
		return fmt.Errorf("%w: graph capability summary mismatch", ErrInvalid)
	}
	adjacency := make(map[string][]string, len(nodes))
	routes := make(map[string]struct{}, len(graph.Edges))
	entryEdges := 0
	entryTarget := ""
	for i, edge := range graph.Edges {
		if edge.From != "" && !validIdentifier(edge.From) || edge.From == "" && edge.Outcome != "start" ||
			len(edge.Outcome) > MaxIdentifierBytes || !validIdentifier(edge.To) {
			return fmt.Errorf("%w: graph edge %d is invalid (%q, %q, %q)", ErrInvalid, i, edge.From, edge.Outcome, edge.To)
		}
		if edge.From != "" {
			if _, ok := nodes[edge.From]; !ok {
				return fmt.Errorf("%w: graph edge source is absent", ErrInvalid)
			}
		}
		if _, ok := nodes[edge.To]; !ok {
			return fmt.Errorf("%w: graph edge target is absent", ErrInvalid)
		}
		if edge.From == "" {
			entryEdges++
			entryTarget = edge.To
		}
		routeKey := edge.From + "\x00" + edge.Outcome
		if _, duplicate := routes[routeKey]; duplicate {
			return fmt.Errorf("%w: graph route is ambiguous", ErrInvalid)
		}
		routes[routeKey] = struct{}{}
		if i > 0 && graph.Edges[i-1] == edge {
			return fmt.Errorf("%w: graph edge is duplicated", ErrInvalid)
		}
		if edge.From != "" {
			adjacency[edge.From] = append(adjacency[edge.From], edge.To)
		}
	}
	if entryEdges != 1 {
		return fmt.Errorf("%w: graph has %d entry edges, want one", ErrInvalid, entryEdges)
	}
	for id, node := range nodes {
		degree := len(adjacency[id])
		if node.Type == string(model.NodeTypeEnd) && degree != 0 || node.Type != string(model.NodeTypeEnd) && degree == 0 ||
			node.Type == string(model.NodeTypeParallel) && degree < 2 {
			return fmt.Errorf("%w: graph node %q has invalid outgoing degree", ErrInvalid, id)
		}
	}
	reachable := map[string]struct{}{}
	stack := []string{entryTarget}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, seen := reachable[id]; seen {
			continue
		}
		reachable[id] = struct{}{}
		stack = append(stack, adjacency[id]...)
	}
	if len(reachable) != len(nodes) {
		return fmt.Errorf("%w: epoch graph contains unreachable nodes", ErrInvalid)
	}
	if graphHasCycle(nodes, adjacency) {
		return fmt.Errorf("%w: epoch graph contains a cycle", ErrInvalid)
	}
	wantDigest, err := graphDigest(graph)
	if err != nil || wantDigest != graph.Digest {
		return fmt.Errorf("%w: epoch graph digest mismatch", ErrInvalid)
	}
	return nil
}

func validateAuthorities(runID string, epochs []TemplateEpoch, authorities []AuthorityRecord, initialization bool) error {
	if len(authorities) > MaxAuthorities || !slices.IsSortedFunc(authorities, func(a, b AuthorityRecord) int { return cmp.Compare(a.Identity, b.Identity) }) {
		return fmt.Errorf("%w: authority collection is over budget or noncanonical", ErrInvalid)
	}
	epochByID := make(map[EpochID]TemplateEpoch, len(epochs))
	epochOrdinal := make(map[EpochID]uint64, len(epochs))
	nodeSets := make(map[EpochID]map[string]struct{}, len(epochs))
	for i, epoch := range epochs {
		if err := validateEpoch(runID, epoch); err != nil {
			return err
		}
		if epoch.Ordinal != uint64(i) || i > 0 && epoch.PredecessorEpochID != epochs[i-1].ID {
			return fmt.Errorf("%w: epoch sequence is not append-only", ErrInvalid)
		}
		epochByID[epoch.ID] = epoch
		epochOrdinal[epoch.ID] = epoch.Ordinal
		nodeSets[epoch.ID] = epochNodeSet(epoch)
	}
	byID := make(map[OwnerIdentity]AuthorityRecord, len(authorities))
	links := 0
	// Reservation IDs are run-wide frontier materialization keys. Historical
	// handed-off and finished authorities remain in this set permanently. A
	// LocalID+NodeID may recur only along its exact atomic successor chain.
	frontierReservations := make(map[string]OwnerIdentity)
	frontierMaterializations := make(map[frontierMaterializationKey][]AuthorityRecord)
	for i, authority := range authorities {
		if i > 0 && authorities[i-1].Identity == authority.Identity {
			return fmt.Errorf("%w: authority identity is duplicated", ErrInvalid)
		}
		if _, ok := epochByID[authority.EpochID]; !ok || !validIdentifier(authority.LocalID) || !validIdentifier(authority.ReservationID) || !validIdentifier(authority.NodeID) {
			return fmt.Errorf("%w: authority %q owner fields are invalid", ErrInvalid, authority.Identity)
		}
		if _, ok := nodeSets[authority.EpochID][authority.NodeID]; !ok {
			return fmt.Errorf("%w: authority node is absent from owner epoch", ErrInvalid)
		}
		want, err := authorityIdentity(runID, authority)
		if err != nil || want != authority.Identity || !canonicalDigest(string(authority.Identity)) {
			return fmt.Errorf("%w: authority identity mismatch", ErrInvalid)
		}
		if !authorityKindValid(authority.Kind) || !authorityStateForKind(authority.Kind, authority.State) {
			return fmt.Errorf("%w: authority kind/state mismatch", ErrInvalid)
		}
		if !slices.IsSorted(authority.DependsOn) || len(slices.Compact(slices.Clone(authority.DependsOn))) != len(authority.DependsOn) {
			return fmt.Errorf("%w: authority dependencies are noncanonical", ErrInvalid)
		}
		links += len(authority.DependsOn)
		if links > MaxAuthorityLinks {
			return &OverBudgetError{Limit: "authority_links", Value: links, Maximum: MaxAuthorityLinks}
		}
		if initialization && authority.State.terminal() {
			return fmt.Errorf("%w: initialization cannot mint terminal authority", ErrInvalid)
		}
		if authority.State == AuthorityHandedOff {
			if authority.Successor == "" || !canonicalDigest(authority.TerminalRecordID) {
				return fmt.Errorf("%w: handed-off authority lacks its terminal receipt", ErrInvalid)
			}
		} else if authority.State.terminal() {
			if authority.Successor != "" || !canonicalDigest(authority.TerminalRecordID) {
				return fmt.Errorf("%w: finished authority terminal record is invalid", ErrInvalid)
			}
		} else if authority.Successor != "" || authority.TerminalRecordID != "" {
			return fmt.Errorf("%w: active authority carries terminal fields", ErrInvalid)
		}
		if authority.Kind == AuthorityFrontier {
			if owner, exists := frontierReservations[authority.ReservationID]; exists && owner != authority.Identity {
				return fmt.Errorf("%w: materialized frontier reservation is reused", ErrInvalid)
			}
			frontierReservations[authority.ReservationID] = authority.Identity
			key := frontierMaterializationKey{authority.LocalID, authority.NodeID}
			frontierMaterializations[key] = append(frontierMaterializations[key], authority)
		}
		byID[authority.Identity] = authority
	}
	parents := make(map[OwnerIdentity][]OwnerIdentity, len(authorities))
	for _, authority := range authorities {
		for _, dependency := range authority.DependsOn {
			if dependency == authority.Identity {
				return fmt.Errorf("%w: authority depends on itself", ErrInvalid)
			}
			parent, ok := byID[dependency]
			if !ok || epochOrdinal[parent.EpochID] > epochOrdinal[authority.EpochID] {
				return fmt.Errorf("%w: authority dependency is absent or from a future epoch", ErrInvalid)
			}
		}
		parents[authority.Identity] = authority.DependsOn
	}
	if authorityDependencyCycle(parents) {
		return fmt.Errorf("%w: authority dependency graph contains a cycle", ErrInvalid)
	}
	for _, materializations := range frontierMaterializations {
		slices.SortFunc(materializations, func(a, b AuthorityRecord) int {
			if order := cmp.Compare(epochOrdinal[a.EpochID], epochOrdinal[b.EpochID]); order != 0 {
				return order
			}
			return cmp.Compare(a.Identity, b.Identity)
		})
		for i := 1; i < len(materializations); i++ {
			previous, current := materializations[i-1], materializations[i]
			if previous.State != AuthorityHandedOff || previous.Successor != current.Identity ||
				epochOrdinal[previous.EpochID] >= epochOrdinal[current.EpochID] ||
				!slices.Contains(current.DependsOn, previous.Identity) {
				return fmt.Errorf("%w: historical logical frontier re-enters outside its atomic handoff", ErrInvalid)
			}
		}
	}
	for _, authority := range authorities {
		if authority.State != AuthorityHandedOff {
			continue
		}
		successor, ok := byID[authority.Successor]
		if !ok || successor.Kind != AuthorityFrontier ||
			epochOrdinal[successor.EpochID] <= epochOrdinal[authority.EpochID] || !slices.Contains(successor.DependsOn, authority.Identity) {
			return fmt.Errorf("%w: handed-off successor violates forward one-to-one ownership", ErrInvalid)
		}
	}
	return nil
}

func validateApplyCoreStatic(runID string, core applyCore) error {
	if err := ensureApplyCoreWireBudget(core); err != nil {
		return err
	}
	if core.RunID != runID || !validIdentifier(core.RunID) {
		return fmt.Errorf("%w: apply run binding is invalid", ErrInvalid)
	}
	if err := core.BaseBinding.validate(); err != nil {
		return err
	}
	if core.CandidateEpoch.Ordinal == 0 || core.CandidateEpoch.PredecessorEpochID != core.PredecessorEpoch ||
		!canonicalDigest(string(core.PredecessorEpoch)) || core.ReasonDigest != "" && !canonicalDigest(core.ReasonDigest) {
		return fmt.Errorf("%w: apply epoch/reason binding is invalid", ErrInvalid)
	}
	if err := validateEpoch(runID, core.CandidateEpoch); err != nil {
		return err
	}
	wantDiff, err := diffDigest(core.Diff)
	if err != nil || wantDiff != core.Diff.Digest || !canonicalDiff(core.Diff) {
		return fmt.Errorf("%w: apply diff is invalid", ErrInvalid)
	}
	if len(core.Protected) > MaxAuthorities || len(core.HandoffSet) > MaxHandoffEntries || len(core.Protected) != len(core.HandoffSet) {
		return fmt.Errorf("%w: apply protected/handoff cardinality mismatch", ErrInvalid)
	}
	if !slices.IsSortedFunc(core.Protected, func(a, b AuthorityRecord) int { return cmp.Compare(a.Identity, b.Identity) }) ||
		!slices.IsSortedFunc(core.HandoffSet, func(a, b Handoff) int { return cmp.Compare(a.Source, b.Source) }) {
		return fmt.Errorf("%w: apply closure/handoff order is noncanonical", ErrInvalid)
	}
	protectedByID := make(map[OwnerIdentity]AuthorityRecord, len(core.Protected))
	parents := make(map[OwnerIdentity][]OwnerIdentity, len(core.Protected))
	links := 0
	for i, authority := range core.Protected {
		want, identityErr := authorityIdentity(runID, authority)
		if identityErr != nil || want != authority.Identity || !canonicalDigest(string(authority.EpochID)) ||
			!validIdentifier(authority.LocalID) || !validIdentifier(authority.ReservationID) || !validIdentifier(authority.NodeID) ||
			!authorityKindValid(authority.Kind) || !authorityStateForKind(authority.Kind, authority.State) ||
			!slices.IsSorted(authority.DependsOn) || len(slices.Compact(slices.Clone(authority.DependsOn))) != len(authority.DependsOn) {
			return fmt.Errorf("%w: protected authority %d is invalid", ErrInvalid, i)
		}
		if authority.State == AuthorityHandedOff && (authority.Successor == "" || !canonicalDigest(authority.TerminalRecordID)) ||
			authority.State.terminal() && authority.State != AuthorityHandedOff && (authority.Successor != "" || !canonicalDigest(authority.TerminalRecordID)) ||
			authority.State.active() && (authority.Successor != "" || authority.TerminalRecordID != "") {
			return fmt.Errorf("%w: protected authority %d terminal shape is invalid", ErrInvalid, i)
		}
		links += len(authority.DependsOn)
		if links > MaxAuthorityLinks {
			return &OverBudgetError{Limit: "protected_authority_links", Value: links, Maximum: MaxAuthorityLinks}
		}
		protectedByID[authority.Identity] = authority
		parents[authority.Identity] = authority.DependsOn
	}
	for _, authority := range core.Protected {
		for _, dependency := range authority.DependsOn {
			if _, ok := protectedByID[dependency]; !ok || dependency == authority.Identity {
				return fmt.Errorf("%w: protected authority closure is incomplete", ErrInvalid)
			}
		}
		if authority.State == AuthorityHandedOff {
			if _, ok := protectedByID[authority.Successor]; !ok {
				return fmt.Errorf("%w: protected handed-off successor is absent", ErrInvalid)
			}
		}
	}
	if authorityDependencyCycle(parents) {
		return fmt.Errorf("%w: protected authority closure contains a cycle", ErrInvalid)
	}
	wantProtected, err := protectedDigest(core.Protected)
	if err != nil || wantProtected != core.ProtectedDigest {
		return fmt.Errorf("%w: protected closure digest mismatch", ErrInvalid)
	}
	basis, err := applyHandoffBasis(core)
	if err != nil {
		return err
	}
	targets := map[OwnerIdentity]struct{}{}
	reservations := map[string]struct{}{}
	frontiers := map[frontierMaterializationKey]struct{}{}
	dependencies, err := newAuthorityDependencyIndex(core.Protected)
	if err != nil {
		return err
	}
	candidateNodes := epochNodeSet(core.CandidateEpoch)
	for i, handoff := range core.HandoffSet {
		if i > 0 && core.HandoffSet[i-1].Source == handoff.Source {
			return fmt.Errorf("%w: handoff source is duplicated", ErrInvalid)
		}
		source, ok := protectedByID[handoff.Source]
		if !ok {
			return fmt.Errorf("%w: handoff set is not complete", ErrInvalid)
		}
		if core.Protected[i].Identity != handoff.Source {
			return fmt.Errorf("%w: handoff set differs from protected closure", ErrInvalid)
		}
		wantID, idErr := handoffIdentity(handoff.Source, handoff.Action, handoff.Target, basis)
		if idErr != nil || wantID != handoff.ID {
			return fmt.Errorf("%w: handoff identity mismatch", ErrInvalid)
		}
		switch handoff.Action {
		case HandoffRetain:
			if handoff.Target != nil {
				return fmt.Errorf("%w: retained handoff has target", ErrInvalid)
			}
		case HandoffTransfer:
			if handoff.Target == nil || source.State != AuthorityVerifiedUnclaimed || source.Kind != AuthorityFrontier ||
				dependencies.hasActiveDependent(source.Identity) {
				return fmt.Errorf("%w: transfer bypasses protected authority", ErrInvalid)
			}
			target := *handoff.Target
			if target.EpochID != core.CandidateEpoch.ID || target.Kind != AuthorityFrontier || target.State != AuthorityVerifiedUnclaimed ||
				!reflect.DeepEqual(target.DependsOn, []OwnerIdentity{source.Identity}) || target.Successor != "" || target.TerminalRecordID != "" {
				return fmt.Errorf("%w: transfer successor shape is invalid", ErrInvalid)
			}
			if !validIdentifier(target.LocalID) || !validIdentifier(target.ReservationID) || !validIdentifier(target.NodeID) {
				return fmt.Errorf("%w: transfer successor fields are invalid", ErrInvalid)
			}
			if _, ok := candidateNodes[target.NodeID]; !ok {
				return fmt.Errorf("%w: transfer successor node is absent from candidate", ErrInvalid)
			}
			wantTarget, targetErr := authorityIdentity(runID, target)
			if targetErr != nil || wantTarget != target.Identity {
				return fmt.Errorf("%w: transfer successor identity mismatch", ErrInvalid)
			}
			if _, exists := targets[target.Identity]; exists {
				return fmt.Errorf("%w: transfer successor is duplicated", ErrInvalid)
			}
			if _, exists := reservations[target.ReservationID]; exists {
				return fmt.Errorf("%w: transfer successor reservation is reused", ErrInvalid)
			}
			if dependencies.reservationUsed(target.ReservationID) {
				return fmt.Errorf("%w: transfer successor resurrects a historical reservation", ErrInvalid)
			}
			if !dependencies.logicalFrontierAvailable(source.Identity, target.LocalID, target.NodeID) {
				return fmt.Errorf("%w: transfer successor re-enters a historical logical frontier", ErrInvalid)
			}
			frontierKey := frontierMaterializationKey{target.LocalID, target.NodeID}
			if _, exists := frontiers[frontierKey]; exists {
				return fmt.Errorf("%w: transfer successors duplicate a logical frontier", ErrInvalid)
			}
			targets[target.Identity] = struct{}{}
			reservations[target.ReservationID] = struct{}{}
			frontiers[frontierKey] = struct{}{}
		default:
			return fmt.Errorf("%w: handoff action is invalid", ErrInvalid)
		}
	}
	wantSet, err := handoffSetDigest(core.HandoffSet)
	if err != nil || wantSet != core.HandoffSetDigest {
		return fmt.Errorf("%w: handoff set digest mismatch", ErrInvalid)
	}
	wantProposal, err := proposalDigest(core)
	if err != nil || wantProposal != core.ProposalDigest {
		return fmt.Errorf("%w: proposal digest mismatch", ErrInvalid)
	}
	return nil
}

func applyHandoffBasis(core applyCore) (string, error) {
	return digestValue("handoff-proposal-basis/v1", struct {
		Base            Binding `json:"base"`
		Candidate       EpochID `json:"candidate"`
		ReasonDigest    string  `json:"reasonDigest,omitempty"`
		DiffDigest      string  `json:"diffDigest"`
		ProtectedDigest string  `json:"protectedDigest"`
	}{core.BaseBinding, core.CandidateEpoch.ID, core.ReasonDigest, core.Diff.Digest, core.ProtectedDigest})
}

func canonicalDiff(diff Diff) bool {
	if len(diff.AddedNodes) > model.MaxNormalizedNodes || len(diff.RemovedNodes) > model.MaxNormalizedNodes ||
		len(diff.ChangedNodes) > model.MaxNormalizedNodes || len(diff.AddedEdges) > model.MaxNormalizedEdges ||
		len(diff.RemovedEdges) > model.MaxNormalizedEdges {
		return false
	}
	if !canonicalTemplateRef(diff.BeforeTemplateRef) || !canonicalTemplateRef(diff.AfterTemplateRef) ||
		!canonicalDigest(diff.BeforeSourceDigest) || !canonicalDigest(diff.AfterSourceDigest) ||
		!slices.IsSorted(diff.AddedNodes) || !slices.IsSorted(diff.RemovedNodes) || !slices.IsSorted(diff.ChangedNodes) ||
		len(slices.Compact(slices.Clone(diff.AddedNodes))) != len(diff.AddedNodes) ||
		len(slices.Compact(slices.Clone(diff.RemovedNodes))) != len(diff.RemovedNodes) ||
		len(slices.Compact(slices.Clone(diff.ChangedNodes))) != len(diff.ChangedNodes) ||
		!slices.IsSortedFunc(diff.AddedEdges, compareGraphEdge) || !slices.IsSortedFunc(diff.RemovedEdges, compareGraphEdge) {
		return false
	}
	nodeMembership := map[string]uint8{}
	for _, group := range []struct {
		values []string
		bit    uint8
	}{{diff.AddedNodes, 1}, {diff.RemovedNodes, 2}, {diff.ChangedNodes, 4}} {
		for _, node := range group.values {
			if !validIdentifier(node) || nodeMembership[node] != 0 {
				return false
			}
			nodeMembership[node] = group.bit
		}
	}
	edgeMembership := make(map[GraphEdge]struct{}, len(diff.AddedEdges)+len(diff.RemovedEdges))
	for _, edge := range append(slices.Clone(diff.AddedEdges), diff.RemovedEdges...) {
		if edge.From != "" && !validIdentifier(edge.From) || len(edge.Outcome) > MaxIdentifierBytes || !validIdentifier(edge.To) {
			return false
		}
		if _, duplicate := edgeMembership[edge]; duplicate {
			return false
		}
		edgeMembership[edge] = struct{}{}
	}
	return true
}

func authorityKindValid(kind AuthorityKind) bool {
	switch kind {
	case AuthorityFrontier, AuthorityOutcome, AuthorityParallel, AuthorityJoin, AuthorityPropagation,
		AuthorityDetachment, AuthorityRetry, AuthorityRollbackForward, AuthorityCommand, AuthorityWait,
		AuthorityTimer, AuthorityObligation, AuthorityContact, AuthorityDispatchedSideEffect:
		return true
	default:
		return false
	}
}

func authorityStateForKind(kind AuthorityKind, state AuthorityState) bool {
	if kind == AuthorityFrontier {
		return state == AuthorityVerifiedUnclaimed || state == AuthorityClaimed || state.terminal()
	}
	return state == AuthorityActive || state == AuthorityCompleted || state == AuthorityFailed || state == AuthorityCanceled
}

func capabilitySubset(required, allowed []Capability) bool {
	for _, capability := range required {
		if !slices.Contains(allowed, capability) {
			return false
		}
	}
	return true
}

func graphHasCycle(nodes map[string]GraphNode, adjacency map[string][]string) bool {
	const (
		visiting = 1
		done     = 2
	)
	state := make(map[string]int, len(nodes))
	var visit func(string) bool
	visit = func(node string) bool {
		if state[node] == visiting {
			return true
		}
		if state[node] == done {
			return false
		}
		state[node] = visiting
		for _, target := range adjacency[node] {
			if visit(target) {
				return true
			}
		}
		state[node] = done
		return false
	}
	for node := range nodes {
		if visit(node) {
			return true
		}
	}
	return false
}

func authorityDependencyCycle(parents map[OwnerIdentity][]OwnerIdentity) bool {
	state := make(map[OwnerIdentity]uint8, len(parents))
	var visit func(OwnerIdentity) bool
	visit = func(id OwnerIdentity) bool {
		if state[id] == 1 {
			return true
		}
		if state[id] == 2 {
			return false
		}
		state[id] = 1
		for _, parent := range parents[id] {
			if visit(parent) {
				return true
			}
		}
		state[id] = 2
		return false
	}
	for id := range parents {
		if visit(id) {
			return true
		}
	}
	return false
}

func validateComposedGraph(epochs []TemplateEpoch, authorities []AuthorityRecord) error {
	type vertex struct {
		epoch EpochID
		node  string
	}
	nodes := make(map[vertex]struct{})
	adjacency := make(map[vertex][]vertex)
	ordinal := make(map[EpochID]uint64, len(epochs))
	for _, epoch := range epochs {
		ordinal[epoch.ID] = epoch.Ordinal
		for _, node := range epoch.Graph.Nodes {
			nodes[vertex{epoch.ID, node.ID}] = struct{}{}
		}
		for _, edge := range epoch.Graph.Edges {
			if edge.From == "" {
				continue
			}
			from, to := vertex{epoch.ID, edge.From}, vertex{epoch.ID, edge.To}
			adjacency[from] = append(adjacency[from], to)
		}
	}
	byID := make(map[OwnerIdentity]AuthorityRecord, len(authorities))
	for _, authority := range authorities {
		byID[authority.Identity] = authority
	}
	for _, authority := range authorities {
		if authority.State != AuthorityHandedOff {
			continue
		}
		target, ok := byID[authority.Successor]
		if !ok || ordinal[target.EpochID] <= ordinal[authority.EpochID] {
			return fmt.Errorf("%w: cross-epoch handoff re-enters an old epoch", ErrInvalid)
		}
		from, to := vertex{authority.EpochID, authority.NodeID}, vertex{target.EpochID, target.NodeID}
		if _, ok := nodes[from]; !ok {
			return fmt.Errorf("%w: composed graph handoff source is absent", ErrInvalid)
		}
		if _, ok := nodes[to]; !ok {
			return fmt.Errorf("%w: composed graph handoff target is absent", ErrInvalid)
		}
		adjacency[from] = append(adjacency[from], to)
	}
	graphNodes := make(map[string]GraphNode, len(nodes))
	graphAdj := make(map[string][]string, len(adjacency))
	key := func(value vertex) string { return string(value.epoch) + "\x00" + value.node }
	for node := range nodes {
		graphNodes[key(node)] = GraphNode{}
	}
	for from, targets := range adjacency {
		for _, to := range targets {
			graphAdj[key(from)] = append(graphAdj[key(from)], key(to))
		}
	}
	if graphHasCycle(graphNodes, graphAdj) {
		return fmt.Errorf("%w: composed multi-epoch graph contains a cycle", ErrInvalid)
	}
	return nil
}
