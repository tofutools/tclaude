package epochv8

import (
	"cmp"
	"fmt"
	"reflect"
	"slices"
)

// Initialize creates epoch zero without retaining template source bytes.
func Initialize(runID string, candidate *TemplateCandidate, seeds []AuthoritySeed) (*CheckpointV8, error) {
	if !validIdentifier(runID) || candidate == nil {
		return nil, fmt.Errorf("%w: run and supported candidate are required", ErrInvalid)
	}
	if len(seeds) > MaxAuthorities {
		return nil, &OverBudgetError{Limit: "authorities", Value: len(seeds), Maximum: MaxAuthorities}
	}
	epoch := cloneEpoch(candidate.epoch)
	epoch.Ordinal = 0
	epoch.PredecessorEpochID = ""
	if err := validateEpochPrototype(epoch); err != nil {
		return nil, err
	}
	var err error
	epoch.ID, err = epochIdentity(runID, epoch)
	if err != nil {
		return nil, err
	}
	nodeSet := epochNodeSet(epoch)
	authorities := make([]AuthorityRecord, len(seeds))
	byLocalID := make(map[string]OwnerIdentity, len(seeds))
	for i, seed := range seeds {
		if !validIdentifier(seed.LocalID) || !validIdentifier(seed.ReservationID) || !validIdentifier(seed.NodeID) {
			return nil, fmt.Errorf("%w: initial authority %d identity fields are invalid", ErrInvalid, i)
		}
		if _, ok := nodeSet[seed.NodeID]; !ok {
			return nil, fmt.Errorf("%w: initial authority %q names absent node %q", ErrInvalid, seed.LocalID, seed.NodeID)
		}
		if _, exists := byLocalID[seed.LocalID]; exists {
			return nil, fmt.Errorf("%w: duplicate initial local authority %q", ErrInvalid, seed.LocalID)
		}
		authority := AuthorityRecord{
			EpochID: epoch.ID, LocalID: seed.LocalID, ReservationID: seed.ReservationID,
			NodeID: seed.NodeID, Kind: seed.Kind, State: seed.State, DependsOn: []OwnerIdentity{},
		}
		if !authorityKindValid(authority.Kind) || !authorityStateForKind(authority.Kind, authority.State) || authority.State.terminal() {
			return nil, fmt.Errorf("%w: initial authority %q kind/state is invalid", ErrInvalid, seed.LocalID)
		}
		authority.Identity, err = authorityIdentity(runID, authority)
		if err != nil {
			return nil, err
		}
		authorities[i] = authority
		byLocalID[seed.LocalID] = authority.Identity
	}
	for i, seed := range seeds {
		dependencies := make([]OwnerIdentity, 0, len(seed.DependencyLocalIDs))
		if len(seed.DependencyLocalIDs) > MaxAuthorities {
			return nil, &OverBudgetError{Limit: "authority_dependencies", Value: len(seed.DependencyLocalIDs), Maximum: MaxAuthorities}
		}
		for _, localID := range seed.DependencyLocalIDs {
			dependency, ok := byLocalID[localID]
			if !ok || dependency == authorities[i].Identity {
				return nil, fmt.Errorf("%w: initial authority %q dependency %q is invalid", ErrInvalid, seed.LocalID, localID)
			}
			dependencies = append(dependencies, dependency)
		}
		slices.Sort(dependencies)
		if len(slices.Compact(slices.Clone(dependencies))) != len(dependencies) {
			return nil, fmt.Errorf("%w: initial authority %q dependencies are duplicated", ErrInvalid, seed.LocalID)
		}
		authorities[i].DependsOn = dependencies
	}
	sortAuthorities(authorities)
	anchor := InitializationAnchor{
		RunID: runID, Capabilities: productionCapabilities(), OriginalEpoch: cloneEpoch(epoch),
		InitialAuthorities: cloneAuthorities(authorities),
	}
	anchor.Digest, err = anchorDigest(anchor)
	if err != nil {
		return nil, err
	}
	wire := checkpointWire{
		StateSchemaVersion: StateSchemaVersion, Protocol: Protocol, Encoding: Encoding,
		Anchor: anchor, CurrentEpochID: epoch.ID, Epochs: []TemplateEpoch{cloneEpoch(epoch)},
		History: []HistoryEvent{}, Authorities: authorities,
	}
	wire.Digest, err = checkpointDigest(wire)
	if err != nil {
		return nil, err
	}
	checkpoint := &CheckpointV8{wire: wire}
	if err := VerifyCheckpointV8(checkpoint); err != nil {
		return nil, err
	}
	return checkpoint, nil
}

