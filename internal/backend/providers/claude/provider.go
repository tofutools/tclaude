// Package claude implements the replacement backend's terminal-authoritative
// Claude Code provider. It owns launch rendering, native continuation, terminal
// interaction, observation, and recovery as one cohesive implementation.
package claude

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers/nativeguidance"
)

const (
	Name                   = "claude"
	NativeNamespace        = "claude-code"
	evidenceVersion uint32 = 1
)

type Config struct {
	HostSandbox          *host.SandboxLaunchPreparer
	AgentSocketDirectory string
	// NativeHome is an explicitly owned persistent CLAUDE_CONFIG_DIR. Empty
	// selects PrivateRoot/native-home for selected host-sandbox launches.
	NativeHome     string
	Executable     string
	TmuxExecutable string
	PrivateRoot    string
	AgentSocket    string
}

type Provider struct {
	nativeHomeExplicit   bool
	hostSandbox          *host.SandboxLaunchPreparer
	agentSocketDirectory string
	nativeHome           string
	executable           string
	terminal             host.TerminalHost
	credentials          host.ActionCredentialHost
	agentSocket          string
	observationRoot      string
	callbackRoot         string
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
		return nil, fmt.Errorf("claude private root must be absolute")
	}
	nativeHome := config.NativeHome
	if nativeHome == "" {
		nativeHome = filepath.Join(config.PrivateRoot, "native-home")
	}
	if !filepath.IsAbs(nativeHome) || filepath.Clean(nativeHome) != nativeHome {
		return nil, fmt.Errorf("claude native home must be a clean absolute path")
	}
	return &Provider{
		nativeHomeExplicit: config.NativeHome != "", hostSandbox: config.HostSandbox, agentSocketDirectory: config.AgentSocketDirectory, nativeHome: nativeHome,
		executable: resolved,
		terminal: host.TerminalHost{
			Executable:  config.TmuxExecutable,
			PrivateRoot: config.PrivateRoot,
		},
		credentials:     host.ActionCredentialHost{PrivateRoot: filepath.Join(config.PrivateRoot, "action-credentials")},
		agentSocket:     config.AgentSocket,
		observationRoot: filepath.Join(config.PrivateRoot, "observations"),
		callbackRoot:    filepath.Join(config.PrivateRoot, "native-callbacks"),
	}, nil
}

func (*Provider) Name() string { return Name }
func (p *Provider) Capabilities() ports.ProviderCapabilities {
	policy := supportedLaunchPolicy()
	return ports.ProviderCapabilities{HostSandbox: p.hostSandbox != nil, LaunchPolicy: &policy, PreparedInitialInput: true, NativeGuidance: []ports.NativeGuidanceCapability{{EventKind: "session_start", Timing: model.StandingOrderSameContinuation}, {EventKind: "user_prompt", Timing: model.StandingOrderSameContinuation}}}
}
func (p *Provider) ActionCredentials() ports.ActionCredentialDelivery { return p.credentials }

type evidence struct {
	HostSandbox           *host.SandboxChildArtifact       `json:"host_sandbox,omitempty"`
	HostSandboxPolicyHash string                           `json:"host_sandbox_policy_hash,omitempty"`
	NativeHome            string                           `json:"native_home,omitempty"`
	ExecutionID           string                           `json:"execution_id"`
	NativeID              string                           `json:"native_id"`
	Intent                ports.StartIntent                `json:"intent"`
	ContextReady          bool                             `json:"context_ready,omitempty"`
	ProviderOrder         string                           `json:"provider_order,omitempty"`
	Prepared              *host.PreparedTerminalIdentity   `json:"prepared,omitempty"`
	Terminal              *host.TerminalIdentity           `json:"terminal,omitempty"`
	Access                *ports.ActionCredentialReceipt   `json:"access,omitempty"`
	ObservationSpool      string                           `json:"observation_spool,omitempty"`
	Callback              *nativeguidance.CallbackEvidence `json:"native_callback,omitempty"`
}

type prepared struct {
	artifact        *host.SandboxChildArtifact
	command         host.ProcessSpec
	nativeHome      string
	provider        *Provider
	request         ports.PreparationRequest
	nativeID        string
	terminal        *host.PreparedTerminal
	describe        ports.PreparedDescription
	access          *ports.ActionCredentialReceipt
	spool           *host.ObservationSpool
	callback        *nativeguidance.CallbackResource
	guidance        *nativeguidance.Runtime
	handler         *nativeguidance.CallbackHandler
	callbackCommand string
}

