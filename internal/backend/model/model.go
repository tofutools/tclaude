package model

import "time"

type Revision uint64

type PrincipalKind string

const (
	PrincipalOperator PrincipalKind = "operator"
	PrincipalAgent    PrincipalKind = "agent"
)

type Principal struct {
	Kind    PrincipalKind
	AgentID AgentID
}

func OperatorPrincipal() Principal { return Principal{Kind: PrincipalOperator} }

func AgentPrincipal(id AgentID) Principal { return Principal{Kind: PrincipalAgent, AgentID: id} }

type Agent struct {
	ID                 AgentID
	Name               string
	Desired            DesiredConfiguration
	PrimaryExecutionID ExecutionID
	Revision           Revision
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type Group struct {
	ID        GroupID
	Name      string
	Members   []AgentID
	Revision  Revision
	CreatedAt time.Time
	UpdatedAt time.Time
}

type DesiredConfiguration struct {
	Harness          string
	Model            string
	WorkingDirectory string
	Approval         ApprovalMode
	Sandbox          SandboxMode
}

type ApprovalMode string

const (
	ApprovalSupervised ApprovalMode = "supervised"
	ApprovalAutomatic  ApprovalMode = "automatic"
)

type SandboxMode string

const (
	SandboxReadOnly       SandboxMode = "read_only"
	SandboxWorkspaceWrite SandboxMode = "workspace_write"
)

type Conversation struct {
	ID        ConversationID
	Revision  Revision
	CreatedAt time.Time
	UpdatedAt time.Time
}

type ConversationAssociation struct {
	AgentID        AgentID
	ConversationID ConversationID
	Current        bool
	Revision       Revision
	AssociatedAt   time.Time
	ReplacedAt     *time.Time
}

// NativeConversationEvidence is a provider-observed history reference. The
// application chooses whether it may be used for continuation; a provider may
// not infer that choice from a platform ConversationID.
type NativeConversationEvidence struct {
	Namespace  string
	Reference  string
	ObservedAt time.Time
}

type ExecutionState string

const (
	ExecutionReserved ExecutionState = "reserved"
	ExecutionPrepared ExecutionState = "prepared"
	ExecutionReleased ExecutionState = "released"
	ExecutionRunning  ExecutionState = "running"
	ExecutionExited   ExecutionState = "exited"
	ExecutionFailed   ExecutionState = "failed"
	ExecutionUnknown  ExecutionState = "unknown"
)

type Execution struct {
	ID                 ExecutionID
	AgentID            AgentID
	ConversationID     ConversationID
	Spec               ResolvedExecutionSpec
	State              ExecutionState
	Evidence           ProviderEvidence
	NativeConversation *NativeConversationEvidence
	Revision           Revision
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// ResolvedExecutionSpec is immutable after an Execution is admitted. Provider
// implementations consume it but must not reinterpret desired configuration.
type ResolvedExecutionSpec struct {
	ExecutionID      ExecutionID
	AgentID          AgentID
	ConversationID   ConversationID
	Harness          string
	Model            string
	WorkingDirectory string
	Approval         ApprovalMode
	Sandbox          SandboxMode
}

type OperationKind string

const (
	OperationLaunch        OperationKind = "launch"
	OperationInteract      OperationKind = "interact"
	OperationAttach        OperationKind = "attach"
	OperationStop          OperationKind = "stop"
	OperationResume        OperationKind = "resume"
	OperationChangeContext OperationKind = "change_context"
	OperationSendMessage   OperationKind = "send_message"
)

type OperationState string

const (
	OperationAdmitted  OperationState = "admitted"
	OperationRunning   OperationState = "running"
	OperationSucceeded OperationState = "succeeded"
	OperationRefused   OperationState = "refused"
	OperationFailed    OperationState = "failed"
	OperationUncertain OperationState = "uncertain"
)

type Operation struct {
	ID          OperationID
	RequestID   RequestID
	Kind        OperationKind
	Principal   Principal
	ExecutionID ExecutionID
	State       OperationState
	ResultCode  string
	Detail      string
	Revision    Revision
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type Message struct {
	ID         MessageID
	Sender     Principal
	Body       string
	Recipients []MessageRecipient
	CreatedAt  time.Time
}

type MessageRecipient struct {
	ID       RecipientID
	AgentID  AgentID
	ReadAt   *time.Time
	Notified bool
}
