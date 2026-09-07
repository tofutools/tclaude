package model

import (
	"encoding/json"
	"time"
)

// DefinitionKind distinguishes reusable authored team deployment from work
// process definitions. Revisions are immutable; Definition.Revision is the
// CAS-protected mutable head.
type DefinitionKind string

const (
	DefinitionTeam    DefinitionKind = "team"
	DefinitionProcess DefinitionKind = "process"
)

type ParameterType string

const (
	ParameterString  ParameterType = "string"
	ParameterNumber  ParameterType = "number"
	ParameterBoolean ParameterType = "boolean"
	ParameterObject  ParameterType = "object"
	ParameterArray   ParameterType = "array"
)

type ParameterDeclaration struct {
	Name        string
	Type        ParameterType
	Required    bool
	Description string
	Default     json.RawMessage
}

type DefinitionRef struct {
	DefinitionID DefinitionID
	RevisionID   DefinitionRevisionID
	ContentHash  string
	Kind         DefinitionKind
}

type Definition struct {
	ID             DefinitionID
	Name           string
	Kind           DefinitionKind
	HeadRevisionID DefinitionRevisionID
	Tombstoned     bool
	Revision       Revision
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// DefinitionRevision contains both preserved authoring source and the
// normalized executable representation. Dependencies are the complete pinned
// closure, in deterministic dependency-before-dependent order.
type DefinitionRevision struct {
	ID              DefinitionRevisionID
	DefinitionID    DefinitionID
	Number          Revision
	ContentHash     string
	SchemaVersion   uint32
	CompilerVersion string
	Source          string
	Parameters      []ParameterDeclaration
	Team            *TeamDefinition
	Process         *ProcessDefinition
	Dependencies    []DefinitionRef
	Author          Principal
	RequestID       RequestID
	CreatedAt       time.Time
}

type WorkspacePolicy string

const (
	WorkspacePolicyShared    WorkspacePolicy = "shared"
	WorkspacePolicyPerMember WorkspacePolicy = "per_member"
)

type TeamDefinition struct {
	Members         []TeamMemberSpec
	Waves           []TeamWave
	Briefings       []TeamBriefing
	WorkspacePolicy WorkspacePolicy
	AdvisoryPhases  []string
	Automation      []AutomationRuleRef
}

type TeamMemberSpec struct {
	Key         string
	Name        string
	Desired     DesiredConfiguration
	Roles       []RoleID
	Owner       bool
	Required    bool
	BriefingIDs []string
}

type TeamWave struct {
	ID             string
	MemberKeys     []string
	DependsOn      []string
	RequiredReady  bool
	RequiredBriefs bool
}

type BriefingTiming string

const (
	// BriefingBeforeFirstWork must be supplied through a provider's prepared
	// launch input. A post-release message never satisfies this contract.
	BriefingBeforeFirstWork BriefingTiming = "before_first_work"
	BriefingAfterReady      BriefingTiming = "after_ready"
)

type TeamBriefing struct {
	ID         string
	Body       string
	Timing     BriefingTiming
	Required   bool
	MemberKeys []string
}

type ProcessDefinition struct {
	Graph WorkGraph
}

type WorkGraph struct {
	CompilerVersion string
	EntryNodeID     WorkNodeID
	Nodes           []WorkNode
	Edges           []WorkEdge
	Outcome         WorkGraphOutcomePolicy
}

type WorkNodeKind string

const (
	WorkNodeTask     WorkNodeKind = "task"
	WorkNodeDecision WorkNodeKind = "decision"
	WorkNodeFork     WorkNodeKind = "fork"
	WorkNodeJoin     WorkNodeKind = "join"
	WorkNodeWait     WorkNodeKind = "wait"
	WorkNodeEnd      WorkNodeKind = "end"
)

type WorkNode struct {
	ID        WorkNodeID
	Kind      WorkNodeKind
	Name      string
	Performer *Performer
	Input     map[string]json.RawMessage
	Retry     RetryPolicy
	Decision  *DecisionNode
	Join      *JoinPolicy
	Wait      *WaitPolicy
	End       *EndPolicy
	Waivable  bool
}

type WorkEdge struct {
	From    WorkNodeID
	To      WorkNodeID
	Verdict string
}

type PerformerKind string

const (
	PerformerAgent   PerformerKind = "agent"
	PerformerProgram PerformerKind = "program"
	PerformerHuman   PerformerKind = "human"
)

type Performer struct {
	Kind    PerformerKind
	Agent   *AgentPerformer
	Program *ProgramPerformer
	Human   *HumanPerformer
}

type AgentContextPolicy string

const (
	AgentContextFresh AgentContextPolicy = "fresh"
	AgentContextReuse AgentContextPolicy = "reuse"
)

type AgentPerformer struct {
	AgentID       AgentID
	MemberKey     string
	CreateDesired *DesiredConfiguration
	ContextPolicy AgentContextPolicy
	Brief         string
}

type ProgramProfileRef struct {
	ProfileID   ProgramProfileID
	RevisionID  ProgramProfileRevisionID
	ContentHash string
}

type ProgramPerformer struct {
	Profile   ProgramProfileRef
	Arguments []string
	Input     json.RawMessage
}

type HumanPerformer struct {
	AgentID AgentID
	RoleID  RoleID
	Prompt  string
}

type RetryPolicy struct {
	MaxAttempts   uint32
	Backoff       time.Duration
	Retryable     []string
	AttemptBudget time.Duration
}

const (
	RetryableProgramFailure = "program_failed"
	RetryableAgentRejection = "agent_rejected"
	RetryableHumanRejection = "human_rejected"
)

type BlockedResolutionAction string

const (
	BlockedRetry  BlockedResolutionAction = "retry"
	BlockedRework BlockedResolutionAction = "rework"
	BlockedWaive  BlockedResolutionAction = "waive"
	BlockedCancel BlockedResolutionAction = "cancel"
)

type DecisionNode struct {
	Kind             DecisionKind
	Audience         []DecisionAudience
	PermittedAnswers []string
	ExpiresAfter     time.Duration
}

type JoinMode string

const (
	JoinAll JoinMode = "all"
	JoinAny JoinMode = "any"
)

type JoinPolicy struct {
	Mode JoinMode
}

type WaitPolicy struct {
	Duration time.Duration
	Until    string
}

type EndPolicy struct {
	Outcome WorkOutcome
}

type WorkGraphOutcomePolicy struct {
	RequiredNodes    []WorkNodeID
	ArtifactRevision string
	HumanJudgment    bool
}

// ProgramProfileRevision is the only way authored work may request a host
// program. It contains bounded argv/environment/output and the exact effect
// authority that must be accepted at run admission and rechecked at release.
type ProgramProfile struct {
	ID             ProgramProfileID
	Name           string
	HeadRevisionID ProgramProfileRevisionID
	Tombstoned     bool
	Revision       Revision
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type ProgramProfileRevision struct {
	ID               ProgramProfileRevisionID
	ProfileID        ProgramProfileID
	Number           Revision
	ContentHash      string
	Executable       string
	ArgumentPrefix   []string
	Environment      map[string]string
	WorkingDirectory string
	Sandbox          SandboxMode
	Timeout          time.Duration
	OutputLimitBytes int64
	EffectAuthority  []ProgramEffectRequirement
	Author           Principal
	RequestID        RequestID
	CreatedAt        time.Time
}

type ProgramEffectRequirement struct {
	Action                 Action
	Resource               ResourceSelector
	RequestedConfiguration *DesiredConfiguration
}

type WorkScope struct {
	WorkspaceID  WorkspaceID
	GroupID      GroupID
	DeploymentID DeploymentID
	RuleID       AutomationRuleID
	OccurrenceID OccurrenceID
}

type WorkStart struct {
	RequestID                 RequestID
	Definition                *DefinitionRef
	InlineGraph               *WorkGraph
	Parameters                map[string]json.RawMessage
	Scope                     WorkScope
	PerformerBindings         map[string]Performer
	AuthorizedProgramProfiles []ProgramProfileRef
	Deadline                  time.Time
}

type WorkControlState string

const (
	WorkControlActive     WorkControlState = "active"
	WorkControlWaiting    WorkControlState = "waiting"
	WorkControlDraining   WorkControlState = "draining"
	WorkControlSuppressed WorkControlState = "suppressed"
	WorkControlSettled    WorkControlState = "settled"
)

type WorkOutcome string

const (
	WorkOutcomeNone      WorkOutcome = ""
	WorkOutcomeVerified  WorkOutcome = "verified"
	WorkOutcomeWaived    WorkOutcome = "waived"
	WorkOutcomeRejected  WorkOutcome = "rejected"
	WorkOutcomeCancelled WorkOutcome = "cancelled"
	WorkOutcomeExpired   WorkOutcome = "expired"
	WorkOutcomeUnknown   WorkOutcome = "unknown"
)

type WorkAttemptRef struct {
	RunID        WorkRunID
	NodeID       WorkNodeID
	ActivationID WorkActivationID
	Attempt      uint32
	IssuanceID   WorkIssuanceID
}

type WorkNodeAttemptState string

const (
	NodeAttemptReady      WorkNodeAttemptState = "ready"
	NodeAttemptAdmitted   WorkNodeAttemptState = "admitted"
	NodeAttemptRunning    WorkNodeAttemptState = "running"
	NodeAttemptWaiting    WorkNodeAttemptState = "waiting"
	NodeAttemptRetryWait  WorkNodeAttemptState = "retry_wait"
	NodeAttemptBlocked    WorkNodeAttemptState = "blocked"
	NodeAttemptSucceeded  WorkNodeAttemptState = "succeeded"
	NodeAttemptFailed     WorkNodeAttemptState = "failed"
	NodeAttemptWaived     WorkNodeAttemptState = "waived"
	NodeAttemptSuppressed WorkNodeAttemptState = "suppressed"
	NodeAttemptUncertain  WorkNodeAttemptState = "uncertain"
)

type WorkNodeAttempt struct {
	Ref         WorkAttemptRef
	State       WorkNodeAttemptState
	Performer   *Performer
	OperationID OperationID
	ExecutionID ExecutionID
	ReadyAt     time.Time
	RetryAt     *time.Time
	Deadline    time.Time
	RetryBudget uint32
	JoinWinner  WorkActivationID
	DecisionID  DecisionID
	Outcome     WorkOutcome
	Detail      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	SettledAt   *time.Time
}

type WorkNodeEvidence struct {
	ID               WorkEvidenceID
	RequestID        RequestID
	Attempt          WorkAttemptRef
	Reporter         Principal
	Kind             WorkEvidenceKind
	ArtifactRevision string
	Passed           *bool
	Disposition      WorkOutcome
	Detail           string
	RecordedAt       time.Time
	Revision         Revision
}

type DecisionKind string

const (
	DecisionAccess  DecisionKind = "access"
	DecisionWork    DecisionKind = "work"
	DecisionBlocked DecisionKind = "blocked"
)

type DecisionState string

const (
	DecisionOpen     DecisionState = "open"
	DecisionAnswered DecisionState = "answered"
	DecisionExpired  DecisionState = "expired"
)

type DecisionWindow struct {
	ID               DecisionID
	Kind             DecisionKind
	SourceRevision   Revision
	Attempt          WorkAttemptRef
	Audience         []DecisionAudience
	Question         string
	PermittedAnswers []string
	EvidenceRefs     []WorkEvidenceID
	ExpiresAt        time.Time
	State            DecisionState
	Revision         Revision
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type DecisionAudience struct {
	Subject AuthoritySubject
	RoleID  RoleID
	GroupID GroupID
}
type DecisionSubmission struct {
	RequestID              RequestID
	DecisionID             DecisionID
	ExpectedWindowRevision Revision
	Answer                 string
	Reason                 string
	EvidenceRefs           []WorkEvidenceID
	Actor                  Principal
	SubmittedAt            time.Time
}

type DeploymentState string

const (
	DeploymentDeploying    DeploymentState = "deploying"
	DeploymentPartial      DeploymentState = "partial"
	DeploymentReady        DeploymentState = "ready"
	DeploymentStandingDown DeploymentState = "standing_down"
	DeploymentStopped      DeploymentState = "stopped"
)

type TeamDeployment struct {
	ID                DeploymentID
	Definition        DefinitionRef
	DependencyClosure []DefinitionRef
	Mission           string
	Parameters        map[string]json.RawMessage
	GroupID           GroupID
	Members           map[string]AgentID
	AutomationRuleIDs []AutomationRuleID
	WorkRunID         WorkRunID
	AdvisoryPhase     uint32
	State             DeploymentState
	Revision          Revision
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type AutomationConditionKind string

const (
	AutomationSchedule      AutomationConditionKind = "schedule"
	AutomationTrigger       AutomationConditionKind = "trigger"
	AutomationStandingOrder AutomationConditionKind = "standing_order"
)

type AutomationRule struct {
	ID             AutomationRuleID
	Name           string
	HeadRevisionID AutomationRuleRevisionID
	Enabled        bool
	Tombstoned     bool
	Revision       Revision
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type AutomationRuleRef struct {
	RuleID      AutomationRuleID
	RevisionID  AutomationRuleRevisionID
	ContentHash string
}

type AutomationRuleRevision struct {
	ID           AutomationRuleRevisionID
	RuleID       AutomationRuleID
	Number       Revision
	ContentHash  string
	Owner        AuthoritySubject
	Delegation   AutomationDelegation
	Condition    AutomationCondition
	Action       AutomationAction
	Policy       OccurrencePolicy
	Dependencies []DefinitionRef
	Author       Principal
	RequestID    RequestID
	CreatedAt    time.Time
}

type AutomationCondition struct {
	Kind          AutomationConditionKind
	Schedule      *ScheduleCondition
	Trigger       *TriggerCondition
	StandingOrder *StandingOrderCondition
}

type ScheduleCondition struct {
	Timezone string
	Cron     string
	Interval time.Duration
	Anchor   time.Time
}

type TriggerCondition struct {
	SourceID  string
	Resource  AutomationFactResource
	FactKind  string
	Values    []string
	Dwell     time.Duration
	Cooldown  time.Duration
	Debounce  time.Duration
	Freshness time.Duration
}

type AutomationFactResourceKind string

const (
	FactResourceOperation         AutomationFactResourceKind = "operation"
	FactResourceMessage           AutomationFactResourceKind = "message"
	FactResourceWork              AutomationFactResourceKind = "work"
	FactResourceAgent             AutomationFactResourceKind = "agent"
	FactResourceRepositoryPullReq AutomationFactResourceKind = "repository_pull_request"
)

type AutomationFactResource struct {
	Kind        AutomationFactResourceKind
	ID          string
	Repository  string
	PullRequest uint64
}

const (
	AutomationSourceApplication = "application"
	FactOperationSucceeded      = "operation.succeeded"
	FactOperationFailed         = "operation.failed"
	FactMessageDelivered        = "message.delivered"
	FactMessageDenied           = "message.denied"
	FactWorkSucceeded           = "work.succeeded"
	FactWorkFailed              = "work.failed"
	FactWorkCancelled           = "work.cancelled"
	FactAgentAwaitingInput      = "agent.awaiting_input"
	FactAgentIdle               = "agent.idle"
	FactPullRequestChanged      = "pull_request.changed"
	FactCICompleted             = "ci.completed"
)

type StandingOrderTiming string

const (
	StandingOrderSameContinuation StandingOrderTiming = "same_continuation"
	StandingOrderNextTurn         StandingOrderTiming = "next_turn"
)

type StandingOrderCondition struct {
	FactKind         string
	Pattern          string
	Timing           StandingOrderTiming
	DispatchDeadline time.Duration
}

type AutomationActionKind string

const (
	AutomationSendMessage AutomationActionKind = "message"
	AutomationStartWork   AutomationActionKind = "work"
	AutomationDeployTeam  AutomationActionKind = "team"
)

type AutomationAction struct {
	Kind    AutomationActionKind
	Message *AutomationMessageAction
	Work    *WorkStart
	Team    *TeamInstantiation
}

type AutomationMessageAction struct {
	Body     string
	AgentIDs []AgentID
	GroupID  GroupID
	RoleID   RoleID
}

type TeamInstantiation struct {
	Definition DefinitionRef
	Mission    string
	Parameters map[string]json.RawMessage
	// Target makes reinforcement explicit. A legacy GroupID without Target is
	// accepted only as a new-group request; it never selects an existing group.
	Target     TeamDeploymentTarget
	Workspaces TeamWorkspaceSelection
	// GroupID is the temporary new-group compatibility input.
	GroupID GroupID
}

type TeamDeploymentTargetKind string

const (
	TeamTargetNewGroup      TeamDeploymentTargetKind = "new_group"
	TeamTargetExistingGroup TeamDeploymentTargetKind = "existing_group"
)

type TeamDeploymentTarget struct {
	Kind    TeamDeploymentTargetKind
	GroupID GroupID
}

// TeamWorkspaceInput authors either a new owned checkout or use of an exact
// existing workspace revision. These choices are mutually exclusive; the
// application never derives a filesystem path from a team or member name.
type TeamWorkspaceInput struct {
	WorkspaceID      WorkspaceID
	ExpectedRevision Revision
	CreateIntent     *WorkspaceIntent
}

type TeamWorkspaceSelection struct {
	Shared  *TeamWorkspaceInput
	Members map[string]TeamWorkspaceInput
}

type MissedTickPolicy string
type OfflineDeliveryPolicy string
type OverlapPolicy string

const (
	MissedTickSkip     MissedTickPolicy      = "skip"
	MissedTickCoalesce MissedTickPolicy      = "coalesce_latest"
	OfflineSkip        OfflineDeliveryPolicy = "skip"
	OfflineQueue       OfflineDeliveryPolicy = "queue"
	OverlapForbid      OverlapPolicy         = "forbid"
	OverlapAllow       OverlapPolicy         = "allow"
	OverlapReplace     OverlapPolicy         = "replace"
)

type OccurrencePolicy struct {
	MissedTicks     MissedTickPolicy
	OfflineDelivery OfflineDeliveryPolicy
	ExpiresAfter    time.Duration
	Overlap         OverlapPolicy
	MaxActive       uint32
	Deadline        time.Duration
	Retry           RetryPolicy
}

type NormalizedFact struct {
	Sequence           uint64
	Source             string
	EventID            string
	Cursor             string
	Kind               string
	Value              string
	OccurredAt         time.Time
	ObservedAt         time.Time
	ExecutionID        ExecutionID
	ExecutionRevision  Revision
	ContextRevision    Revision
	ResourceRefs       []string
	Resource           AutomationFactResource
	ParentOccurrenceID OccurrenceID
	CausalDepth        uint32
	Payload            json.RawMessage
}

type OccurrenceState string

const (
	OccurrencePending   OccurrenceState = "pending"
	OccurrenceAdmitted  OccurrenceState = "admitted"
	OccurrencePartial   OccurrenceState = "partial"
	OccurrenceDelivered OccurrenceState = "delivered"
	OccurrenceParked    OccurrenceState = "parked"
	OccurrenceExpired   OccurrenceState = "expired"
	OccurrenceDenied    OccurrenceState = "denied"
	OccurrenceUncertain OccurrenceState = "uncertain"
)

type AutomationOccurrence struct {
	ID                  OccurrenceID
	RuleID              AutomationRuleID
	RuleRevisionID      AutomationRuleRevisionID
	SourceOccurrenceKey string
	RequestID           RequestID
	Requester           Principal
	ParentOccurrenceID  OccurrenceID
	CausalDepth         uint32
	ScheduledAt         time.Time
	EventAt             time.Time
	EligibleAt          time.Time
	ExpiresAt           time.Time
	State               OccurrenceState
	Recipients          []OccurrenceRecipient
	OperationID         OperationID
	WorkRunID           WorkRunID
	DeploymentID        DeploymentID
	Revision            Revision
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

type RecipientDisposition string

const (
	RecipientPending   RecipientDisposition = "pending"
	RecipientAdmitted  RecipientDisposition = "admitted"
	RecipientDelivered RecipientDisposition = "delivered"
	RecipientSkipped   RecipientDisposition = "skipped"
	RecipientQueued    RecipientDisposition = "queued"
	RecipientDenied    RecipientDisposition = "denied"
	RecipientExpired   RecipientDisposition = "expired"
	RecipientUncertain RecipientDisposition = "uncertain"
)

type OccurrenceRecipient struct {
	AgentID     AgentID
	Disposition RecipientDisposition
	OperationID OperationID
	Detail      string
}

type AutomationConditionState struct {
	RuleID          AutomationRuleID
	SourceCursor    string
	DwellEpisodeID  string
	DwellSince      *time.Time
	CooldownUntil   *time.Time
	DebounceAt      *time.Time
	DebouncePayload json.RawMessage
	ObservedAt      time.Time
	Revision        Revision
}
