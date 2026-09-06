// Package claude implements the replacement backend's terminal-authoritative
// Claude Code provider. It owns launch rendering, native continuation, terminal
// interaction, observation, and recovery as one cohesive implementation.
package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

const (
	Name                   = "claude"
	NativeNamespace        = "claude-code"
	evidenceVersion uint32 = 1
)

type Config struct {
	Executable     string
	TmuxExecutable string
	PrivateRoot    string
}

type Provider struct {
	executable string
	terminal   host.TerminalHost
}

func New(config Config) (*Provider, error) {
	executable := config.Executable
	if executable == "" {
		executable = "claude"
	}
	resolved, err := exec.LookPath(executable)
	if err != nil {
		return nil, fmt.Errorf("resolve Claude Code executable: %w", err)
	}
	if !filepath.IsAbs(config.PrivateRoot) {
		return nil, fmt.Errorf("Claude private root must be absolute")
	}
	return &Provider{
		executable: resolved,
		terminal: host.TerminalHost{
			Executable:  config.TmuxExecutable,
			PrivateRoot: config.PrivateRoot,
		},
	}, nil
}

func (*Provider) Name() string { return Name }

type evidence struct {
	ExecutionID string                 `json:"execution_id"`
	NativeID    string                 `json:"native_id"`
	Terminal    *host.TerminalIdentity `json:"terminal,omitempty"`
}

type prepared struct {
	provider *Provider
	request  ports.PreparationRequest
	nativeID string
	terminal *host.PreparedTerminal
	describe ports.PreparedDescription
}

func (p *Provider) Prepare(ctx context.Context, request ports.PreparationRequest) (ports.PreparedAttempt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.Spec.Harness != Name {
		return nil, fmt.Errorf("Claude provider cannot prepare harness %q", request.Spec.Harness)
	}
	if err := validateDirectory(request.Spec.WorkingDirectory); err != nil {
		return nil, err
	}
	if request.Spec.Sandbox != model.SandboxWorkspaceWrite {
		return nil, fmt.Errorf("Claude provider does not enforce sandbox mode %q", request.Spec.Sandbox)
	}
	if request.Spec.Approval != model.ApprovalSupervised && request.Spec.Approval != model.ApprovalAutomatic {
		return nil, fmt.Errorf("Claude provider does not support approval mode %q", request.Spec.Approval)
	}
	nativeID, err := nativeIDFor(request)
	if err != nil {
		return nil, err
	}
	terminal, err := p.terminal.Prepare(string(request.Spec.ExecutionID))
	if err != nil {
		return nil, err
	}
	initial, err := encodeEvidence(evidence{ExecutionID: string(request.Spec.ExecutionID), NativeID: nativeID})
	if err != nil {
		_ = terminal.Abort()
		return nil, err
	}
	return &prepared{
		provider: p, request: request, nativeID: nativeID, terminal: terminal,
		describe: ports.PreparedDescription{
			ExecutionID: request.Spec.ExecutionID,
			Topology:    ports.TopologyTerminalAuthoritative,
			Requirements: ports.RuntimeRequirements{
				Executable: p.executable, WorkingDirectory: request.Spec.WorkingDirectory,
				PrivateStorage: true,
				Terminal:       &ports.TerminalRequirement{Interactive: true},
				Policy: ports.PolicyRequirements{
					SupportedApproval: []model.ApprovalMode{model.ApprovalSupervised, model.ApprovalAutomatic},
					SupportedSandbox:  []model.SandboxMode{model.SandboxWorkspaceWrite},
				},
			},
			EffectivePolicy: ports.EffectivePolicy{
				Approval: request.Spec.Approval, Sandbox: request.Spec.Sandbox,
				ApprovalEnforced: true, SandboxEnforced: true,
			},
			Resources: []ports.ResourceClaim{{Kind: ports.ResourceTerminal, Key: terminal.ResourceKey()}},
			Evidence:  initial,
		},
	}, nil
}

func (p *prepared) Describe() ports.PreparedDescription { return p.describe }

func (p *prepared) Abort(context.Context) error { return p.terminal.Abort() }

