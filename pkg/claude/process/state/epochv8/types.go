// Package epochv8 implements the pure schema-8 path-v1 template-epoch
// authority protocol. It deliberately owns no persistence, scheduling, or
// external-dispatch behavior.
package epochv8

import (
	"errors"

	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
)

const (
	StateSchemaVersion = 8
	Protocol           = "path_v1_epoch"
	Encoding           = uint32(1)

	MaxEpochs            = 1_024
	MaxHistoryEvents     = 4_096
	MaxRuntimeReceipts   = 32_768
	MaxAuthorities       = 100_000
	MaxAuthorityLinks    = 400_000
	MaxHandoffEntries    = 100_000
	MaxReasonDigestBytes = 64
	MaxCheckpointBytes   = 16 << 20
	MaxApplyPlanBytes    = 16 << 20
	MaxIdentifierBytes   = 1 << 10
	MaxBlockers          = 4_096
)

var (
	ErrInvalid            = errors.New("schema-8 epoch authority is invalid")
	ErrOverBudget         = errors.New("schema-8 epoch authority is over budget")
	ErrNonCanonical       = errors.New("schema-8 input is not canonical")
	ErrTerminalIdentity   = errors.New("schema-8 owner identity is terminal")
	ErrProtectedAuthority = errors.New("schema-8 protected authority remains active")
)

type EpochID string
type OwnerIdentity string

type Capability string

const (
	CapabilityFoundationV1  Capability = "foundation_v1"
	CapabilityParallelAllV1 Capability = "parallel_all_v1"
	CapabilityParallelAnyV1 Capability = "parallel_any_v1"
)

type Binding struct {
	Revision uint64 `json:"revision"`
	Digest   string `json:"digest"`
}

// RuntimeBinding identifies the exact immutable path-v1 runtime artifact.
// The only absent value is {0,""}.
type RuntimeBinding struct {
	Revision uint64 `json:"revision"`
	Digest   string `json:"digest"`
}

type GraphNode struct {
	ID                   string       `json:"id"`
	Type                 string       `json:"type"`
	Join                 string       `json:"join,omitempty"`
	SemanticDigest       string       `json:"semanticDigest"`
	RequiredCapabilities []Capability `json:"requiredCapabilities"`
}

type GraphEdge struct {
	From    string `json:"from"`
	Outcome string `json:"outcome"`
	To      string `json:"to"`
}

type EpochGraph struct {
	Nodes  []GraphNode `json:"nodes"`
	Edges  []GraphEdge `json:"edges"`
	Digest string      `json:"digest"`
}

type TemplateEpoch struct {
	ID                   EpochID      `json:"id"`
	Ordinal              uint64       `json:"ordinal"`
	PredecessorEpochID   EpochID      `json:"predecessorEpochId,omitempty"`
	TemplateRef          string       `json:"templateRef"`
	TemplateSourceDigest string       `json:"templateSourceDigest"`
	RequiredCapabilities []Capability `json:"requiredCapabilities"`
	Graph                EpochGraph   `json:"graph"`
}

type AuthorityKind string

const (
	AuthorityFrontier             AuthorityKind = "frontier"
	AuthorityOutcome              AuthorityKind = "outcome"
	AuthorityParallel             AuthorityKind = "parallel"
	AuthorityJoin                 AuthorityKind = "join"
	AuthorityPropagation          AuthorityKind = "propagation"
	AuthorityDetachment           AuthorityKind = "detachment"
	AuthorityRetry                AuthorityKind = "retry"
	AuthorityRollbackForward      AuthorityKind = "rollback_forward"
	AuthorityCommand              AuthorityKind = "command"
	AuthorityWait                 AuthorityKind = "wait"
	AuthorityTimer                AuthorityKind = "timer"
	AuthorityObligation           AuthorityKind = "obligation"
	AuthorityContact              AuthorityKind = "contact"
	AuthorityDispatchedSideEffect AuthorityKind = "dispatched_side_effect"
)

