// Package pathv1 contains the schema-independent durable path-v1 routing
// protocol used by the schema-7 foundation and parallel-all executor.
package pathv1

import "encoding/json"

type (
	PathID              = string
	ScopeID             = string
	ReservationID       = string
	CandidateID         = string
	PossibleSlotID      = string
	ActivationID        = string
	EdgeID              = string
	CauseID             = string
	CauseDigest         = string
	CandidateClosureKey = string
	CandidateClosureID  = string
	DetachmentKey       = string
	DetachmentID        = string
	DetachmentSetID     = string
	PropagationIntentID = string
)

const (
	Protocol = "path_v1"
	Encoding = uint32(1)
)

type RoutingState struct {
	Protocol          string                                    `json:"protocol"`
	Encoding          uint32                                    `json:"encoding"`
	Paths             map[PathID]PathRecord                     `json:"paths"`
	Scopes            map[ScopeID]ScopeRecord                   `json:"scopes"`
	Reservations      map[ReservationID]ActivationReservation   `json:"reservations"`
	Activations       map[ActivationID]ActivationRecord         `json:"activations"`
	CandidateClosures map[CandidateClosureKey]CandidateClosure  `json:"candidateClosures"`
	CauseRecords      map[CauseID]CauseRecord                   `json:"causeRecords"`
	CauseSets         map[CauseDigest]CauseSetRecord            `json:"causeSets"`
	DetachmentSets    map[DetachmentSetID]DetachmentSetRecord   `json:"detachmentSets"`
	Detachments       map[DetachmentKey]DetachmentRecord        `json:"detachments"`
	Propagation       map[PropagationIntentID]PropagationIntent `json:"propagation"`
}

type ActivationRef struct {
	ID         ActivationID `json:"id"`
	Generation uint64       `json:"generation"`
}

type EdgeKey struct {
	TemplateRef string `json:"templateRef"`
	ID          EdgeID `json:"id"`
	FromNodeID  string `json:"fromNodeId"`
	Outcome     string `json:"outcome"`
	ToNodeID    string `json:"toNodeId"`
}

type CandidateClosureKeyRecord struct {
	ID            CandidateClosureKey `json:"id"`
	ReservationID ReservationID       `json:"reservationId"`
	CandidateID   CandidateID         `json:"candidateId"`
}

type CandidateRecord struct {
	ID              CandidateID      `json:"id"`
	Kind            CandidateKind    `json:"kind"`
	MemberID        string           `json:"memberId"`
	PossibleSlotIDs []PossibleSlotID `json:"possibleSlotIds"`
}

type PossibleSlotRecord struct {
	ID                 PossibleSlotID `json:"id"`
	ReservationID      ReservationID  `json:"reservationId"`
	CandidateID        CandidateID    `json:"candidateId"`
	SourceNodeID       string         `json:"sourceNodeId"`
	SourceEdgeID       EdgeID         `json:"sourceEdgeId"`
	SourceScopeID      ScopeID        `json:"sourceScopeId"`
	SourceBranchEdgeID EdgeID         `json:"sourceBranchEdgeId"`
	Generation         uint64         `json:"generation"`
}