func (p *Provider) Prepare(ctx context.Context, request ports.PreparationRequest) (ports.PreparedAttempt, error) {
	if request.Spec.HostSandbox == nil && request.HostSandboxPolicy != nil {
		return nil, fmt.Errorf("sandbox materialization has no selected identity")
	}
	if request.Spec.HostSandbox != nil && (p.hostSandbox == nil || request.HostSandboxPolicy == nil) {
		return nil, fmt.Errorf("selected host sandbox preparation is unavailable")
	}
	request.Spec.HostSandbox = model.CloneSandboxSelection(request.Spec.HostSandbox)
	if err := request.Spec.Environment.Validate(); err != nil {
		return nil, err
	}
	request.Spec.Environment = request.Spec.Environment.Clone()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.Spec.Harness != Name {
		return nil, fmt.Errorf("claude provider cannot prepare harness %q", request.Spec.Harness)
	}
	if err := model.ValidateEffort(request.Spec.Effort); err != nil {
		return nil, err
	}
	if err := validateDirectory(request.Spec.WorkingDirectory); err != nil {
		return nil, err
	}
	if request.Spec.Sandbox != model.SandboxWorkspaceWrite {
		return nil, fmt.Errorf("claude provider does not enforce sandbox mode %q", request.Spec.Sandbox)
	}
	if !slices.Contains(claudeApprovalModes(), request.Spec.Approval) {
		return nil, fmt.Errorf("claude provider does not support approval mode %q", request.Spec.Approval)
	}
	initialInput, err := preparedInitialInput(request.InitialInput)
	if err != nil {
		return nil, err
	}
	nativeID, err := nativeIDFor(request)
	if err != nil {
		return nil, err
	}
	var access *ports.ActionCredentialReceipt
	if request.ActionCredential != nil {
		if request.ActionCredential.ExecutionID != request.Spec.ExecutionID {
			return nil, fmt.Errorf("action credential does not match Claude execution")
		}
		if !filepath.IsAbs(p.agentSocket) {
			return nil, fmt.Errorf("claude agent API socket must be absolute for credential delivery")
		}
		receipt, deliveryErr := p.credentials.PrepareActionCredential(ctx, *request.ActionCredential)
		if deliveryErr != nil {
			return nil, deliveryErr
		}
		access = &receipt
	}
	terminal, err := p.terminal.Prepare(string(request.Spec.ExecutionID))
	if err != nil {
		if access != nil {
			_ = p.credentials.RemoveActionCredential(context.Background(), *access)
		}
		return nil, err
	}
	spool, err := host.PrepareObservationSpool(p.observationRoot)
	if err != nil {
		_ = terminal.Abort()
		if access != nil {
			_ = p.credentials.RemoveActionCredential(context.Background(), *access)
		}
		return nil, err
	}
	var callback *nativeguidance.CallbackResource
	var guidance *nativeguidance.Runtime
	var handler *nativeguidance.CallbackHandler
	var callbackCommand string
	if request.NativeGuidance != nil {
		if request.CallbackIngress == nil {
			_ = terminal.Abort()
			_ = spool.Remove()
			if access != nil {
				_ = p.credentials.RemoveActionCredential(context.Background(), *access)
			}
			return nil, fmt.Errorf("claude native guidance requires callback ingress")
		}
		callback, err = nativeguidance.PrepareCallback(p.callbackRoot)
		if err == nil {
			guidance = &nativeguidance.Runtime{Evaluator: request.NativeGuidance, Kinds: map[string]struct{}{"session_start": {}, "user_prompt": {}}, Correlation: func(value string) bool { return value == nativeID }}
			handler = &nativeguidance.CallbackHandler{Normalize: claudeNativeNormalizer(nativeID), Encode: encodeClaudeGuidance}
			err = callback.Register(ctx, request.CallbackIngress, request.Spec.ExecutionID, request.Spec.Attempt, handler)
		}
		if err == nil {
			var client string
			client, err = exec.LookPath("curl")
			if err == nil {
				callbackCommand, err = callback.Command(client)
				if err == nil {
					callbackCommand, err = callback.WriteCommandScript(callbackCommand)
				}
			}
		}
		if err != nil {
			if callback != nil {
				_ = callback.Remove(context.Background())
			}
			_ = terminal.Abort()
			_ = spool.Remove()
			if access != nil {
				_ = p.credentials.RemoveActionCredential(context.Background(), *access)
			}
			return nil, err
		}
	}
	preparedIdentity := terminal.Identity()
	var callbackEvidence *nativeguidance.CallbackEvidence
	if callback != nil {
		value := callback.Evidence()
		callbackEvidence = &value
	}
	initial, err := encodeEvidence(evidence{ExecutionID: string(request.Spec.ExecutionID), NativeID: nativeID, Intent: request.Intent,
		Prepared: &preparedIdentity, Access: access, ObservationSpool: spool.Directory(), Callback: callbackEvidence})
	if err != nil {
		_ = terminal.Abort()
		_ = spool.Remove()
		if callback != nil {
			_ = callback.Remove(context.Background())
		}
		if access != nil {
			_ = p.credentials.RemoveActionCredential(context.Background(), *access)
		}
		return nil, err
	}
	result := &prepared{
		provider: p, request: request, nativeID: nativeID, terminal: terminal,
		access: access, spool: spool, callback: callback, guidance: guidance, handler: handler, callbackCommand: callbackCommand, describe: ports.PreparedDescription{
			ExecutionID: request.Spec.ExecutionID,
			Attempt:     request.Spec.Attempt,
			Topology:    ports.TopologyTerminalAuthoritative,
			Requirements: ports.RuntimeRequirements{
				Executable: p.executable, WorkingDirectory: request.Spec.WorkingDirectory,
				PrivateStorage: true,
				Terminal:       &ports.TerminalRequirement{Interactive: true},
				Policy:         supportedLaunchPolicy(),
			},
			EffectivePolicy: ports.EffectivePolicy{
				Approval: request.Spec.Approval, Sandbox: request.Spec.Sandbox,
				ApprovalEnforced: true, SandboxEnforced: true,
			},
			Resources:      []ports.ResourceClaim{{Kind: ports.ResourceTerminal, Key: terminal.ResourceKey()}},
			Evidence:       initial,
			AccessDelivery: access,
			InitialInput:   initialInput,
		},
	}
	if err := result.prepareSandbox(ctx); err != nil {
		_ = result.Abort(context.WithoutCancel(ctx))
		return nil, err
	}
	return result, nil
}

