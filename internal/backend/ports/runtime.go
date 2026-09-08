package ports

import (
	"context"
	"io"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/sandboxpolicy"
)

type WorkloadTopology string

const (
	TopologyTerminalAuthoritative WorkloadTopology = "terminal_authoritative"
	TopologyIndependentServer     WorkloadTopology = "independent_server"
)

type ResourceKind string

const (
	ResourceProcess  ResourceKind = "process"
	ResourceServer   ResourceKind = "server"
	ResourceTerminal ResourceKind = "terminal"
)

type ResourceClaim struct {
	Kind ResourceKind
	Key  string
}

type PreparedDescription struct {
	HostSandboxPolicyHash string
	ExecutionID           model.ExecutionID
	Attempt               model.AttemptGeneration
	Topology              WorkloadTopology
	Requirements          RuntimeRequirements
	EffectivePolicy       EffectivePolicy
	Resources             []ResourceClaim
	Evidence              model.ProviderEvidence
	AccessDelivery        *ActionCredentialReceipt
	InitialInput          *PreparedInitialInputDescription
}

type PreparedInitialInput struct {
	Body                    string
	Correlation             string
	RequiredBeforeFirstWork bool
}

type PreparedInitialInputDescription struct {
	Correlation string
	Supported   bool
}

// ActionCredentialMaterial is application-issued and transient. A cohesive
// host/provider writes Secret to protected storage and must not retain it in
// provider evidence, argv, environment values or logs.
type ActionCredentialMaterial struct {
	ExecutionID model.ExecutionID
	Generation  model.AccessGeneration
	DeliveryID  string
	Secret      []byte
	ExpiresAt   time.Time
}

// ActionCredentialReceipt contains only non-secret delivery evidence. Resource
// is an opaque host-owned handle; providers may expose its path to the exact
// workload but application and transport do not interpret it.
type ActionCredentialReceipt struct {
	ExecutionID  model.ExecutionID
	Generation   model.AccessGeneration
	DeliveryID   string
	Resource     string
	FileIdentity string
	DeliveredAt  time.Time
}

type ActionCredentialDelivery interface {
	PrepareActionCredential(context.Context, ActionCredentialMaterial) (ActionCredentialReceipt, error)
	RotateActionCredential(context.Context, ActionCredentialReceipt, ActionCredentialMaterial) (ActionCredentialReceipt, error)
	InspectActionCredential(context.Context, model.ExecutionAccessBinding) (ActionCredentialRecoveryProof, error)
	RemoveActionCredential(context.Context, ActionCredentialReceipt) error
}

// ActionCredentialRecoveryProof is host-produced metadata about the protected
// resource. It proves exact delivery identity/generation without returning or
// persisting bearer plaintext in provider/domain evidence.
type ActionCredentialRecoveryProof struct {
	ExecutionID  model.ExecutionID
	Generation   model.AccessGeneration
	DeliveryID   string
	Resource     string
	FileIdentity string
	InspectedAt  time.Time
}

// ActionCredentialProvider is an optional cohesive-provider capability. It is
// separate from Provider so a provider that cannot deliver renewable protected
// credentials cannot accidentally advertise execution-agent access.
type ActionCredentialProvider interface {
	Provider
	ActionCredentials() ActionCredentialDelivery
}

type RuntimeRequirements struct {
	Executable       string
	WorkingDirectory string
	PrivateStorage   bool
	Terminal         *TerminalRequirement
	Loopback         *LoopbackRequirement
	Policy           PolicyRequirements
}

type TerminalRequirement struct {
	Interactive bool
}

type LoopbackRequirement struct {
	Protocol string
}

type PolicyRequirements struct {
	// DefaultSandbox resolves an incompatible inherited profile value only.
	DefaultSandbox    model.SandboxMode
	SupportedApproval []model.ApprovalMode
	SupportedSandbox  []model.SandboxMode
}

type EffectivePolicy struct {
	Approval         model.ApprovalMode
	Sandbox          model.SandboxMode
	ApprovalEnforced bool
	SandboxEnforced  bool
}

