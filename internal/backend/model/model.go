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

type AgentDisplayLabels struct {
	Role        string
	Description string
}

type AgentLabels struct {
	Role        string
	Description string
	Groups      map[GroupID]AgentDisplayLabels `json:",omitempty"`
}

type Agent struct {
	Labels               AgentLabels
	ID                   AgentID
	Name                 string
	TaskReference        string
	ParentAgentID        AgentID
	CloneSourceAgentID   AgentID
	Lifecycle            AgentLifecycleState
	RetiredAt            *time.Time
	RetiredBy            Principal
	RetirementReason     string
	Notifications        AgentNotificationPreferences
	Desired              DesiredConfiguration
	ConfigurationProfile *ConfigurationProfileRef
	PrimaryExecutionID   ExecutionID
	Revision             Revision
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type AgentLifecycleState string

const (
	AgentActive  AgentLifecycleState = "active"
	AgentRetired AgentLifecycleState = "retired"
)

type AgentNotificationPreferences struct {
	DirectMessage NotificationIntent
}

type Group struct {
	MaxActiveMembers int64
	Details          *GroupDetails `json:",omitempty"`
	ParentGroupID    GroupID       `json:",omitempty"`
	ID               GroupID
	Name             string
	Members          []AgentID
	OwnerAgentID     AgentID
	OwnerAgentIDs    []AgentID `json:",omitempty"`
	Revision         Revision
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type DesiredConfiguration struct {
	HostSandbox *SandboxSelection `json:",omitempty"`
	Environment Environment       `json:",omitempty"`
	Harness     string
	Model       string
	// Effort is the requested native reasoning effort or variant, not observed effective effort.
	Effort            string            `json:",omitempty"`
	ToolGovernance    ToolGovernance    `json:",omitempty"`
	FastMode          FastMode          `json:",omitempty"`
	AutoReview        bool              `json:",omitempty"`
	AutoMemory        bool              `json:",omitempty"`
	PeerMessaging     bool              `json:",omitempty"`
	TrustDirectory    bool              `json:",omitempty"`
	AutoCompactWindow AutoCompactWindow `json:",omitempty"`
	WorkingDirectory  string
	Approval          ApprovalMode
	Sandbox           SandboxMode
}

type ApprovalMode string

const (
	ApprovalSupervised ApprovalMode = "supervised"
	ApprovalAutomatic  ApprovalMode = "automatic"
	// ApprovalDeny is the OpenCode unattended approval policy. Its audited tool
	// baseline remains separate from edit/web approval decisions.
	ApprovalDeny              ApprovalMode = "deny"
	ApprovalAllowTools        ApprovalMode = "allow-tools"
	ApprovalYolo              ApprovalMode = "yolo"
	ApprovalAsk               ApprovalMode = "ask"
	ApprovalNever             ApprovalMode = "never"
	ApprovalOnRequest         ApprovalMode = "on-request"
	ApprovalOnFailure         ApprovalMode = "on-failure"
	ApprovalUntrusted         ApprovalMode = "untrusted"
	ApprovalInherit           ApprovalMode = "inherit"
	ApprovalDefault           ApprovalMode = "default"
	ApprovalManual            ApprovalMode = "manual"
	ApprovalPlan              ApprovalMode = "plan"
	ApprovalAcceptEdits       ApprovalMode = "acceptEdits"
	ApprovalAuto              ApprovalMode = "auto"
	ApprovalDontAsk           ApprovalMode = "dontAsk"
	ApprovalBypassPermissions ApprovalMode = "bypassPermissions"
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
	// ContextUsage is a query projection of persisted provider evidence, never authored state.
	ContextUsage       *ContextUsage `json:",omitempty"`
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
	HostSandbox          *SandboxSelection    `json:",omitempty"`
	ShellGroup           *ShellGroupSelection `json:",omitempty"`
	Environment          Environment          `json:",omitempty"`
	ConfigurationProfile *ConfigurationProfileRef
	ExecutionID          ExecutionID
	Workload             ExecutionWorkloadKind
	Attempt              AttemptGeneration
	AgentID              AgentID
	ConversationID       ConversationID
	Harness              string
	Model                string
	Effort               string            `json:",omitempty"`
	ToolGovernance       ToolGovernance    `json:",omitempty"`
	FastMode             FastMode          `json:",omitempty"`
	AutoReview           bool              `json:",omitempty"`
	AutoMemory           bool              `json:",omitempty"`
	PeerMessaging        bool              `json:",omitempty"`
	TrustDirectory       bool              `json:",omitempty"`
	AutoCompactWindow    AutoCompactWindow `json:",omitempty"`
	WorkingDirectory     string
	Approval             ApprovalMode
	Sandbox              SandboxMode
}

type OperationKind string

const (
	OperationLaunch            OperationKind = "launch"
	OperationInteract          OperationKind = "interact"
	OperationStageTerminalFile OperationKind = "stage_terminal_file"
	OperationAttach            OperationKind = "attach"
	OperationStop              OperationKind = "stop"
	OperationResume            OperationKind = "resume"
	OperationChangeContext     OperationKind = "change_context"
	OperationSendMessage       OperationKind = "send_message"
	OperationCreateWorkspace   OperationKind = "create_workspace"
	OperationRemoveWorkspace   OperationKind = "remove_workspace"
	OperationRestoreWorkspace  OperationKind = "restore_workspace"
	OperationStartWork         OperationKind = "start_work"
	OperationCancelWork        OperationKind = "cancel_work"
	OperationStartShell        OperationKind = "start_shell"
	OperationRunProgram        OperationKind = "run_program"
	OperationAssignWork        OperationKind = "assign_work"
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
	ID                   MessageID
	Sender               Principal
	SenderConversationID ConversationID
	Subject              string
	Body                 string
	ParentMessageID      MessageID
	ThreadID             MessageID
	Recipients           []MessageRecipient
	Attachments          []Attachment
	CreatedAt            time.Time
}

type MessageAddressKind string

const (
	MessageAddressAgent    MessageAddressKind = "agent"
	MessageAddressOperator MessageAddressKind = "operator"
)

type MessageAudienceKind string

const (
	MessageAudienceTo MessageAudienceKind = "to"
	MessageAudienceCC MessageAudienceKind = "cc"
)

type NotificationIntent string

const (
	NotificationNone        NotificationIntent = "none"
	NotificationIfAvailable NotificationIntent = "if_available"
)

type NotificationOutcome string

const (
	NotificationNotRequested NotificationOutcome = "not_requested"
	NotificationPending      NotificationOutcome = "pending"
	NotificationUnknown      NotificationOutcome = "unknown"
	NotificationDelivered    NotificationOutcome = "delivered"
	NotificationUnavailable  NotificationOutcome = "unavailable"
	NotificationFailed       NotificationOutcome = "failed"
)

type MessageRecipient struct {
	ID                  RecipientID
	AddressKind         MessageAddressKind
	AgentID             AgentID
	Audience            MessageAudienceKind
	ReadAt              *time.Time
	NotificationIntent  NotificationIntent
	NotificationOutcome NotificationOutcome
	NotificationDetail  string
	NotifiedAt          *time.Time
}

type Attachment struct {
	ID        AttachmentID
	Filename  string
	MediaType string
	Size      int64
	SHA256    string
	CreatedAt time.Time
}

type AttachmentClaim struct {
	ID         AttachmentClaimID
	Attachment Attachment
	Owner      Principal
	ExpiresAt  time.Time
	CreatedAt  time.Time
}
