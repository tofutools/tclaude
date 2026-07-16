package pathv1

import (
	"encoding/json"
	"errors"
	"strings"
)

var (
	ErrMutationInvalid      = errors.New("path-v1 mutation plan is invalid")
	ErrMutationInconsistent = errors.New("path-v1 mutation replay state is inconsistent")
)

// CheckpointBinding identifies the coherently loaded checkpoint generation
// against which a dormant path-v1 mutation command was planned.
type CheckpointBinding struct {
	Generation uint64 `json:"generation"`
	Digest     string `json:"digest"`
}

// MutationReplayView is the complete pure input to mutation validation and
// replay. It has no state-v6, planner, reducer, executor, or store wiring.
type MutationReplayView struct {
	Aggregate  AggregateView
	Checkpoint CheckpointBinding
}

type MutationRecordKind string

const (
	// MutationCommandPlaceholder breaks the otherwise circular dependency
	// between a raw-payload PlanDigest and command-owned post-record bytes. It
	// is materialized as the derived command ID before validation/application.
	MutationCommandPlaceholder = "$commandId"

	MutationPath             MutationRecordKind = "path"
	MutationScope            MutationRecordKind = "scope"
	MutationReservation      MutationRecordKind = "reservation"
	MutationActivation       MutationRecordKind = "activation"
	MutationCandidateClosure MutationRecordKind = "candidate_closure"
	MutationCauseRecord      MutationRecordKind = "cause_record"
	MutationCauseSet         MutationRecordKind = "cause_set"
	MutationDetachmentSet    MutationRecordKind = "detachment_set"
	MutationDetachment       MutationRecordKind = "detachment"
	MutationPropagation      MutationRecordKind = "propagation"
)

// RecordMutation stores exact canonical pre/post record bytes. Exactly one of
// Before and After is empty for a create/delete; neither is empty for update.
type RecordMutation struct {
	Kind   MutationRecordKind `json:"kind"`
	Key    string             `json:"key"`
	Before json.RawMessage    `json:"before,omitempty"`
	After  json.RawMessage    `json:"after,omitempty"`
}

// MutationBatch is the indivisible routing-record portion of one aggregate
// reducer event. Validate is mandatory before replay classification.
type MutationBatch struct {
	EventSeq     int64            `json:"eventSeq"`
	LogEntries   int              `json:"logEntries"`
	BeforeDigest string           `json:"beforeDigest"`
	AfterDigest  string           `json:"afterDigest"`
	Mutations    []RecordMutation `json:"mutations"`
}

type RoutePathsPlan struct {
	SettlementCommandID string       `json:"settlementCommandId"`
	SourceActivationID  ActivationID `json:"sourceActivationId"`
	SourceGeneration    uint64       `json:"sourceGeneration"`
	SourcePathID        PathID       `json:"sourcePathId"`
	Attempt             uint64       `json:"attempt"`
	CauseDigest         CauseDigest  `json:"causeDigest"`
	ResultCode          string       `json:"resultCode"`
	// SelectedEdgeIDs is populated only for parallel fan-out and preserves
	// canonical EdgeKey tuple order. ProducedPathIDs remains opaque-ID sorted
	// record order and must not be used to choose materialization order.
	SelectedEdgeIDs []EdgeID      `json:"selectedEdgeIds,omitempty"`
	ProducedPathIDs []PathID      `json:"producedPathIds"`
	Batch           MutationBatch `json:"batch"`
}

type ActivateGenerationPlan struct {
	ReservationID          ReservationID        `json:"reservationId"`
	Generation             uint64               `json:"generation"`
	InputDigest            string               `json:"inputDigest"`
	CauseDigest            CauseDigest          `json:"causeDigest"`
	JoinPolicy             JoinPolicy           `json:"joinPolicy"`
	InputPathIDs           []PathID             `json:"inputPathIds"`
	WinnerPathID           PathID               `json:"winnerPathId,omitempty"`
	LosingCandidateIDs     []CandidateID        `json:"losingCandidateIds,omitempty"`
	PreArrivedLoserPathIDs []PathID             `json:"preArrivedLoserPathIds,omitempty"`
	Candidates             []CandidateRecord    `json:"candidates"`
	PossibleSlots          []PossibleSlotRecord `json:"possibleSlots"`
	Intents                []PropagationIntent  `json:"intents,omitempty"`
	Batch                  MutationBatch        `json:"batch"`
}

type PropagateClosurePlan struct {
	SourcePathID        PathID              `json:"sourcePathId,omitempty"`
	TargetReservationID ReservationID       `json:"targetReservationId"`
	TargetGeneration    uint64              `json:"targetGeneration"`
	InputDigest         string              `json:"inputDigest"`
	CauseDigest         CauseDigest         `json:"causeDigest"`
	RootReservationID   ReservationID       `json:"rootReservationId"`
	RootCandidateID     CandidateID         `json:"rootCandidateId"`
	RootCauseDigest     CauseDigest         `json:"rootCauseDigest"`
	Intents             []PropagationIntent `json:"intents"`
	Batch               MutationBatch       `json:"batch"`
}

