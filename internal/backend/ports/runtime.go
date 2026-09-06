package ports

import (
	"context"
	"io"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
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
	ExecutionID     model.ExecutionID
	Topology        WorkloadTopology
	Requirements    RuntimeRequirements
	EffectivePolicy EffectivePolicy
	Resources       []ResourceClaim
	Evidence        model.ProviderEvidence
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
	Prepare(context.Context, PreparationRequest) (PreparedAttempt, error)
	Recover(context.Context, RecoveryRequest) (RecoveryResult, error)
}

type StartIntent string

const (
	StartFresh    StartIntent = "fresh"
	StartContinue StartIntent = "continue"
)

type PreparationRequest struct {
	Spec          model.ResolvedExecutionSpec
	Intent        StartIntent
	Continuation  *model.NativeConversationEvidence
	PriorEvidence model.ProviderEvidence
}

type ProviderRegistry interface {
	Provider(harness string) (Provider, bool)
}

type RecoveryRequest struct {
	ExecutionID model.ExecutionID
	Spec        model.ResolvedExecutionSpec
	Evidence    model.ProviderEvidence
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

type Observation struct {
	ObservedAt         time.Time
	Workload           WorkloadObservedState
	Context            ContextObservedState
	AttachmentActive   bool
	ExitCode           *int
	NativeConversation *model.NativeConversationEvidence
	Evidence           model.ProviderEvidence
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