func (checkpoint *CheckpointV8) Binding() Binding {
	if checkpoint == nil {
		return Binding{}
	}
	return Binding{Revision: uint64(len(checkpoint.wire.History)), Digest: checkpoint.wire.Digest}
}

func (checkpoint *CheckpointV8) View() CheckpointView {
	if checkpoint == nil {
		return CheckpointView{}
	}
	wire := cloneWire(checkpoint.wire)
	protected, _ := protectedClosure(wire.Authorities)
	return CheckpointView{
		Binding: checkpoint.Binding(), RunID: wire.Anchor.RunID,
		OriginalEpoch: wire.Anchor.OriginalEpoch.ID, CurrentEpoch: wire.CurrentEpochID,
		Epochs: wire.Epochs, Authorities: wire.Authorities, ProtectedAuthorities: protected, History: wire.History,
	}
}

func (plan *ApplyPlan) BaseBinding() Binding {
	if plan == nil {
		return Binding{}
	}
	return plan.core.BaseBinding
}

func (plan *ApplyPlan) ProposalDigest() string {
	if plan == nil {
		return ""
	}
	return plan.core.ProposalDigest
}

func (plan *ApplyPlan) CandidateEpoch() TemplateEpoch {
	if plan == nil {
		return TemplateEpoch{}
	}
	return cloneEpoch(plan.core.CandidateEpoch)
}

// PreviewApply computes a complete canonical proposal. Active-authority
// conflicts are stable blockers, not partially applicable plans.
func PreviewApply(checkpoint *CheckpointV8, draft ApplyDraft) (PreviewResult, error) {
	if err := VerifyCheckpointV8(checkpoint); err != nil {
		return PreviewResult{}, err
	}
	if draft.Candidate == nil {
		return PreviewResult{}, fmt.Errorf("%w: supported candidate is required", ErrInvalid)
	}
	currentBinding := checkpoint.Binding()
	if draft.BaseBinding != currentBinding {
		return PreviewResult{Blockers: []Blocker{{Code: BlockerStaleBinding}}}, nil
	}
	if draft.ReasonDigest != "" && !canonicalDigest(draft.ReasonDigest) {
		return PreviewResult{}, fmt.Errorf("%w: reason must be a canonical digest", ErrInvalid)
	}
	epoch := cloneEpoch(draft.Candidate.epoch)
	epoch.Ordinal = uint64(len(checkpoint.wire.Epochs))
	epoch.PredecessorEpochID = checkpoint.wire.CurrentEpochID
	if epoch.Ordinal >= MaxEpochs {
		return PreviewResult{}, &OverBudgetError{Limit: "epochs", Value: int(epoch.Ordinal + 1), Maximum: MaxEpochs}
	}
	if err := validateEpochPrototype(epoch); err != nil {
		return PreviewResult{}, err
	}
	var err error
	epoch.ID, err = epochIdentity(checkpoint.wire.Anchor.RunID, epoch)
	if err != nil {
		return PreviewResult{}, err
	}
	currentEpoch := checkpoint.wire.Epochs[len(checkpoint.wire.Epochs)-1]
	diff, err := computeDiff(currentEpoch, epoch)
	if err != nil {
		return PreviewResult{}, err
	}
	protected, err := protectedClosure(checkpoint.wire.Authorities)
	if err != nil {
		return PreviewResult{}, err
	}
	protectedHash, err := protectedDigest(protected)
	if err != nil {
		return PreviewResult{}, err
	}
	basisCore := applyCore{
		RunID: checkpoint.wire.Anchor.RunID, BaseBinding: currentBinding, CandidateEpoch: epoch, ReasonDigest: draft.ReasonDigest,
		Diff: diff, ProtectedDigest: protectedHash,
	}
	basis, err := applyHandoffBasis(basisCore)
	if err != nil {
		return PreviewResult{}, err
	}
	dependencies, err := newAuthorityDependencyIndex(checkpoint.wire.Authorities)
	if err != nil {
		return PreviewResult{}, err
	}
	handoffs, blockers, err := planHandoffs(checkpoint.wire.Anchor.RunID, dependencies, protected, epoch, basis, draft.Handoffs)
	if err != nil {
		return PreviewResult{}, err
	}
	if len(blockers) != 0 {
		return PreviewResult{Diff: diff, Blockers: blockers}, nil
	}
	handoffHash, err := handoffSetDigest(handoffs)
	if err != nil {
		return PreviewResult{}, err
	}
	core := applyCore{
		RunID: checkpoint.wire.Anchor.RunID, BaseBinding: currentBinding, PredecessorEpoch: checkpoint.wire.CurrentEpochID,
		CandidateEpoch: epoch, ReasonDigest: draft.ReasonDigest, Diff: diff,
		Protected: protected, ProtectedDigest: protectedHash,
		HandoffSet: handoffs, HandoffSetDigest: handoffHash,
	}
	core.ProposalDigest, err = proposalDigest(core)
	if err != nil {
		return PreviewResult{}, err
	}
	plan := &ApplyPlan{core: cloneApplyCore(core)}
	if err := validateApplyCoreStatic(checkpoint.wire.Anchor.RunID, plan.core); err != nil {
		return PreviewResult{}, err
	}
	return PreviewResult{Plan: plan, Diff: diff, Blockers: []Blocker{}}, nil
}

