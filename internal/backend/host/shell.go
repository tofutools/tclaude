//go:build linux || darwin

package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

const (
	shellResourceOwner   = "host.shell-terminal"
	shellResourceVersion = uint32(1)
)

type ShellConfig struct {
	Terminal    TerminalHost
	Executable  string
	Environment []string
}

type ShellTerminalHost struct {
	terminal    TerminalHost
	executable  string
	environment []string
}

type shellEvidence struct {
	ExecutionID model.ExecutionID        `json:"execution_id"`
	Attempt     model.AttemptGeneration  `json:"attempt"`
	WorkspaceID model.WorkspaceID        `json:"workspace_id"`
	Directory   string                   `json:"directory"`
	Sandbox     model.SandboxMode        `json:"sandbox"`
	Prepared    PreparedTerminalIdentity `json:"prepared"`
	Terminal    *TerminalIdentity        `json:"terminal,omitempty"`
}

type preparedShell struct {
	host     *ShellTerminalHost
	request  ports.ShellPreparationRequest
	terminal *PreparedTerminal
	evidence ports.ShellResourceEvidence
}

type shellRuntime struct {
	host      *ShellTerminalHost
	execution model.ExecutionID
	workspace model.WorkspaceID
	directory string
	sandbox   model.SandboxMode
	terminal  *Terminal
	evidence  ports.ShellResourceEvidence
}

func NewShellHost(config ShellConfig) (*ShellTerminalHost, error) {
	executable := config.Executable
	if executable == "" {
		executable = "/bin/sh"
	}
	resolved, err := exec.LookPath(executable)
	if err != nil {
		return nil, fmt.Errorf("resolve configured shell: %w", err)
	}
	return &ShellTerminalHost{terminal: config.Terminal, executable: resolved,
		environment: append([]string(nil), config.Environment...)}, nil
}

func (h *ShellTerminalHost) PrepareShell(_ context.Context, request ports.ShellPreparationRequest) (ports.PreparedShell, error) {
	if err := request.ExecutionID.Validate(); err != nil {
		return nil, err
	}
	if err := request.WorkspaceID.Validate(); err != nil {
		return nil, err
	}
	if request.Attempt == 0 {
		return nil, fmt.Errorf("shell attempt is required")
	}
	if request.Sandbox != model.SandboxUnconfined {
		return nil, fmt.Errorf("shell host does not provide OS confinement for sandbox %q", request.Sandbox)
	}
	if !filepath.IsAbs(request.WorkingDirectory) {
		return nil, fmt.Errorf("shell working directory must be absolute")
	}
	info, err := os.Stat(request.WorkingDirectory)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("shell working directory is unavailable")
	}
	prepared, err := h.terminal.Prepare(string(request.ExecutionID))
	if err != nil {
		return nil, err
	}
	value := shellEvidence{ExecutionID: request.ExecutionID, Attempt: request.Attempt,
		WorkspaceID: request.WorkspaceID, Directory: filepath.Clean(request.WorkingDirectory),
		Sandbox: request.Sandbox, Prepared: prepared.Identity()}
	evidence, err := encodeShellEvidence(value)
	if err != nil {
		_ = prepared.Abort()
		return nil, err
	}
	return &preparedShell{host: h, request: request, terminal: prepared, evidence: evidence}, nil
}

func (p *preparedShell) Describe() ports.ShellPreparedDescription {
	return ports.ShellPreparedDescription{ExecutionID: p.request.ExecutionID, Attempt: p.request.Attempt,
		Resources: []ports.ResourceClaim{{Kind: ports.ResourceTerminal, Key: p.terminal.ResourceKey()}}, Evidence: p.evidence}
}

func (p *preparedShell) Abort(context.Context) error { return p.terminal.Abort() }

