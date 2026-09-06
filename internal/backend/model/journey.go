package model

import "time"

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

type Workspace struct {
	ID          WorkspaceID
	Intent      WorkspaceIntent
	State       WorkspaceState
	Observation WorkspaceObservation
	Revision    Revision
	CreatedAt   time.Time
	UpdatedAt   time.Time
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
	History       HistorySelection
	WorkspaceID   WorkspaceID
	WorkerAgentID AgentID
	Brief         string
	ArtifactRef   string
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
	ID         WorkRunID
	Requester  Principal
	Authority  AuthoritySubject
	Delegation *AutomationDelegation
	Spec       WorkRunSpec
	State      WorkRunState
	Attempts   []WorkStepAttempt
	Revision   Revision
	CreatedAt  time.Time
	UpdatedAt  time.Time
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
	WorkRunID        WorkRunID
	Step             WorkStep
	Attempt          uint64
	Kind             WorkEvidenceKind
	Reporter         Principal
	ArtifactRevision string
	Passed           *bool
	Detail           string
	RecordedAt       time.Time
}

type WorkDecisionKind string

const (
	WorkDecisionAccept WorkDecisionKind = "accept"
	WorkDecisionReject WorkDecisionKind = "reject"
	WorkDecisionCancel WorkDecisionKind = "cancel"
)

type WorkDecision struct {
	WorkRunID WorkRunID
	Step      WorkStep
	Attempt   uint64
	Decision  WorkDecisionKind
	Decider   Principal
	Reason    string
	DecidedAt time.Time
}