// ReleasePermit is application-owned one-shot authority. Release must call
// Consume immediately before its first irreversible effect. Public IDs are
// correlation only and cannot authorize release by themselves.
type ReleasePermit interface {
	ExecutionID() model.ExecutionID
	OperationID() model.OperationID
	Consume(context.Context) error
}

type ReleaseState string

const (
	ReleaseStarted   ReleaseState = "started"
	ReleaseUncertain ReleaseState = "uncertain"
)

type ReleaseResult struct {
	State    ReleaseState
	Runtime  Runtime
	Evidence model.ProviderEvidence
}

type PreparedAttempt interface {
	Describe() PreparedDescription
	Release(context.Context, ReleasePermit) (ReleaseResult, error)
	Abort(context.Context) error
}

type Provider interface {
	Name() string
	Capabilities() ProviderCapabilities
	Prepare(context.Context, PreparationRequest) (PreparedAttempt, error)
	Recover(context.Context, RecoveryRequest) (RecoveryResult, error)
}

type ProviderCapabilities struct {
	// LaunchPolicy is the static adapter contract, not a readiness or authority check.
	// Nil means that the provider does not publish this capability.
	LaunchPolicy *PolicyRequirements

	// HostSandbox requires host-owned confinement preparation in addition to
	// the harness native sandbox setting. False must refuse selected policies.
	HostSandbox          bool
	PreparedInitialInput bool
	NativeGuidance       []NativeGuidanceCapability
}

type NativeGuidanceCapability struct {
	EventKind string
	Timing    model.StandingOrderTiming
}

type StartIntent string

const (
	StartFresh    StartIntent = "fresh"
	StartContinue StartIntent = "continue"
	StartFork     StartIntent = "fork"
)

type PreparationRequest struct {
	HostSandboxPolicy *sandboxpolicy.PolicyMaterialization
	Spec              model.ResolvedExecutionSpec
	Intent            StartIntent
	Continuation      *model.NativeConversationEvidence
	// History is the application-resolved source for continuation or fork.
	// Public callers select platform catalog IDs and revisions instead.
	History          *HistorySourceSelection
	PriorEvidence    model.ProviderEvidence
	ActionCredential *ActionCredentialMaterial
	Observations     PrimaryObservationSink
	AgentAPIEndpoint string
	InitialInput     *PreparedInitialInput
	NativeGuidance   NativeGuidanceEvaluator
	CallbackIngress  CallbackIngress
}

type ProviderRegistry interface {
	Provider(harness string) (Provider, bool)
}

// PrimaryContextRecovery is the separately admitted application-owned binding,
// which may be newer than the evidence captured when a provider was released.
type PrimaryContextRecovery struct {
	Binding       model.NativeBinding
	Readiness     model.ContextReadiness
	ProviderOrder string
}

type RecoveryRequest struct {
	PrimaryContext   *PrimaryContextRecovery
	ExecutionID      model.ExecutionID
	Spec             model.ResolvedExecutionSpec
	Evidence         model.ProviderEvidence
	Attempt          model.AttemptGeneration
	Access           *model.ExecutionAccessBinding
	Observations     PrimaryObservationSink
	NativeGuidance   NativeGuidanceEvaluator
	CallbackIngress  CallbackIngress
	AgentAPIEndpoint string
}

type RecoveryState string

const (
	RecoveryControlled RecoveryState = "controlled"
	RecoveryExited     RecoveryState = "exited"
	RecoveryUnknown    RecoveryState = "unknown"
)

type RecoveryResult struct {
	State       RecoveryState
	Runtime     Runtime
	Observation Observation
	Evidence    model.ProviderEvidence
	Attempt     model.AttemptGeneration
	AccessProof *ActionCredentialRecoveryProof
}

type PrimaryContextDisposition string

const (
	PrimaryContextInitial    PrimaryContextDisposition = "initial"
	PrimaryContextContinuity PrimaryContextDisposition = "continuity"
	PrimaryContextReset      PrimaryContextDisposition = "reset"
	PrimaryContextUnresolved PrimaryContextDisposition = "unresolved"
)