func (p *prepared) Describe() ports.PreparedDescription { return p.describe }

func (p *prepared) Abort(ctx context.Context) error {
	err := p.terminal.Abort()
	if err == nil && p.artifact != nil {
		err = host.AbortSandboxChild(*p.artifact)
	}
	if p.spool != nil {
		err = errors.Join(err, p.spool.Remove())
	}
	if p.access != nil {
		err = errors.Join(err, p.provider.credentials.RemoveActionCredential(ctx, *p.access))
	}
	if p.callback != nil {
		err = errors.Join(err, p.callback.Remove(ctx))
	}
	return err
}

func (p *prepared) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ReleaseResult, error) {
	if permit == nil || permit.ExecutionID() != p.request.Spec.ExecutionID {
		return ports.ReleaseResult{}, fmt.Errorf("release permit does not match execution")
	}
	if p.guidance != nil {
		p.guidance.Evidence = func() (model.ProviderEvidence, error) { return p.describe.Evidence, nil }
		if err := p.handler.Bind(p.guidance); err != nil {
			return ports.ReleaseResult{}, err
		}
	}
	if p.artifact != nil {
		if err := host.VerifySandboxChild(ctx, *p.artifact); err != nil {
			return ports.ReleaseResult{}, err
		}
	}
	if err := permit.Consume(ctx); err != nil {
		return ports.ReleaseResult{}, fmt.Errorf("consume release permit: %w", err)
	}
	terminal, err := p.terminal.Release(p.command)
	if err != nil {
		if terminal != nil {
			runtime := p.runtime(terminal)
			evidence, _ := runtime.providerEvidence()
			return ports.ReleaseResult{State: ports.ReleaseUncertain, Runtime: runtime, Evidence: evidence}, err
		}
		_ = p.spool.Remove()
		if p.callback != nil {
			_ = p.callback.Remove(context.Background())
		}
		if p.access != nil {
			_ = p.provider.credentials.RemoveActionCredential(context.Background(), *p.access)
		}
		return ports.ReleaseResult{}, err
	}
	runtime := p.runtime(terminal)
	evidence, err := runtime.providerEvidence()
	if err != nil {
		return ports.ReleaseResult{State: ports.ReleaseUncertain, Runtime: runtime}, err
	}
	return ports.ReleaseResult{State: ports.ReleaseStarted, Runtime: runtime, Evidence: evidence}, nil
}

func (p *prepared) runtime(terminal *host.Terminal) *Runtime {
	return &Runtime{artifact: p.artifact, policyHash: p.describe.HostSandboxPolicyHash, nativeHome: p.nativeHome,
		provider: p.provider, executionID: p.request.Spec.ExecutionID, attempt: p.request.Spec.Attempt,
		terminal: terminal, nativeID: p.nativeID, intent: p.request.Intent, observations: p.request.Observations,
		access: p.access, spool: p.spool,
		guidance: p.guidance, callback: p.callback,
	}
}

