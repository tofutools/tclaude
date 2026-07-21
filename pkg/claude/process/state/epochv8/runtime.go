package epochv8

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
)

const (
	RuntimeArtifactVersion  = 1
	MaxRuntimeArtifactBytes = 16 << 20
)

// RuntimeArtifactV1 is one immutable complete path-v1 aggregate head. The
// public process run id never enters the nested path-v1 identity namespace.
type RuntimeArtifactV1 struct {
	Version              int               `json:"version"`
	InternalRunID        string            `json:"internalRunId"`
	HeadOwner            OwnerIdentity     `json:"headOwner"`
	EpochID              EpochID           `json:"epochId"`
	TemplateRef          string            `json:"templateRef"`
	TemplateSourceDigest string            `json:"templateSourceDigest"`
	Checkpoint           json.RawMessage   `json:"checkpoint"`
	Projection           []AuthorityRecord `json:"projection"`
	Digest               string            `json:"digest"`
}

type RuntimeTransitionResult struct {
	Checkpoint   *CheckpointV8
	Artifact     *RuntimeArtifactV1
	ArtifactJSON []byte
	Disposition  Disposition
	Binding      Binding
	Provenance   ApplyAuthorization
}

// RuntimeInternalRunID returns the full domain-separated nested path-v1 run
// identity. It is intentionally distinct from the public run id.
func RuntimeInternalRunID(publicRunID string, owner OwnerIdentity, epoch EpochID, sourceDigest string) (string, error) {
	if !validIdentifier(publicRunID) || !canonicalDigest(string(owner)) || !canonicalDigest(string(epoch)) || !canonicalDigest(sourceDigest) {
		return "", fmt.Errorf("%w: runtime identity input is invalid", ErrInvalid)
	}
	return digestValue("runtime-internal-run/v1", struct {
		RunID  string        `json:"runId"`
		Owner  OwnerIdentity `json:"owner"`
		Epoch  EpochID       `json:"epoch"`
		Source string        `json:"source"`
	}{publicRunID, owner, epoch, sourceDigest})
}

// AttachGenesis binds the sole verified-unclaimed epoch-zero frontier to a
// fresh complete path-v1 aggregate at the exact template start.
func AttachGenesis(ctx context.Context, checkpoint *CheckpointV8, templateSource []byte) (RuntimeTransitionResult, error) {
	if err := VerifyCheckpointV8(checkpoint); err != nil {
		return RuntimeTransitionResult{}, err
	}
	if checkpoint.wire.RuntimeBinding != (RuntimeBinding{}) {
		return RuntimeTransitionResult{}, fmt.Errorf("%w: runtime is already attached", ErrInvalid)
	}
	var head AuthorityRecord
	for _, authority := range checkpoint.wire.Authorities {
		if authority.Kind == AuthorityFrontier && authority.State == AuthorityVerifiedUnclaimed {
			if head.Identity != "" {
				return RuntimeTransitionResult{}, fmt.Errorf("%w: genesis requires exactly one frontier", ErrInvalid)
			}
			head = authority
		} else if authority.State.active() {
			return RuntimeTransitionResult{}, fmt.Errorf("%w: genesis carries active accessories", ErrInvalid)
		}
	}
	if head.Identity == "" || head.EpochID != checkpoint.wire.Anchor.OriginalEpoch.ID {
		return RuntimeTransitionResult{}, fmt.Errorf("%w: epoch-zero frontier is absent", ErrInvalid)
	}
	epoch, ok := epochByID(checkpoint.wire.Epochs, head.EpochID)
	if !ok || epoch.TemplateSourceDigest != digestSource(templateSource) {
		return RuntimeTransitionResult{}, fmt.Errorf("%w: exact owner source differs", ErrInvalid)
	}
	internal, err := RuntimeInternalRunID(checkpoint.wire.Anchor.RunID, head.Identity, head.EpochID, epoch.TemplateSourceDigest)
	if err != nil {
		return RuntimeTransitionResult{}, err
	}
	inner, genesisWitness, err := pathv1.BuildRuntimeGenesisWitness(ctx, internal, templateSource, head.NodeID)
	if err != nil {
		return RuntimeTransitionResult{}, err
	}
	innerJSON, err := pathv1.EncodeCheckpointV7(inner)
	if err != nil {
		return RuntimeTransitionResult{}, err
	}
	artifact, artifactJSON, err := buildRuntimeArtifact(ctx, checkpoint, head.Identity, head.EpochID, templateSource, innerJSON)
	if err != nil {
		return RuntimeTransitionResult{}, err
	}
	return appendRuntimeReceipt(checkpoint, artifact, artifactJSON, RuntimeReceipt{
		Kind: RuntimeAttachGenesis, Owner: head.Identity, EpochID: head.EpochID,
		TemplateSourceDigest: epoch.TemplateSourceDigest, PreRuntime: RuntimeBinding{},
		PostRuntime: RuntimeBinding{Revision: 1, Digest: artifact.Digest},
		Before:      cloneAuthorities(checkpoint.wire.Authorities), After: cloneAuthorities(artifact.Projection),
		GenesisWitness: genesisWitness,
	})
}

func AdvanceHead(ctx context.Context, checkpoint *CheckpointV8, artifactJSON, templateSource []byte, transition *pathv1.ExecutionTransition) (RuntimeTransitionResult, error) {
	if transition == nil || transition.Witness() == nil || !runtimeAdvanceKind(transition.Kind()) {
		return RuntimeTransitionResult{}, fmt.Errorf("%w: path-v1 transition kind is not admitted", ErrInvalid)
	}
	return advanceRuntime(ctx, checkpoint, artifactJSON, templateSource, transition, RuntimeAdvanceHead, "")
}

func ClaimExternal(ctx context.Context, checkpoint *CheckpointV8, artifactJSON, templateSource []byte, transition *pathv1.ExecutionTransition) (RuntimeTransitionResult, error) {
	if transition == nil || transition.Witness() == nil || transition.Kind() != pathv1.TransitionClaimAttempt {
		return RuntimeTransitionResult{}, fmt.Errorf("%w: exact attempt claim is required", ErrInvalid)
	}
	return advanceRuntime(ctx, checkpoint, artifactJSON, templateSource, transition, RuntimeClaimExternal, "")
}

func FinishClaimedHead(ctx context.Context, checkpoint *CheckpointV8, artifactJSON, templateSource []byte, transition *pathv1.ExecutionTransition, evidenceDigest string) (RuntimeTransitionResult, error) {
	if transition == nil || transition.Witness() == nil || transition.Kind() != pathv1.TransitionObserveAttempt || !canonicalDigest(evidenceDigest) {
		return RuntimeTransitionResult{}, fmt.Errorf("%w: exact attempt observation/evidence is required", ErrInvalid)
	}
	return advanceRuntime(ctx, checkpoint, artifactJSON, templateSource, transition, RuntimeFinishClaimed, evidenceDigest)
}