func (p *preparedShell) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ShellReleaseResult, error) {
	if permit == nil || permit.ExecutionID() != p.request.ExecutionID {
		return ports.ShellReleaseResult{}, fmt.Errorf("release permit does not match shell execution")
	}
	if err := permit.Consume(ctx); err != nil {
		return ports.ShellReleaseResult{}, fmt.Errorf("consume shell release permit: %w", err)
	}
	terminal, err := p.terminal.Release(ProcessSpec{Executable: p.host.executable,
		Directory: p.request.WorkingDirectory, Env: p.host.environment})
	if terminal == nil {
		return ports.ShellReleaseResult{}, err
	}
	value, decodeErr := decodeShellEvidence(p.evidence)
	if decodeErr != nil {
		return ports.ShellReleaseResult{}, errors.Join(err, decodeErr)
	}
	identity := terminal.Identity()
	value.Terminal = &identity
	evidence, encodeErr := encodeShellEvidence(value)
	runtime := &shellRuntime{host: p.host, execution: value.ExecutionID, workspace: value.WorkspaceID,
		directory: value.Directory, sandbox: value.Sandbox, terminal: terminal, evidence: evidence}
	if err != nil || encodeErr != nil {
		return ports.ShellReleaseResult{State: ports.ReleaseUncertain, Runtime: runtime, Evidence: evidence}, errors.Join(err, encodeErr)
	}
	return ports.ShellReleaseResult{State: ports.ReleaseStarted, Runtime: runtime, Evidence: evidence}, nil
}

func (h *ShellTerminalHost) RecoverShell(_ context.Context, request ports.ShellRecoveryRequest) (ports.ShellRecoveryResult, error) {
	value, err := decodeShellEvidence(request.Evidence)
	if err != nil || value.ExecutionID != request.ExecutionID || value.Attempt != request.Attempt || value.WorkspaceID != request.WorkspaceID {
		return ports.ShellRecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence}, err
	}
	if value.Terminal == nil {
		terminal, recoverErr := RecoverPreparedTerminal(h.terminal, value.Prepared)
		if recoverErr != nil {
			if errors.Is(recoverErr, os.ErrNotExist) || errors.Is(recoverErr, os.ErrProcessDone) {
				return ports.ShellRecoveryResult{State: ports.RecoveryExited, Evidence: request.Evidence}, nil
			}
			return ports.ShellRecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence}, recoverErr
		}
		identity := terminal.Identity()
		value.Terminal = &identity
	} else {
		terminal, recoverErr := RecoverTerminal(h.terminal, *value.Terminal)
		if errors.Is(recoverErr, os.ErrProcessDone) {
			observation := ports.HostObservation{ObservedAt: time.Now().UTC(), Workload: ports.WorkloadExited, Evidence: request.Evidence}
			return ports.ShellRecoveryResult{State: ports.RecoveryExited, Observation: observation, Evidence: request.Evidence}, nil
		}
		if recoverErr != nil {
			return ports.ShellRecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence}, recoverErr
		}
		return recoveredShell(h, value, terminal)
	}
	terminal, err := RecoverTerminal(h.terminal, *value.Terminal)
	if err != nil {
		return ports.ShellRecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence}, err
	}
	return recoveredShell(h, value, terminal)
}

func recoveredShell(h *ShellTerminalHost, value shellEvidence, terminal *Terminal) (ports.ShellRecoveryResult, error) {
	evidence, err := encodeShellEvidence(value)
	if err != nil {
		return ports.ShellRecoveryResult{State: ports.RecoveryUnknown}, err
	}
	runtime := &shellRuntime{host: h, execution: value.ExecutionID, workspace: value.WorkspaceID,
		directory: value.Directory, sandbox: value.Sandbox, terminal: terminal, evidence: evidence}
	observation, err := runtime.ObserveHost(context.Background())
	return ports.ShellRecoveryResult{State: ports.RecoveryControlled, Runtime: runtime,
		Observation: observation, Evidence: evidence}, err
}

func (r *shellRuntime) ExecutionID() model.ExecutionID { return r.execution }