func (p *prepared) runtimeEnvironment() []string {
	memory := "1"
	if p.request.Spec.AutoMemory {
		memory = "0"
	}
	result := []string{
		"CLAUDE_CODE_DISABLE_AUTO_MEMORY=" + memory,
		"TCLAUDE_OBSERVATION_SPOOL=" + p.spool.Directory(),
		"TCLAUDE_BACKEND_CREDENTIAL_FILE=",
		"TCLAUDE_BACKEND_SOCKET=",
	}
	if p.access != nil {
		result = append(result,
			"TCLAUDE_BACKEND_CREDENTIAL_FILE="+p.access.Resource,
			"TCLAUDE_BACKEND_SOCKET="+p.provider.agentSocket)
	}
	return append(p.request.Spec.Environment.Entries(), result...)
}

func (p *prepared) argv() []string {
	args := make([]string, 0, 10)
	if p.request.Intent == ports.StartContinue {
		args = append(args, "--resume", p.nativeID)
	} else {
		args = append(args, "--session-id", p.nativeID)
	}
	if p.request.Spec.Effort != "" {
		args = append(args, "--effort", p.request.Spec.Effort)
	}
	if p.request.Spec.Model != "" {
		args = append(args, "--model", p.request.Spec.Model)
	}
	if mode := nativeApprovalMode(p.request.Spec.Approval); mode != "" {
		args = append(args, "--permission-mode", mode)
	}
	settings := map[string]any{
		"sandbox": map[string]any{
			"enabled": true, "failIfUnavailable": true,
			"allowUnsandboxedCommands": false,
			"filesystem":               map[string]any{"allowWrite": []string{p.request.Spec.WorkingDirectory}},
		},
		"hooks": p.hooks(),
	}
	if !p.request.Spec.PeerMessaging {
		settings["crossSessionInbound"] = "refuse"
		settings["isolatePeerMachines"] = true
		settings["permissions"] = map[string]any{"deny": []string{"ListAgents"}}
	}
	encodedSettings, _ := json.Marshal(settings)
	args = append(args, "--settings", string(encodedSettings))
	if p.request.InitialInput != nil {
		args = append(args, p.request.InitialInput.Body)
	}
	return args
}

func (p *prepared) hooks() map[string]any {
	sessionHooks := []any{map[string]any{"type": "command", "command": "/bin/sh", "args": []string{"-c", claudeObservationCommand}}}
	result := map[string]any{"SessionStart": []any{map[string]any{"matcher": "startup|resume|clear|compact", "hooks": sessionHooks}}}
	if p.callbackCommand != "" {
		callback := map[string]any{"type": "command", "command": p.callbackCommand}
		sessionHooks = append(sessionHooks, callback)
		result["SessionStart"] = []any{map[string]any{"matcher": "startup|resume|clear|compact", "hooks": sessionHooks}}
		result["UserPromptSubmit"] = []any{map[string]any{"hooks": []any{callback}}}
		result["Notification"] = []any{map[string]any{"matcher": "idle_prompt|permission_prompt|elicitation_dialog", "hooks": []any{callback}}}
		for _, event := range []string{"PreToolUse", "PostToolUse", "Stop"} {
			result[event] = []any{map[string]any{"hooks": []any{callback}}}
		}
	}
	return result
}

func preparedInitialInput(input *ports.PreparedInitialInput) (*ports.PreparedInitialInputDescription, error) {
	if input == nil {
		return nil, nil
	}
	if strings.TrimSpace(input.Body) == "" || strings.TrimSpace(input.Correlation) == "" {
		return nil, fmt.Errorf("claude prepared initial input requires body and correlation")
	}
	return &ports.PreparedInitialInputDescription{Correlation: input.Correlation, Supported: true}, nil
}