type PathRecord struct {
	ID                    PathID                  `json:"id"`
	Kind                  PathKind                `json:"kind"`
	State                 PathState               `json:"state"`
	ParentPathID          PathID                  `json:"parentPathId,omitempty"`
	ProducedPathIDs       []PathID                `json:"producedPathIds,omitempty"`
	SourceActivation      ActivationRef           `json:"sourceActivation"`
	Edge                  *EdgeKey                `json:"edge,omitempty"`
	TargetReservationID   ReservationID           `json:"targetReservationId,omitempty"`
	CandidateID           CandidateID             `json:"candidateId,omitempty"`
	ScopeID               ScopeID                 `json:"scopeId"`
	BranchEdgeID          EdgeID                  `json:"branchEdgeId,omitempty"`
	CandidateLineage      []CandidateLineageFrame `json:"candidateLineage,omitempty"`
	CandidateLineageID    string                  `json:"candidateLineageId,omitempty"`
	LineageDepth          uint32                  `json:"lineageDepth,omitempty"`
	ArrivalID             string                  `json:"arrivalId,omitempty"`
	ArrivedSeq            int64                   `json:"arrivedSeq,omitempty"`
	ConsumedBy            *ActivationRef          `json:"consumedBy,omitempty"`
	Disposition           *DispositionReceipt     `json:"disposition,omitempty"`
	DetachmentSetID       DetachmentSetID         `json:"detachmentSetId,omitempty"`
	DetachedSink          *DetachedSinkReceipt    `json:"detachedSink,omitempty"`
	ImpossibleCauseDigest CauseDigest             `json:"impossibleCauseDigest,omitempty"`
	TerminalCauseID       CauseID                 `json:"terminalCauseId,omitempty"`
	CreatedSeq            int64                   `json:"createdSeq"`
	UpdatedSeq            int64                   `json:"updatedSeq"`
}

type CandidateLineageFrame struct {
	ID              string        `json:"id"`
	ParentLineageID string        `json:"parentLineageId,omitempty"`
	ReservationID   ReservationID `json:"reservationId"`
	CandidateID     CandidateID   `json:"candidateId"`
}

type ScopeRecord struct {
	ID                    ScopeID          `json:"id"`
	RunID                 string           `json:"runId"`
	ParentScopeID         ScopeID          `json:"parentScopeId,omitempty"`
	ParentBranchEdgeID    EdgeID           `json:"parentBranchEdgeId,omitempty"`
	ForkActivationID      ActivationID     `json:"forkActivationId,omitempty"`
	ForkOutputPathID      PathID           `json:"forkOutputPathId,omitempty"`
	Generation            uint64           `json:"generation"`
	ExpectedBranchEdgeIDs []EdgeID         `json:"expectedBranchEdgeIds"`
	JoinNodeID            string           `json:"joinNodeId,omitempty"`
	JoinReservationID     ReservationID    `json:"joinReservationId,omitempty"`
	State                 ScopeState       `json:"state"`
	CloseReason           ScopeCloseReason `json:"closeReason,omitempty"`
	ClosedByCommandID     string           `json:"closedByCommandId,omitempty"`
	EventSeq              int64            `json:"eventSeq"`
}

type ActivationReservation struct {
	ID             ReservationID        `json:"id"`
	RunID          string               `json:"runId"`
	NodeID         string               `json:"nodeId"`
	ScopeID        ScopeID              `json:"scopeId"`
	BranchEdgeID   EdgeID               `json:"branchEdgeId,omitempty"`
	Generation     uint64               `json:"generation"`
	JoinPolicy     JoinPolicy           `json:"joinPolicy"`
	IsReducing     bool                 `json:"isReducing,omitempty"`
	ReducesScopeID ScopeID              `json:"reducesScopeId,omitempty"`
	Candidates     []CandidateRecord    `json:"candidates"`
	PossibleSlots  []PossibleSlotRecord `json:"possibleSlots"`
	State          ReservationState     `json:"state"`
	Activation     *ActivationRef       `json:"activation,omitempty"`
	CloseReceipt   *ActivationReceipt   `json:"closeReceipt,omitempty"`
	ClosedReason   string               `json:"closedReason,omitempty"`
	CauseDigest    CauseDigest          `json:"causeDigest,omitempty"`
	CommandID      string               `json:"commandId,omitempty"`
	EventSeq       int64                `json:"eventSeq"`
}

type ActivationRecord struct {
	ID             ActivationID      `json:"id"`
	RunID          string            `json:"runId"`
	Ref            ActivationRef     `json:"ref"`
	ReservationID  ReservationID     `json:"reservationId"`
	InputPathIDs   []PathID          `json:"inputPathIds"`
	InputSetDigest string            `json:"inputSetDigest"`
	OutputPathID   PathID            `json:"outputPathId,omitempty"`
	Receipt        ActivationReceipt `json:"receipt"`
	CommandID      string            `json:"commandId"`
	EventSeq       int64             `json:"eventSeq"`
}

