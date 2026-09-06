package model

import "time"

type Revision uint64

type PrincipalKind string

const (
	PrincipalOperator   PrincipalKind = "operator"
	PrincipalAgent      PrincipalKind = "agent"
	PrincipalExecution  PrincipalKind = "execution"
	PrincipalAutomation PrincipalKind = "automation"
)

type Principal struct {
	Kind          PrincipalKind
	AgentID       AgentID
	ExecutionID   ExecutionID
	Generation    AccessGeneration
	AutomationRun string
	Authority     AuthoritySubject
	Delegation    *AutomationDelegation
}

func AutomationPrincipal(run string, authority AuthoritySubject, delegation AutomationDelegation) Principal {
	return Principal{Kind: PrincipalAutomation, AutomationRun: run, Authority: authority, Delegation: &delegation}
}

func OperatorPrincipal() Principal { return Principal{Kind: PrincipalOperator} }

func AgentPrincipal(id AgentID) Principal { return Principal{Kind: PrincipalAgent, AgentID: id} }

func ExecutionPrincipal(executionID ExecutionID, agentID AgentID, generation AccessGeneration) Principal {
	subject := AuthoritySubject{Kind: AuthorityExecution, ExecutionID: executionID}
	if agentID != "" {
		subject = AuthoritySubject{Kind: AuthorityAgent, AgentID: agentID}
	}
	return Principal{Kind: PrincipalExecution, AgentID: agentID, ExecutionID: executionID, Generation: generation, Authority: subject}
}

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
	ID           GroupID
	Name         string
	Members      []AgentID
	OwnerAgentID AgentID
	Revision     Revision
	CreatedAt    time.Time
	UpdatedAt    time.Time
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
	// SandboxUnconfined is an explicit operator-selected absence of OS
	// confinement. It is never an implicit fallback from a confined mode.
	SandboxUnconfined     SandboxMode = "unconfined"
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

type ExecutionWorkloadKind string

const (
	ExecutionWorkloadHarness ExecutionWorkloadKind = "harness"
	ExecutionWorkloadShell   ExecutionWorkloadKind = "shell"
	ExecutionWorkloadProgram ExecutionWorkloadKind = "program"
)

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
	Workload           ExecutionWorkloadKind
	AgentID            AgentID
	ConversationID     ConversationID
	Spec               ResolvedExecutionSpec
	State              ExecutionState
	Attempt            AttemptGeneration
	ContextReadiness   ContextReadiness
	ContextOrder       string
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
	Workload         ExecutionWorkloadKind
	Attempt          AttemptGeneration
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
	OperationLaunch           OperationKind = "launch"
	OperationInteract         OperationKind = "interact"
	OperationAttach           OperationKind = "attach"
	OperationStop             OperationKind = "stop"
	OperationResume           OperationKind = "resume"
	OperationChangeContext    OperationKind = "change_context"
	OperationSendMessage      OperationKind = "send_message"
	OperationCreateWorkspace  OperationKind = "create_workspace"
	OperationRemoveWorkspace  OperationKind = "remove_workspace"
	OperationRestoreWorkspace OperationKind = "restore_workspace"
	OperationStartWork        OperationKind = "start_work"
	OperationCancelWork       OperationKind = "cancel_work"
	OperationStartShell       OperationKind = "start_shell"
	OperationRunProgram       OperationKind = "run_program"
	OperationAssignWork       OperationKind = "assign_work"
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