// Apply validates plan integrity before making the replay/stale/CAS decision.
func Apply(checkpoint *CheckpointV8, plan *ApplyPlan) (TransitionResult, error) {
	if err := VerifyCheckpointV8(checkpoint); err != nil {
		return TransitionResult{}, err
	}
	if plan == nil {
		return TransitionResult{}, fmt.Errorf("%w: apply plan is required", ErrInvalid)
	}
	core := cloneApplyCore(plan.core)
	if err := validateApplyCoreStatic(checkpoint.wire.Anchor.RunID, core); err != nil {
		return TransitionResult{}, err
	}
	for _, event := range checkpoint.wire.History {
		if event.Apply != nil && reflect.DeepEqual(event.Apply.applyCore, core) {
			return TransitionResult{Checkpoint: checkpoint, Disposition: DispositionReplayed, Binding: checkpoint.Binding()}, nil
		}
	}
	if core.BaseBinding != checkpoint.Binding() {
		return TransitionResult{Checkpoint: checkpoint, Disposition: DispositionStale, Binding: checkpoint.Binding()}, nil
	}
	if len(checkpoint.wire.History) >= MaxHistoryEvents {
		return TransitionResult{}, &OverBudgetError{Limit: "history_events", Value: len(checkpoint.wire.History) + 1, Maximum: MaxHistoryEvents}
	}
	protected, err := protectedClosure(checkpoint.wire.Authorities)
	if err != nil {
		return TransitionResult{}, err
	}
	if !reflect.DeepEqual(protected, core.Protected) || core.PredecessorEpoch != checkpoint.wire.CurrentEpochID ||
		core.CandidateEpoch.Ordinal != uint64(len(checkpoint.wire.Epochs)) {
		return TransitionResult{}, fmt.Errorf("%w: apply plan no longer matches protected authority", ErrInvalid)
	}
	dependencies, err := newAuthorityDependencyIndex(checkpoint.wire.Authorities)
	if err != nil {
		return TransitionResult{}, err
	}
	authorities, err := applyHandoffSet(checkpoint.wire.Anchor.RunID, checkpoint.wire.Authorities, core.HandoffSet, dependencies)
	if err != nil {
		return TransitionResult{}, err
	}
	record := &ApplyRecord{applyCore: core}
	record.RecordDigest, err = applyRecordDigest(*record)
	if err != nil {
		return TransitionResult{}, err
	}
	event := HistoryEvent{
		Revision: uint64(len(checkpoint.wire.History) + 1), Kind: HistoryApply, Apply: record,
	}
	event.Digest, err = historyEventDigest(event)
	if err != nil {
		return TransitionResult{}, err
	}
	wire := cloneWire(checkpoint.wire)
	wire.Epochs = append(wire.Epochs, cloneEpoch(core.CandidateEpoch))
	wire.CurrentEpochID = core.CandidateEpoch.ID
	wire.Authorities = authorities
	wire.History = append(wire.History, event)
	wire.Digest, err = checkpointDigest(wire)
	if err != nil {
		return TransitionResult{}, err
	}
	next := &CheckpointV8{wire: wire}
	if err := VerifyCheckpointV8(next); err != nil {
		return TransitionResult{}, err
	}
	return TransitionResult{Checkpoint: next, Disposition: DispositionApplied, Binding: next.Binding()}, nil
}

