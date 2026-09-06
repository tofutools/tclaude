package app

import (
	"context"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

// Store exposes application-owned persistence operations. Methods that admit
// effects atomically create the Operation and reserve its exact target.
type Store interface {
	OrchestrationStore
	CreateAgent(context.Context, model.Agent) error
	UpdateAgent(context.Context, model.AgentID, model.Revision, string, model.DesiredConfiguration, model.AuthorityRequest, time.Time) (model.Agent, error)
	Agent(context.Context, model.AgentID) (model.Agent, error)
	CreateGroup(context.Context, model.Group, model.ConfigurationBounds) error
	Group(context.Context, model.GroupID) (model.Group, error)
	SetGroupOwner(context.Context, model.GroupID, model.AgentID, model.ConfigurationBounds, model.Revision, time.Time) (model.Group, error)

	AuthorityState(context.Context) (AuthorityStateResult, error)
	PutGrant(context.Context, model.AuthorityGrant, model.Revision) (model.AuthorityGrant, error)
	DeleteGrant(context.Context, model.GrantID, model.Revision) error
	PutRole(context.Context, model.Role, model.Revision) (model.Role, error)
	PutRoleAssignment(context.Context, model.RoleAssignment, model.Revision) (model.RoleAssignment, error)
	DeleteRoleAssignment(context.Context, model.RoleAssignment, model.Revision) error
	Authorize(context.Context, model.AuthorityRequest, time.Time) (model.AuthorityDecision, error)

	AdmitLaunch(context.Context, LaunchAdmission) (AdmissionResult, error)
	AdmitExecutionOperation(context.Context, ExecutionOperationAdmission) (AdmissionResult, error)
	ConsumeExecutionEffect(context.Context, model.OperationID, time.Time) error
	RecordPrepared(context.Context, model.ExecutionID, model.OperationID, model.ProviderEvidence, time.Time) (model.Execution, error)
	ConsumeRelease(context.Context, model.ExecutionID, model.OperationID, time.Time) error
	CompleteOperation(context.Context, OperationCompletion) (AdmissionResult, error)
	CompleteContextOperation(context.Context, OperationCompletion, ContextAssociation) (AdmissionResult, error)
	OperationResult(context.Context, model.OperationID) (AdmissionResult, error)
	Execution(context.Context, model.ExecutionID) (model.Execution, error)
	RecoverableExecutions(context.Context) ([]model.Execution, error)
	RecordRecovery(context.Context, model.ExecutionID, model.ExecutionState, *model.NativeConversationEvidence, model.ProviderEvidence, time.Time) (model.Execution, error)
	Continuation(context.Context, model.AgentID, model.ConversationID, model.Revision) (ContinuationRecord, error)
	CurrentConversation(context.Context, model.AgentID) (model.ConversationAssociation, error)
	BeginContextTransition(context.Context, PendingContextTransition) error
	CancelContextTransition(context.Context, model.OperationID) error
	AdmitPrimaryContext(context.Context, PrimaryContextAdmission) (model.Execution, error)

	ExecutionAccess(context.Context, model.ExecutionID) (model.ExecutionAccess, error)
	ExecutionAccessesDue(context.Context, time.Time, time.Time) ([]model.ExecutionAccess, error)
	AuthenticateExecutionAccess(context.Context, []byte, time.Time) (model.ExecutionAccess, error)
	RecordAccessDelivery(context.Context, model.ExecutionID, model.AccessGeneration, ports.ActionCredentialReceipt, time.Time) (model.ExecutionAccess, error)
	RotateExecutionAccess(context.Context, model.ExecutionID, model.AccessGeneration, model.Revision, []byte, ports.ActionCredentialReceipt, time.Time, time.Time, time.Time) (model.ExecutionAccess, error)
	ReactivateExecutionAccess(context.Context, model.ExecutionID, model.AccessGeneration, ports.ActionCredentialRecoveryProof, time.Time) (model.ExecutionAccess, error)
	RevokeExecutionAccess(context.Context, model.ExecutionID, model.Revision, time.Time) (model.ExecutionAccess, error)

	CreateMessage(context.Context, model.Message, model.RequestID, model.OperationID, []model.AuthorityRequest) (MessageAdmissionResult, error)
	MarkMessageRead(context.Context, model.MessageID, model.AgentID, model.AuthorityRequest, time.Time) (model.Message, error)
	MessagesForAgent(context.Context, model.AgentID, bool) ([]model.Message, error)
	Snapshot(context.Context) (Snapshot, error)
	AssociateConversation(context.Context, ContextAssociation) error

	CatalogHistory(context.Context, string, string, []HistoryCatalogWrite, model.HistoryCoverage, time.Time) ([]model.HistoryCatalogEntry, error)
	SearchHistory(context.Context, HistorySearchFilter) (HistorySearchResult, error)
	ResolveHistory(context.Context, model.HistorySelection) (HistorySelectionRecord, error)
	HistoryPoints(context.Context, model.ConversationID) ([]model.HistoryPoint, error)
	IndexHistoryRead(context.Context, model.ConversationID, string, model.HistoryCoverage, time.Time) (model.HistoryCatalogEntry, error)
	SetHistoryMetadata(context.Context, model.ConversationID, model.Revision, string, bool, model.RequestID, model.AuthorityRequest, time.Time) (model.HistoryCatalogEntry, error)

	RegisterWorkspace(context.Context, model.Workspace) error
	Workspace(context.Context, model.WorkspaceID) (model.Workspace, error)
	AdmitWorkspaceEffect(context.Context, WorkspaceEffectAdmission) (WorkspaceEffectAdmissionResult, error)
	CompleteWorkspaceEffect(context.Context, WorkspaceEffectCompletion) (model.Workspace, error)
	UpdateWorkspaceObservation(context.Context, model.WorkspaceID, model.Revision, model.WorkspaceState, model.WorkspaceObservation, model.WorkspaceResourceEvidence, time.Time) (model.Workspace, error)
	ActiveWorkspaceUses(context.Context, model.WorkspaceID) ([]model.WorkspaceUse, error)
	WorkspaceUseForExecution(context.Context, model.ExecutionID) (model.WorkspaceUse, error)
	ReleaseWorkspaceUse(context.Context, model.WorkspaceUseID, model.ExecutionID, time.Time) error
	AcquireHistoryUse(context.Context, model.HistoryUseClaim) error
	SettleHistoryUse(context.Context, model.HistoryUseID, model.Revision, model.HistoryUseState, time.Time) (model.HistoryUseClaim, error)
	HistoryUse(context.Context, model.HistoryUseID) (model.HistoryUseClaim, error)
	AdmitShell(context.Context, ShellAdmission) (AdmissionResult, error)
	RecordShellPrepared(context.Context, model.ExecutionID, model.OperationID, ports.ShellResourceEvidence, time.Time) (model.Execution, error)
	CompleteShell(context.Context, OperationCompletion, ports.ShellResourceEvidence) (AdmissionResult, error)
	ShellRecovery(context.Context, model.ExecutionID) (ShellRecoveryRecord, error)
	RecordShellRecovery(context.Context, model.ExecutionID, model.ExecutionState, ports.ShellResourceEvidence, time.Time) (model.Execution, error)

	CreateWorkRun(context.Context, model.WorkRun, *model.HistoryUseClaim) (model.WorkRun, bool, error)
	WorkRun(context.Context, model.WorkRunID) (WorkRunRecord, error)
	WorkRunByRequest(context.Context, model.Principal, model.RequestID) (WorkRunRecord, error)
	PendingWorkRuns(context.Context) ([]WorkRunRecord, error)
	RecordWorkProgress(context.Context, WorkProgress) (WorkRunRecord, error)
	RecordWorkEvidence(context.Context, model.WorkEvidence, model.Revision, model.AuthorityRequest, time.Time) (WorkRunRecord, error)
	DecideWork(context.Context, model.WorkDecision, model.Revision, model.AuthorityRequest, time.Time) (WorkRunRecord, error)
	CancelWork(context.Context, model.WorkRunID, model.Revision, string, model.RequestID, model.AuthorityRequest, time.Time) (WorkRunRecord, error)
	ResolveWorkUncertainty(context.Context, model.WorkDecision, model.Revision, model.AuthorityRequest, time.Time) (WorkRunRecord, error)
}

// OrchestrationStore persists authored definitions and graph execution in the
// same database/transaction owner as ordinary Operations and WorkRuns.
type OrchestrationStore interface {
	SaveDefinition(context.Context, model.Definition, model.DefinitionRevision, model.Revision) (DefinitionRecord, error)
	Definition(context.Context, model.DefinitionID) (DefinitionRecord, error)
	DefinitionRevision(context.Context, model.DefinitionRevisionID) (model.DefinitionRevision, error)
	ListDefinitions(context.Context, model.DefinitionKind, bool) ([]model.Definition, error)
	SaveProgramProfile(context.Context, model.ProgramProfile, model.ProgramProfileRevision, model.Revision) (ProgramProfileRecord, error)
	ProgramProfile(context.Context, model.ProgramProfileID) (ProgramProfileRecord, error)
	ProgramProfileRevision(context.Context, model.ProgramProfileRevisionID) (model.ProgramProfileRevision, error)
	ListProgramProfiles(context.Context, bool) ([]model.ProgramProfile, error)
	CreateGraphWorkRun(context.Context, model.WorkRun, []model.DecisionWindow) (WorkRunRecord, bool, error)
	Decision(context.Context, model.DecisionID) (DecisionRecord, error)
	PendingDecisions(context.Context) ([]DecisionRecord, error)
	SubmitDecision(context.Context, model.DecisionSubmission, model.AuthorityRequest, time.Time) (DecisionRecord, error)
	ApplyGraphTransition(context.Context, GraphTransition) (WorkRunRecord, error)
	SaveAutomationRule(context.Context, model.AutomationRule, model.AutomationRuleRevision, model.Revision) (AutomationRuleRecord, error)
	AutomationRule(context.Context, model.AutomationRuleID) (AutomationRuleRecord, error)
	AutomationRuleRevision(context.Context, model.AutomationRuleRevisionID) (model.AutomationRuleRevision, error)
	ListAutomationRules(context.Context, bool) ([]model.AutomationRule, error)
	MaterializeOccurrence(context.Context, model.AutomationOccurrence, model.Revision) (OccurrenceRecord, bool, error)
	Occurrence(context.Context, model.OccurrenceID) (OccurrenceRecord, error)
	OccurrencesForRule(context.Context, model.AutomationRuleID) ([]OccurrenceRecord, error)
	PendingOccurrences(context.Context) ([]OccurrenceRecord, error)
	UpdateOccurrence(context.Context, model.OccurrenceID, model.Revision, model.OccurrenceState, model.OperationID, model.WorkRunID, model.DeploymentID, []model.OccurrenceRecipient, time.Time) (OccurrenceRecord, error)
	CreateTeamDeployment(context.Context, model.TeamDeployment, model.Group, []model.Agent) (model.TeamDeployment, bool, error)
	TeamDeployment(context.Context, model.DeploymentID) (model.TeamDeployment, error)
	PendingTeamDeployments(context.Context) ([]model.TeamDeployment, error)
	UpdateTeamDeployment(context.Context, model.DeploymentID, model.Revision, model.DeploymentState, uint32, time.Time) (model.TeamDeployment, error)
}

type ShellAdmission struct {
	Operation         model.Operation
	Execution         model.Execution
	WorkspaceUse      model.WorkspaceUse
	WorkspaceRevision model.Revision
	Authority         model.AuthorityRequest
}

type ShellRecoveryRecord struct {
	WorkspaceID model.WorkspaceID
	Evidence    ports.ShellResourceEvidence
}

type HistoryPointWrite struct {
	Point model.HistoryPoint
	Token string
}

type HistoryCatalogWrite struct {
	Entry             model.HistoryCatalogEntry
	Native            model.NativeConversationEvidence
	SourceToken       string
	SourceFingerprint string
	Evidence          model.ProviderEvidence
	Points            []HistoryPointWrite
}

type HistorySearchFilter struct {
	Harness     string
	WorkspaceID model.WorkspaceID
	Query       string
	Archived    *bool
}

type HistorySelectionRecord struct {
	Entry  model.HistoryCatalogEntry
	Point  *model.HistoryPoint
	Source ports.HistorySourceSelection
}

type WorkspaceEffectAdmission struct {
	Operation model.Operation
	Workspace model.Workspace
	Authority model.AuthorityRequest
}

type WorkspaceEffectAdmissionResult struct {
	Operation model.Operation
	Workspace model.Workspace
	Repeated  bool
}

type WorkspaceEffectCompletion struct {
	OperationID model.OperationID
	WorkspaceID model.WorkspaceID
	State       model.WorkspaceState
	Observation model.WorkspaceObservation
	Resource    model.WorkspaceResourceEvidence
	Disposition ports.EffectDisposition
	Detail      string
	At          time.Time
}

type WorkRunRecord struct {
	Run          model.WorkRun
	Evidence     []model.WorkEvidence
	Decision     *model.WorkDecision
	NodeEvidence []model.WorkNodeEvidence
	Decisions    []model.DecisionWindow
}

type DefinitionRecord struct {
	Definition model.Definition
	Head       model.DefinitionRevision
}

type ProgramProfileRecord struct {
	Profile model.ProgramProfile
	Head    model.ProgramProfileRevision
}

type AutomationRuleRecord struct {
	Rule model.AutomationRule
	Head model.AutomationRuleRevision
}

type OccurrenceRecord struct {
	Occurrence model.AutomationOccurrence
}

type DecisionRecord struct {
	Window     model.DecisionWindow
	Submission *model.DecisionSubmission
}

type GraphAttemptUpdate struct {
	Ref           model.WorkAttemptRef
	NewIssuanceID model.WorkIssuanceID
	OperationID   model.OperationID
	ExecutionID   model.ExecutionID
	State         model.WorkNodeAttemptState
	Outcome       model.WorkOutcome
	Detail        string
}

type GraphTransition struct {
	WorkRunID        model.WorkRunID
	ExpectedRevision model.Revision
	Authority        model.AuthorityRequest
	Evidence         *model.WorkNodeEvidence
	Decision         *model.DecisionSubmission
	Operation        *model.Operation
	Execution        *model.Execution
	WorkspaceUse     *model.WorkspaceUse
	AgentExpected    model.Revision
	Access           *model.ExecutionAccess
	Updates          []GraphAttemptUpdate
	Activations      []model.WorkNodeAttempt
	DecisionWindows  []model.DecisionWindow
	RunState         model.WorkRunState
	ControlState     model.WorkControlState
	RunOutcome       model.WorkOutcome
	At               time.Time
}

type WorkProgress struct {
	WorkRunID         model.WorkRunID
	ExpectedRevision  model.Revision
	Step              model.WorkStep
	Attempt           uint64
	OperationID       model.OperationID
	AttemptState      model.WorkAttemptState
	RunState          model.WorkRunState
	WorkerExecutionID model.ExecutionID
	Detail            string
	At                time.Time
}

type LaunchAdmission struct {
	Operation                    model.Operation
	Execution                    model.Execution
	AgentID                      model.AgentID
	Expected                     model.Revision
	ExpectedConversationRevision model.Revision
	Authority                    model.AuthorityRequest
	Access                       model.ExecutionAccess
}

type ExecutionOperationAdmission struct {
	Operation model.Operation
	Authority model.AuthorityRequest
}

type PrimaryContextAdmission struct {
	Evidence            ports.PrimaryContextEvidence
	ResetConversationID model.ConversationID
	At                  time.Time
}

type PendingContextTransition struct {
	OperationID                 model.OperationID
	ExecutionID                 model.ExecutionID
	ExpectedConversationID      model.ConversationID
	ExpectedAssociationRevision model.Revision
	Correlation                 string
	CreatedAt                   time.Time
}

type AdmissionResult struct {
	Operation model.Operation
	Execution model.Execution
	Repeated  bool
}

type OperationCompletion struct {
	OperationID          model.OperationID
	OperationState       model.OperationState
	ResultCode           string
	Detail               string
	ExecutionID          model.ExecutionID
	ExecutionState       model.ExecutionState
	UpdateExecutionState bool
	Evidence             model.ProviderEvidence
	Native               *model.NativeConversationEvidence
	At                   time.Time
}

type MessageAdmissionResult struct {
	Operation model.Operation
	Message   model.Message
	Repeated  bool
}

type ContextAssociation struct {
	ExecutionID      model.ExecutionID
	AgentID          model.AgentID
	ConversationID   model.ConversationID
	ExpectedRevision model.Revision
	Native           *model.NativeConversationEvidence
	At               time.Time
}

type ContinuationRecord struct {
	Conversation model.ConversationAssociation
	Native       model.NativeConversationEvidence
	Evidence     model.ProviderEvidence
}