// AuditedSettlement records one explicit operator rescue against an exact
// finished generation. It is deliberately separate from ordinary execution
// advancement and derives its receipt authority from the sealed path-v1
// transition rather than caller-supplied metadata.
func AuditedSettlement(ctx context.Context, checkpoint *CheckpointV8, artifactJSON, templateSource []byte, transition *pathv1.ExecutionTransition) (RuntimeTransitionResult, error) {
	if transition == nil || transition.Witness() == nil {
		return RuntimeTransitionResult{}, fmt.Errorf("%w: exact audited settlement is required", ErrInvalid)
	}
	resolution, ok := transition.AuditedResolution()
	if transition.Kind() != pathv1.TransitionAuditedSettlement || !ok {
		return RuntimeTransitionResult{}, fmt.Errorf("%w: exact audited settlement is required", ErrInvalid)
	}
	resolutionDigest, err := pathv1.ValidateBlockResolution(resolution)
	if err != nil {
		return RuntimeTransitionResult{}, fmt.Errorf("%w: audited settlement provenance is invalid", ErrInvalid)
	}
	current, err := verifyCurrentRuntime(ctx, checkpoint, artifactJSON, templateSource)
	if err != nil {
		return RuntimeTransitionResult{}, err
	}
	currentInner, err := pathv1.DecodeCheckpointV7(current.Checkpoint)
	if err != nil {
		return RuntimeTransitionResult{}, err
	}
	if pathv1.CurrentCheckpointBinding(currentInner) == transition.PostBinding() {
		return exactRuntimeReplay(checkpoint, current, artifactJSON, RuntimeSettlement, transition.Kind(), "", resolutionDigest)
	}
	_, postBytes, _, err := pathv1.ValidateExecutionTransitionForAppend(ctx, current.Checkpoint, templateSource, transition)
	if err != nil {
		return RuntimeTransitionResult{}, err
	}
	if bytes.Equal(current.Checkpoint, postBytes) {
		return exactRuntimeReplay(checkpoint, current, artifactJSON, RuntimeSettlement, transition.Kind(), "", resolutionDigest)
	}
	next, nextJSON, err := buildRuntimeArtifact(ctx, checkpoint, current.HeadOwner, current.EpochID, templateSource, postBytes)
	if err != nil {
		return RuntimeTransitionResult{}, err
	}
	owner := current.HeadOwner
	added := addedVerifiedFrontiers(current.Projection, next.Projection)
	if resolution.Decision == "retry" {
		if len(added) != 1 {
			return RuntimeTransitionResult{}, fmt.Errorf("%w: retry settlement must create exactly one verified frontier", ErrInvalid)
		}
		owner = added[0]
	} else if len(added) != 0 {
		return RuntimeTransitionResult{}, fmt.Errorf("%w: non-retry settlement created execution authority", ErrInvalid)
	}
	receipt := RuntimeReceipt{
		Kind: RuntimeSettlement, PathTransitionKind: transition.Kind(), Owner: owner, EpochID: current.EpochID,
		TemplateSourceDigest: current.TemplateSourceDigest, PreRuntime: checkpoint.wire.RuntimeBinding,
		PostRuntime: RuntimeBinding{Revision: checkpoint.wire.RuntimeBinding.Revision + 1, Digest: next.Digest},
		Before:      cloneAuthorities(checkpoint.wire.Authorities), After: cloneAuthorities(next.Projection),
		Decision: resolution.Decision, Actor: resolution.Actor, Timestamp: resolution.Timestamp,
		NodeID: resolution.NodeID, BlockedAttempt: resolution.BlockedAttempt, Reason: resolution.Reason,
		EvidenceRef: resolution.EvidenceRef, ResolutionDigest: resolutionDigest,
		ExecutionWitness: transition.Witness(),
	}
	return appendRuntimeReceipt(checkpoint, next, nextJSON, receipt)
}

func addedVerifiedFrontiers(before, after []AuthorityRecord) []OwnerIdentity {
	known := make(map[OwnerIdentity]struct{}, len(before))
	for _, authority := range before {
		known[authority.Identity] = struct{}{}
	}
	result := make([]OwnerIdentity, 0, 1)
	for _, authority := range after {
		if _, exists := known[authority.Identity]; !exists && authority.Kind == AuthorityFrontier && authority.State == AuthorityVerifiedUnclaimed {
			result = append(result, authority.Identity)
		}
	}
	return result
}

func ApplyRetainHead(ctx context.Context, checkpoint *CheckpointV8, artifactJSON, ownerSource []byte, plan *ApplyPlan) (RuntimeTransitionResult, error) {
	return applyRetainHead(ctx, checkpoint, artifactJSON, ownerSource, plan, nil)
}

func ApplyRetainHeadAuthorized(ctx context.Context, checkpoint *CheckpointV8, artifactJSON, ownerSource []byte, plan *ApplyPlan, authorization ApplyAuthorization) (RuntimeTransitionResult, error) {
	if err := validateAuthorizedApplyAuthorization(authorization); err != nil {
		return RuntimeTransitionResult{}, err
	}
	return applyRetainHead(ctx, checkpoint, artifactJSON, ownerSource, plan, &authorization)
}

func applyRetainHead(ctx context.Context, checkpoint *CheckpointV8, artifactJSON, ownerSource []byte, plan *ApplyPlan, authorization *ApplyAuthorization) (RuntimeTransitionResult, error) {
	if replay, ok, err := replayPublishedRuntimeApply(ctx, checkpoint, artifactJSON, ownerSource, plan, RuntimeApplyRetain, authorization); ok || err != nil {
		return replay, err
	}
	current, err := verifyCurrentRuntime(ctx, checkpoint, artifactJSON, ownerSource)
	if err != nil {
		return RuntimeTransitionResult{}, err
	}
	record, authorities, err := prepareRuntimeApply(checkpoint, plan, authorization)
	if err != nil {
		return RuntimeTransitionResult{}, err
	}
	for _, handoff := range record.HandoffSet {
		if handoff.Action != HandoffRetain || handoff.Target != nil {
			return RuntimeTransitionResult{}, fmt.Errorf("%w: retain apply contains a transfer", ErrInvalid)
		}
	}
	if !reflectAuthorities(authorities, checkpoint.wire.Authorities) {
		return RuntimeTransitionResult{}, fmt.Errorf("%w: retain apply changed runtime authority", ErrInvalid)
	}
	receipt := RuntimeReceipt{
		Kind: RuntimeApplyRetain, Owner: current.HeadOwner, EpochID: current.EpochID,
		TemplateSourceDigest: current.TemplateSourceDigest, PreRuntime: checkpoint.wire.RuntimeBinding,
		PostRuntime: checkpoint.wire.RuntimeBinding, Before: cloneAuthorities(checkpoint.wire.Authorities), After: cloneAuthorities(authorities),
	}
	return appendRuntimeApplyReceipt(checkpoint, record, current, artifactJSON, receipt)
}

func ApplyTransferHead(ctx context.Context, checkpoint *CheckpointV8, artifactJSON, ownerSource, candidateSource []byte, plan *ApplyPlan) (RuntimeTransitionResult, error) {
	return applyTransferHead(ctx, checkpoint, artifactJSON, ownerSource, candidateSource, plan, nil)
}