// FinishClaimed settles claimed work on its immutable owner epoch. It neither
// relabels the work to CurrentEpochID nor accepts actor/time provenance.
func FinishClaimed(checkpoint *CheckpointV8, claim FinishClaim) (TransitionResult, error) {
	if err := VerifyCheckpointV8(checkpoint); err != nil {
		return TransitionResult{}, err
	}
	if !claim.Result.valid() || !canonicalDigest(claim.EvidenceDigest) || !canonicalDigest(string(claim.Identity)) {
		return TransitionResult{}, fmt.Errorf("%w: finish claim is invalid", ErrInvalid)
	}
	authority, ok := authorityByID(checkpoint.wire.Authorities, claim.Identity)
	ownerEpoch := EpochID("")
	if ok {
		ownerEpoch = authority.EpochID
	}
	receipt := FinishReceipt{
		BaseBinding: claim.BaseBinding, Identity: claim.Identity, OwnerEpochID: ownerEpoch,
		Result: claim.Result, EvidenceDigest: claim.EvidenceDigest,
	}
	var err error
	receipt.ID, err = finishIdentity(receipt)
	if err != nil {
		return TransitionResult{}, err
	}
	for _, event := range checkpoint.wire.History {
		if event.Finish != nil && reflect.DeepEqual(*event.Finish, receipt) {
			return TransitionResult{Checkpoint: checkpoint, Disposition: DispositionReplayed, Binding: checkpoint.Binding()}, nil
		}
	}
	if claim.BaseBinding != checkpoint.Binding() {
		return TransitionResult{Checkpoint: checkpoint, Disposition: DispositionStale, Binding: checkpoint.Binding()}, nil
	}
	if !ok {
		return TransitionResult{}, fmt.Errorf("%w: finish owner identity is absent", ErrInvalid)
	}
	if authority.State != AuthorityClaimed || authority.Kind != AuthorityFrontier {
		if authority.State.terminal() {
			return TransitionResult{}, ErrTerminalIdentity
		}
		return TransitionResult{}, fmt.Errorf("%w: owner identity is not claimed work", ErrInvalid)
	}
	dependencies, err := newAuthorityDependencyIndex(checkpoint.wire.Authorities)
	if err != nil {
		return TransitionResult{}, err
	}
	if dependencies.hasActiveDependent(authority.Identity) {
		return TransitionResult{}, ErrProtectedAuthority
	}
	if len(checkpoint.wire.History) >= MaxHistoryEvents {
		return TransitionResult{}, &OverBudgetError{Limit: "history_events", Value: len(checkpoint.wire.History) + 1, Maximum: MaxHistoryEvents}
	}
	authorities := cloneAuthorities(checkpoint.wire.Authorities)
	for i := range authorities {
		if authorities[i].Identity != claim.Identity {
			continue
		}
		authorities[i].State = claim.Result.authorityState()
		authorities[i].TerminalRecordID = receipt.ID
	}
	event := HistoryEvent{
		Revision: uint64(len(checkpoint.wire.History) + 1), Kind: HistoryFinishClaimed, Finish: &receipt,
	}
	event.Digest, err = historyEventDigest(event)
	if err != nil {
		return TransitionResult{}, err
	}
	wire := cloneWire(checkpoint.wire)
	wire.Authorities = authorities
	wire.History = append(wire.History, event)
	wire.Digest, err = checkpointDigest(wire)
	if err != nil {
		return TransitionResult{}, err
	}
	next := &CheckpointV8{wire: wire}
	if err := VerifyCheckpointV8(next); err != nil {
		return TransitionResult{}, err
	}
	return TransitionResult{Checkpoint: next, Disposition: DispositionApplied, Binding: next.Binding()}, nil
}

func computeDiff(before, after TemplateEpoch) (Diff, error) {
	diff := Diff{
		BeforeTemplateRef: before.TemplateRef, AfterTemplateRef: after.TemplateRef,
		BeforeSourceDigest: before.TemplateSourceDigest, AfterSourceDigest: after.TemplateSourceDigest,
		AddedNodes: []string{}, RemovedNodes: []string{}, ChangedNodes: []string{},
		AddedEdges: []GraphEdge{}, RemovedEdges: []GraphEdge{},
	}
	beforeNodes := make(map[string]GraphNode, len(before.Graph.Nodes))
	for _, node := range before.Graph.Nodes {
		beforeNodes[node.ID] = node
	}
	afterNodes := make(map[string]GraphNode, len(after.Graph.Nodes))
	for _, node := range after.Graph.Nodes {
		afterNodes[node.ID] = node
		old, exists := beforeNodes[node.ID]
		if !exists {
			diff.AddedNodes = append(diff.AddedNodes, node.ID)
		} else if !reflect.DeepEqual(old, node) {
			diff.ChangedNodes = append(diff.ChangedNodes, node.ID)
		}
	}
	for _, node := range before.Graph.Nodes {
		if _, exists := afterNodes[node.ID]; !exists {
			diff.RemovedNodes = append(diff.RemovedNodes, node.ID)
		}
	}
	beforeEdges := make(map[GraphEdge]struct{}, len(before.Graph.Edges))
	for _, edge := range before.Graph.Edges {
		beforeEdges[edge] = struct{}{}
	}
	afterEdges := make(map[GraphEdge]struct{}, len(after.Graph.Edges))
	for _, edge := range after.Graph.Edges {
		afterEdges[edge] = struct{}{}
		if _, exists := beforeEdges[edge]; !exists {
			diff.AddedEdges = append(diff.AddedEdges, edge)
		}
	}
	for _, edge := range before.Graph.Edges {
		if _, exists := afterEdges[edge]; !exists {
			diff.RemovedEdges = append(diff.RemovedEdges, edge)
		}
	}
	slices.Sort(diff.AddedNodes)
	slices.Sort(diff.RemovedNodes)
	slices.Sort(diff.ChangedNodes)
	slices.SortFunc(diff.AddedEdges, compareGraphEdge)
	slices.SortFunc(diff.RemovedEdges, compareGraphEdge)
	var err error
	diff.Digest, err = diffDigest(diff)
	return diff, err
}

