package ports

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
)

// CallbackIngress is composition-owned ephemeral routing/authentication for
// provider-native callbacks. It starts empty after backend restart; only after
// recovering proof for the exact Execution attempt may a provider re-register
// its retained opaque registration ID and digest of its retained private
// credential. Credential plaintext remains entirely provider-private.
type CallbackIngress interface {
	RegisterCallback(context.Context, CallbackRegistration) (CallbackBinding, error)
}

type CallbackCredentialDigest [sha256.Size]byte

type CallbackRegistration struct {
	RegistrationID   string
	ExecutionID      model.ExecutionID
	Attempt          model.AttemptGeneration
	CredentialDigest CallbackCredentialDigest
	MaxRequestBytes  uint32
	MaxResponseBytes uint32
	Handler          NativeCallbackHandler
}

// NativeCallbackHandler receives raw bounded JSON only after ingress has
// authenticated the private route credential. Generic transport does not
// interpret provider payloads or manufacture normalized provenance.
type NativeCallbackHandler interface {
	HandleNativeCallback(context.Context, RawNativeCallback, RawNativeCallbackResponder) error
}

// RawNativeCallbackResponder owns the actual synchronous response write.
// Providers consume their guidance permit immediately before invoking it and
// settle only after its returned disposition.
type RawNativeCallbackResponder interface {
	Respond(context.Context, RawNativeCallbackResponse) (EffectDisposition, error)
}

type RawNativeCallback struct {
	Body       []byte
	ReceivedAt time.Time
}

type RawNativeCallbackResponse struct {
	StatusCode  int
	ContentType string
	Body        []byte
}

type CallbackBinding struct {
	RegistrationID string
	ExecutionID    model.ExecutionID
	Attempt        model.AttemptGeneration
	// Endpoint and Route are local routing metadata for provider preparation;
	// they are not public application state or runtime authority.
	Endpoint string
	Route    string
	Cleanup  CallbackCleanup
}

// CallbackCleanup is exact-registration and exact-attempt bound. Close is
// idempotent and cannot remove a successor registration.
type CallbackCleanup interface {
	RegistrationID() string
	ExecutionID() model.ExecutionID
	Attempt() model.AttemptGeneration
	Close(context.Context) error
}

// NativeGuidanceEvaluator is application-owned and bound at preparation to
// one exact Execution attempt and current Conversation association revision.
type NativeGuidanceEvaluator interface {
	EvaluateNativeGuidance(context.Context, NormalizedNativeEvent) (NativeGuidanceAdmission, error)
	SettleNativeGuidance(context.Context, NativeGuidanceSettlement) error
}

type NormalizedNativeEvent struct {
	EventID           string
	Kind              string
	OccurredAt        time.Time
	ObservedAt        time.Time
	NativeCorrelation string
	Payload           json.RawMessage
	Timing            model.StandingOrderTiming
}

// NativeGuidancePermit is consumed immediately before guidance is returned or
// written into the native callback continuation.
type NativeGuidancePermit interface {
	IssuanceID() model.WorkIssuanceID
	OperationID() model.OperationID
	Consume(context.Context) error
}

type NativeGuidanceAdmission struct {
	IssuanceID model.WorkIssuanceID
	Guidance   string
	Deadline   time.Time
	Permit     NativeGuidancePermit
}

type NativeGuidanceSettlement struct {
	IssuanceID  model.WorkIssuanceID
	Disposition EffectDisposition
	Evidence    model.ProviderEvidence
	SettledAt   time.Time
}

// NativeGuidanceRuntime is an optional focused view on a cohesive provider
// runtime. Provider-specific hook transports remain private.
type NativeGuidanceRuntime interface {
	HandleNativeEvent(context.Context, NormalizedNativeEvent, NativeGuidanceResponder) (NativeGuidanceSettlement, error)
}

// NativeGuidanceResponder is the provider-owned write boundary for one native
// callback. Generic transport supplies the response mechanism but never
// constructs provenance or receives an unconsumed admission.
type NativeGuidanceResponder interface {
	RespondNativeGuidance(context.Context, string) (EffectDisposition, error)
}

type ProgramEffectivePolicy struct {
	Sandbox  model.SandboxMode
	Enforced bool
}

type ProgramPreparationRequest struct {
	Execution        model.Execution
	Profile          model.ProgramProfileRevision
	Arguments        []string
	Input            json.RawMessage
	Workspace        model.Workspace
	WorkspaceUse     model.WorkspaceUse
	WorkingDirectory string
	Deadline         time.Time
}

type ProgramPreparedDescription struct {
	ExecutionID     model.ExecutionID
	Attempt         model.AttemptGeneration
	Requirements    RuntimeRequirements
	EffectivePolicy ProgramEffectivePolicy
	Resources       []ResourceClaim
	Evidence        model.ProviderEvidence
}

type PreparedProgram interface {
	Describe() ProgramPreparedDescription
	Release(context.Context, ReleasePermit) (ProgramReleaseResult, error)
	Abort(context.Context) error
}

type ProgramReleaseResult struct {
	State    ReleaseState
	Runtime  ProgramRuntime
	Evidence model.ProviderEvidence
}

type ProgramOutput struct {
	Data      []byte
	MediaType string
	Truncated bool
}

type ProgramObservation struct {
	ObservedAt time.Time
	Workload   WorkloadObservedState
	ExitCode   *int
	Stdout     ProgramOutput
	Stderr     ProgramOutput
	Evidence   model.ProviderEvidence
}

type ProgramRuntime interface {
	ExecutionID() model.ExecutionID
	ObserveProgram(context.Context) (ProgramObservation, error)
	StopProgram(context.Context, StopRequest) (StopResult, error)
	// ReleaseProgramResources is called only after the application durably
	// records the exact observation evidence. Natural exit and recovery retain
	// bounded output resources until this acknowledgement closes the crash gap.
	ReleaseProgramResources(context.Context, model.ProviderEvidence) error
}

type ProgramRecoveryRequest struct {
	Execution        model.Execution
	Profile          model.ProgramProfileRevision
	Workspace        model.Workspace
	WorkspaceUse     model.WorkspaceUse
	WorkingDirectory string
	Evidence         model.ProviderEvidence
}

type ProgramRecoveryResult struct {
	State       RecoveryState
	Runtime     ProgramRuntime
	Observation ProgramObservation
	Evidence    model.ProviderEvidence
}

// ProgramHost owns exact process/output-spool resources. Application code
// resolves profile/workspace and owns admission, release and authority.
type ProgramHost interface {
	PrepareProgram(context.Context, ProgramPreparationRequest) (PreparedProgram, error)
	RecoverProgram(context.Context, ProgramRecoveryRequest) (ProgramRecoveryResult, error)
}
