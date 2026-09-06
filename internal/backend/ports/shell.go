package ports

import (
	"context"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
)

// ShellResourceEvidence is opaque host recovery/ownership evidence. It is
// process evidence only and cannot establish native conversation context.
type ShellResourceEvidence struct {
	Owner   string
	Version uint32
	Payload []byte
}

type ShellPreparationRequest struct {
	ExecutionID      model.ExecutionID
	Attempt          model.AttemptGeneration
	WorkspaceID      model.WorkspaceID
	WorkingDirectory string
	Sandbox          model.SandboxMode
}

type ShellPreparedDescription struct {
	ExecutionID model.ExecutionID
	Attempt     model.AttemptGeneration
	Resources   []ResourceClaim
	Evidence    ShellResourceEvidence
}

type PreparedShell interface {
	Describe() ShellPreparedDescription
	Release(context.Context, ReleasePermit) (ShellReleaseResult, error)
	Abort(context.Context) error
}

type ShellReleaseResult struct {
	State    ReleaseState
	Runtime  HostRuntime
	Evidence ShellResourceEvidence
}

type ShellRecoveryRequest struct {
	ExecutionID model.ExecutionID
	Attempt     model.AttemptGeneration
	WorkspaceID model.WorkspaceID
	Evidence    ShellResourceEvidence
}

type ShellRecoveryResult struct {
	State       RecoveryState
	Runtime     HostRuntime
	Observation HostObservation
	Evidence    ShellResourceEvidence
}

type HostObservation struct {
	ObservedAt       time.Time
	Workload         WorkloadObservedState
	AttachmentActive bool
	ExitCode         *int
	Evidence         ShellResourceEvidence
}

type HostAttachmentResult struct {
	Disposition EffectDisposition
	Attachment  Attachment
	Evidence    ShellResourceEvidence
}

type HostStopResult struct {
	Disposition  EffectDisposition
	Acknowledged bool
	Exited       bool
	Evidence     ShellResourceEvidence
}

type HostRuntime interface {
	ExecutionID() model.ExecutionID
	ObserveHost(context.Context) (HostObservation, error)
	AttachHost(context.Context, AttachmentRequest) (HostAttachmentResult, error)
	StopHost(context.Context, StopRequest) (HostStopResult, error)
}

// ShellHost is composition configured with the executable/environment. Public
// callers select only an authorized workspace and confinement policy.
type ShellHost interface {
	PrepareShell(context.Context, ShellPreparationRequest) (PreparedShell, error)
	RecoverShell(context.Context, ShellRecoveryRequest) (ShellRecoveryResult, error)
}
