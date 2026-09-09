package model

import "time"

// Action names an application capability. Providers and hosts transport
// admitted effects but never interpret actions or grants.
type Action string

const (
	ActionReadIdentity         Action = "identity.read"
	ActionReadStatus           Action = "status.read"
	ActionReadInbox            Action = "inbox.read"
	ActionMarkInboxRead        Action = "inbox.mark_read"
	ActionSendMessage          Action = "message.send"
	ActionLaunch               Action = "execution.launch"
	ActionInteract             Action = "execution.interact"
	ActionReadExecutionFile    Action = "execution.file.read"
	ActionStageTerminalFile    Action = "execution.file.stage"
	ActionAttach               Action = "execution.attach"
	ActionStop                 Action = "execution.stop"
	ActionChangeContext        Action = "execution.context.change"
	ActionUpdateConfiguration  Action = "agent.configuration.update"
	ActionRetireAgent          Action = "agent.retire"
	ActionReactivateAgent      Action = "agent.reactivate"
	ActionManageMembership     Action = "group.membership.manage"
	ActionCreateGroupMember    Action = "group.members.create"
	ActionDisbandGroup         Action = "group.disband"
	ActionReadAttachment       Action = "attachment.read"
	ActionReadHistory          Action = "history.read"
	ActionRefreshHistory       Action = "history.refresh"
	ActionReadUsage            Action = "usage.read"
	ActionRefreshUsage         Action = "usage.refresh"
	ActionReadActivity         Action = "activity.read"
	ActionSetHistoryMetadata   Action = "history.metadata.set"
	ActionRegisterWorkspace    Action = "workspace.register"
	ActionCreateWorkspace      Action = "workspace.create"
	ActionInspectWorkspace     Action = "workspace.inspect"
	ActionRemoveWorkspace      Action = "workspace.remove"
	ActionRestoreWorkspace     Action = "workspace.restore"
	ActionStartWork            Action = "work.start"
	ActionRecordWorkEvidence   Action = "work.evidence.record"
	ActionDecideWork           Action = "work.decide"
	ActionCancelWork           Action = "work.cancel"
	ActionResolveWork          Action = "work.resolve"
	ActionStartShell           Action = "shell.start"
	ActionReadDefinition       Action = "definition.read"
	ActionManageDefinition     Action = "definition.manage"
	ActionManageProgramProfile Action = "program_profile.manage"
	ActionReadProgramProfile   Action = "program_profile.read"
	ActionExecuteProgram       Action = "program.execute"
	ActionManageAutomation     Action = "automation.manage"
	ActionReadAutomation       Action = "automation.read"
	ActionRunAutomation        Action = "automation.run"
)

type AuthoritySubjectKind string

const (
	AuthorityOperator  AuthoritySubjectKind = "operator"
	AuthorityAgent     AuthoritySubjectKind = "agent"
	AuthorityExecution AuthoritySubjectKind = "execution"
)

// AuthoritySubject is stable Agent authority or authority scoped to one
// standalone Execution. Managed execution callers derive their subject from
// their immutable Agent association.
type AuthoritySubject struct {
	Kind        AuthoritySubjectKind
	AgentID     AgentID
	ExecutionID ExecutionID
}

type ResourceSelectorKind string

const (
	// ResourceAll is an operator-authored unscoped grant for one action.
	ResourceAll            ResourceSelectorKind = "all"
	ResourceSelf           ResourceSelectorKind = "self"
	ResourceOperator       ResourceSelectorKind = "operator"
	ResourceAgent          ResourceSelectorKind = "agent"
	ResourceExecution      ResourceSelectorKind = "execution"
	ResourceGroup          ResourceSelectorKind = "group"
	ResourceGroupPeers     ResourceSelectorKind = "group_members"
	ResourceConversation   ResourceSelectorKind = "conversation"
	ResourceWorkspace      ResourceSelectorKind = "workspace"
	ResourceWorkRun        ResourceSelectorKind = "work_run"
	ResourceDefinition     ResourceSelectorKind = "definition"
	ResourceProgramProfile ResourceSelectorKind = "program_profile"
	ResourceAutomationRule ResourceSelectorKind = "automation_rule"
)

// ResourceSelector is deliberately typed. Exactly the field named by Kind is
// populated; Self and All carry no ID. Self expands from the authenticated
// caller at evaluation time. All is explicit unscoped reach for one action.
type ResourceSelector struct {
	Kind             ResourceSelectorKind
	AgentID          AgentID
	ExecutionID      ExecutionID
	GroupID          GroupID
	ConversationID   ConversationID
	WorkspaceID      WorkspaceID
	WorkRunID        WorkRunID
	DefinitionID     DefinitionID
	ProgramProfileID ProgramProfileID
	AutomationRuleID AutomationRuleID
}