func planHandoffs(runID string, dependencies *authorityDependencyIndex, protected []AuthorityRecord, epoch TemplateEpoch, basis string, directives []HandoffDirective) ([]Handoff, []Blocker, error) {
	if len(directives) > MaxHandoffEntries {
		return nil, nil, &OverBudgetError{Limit: "handoff_entries", Value: len(directives), Maximum: MaxHandoffEntries}
	}
	protectedByID := make(map[OwnerIdentity]AuthorityRecord, len(protected))
	for _, authority := range protected {
		protectedByID[authority.Identity] = authority
	}
	directiveByID := make(map[OwnerIdentity]HandoffDirective, len(directives))
	blockers := make([]Blocker, 0)
	for _, directive := range directives {
		if !canonicalDigest(string(directive.Source)) {
			return nil, nil, fmt.Errorf("%w: handoff source identity is not canonical", ErrInvalid)
		}
		if _, duplicate := directiveByID[directive.Source]; duplicate {
			blockers = append(blockers, Blocker{Code: BlockerHandoffDuplicate, AuthorityID: directive.Source})
			continue
		}
		directiveByID[directive.Source] = directive
		if _, ok := protectedByID[directive.Source]; !ok {
			blockers = append(blockers, Blocker{Code: BlockerHandoffUnknown, AuthorityID: directive.Source})
		}
	}
	nodeSet := epochNodeSet(epoch)
	targetIDs := make(map[OwnerIdentity]struct{})
	targetReservations := make(map[string]struct{})
	targetFrontiers := make(map[frontierMaterializationKey]OwnerIdentity)
	transferSources := make(map[OwnerIdentity]struct{})
	handoffs := make([]Handoff, 0, len(protected))
	for _, authority := range protected {
		directive, ok := directiveByID[authority.Identity]
		if !ok {
			blockers = append(blockers, Blocker{Code: BlockerHandoffMissing, AuthorityID: authority.Identity})
			continue
		}
		switch directive.Action {
		case HandoffRetain:
			if directive.TargetLocalID != "" || directive.TargetReservationID != "" || directive.TargetNodeID != "" {
				return nil, nil, fmt.Errorf("%w: retained authority %q has a target", ErrInvalid, authority.Identity)
			}
			handoff := Handoff{Source: authority.Identity, Action: HandoffRetain}
			var err error
			handoff.ID, err = handoffIdentity(handoff.Source, handoff.Action, nil, basis)
			if err != nil {
				return nil, nil, err
			}
			handoffs = append(handoffs, handoff)
		case HandoffTransfer:
			if blocker, blocked := transferSourceBlocker(authority); blocked {
				blockers = append(blockers, blocker)
			}
			transferSources[authority.Identity] = struct{}{}
			if !validIdentifier(directive.TargetLocalID) || !validIdentifier(directive.TargetReservationID) || !validIdentifier(directive.TargetNodeID) {
				return nil, nil, fmt.Errorf("%w: transfer target fields are invalid", ErrInvalid)
			}
			if _, ok := nodeSet[directive.TargetNodeID]; !ok {
				return nil, nil, fmt.Errorf("%w: transfer target node %q is absent", ErrInvalid, directive.TargetNodeID)
			}
			target := AuthorityRecord{
				EpochID: epoch.ID, LocalID: directive.TargetLocalID, ReservationID: directive.TargetReservationID,
				NodeID: directive.TargetNodeID, Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed,
				DependsOn: []OwnerIdentity{authority.Identity},
			}
			var err error
			target.Identity, err = authorityIdentity(runID, target)
			if err != nil {
				return nil, nil, err
			}
			if _, exists := targetIDs[target.Identity]; exists {
				return nil, nil, fmt.Errorf("%w: duplicate transfer successor", ErrInvalid)
			}
			if _, exists := targetReservations[target.ReservationID]; exists {
				return nil, nil, fmt.Errorf("%w: transfer successor reservation is reused", ErrInvalid)
			}
			if dependencies.reservationUsed(target.ReservationID) {
				return nil, nil, fmt.Errorf("%w: transfer successor resurrects a historical reservation", ErrInvalid)
			}
			if !dependencies.logicalFrontierAvailable(authority.Identity, target.LocalID, target.NodeID) {
				return nil, nil, fmt.Errorf("%w: transfer successor re-enters a historical logical frontier", ErrInvalid)
			}
			frontierKey := frontierMaterializationKey{target.LocalID, target.NodeID}
			if _, exists := targetFrontiers[frontierKey]; exists {
				blockers = append(blockers, Blocker{Code: BlockerHandoffDuplicate, AuthorityID: authority.Identity})
			} else {
				targetFrontiers[frontierKey] = authority.Identity
			}
			if _, exists := dependencies.byID[target.Identity]; exists {
				return nil, nil, fmt.Errorf("%w: transfer successor resurrects an existing identity", ErrInvalid)
			}
			targetIDs[target.Identity] = struct{}{}
			targetReservations[target.ReservationID] = struct{}{}
			handoff := Handoff{Source: authority.Identity, Action: HandoffTransfer, Target: &target}
			handoff.ID, err = handoffIdentity(handoff.Source, handoff.Action, handoff.Target, basis)
			if err != nil {
				return nil, nil, err
			}
			handoffs = append(handoffs, handoff)
		default:
			return nil, nil, fmt.Errorf("%w: handoff action %q is invalid", ErrInvalid, directive.Action)
		}
	}
	blockers = canonicalBlockers(blockers)
	if len(blockers) > MaxBlockers {
		blockers = blockers[:MaxBlockers]
	}
	blockers = append(blockers, dependencies.activeDependentBlockers(transferSources, MaxBlockers-len(blockers))...)
	slices.SortFunc(handoffs, func(a, b Handoff) int { return cmp.Compare(a.Source, b.Source) })
	blockers = canonicalBlockers(blockers)
	if len(blockers) > MaxBlockers {
		blockers = blockers[:MaxBlockers]
	}
	return handoffs, blockers, nil
}