type AuthorityState string

const (
	AuthorityVerifiedUnclaimed AuthorityState = "verified_unclaimed"
	AuthorityClaimed           AuthorityState = "claimed"
	AuthorityActive            AuthorityState = "active"
	AuthorityCompleted         AuthorityState = "completed"
	AuthorityFailed            AuthorityState = "failed"
	AuthorityCanceled          AuthorityState = "canceled"
	AuthorityHandedOff         AuthorityState = "handed_off"
)

type AuthorityRecord struct {
	Identity         OwnerIdentity   `json:"identity"`
	EpochID          EpochID         `json:"epochId"`
	LocalID          string          `json:"localId"`
	ReservationID    string          `json:"reservationId"`
	NodeID           string          `json:"nodeId"`
	Kind             AuthorityKind   `json:"kind"`
	State            AuthorityState  `json:"state"`
	DependsOn        []OwnerIdentity `json:"dependsOn"`
	Successor        OwnerIdentity   `json:"successor,omitempty"`
	TerminalRecordID string          `json:"terminalRecordId,omitempty"`
}

// AuthoritySeed is an initialization-only local authority description.
// DependencyLocalIDs are resolved inside epoch zero and cannot name external
// or future authority.
type AuthoritySeed struct {
	LocalID            string
	ReservationID      string
	NodeID             string
	Kind               AuthorityKind
	State              AuthorityState
	DependencyLocalIDs []string
}

type InitializationAnchor struct {
	RunID              string            `json:"runId"`
	Capabilities       []Capability      `json:"capabilities"`
	OriginalEpoch      TemplateEpoch     `json:"originalEpoch"`
	InitialAuthorities []AuthorityRecord `json:"initialAuthorities"`
	RuntimeBinding     RuntimeBinding    `json:"runtimeBinding,omitzero"`
	Digest             string            `json:"digest"`
}

type Diff struct {
	BeforeTemplateRef  string      `json:"beforeTemplateRef"`
	AfterTemplateRef   string      `json:"afterTemplateRef"`
	BeforeSourceDigest string      `json:"beforeSourceDigest"`
	AfterSourceDigest  string      `json:"afterSourceDigest"`
	AddedNodes         []string    `json:"addedNodes"`
	RemovedNodes       []string    `json:"removedNodes"`
	ChangedNodes       []string    `json:"changedNodes"`
	AddedEdges         []GraphEdge `json:"addedEdges"`
	RemovedEdges       []GraphEdge `json:"removedEdges"`
	Digest             string      `json:"digest"`
}

type HandoffAction string

const (
	HandoffRetain   HandoffAction = "retain_owner_epoch"
	HandoffTransfer HandoffAction = "transfer_verified_unclaimed"
)

// HandoffDirective is caller intent. Preview canonicalizes directive order;
// the sealed plan and checkpoint always carry a complete sorted HandoffSet.
type HandoffDirective struct {
	Source              OwnerIdentity
	Action              HandoffAction
	TargetLocalID       string
	TargetReservationID string
	TargetNodeID        string
}

type Handoff struct {
	ID     string           `json:"id"`
	Source OwnerIdentity    `json:"source"`
	Action HandoffAction    `json:"action"`
	Target *AuthorityRecord `json:"target,omitempty"`
}

type BlockerCode string