type GrantID string

func (id GrantID) Validate() error { return ValidateStableID("grant id", string(id)) }

// AuthorityGrant is an additive, operator-authored capability. Group
// membership and expiry are evaluated live; a stored grant is not an access
// token and providers never receive it.
type AuthorityGrant struct {
	Scope     PermissionScope `json:",omitempty"`
	ID        GrantID
	Subject   AuthoritySubject
	Action    Action
	Resource  ResourceSelector
	Bounds    ConfigurationBounds
	ExpiresAt *time.Time
	Revision  Revision
	CreatedAt time.Time
	UpdatedAt time.Time
}

type RoleID string

func (id RoleID) Validate() error { return ValidateStableID("role id", string(id)) }

const GroupOwnerRole RoleID = "group_owner"

type Role struct {
	Description string `json:",omitempty"`
	Brief       string `json:",omitempty"`
	ID          RoleID
	Name        string
	Actions     []Action
	Revision    Revision
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type RoleAssignment struct {
	RoleID    RoleID
	Subject   AuthoritySubject
	Resource  ResourceSelector
	Bounds    ConfigurationBounds
	Revision  Revision
	CreatedAt time.Time
	UpdatedAt time.Time
}

type AuthoritySourceKind string

const (
	AuthorityDenied  AuthoritySourceKind = "deny"
	AuthorityDefault AuthoritySourceKind = "default"
	AuthorityDirect  AuthoritySourceKind = "grant"
	AuthorityRole    AuthoritySourceKind = "role"
)

type AuthorityDecision struct {
	Allowed    bool
	Action     Action
	Resource   ResourceSelector
	SourceKind AuthoritySourceKind
	SourceID   string
	Revision   Revision
	Bounds     ConfigurationBounds
}

type AuthorityRequest struct {
	SpawnLineage           *SpawnLineage     `json:"-"`
	RequestedHostSandbox   *SandboxSelection `json:",omitempty"`
	Principal              Principal
	Action                 Action
	Resource               ResourceSelector
	RequestedConfiguration *DesiredConfiguration
	RequestedEnvironment   *Environment `json:",omitempty"`
}

// AutomationDelegation is an application-authenticated run fixture, not a
// scheduler. Its accepted scope is intersected with the authority subject's
// current grants for every effect.
type AutomationDelegation struct {
	// NoExpiry explicitly retains delegation until the rule is disabled or current authority is revoked.
	NoExpiry  bool `json:",omitempty"`
	Actions   []Action
	Resources []ResourceSelector
	Bounds    ConfigurationBounds
	ExpiresAt time.Time
}

// ConfigurationBounds are an allow-list, not advisory metadata. Delegated
// launch/configuration authority must match every populated dimension. Empty
// bounds grant no configuration-bearing effect; operator authority is the only
// unbounded case.
type ConfigurationBounds struct {
	// AutoReview permits an explicit native classifier approval opt-in. Older grants do not grant it.
	AutoReview bool `json:",omitempty"`
	// HostSandboxProfiles permits stable sandbox profile IDs. Profile edits take effect on the next launch. An absent list permits only no host profile.
	HostSandboxProfiles []string `json:",omitempty"`
	// Environments permits exact authored sets; absent permits only empty environment.
	Environments          []Environment `json:",omitempty"`
	Harnesses             []string
	Models                []string
	WorkingDirectoryRoots []string
	ApprovalModes         []ApprovalMode
	SandboxModes          []SandboxMode
}

type AccessGeneration uint64

type ExecutionAccessState string

const (
	ExecutionAccessInactive  ExecutionAccessState = "inactive"
	ExecutionAccessActive    ExecutionAccessState = "active"
	ExecutionAccessSuspended ExecutionAccessState = "suspended"
	ExecutionAccessRevoked   ExecutionAccessState = "revoked"
	ExecutionAccessExpired   ExecutionAccessState = "expired"
)

// ExecutionAccess is durable credential lifecycle state. CredentialDigest is
// never returned through public application queries; bearer plaintext is not
// durable domain state.
type ExecutionAccess struct {
	ExecutionID      ExecutionID
	AgentID          AgentID
	Generation       AccessGeneration
	CredentialDigest []byte
	DeliveryID       string
	FileIdentity     string
	State            ExecutionAccessState
	IssuedAt         time.Time
	ExpiresAt        time.Time
	RevokedAt        *time.Time
	Revision         Revision
}

// ExecutionAccessBinding is the non-secret lifecycle view shared with the
// host/provider boundary.
type ExecutionAccessBinding struct {
	ExecutionID ExecutionID
	Generation  AccessGeneration
	DeliveryID  string
	State       ExecutionAccessState
	ExpiresAt   time.Time
	Revision    Revision
}