func transferSourceBlocker(source AuthorityRecord) (Blocker, bool) {
	if source.State != AuthorityVerifiedUnclaimed || source.Kind != AuthorityFrontier {
		code := BlockerNotTransferable
		switch source.State {
		case AuthorityClaimed:
			code = BlockerClaimed
		case AuthorityActive:
			code = blockerForKind(source.Kind)
		}
		return Blocker{Code: code, AuthorityID: source.Identity}, true
	}
	return Blocker{}, false
}

func blockerForKind(kind AuthorityKind) BlockerCode {
	switch kind {
	case AuthorityCommand:
		return BlockerActiveCommand
	case AuthorityWait:
		return BlockerActiveWait
	case AuthorityTimer:
		return BlockerActiveTimer
	case AuthorityObligation:
		return BlockerActiveObligation
	case AuthorityContact:
		return BlockerActiveContact
	case AuthorityDispatchedSideEffect:
		return BlockerDispatchedSideEffect
	case AuthorityOutcome:
		return BlockerActiveOutcome
	case AuthorityParallel:
		return BlockerActiveParallel
	case AuthorityJoin:
		return BlockerActiveJoin
	case AuthorityPropagation:
		return BlockerActivePropagation
	case AuthorityDetachment:
		return BlockerActiveDetachment
	case AuthorityRetry:
		return BlockerActiveRetry
	case AuthorityRollbackForward:
		return BlockerActiveRollbackForward
	default:
		return BlockerActiveAuthority
	}
}

func canonicalBlockers(blockers []Blocker) []Blocker {
	slices.SortFunc(blockers, func(a, b Blocker) int {
		if value := cmp.Compare(a.Code, b.Code); value != 0 {
			return value
		}
		return cmp.Compare(a.AuthorityID, b.AuthorityID)
	})
	return slices.Compact(blockers)
}

func protectedClosure(authorities []AuthorityRecord) ([]AuthorityRecord, error) {
	byID := make(map[OwnerIdentity]AuthorityRecord, len(authorities))
	for _, authority := range authorities {
		byID[authority.Identity] = authority
	}
	included := make(map[OwnerIdentity]struct{})
	stack := make([]OwnerIdentity, 0)
	for _, authority := range authorities {
		if authority.State.active() {
			stack = append(stack, authority.Identity)
		}
	}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, exists := included[id]; exists {
			continue
		}
		authority, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("%w: protected closure dependency %q is absent", ErrInvalid, id)
		}
		included[id] = struct{}{}
		stack = append(stack, authority.DependsOn...)
	}
	result := make([]AuthorityRecord, 0, len(included))
	for id := range included {
		result = append(result, cloneAuthority(byID[id]))
	}
	sortAuthorities(result)
	return result, nil
}