const (
	BlockerStaleBinding          BlockerCode = "stale_binding"
	BlockerHandoffMissing        BlockerCode = "handoff_missing"
	BlockerHandoffDuplicate      BlockerCode = "handoff_duplicate"
	BlockerHandoffUnknown        BlockerCode = "handoff_unknown"
	BlockerClaimed               BlockerCode = "claimed_work"
	BlockerActiveCommand         BlockerCode = "active_command"
	BlockerActiveWait            BlockerCode = "active_wait"
	BlockerActiveTimer           BlockerCode = "active_timer"
	BlockerActiveObligation      BlockerCode = "active_obligation"
	BlockerActiveContact         BlockerCode = "active_contact"
	BlockerDispatchedSideEffect  BlockerCode = "dispatched_side_effect"
	BlockerActiveOutcome         BlockerCode = "active_outcome"
	BlockerActiveParallel        BlockerCode = "active_parallel"
	BlockerActiveJoin            BlockerCode = "active_join"
	BlockerActivePropagation     BlockerCode = "active_propagation"
	BlockerActiveDetachment      BlockerCode = "active_detachment"
	BlockerActiveRetry           BlockerCode = "active_retry"
	BlockerActiveRollbackForward BlockerCode = "active_rollback_forward"
	BlockerActiveAuthority       BlockerCode = "active_authority"
	BlockerNotTransferable       BlockerCode = "not_verified_unclaimed_frontier"
)

type Blocker struct {
	Code        BlockerCode   `json:"code"`
	AuthorityID OwnerIdentity `json:"authorityId,omitempty"`
}

type applyCore struct {
	RunID            string            `json:"runId"`
	BaseBinding      Binding           `json:"baseBinding"`
	PredecessorEpoch EpochID           `json:"predecessorEpoch"`
	CandidateEpoch   TemplateEpoch     `json:"candidateEpoch"`
	ReasonDigest     string            `json:"reasonDigest,omitempty"`
	Diff             Diff              `json:"diff"`
	Protected        []AuthorityRecord `json:"protected"`
	ProtectedDigest  string            `json:"protectedDigest"`
	HandoffSet       []Handoff         `json:"handoffSet"`
	HandoffSetDigest string            `json:"handoffSetDigest"`
	ProposalDigest   string            `json:"proposalDigest"`
}

// ApplyPlan is sealed by PreviewApply and can only be serialized through the
// strict plan encoder. Its fields are intentionally not mutation bags.
type ApplyPlan struct{ core applyCore }

type ApplyRecord struct {
	applyCore
	RecordDigest string `json:"recordDigest"`
}

type FinishResult string

const (
	FinishCompleted FinishResult = "completed"
	FinishFailed    FinishResult = "failed"
	FinishCanceled  FinishResult = "canceled"
)

type FinishClaim struct {
	BaseBinding    Binding
	Identity       OwnerIdentity
	Result         FinishResult
	EvidenceDigest string
}

type FinishReceipt struct {
	ID             string        `json:"id"`
	BaseBinding    Binding       `json:"baseBinding"`
	Identity       OwnerIdentity `json:"identity"`
	OwnerEpochID   EpochID       `json:"ownerEpochId"`
	Result         FinishResult  `json:"result"`
	EvidenceDigest string        `json:"evidenceDigest"`
}

type HistoryKind string

const (
	HistoryApply         HistoryKind = "apply"
	HistoryFinishClaimed HistoryKind = "finish_claimed"
	HistoryRuntime       HistoryKind = "runtime"
)

type RuntimeTransitionKind string

type RuntimeApplyPreflight string

const (
	RuntimeApplyTransferReady RuntimeApplyPreflight = "transfer_ready"
	RuntimeApplyRetainReady   RuntimeApplyPreflight = "retain_ready"
	RuntimeApplyRefused       RuntimeApplyPreflight = "refused"
)

const (
	RuntimeAttachGenesis RuntimeTransitionKind = "attach_genesis"
	RuntimeAdvanceHead   RuntimeTransitionKind = "advance_head"
	RuntimeClaimExternal RuntimeTransitionKind = "claim_external"
	RuntimeFinishClaimed RuntimeTransitionKind = "finish_claimed_head"
	RuntimeSettlement    RuntimeTransitionKind = "audited_settlement"
	RuntimeApplyRetain   RuntimeTransitionKind = "apply_retain_head"
	RuntimeApplyTransfer RuntimeTransitionKind = "apply_transfer_head"
)