func ApplyTransferHeadAuthorized(ctx context.Context, checkpoint *CheckpointV8, artifactJSON, ownerSource, candidateSource []byte, plan *ApplyPlan, authorization ApplyAuthorization) (RuntimeTransitionResult, error) {
	if err := validateAuthorizedApplyAuthorization(authorization); err != nil {
		return RuntimeTransitionResult{}, err
	}
	return applyTransferHead(ctx, checkpoint, artifactJSON, ownerSource, candidateSource, plan, &authorization)
}

func applyTransferHead(ctx context.Context, checkpoint *CheckpointV8, artifactJSON, ownerSource, candidateSource []byte, plan *ApplyPlan, authorization *ApplyAuthorization) (RuntimeTransitionResult, error) {
	if replay, ok, err := replayPublishedRuntimeApply(ctx, checkpoint, artifactJSON, ownerSource, plan, RuntimeApplyTransfer, authorization); ok || err != nil {
		return replay, err
	}
	if _, err := verifyCurrentRuntime(ctx, checkpoint, artifactJSON, ownerSource); err != nil {
		return RuntimeTransitionResult{}, err
	}
	record, authorities, err := prepareRuntimeApply(checkpoint, plan, authorization)
	if err != nil {
		return RuntimeTransitionResult{}, err
	}
	active := make([]AuthorityRecord, 0, 2)
	for _, authority := range checkpoint.wire.Authorities {
		if authority.State.active() {
			active = append(active, authority)
		}
	}
	if len(active) != 1 || active[0].Kind != AuthorityFrontier || active[0].State != AuthorityVerifiedUnclaimed {
		return RuntimeTransitionResult{}, fmt.Errorf("%w: transfer active closure is not bare", ErrProtectedAuthority)
	}
	var target *AuthorityRecord
	for _, handoff := range record.HandoffSet {
		if handoff.Action == HandoffTransfer && handoff.Source == active[0].Identity && handoff.Target != nil {
			copy := cloneAuthority(*handoff.Target)
			target = &copy
		} else if handoff.Action != HandoffRetain {
			return RuntimeTransitionResult{}, fmt.Errorf("%w: transfer apply has an extra handoff", ErrInvalid)
		}
	}
	if target == nil || target.EpochID != record.CandidateEpoch.ID || digestSource(candidateSource) != record.CandidateEpoch.TemplateSourceDigest {
		return RuntimeTransitionResult{}, fmt.Errorf("%w: sealed transfer target/source is invalid", ErrInvalid)
	}
	internal, err := RuntimeInternalRunID(checkpoint.wire.Anchor.RunID, target.Identity, target.EpochID, record.CandidateEpoch.TemplateSourceDigest)
	if err != nil {
		return RuntimeTransitionResult{}, err
	}
	inner, genesisWitness, err := pathv1.BuildRuntimeGenesisWitness(ctx, internal, candidateSource, target.NodeID)
	if err != nil {
		return RuntimeTransitionResult{}, err
	}
	innerJSON, err := pathv1.EncodeCheckpointV7(inner)
	if err != nil {
		return RuntimeTransitionResult{}, err
	}
	epochs := append(cloneEpochs(checkpoint.wire.Epochs), cloneEpoch(record.CandidateEpoch))
	artifact, nextJSON, err := buildRuntimeArtifactState(ctx, checkpoint.wire.Anchor.RunID, epochs, authorities, target.Identity, target.EpochID, candidateSource, innerJSON)
	if err != nil {
		return RuntimeTransitionResult{}, err
	}
	receipt := RuntimeReceipt{
		Kind: RuntimeApplyTransfer, Owner: target.Identity, EpochID: target.EpochID,
		TemplateSourceDigest: record.CandidateEpoch.TemplateSourceDigest, PreRuntime: checkpoint.wire.RuntimeBinding,
		PostRuntime: RuntimeBinding{Revision: checkpoint.wire.RuntimeBinding.Revision + 1, Digest: artifact.Digest},
		Before:      cloneAuthorities(checkpoint.wire.Authorities), After: cloneAuthorities(artifact.Projection),
		GenesisWitness: genesisWitness,
	}
	return appendRuntimeApplyReceipt(checkpoint, record, artifact, nextJSON, receipt)
}

// PreflightRuntimeApply executes the same pure typed constructors used by a
// later publication, then discards the prospective transition. A sealed
// proposal is rescue-ready only when it transfers the sole bare
// verified-unclaimed frontier; constructor refusals are a valid non-rescue
// preview result rather than an alternate mutation path.
func PreflightRuntimeApply(ctx context.Context, checkpoint *CheckpointV8, artifactJSON, ownerSource, candidateSource []byte, plan *ApplyPlan) (RuntimeApplyPreflight, error) {
	if err := VerifyCheckpointV8(checkpoint); err != nil || plan == nil {
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf("%w: sealed apply is required", ErrInvalid)
	}
	bare, transfer := bareTransfer(checkpoint, plan)
	if checkpoint.wire.RuntimeBinding == (RuntimeBinding{}) {
		if _, err := Apply(checkpoint, plan); err != nil {
			return RuntimeApplyRefused, nil
		}
		if bare && transfer {
			return RuntimeApplyTransferReady, nil
		}
		return RuntimeApplyRetainReady, nil
	}
	if bare && transfer {
		if _, err := ApplyTransferHead(ctx, checkpoint, artifactJSON, ownerSource, candidateSource, plan); err == nil {
			return RuntimeApplyTransferReady, nil
		}
		return RuntimeApplyRefused, nil
	}
	if _, err := ApplyRetainHead(ctx, checkpoint, artifactJSON, ownerSource, plan); err == nil {
		return RuntimeApplyRetainReady, nil
	}
	return RuntimeApplyRefused, nil
}

func bareTransfer(checkpoint *CheckpointV8, plan *ApplyPlan) (bool, bool) {
	active := make([]AuthorityRecord, 0, 2)
	for _, authority := range checkpoint.wire.Authorities {
		if authority.State.active() {
			active = append(active, authority)
		}
	}
	if len(active) != 1 || active[0].Kind != AuthorityFrontier || active[0].State != AuthorityVerifiedUnclaimed {
		return false, false
	}
	transfers := 0
	matched := false
	for _, handoff := range plan.core.HandoffSet {
		if handoff.Action == HandoffTransfer {
			transfers++
			matched = handoff.Source == active[0].Identity && handoff.Target != nil
		}
	}
	return true, transfers == 1 && matched
}

// PreflightAuditedSettlement runs the exact outer runtime constructor in
// memory. AuditedSettlement itself proves that retry creates exactly one new
// verified frontier and that non-retry decisions create none.
func PreflightAuditedSettlement(ctx context.Context, checkpoint *CheckpointV8, artifactJSON, ownerSource []byte, transition *pathv1.ExecutionTransition) (RuntimeTransitionResult, error) {
	return AuditedSettlement(ctx, checkpoint, artifactJSON, ownerSource, transition)
}