func (p *Provider) Recover(ctx context.Context, request ports.RecoveryRequest) (ports.RecoveryResult, error) {
	if err := ctx.Err(); err != nil {
		return ports.RecoveryResult{}, err
	}
	recorded, err := decodeEvidence(request.Evidence)
	if err != nil {
		return ports.RecoveryResult{}, err
	}
	expectedHash := ""
	if request.Spec.HostSandbox != nil {
		expectedHash = request.Spec.HostSandbox.PolicyHash
	}
	if recorded.HostSandboxPolicyHash != expectedHash || (recorded.HostSandbox != nil) != (expectedHash != "") || (expectedHash != "" && recorded.NativeHome == "") || (recorded.NativeHome != "" && recorded.NativeHome != p.nativeHome) {
		return ports.RecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence}, nil
	}
	if recorded.ExecutionID != string(request.ExecutionID) {
		return ports.RecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence}, nil
	}
	var terminal *host.Terminal
	if recorded.Terminal != nil {
		terminal, err = host.RecoverTerminal(p.terminal, *recorded.Terminal)
	} else if recorded.Prepared != nil {
		terminal, err = host.RecoverPreparedTerminal(p.terminal, *recorded.Prepared)
	} else {
		return ports.RecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence}, nil
	}
	if errors.Is(err, os.ErrProcessDone) {
		cleanupErr := host.RemoveObservationSpool(p.observationRoot, recorded.ObservationSpool)
		if recorded.Callback != nil {
			if resource, callbackErr := nativeguidance.RecoverCallback(p.callbackRoot, *recorded.Callback); callbackErr == nil {
				cleanupErr = errors.Join(cleanupErr, resource.Remove(ctx))
			} else {
				cleanupErr = errors.Join(cleanupErr, callbackErr)
			}
		}
		if recorded.Access != nil {
			cleanupErr = errors.Join(cleanupErr, p.credentials.RemoveActionCredential(ctx, *recorded.Access))
		}
		return ports.RecoveryResult{
			State: ports.RecoveryExited, Evidence: request.Evidence,
			Observation: ports.Observation{ObservedAt: time.Now(), Workload: ports.WorkloadExited,
				Context: ports.ContextUnknown, NativeConversation: nativeEvidence(recorded.NativeID)},
		}, cleanupErr
	}
	if err != nil {
		return ports.RecoveryResult{
			State: ports.RecoveryUnknown, Evidence: request.Evidence,
			Observation: ports.Observation{ObservedAt: time.Now(), Workload: ports.WorkloadUnknown,
				Context: ports.ContextUnknown, NativeConversation: nativeEvidence(recorded.NativeID)},
		}, nil
	}
	spool, spoolErr := host.RecoverObservationSpool(p.observationRoot, recorded.ObservationSpool)
	if spoolErr != nil {
		return ports.RecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence, Attempt: request.Attempt}, spoolErr
	}
	var accessProof *ports.ActionCredentialRecoveryProof
	if request.Access != nil {
		if recorded.Access == nil || recorded.Access.DeliveryID != request.Access.DeliveryID || recorded.Access.ExecutionID != request.ExecutionID {
			return ports.RecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence, Attempt: request.Attempt}, nil
		}
		proof, inspectErr := p.credentials.InspectActionCredential(ctx, *request.Access)
		if inspectErr != nil || proof.Resource != recorded.Access.Resource {
			return ports.RecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence, Attempt: request.Attempt}, inspectErr
		}
		accessProof = &proof
	}
	var callback *nativeguidance.CallbackResource
	var guidance *nativeguidance.Runtime
	if recorded.Callback != nil {
		if request.NativeGuidance == nil || request.CallbackIngress == nil {
			return ports.RecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence, Attempt: request.Attempt}, nil
		}
		callback, err = nativeguidance.RecoverCallback(p.callbackRoot, *recorded.Callback)
		if err != nil {
			return ports.RecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence, Attempt: request.Attempt}, err
		}
		handler := &nativeguidance.CallbackHandler{Normalize: claudeNativeNormalizer(recorded.NativeID), Encode: encodeClaudeGuidance}
		if err := callback.Register(ctx, request.CallbackIngress, request.ExecutionID, request.Attempt, handler); err != nil {
			return ports.RecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence, Attempt: request.Attempt}, err
		}
		client, clientErr := exec.LookPath("curl")
		command, commandErr := callback.Command(client)
		if clientErr == nil && commandErr == nil {
			_, commandErr = callback.WriteCommandScript(command)
		}
		if clientErr != nil || commandErr != nil {
			_ = callback.CloseRegistration(context.Background())
			return ports.RecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence, Attempt: request.Attempt}, errors.Join(clientErr, commandErr)
		}
		guidance = &nativeguidance.Runtime{Evaluator: request.NativeGuidance, Evidence: func() (model.ProviderEvidence, error) { return request.Evidence, nil }, Kinds: map[string]struct{}{"session_start": {}, "user_prompt": {}}, Correlation: func(value string) bool { return value == recorded.NativeID }}
		if err := handler.Bind(guidance); err != nil {
			_ = callback.CloseRegistration(context.Background())
			return ports.RecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence, Attempt: request.Attempt}, err
		}
	} else if request.NativeGuidance != nil {
		return ports.RecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence, Attempt: request.Attempt}, nil
	}
	runtime := &Runtime{artifact: recorded.HostSandbox, policyHash: recorded.HostSandboxPolicyHash, nativeHome: recorded.NativeHome, provider: p, executionID: request.ExecutionID, attempt: request.Attempt, terminal: terminal,
		nativeID: recorded.NativeID, intent: recorded.Intent, observations: request.Observations, access: recorded.Access, spool: spool,
		contextReady: recorded.ContextReady, providerOrder: recorded.ProviderOrder, guidance: guidance, callback: callback}
	observation, _ := runtime.Observe(ctx)
	return ports.RecoveryResult{State: ports.RecoveryControlled, Runtime: runtime, Observation: observation, Evidence: request.Evidence, Attempt: request.Attempt, AccessProof: accessProof}, nil
}