// RuntimeReceipt is the complete typed authority receipt for one indivisible
// owner-runtime event. Before/After are canonical complete projections; the
// typed witness makes them independently replayable after artifact GC.
type RuntimeReceipt struct {
	ID                   string                          `json:"id"`
	Kind                 RuntimeTransitionKind           `json:"kind"`
	PathTransitionKind   string                          `json:"pathTransitionKind,omitempty"`
	Owner                OwnerIdentity                   `json:"owner"`
	EpochID              EpochID                         `json:"epochId"`
	TemplateSourceDigest string                          `json:"templateSourceDigest"`
	PreRuntime           RuntimeBinding                  `json:"preRuntime"`
	PostRuntime          RuntimeBinding                  `json:"postRuntime"`
	Before               []AuthorityRecord               `json:"before"`
	After                []AuthorityRecord               `json:"after"`
	EvidenceDigest       string                          `json:"evidenceDigest,omitempty"`
	Decision             string                          `json:"decision,omitempty"`
	Actor                string                          `json:"actor,omitempty"`
	Timestamp            string                          `json:"timestamp,omitempty"`
	NodeID               string                          `json:"nodeId,omitempty"`
	BlockedAttempt       uint64                          `json:"blockedAttempt,omitempty"`
	Reason               string                          `json:"reason,omitempty"`
	EvidenceRef          string                          `json:"evidenceRef,omitempty"`
	ResolutionDigest     string                          `json:"resolutionDigest,omitempty"`
	GenesisWitness       *pathv1.RuntimeGenesisWitnessV1 `json:"genesisWitness,omitempty"`
	ExecutionWitness     *pathv1.ExecutionWitnessV1      `json:"executionWitness,omitempty"`
}

type HistoryEvent struct {
	Revision uint64          `json:"revision"`
	Kind     HistoryKind     `json:"kind"`
	Apply    *ApplyRecord    `json:"apply,omitempty"`
	Finish   *FinishReceipt  `json:"finish,omitempty"`
	Runtime  *RuntimeReceipt `json:"runtime,omitempty"`
	Digest   string          `json:"digest"`
}

type checkpointWire struct {
	StateSchemaVersion int                  `json:"stateSchemaVersion"`
	Protocol           string               `json:"protocol"`
	Encoding           uint32               `json:"encoding"`
	Anchor             InitializationAnchor `json:"anchor"`
	CurrentEpochID     EpochID              `json:"currentEpochId"`
	Epochs             []TemplateEpoch      `json:"epochs"`
	History            []HistoryEvent       `json:"history"`
	Authorities        []AuthorityRecord    `json:"authorities"`
	RuntimeBinding     RuntimeBinding       `json:"runtimeBinding,omitzero"`
	Digest             string               `json:"digest"`
}

// CheckpointV8 is an opaque immutable value. All exported transitions clone
// their input and return a new verified checkpoint.
type CheckpointV8 struct{ wire checkpointWire }

type CheckpointView struct {
	Binding              Binding
	RunID                string
	OriginalEpoch        EpochID
	CurrentEpoch         EpochID
	Epochs               []TemplateEpoch
	Authorities          []AuthorityRecord
	ProtectedAuthorities []AuthorityRecord
	History              []HistoryEvent
	RuntimeBinding       RuntimeBinding
}

type ApplyDraft struct {
	BaseBinding  Binding
	Candidate    *TemplateCandidate
	ReasonDigest string
	Handoffs     []HandoffDirective
}

type PreviewResult struct {
	Plan     *ApplyPlan
	Diff     Diff
	Blockers []Blocker
}

type Disposition string

const (
	DispositionApplied  Disposition = "applied"
	DispositionReplayed Disposition = "replayed"
	DispositionStale    Disposition = "stale"
)

type TransitionResult struct {
	Checkpoint  *CheckpointV8
	Disposition Disposition
	Binding     Binding
}

type OverBudgetError struct {
	Limit          string
	Value, Maximum int
}

func (e *OverBudgetError) Error() string {
	return "schema-8 " + e.Limit + " is over budget"
}

func (e *OverBudgetError) Unwrap() error { return ErrOverBudget }