type SettleDetachedSinkPlan struct {
	SettlementCommandID  string          `json:"settlementCommandId"`
	SourceActivationID   ActivationID    `json:"sourceActivationId"`
	SourceGeneration     uint64          `json:"sourceGeneration"`
	SourceAttempt        uint64          `json:"sourceAttempt"`
	SettlementResultCode string          `json:"settlementResultCode"`
	SourcePathID         PathID          `json:"sourcePathId"`
	ReservationID        ReservationID   `json:"reservationId"`
	Generation           uint64          `json:"generation"`
	DetachmentSetID      DetachmentSetID `json:"detachmentSetId"`
	DetachmentID         DetachmentID    `json:"detachmentId"`
	CauseDigest          CauseDigest     `json:"causeDigest,omitempty"`
	ResultCode           string          `json:"resultCode"`
	Batch                MutationBatch   `json:"batch"`
}

// InternDetachmentSetPlan owns one immutable, path-neutral set-node create.
// Repeating this bounded primitive prepares a reducer activation without
// expanding its canonical mutation formula.
type InternDetachmentSetPlan struct {
	ReservationID ReservationID       `json:"reservationId"`
	Generation    uint64              `json:"generation"`
	SourcePathID  PathID              `json:"sourcePathId"`
	Record        DetachmentSetRecord `json:"record"`
	Batch         MutationBatch       `json:"batch"`
}

type mutationPayload[T any] struct {
	TemplateRef        string            `json:"templateRef"`
	TemplateSourceHash string            `json:"templateSourceHash"`
	Checkpoint         CheckpointBinding `json:"checkpoint"`
	Plan               T                 `json:"plan"`
}

type ReplayDisposition string

const (
	ReplayApplied        ReplayDisposition = "applied"
	ReplayAlreadyApplied ReplayDisposition = "already_applied"
)

func exactSettlementResult(resultCode string, exclusive bool) (string, bool) {
	// Exclusive routes own exactly one protocol prefix; all other routes own
	// the bare settlement result. Keep case exact and reject a valid payload
	// whose result uses the form belonging to a different transition kind.
	if exclusive {
		outcome, ok := strings.CutPrefix(resultCode, "exclusive/")
		return outcome, ok && outcome != ""
	}
	if resultCode == "" || strings.Contains(resultCode, "/") {
		return "", false
	}
	return resultCode, true
}

type MutationReplayResult struct {
	Routing     RoutingState
	Disposition ReplayDisposition
}

type CompletionBasis struct {
	SelfCommandID         string      `json:"selfCommandId"`
	BasisRunStatus        string      `json:"basisRunStatus"`
	BasisLastLogSeq       uint64      `json:"basisLastLogSeq"`
	BasisLogChecksum      string      `json:"basisLogChecksum"`
	CheckpointDigest      string      `json:"checkpointDigest"`
	PathFoldDigest        string      `json:"pathFoldDigest"`
	ReservationFoldDigest string      `json:"reservationFoldDigest"`
	PropagationFoldDigest string      `json:"propagationFoldDigest"`
	SideEffectFoldDigest  string      `json:"sideEffectFoldDigest"`
	TerminalCauseDigest   CauseDigest `json:"terminalCauseDigest"`
	ActiveCommandDigest   string      `json:"activeCommandDigest"`
	AggregateDigest       string      `json:"aggregateDigest"`
	Result                string      `json:"result"`
}

type CompleteRunCommandPayload struct {
	TemplateRef        string            `json:"templateRef"`
	TemplateSourceHash string            `json:"templateSourceHash"`
	Checkpoint         CheckpointBinding `json:"checkpoint"`
	Basis              CompletionBasis   `json:"basis"`
}

// CompletionReplayView is the pure complete_run_v1 recovery input. The raw
// checkpoint remains outside RoutingState because this layer is dormant and
// schema v6 must not gain path-v1 fields.
type CompletionReplayView struct {
	Aggregate      AggregateView
	Checkpoint     CheckpointBinding
	CheckpointJSON json.RawMessage
	RunStatus      string
	LastLogSeq     uint64
	LogChecksum    string
}

type CompletionRecoveryPhase string

const (
	CompletionReadyToClaim   CompletionRecoveryPhase = "ready_to_claim"
	CompletionReadyToObserve CompletionRecoveryPhase = "ready_to_observe"
	CompletionRecovered      CompletionRecoveryPhase = "complete"
)

type CompletionRecovery struct {
	Phase   CompletionRecoveryPhase
	Command CommandRecord
	Result  string
}