// PrimaryContextEvidence is normalized trusted provider output. The sink is
// bound by composition to an exact attempt; payload IDs remain correlation and
// are checked again by application before a durable transition.
type PrimaryContextEvidence struct {
	ExecutionID                 model.ExecutionID
	Attempt                     model.AttemptGeneration
	Provider                    string
	PrimaryCorrelation          string
	Disposition                 PrimaryContextDisposition
	PriorBinding                *model.NativeBinding
	NextBinding                 *model.NativeBinding
	TransitionCorrelation       string
	ExpectedConversation        model.ConversationID
	ExpectedAssociationRevision model.Revision
	PriorProviderOrder          string
	ProviderOrder               string
	ObservedAt                  time.Time
}

type PrimaryObservationSink interface {
	ObservePrimaryContext(context.Context, PrimaryContextEvidence) error
}

type Runtime interface {
	ExecutionID() model.ExecutionID
	Observe(context.Context) (Observation, error)
	Interact(context.Context, Interaction) (InteractionResult, error)
	Attach(context.Context, AttachmentRequest) (AttachmentResult, error)
	ChangeContext(context.Context, ContextChange) (ContextChangeResult, error)
	Stop(context.Context, StopRequest) (StopResult, error)
}

type WorkloadObservedState string

const (
	WorkloadStarting WorkloadObservedState = "starting"
	WorkloadRunning  WorkloadObservedState = "running"
	WorkloadExited   WorkloadObservedState = "exited"
	WorkloadUnknown  WorkloadObservedState = "unknown"
)

type ContextObservedState string

const (
	ContextPending ContextObservedState = "pending"
	ContextReady   ContextObservedState = "ready"
	ContextUnknown ContextObservedState = "unknown"
)

// AgentActivityObservedState is provider evidence, never inferred from a
// running process, an empty work queue, or elapsed time.
type AgentActivityObservedState string

const (
	AgentActivityActive        AgentActivityObservedState = "active"
	AgentActivityIdle          AgentActivityObservedState = "idle"
	AgentActivityAwaitingInput AgentActivityObservedState = "awaiting_input"
	AgentActivityUnknown       AgentActivityObservedState = "unknown"
)

type Observation struct {
	ObservedAt              time.Time
	Workload                WorkloadObservedState
	Context                 ContextObservedState
	AgentActivity           AgentActivityObservedState
	AgentActivityObservedAt time.Time
	AttachmentActive        bool
	ExitCode                *int
	NativeConversation      *model.NativeConversationEvidence
	Evidence                model.ProviderEvidence
}

type Interaction struct {
	Text string
}

type InteractionResult struct {
	Disposition EffectDisposition
	Evidence    model.ProviderEvidence
}

type EffectDisposition string

const (
	EffectAccepted    EffectDisposition = "accepted"
	EffectRefused     EffectDisposition = "refused"
	EffectUnsupported EffectDisposition = "unsupported"
	EffectUnknown     EffectDisposition = "unknown"
)

type AttachmentKind string

const AttachmentTerminal AttachmentKind = "terminal"

type AttachmentRequest struct {
	Kind AttachmentKind
}

type Attachment interface {
	io.ReadWriteCloser
	Kind() AttachmentKind
}

type TerminalSize struct {
	Columns uint16
	Rows    uint16
}

// ResizableAttachment is an optional focused view. Fixed-size attachments do
// not implement it; implementations reject zero or unreasonably large sizes.
type ResizableAttachment interface {
	Attachment
	Resize(context.Context, TerminalSize) error
}

type AttachmentResult struct {
	Disposition EffectDisposition
	Attachment  Attachment
	Evidence    model.ProviderEvidence
}

type ContextChangeIntent string

const (
	ContextClear ContextChangeIntent = "clear"
	ContextReset ContextChangeIntent = "reset"
)

type ContextChange struct {
	Intent                      ContextChangeIntent
	ExpectedConversation        model.ConversationID
	ExpectedAssociationRevision model.Revision
	TransitionCorrelation       string
}

type ContextChangeResult struct {
	Disposition        EffectDisposition
	NativeConversation *model.NativeConversationEvidence
	Evidence           model.ProviderEvidence
}

type StopRequest struct {
	Force bool
}

type StopResult struct {
	Disposition  EffectDisposition
	Acknowledged bool
	Exited       bool
	Evidence     model.ProviderEvidence
}