type ActivationReceipt struct {
	ID             string                  `json:"id"`
	ActivationID   ActivationID            `json:"activationId,omitempty"`
	ReservationID  ReservationID           `json:"reservationId"`
	InputSetDigest string                  `json:"inputSetDigest"`
	OutputPathID   PathID                  `json:"outputPathId,omitempty"`
	ScopeID        ScopeID                 `json:"scopeId"`
	BranchEdgeID   EdgeID                  `json:"branchEdgeId,omitempty"`
	ReducedScopeID ScopeID                 `json:"reducedScopeId,omitempty"`
	JoinPolicy     JoinPolicy              `json:"joinPolicy"`
	Result         ActivationReceiptResult `json:"result"`
	CauseDigest    CauseDigest             `json:"causeDigest,omitempty"`
	CommandID      string                  `json:"commandId"`
	EventSeq       int64                   `json:"eventSeq"`
}

type CauseRecord struct {
	ID                 CauseID      `json:"id"`
	SourcePathID       PathID       `json:"sourcePathId,omitempty"`
	TerminalKind       TerminalKind `json:"terminalKind"`
	DispositionReason  string       `json:"dispositionReason"`
	SourceActivationID ActivationID `json:"sourceActivationId,omitempty"`
	SourceCommandID    string       `json:"sourceCommandId,omitempty"`
	AdminRecordID      string       `json:"adminRecordId,omitempty"`
	EventSeq           int64        `json:"eventSeq"`
}

type CauseSetRecord struct {
	Digest   CauseDigest `json:"digest"`
	CauseIDs []CauseID   `json:"causeIds"`
}

type CandidateClosure struct {
	ID           CandidateClosureID        `json:"id"`
	Key          CandidateClosureKeyRecord `json:"key"`
	TerminalKind TerminalKind              `json:"terminalKind"`
	CauseDigest  CauseDigest               `json:"causeDigest"`
	CommandID    string                    `json:"commandId"`
	EventSeq     int64                     `json:"eventSeq"`
}

type DispositionReceipt struct {
	ID            string    `json:"id"`
	PathID        PathID    `json:"pathId"`
	FromState     PathState `json:"fromState"`
	ToState       PathState `json:"toState"`
	ReasonCode    string    `json:"reasonCode"`
	CommandID     string    `json:"commandId,omitempty"`
	AdminRecordID string    `json:"adminRecordId,omitempty"`
	EventSeq      int64     `json:"eventSeq"`
}

type DetachmentKeyRecord struct {
	ID            DetachmentKey `json:"id"`
	ReservationID ReservationID `json:"reservationId"`
	CandidateID   CandidateID   `json:"candidateId"`
}

type DetachmentRecord struct {
	ID             DetachmentID        `json:"id"`
	Key            DetachmentKeyRecord `json:"key"`
	ReservationID  ReservationID       `json:"reservationId"`
	CandidateID    CandidateID         `json:"candidateId"`
	WinnerPathID   PathID              `json:"winnerPathId"`
	JoinActivation ActivationRef       `json:"joinActivation"`
	ReasonCode     string              `json:"reasonCode"`
	CommandID      string              `json:"commandId"`
	ActivatedSeq   int64               `json:"activatedSeq"`
	EventSeq       int64               `json:"eventSeq"`
	Actor          string              `json:"actor"`
	AdminRecordID  string              `json:"adminRecordId,omitempty"`
}

type DetachmentSetRecord struct {
	ID           DetachmentSetID `json:"id"`
	ParentSetID  DetachmentSetID `json:"parentSetId,omitempty"`
	DetachmentID DetachmentID    `json:"detachmentId"`
}

type DetachedSinkReceipt struct {
	DetachmentID DetachmentID `json:"detachmentId"`
	CommandID    string       `json:"commandId"`
	ReasonCode   string       `json:"reasonCode"`
	EventSeq     int64        `json:"eventSeq"`
}