func advanceRuntime(ctx context.Context, checkpoint *CheckpointV8, artifactJSON, templateSource []byte, transition *pathv1.ExecutionTransition, kind RuntimeTransitionKind, evidence string) (RuntimeTransitionResult, error) {
	current, err := verifyCurrentRuntime(ctx, checkpoint, artifactJSON, templateSource)
	if err != nil {
		return RuntimeTransitionResult{}, err
	}
	currentInner, err := pathv1.DecodeCheckpointV7(current.Checkpoint)
	if err != nil {
		return RuntimeTransitionResult{}, err
	}
	if pathv1.CurrentCheckpointBinding(currentInner) == transition.PostBinding() {
		return exactRuntimeReplay(checkpoint, current, artifactJSON, kind, transition.Kind(), evidence, "")
	}
	_, postBytes, _, err := pathv1.ValidateExecutionTransitionForAppend(ctx, current.Checkpoint, templateSource, transition)
	if err != nil {
		return RuntimeTransitionResult{}, err
	}
	if bytes.Equal(current.Checkpoint, postBytes) {
		return exactRuntimeReplay(checkpoint, current, artifactJSON, kind, transition.Kind(), evidence, "")
	}
	next, nextJSON, err := buildRuntimeArtifact(ctx, checkpoint, current.HeadOwner, current.EpochID, templateSource, postBytes)
	if err != nil {
		return RuntimeTransitionResult{}, err
	}
	owner := current.HeadOwner
	if kind == RuntimeClaimExternal || kind == RuntimeFinishClaimed {
		owner, err = changedFrontierOwner(current.Projection, next.Projection, kind)
		if err != nil {
			return RuntimeTransitionResult{}, err
		}
	}
	receipt := RuntimeReceipt{
		Kind: kind, PathTransitionKind: transition.Kind(), Owner: owner, EpochID: current.EpochID,
		TemplateSourceDigest: current.TemplateSourceDigest, PreRuntime: checkpoint.wire.RuntimeBinding,
		PostRuntime: RuntimeBinding{Revision: checkpoint.wire.RuntimeBinding.Revision + 1, Digest: next.Digest},
		Before:      cloneAuthorities(checkpoint.wire.Authorities), After: cloneAuthorities(next.Projection), EvidenceDigest: evidence,
		ExecutionWitness: transition.Witness(),
	}
	return appendRuntimeReceipt(checkpoint, next, nextJSON, receipt)
}

func exactRuntimeReplay(checkpoint *CheckpointV8, artifact *RuntimeArtifactV1, artifactJSON []byte, kind RuntimeTransitionKind, pathKind, evidence, resolutionDigest string) (RuntimeTransitionResult, error) {
	for index := len(checkpoint.wire.History) - 1; index >= 0; index-- {
		receipt := checkpoint.wire.History[index].Runtime
		if receipt == nil || receipt.PostRuntime != checkpoint.wire.RuntimeBinding {
			continue
		}
		if receipt.Kind != kind || receipt.PathTransitionKind != pathKind || receipt.EvidenceDigest != evidence ||
			resolutionDigest != "" && receipt.ResolutionDigest != resolutionDigest {
			continue
		}
		return RuntimeTransitionResult{Checkpoint: checkpoint, Artifact: cloneRuntimeArtifact(artifact), ArtifactJSON: bytes.Clone(artifactJSON), Disposition: DispositionReplayed, Binding: checkpoint.Binding()}, nil
	}
	return RuntimeTransitionResult{}, fmt.Errorf("%w: runtime replay receipt is absent", ErrInvalid)
}

func replayPublishedRuntimeApply(ctx context.Context, checkpoint *CheckpointV8, artifactJSON, source []byte, plan *ApplyPlan, kind RuntimeTransitionKind, authorization *ApplyAuthorization) (RuntimeTransitionResult, bool, error) {
	if plan == nil {
		return RuntimeTransitionResult{}, false, nil
	}
	current, err := verifyCurrentRuntime(ctx, checkpoint, artifactJSON, source)
	if err != nil {
		return RuntimeTransitionResult{}, false, err
	}
	for _, event := range checkpoint.wire.History {
		if event.Apply != nil && event.Runtime != nil && event.Runtime.Kind == kind && reflect.DeepEqual(event.Apply.applyCore, plan.core) {
			provenance := applyAuthorization(*event.Apply)
			if authorization != nil {
				if err := validateAuthorizedApplyAuthorization(provenance); err != nil ||
					provenance.HandoffDirectiveDigest != authorization.HandoffDirectiveDigest {
					return RuntimeTransitionResult{}, true, fmt.Errorf("%w: committed runtime apply authorization differs", ErrInvalid)
				}
			}
			return RuntimeTransitionResult{Checkpoint: checkpoint, Artifact: current, ArtifactJSON: bytes.Clone(artifactJSON), Disposition: DispositionReplayed, Binding: checkpoint.Binding(), Provenance: provenance}, true, nil
		}
	}
	return RuntimeTransitionResult{}, false, nil
}

func appendRuntimeReceipt(checkpoint *CheckpointV8, artifact *RuntimeArtifactV1, artifactJSON []byte, receipt RuntimeReceipt) (RuntimeTransitionResult, error) {
	_, runtimeEvents := runtimeHistoryCounts(checkpoint.wire.History)
	if runtimeEvents >= MaxRuntimeReceipts {
		return RuntimeTransitionResult{}, &OverBudgetError{Limit: "runtime_receipts", Value: runtimeEvents + 1, Maximum: MaxRuntimeReceipts}
	}
	var err error
	receipt.ID, err = runtimeReceiptIdentity(receipt)
	if err != nil {
		return RuntimeTransitionResult{}, err
	}
	event := HistoryEvent{Revision: uint64(len(checkpoint.wire.History) + 1), Kind: HistoryRuntime, Runtime: &receipt}
	event.Digest, err = historyEventDigest(event)
	if err != nil {
		return RuntimeTransitionResult{}, err
	}
	wire := cloneWire(checkpoint.wire)
	wire.RuntimeBinding = receipt.PostRuntime
	wire.Authorities = cloneAuthorities(receipt.After)
	wire.History = append(wire.History, event)
	wire.Digest, err = checkpointDigest(wire)
	if err != nil {
		return RuntimeTransitionResult{}, err
	}
	next := &CheckpointV8{wire: wire}
	if err := VerifyCheckpointV8(next); err != nil {
		return RuntimeTransitionResult{}, err
	}
	return RuntimeTransitionResult{Checkpoint: next, Artifact: cloneRuntimeArtifact(artifact), ArtifactJSON: bytes.Clone(artifactJSON), Disposition: DispositionApplied, Binding: next.Binding()}, nil
}

