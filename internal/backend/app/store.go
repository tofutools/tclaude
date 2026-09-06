package app

import (
	"context"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
)

// Store exposes application-owned persistence operations. Methods that admit
// effects atomically create the Operation and reserve its exact target.
type Store interface {
	CreateAgent(context.Context, model.Agent) error
	UpdateAgent(context.Context, model.AgentID, model.Revision, string, model.DesiredConfiguration, time.Time) (model.Agent, error)
	Agent(context.Context, model.AgentID) (model.Agent, error)
	CreateGroup(context.Context, model.Group) error
	Group(context.Context, model.GroupID) (model.Group, error)

	AdmitLaunch(context.Context, LaunchAdmission) (AdmissionResult, error)
	AdmitExecutionOperation(context.Context, ExecutionOperationAdmission) (AdmissionResult, error)
	RecordPrepared(context.Context, model.ExecutionID, model.OperationID, model.ProviderEvidence, time.Time) (model.Execution, error)
	ConsumeRelease(context.Context, model.ExecutionID, model.OperationID, time.Time) error
	CompleteOperation(context.Context, OperationCompletion) (AdmissionResult, error)
	CompleteContextOperation(context.Context, OperationCompletion, ContextAssociation) (AdmissionResult, error)
	Execution(context.Context, model.ExecutionID) (model.Execution, error)
	RecoverableExecutions(context.Context) ([]model.Execution, error)
	RecordRecovery(context.Context, model.ExecutionID, model.ExecutionState, *model.NativeConversationEvidence, model.ProviderEvidence, time.Time) (model.Execution, error)
	Continuation(context.Context, model.AgentID, model.ConversationID, model.Revision) (ContinuationRecord, error)
	CurrentConversation(context.Context, model.AgentID) (model.ConversationAssociation, error)

	CreateMessage(context.Context, model.Message, model.RequestID, model.OperationID) (MessageAdmissionResult, error)
	MarkMessageRead(context.Context, model.MessageID, model.AgentID, time.Time) (model.Message, error)
	Snapshot(context.Context) (Snapshot, error)
	AssociateConversation(context.Context, ContextAssociation) error
}

type LaunchAdmission struct {
	Operation                    model.Operation
	Execution                    model.Execution
	AgentID                      model.AgentID
	Expected                     model.Revision
	ExpectedConversationRevision model.Revision
}

type ExecutionOperationAdmission struct {
	Operation model.Operation
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
