package app

import (
	"context"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

type API interface {
	CreateAgent(context.Context, CreateAgentRequest) (AgentResult, error)
	UpdateAgent(context.Context, UpdateAgentRequest) (AgentResult, error)
	CreateGroup(context.Context, CreateGroupRequest) (GroupResult, error)
	Launch(context.Context, LaunchRequest) (OperationResult, error)
	Interact(context.Context, InteractRequest) (OperationResult, error)
	Attach(context.Context, AttachRequest) (AttachmentResult, error)
	Stop(context.Context, StopRequest) (OperationResult, error)
	Resume(context.Context, ResumeRequest) (OperationResult, error)
	ChangeContext(context.Context, ChangeContextRequest) (OperationResult, error)
	SendMessage(context.Context, SendMessageRequest) (MessageResult, error)
	MarkMessageRead(context.Context, MarkMessageReadRequest) (MessageResult, error)
	Snapshot(context.Context, SnapshotRequest) (Snapshot, error)
	Recover(context.Context, RecoverRequest) (RecoveryReport, error)
}

type RequestContext struct {
	Principal model.Principal
	RequestID model.RequestID
}

type CreateAgentRequest struct {
	Context model.Principal
	ID      model.AgentID
	Name    string
	Desired model.DesiredConfiguration
}

type UpdateAgentRequest struct {
	Context          model.Principal
	ID               model.AgentID
	ExpectedRevision model.Revision
	Name             string
	Desired          model.DesiredConfiguration
}

type AgentResult struct{ Agent model.Agent }

type CreateGroupRequest struct {
	Context model.Principal
	ID      model.GroupID
	Name    string
	Members []model.AgentID
}

type GroupResult struct{ Group model.Group }

type LaunchRequest struct {
	RequestContext
	AgentID          model.AgentID
	ExpectedRevision model.Revision
}

type InteractRequest struct {
	RequestContext
	ExecutionID model.ExecutionID
	Text        string
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
	AgentID          model.AgentID
	ConversationID   model.ConversationID
	ExpectedRevision model.Revision
}

type ChangeContextRequest struct {
	RequestContext
	ExecutionID model.ExecutionID
	Mode        ports.ContextChangeMode
}

type SendMessageRequest struct {
	RequestContext
	RecipientAgentIDs []model.AgentID
	Body              string
}

type MarkMessageReadRequest struct {
	RequestContext
	MessageID model.MessageID
	AgentID   model.AgentID
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
	Revision   model.Revision
	Agents     []model.Agent
	Groups     []model.Group
	Executions []model.Execution
	Operations []model.Operation
	Messages   []model.Message
}

type RecoverRequest struct{ Principal model.Principal }

type RecoveryReport struct {
	Controlled []model.ExecutionID
	Exited     []model.ExecutionID
	Unknown    []model.ExecutionID
}