func prepareRuntimeApply(checkpoint *CheckpointV8, plan *ApplyPlan, authorization *ApplyAuthorization) (*ApplyRecord, []AuthorityRecord, error) {
	if err := VerifyCheckpointV8(checkpoint); err != nil {
		return nil, nil, err
	}
	if checkpoint.wire.RuntimeBinding == (RuntimeBinding{}) || plan == nil {
		return nil, nil, fmt.Errorf("%w: attached runtime and sealed apply are required", ErrInvalid)
	}
	core := cloneApplyCore(plan.core)
	if err := validateApplyCoreStatic(checkpoint.wire.Anchor.RunID, core); err != nil {
		return nil, nil, err
	}
	if core.BaseBinding != checkpoint.Binding() || core.PredecessorEpoch != checkpoint.wire.CurrentEpochID || core.CandidateEpoch.Ordinal != uint64(len(checkpoint.wire.Epochs)) {
		return nil, nil, fmt.Errorf("%w: runtime apply is stale", ErrInvalid)
	}
	protected, err := protectedClosure(checkpoint.wire.Authorities)
	if err != nil || !reflectAuthorities(protected, core.Protected) {
		return nil, nil, fmt.Errorf("%w: runtime apply protected closure changed", ErrInvalid)
	}
	dependencies, err := newAuthorityDependencyIndex(checkpoint.wire.Authorities)
	if err != nil {
		return nil, nil, err
	}
	authorities, err := applyHandoffSet(checkpoint.wire.Anchor.RunID, checkpoint.wire.Authorities, core.HandoffSet, dependencies)
	if err != nil {
		return nil, nil, err
	}
	record := &ApplyRecord{applyCore: core}
	if authorization != nil {
		record.HandoffDirectiveDigest = authorization.HandoffDirectiveDigest
		record.ReasonCode = authorization.ReasonCode
		record.Actor = authorization.Actor
		record.AppliedAt = authorization.AppliedAt
	}
	record.RecordDigest, err = applyRecordDigest(*record)
	if err != nil {
		return nil, nil, err
	}
	return record, authorities, nil
}

func appendRuntimeApplyReceipt(checkpoint *CheckpointV8, record *ApplyRecord, artifact *RuntimeArtifactV1, artifactJSON []byte, receipt RuntimeReceipt) (RuntimeTransitionResult, error) {
	epochEvents, runtimeEvents := runtimeHistoryCounts(checkpoint.wire.History)
	if epochEvents >= MaxHistoryEvents {
		return RuntimeTransitionResult{}, &OverBudgetError{Limit: "history_events", Value: epochEvents + 1, Maximum: MaxHistoryEvents}
	}
	if runtimeEvents >= MaxRuntimeReceipts {
		return RuntimeTransitionResult{}, &OverBudgetError{Limit: "runtime_receipts", Value: runtimeEvents + 1, Maximum: MaxRuntimeReceipts}
	}
	var err error
	receipt.ID, err = runtimeReceiptIdentity(receipt)
	if err != nil {
		return RuntimeTransitionResult{}, err
	}
	event := HistoryEvent{Revision: uint64(len(checkpoint.wire.History) + 1), Kind: HistoryRuntime, Apply: record, Runtime: &receipt}
	event.Digest, err = historyEventDigest(event)
	if err != nil {
		return RuntimeTransitionResult{}, err
	}
	wire := cloneWire(checkpoint.wire)
	wire.Epochs = append(wire.Epochs, cloneEpoch(record.CandidateEpoch))
	wire.CurrentEpochID = record.CandidateEpoch.ID
	wire.RuntimeBinding = receipt.PostRuntime
	wire.Authorities = cloneAuthorities(receipt.After)
	wire.History = append(wire.History, event)
	wire.Digest, err = checkpointDigest(wire)
	if err != nil {
		return RuntimeTransitionResult{}, err
	}
	next := &CheckpointV8{wire: wire}
	if err := VerifyCheckpointV8(next); err != nil {
		return RuntimeTransitionResult{}, err
	}
	return RuntimeTransitionResult{Checkpoint: next, Artifact: cloneRuntimeArtifact(artifact), ArtifactJSON: bytes.Clone(artifactJSON), Disposition: DispositionApplied, Binding: next.Binding(), Provenance: applyAuthorization(*record)}, nil
}

func runtimeHistoryCounts(events []HistoryEvent) (epochEvents, runtimeEvents int) {
	for _, event := range events {
		if event.Runtime != nil {
			runtimeEvents++
		}
		if event.Apply != nil || event.Kind != HistoryRuntime {
			epochEvents++
		}
	}
	return epochEvents, runtimeEvents
}

func cloneEpochs(source []TemplateEpoch) []TemplateEpoch {
	result := make([]TemplateEpoch, len(source))
	for i := range source {
		result[i] = cloneEpoch(source[i])
	}
	return result
}

func buildRuntimeArtifact(ctx context.Context, checkpoint *CheckpointV8, head OwnerIdentity, epochID EpochID, source, innerJSON []byte) (*RuntimeArtifactV1, []byte, error) {
	if err := VerifyCheckpointV8(checkpoint); err != nil {
		return nil, nil, err
	}
	return buildRuntimeArtifactState(ctx, checkpoint.wire.Anchor.RunID, checkpoint.wire.Epochs, checkpoint.wire.Authorities, head, epochID, source, innerJSON)
}

func buildRuntimeArtifactState(ctx context.Context, publicRunID string, epochs []TemplateEpoch, authorities []AuthorityRecord, head OwnerIdentity, epochID EpochID, source, innerJSON []byte) (*RuntimeArtifactV1, []byte, error) {
	epoch, ok := epochByID(epochs, epochID)
	if !ok || epoch.TemplateSourceDigest != digestSource(source) {
		return nil, nil, fmt.Errorf("%w: runtime owner epoch/source is invalid", ErrInvalid)
	}
	verified, err := pathv1.VerifyExecutionInput(ctx, innerJSON, source)
	if err != nil {
		return nil, nil, err
	}
	inner, err := pathv1.DecodeCheckpointV7(innerJSON)
	if err != nil {
		return nil, nil, err
	}
	aggregate, err := pathv1.CurrentAggregateCheckpoint(inner)
	if err != nil {
		return nil, nil, err
	}
	_ = verified
	internal, err := RuntimeInternalRunID(publicRunID, head, epochID, epoch.TemplateSourceDigest)
	if err != nil || aggregate.RunID != internal || aggregate.TemplateSourceHash != epoch.TemplateSourceDigest {
		return nil, nil, fmt.Errorf("%w: nested runtime identity differs", ErrInvalid)
	}
	headRecord, ok := authorityByID(authorities, head)
	if !ok {
		return nil, nil, fmt.Errorf("%w: runtime head owner is absent", ErrInvalid)
	}
	projection, err := projectRuntimeAuthorities(publicRunID, epochID, headRecord, aggregate)
	if err != nil {
		return nil, nil, err
	}
	projection = mergeRuntimeHistory(authorities, projection)
	if err := validateAuthorities(publicRunID, epochs, projection, false); err != nil {
		return nil, nil, err
	}
	artifact := &RuntimeArtifactV1{
		Version: RuntimeArtifactVersion, InternalRunID: internal, HeadOwner: head, EpochID: epochID,
		TemplateRef: epoch.TemplateRef, TemplateSourceDigest: epoch.TemplateSourceDigest,
		Checkpoint: bytes.Clone(innerJSON), Projection: projection,
	}
	artifact.Digest, err = runtimeArtifactDigest(*artifact)
	if err != nil {
		return nil, nil, err
	}
	encoded, err := encodeRuntimeArtifact(artifact)
	if err != nil {
		return nil, nil, err
	}
	canonical, err := decodeRuntimeArtifact(encoded)
	return canonical, encoded, err
}