// authorityDependencyIndex is built once for an authority snapshot. It keeps
// transfer and finish classification linear in the snapshot plus the bounded
// descendants actually reported, rather than rebuilding and rescanning the
// whole authority graph for every handoff.
type authorityDependencyIndex struct {
	byID                     map[OwnerIdentity]AuthorityRecord
	reverse                  map[OwnerIdentity][]OwnerIdentity
	activeDependent          map[OwnerIdentity]bool
	materializedReservations map[string]OwnerIdentity
	materializedFrontiers    map[frontierMaterializationKey]map[OwnerIdentity]struct{}
	buildAuthorityVisits     int
	buildDependencyVisits    int
	descendantVisits         int
}

type frontierMaterializationKey struct {
	localID string
	nodeID  string
}

func newAuthorityDependencyIndex(authorities []AuthorityRecord) (*authorityDependencyIndex, error) {
	index := &authorityDependencyIndex{
		byID:                     make(map[OwnerIdentity]AuthorityRecord, len(authorities)),
		reverse:                  make(map[OwnerIdentity][]OwnerIdentity, len(authorities)),
		activeDependent:          make(map[OwnerIdentity]bool, len(authorities)),
		materializedReservations: make(map[string]OwnerIdentity, len(authorities)),
		materializedFrontiers:    make(map[frontierMaterializationKey]map[OwnerIdentity]struct{}, len(authorities)),
	}
	for _, authority := range authorities {
		index.buildAuthorityVisits++
		index.byID[authority.Identity] = authority
		if authority.Kind == AuthorityFrontier {
			if owner, exists := index.materializedReservations[authority.ReservationID]; exists && owner != authority.Identity {
				return nil, fmt.Errorf("%w: materialized frontier reservation is reused", ErrInvalid)
			}
			index.materializedReservations[authority.ReservationID] = authority.Identity
			key := frontierMaterializationKey{authority.LocalID, authority.NodeID}
			if index.materializedFrontiers[key] == nil {
				index.materializedFrontiers[key] = make(map[OwnerIdentity]struct{})
			}
			index.materializedFrontiers[key][authority.Identity] = struct{}{}
		}
	}
	for _, authority := range authorities {
		for _, dependency := range authority.DependsOn {
			index.buildDependencyVisits++
			if _, ok := index.byID[dependency]; !ok {
				return nil, fmt.Errorf("%w: authority dependency is absent", ErrInvalid)
			}
			index.reverse[dependency] = append(index.reverse[dependency], authority.Identity)
		}
	}
	for dependency := range index.reverse {
		slices.Sort(index.reverse[dependency])
	}

	// Propagate the existence of active descendants toward their dependencies.
	// Each reachable authority and dependency link is visited at most once.
	reachable := make(map[OwnerIdentity]struct{}, len(authorities))
	queue := make([]OwnerIdentity, 0, len(authorities))
	for _, authority := range authorities {
		if authority.State.active() {
			reachable[authority.Identity] = struct{}{}
			queue = append(queue, authority.Identity)
		}
	}
	for cursor := 0; cursor < len(queue); cursor++ {
		authority := index.byID[queue[cursor]]
		for _, dependency := range authority.DependsOn {
			index.activeDependent[dependency] = true
			if _, seen := reachable[dependency]; seen {
				continue
			}
			reachable[dependency] = struct{}{}
			queue = append(queue, dependency)
		}
	}
	return index, nil
}

func (index *authorityDependencyIndex) hasActiveDependent(source OwnerIdentity) bool {
	return index != nil && index.activeDependent[source]
}

func (index *authorityDependencyIndex) reservationUsed(reservationID string) bool {
	if index == nil {
		return false
	}
	_, exists := index.materializedReservations[reservationID]
	return exists
}

// logicalFrontierAvailable permits the sole historical-key exception: an
// atomic one-to-one handoff may preserve its source LocalID+NodeID under a
// fresh reservation. Any other return to an older logical frontier is reentry.
func (index *authorityDependencyIndex) logicalFrontierAvailable(source OwnerIdentity, localID, nodeID string) bool {
	if index == nil {
		return true
	}
	owners := index.materializedFrontiers[frontierMaterializationKey{localID, nodeID}]
	if len(owners) == 0 {
		return true
	}
	_, direct := owners[source]
	return direct
}