type Runtime struct {
	artifact      *host.SandboxChildArtifact
	policyHash    string
	nativeHome    string
	provider      *Provider
	executionID   model.ExecutionID
	attempt       model.AttemptGeneration
	terminal      *host.Terminal
	nativeID      string
	intent        ports.StartIntent
	observations  ports.PrimaryObservationSink
	access        *ports.ActionCredentialReceipt
	spool         *host.ObservationSpool
	contextReady  bool
	providerOrder string
	guidance      *nativeguidance.Runtime
	callback      *nativeguidance.CallbackResource
	cleanupOnce   sync.Once
	cleanupErr    error
	mu            sync.Mutex
}

func (r *Runtime) ExecutionID() model.ExecutionID { return r.executionID }

func (r *Runtime) HandleNativeEvent(ctx context.Context, event ports.NormalizedNativeEvent, responder ports.NativeGuidanceResponder) (ports.NativeGuidanceSettlement, error) {
	if r.guidance == nil {
		return ports.NativeGuidanceSettlement{Disposition: ports.EffectUnsupported}, nil
	}
	return r.guidance.HandleNativeEvent(ctx, event, responder)
}

func (r *Runtime) Observe(ctx context.Context) (ports.Observation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	observation := r.terminal.Observe()
	var observationErr error
	if observation.Running {
		if _, err := r.consumeObservationEvents(ctx, nil); err != nil {
			observationErr = err
			if afterIngress := r.terminal.Observe(); afterIngress.Exited {
				observation = afterIngress
			} else {
				return ports.Observation{}, err
			}
		}
	}
	result := ports.Observation{
		ObservedAt: time.Now(), Context: ports.ContextUnknown,
		NativeConversation: nativeEvidence(r.nativeID),
	}
	switch {
	case observation.Running:
		result.Workload = ports.WorkloadRunning
		if r.guidance != nil {
			result.AgentActivity, result.AgentActivityObservedAt = r.guidance.ActivityObservation()
		}
		result.AttachmentActive = r.terminal.AttachmentActive()
		if r.contextReady {
			result.Context = ports.ContextReady
		}
	case observation.Exited:
		result.Workload = ports.WorkloadExited
		result.ExitCode = observation.ExitCode
		r.cleanupResources(ctx)
	case observation.Unknown:
		result.Workload = ports.WorkloadUnknown
	}
	evidence, err := r.providerEvidenceUnlocked()
	if err != nil {
		return ports.Observation{}, err
	}
	result.Evidence = evidence
	return result, errors.Join(observationErr, r.cleanupErr)
}

func (r *Runtime) Interact(ctx context.Context, interaction ports.Interaction) (ports.InteractionResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.TrimSpace(interaction.Text) == "" {
		return ports.InteractionResult{Disposition: ports.EffectRefused}, nil
	}
	if r.guidance != nil {
		r.guidance.InvalidateActivity()
	}
	if err := r.terminal.SendLiteral(ctx, interaction.Text); err != nil {
		evidence, _ := r.providerEvidenceUnlocked()
		return ports.InteractionResult{Disposition: ports.EffectUnknown, Evidence: evidence}, err
	}
	evidence, err := r.providerEvidenceUnlocked()
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
		Attachment:  terminalAttachment{TerminalAttachment: attachment},
		Evidence:    evidence,
	}, nil
}