func mergeRuntimeHistory(current, projection []AuthorityRecord) []AuthorityRecord {
	byID := make(map[OwnerIdentity]struct{}, len(projection))
	for _, authority := range projection {
		byID[authority.Identity] = struct{}{}
	}
	for _, authority := range current {
		if _, exists := byID[authority.Identity]; exists {
			continue
		}
		retained := cloneAuthority(authority)
		if !retained.State.terminal() {
			if retained.State != AuthorityActive || retained.Kind != AuthorityOutcome && retained.Kind != AuthorityJoin ||
				!strings.HasPrefix(retained.LocalID, "reservation.") || retained.ReservationID != retained.LocalID {
				continue
			}
			retained.State = AuthorityCompleted
			sealTerminal(&retained, "reservation", strings.TrimPrefix(retained.LocalID, "reservation."), string(pathv1.ReservationActivated))
		}
		projection = append(projection, retained)
	}
	sortAuthorities(projection)
	return projection
}

func VerifyRuntimeArtifact(ctx context.Context, checkpoint *CheckpointV8, artifactJSON, source []byte) (*RuntimeArtifactV1, error) {
	return verifyCurrentRuntime(ctx, checkpoint, artifactJSON, source)
}

func verifyCurrentRuntime(ctx context.Context, checkpoint *CheckpointV8, artifactJSON, source []byte) (*RuntimeArtifactV1, error) {
	if err := VerifyCheckpointV8(checkpoint); err != nil {
		return nil, err
	}
	artifact, err := decodeRuntimeArtifact(artifactJSON)
	if err != nil {
		return nil, err
	}
	if checkpoint.wire.RuntimeBinding == (RuntimeBinding{}) || artifact.Digest != checkpoint.wire.RuntimeBinding.Digest ||
		!reflectAuthorities(artifact.Projection, checkpoint.wire.Authorities) {
		return nil, fmt.Errorf("%w: runtime artifact differs from checkpoint binding/projection", ErrInvalid)
	}
	rebuilt, _, err := buildRuntimeArtifact(ctx, checkpoint, artifact.HeadOwner, artifact.EpochID, source, artifact.Checkpoint)
	if err != nil || rebuilt.Digest != artifact.Digest {
		return nil, fmt.Errorf("%w: runtime artifact cannot be rederived", ErrInvalid)
	}
	return artifact, nil
}

func encodeRuntimeArtifact(artifact *RuntimeArtifactV1) ([]byte, error) {
	if artifact == nil {
		return nil, fmt.Errorf("%w: runtime artifact is nil", ErrInvalid)
	}
	encoded, err := json.Marshal(artifact)
	if err != nil {
		return nil, err
	}
	encoded = append(encoded, '\n')
	if len(encoded) > MaxRuntimeArtifactBytes {
		return nil, &OverBudgetError{Limit: "runtime_artifact_bytes", Value: len(encoded), Maximum: MaxRuntimeArtifactBytes}
	}
	return encoded, nil
}