func (index *authorityDependencyIndex) activeDependentBlockers(sources map[OwnerIdentity]struct{}, limit int) []Blocker {
	if index == nil || limit <= 0 || len(sources) == 0 {
		return nil
	}
	queue := make([]OwnerIdentity, 0, len(sources))
	seen := make(map[OwnerIdentity]struct{}, len(sources))
	for source := range sources {
		queue = append(queue, source)
		seen[source] = struct{}{}
	}
	slices.Sort(queue)
	result := make([]Blocker, 0)
	reported := make(map[OwnerIdentity]struct{})
	for cursor := 0; cursor < len(queue) && len(result) < limit; cursor++ {
		for _, id := range index.reverse[queue[cursor]] {
			authority := index.byID[id]
			_, alreadyReported := reported[id]
			if authority.State.active() && !alreadyReported {
				result = append(result, Blocker{Code: blockerForKind(authority.Kind), AuthorityID: authority.Identity})
				reported[id] = struct{}{}
				if len(result) == limit {
					break
				}
			}
			if _, exists := seen[id]; exists {
				continue
			}
			seen[id] = struct{}{}
			index.descendantVisits++
			queue = append(queue, id)
		}
	}
	return canonicalBlockers(result)
}

func applyHandoffSet(runID string, current []AuthorityRecord, handoffs []Handoff, dependencies *authorityDependencyIndex) ([]AuthorityRecord, error) {
	result := cloneAuthorities(current)
	index := make(map[OwnerIdentity]int, len(result))
	targetFrontiers := make(map[frontierMaterializationKey]struct{})
	for i, authority := range result {
		index[authority.Identity] = i
	}
	for _, handoff := range handoffs {
		i, ok := index[handoff.Source]
		if !ok {
			return nil, fmt.Errorf("%w: handoff source %q is absent", ErrInvalid, handoff.Source)
		}
		if handoff.Action == HandoffRetain {
			continue
		}
		if handoff.Action != HandoffTransfer || handoff.Target == nil || result[i].State != AuthorityVerifiedUnclaimed || result[i].Kind != AuthorityFrontier {
			return nil, fmt.Errorf("%w: handoff transfer is invalid", ErrInvalid)
		}
		if dependencies.hasActiveDependent(handoff.Source) {
			return nil, ErrProtectedAuthority
		}
		target := cloneAuthority(*handoff.Target)
		want, err := authorityIdentity(runID, target)
		if err != nil || want != target.Identity {
			return nil, fmt.Errorf("%w: handoff successor identity is invalid", ErrInvalid)
		}
		if _, exists := index[target.Identity]; exists {
			return nil, fmt.Errorf("%w: handoff successor already exists", ErrInvalid)
		}
		if dependencies.reservationUsed(target.ReservationID) {
			return nil, fmt.Errorf("%w: handoff successor resurrects a historical reservation", ErrInvalid)
		}
		if !dependencies.logicalFrontierAvailable(handoff.Source, target.LocalID, target.NodeID) {
			return nil, fmt.Errorf("%w: handoff successor re-enters a historical logical frontier", ErrInvalid)
		}
		frontierKey := frontierMaterializationKey{target.LocalID, target.NodeID}
		if _, exists := targetFrontiers[frontierKey]; exists {
			return nil, fmt.Errorf("%w: handoff successors duplicate a logical frontier", ErrInvalid)
		}
		targetFrontiers[frontierKey] = struct{}{}
		result[i].State = AuthorityHandedOff
		result[i].Successor = target.Identity
		result[i].TerminalRecordID = handoff.ID
		index[target.Identity] = len(result)
		result = append(result, target)
	}
	sortAuthorities(result)
	return result, nil
}

func authorityByID(authorities []AuthorityRecord, id OwnerIdentity) (AuthorityRecord, bool) {
	i, ok := slices.BinarySearchFunc(authorities, id, func(record AuthorityRecord, target OwnerIdentity) int {
		return cmp.Compare(record.Identity, target)
	})
	if !ok {
		return AuthorityRecord{}, false
	}
	return authorities[i], true
}

func sortAuthorities(authorities []AuthorityRecord) {
	slices.SortFunc(authorities, func(a, b AuthorityRecord) int { return cmp.Compare(a.Identity, b.Identity) })
}

func epochNodeSet(epoch TemplateEpoch) map[string]struct{} {
	result := make(map[string]struct{}, len(epoch.Graph.Nodes))
	for _, node := range epoch.Graph.Nodes {
		result[node.ID] = struct{}{}
	}
	return result
}

func (state AuthorityState) active() bool {
	return state == AuthorityVerifiedUnclaimed || state == AuthorityClaimed || state == AuthorityActive
}

func (state AuthorityState) terminal() bool {
	return state == AuthorityCompleted || state == AuthorityFailed || state == AuthorityCanceled || state == AuthorityHandedOff
}

func (result FinishResult) valid() bool {
	return result == FinishCompleted || result == FinishFailed || result == FinishCanceled
}

func (result FinishResult) authorityState() AuthorityState {
	switch result {
	case FinishCompleted:
		return AuthorityCompleted
	case FinishFailed:
		return AuthorityFailed
	case FinishCanceled:
		return AuthorityCanceled
	default:
		return ""
	}
}