type PropagationIntent struct {
	ID                PropagationIntentID   `json:"id"`
	RootReservationID ReservationID         `json:"rootReservationId"`
	RootCandidateID   CandidateID           `json:"rootCandidateId"`
	RootCauseDigest   CauseDigest           `json:"rootCauseDigest"`
	Shard             uint32                `json:"shard"`
	Cursor            uint32                `json:"cursor"`
	Frontier          []CandidateClosureKey `json:"frontier"`
	PlanDigest        string                `json:"planDigest"`
	State             PropagationState      `json:"state"`
	CommandID         string                `json:"commandId"`
	EventSeq          int64                 `json:"eventSeq"`
}

// SideEffectIdentity is embedded by every non-command path-v1 side-effect
// record. Unused tuple members remain canonical empty/zero.
type SideEffectIdentity struct {
	Kind            SideEffectKind `json:"kind"`
	ID              string         `json:"id"`
	RunID           string         `json:"runId"`
	ActivationID    ActivationID   `json:"activationId,omitempty"`
	Attempt         uint64         `json:"attempt,omitempty"`
	BlockedAttempt  uint64         `json:"blockedAttempt,omitempty"`
	WaitKind        string         `json:"waitKind,omitempty"`
	SourceCommandID string         `json:"sourceCommandId,omitempty"`
	Assignee        string         `json:"assignee,omitempty"`
	State           string         `json:"state"`
}

type CommandIdentity struct {
	RunID               string        `json:"runId"`
	Kind                CommandKindV1 `json:"kind"`
	PayloadSchema       uint64        `json:"payloadSchema"`
	SourceActivationID  ActivationID  `json:"sourceActivationId,omitempty"`
	SourceGeneration    uint64        `json:"sourceGeneration,omitempty"`
	SourcePathID        PathID        `json:"sourcePathId,omitempty"`
	TargetReservationID ReservationID `json:"targetReservationId,omitempty"`
	TargetGeneration    uint64        `json:"targetGeneration,omitempty"`
	Attempt             uint64        `json:"attempt,omitempty"`
	InputDigest         string        `json:"inputDigest,omitempty"`
	CauseDigest         string        `json:"causeDigest,omitempty"`
	PlanDigest          string        `json:"planDigest,omitempty"`
	ResultCode          string        `json:"resultCode,omitempty"`
}

type CommandRecord struct {
	ID             string          `json:"id"`
	IdempotencyKey string          `json:"idempotencyKey"`
	Identity       CommandIdentity `json:"identity"`
	Payload        json.RawMessage `json:"payload"`
	PayloadHash    string          `json:"payloadHash"`
	State          CommandState    `json:"state"`
}

type PathV1AdminRecord struct {
	ID                 string `json:"id"`
	RunID              string `json:"runId"`
	EventSeq           int64  `json:"eventSeq"`
	AdminType          string `json:"adminType"`
	Actor              string `json:"actor"`
	ReasonCode         string `json:"reasonCode"`
	EvidenceRef        string `json:"evidenceRef,omitempty"`
	Timestamp          string `json:"timestamp,omitempty"`
	ResolutionDigest   string `json:"resolutionDigest,omitempty"`
	OriginalArrayIndex uint64 `json:"originalArrayIndex,omitempty"`
}

type BlockResolution struct {
	NodeID         string `json:"nodeId"`
	BlockedAttempt uint64 `json:"blockedAttempt"`
	Decision       string `json:"decision"`
	Actor          string `json:"actor"`
	Reason         string `json:"reason"`
	EvidenceRef    string `json:"evidenceRef"`
	Timestamp      string `json:"timestamp"`
}

type CandidateFoldEntry struct {
	CandidateID     CandidateID
	FoldKind        string
	PathOrClosureID string
}

type PathFoldEntry struct {
	PathID     PathID
	State      PathState
	UpdatedSeq uint64
}
type ReservationFoldEntry struct {
	ReservationID ReservationID
	State         ReservationState
	EventSeq      uint64
}
type PropagationFoldEntry struct {
	IntentID PropagationIntentID
	State    PropagationState
	Cursor   uint64
}
type SideEffectFoldEntry struct {
	Kind      SideEffectKind
	ID, State string
}
