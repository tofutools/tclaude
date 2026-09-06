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