func (r *Runtime) ChangeContext(ctx context.Context, change ports.ContextChange) (ports.ContextChangeResult, error) {
	if change.Intent != ports.ContextClear && change.Intent != ports.ContextReset {
		return ports.ContextChangeResult{Disposition: ports.EffectUnsupported}, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.consumeObservationEvents(ctx, nil); err != nil {
		evidence, _ := r.providerEvidenceUnlocked()
		return ports.ContextChangeResult{Disposition: ports.EffectUnknown, Evidence: evidence}, err
	}
	if r.guidance != nil {
		r.guidance.InvalidateActivity()
	}
	if err := r.terminal.SendLiteral(ctx, "/clear"); err != nil {
		evidence, _ := r.providerEvidenceUnlocked()
		return ports.ContextChangeResult{Disposition: ports.EffectUnknown, Evidence: evidence}, err
	}
	for {
		confirmed, err := r.consumeObservationEvents(ctx, &change)
		if err != nil {
			evidence, _ := r.providerEvidenceUnlocked()
			return ports.ContextChangeResult{Disposition: ports.EffectUnknown, Evidence: evidence}, err
		}
		if confirmed {
			evidence, evidenceErr := r.providerEvidenceUnlocked()
			return ports.ContextChangeResult{Disposition: ports.EffectAccepted,
				NativeConversation: nativeEvidence(r.nativeID), Evidence: evidence}, evidenceErr
		}
		select {
		case <-ctx.Done():
			evidence, _ := r.providerEvidenceUnlocked()
			return ports.ContextChangeResult{Disposition: ports.EffectUnknown, Evidence: evidence}, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (r *Runtime) Stop(ctx context.Context, request ports.StopRequest) (ports.StopResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	acknowledged, exited, err := r.terminal.Stop(ctx, request.Force)
	if exited {
		r.cleanupResources(ctx)
		err = errors.Join(err, r.cleanupErr)
	}
	evidence, evidenceErr := r.providerEvidenceUnlocked()
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
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.providerEvidenceUnlocked()
}

func (r *Runtime) providerEvidenceUnlocked() (model.ProviderEvidence, error) {
	identity := r.terminal.Identity()
	var callbackEvidence *nativeguidance.CallbackEvidence
	if r.callback != nil {
		value := r.callback.Evidence()
		callbackEvidence = &value
	}
	return encodeEvidence(evidence{HostSandbox: r.artifact, HostSandboxPolicyHash: r.policyHash, NativeHome: r.nativeHome, ExecutionID: string(r.executionID), NativeID: r.nativeID, Terminal: &identity,
		Intent: r.intent, ContextReady: r.contextReady, ProviderOrder: r.providerOrder,
		Access: r.access, ObservationSpool: r.spool.Directory(), Callback: callbackEvidence})
}

func (r *Runtime) cleanupResources(ctx context.Context) {
	r.cleanupOnce.Do(func() {
		r.cleanupErr = errors.Join(r.cleanupErr, host.SettleSandboxChild(r.artifact))
		if r.callback != nil {
			if err := r.callback.Remove(ctx); err != nil {
				r.cleanupErr = errors.Join(r.cleanupErr, err)
			} else {
				r.callback = nil
			}
		}
		if r.access != nil {
			r.cleanupErr = errors.Join(r.cleanupErr, r.provider.credentials.RemoveActionCredential(ctx, *r.access))
		}
		if r.spool != nil {
			r.cleanupErr = errors.Join(r.cleanupErr, r.spool.Remove())
		}
	})
}

type sessionStartEvent struct {
	SessionID     string `json:"session_id"`
	Transcript    string `json:"transcript_path"`
	HookEventName string `json:"hook_event_name"`
	Source        string `json:"source"`
	AgentID       string `json:"agent_id,omitempty"`
}

func (r *Runtime) consumeObservationEvents(ctx context.Context, transition *ports.ContextChange) (bool, error) {
	if r.spool == nil || r.observations == nil {
		return false, nil
	}
	events, err := r.spool.ReadPending()
	if err != nil {
		return false, err
	}
	confirmed := false
	for _, spooled := range events {
		if spooled.Order == r.providerOrder {
			if err := r.spool.Acknowledge(spooled.Order); err != nil {
				return false, err
			}
			continue
		}
		var event sessionStartEvent
		if err := json.Unmarshal(spooled.Payload, &event); err != nil || event.HookEventName != "SessionStart" || event.AgentID != "" {
			if err := r.spool.Acknowledge(spooled.Order); err != nil {
				return false, err
			}
			continue
		}
		if _, err := uuid.Parse(event.SessionID); err != nil {
			if err := r.spool.Acknowledge(spooled.Order); err != nil {
				return false, err
			}
			continue
		}
		prior := nativeBinding(r.nativeID)
		next := nativeBinding(event.SessionID)
		disposition := ports.PrimaryContextUnresolved
		transitionCorrelation := ""
		var expectedConversation model.ConversationID
		var expectedRevision model.Revision
		switch {
		case transition != nil && event.Source == "clear":
			disposition = ports.PrimaryContextReset
			transitionCorrelation = transition.TransitionCorrelation
			expectedConversation = transition.ExpectedConversation
			expectedRevision = transition.ExpectedAssociationRevision
		case transition == nil && !r.contextReady && event.SessionID == r.nativeID &&
			((r.intent == ports.StartFresh && event.Source == "startup") || (r.intent == ports.StartContinue && event.Source == "resume")):
			if r.intent == ports.StartFresh {
				disposition = ports.PrimaryContextInitial
				prior = nil
			} else {
				disposition = ports.PrimaryContextContinuity
			}
		case transition == nil && r.contextReady && event.Source == "compact" && event.SessionID == r.nativeID:
			disposition = ports.PrimaryContextContinuity
		}
		evidence := ports.PrimaryContextEvidence{
			ExecutionID: r.executionID, Attempt: r.attempt, Provider: Name,
			PrimaryCorrelation: r.primaryCorrelation(), Disposition: disposition,
			PriorBinding: prior, NextBinding: next, TransitionCorrelation: transitionCorrelation,
			ExpectedConversation: expectedConversation, ExpectedAssociationRevision: expectedRevision,
			PriorProviderOrder: r.providerOrder, ProviderOrder: spooled.Order, ObservedAt: time.Now().UTC(),
		}
		if err := r.observations.ObservePrimaryContext(ctx, evidence); err != nil {
			return false, err
		}
		r.providerOrder = spooled.Order
		if err := r.spool.Acknowledge(spooled.Order); err != nil {
			return false, err
		}
		if disposition == ports.PrimaryContextInitial || disposition == ports.PrimaryContextContinuity || disposition == ports.PrimaryContextReset {
			r.nativeID = event.SessionID
			r.contextReady = true
		}
		if disposition == ports.PrimaryContextReset {
			confirmed = true
		}
	}
	return confirmed, nil
}

func (r *Runtime) primaryCorrelation() string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d", r.spool.Directory(), r.executionID, r.attempt)))
	return fmt.Sprintf("terminal:%x", digest[:16])
}

func nativeBinding(id string) *model.NativeBinding {
	if id == "" {
		return nil
	}
	return &model.NativeBinding{Namespace: NativeNamespace, Reference: id}
}

const claudeObservationCommand = `set -eu
umask 077
tmp=$(mktemp "$TCLAUDE_OBSERVATION_SPOOL/.event-XXXXXX")
trap 'rm -f "$tmp"' EXIT
cat >"$tmp"
name=${tmp##*/}
name=${name#.event-}
mv "$tmp" "$TCLAUDE_OBSERVATION_SPOOL/event-$name"`

type terminalAttachment struct{ host.TerminalAttachment }

var _ ports.ResizableAttachment = terminalAttachment{}

func (terminalAttachment) Kind() ports.AttachmentKind { return ports.AttachmentTerminal }
func (a terminalAttachment) Resize(ctx context.Context, size ports.TerminalSize) error {
	return a.TerminalAttachment.Resize(ctx, size.Columns, size.Rows)
}

func nativeIDFor(request ports.PreparationRequest) (string, error) {
	switch request.Intent {
	case ports.StartFresh:
		return uuid.NewString(), nil
	case ports.StartContinue:
		if request.Continuation == nil || request.Continuation.Namespace != NativeNamespace {
			return "", fmt.Errorf("claude continuation requires %s native evidence", NativeNamespace)
		}
		if _, err := uuid.Parse(request.Continuation.Reference); err != nil {
			return "", fmt.Errorf("invalid Claude continuation reference: %w", err)
		}
		return request.Continuation.Reference, nil
	case ports.StartFork:
		return "", ports.ErrHistoryUnsupported
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
var _ ports.ActionCredentialProvider = (*Provider)(nil)
var _ ports.PreparedAttempt = (*prepared)(nil)
var _ ports.Runtime = (*Runtime)(nil)

func supportedLaunchPolicy() ports.PolicyRequirements {
	return ports.PolicyRequirements{DefaultApproval: model.ApprovalAutomatic, DefaultSandbox: model.SandboxWorkspaceWrite, SupportedApproval: claudeApprovalModes(), ApprovalDescriptions: claudeApprovalDescriptions(), SupportedSandbox: []model.SandboxMode{model.SandboxWorkspaceWrite}}
}