func (r *shellRuntime) ObserveHost(context.Context) (ports.HostObservation, error) {
	observed := r.terminal.Observe()
	result := ports.HostObservation{ObservedAt: time.Now().UTC(), AttachmentActive: r.terminal.AttachmentActive(), Evidence: r.evidence}
	switch {
	case observed.Running:
		result.Workload = ports.WorkloadRunning
	case observed.Exited:
		result.Workload, result.ExitCode = ports.WorkloadExited, observed.ExitCode
	default:
		result.Workload = ports.WorkloadUnknown
	}
	return result, nil
}

func (r *shellRuntime) AttachHost(ctx context.Context, request ports.AttachmentRequest) (ports.HostAttachmentResult, error) {
	if request.Kind != ports.AttachmentTerminal {
		return ports.HostAttachmentResult{Disposition: ports.EffectUnsupported, Evidence: r.evidence}, nil
	}
	attachment, err := r.terminal.Attach(ctx)
	if err != nil {
		return ports.HostAttachmentResult{Disposition: ports.EffectUnknown, Evidence: r.evidence}, err
	}
	return ports.HostAttachmentResult{Disposition: ports.EffectAccepted,
		Attachment: shellAttachment{TerminalAttachment: attachment}, Evidence: r.evidence}, nil
}

func (r *shellRuntime) StopHost(ctx context.Context, request ports.StopRequest) (ports.HostStopResult, error) {
	var acknowledged, exited bool
	var err error
	if request.Force {
		acknowledged, exited, err = r.terminal.Stop(ctx, true)
	} else {
		err = r.terminal.SendLiteral(ctx, "exit")
		acknowledged = err == nil
		exited = r.terminal.Observe().Exited
	}
	result := ports.HostStopResult{Disposition: ports.EffectAccepted, Acknowledged: acknowledged, Exited: exited, Evidence: r.evidence}
	if err != nil {
		result.Disposition = ports.EffectUnknown
	}
	return result, err
}

type shellAttachment struct{ TerminalAttachment }

var _ ports.ResizableAttachment = shellAttachment{}

func (shellAttachment) Kind() ports.AttachmentKind { return ports.AttachmentTerminal }
func (a shellAttachment) Resize(ctx context.Context, size ports.TerminalSize) error {
	return a.TerminalAttachment.Resize(ctx, size.Columns, size.Rows)
}

func encodeShellEvidence(value shellEvidence) (ports.ShellResourceEvidence, error) {
	payload, err := json.Marshal(value)
	return ports.ShellResourceEvidence{Owner: shellResourceOwner, Version: shellResourceVersion, Payload: payload}, err
}

func decodeShellEvidence(evidence ports.ShellResourceEvidence) (shellEvidence, error) {
	if evidence.Owner != shellResourceOwner || evidence.Version != shellResourceVersion || len(evidence.Payload) == 0 {
		return shellEvidence{}, fmt.Errorf("unsupported shell resource evidence")
	}
	var value shellEvidence
	if err := json.Unmarshal(evidence.Payload, &value); err != nil {
		return shellEvidence{}, err
	}
	if value.ExecutionID == "" || value.WorkspaceID == "" || value.Directory == "" || value.Prepared.SocketPath == "" {
		return shellEvidence{}, fmt.Errorf("incomplete shell resource evidence")
	}
	return value, nil
}

var _ ports.ShellHost = (*ShellTerminalHost)(nil)
var _ ports.PreparedShell = (*preparedShell)(nil)
var _ ports.HostRuntime = (*shellRuntime)(nil)

func (r *shellRuntime) StageTerminalFile(ctx context.Context, in ports.StageTerminalFileRequest) (ports.StageTerminalFileResult, error) {
	if r.terminal == nil || !r.terminal.Observe().Running {
		return ports.StageTerminalFileResult{Disposition: ports.EffectRefused}, nil
	}
	return StageTerminalFile(ctx, r.host.terminal.PrivateRoot, r.execution, in)
}