func (p *prepared) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ReleaseResult, error) {
	if permit == nil || permit.ExecutionID() != p.request.Spec.ExecutionID {
		return ports.ReleaseResult{}, fmt.Errorf("release permit does not match execution")
	}
	if err := permit.Consume(ctx); err != nil {
		return ports.ReleaseResult{}, fmt.Errorf("consume release permit: %w", err)
	}
	terminal, err := p.terminal.Release(host.ProcessSpec{
		Executable: p.provider.executable,
		Args:       p.argv(),
		Directory:  p.request.Spec.WorkingDirectory,
	})
	if err != nil {
		if terminal != nil {
			runtime := &Runtime{executionID: p.request.Spec.ExecutionID, terminal: terminal, nativeID: p.nativeID}
			evidence, _ := runtime.providerEvidence()
			return ports.ReleaseResult{State: ports.ReleaseUncertain, Runtime: runtime, Evidence: evidence}, err
		}
		return ports.ReleaseResult{}, err
	}
	runtime := &Runtime{
		executionID: p.request.Spec.ExecutionID,
		terminal:    terminal,
		nativeID:    p.nativeID,
	}
	evidence, err := runtime.providerEvidence()
	if err != nil {
		return ports.ReleaseResult{State: ports.ReleaseUncertain, Runtime: runtime}, err
	}
	return ports.ReleaseResult{State: ports.ReleaseStarted, Runtime: runtime, Evidence: evidence}, nil
}

func (p *prepared) argv() []string {
	args := make([]string, 0, 10)
	if p.request.Intent == ports.StartContinue {
		args = append(args, "--resume", p.nativeID)
	} else {
		args = append(args, "--session-id", p.nativeID)
	}
	if p.request.Spec.Model != "" {
		args = append(args, "--model", p.request.Spec.Model)
	}
	mode := "manual"
	if p.request.Spec.Approval == model.ApprovalAutomatic {
		mode = "auto"
	}
	args = append(args, "--permission-mode", mode)
	settings, _ := json.Marshal(map[string]any{"sandbox": map[string]any{
		"enabled": true, "failIfUnavailable": true,
		"allowUnsandboxedCommands": false,
		"filesystem":               map[string]any{"allowWrite": []string{p.request.Spec.WorkingDirectory}},
	}})
	args = append(args, "--settings", string(settings))
	return args
}

func (p *Provider) Recover(ctx context.Context, request ports.RecoveryRequest) (ports.RecoveryResult, error) {
	if err := ctx.Err(); err != nil {
		return ports.RecoveryResult{}, err
	}
	recorded, err := decodeEvidence(request.Evidence)
	if err != nil {
		return ports.RecoveryResult{}, err
	}
	if recorded.ExecutionID != string(request.ExecutionID) || recorded.Terminal == nil {
		return ports.RecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence}, nil
	}
	terminal, err := host.RecoverTerminal(p.terminal, *recorded.Terminal)
	if errors.Is(err, os.ErrProcessDone) {
		return ports.RecoveryResult{
			State: ports.RecoveryExited, Evidence: request.Evidence,
			Observation: ports.Observation{ObservedAt: time.Now(), Workload: ports.WorkloadExited,
				Context: ports.ContextUnknown, NativeConversation: nativeEvidence(recorded.NativeID)},
		}, nil
	}
	if err != nil {
		return ports.RecoveryResult{
			State: ports.RecoveryUnknown, Evidence: request.Evidence,
			Observation: ports.Observation{ObservedAt: time.Now(), Workload: ports.WorkloadUnknown,
				Context: ports.ContextUnknown, NativeConversation: nativeEvidence(recorded.NativeID)},
		}, nil
	}
	runtime := &Runtime{executionID: request.ExecutionID, terminal: terminal, nativeID: recorded.NativeID}
	observation, _ := runtime.Observe(ctx)
	return ports.RecoveryResult{State: ports.RecoveryControlled, Runtime: runtime, Observation: observation, Evidence: request.Evidence}, nil
}

type Runtime struct {
	executionID model.ExecutionID
	terminal    *host.Terminal
	nativeID    string
	mu          sync.Mutex
}

func (r *Runtime) ExecutionID() model.ExecutionID { return r.executionID }

func (r *Runtime) Observe(context.Context) (ports.Observation, error) {
	observation := r.terminal.Observe()
	result := ports.Observation{
		ObservedAt: time.Now(), Context: ports.ContextUnknown,
		NativeConversation: nativeEvidence(r.nativeID),
	}
	switch {
	case observation.Running:
		result.Workload = ports.WorkloadRunning
		result.AttachmentActive = r.terminal.AttachmentActive()
	case observation.Exited:
		result.Workload = ports.WorkloadExited
		result.ExitCode = observation.ExitCode
	case observation.Unknown:
		result.Workload = ports.WorkloadUnknown
	}
	evidence, err := r.providerEvidence()
	if err != nil {
		return ports.Observation{}, err
	}
	result.Evidence = evidence
	return result, nil
}

