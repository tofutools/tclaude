package model

import (
	"encoding/json"
	"time"
)

type HistoryAvailability string

const (
	HistoryMetadataOnly HistoryAvailability = "metadata_only"
	HistoryContent      HistoryAvailability = "content"
	HistoryAbsent       HistoryAvailability = "absent"
	HistoryUnknown      HistoryAvailability = "unknown"
)

type HistoryCoverageState string

const (
	HistoryCoverageComplete HistoryCoverageState = "complete"
	HistoryCoveragePartial  HistoryCoverageState = "partial"
	HistoryCoverageUnknown  HistoryCoverageState = "unknown"
)

// HistoryCoverage qualifies an otherwise ambiguous empty or partial history
// result. SourceRevision is provider-owned correlation, not caller authority.
type HistoryCoverage struct {
	Metadata       HistoryCoverageState
	Content        HistoryCoverageState
	SourceRevision string
	RefreshedAt    time.Time
}

// HistoryCatalogEntry is the public, platform-owned catalog view for a logical
// Conversation. Native source tokens and paths are deliberately absent.
type HistoryCatalogEntry struct {
	ConversationID ConversationID
	Harness        string
	Title          string
	WorkspaceID    WorkspaceID
	WorkspaceHint  string
	Archived       bool
	Availability   HistoryAvailability
	Coverage       HistoryCoverage
	ModifiedAt     time.Time
	Revision       Revision
}

type HistoryPointKind string

const (
	HistoryPointHead    HistoryPointKind = "head"
	HistoryPointMessage HistoryPointKind = "message"
	// HistoryPointBeforeMessage is an exclusive boundary: the selected
	// message itself is not included in the read/fork prefix.
	HistoryPointBeforeMessage HistoryPointKind = "before_message"
	HistoryPointTurn          HistoryPointKind = "turn"
)

// HistoryPoint identifies only a provider-supported selectable point. A
// provider message/session token is retained privately by persistence.
type HistoryPoint struct {
	ID             HistoryPointID
	ConversationID ConversationID
	Kind           HistoryPointKind
	OccurredAt     time.Time
	Revision       Revision
}

type HistorySelection struct {
	ConversationID               ConversationID
	ExpectedConversationRevision Revision
	PointID                      HistoryPointID
	ExpectedPointRevision        Revision
}

type HistoryUseState string

const (
	HistoryUseHeld      HistoryUseState = "held"
	HistoryUseUncertain HistoryUseState = "uncertain"
	HistoryUseReleased  HistoryUseState = "released"
)

// HistoryUseClaim is application-owned durable exclusion for a provider that
// cannot fork a mutable source safely while it is shared. Unknown effects keep
// the claim held as uncertain until explicit reconciliation.
type HistoryUseClaim struct {
	ID                HistoryUseID
	ConversationID    ConversationID
	PointID           HistoryPointID
	OperationID       OperationID
	WorkRunID         WorkRunID
	SourceRevision    string
	SourceFingerprint string
	State             HistoryUseState
	Revision          Revision
	CreatedAt         time.Time
	SettledAt         *time.Time
}

type WorkspaceProvenance string

const (
	WorkspaceRegistered      WorkspaceProvenance = "registered"
	WorkspacePlatformCreated WorkspaceProvenance = "platform_created"
)

type WorkspaceOwnership string

const (
	WorkspaceExternal WorkspaceOwnership = "external"
	WorkspaceOwned    WorkspaceOwnership = "owned"
	WorkspaceShared   WorkspaceOwnership = "shared"
)

type WorkspaceState string

const (
	WorkspacePending   WorkspaceState = "pending"
	WorkspaceAvailable WorkspaceState = "available"
	WorkspaceUncertain WorkspaceState = "uncertain"
	WorkspaceRemoved   WorkspaceState = "removed"
)

type WorkspaceIntent struct {
	Repository     string
	IntendedPath   string
	BaseRevision   string
	Branch         string
	Provenance     WorkspaceProvenance
	Ownership      WorkspaceOwnership
	RetainOnFinish bool
}

type WorkspaceObservation struct {
	ActualPath     string
	RepositoryRoot string
	Revision       string
	Branch         string
	Dirty          bool
	ObservedAt     time.Time
}

// WorkspaceResourceEvidence is an opaque host-issued ownership receipt. The
// application persists and returns it to the host but never interprets Payload.
type WorkspaceResourceEvidence struct {
	Owner   string
	Version uint32
	Payload []byte
}

