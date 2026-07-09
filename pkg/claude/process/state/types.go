package state

import (
	"encoding/json"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

const StateSchemaVersion = 2

type RunStatus string

const (
	RunStatusPending      RunStatus = "pending"
	RunStatusRunning      RunStatus = "running"
	RunStatusPaused       RunStatus = "paused"
	RunStatusBlocked      RunStatus = "blocked"
	RunStatusCompleted    RunStatus = "completed"
	RunStatusFailed       RunStatus = "failed"
	RunStatusCanceled     RunStatus = "canceled"
	RunStatusInconsistent RunStatus = "inconsistent"
	RunStatusDirty        RunStatus = "dirty"
)

type PauseKind string

const (
	PauseKindRateLimited    PauseKind = "rate_limited"
	PauseKindNeedsReconcile PauseKind = "needs_reconcile"
)

type NodeStatus string

const (
	NodeStatusPending        NodeStatus = "pending"
	NodeStatusReady          NodeStatus = "ready"
	NodeStatusRunning        NodeStatus = "running"
	NodeStatusWaitingHuman   NodeStatus = "waiting_human"
	NodeStatusWaitingAgent   NodeStatus = "waiting_agent"
	NodeStatusWaitingProgram NodeStatus = "waiting_program"
	NodeStatusWaitingTimer   NodeStatus = "waiting_timer"
	NodeStatusWaitingSignal  NodeStatus = "waiting_signal"
	NodeStatusBlocked        NodeStatus = "blocked"
	NodeStatusCompleted      NodeStatus = "completed"
	NodeStatusFailed         NodeStatus = "failed"
	// Skipped is reserved for manual advancement/repair flows in later process
	// PRs; the phase-1 reducer validates but does not currently emit it.
	NodeStatusSkipped NodeStatus = "skipped"
)

type CommandStatus string

const (
	CommandStatusIssued   CommandStatus = "issued"
	CommandStatusObserved CommandStatus = "observed"
	// Reconciled and canceled are reserved for the engine/store layer that will
	// own command lifecycle cleanup after this schema-only package.
	CommandStatusReconciled CommandStatus = "reconciled"
	CommandStatusCanceled   CommandStatus = "canceled"
)

type CommandKind string

const (
	CommandKindActivateNode   CommandKind = "activate_node"
	CommandKindExpandNode     CommandKind = "expand_node"
	CommandKindStartAttempt   CommandKind = "start_attempt"
	CommandKindSettleAttempt  CommandKind = "settle_attempt"
	CommandKindRecordDecision CommandKind = "record_decision"
	CommandKindShortCircuit   CommandKind = "short_circuit_gate"
	CommandKindGateFeedback   CommandKind = "gate_feedback"
	CommandKindBlockNode      CommandKind = "block_node"
	CommandKindSetTimer       CommandKind = "set_timer"
	CommandKindWaitSignal     CommandKind = "wait_signal"
	CommandKindCompleteRun    CommandKind = "complete_run"
)

type WaitStatus string

const (
	WaitStatusPending   WaitStatus = "pending"
	WaitStatusSatisfied WaitStatus = "satisfied"
	// Canceled is reserved for the engine/store layer; the phase-1 reducer only
	// creates and satisfies waits.
	WaitStatusCanceled WaitStatus = "canceled"
)

type WaitKind string

const (
	WaitKindHuman   WaitKind = "human"
	WaitKindAgent   WaitKind = "agent"
	WaitKindProgram WaitKind = "program"
	WaitKindTimer   WaitKind = "timer"
	WaitKindSignal  WaitKind = "signal"
)

type State struct {
	StateSchemaVersion int         `json:"stateSchemaVersion"`
	RunID              string      `json:"runId,omitempty"`
	Status             RunStatus   `json:"status"`
	Pause              *PauseState `json:"pause,omitempty"`

	OriginalTemplateRef string              `json:"originalTemplateRef"`
	CurrentTemplateRef  string              `json:"currentTemplateRef"`
	TemplateDivergence  *TemplateDivergence `json:"templateDivergence,omitempty"`

	Nodes               map[string]NodeState          `json:"nodes"`
	OutstandingCommands map[string]OutstandingCommand `json:"outstandingCommands,omitempty"`
	Waits               map[string]WaitRecord         `json:"waits,omitempty"`
	Timers              map[string]TimerRecord        `json:"timers,omitempty"`
	AdminRecords        []AdminRecord                 `json:"adminRecords,omitempty"`

	LastLogSeq  int64  `json:"lastLogSeq"`
	LogChecksum string `json:"logChecksum"`
}

// PauseState is scheduler-owned durable state. It distinguishes an automatic
// pause from an operator pause and gives a restarted host everything it needs
// to decide whether it may resume without replaying evidence logs.
type PauseState struct {
	Kind      PauseKind `json:"kind"`
	Reason    string    `json:"reason"`
	CommandID string    `json:"commandId,omitempty"`
	Owner     ActorRef  `json:"owner,omitempty"`
	Until     time.Time `json:"until,omitzero"`
}

type TemplateDivergence struct {
	Diverged bool      `json:"diverged"`
	Reason   string    `json:"reason,omitempty"`
	Actor    ActorRef  `json:"actor,omitempty"`
	At       time.Time `json:"at,omitzero"`
}

type NodeState struct {
	Type     model.NodeType `json:"type,omitempty"`
	Status   NodeStatus     `json:"status"`
	Assignee string         `json:"assignee,omitempty"`
	Attempt  int            `json:"attempt,omitempty"`

	// Compound expansion linkage: Parent/Stage/StepID are set on expanded stage
	// children; Children is the ordered stage-child list on the expanded parent.
	Parent   string          `json:"parent,omitempty"`
	Stage    model.StageKind `json:"stage,omitempty"`
	StepID   string          `json:"stepId,omitempty"`
	Children []string        `json:"children,omitempty"`

	// Gate feedback-loop accounting on stage children. FailCount counts a
	// gate's failed verdicts in the current loop window (cross-gate resets
	// zero it); LastEvidenceHash is the work-evidence hash the gate's latest
	// verdict evaluated (the evidence-unchanged short-circuit compares it);
	// PendingFeedback is the gate payload a work stage's next attempt consumes.
	FailCount        int          `json:"failCount,omitempty"`
	LastEvidenceHash string       `json:"lastEvidenceHash,omitempty"`
	PendingFeedback  *FeedbackRef `json:"pendingFeedback,omitempty"`

	ActiveAttempt *AttemptState    `json:"activeAttempt,omitempty"`
	Decisions     []DecisionRecord `json:"decisions,omitempty"`
	ChosenEdge    string           `json:"chosenEdge,omitempty"`

	BlockedReason string `json:"blockedReason,omitempty"`
	BlockedOwner  string `json:"blockedOwner,omitempty"`
}

// FeedbackRef records which gate verdict a work stage must answer on its next
// attempt, and with what payload.
type FeedbackRef struct {
	FromNodeID  string    `json:"fromNodeId"`
	FromAttempt int       `json:"fromAttempt,omitempty"`
	Feedback    string    `json:"feedback,omitempty"`
	EvidenceRef string    `json:"evidenceRef,omitempty"`
	At          time.Time `json:"at,omitzero"`
}

type AttemptState struct {
	Attempt      int       `json:"attempt"`
	Actor        ActorRef  `json:"actor,omitempty"`
	CommandID    string    `json:"commandId,omitempty"`
	EvidenceRef  string    `json:"evidenceRef,omitempty"`
	EvidenceHash string    `json:"evidenceHash,omitempty"`
	Feedback     string    `json:"feedback,omitempty"`
	StartedAt    time.Time `json:"startedAt,omitzero"`
	SettledAt    time.Time `json:"settledAt,omitzero"`
	Outcome      string    `json:"outcome,omitempty"`
}

type OutstandingCommand struct {
	ID             string          `json:"id"`
	IdempotencyKey string          `json:"idempotencyKey,omitempty"`
	PayloadHash    string          `json:"payloadHash,omitempty"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	NodeID         string          `json:"nodeId"`
	Attempt        int             `json:"attempt,omitempty"`
	Kind           CommandKind     `json:"kind"`
	// Inactive commands (canceled or reconciled) are retained as evidence but may
	// be replaced by a deterministic reissue with the same ID.
	Status         CommandStatus `json:"status"`
	ExternalRef    string        `json:"externalRef,omitempty"`
	Actor          ActorRef      `json:"actor,omitempty"`
	Verdict        string        `json:"verdict,omitempty"`
	EvidenceRef    string        `json:"evidenceRef,omitempty"`
	EvidenceHash   string        `json:"evidenceHash,omitempty"`
	Feedback       string        `json:"feedback,omitempty"`
	CreatedAt      time.Time     `json:"createdAt,omitzero"`
	ReconcileAfter time.Time     `json:"reconcileAfter,omitzero"`
}

type WaitRecord struct {
	ID          string     `json:"id"`
	NodeID      string     `json:"nodeId"`
	Kind        WaitKind   `json:"kind"`
	Status      WaitStatus `json:"status"`
	Assignee    string     `json:"assignee,omitempty"`
	CommandID   string     `json:"commandId,omitempty"`
	CreatedAt   time.Time  `json:"createdAt,omitzero"`
	DueAt       time.Time  `json:"dueAt,omitzero"`
	SatisfiedAt time.Time  `json:"satisfiedAt,omitzero"`
}

type TimerRecord struct {
	ID          string     `json:"id"`
	NodeID      string     `json:"nodeId"`
	Status      WaitStatus `json:"status"`
	CreatedAt   time.Time  `json:"createdAt,omitzero"`
	DueAt       time.Time  `json:"dueAt,omitzero"`
	SatisfiedAt time.Time  `json:"satisfiedAt,omitzero"`
}

type ActorRef string

type DecisionRecord struct {
	Actor       ActorRef  `json:"actor"`
	Verdict     string    `json:"verdict"`
	EvidenceRef string    `json:"evidenceRef,omitempty"`
	Timestamp   time.Time `json:"timestamp,omitzero"`
}

type AdminRecord struct {
	Type        EventType `json:"type,omitempty"`
	Actor       ActorRef  `json:"actor"`
	Reason      string    `json:"reason"`
	EvidenceRef string    `json:"evidenceRef,omitempty"`
	Timestamp   time.Time `json:"timestamp,omitzero"`
}

type NodeInit struct {
	ID       string         `json:"id"`
	Type     model.NodeType `json:"type,omitempty"`
	Status   NodeStatus     `json:"status,omitempty"`
	Assignee string         `json:"assignee,omitempty"`

	// Stage metadata for children introduced by node_expanded events.
	Parent string          `json:"parent,omitempty"`
	Stage  model.StageKind `json:"stage,omitempty"`
	StepID string          `json:"stepId,omitempty"`
}