func (r *Runtime) Interact(ctx context.Context, interaction ports.Interaction) (ports.InteractionResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.TrimSpace(interaction.Text) == "" {
		return ports.InteractionResult{Disposition: ports.EffectRefused}, nil
	}
	if err := r.terminal.SendLiteral(ctx, interaction.Text); err != nil {
		evidence, _ := r.providerEvidence()
		return ports.InteractionResult{Disposition: ports.EffectUnknown, Evidence: evidence}, err
	}
	evidence, err := r.providerEvidence()
	return ports.InteractionResult{Disposition: ports.EffectAccepted, Evidence: evidence}, err
}

func (r *Runtime) Attach(ctx context.Context, request ports.AttachmentRequest) (ports.AttachmentResult, error) {
	if request.Kind != ports.AttachmentTerminal {
		return ports.AttachmentResult{Disposition: ports.EffectUnsupported}, nil
	}
	attachment, err := r.terminal.Attach(ctx)
	if err != nil {
		return ports.AttachmentResult{Disposition: ports.EffectRefused}, err
	}
	evidence, err := r.providerEvidence()
	if err != nil {
		_ = attachment.Close()
		return ports.AttachmentResult{}, err
	}
	return ports.AttachmentResult{
		Disposition: ports.EffectAccepted,
		Attachment:  terminalAttachment{ReadWriteCloser: attachment},
		Evidence:    evidence,
	}, nil
}

func (r *Runtime) ChangeContext(ctx context.Context, change ports.ContextChange) (ports.ContextChangeResult, error) {
	// Claude /clear rotates its native session reference. Until this provider
	// has an authenticated SessionStart observation channel, dispatching it
	// would leave durable evidence pointing at the predecessor. Refuse instead
	// of claiming a context change we cannot correlate.
	_ = ctx
	_ = change
	evidence, err := r.providerEvidence()
	return ports.ContextChangeResult{Disposition: ports.EffectUnsupported, Evidence: evidence}, err
}

func (r *Runtime) Stop(ctx context.Context, request ports.StopRequest) (ports.StopResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	acknowledged, exited, err := r.terminal.Stop(ctx, request.Force)
	evidence, evidenceErr := r.providerEvidence()
	if evidenceErr != nil && err == nil {
		err = evidenceErr
	}
	disposition := ports.EffectAccepted
	if err != nil {
		disposition = ports.EffectUnknown
	}
	return ports.StopResult{Disposition: disposition, Acknowledged: acknowledged, Exited: exited, Evidence: evidence}, err
}

func (r *Runtime) providerEvidence() (model.ProviderEvidence, error) {
	identity := r.terminal.Identity()
	return encodeEvidence(evidence{ExecutionID: string(r.executionID), NativeID: r.nativeID, Terminal: &identity})
}

type terminalAttachment struct{ io.ReadWriteCloser }

func (terminalAttachment) Kind() ports.AttachmentKind { return ports.AttachmentTerminal }

func nativeIDFor(request ports.PreparationRequest) (string, error) {
	switch request.Intent {
	case ports.StartFresh:
		return uuid.NewString(), nil
	case ports.StartContinue:
		if request.Continuation == nil || request.Continuation.Namespace != NativeNamespace {
			return "", fmt.Errorf("Claude continuation requires %s native evidence", NativeNamespace)
		}
		if _, err := uuid.Parse(request.Continuation.Reference); err != nil {
			return "", fmt.Errorf("invalid Claude continuation reference: %w", err)
		}
		return request.Continuation.Reference, nil
	default:
		return "", fmt.Errorf("unsupported start intent %q", request.Intent)
	}
}

func nativeEvidence(id string) *model.NativeConversationEvidence {
	if id == "" {
		return nil
	}
	return &model.NativeConversationEvidence{Namespace: NativeNamespace, Reference: id, ObservedAt: time.Now()}
}

func encodeEvidence(value evidence) (model.ProviderEvidence, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return model.ProviderEvidence{}, err
	}
	return model.NewProviderEvidence(Name, evidenceVersion, payload)
}

func decodeEvidence(envelope model.ProviderEvidence) (evidence, error) {
	if envelope.Provider != Name || envelope.Version != evidenceVersion {
		return evidence{}, fmt.Errorf("unsupported Claude evidence %q version %d", envelope.Provider, envelope.Version)
	}
	var value evidence
	if err := json.Unmarshal(envelope.Payload, &value); err != nil {
		return evidence{}, fmt.Errorf("decode Claude evidence: %w", err)
	}
	return value, nil
}

func validateDirectory(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("working directory must be absolute")
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("working directory is not a directory")
	}
	return nil
}

var _ ports.Provider = (*Provider)(nil)
var _ ports.PreparedAttempt = (*prepared)(nil)
var _ ports.Runtime = (*Runtime)(nil)
