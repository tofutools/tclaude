package app

import (
	"context"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

type API interface {
	CreateAgent(context.Context, CreateAgentRequest) (AgentResult, error)
	UpdateAgent(context.Context, UpdateAgentRequest) (AgentResult, error)
	RetireAgent(context.Context, RetireAgentRequest) (AgentResult, error)
	ReactivateAgent(context.Context, ReactivateAgentRequest) (AgentResult, error)
	CreateGroup(context.Context, CreateGroupRequest) (GroupResult, error)
	Launch(context.Context, LaunchRequest) (OperationResult, error)
	Observe(context.Context, ObserveRequest) (ObservationResult, error)
	Interact(context.Context, InteractRequest) (OperationResult, error)
	Attach(context.Context, AttachRequest) (AttachmentResult, error)
	Stop(context.Context, StopRequest) (OperationResult, error)
	Resume(context.Context, ResumeRequest) (OperationResult, error)
	ChangeContext(context.Context, ChangeContextRequest) (OperationResult, error)
	SendMessage(context.Context, SendMessageRequest) (MessageResult, error)
	MarkMessageRead(context.Context, MarkMessageReadRequest) (MessageResult, error)
	CreateAttachmentClaim(context.Context, CreateAttachmentClaimRequest) (AttachmentClaimResult, error)
	ReadAttachment(context.Context, ReadAttachmentRequest) (AttachmentContentResult, error)
	Snapshot(context.Context, SnapshotRequest) (Snapshot, error)
	Recover(context.Context, RecoverRequest) (RecoveryReport, error)
}

// JourneyAPI is the focused public application surface for history,
// workspaces, and bounded work. Provider tokens and host receipts stay behind
// this boundary.
type JourneyAPI interface {
	RefreshHistory(context.Context, RefreshHistoryRequest) (HistorySearchResult, error)
	SearchHistory(context.Context, SearchHistoryRequest) (HistorySearchResult, error)
	ReadHistory(context.Context, ReadHistoryRequest) (HistoryReadResult, error)
	SetConversationMetadata(context.Context, SetConversationMetadataRequest) (HistorySearchResult, error)
	RegisterWorkspace(context.Context, RegisterWorkspaceRequest) (WorkspaceResult, error)
	CreateCheckout(context.Context, CreateCheckoutRequest) (WorkspaceResult, error)
	InspectWorkspace(context.Context, InspectWorkspaceRequest) (WorkspaceResult, error)
	RemoveCheckout(context.Context, RemoveCheckoutRequest) (WorkspaceResult, error)
	RestoreCheckout(context.Context, RestoreCheckoutRequest) (WorkspaceResult, error)
	StartShell(context.Context, StartShellRequest) (OperationResult, error)
	StartWork(context.Context, StartWorkRequest) (WorkRunResult, error)
	InspectWork(context.Context, InspectWorkRequest) (WorkRunResult, error)
	RecordWorkEvidence(context.Context, RecordWorkEvidenceRequest) (WorkRunResult, error)
	DecideWork(context.Context, DecideWorkRequest) (WorkRunResult, error)
	CancelWork(context.Context, CancelWorkRequest) (WorkRunResult, error)
	ResolveWorkUncertainty(context.Context, ResolveWorkUncertaintyRequest) (WorkRunResult, error)
}

type WorkReconciler interface {
	ReconcilePendingWork(context.Context) (WorkReconcileReport, error)
}

type RequestContext struct {
	Principal model.Principal
	RequestID model.RequestID
}

type CreateAgentRequest struct {
	ConfigurationProfile *model.ConfigurationProfileRef
	Context              model.Principal
	ID                   model.AgentID
	Name                 string
	TaskReference        string
	ParentAgentID        model.AgentID
	CloneSourceAgentID   model.AgentID
	Notifications        model.AgentNotificationPreferences
	Desired              model.DesiredConfiguration
}

type UpdateAgentRequest struct {
	ConfigurationProfile *model.ConfigurationProfileRef
	Context              model.Principal
	ID                   model.AgentID
	ExpectedRevision     model.Revision
	Name                 string
	TaskReference        string
	Notifications        model.AgentNotificationPreferences
	Desired              model.DesiredConfiguration
}

type RetireAgentRequest struct {
	Context          model.Principal
	ID               model.AgentID
	ExpectedRevision model.Revision
	Reason           string
}

type ReactivateAgentRequest struct {
	Context          model.Principal
	ID               model.AgentID
	ExpectedRevision model.Revision
}

type AgentResult struct{ Agent model.Agent }

type CreateGroupRequest struct {
	Context      model.Principal
	ID           model.GroupID
	Name         string
	Members      []model.AgentID
	OwnerAgentID model.AgentID
	OwnerBounds  model.ConfigurationBounds
}

type GroupResult struct{ Group model.Group }

type LaunchRequest struct {
	RequestContext
	Target LaunchTarget
}

type LaunchTarget struct {
	Agent      *AgentLaunchTarget
	Standalone *StandaloneLaunchTarget
}

type AgentLaunchTarget struct {
	AgentID          model.AgentID
	ExpectedRevision model.Revision
}

type StandaloneLaunchTarget struct {
	Desired        model.DesiredConfiguration
	ConversationID model.ConversationID
}

type InteractRequest struct {
	RequestContext
	ExecutionID model.ExecutionID
	Text        string
}

type ObserveRequest struct {
	Principal   model.Principal
	ExecutionID model.ExecutionID
}

type ObservationResult struct {
	Execution   model.Execution
	Observation ports.Observation
}

type AttachRequest struct {
	RequestContext
	ExecutionID model.ExecutionID
	Kind        ports.AttachmentKind
}

type AttachmentResult struct {
	Operation  model.Operation
	Attachment ports.Attachment
}

type StopRequest struct {
	RequestContext
	ExecutionID model.ExecutionID
	Force       bool
}

type ResumeRequest struct {
	RequestContext
	Target                      LaunchTarget
	ConversationID              model.ConversationID
	ExpectedAssociationRevision model.Revision
}

type ChangeContextRequest struct {
	RequestContext
	ExecutionID                 model.ExecutionID
	Intent                      ports.ContextChangeIntent
	ExpectedConversationID      model.ConversationID
	ExpectedAssociationRevision model.Revision
}

type SendMessageRequest struct {
	RequestContext
	Subject         string
	ParentMessageID model.MessageID
	To              model.MessageAudience
	CC              model.MessageAudience
	Attachments     []AttachmentInput
	// RecipientAgentIDs is the temporary transport adapter for the existing
	// client. New callers author To/CC explicitly.
	RecipientAgentIDs []model.AgentID
	Body              string
}

type AttachmentInput struct {
	Filename  string
	MediaType string
	Content   []byte
	Claim     *AttachmentClaimReference
}

// AttachmentClaimReference repeats immutable metadata so exact RequestID
// retry comparison does not depend on rereading mutable claim state.
type AttachmentClaimReference struct {
	ClaimID      model.AttachmentClaimID
	AttachmentID model.AttachmentID
	Filename     string
	MediaType    string
	Size         int64
	SHA256       string
}

type CreateAttachmentClaimRequest struct {
	Principal model.Principal
	Filename  string
	MediaType string
	Content   []byte
}

type AttachmentClaimResult struct{ Claim model.AttachmentClaim }

type ReadAttachmentRequest struct {
	Principal    model.Principal
	AttachmentID model.AttachmentID
}

type AttachmentContentResult struct {
	Attachment model.Attachment
	Content    []byte
}

type MarkMessageReadRequest struct {
	RequestContext
	MessageID model.MessageID
	AgentID   model.AgentID
	Operator  bool
}

type MessageResult struct{ Message model.Message }

type OperationResult struct {
	Operation model.Operation
	Execution *model.Execution
	Repeated  bool
}

type SnapshotRequest struct {
	Principal model.Principal
}

type Snapshot struct {
	Conversations []model.Conversation
	Associations  []model.ConversationAssociation
	Revision      model.Revision
	Agents        []model.Agent
	Groups        []model.Group
	Executions    []model.Execution
	Operations    []model.Operation
	Messages      []model.Message
	History       []model.HistoryCatalogEntry
	HistoryPoints []model.HistoryPoint
	Workspaces    []WorkspaceView
	WorkspaceUses []model.WorkspaceUse
	WorkRuns      []model.WorkRun
	WorkEvidence  []model.WorkEvidence
}

type RecoverRequest struct{ Principal model.Principal }

type RecoveryReport struct {
	Controlled []model.ExecutionID
	Exited     []model.ExecutionID
	Unknown    []model.ExecutionID
}

type RefreshHistoryRequest struct {
	Principal  model.Principal
	Harness    string
	SourceName string
}

type SearchHistoryRequest struct {
	Principal   model.Principal
	Harness     string
	WorkspaceID model.WorkspaceID
	Query       string
	Archived    *bool
}

type HistorySearchResult struct {
	Entries  []model.HistoryCatalogEntry
	Coverage model.HistoryCoverage
}

type ReadHistoryRequest struct {
	Principal model.Principal
	Selection model.HistorySelection
}

type SetConversationMetadataRequest struct {
	Context          RequestContext
	ConversationID   model.ConversationID
	ExpectedRevision model.Revision
	Title            string
	Archived         bool
}

type HistoryReadResult struct {
	Entry    model.HistoryCatalogEntry
	Point    *model.HistoryPoint
	Points   []model.HistoryPoint
	Turns    []HistoryTurn
	Coverage model.HistoryCoverage
}

type HistoryTurn struct {
	PointID model.HistoryPointID
	Role    string
	Parts   []ports.HistoryPart
}

type RegisterWorkspaceRequest struct {
	Context RequestContext
	ID      model.WorkspaceID
	Intent  model.WorkspaceIntent
}

type CreateCheckoutRequest struct {
	Context RequestContext
	ID      model.WorkspaceID
	Intent  model.WorkspaceIntent
}

type InspectWorkspaceRequest struct {
	Principal   model.Principal
	WorkspaceID model.WorkspaceID
}

type RemoveCheckoutRequest struct {
	Context          RequestContext
	WorkspaceID      model.WorkspaceID
	ExpectedRevision model.Revision
	Destructive      bool
}

type RestoreCheckoutRequest struct {
	Context          RequestContext
	WorkspaceID      model.WorkspaceID
	ExpectedRevision model.Revision
}

type StartShellRequest struct {
	Context          RequestContext
	WorkspaceID      model.WorkspaceID
	ExpectedRevision model.Revision
	Sandbox          model.SandboxMode
}

type WorkspaceView struct {
	ID          model.WorkspaceID
	Intent      model.WorkspaceIntent
	State       model.WorkspaceState
	Observation model.WorkspaceObservation
	Revision    model.Revision
}

type WorkspaceResult struct{ Workspace WorkspaceView }

type StartWorkRequest struct {
	Context RequestContext
	ID      model.WorkRunID
	Spec    model.WorkRunSpec
}

type InspectWorkRequest struct {
	Principal model.Principal
	WorkRunID model.WorkRunID
}

type WorkRunResult struct {
	Run      model.WorkRun
	Evidence []model.WorkEvidence
	Decision *model.WorkDecision
}

type RecordWorkEvidenceRequest struct {
	Context             RequestContext
	WorkRunID           model.WorkRunID
	Step                model.WorkStep
	Attempt             uint64
	Kind                model.WorkEvidenceKind
	ArtifactRevision    string
	Passed              *bool
	Detail              string
	ExpectedRunRevision model.Revision
}

type DecideWorkRequest struct {
	Context             RequestContext
	WorkRunID           model.WorkRunID
	Step                model.WorkStep
	Attempt             uint64
	Decision            model.WorkDecisionKind
	Reason              string
	ExpectedRunRevision model.Revision
}

type CancelWorkRequest struct {
	Context             RequestContext
	WorkRunID           model.WorkRunID
	ExpectedRunRevision model.Revision
	Reason              string
}

// ResolveWorkUncertaintyRequest is an explicit operator conclusion that the
// uncertain external effect did not occur. It never replays that effect.
type ResolveWorkUncertaintyRequest struct {
	Context             RequestContext
	WorkRunID           model.WorkRunID
	ExpectedRunRevision model.Revision
	Reason              string
}

type WorkReconcileReport struct {
	Pending   []model.WorkRunID
	Uncertain []model.WorkRunID
}