func decodeRuntimeArtifact(data []byte) (*RuntimeArtifactV1, error) {
	if len(data) == 0 || len(data) > MaxRuntimeArtifactBytes {
		return nil, &OverBudgetError{Limit: "runtime_artifact_bytes", Value: len(data), Maximum: MaxRuntimeArtifactBytes}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var artifact RuntimeArtifactV1
	if err := decoder.Decode(&artifact); err != nil {
		return nil, fmt.Errorf("%w: decode runtime artifact: %v", ErrInvalid, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: runtime artifact has trailing data", ErrInvalid)
	}
	want, err := runtimeArtifactDigest(artifact)
	if err != nil || want != artifact.Digest || artifact.Version != RuntimeArtifactVersion || !canonicalDigest(artifact.Digest) {
		return nil, fmt.Errorf("%w: runtime artifact digest/envelope is invalid", ErrInvalid)
	}
	canonical, err := encodeRuntimeArtifact(&artifact)
	if err != nil || !bytes.Equal(canonical, data) {
		return nil, ErrNonCanonical
	}
	return cloneRuntimeArtifact(&artifact), nil
}

func runtimeArtifactDigest(artifact RuntimeArtifactV1) (string, error) {
	artifact.Digest = ""
	return digestValue("runtime-artifact/v1", artifact)
}

func digestSource(source []byte) string {
	if parsed, err := model.ParseExactSource(source); err == nil && parsed != nil {
		return parsed.SourceHash
	}
	return ""
}

func cloneRuntimeArtifact(value *RuntimeArtifactV1) *RuntimeArtifactV1 {
	if value == nil {
		return nil
	}
	result := *value
	result.Checkpoint = bytes.Clone(value.Checkpoint)
	result.Projection = cloneAuthorities(value.Projection)
	return &result
}

func reflectAuthorities(a, b []AuthorityRecord) bool {
	return slices.EqualFunc(a, b, func(x, y AuthorityRecord) bool { return equalAuthority(x, y) })
}

func equalAuthority(a, b AuthorityRecord) bool {
	return a.Identity == b.Identity && a.EpochID == b.EpochID && a.LocalID == b.LocalID && a.ReservationID == b.ReservationID &&
		a.NodeID == b.NodeID && a.Kind == b.Kind && a.State == b.State && slices.Equal(a.DependsOn, b.DependsOn) &&
		a.Successor == b.Successor && a.TerminalRecordID == b.TerminalRecordID
}

func changedFrontierOwner(before, after []AuthorityRecord, kind RuntimeTransitionKind) (OwnerIdentity, error) {
	for _, old := range before {
		if old.Kind != AuthorityFrontier {
			continue
		}
		next, ok := authorityByID(after, old.Identity)
		if !ok {
			continue
		}
		if kind == RuntimeClaimExternal && old.State == AuthorityVerifiedUnclaimed && next.State == AuthorityClaimed ||
			kind == RuntimeFinishClaimed && old.State == AuthorityClaimed && next.State.terminal() {
			return old.Identity, nil
		}
	}
	return "", fmt.Errorf("%w: exact frontier state change is absent", ErrInvalid)
}

func projectRuntimeAuthorities(publicRunID string, epochID EpochID, head AuthorityRecord, aggregate pathv1.AggregateCheckpoint) ([]AuthorityRecord, error) {
	result := make([]AuthorityRecord, 0, len(aggregate.Routing.Paths)+len(aggregate.Commands)+len(aggregate.SideEffects))
	for _, path := range aggregate.Routing.Paths {
		activation, ok := aggregate.Routing.Activations[path.SourceActivation.ID]
		if !ok {
			return nil, fmt.Errorf("%w: runtime path activation is absent", ErrInvalid)
		}
		reservationID := activation.ReservationID
		if path.TargetReservationID != "" {
			reservationID = path.TargetReservationID
		}
		reservation, ok := aggregate.Routing.Reservations[reservationID]
		if !ok {
			return nil, fmt.Errorf("%w: runtime path reservation is absent", ErrInvalid)
		}
		retries, err := runtimeRetryResolutions(aggregate, activation.ID, reservation.NodeID)
		if err != nil {
			return nil, err
		}
		kind := AuthorityOutcome
		state := outcomePathAuthorityState(path.State)
		if path.Kind == pathv1.PathActivationOutput {
			kind, state = AuthorityFrontier, pathAuthorityState(path.State)
		}
		authority := AuthorityRecord{EpochID: epochID, LocalID: "path." + path.ID, ReservationID: reservation.ID, NodeID: reservation.NodeID, Kind: kind, State: state, DependsOn: []OwnerIdentity{}}
		if path.Kind == pathv1.PathActivationOutput && path.ID == aggregate.Authority.Genesis.OutputPathID {
			authority.LocalID, authority.ReservationID, authority.NodeID, authority.Identity = head.LocalID, head.ReservationID, head.NodeID, head.Identity
			authority.DependsOn = slices.Clone(head.DependsOn)
		}
		if authority.Identity == "" {
			var err error
			authority.Identity, err = authorityIdentity(publicRunID, authority)
			if err != nil {
				return nil, err
			}
		}
		for _, command := range aggregate.Commands {
			if path.Kind == pathv1.PathActivationOutput && len(retries) == 0 && command.Identity.Kind == pathv1.CommandPerformAttempt && command.Identity.SourceActivationID == activation.ID {
				if command.State.Active() {
					authority.State = AuthorityClaimed
				} else if command.State == pathv1.CommandObserved || command.State == pathv1.CommandReconciled {
					authority.State = AuthorityCompleted
				}
			}
		}
		// Once an audited retry exists, the original frontier remains the
		// terminal failed authority. The revived inner path is represented by a
		// fresh retry authority below, preventing terminal identity re-entry.
		if path.Kind == pathv1.PathActivationOutput && len(retries) > 0 {
			authority.State = AuthorityFailed
		}
		sealTerminal(&authority, "path", path.ID, string(authority.State))
		result = append(result, authority)
		if path.Kind != pathv1.PathActivationOutput {
			continue
		}
		for _, retry := range retries {
			retryAuthority := AuthorityRecord{
				EpochID: epochID, LocalID: "retry." + retry.digest, ReservationID: "retry." + retry.digest,
				NodeID: reservation.NodeID, Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed,
				DependsOn: []OwnerIdentity{},
			}
			retryAuthority.Identity, err = authorityIdentity(publicRunID, retryAuthority)
			if err != nil {
				return nil, err
			}
			nextAttempt := retry.resolution.BlockedAttempt + 1
			for _, command := range aggregate.Commands {
				if command.Identity.Kind != pathv1.CommandPerformAttempt || command.Identity.SourceActivationID != activation.ID || command.Identity.Attempt != nextAttempt {
					continue
				}
				if command.State.Active() {
					retryAuthority.State = AuthorityClaimed
				} else if command.State == pathv1.CommandObserved || command.State == pathv1.CommandReconciled {
					retryAuthority.State = AuthorityCompleted
					if effectID, effectErr := pathv1.AttemptIdentity(aggregate.RunID, activation.ID, nextAttempt); effectErr == nil && aggregate.SideEffects[effectID].State == "failed" {
						retryAuthority.State = AuthorityFailed
					}
				}
			}
			sealTerminal(&retryAuthority, "retry", retry.digest, string(retryAuthority.State))
			result = append(result, retryAuthority)
		}
	}
	for id, scope := range aggregate.Routing.Scopes {
		if scope.ParentScopeID == "" {
			continue
		}
		state := AuthorityCompleted
		switch scope.State {
		case pathv1.ScopeOpen:
			state = AuthorityActive
		case pathv1.ScopeClosedNoActivation:
			state = AuthorityCanceled
		}
		authority := AuthorityRecord{EpochID: epochID, LocalID: "scope." + id, ReservationID: "scope." + id, NodeID: scope.JoinNodeID, Kind: AuthorityParallel, State: state, DependsOn: []OwnerIdentity{}}
		var err error
		authority.Identity, err = authorityIdentity(publicRunID, authority)
		if err != nil {
			return nil, err
		}
		sealTerminal(&authority, "scope", id, string(scope.State))
		result = append(result, authority)
	}
	for id, reservation := range aggregate.Routing.Reservations {
		if reservation.State == pathv1.ReservationActivated {
			continue
		}
		kind := AuthorityOutcome
		if reservation.JoinPolicy != pathv1.JoinExclusive || reservation.IsReducing {
			kind = AuthorityJoin
		}
		state := AuthorityActive
		if reservation.State == pathv1.ReservationClosedNoActivation {
			state = AuthorityCanceled
		}
		authority := AuthorityRecord{EpochID: epochID, LocalID: "reservation." + id, ReservationID: "reservation." + id, NodeID: reservation.NodeID, Kind: kind, State: state, DependsOn: []OwnerIdentity{}}
		var err error
		authority.Identity, err = authorityIdentity(publicRunID, authority)
		if err != nil {
			return nil, err
		}
		sealTerminal(&authority, "reservation", id, string(reservation.State))
		result = append(result, authority)
	}
	for key, closure := range aggregate.Routing.CandidateClosures {
		reservation := aggregate.Routing.Reservations[closure.Key.ReservationID]
		authority := AuthorityRecord{EpochID: epochID, LocalID: "closure." + key, ReservationID: "closure." + key, NodeID: reservation.NodeID, Kind: AuthorityOutcome, State: AuthorityCompleted, DependsOn: []OwnerIdentity{}}
		var err error
		authority.Identity, err = authorityIdentity(publicRunID, authority)
		if err != nil {
			return nil, err
		}
		sealTerminal(&authority, "closure", key, string(closure.TerminalKind))
		result = append(result, authority)
	}
	activeDetachmentSets := activeRuntimeDetachmentSets(aggregate)
	for key, detachment := range aggregate.Routing.Detachments {
		reservation := aggregate.Routing.Reservations[detachment.ReservationID]
		state := AuthorityCompleted
		if activeDetachmentSets[detachment.ID] {
			state = AuthorityActive
		}
		authority := AuthorityRecord{EpochID: epochID, LocalID: "detachment." + key, ReservationID: "detachment." + key, NodeID: reservation.NodeID, Kind: AuthorityDetachment, State: state, DependsOn: []OwnerIdentity{}}
		var err error
		authority.Identity, err = authorityIdentity(publicRunID, authority)
		if err != nil {
			return nil, err
		}
		sealTerminal(&authority, "detachment", key, detachment.ReasonCode)
		result = append(result, authority)
	}
	for _, command := range aggregate.Commands {
		if command.Identity.Kind == pathv1.CommandInitializeRouting {
			continue
		}
		nodeID, reservationID := aggregate.Authority.Genesis.StartNodeID, aggregate.Authority.Genesis.ReservationID
		if activation, ok := aggregate.Routing.Activations[command.Identity.SourceActivationID]; ok {
			if reservation, found := aggregate.Routing.Reservations[activation.ReservationID]; found {
				nodeID, reservationID = reservation.NodeID, reservation.ID
			}
		}
		authority := AuthorityRecord{EpochID: epochID, LocalID: "command." + command.ID, ReservationID: reservationID, NodeID: nodeID, Kind: AuthorityCommand, State: accessoryCommandState(command.State), DependsOn: []OwnerIdentity{}}
		var err error
		authority.Identity, err = authorityIdentity(publicRunID, authority)
		if err != nil {
			return nil, err
		}
		sealTerminal(&authority, "command", command.ID, string(command.State))
		result = append(result, authority)
	}
	for _, effect := range aggregate.SideEffects {
		kind := authorityKindForEffect(effect.Kind)
		if kind == "" {
			continue
		}
		nodeID, reservationID := aggregate.Authority.Genesis.StartNodeID, aggregate.Authority.Genesis.ReservationID
		if activation, ok := aggregate.Routing.Activations[effect.ActivationID]; ok {
			if reservation, found := aggregate.Routing.Reservations[activation.ReservationID]; found {
				nodeID, reservationID = reservation.NodeID, reservation.ID
			}
		}
		state := AuthorityCompleted
		if pathv1.ActiveSideEffect(effect) {
			state = AuthorityActive
		} else if strings.Contains(effect.State, "cancel") {
			state = AuthorityCanceled
		}
		authority := AuthorityRecord{EpochID: epochID, LocalID: "effect." + effect.ID, ReservationID: reservationID, NodeID: nodeID, Kind: kind, State: state, DependsOn: []OwnerIdentity{}}
		var err error
		authority.Identity, err = authorityIdentity(publicRunID, authority)
		if err != nil {
			return nil, err
		}
		sealTerminal(&authority, "effect", effect.ID, effect.State)
		result = append(result, authority)
	}
	for id, intent := range aggregate.Routing.Propagation {
		state := AuthorityActive
		if intent.State == pathv1.PropagationComplete {
			state = AuthorityCompleted
		}
		authority := AuthorityRecord{EpochID: epochID, LocalID: "propagation." + id, ReservationID: intent.RootReservationID, NodeID: aggregate.Routing.Reservations[intent.RootReservationID].NodeID, Kind: AuthorityPropagation, State: state, DependsOn: []OwnerIdentity{}}
		var err error
		authority.Identity, err = authorityIdentity(publicRunID, authority)
		if err != nil {
			return nil, err
		}
		sealTerminal(&authority, "propagation", id, string(intent.State))
		result = append(result, authority)
	}
	sortAuthorities(result)
	return result, nil
}

func activeRuntimeDetachmentSets(aggregate pathv1.AggregateCheckpoint) map[pathv1.DetachmentID]bool {
	result := make(map[pathv1.DetachmentID]bool)
	for _, path := range aggregate.Routing.Paths {
		if path.DetachmentSetID == "" || path.State != pathv1.PathLive && path.State != pathv1.PathArrived {
			continue
		}
		for setID := path.DetachmentSetID; setID != ""; {
			set, ok := aggregate.Routing.DetachmentSets[setID]
			if !ok {
				break
			}
			result[set.DetachmentID] = true
			setID = set.ParentSetID
		}
	}
	return result
}

type runtimeRetryResolution struct {
	digest     string
	resolution pathv1.BlockResolution
}

func runtimeRetryResolutions(aggregate pathv1.AggregateCheckpoint, activationID pathv1.ActivationID, nodeID string) ([]runtimeRetryResolution, error) {
	result := make([]runtimeRetryResolution, 0, 2)
	for recordID, resolution := range aggregate.AdminResolutions {
		if resolution.NodeID != nodeID || resolution.Decision != "retry" {
			continue
		}
		matched := false
		for _, command := range aggregate.Commands {
			if command.Identity.Kind == pathv1.CommandPerformAttempt && command.Identity.SourceActivationID == activationID && command.Identity.Attempt == resolution.BlockedAttempt &&
				(command.State == pathv1.CommandObserved || command.State == pathv1.CommandReconciled) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		digest, err := pathv1.ValidateBlockResolution(resolution)
		if err != nil || aggregate.AdminRecords[recordID].ResolutionDigest != digest {
			return nil, fmt.Errorf("%w: retry resolution authority is invalid", ErrInvalid)
		}
		result = append(result, runtimeRetryResolution{digest: digest, resolution: resolution})
	}
	slices.SortFunc(result, func(a, b runtimeRetryResolution) int {
		if a.resolution.BlockedAttempt < b.resolution.BlockedAttempt {
			return -1
		}
		if a.resolution.BlockedAttempt > b.resolution.BlockedAttempt {
			return 1
		}
		return strings.Compare(a.digest, b.digest)
	})
	return result, nil
}

func pathAuthorityState(state pathv1.PathState) AuthorityState {
	switch state {
	case pathv1.PathLive:
		return AuthorityVerifiedUnclaimed
	case pathv1.PathFailed:
		return AuthorityFailed
	case pathv1.PathCanceled, pathv1.PathSkipped:
		return AuthorityCanceled
	default:
		return AuthorityCompleted
	}
}

func outcomePathAuthorityState(state pathv1.PathState) AuthorityState {
	switch state {
	case pathv1.PathLive, pathv1.PathArrived:
		return AuthorityActive
	case pathv1.PathFailed:
		return AuthorityFailed
	case pathv1.PathCanceled, pathv1.PathSkipped, pathv1.PathImpossible:
		return AuthorityCanceled
	default:
		return AuthorityCompleted
	}
}

func accessoryCommandState(state pathv1.CommandState) AuthorityState {
	if state.Active() {
		return AuthorityActive
	}
	if state == pathv1.CommandCanceled {
		return AuthorityCanceled
	}
	return AuthorityCompleted
}

func authorityKindForEffect(kind pathv1.SideEffectKind) AuthorityKind {
	switch kind {
	case pathv1.SideEffectWait:
		return AuthorityWait
	case pathv1.SideEffectTimer:
		return AuthorityTimer
	case pathv1.SideEffectContact:
		return AuthorityContact
	case pathv1.SideEffectObligation, pathv1.SideEffectBlock:
		return AuthorityObligation
	case pathv1.SideEffectAttempt:
		return AuthorityDispatchedSideEffect
	case pathv1.SideEffectCommand:
		return AuthorityCommand
	default:
		return ""
	}
}

func sealTerminal(authority *AuthorityRecord, domain, id, state string) {
	if authority == nil || !authority.State.terminal() {
		return
	}
	authority.TerminalRecordID, _ = digestValue("runtime-terminal/v1", struct{ Domain, ID, State string }{domain, id, state})
}