type Workspace struct {
	ID          WorkspaceID
	Intent      WorkspaceIntent
	State       WorkspaceState
	Observation WorkspaceObservation
	Resource    WorkspaceResourceEvidence
	Revision    Revision
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// WorkspaceSelection identifies an operator-selected available checkout.
type WorkspaceSelection struct {
	WorkspaceID      WorkspaceID
	ExpectedRevision Revision
}

// WorkspaceUse is the durable exact claim that prevents cleanup from being
// inferred from an Agent, Work Run, or path alone.
type WorkspaceUse struct {
	ID          WorkspaceUseID
	WorkspaceID WorkspaceID
	ExecutionID ExecutionID
	WorkRunID   WorkRunID
	ReleasedAt  *time.Time
	CreatedAt   time.Time
}

type WorkRunState string

const (
	WorkRunPending   WorkRunState = "pending"
	WorkRunRunning   WorkRunState = "running"
	WorkRunWaiting   WorkRunState = "waiting_evidence"
	WorkRunSucceeded WorkRunState = "succeeded"
	WorkRunFailed    WorkRunState = "failed"
	WorkRunCancelled WorkRunState = "cancelled"
	WorkRunUncertain WorkRunState = "uncertain"
)

type WorkStep string

const (
	WorkStepPrepareWorkspace WorkStep = "prepare_workspace"
	WorkStepPrepareHistory   WorkStep = "prepare_history"
	WorkStepLaunchWorker     WorkStep = "launch_worker"
	WorkStepDeliverRequest   WorkStep = "deliver_request"
	WorkStepAwaitEvidence    WorkStep = "await_evidence"
	WorkStepEvaluate         WorkStep = "evaluate"
)

type WorkAttemptState string

const (
	WorkAttemptPending   WorkAttemptState = "pending"
	WorkAttemptRunning   WorkAttemptState = "running"
	WorkAttemptSucceeded WorkAttemptState = "succeeded"
	WorkAttemptFailed    WorkAttemptState = "failed"
	WorkAttemptUncertain WorkAttemptState = "uncertain"
)

type WorkRunSpec struct {
	SourceMode          WorkSourceMode
	History             HistorySelection
	FreshHandoff        string
	WorkspaceID         WorkspaceID
	WorkspaceRevision   Revision
	WorkerAgentID       AgentID
	WorkerAgentRevision Revision
	WorkerDesired       DesiredConfiguration
	Brief               string
	Outcome             WorkOutcomePolicy
}

type WorkSourceMode string

const (
	WorkSourceFork         WorkSourceMode = "fork"
	WorkSourceFreshHandoff WorkSourceMode = "fresh_handoff"
)

type WorkOutcomeMode string

const (
	WorkOutcomeHumanDecision WorkOutcomeMode = "human_decision"
	WorkOutcomeVerification  WorkOutcomeMode = "verification"
)

type WorkVerification struct {
	Command          []string
	ArtifactRef      string
	ExpectedExitCode int
}

// WorkOutcomePolicy pins what may settle this run. Human decisions still need
// current decision authority; verification evidence must match the exact
// attempt and artifact revision.
type WorkOutcomePolicy struct {
	Mode         WorkOutcomeMode
	Verification *WorkVerification
}

type WorkStepAttempt struct {
	Step        WorkStep
	Attempt     uint64
	OperationID OperationID
	State       WorkAttemptState
	Detail      string
	StartedAt   time.Time
	SettledAt   *time.Time
}

type WorkRun struct {
	ID                    WorkRunID
	RequestID             RequestID
	Requester             Principal
	Authority             AuthoritySubject
	Delegation            *AutomationDelegation
	Spec                  WorkRunSpec
	WorkspaceUseID        WorkspaceUseID
	HistoryUseID          HistoryUseID
	WorkerExecutionID     ExecutionID
	CancellationRequested bool
	CancellationReason    string
	State                 WorkRunState
	Attempts              []WorkStepAttempt
	Graph                 *WorkGraph
	DefinitionClosure     []DefinitionRef
	Parameters            map[string]json.RawMessage
	Scope                 WorkScope
	AuthorizedPrograms    []ProgramProfileRef
	ControlState          WorkControlState
	Outcome               WorkOutcome
	Deadline              time.Time
	NodeAttempts          []WorkNodeAttempt
	Revision              Revision
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

type WorkEvidenceKind string

const (
	WorkEvidenceWorkerReport WorkEvidenceKind = "worker_report"
	WorkEvidenceVerification WorkEvidenceKind = "verification"
	WorkEvidenceArtifact     WorkEvidenceKind = "artifact"
)

// WorkEvidence is bound to one exact step attempt and artifact revision. It is
// attributed input to settlement, never an outcome by itself.
type WorkEvidence struct {
	ID               WorkEvidenceID
	RequestID        RequestID
	WorkRunID        WorkRunID
	Step             WorkStep
	Attempt          uint64
	Kind             WorkEvidenceKind
	Reporter         Principal
	ArtifactRevision string
	Passed           *bool
	Detail           string
	RecordedAt       time.Time
	Revision         Revision
}

type WorkDecisionKind string

const (
	WorkDecisionAccept WorkDecisionKind = "accept"
	WorkDecisionReject WorkDecisionKind = "reject"
	WorkDecisionCancel WorkDecisionKind = "cancel"
)

type WorkDecision struct {
	WorkRunID WorkRunID
	RequestID RequestID
	Step      WorkStep
	Attempt   uint64
	Decision  WorkDecisionKind
	Decider   Principal
	Reason    string
	DecidedAt time.Time
	Revision  Revision
}

// ShellGroupSelection is an explicit group context pinned at shell admission.
// It confers no group ownership or lifecycle authority.
type ShellGroupSelection struct {
	GroupID               GroupID
	Revision              Revision
	ConfigurationRevision Revision
}
