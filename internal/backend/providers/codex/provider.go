// Package codex implements the replacement backend's terminal-authoritative
// OpenAI Codex CLI provider.
package codex

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/tofutools/tclaude/internal/backend/providers/nativeactivity"
	"github.com/tofutools/tclaude/internal/backend/providers/nativeguidance"
)

const (
	Name                   = "codex"
	NativeNamespace        = "openai-codex-cli"
	evidenceVersion uint32 = 1
)

type Config struct {
	AgentSocketDirectory string
	HostSandbox          *host.SandboxLaunchPreparer
	Executable           string
	TmuxExecutable       string
	PrivateRoot          string
	// NativeHome is a durable provider-owned CODEX_HOME. Empty selects
	// PrivateRoot/native-home; an override must remain inside PrivateRoot.
	NativeHome  string
	AgentSocket string
	TurnForker  TurnForker
}

type Provider struct {
	agentSocketDirectory string
	hostSandbox          *host.SandboxLaunchPreparer
	executable           string
	privateRoot          string
	nativeHome           string
	terminal             host.TerminalHost
	credentials          host.ActionCredentialHost
	agentSocket          string
	observationRoot      string
	turnForker           TurnForker
	stateMu              sync.Mutex
}

func New(config Config) (*Provider, error) {
	executable := config.Executable
	if executable == "" {
		executable = "codex"
	}
	resolved, err := exec.LookPath(executable)
	if err != nil {
		return nil, fmt.Errorf("resolve Codex executable: %w", err)
	}
	if !filepath.IsAbs(config.PrivateRoot) {
		return nil, fmt.Errorf("codex private root must be absolute")
	}
	nativeHome := config.NativeHome
	if nativeHome == "" {
		nativeHome = filepath.Join(config.PrivateRoot, "native-home")
	}
	if !filepath.IsAbs(nativeHome) || !pathWithin(config.PrivateRoot, nativeHome) {
		return nil, fmt.Errorf("codex native home must be an absolute provider-owned path inside private root")
	}
	forker := config.TurnForker
	if forker == nil {
		forker = nativeTurnForker{executable: resolved}
	}
	return &Provider{
		agentSocketDirectory: config.AgentSocketDirectory, hostSandbox: config.HostSandbox, executable: resolved, privateRoot: config.PrivateRoot, nativeHome: filepath.Clean(nativeHome),
		terminal:    host.TerminalHost{Executable: config.TmuxExecutable, PrivateRoot: filepath.Join(config.PrivateRoot, "terminals")},
		credentials: host.ActionCredentialHost{PrivateRoot: filepath.Join(config.PrivateRoot, "action-credentials")},
		agentSocket: config.AgentSocket, observationRoot: filepath.Join(config.PrivateRoot, "observations"), turnForker: forker,
	}, nil
}

func (*Provider) Name() string { return Name }
func (p *Provider) Capabilities() ports.ProviderCapabilities {
	policy := supportedLaunchPolicy()
	return ports.ProviderCapabilities{HostSandbox: p.hostSandbox != nil, LaunchPolicy: &policy, PreparedInitialInput: true, NativeGuidance: []ports.NativeGuidanceCapability{{EventKind: "session_start", Timing: model.StandingOrderSameContinuation}, {EventKind: "user_prompt", Timing: model.StandingOrderSameContinuation}}}
}
func (p *Provider) ActionCredentials() ports.ActionCredentialDelivery { return p.credentials }
func (p *Provider) History() ports.HistoryReader                      { return historyReader{provider: p} }

type evidence struct {
	ForkReceipt           string                           `json:"fork_receipt,omitempty"`
	HostSandbox           *host.SandboxChildArtifact       `json:"host_sandbox,omitempty"`
	HostSandboxPolicyHash string                           `json:"host_sandbox_policy_hash,omitempty"`
	ExecutionID           string                           `json:"execution_id"`
	NativeID              string                           `json:"native_id"`
	Intent                ports.StartIntent                `json:"intent"`
	StateRoot             string                           `json:"state_root"`
	RemoveOnAbort         bool                             `json:"remove_on_abort,omitempty"`
	ContextReady          bool                             `json:"context_ready,omitempty"`
	ProviderOrder         string                           `json:"provider_order,omitempty"`
	Prepared              *host.PreparedTerminalIdentity   `json:"prepared,omitempty"`
	Terminal              *host.TerminalIdentity           `json:"terminal,omitempty"`
	Access                *ports.ActionCredentialReceipt   `json:"access,omitempty"`
	ObservationSpool      string                           `json:"observation_spool"`
	Callback              *nativeguidance.CallbackEvidence `json:"native_callback,omitempty"`
}

type prepared struct {
	forkReceipt     string
	artifact        *host.SandboxChildArtifact
	command         host.ProcessSpec
	provider        *Provider
	request         ports.PreparationRequest
	nativeID        string
	stateRoot       string
	removeOnAbort   bool
	terminal        *host.PreparedTerminal
	spool           *host.ObservationSpool
	access          *ports.ActionCredentialReceipt
	callback        *nativeguidance.CallbackResource
	guidance        *nativeguidance.Runtime
	handler         *nativeguidance.CallbackHandler
	normalizer      *codexNativeNormalizer
	callbackCommand string
	description     ports.PreparedDescription
}

func (p *Provider) Prepare(ctx context.Context, request ports.PreparationRequest) (ports.PreparedAttempt, error) {
	if request.Spec.HostSandbox == nil && request.HostSandboxPolicy != nil {
		return nil, fmt.Errorf("sandbox materialization has no selected identity")
	}
	if request.Spec.HostSandbox != nil {
		if p.hostSandbox == nil || request.HostSandboxPolicy == nil {
			return nil, fmt.Errorf("selected host sandbox preparation is unavailable")
		}
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
		return nil, fmt.Errorf("codex provider cannot prepare harness %q", request.Spec.Harness)
	}
	if err := model.ValidateEffort(request.Spec.Effort); err != nil {
		return nil, err
	}
	if err := validateDirectory(request.Spec.WorkingDirectory); err != nil {
		return nil, err
	}
	if request.Spec.Approval != model.ApprovalSupervised && request.Spec.Approval != model.ApprovalAutomatic {
		return nil, fmt.Errorf("codex provider does not support approval mode %q", request.Spec.Approval)
	}
	if request.Spec.Sandbox != model.SandboxReadOnly && request.Spec.Sandbox != model.SandboxWorkspaceWrite && request.Spec.Sandbox != model.SandboxUnconfined {
		return nil, fmt.Errorf("codex provider does not support sandbox mode %q", request.Spec.Sandbox)
	}
	initialInput, err := preparedInitialInput(request.InitialInput)
	if err != nil {
		return nil, err
	}
	nativeID, stateRoot, removeOnAbort, err := p.prepareHistory(request)
	if err != nil {
		return nil, err
	}
	var callback *nativeguidance.CallbackResource
	cleanupState := func() {
		if callback != nil {
			_ = callback.Remove(context.Background())
		}
		if removeOnAbort {
			_ = os.RemoveAll(stateRoot)
		}
	}
	var guidance *nativeguidance.Runtime
	var handler *nativeguidance.CallbackHandler
	var normalizer *codexNativeNormalizer
	var callbackCommand string
	if request.NativeGuidance != nil {
		if request.CallbackIngress == nil {
			cleanupState()
			return nil, fmt.Errorf("codex native guidance requires callback ingress")
		}
		callback, err = nativeguidance.PrepareCallback(filepath.Join(p.privateRoot, "native-callbacks"))
		if err == nil {
			normalizer = newCodexNativeNormalizer(nativeID)
			guidance = &nativeguidance.Runtime{Evaluator: request.NativeGuidance, Kinds: map[string]struct{}{"session_start": {}, "user_prompt": {}}, Correlation: normalizer.Matches}
			handler = &nativeguidance.CallbackHandler{Normalize: normalizer.Normalize, Encode: encodeCodexGuidance}
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
			cleanupState()
			return nil, err
		}
	}
	if err := p.prepareStateRoot(stateRoot, callbackCommand); err != nil {
		if callback != nil {
			_ = callback.Remove(context.Background())
		}
		cleanupState()
		return nil, err
	}
	var access *ports.ActionCredentialReceipt
	if request.ActionCredential != nil {
		if request.ActionCredential.ExecutionID != request.Spec.ExecutionID {
			cleanupState()
			return nil, fmt.Errorf("action credential does not match Codex execution")
		}
		if !filepath.IsAbs(p.agentSocket) {
			cleanupState()
			return nil, fmt.Errorf("codex agent API socket must be absolute for credential delivery")
		}
		receipt, deliveryErr := p.credentials.PrepareActionCredential(ctx, *request.ActionCredential)
		if deliveryErr != nil {
			cleanupState()
			return nil, deliveryErr
		}
		access = &receipt
	}
	terminal, err := p.terminal.Prepare(string(request.Spec.ExecutionID))
	if err != nil {
		cleanupState()
		if access != nil {
			_ = p.credentials.RemoveActionCredential(context.Background(), *access)
		}
		return nil, err
	}
	spool, err := host.PrepareObservationSpool(p.observationRoot)
	if err != nil {
		_ = terminal.Abort()
		cleanupState()
		if access != nil {
			_ = p.credentials.RemoveActionCredential(context.Background(), *access)
		}
		return nil, err
	}
	preparedIdentity := terminal.Identity()
	var callbackEvidence *nativeguidance.CallbackEvidence
	if callback != nil {
		value := callback.Evidence()
		callbackEvidence = &value
	}
	initial, err := encodeEvidence(evidence{ExecutionID: string(request.Spec.ExecutionID), NativeID: nativeID, Intent: request.Intent, StateRoot: stateRoot, RemoveOnAbort: removeOnAbort, Prepared: &preparedIdentity, Access: access, ObservationSpool: spool.Directory(), Callback: callbackEvidence})
	if err != nil {
		_ = terminal.Abort()
		_ = spool.Remove()
		if callback != nil {
			_ = callback.Remove(context.Background())
		}
		cleanupState()
		if access != nil {
			_ = p.credentials.RemoveActionCredential(context.Background(), *access)
		}
		return nil, err
	}
	result := &prepared{provider: p, request: request, nativeID: nativeID, stateRoot: stateRoot, removeOnAbort: removeOnAbort, terminal: terminal, spool: spool, access: access, callback: callback, guidance: guidance, handler: handler, normalizer: normalizer, callbackCommand: callbackCommand,
		description: ports.PreparedDescription{ExecutionID: request.Spec.ExecutionID, Attempt: request.Spec.Attempt, Topology: ports.TopologyTerminalAuthoritative,
			Requirements:    ports.RuntimeRequirements{Executable: p.executable, WorkingDirectory: request.Spec.WorkingDirectory, PrivateStorage: true, Terminal: &ports.TerminalRequirement{Interactive: true}, Policy: supportedLaunchPolicy()},
			EffectivePolicy: ports.EffectivePolicy{Approval: request.Spec.Approval, Sandbox: request.Spec.Sandbox, ApprovalEnforced: true, SandboxEnforced: true},
			Resources:       []ports.ResourceClaim{{Kind: ports.ResourceTerminal, Key: terminal.ResourceKey()}, {Kind: ports.ResourceProcess, Key: stateRoot}}, Evidence: initial, AccessDelivery: access, InitialInput: initialInput}}
	if err := result.prepareSandbox(ctx); err != nil {
		_ = result.Abort(context.WithoutCancel(ctx))
		return nil, err
	}
	return result, nil
}

func (p *Provider) prepareHistory(request ports.PreparationRequest) (string, string, bool, error) {
	switch request.Intent {
	case ports.StartFresh:
		return "", p.nativeHome, false, nil
	case ports.StartContinue, ports.StartFork:
		if request.Intent == ports.StartContinue && request.History == nil {
			if request.Continuation == nil || request.Continuation.Namespace != NativeNamespace || request.Continuation.Reference == "" {
				return "", "", false, fmt.Errorf("codex continuation requires native conversation evidence")
			}
			prior, priorErr := decodeEvidence(request.PriorEvidence)
			if priorErr != nil || prior.NativeID != request.Continuation.Reference || filepath.Clean(prior.StateRoot) != p.nativeHome {
				return "", "", false, fmt.Errorf("codex continuation evidence does not match provider-owned native state")
			}
			return prior.NativeID, prior.StateRoot, false, nil
		}
		if request.History == nil || request.History.Provider != Name || request.History.Native.Namespace != NativeNamespace {
			return "", "", false, fmt.Errorf("codex continuation or fork requires application-resolved history")
		}
		token, err := decodeSourceToken(request.History.SourceToken)
		if err != nil || token.SessionID != request.History.Native.Reference || filepath.Clean(token.StateRoot) != p.nativeHome {
			return "", "", false, fmt.Errorf("codex continuation history evidence is invalid")
		}
		if _, err := verifyHistorySelection(*request.History, token); err != nil {
			return "", "", false, err
		}
		if request.Intent == ports.StartFork && (request.History.Point == nil || request.History.Point.Kind != model.HistoryPointTurn) {
			return "", "", false, fmt.Errorf("codex fork requires an exact turn selection")
		}
		return token.SessionID, token.StateRoot, false, nil
	default:
		return "", "", false, fmt.Errorf("unsupported start intent %q", request.Intent)
	}
}

func (p *Provider) prepareStateRoot(root, _ string) error {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	if err := os.MkdirAll(filepath.Join(root, "hooks"), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return err
	}
	hookScript := filepath.Join(root, "hooks", "tclaude-observation")
	if err := host.WriteProtectedFile(hookScript, []byte("#!/bin/sh\n"+observationCommand+"\n")); err != nil {
		return err
	}
	if err := os.Chmod(hookScript, 0o700); err != nil {
		return err
	}
	callbackDispatcher := filepath.Join(root, "hooks", "tclaude-native-callback")
	if err := host.WriteProtectedFile(callbackDispatcher, []byte("#!/bin/sh\n"+codexCallbackDispatchCommand+"\n")); err != nil {
		return err
	}
	if err := os.Chmod(callbackDispatcher, 0o700); err != nil {
		return err
	}
	callback := map[string]any{"type": "command", "command": callbackDispatcher}
	sessionHooks := []any{map[string]any{"type": "command", "command": hookScript}, callback}
	hookGroups := map[string]any{
		"SessionStart":     []any{map[string]any{"hooks": sessionHooks}},
		"UserPromptSubmit": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": hookScript}, callback}}},
		"Stop":             []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": hookScript}}}},
	}
	hooks := map[string]any{"hooks": hookGroups}
	raw, err := json.Marshal(hooks)
	if err != nil {
		return err
	}
	// Reuse unchanged shared configuration: concurrent prepared launches pin
	// this inode read-only and must not invalidate one another.
	hooksPath := filepath.Join(root, "hooks.json")
	existing, readErr := host.ReadProtectedFile(hooksPath, 1<<20)
	if readErr != nil || string(existing) != string(raw) {
		if err := host.WriteProtectedFile(hooksPath, raw); err != nil {
			return err
		}
	}
	return nil
}

func (p *prepared) Describe() ports.PreparedDescription { return p.description }
func (p *prepared) Abort(ctx context.Context) error {
	err := p.terminal.Abort()
	if err == nil && p.artifact != nil {
		err = errors.Join(err, host.AbortSandboxChild(*p.artifact))
	}
	if err == nil && p.forkReceipt != "" {
		err = os.RemoveAll(filepath.Dir(p.forkReceipt))
	}
	err = errors.Join(err, p.spool.Remove())
	if p.callback != nil {
		err = errors.Join(err, p.callback.Remove(ctx))
	}
	if p.access != nil {
		err = errors.Join(err, p.provider.credentials.RemoveActionCredential(ctx, *p.access))
	}
	if p.removeOnAbort {
		err = errors.Join(err, os.RemoveAll(p.stateRoot))
	}
	return err
}
func (p *prepared) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ReleaseResult, error) {
	if permit == nil || permit.ExecutionID() != p.request.Spec.ExecutionID {
		return ports.ReleaseResult{}, fmt.Errorf("release permit does not match execution")
	}
	if p.guidance != nil {
		p.guidance.Evidence = func() (model.ProviderEvidence, error) { return p.description.Evidence, nil }
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
	if p.request.Intent == ports.StartFork {
		token, tokenErr := decodeSourceToken(p.request.History.SourceToken)
		if tokenErr != nil {
			return ports.ReleaseResult{}, tokenErr
		}
		if _, verifyErr := verifyHistorySelection(*p.request.History, token); verifyErr != nil {
			return ports.ReleaseResult{}, verifyErr
		}
		if p.artifact != nil {
			// The supervised child forks and execs the TUI under one boundary.
			// Its receipt supplies the new identity; never report the source
			// as the conversation owned by this new execution.
			p.nativeID = ""
		} else {
			forkedID, forkErr := p.provider.turnForker.Fork(ctx, TurnForkRequest{StateRoot: p.stateRoot, WorkingDirectory: p.request.Spec.WorkingDirectory, ThreadID: p.nativeID, LastTurnID: p.request.History.Point.Token})
			if forkErr != nil {
				return ports.ReleaseResult{}, forkErr
			}
			p.nativeID = forkedID
			if p.normalizer != nil {
				p.normalizer.Set(forkedID)
			}
		}
	}
	command := p.command
	if p.artifact == nil {
		command = host.ProcessSpec{Executable: p.provider.executable, Args: p.argv(), Directory: p.request.Spec.WorkingDirectory, Env: p.runtimeEnvironment()}
	}
	terminal, err := p.terminal.Release(command)
	if err != nil {
		if terminal != nil {
			r := p.runtime(terminal)
			evidence, _ := r.providerEvidence()
			return ports.ReleaseResult{State: ports.ReleaseUncertain, Runtime: r, Evidence: evidence}, err
		}
		_ = p.spool.Remove()
		if p.callback != nil {
			_ = p.callback.Remove(context.Background())
		}
		if p.access != nil {
			_ = p.provider.credentials.RemoveActionCredential(context.Background(), *p.access)
		}
		if p.removeOnAbort {
			_ = os.RemoveAll(p.stateRoot)
		}
		return ports.ReleaseResult{}, err
	}
	r := p.runtime(terminal)
	evidence, e := r.providerEvidence()
	if e != nil {
		return ports.ReleaseResult{State: ports.ReleaseUncertain, Runtime: r}, e
	}
	return ports.ReleaseResult{State: ports.ReleaseStarted, Runtime: r, Evidence: evidence}, nil
}
func (p *prepared) argv() []string {
	args := []string{"--dangerously-bypass-hook-trust", "-a", "on-request", "-s", codexSandbox(p.request.Spec.Sandbox)}
	if p.request.Spec.Approval == model.ApprovalAutomatic {
		args[2] = "never"
	}
	switch p.request.Intent {
	case ports.StartContinue:
		args = append(args, "resume", p.nativeID)
	case ports.StartFork:
		args = append(args, "resume", p.nativeID)
	}
	if p.request.Spec.Effort != "" {
		encoded, _ := json.Marshal(p.request.Spec.Effort)
		args = append(args, "-c", "model_reasoning_effort="+string(encoded))
	}
	if p.request.Spec.Model != "" {
		args = append(args, "--model", p.request.Spec.Model)
	}
	if p.request.InitialInput != nil {
		args = append(args, p.request.InitialInput.Body)
	}
	return args
}

func preparedInitialInput(input *ports.PreparedInitialInput) (*ports.PreparedInitialInputDescription, error) {
	if input == nil {
		return nil, nil
	}
	if strings.TrimSpace(input.Body) == "" || strings.TrimSpace(input.Correlation) == "" {
		return nil, fmt.Errorf("codex prepared initial input requires body and correlation")
	}
	return &ports.PreparedInitialInputDescription{Correlation: input.Correlation, Supported: true}, nil
}
func (p *prepared) runtimeEnvironment() []string {
	result := []string{"CODEX_HOME=" + p.stateRoot, "TCLAUDE_OBSERVATION_SPOOL=" + p.spool.Directory(), "TCLAUDE_BACKEND_CREDENTIAL_FILE=", "TCLAUDE_BACKEND_SOCKET=", "TCLAUDE_NATIVE_CALLBACK_SCRIPT="}
	if p.access != nil {
		result = append(result, "TCLAUDE_BACKEND_CREDENTIAL_FILE="+p.access.Resource, "TCLAUDE_BACKEND_SOCKET="+p.provider.agentSocket)
	}
	if p.callbackCommand != "" {
		result = append(result, "TCLAUDE_NATIVE_CALLBACK_SCRIPT="+p.callbackCommand)
	}
	return append(p.request.Spec.Environment.Entries(), result...)
}
func (p *prepared) runtime(t *host.Terminal) *Runtime {
	return &Runtime{forkReceipt: p.forkReceipt, artifact: p.artifact, policyHash: p.description.HostSandboxPolicyHash, provider: p.provider, executionID: p.request.Spec.ExecutionID, attempt: p.request.Spec.Attempt, terminal: t, nativeID: p.nativeID, intent: p.request.Intent, stateRoot: p.stateRoot, observations: p.request.Observations, access: p.access, spool: p.spool, guidance: p.guidance, callback: p.callback}
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
	if recorded.HostSandboxPolicyHash != expectedHash || (recorded.HostSandbox != nil) != (expectedHash != "") {
		return ports.RecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence}, nil
	}
	if recorded.ExecutionID != string(request.ExecutionID) || filepath.Clean(recorded.StateRoot) != p.nativeHome {
		return ports.RecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence}, nil
	}
	if recorded.ForkReceipt != "" {
		root := filepath.Join(p.privateRoot, "fork-results")
		if recorded.Intent != ports.StartFork || recorded.HostSandbox == nil || !pathWithin(root, recorded.ForkReceipt) {
			return ports.RecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence}, nil
		}
		if recorded.NativeID == "" {
			recorded.NativeID, _ = readForkReceipt(recorded.ForkReceipt)
		}
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
		cleanup := host.RemoveObservationSpool(p.observationRoot, recorded.ObservationSpool)
		if recorded.Callback != nil {
			if resource, callbackErr := nativeguidance.RecoverCallback(filepath.Join(p.privateRoot, "native-callbacks"), *recorded.Callback); callbackErr == nil {
				cleanup = errors.Join(cleanup, resource.Remove(ctx))
			} else {
				cleanup = errors.Join(cleanup, callbackErr)
			}
		}
		if recorded.Access != nil {
			cleanup = errors.Join(cleanup, p.credentials.RemoveActionCredential(ctx, *recorded.Access))
		}
		return ports.RecoveryResult{State: ports.RecoveryExited, Evidence: request.Evidence, Observation: ports.Observation{ObservedAt: time.Now(), Workload: ports.WorkloadExited, Context: ports.ContextUnknown, NativeConversation: nativeEvidence(recorded.NativeID)}}, cleanup
	}
	if err != nil {
		return ports.RecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence}, nil
	}
	spool, err := host.RecoverObservationSpool(p.observationRoot, recorded.ObservationSpool)
	if err != nil {
		return ports.RecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence, Attempt: request.Attempt}, err
	}
	var proof *ports.ActionCredentialRecoveryProof
	if request.Access != nil {
		if recorded.Access == nil || recorded.Access.DeliveryID != request.Access.DeliveryID {
			return ports.RecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence, Attempt: request.Attempt}, nil
		}
		v, e := p.credentials.InspectActionCredential(ctx, *request.Access)
		if e != nil || v.Resource != recorded.Access.Resource {
			return ports.RecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence, Attempt: request.Attempt}, e
		}
		proof = &v
	}
	var callback *nativeguidance.CallbackResource
	var guidance *nativeguidance.Runtime
	if recorded.Callback != nil {
		if request.NativeGuidance == nil || request.CallbackIngress == nil {
			return ports.RecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence, Attempt: request.Attempt}, nil
		}
		callback, err = nativeguidance.RecoverCallback(filepath.Join(p.privateRoot, "native-callbacks"), *recorded.Callback)
		if err != nil {
			return ports.RecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence, Attempt: request.Attempt}, err
		}
		normalizer := newCodexNativeNormalizer(recorded.NativeID)
		if recorded.ForkReceipt != "" {
			normalizer.SetForkReceipt(recorded.ForkReceipt)
			normalizer.Set(recorded.NativeID)
		}
		handler := &nativeguidance.CallbackHandler{Normalize: normalizer.Normalize, Encode: encodeCodexGuidance}
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
		guidance = &nativeguidance.Runtime{Evaluator: request.NativeGuidance, Evidence: func() (model.ProviderEvidence, error) { return request.Evidence, nil }, Kinds: map[string]struct{}{"session_start": {}, "user_prompt": {}}, Correlation: normalizer.Matches}
		if err := handler.Bind(guidance); err != nil {
			_ = callback.CloseRegistration(context.Background())
			return ports.RecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence, Attempt: request.Attempt}, err
		}
	} else if request.NativeGuidance != nil {
		return ports.RecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence, Attempt: request.Attempt}, nil
	}
	r := &Runtime{forkReceipt: recorded.ForkReceipt, artifact: recorded.HostSandbox, policyHash: recorded.HostSandboxPolicyHash, provider: p, executionID: request.ExecutionID, attempt: request.Attempt, terminal: terminal, nativeID: recorded.NativeID, intent: recorded.Intent, stateRoot: recorded.StateRoot, observations: request.Observations, access: recorded.Access, spool: spool, contextReady: recorded.ContextReady, providerOrder: recorded.ProviderOrder, guidance: guidance, callback: callback}
	obs, _ := r.Observe(ctx)
	return ports.RecoveryResult{State: ports.RecoveryControlled, Runtime: r, Observation: obs, Evidence: request.Evidence, Attempt: request.Attempt, AccessProof: proof}, nil
}

type Runtime struct {
	activity       nativeactivity.State
	activityLoaded bool
	forkReceipt    string
	artifact       *host.SandboxChildArtifact
	policyHash     string
	provider       *Provider
	executionID    model.ExecutionID
	attempt        model.AttemptGeneration
	terminal       *host.Terminal
	nativeID       string
	intent         ports.StartIntent
	stateRoot      string
	observations   ports.PrimaryObservationSink
	access         *ports.ActionCredentialReceipt
	spool          *host.ObservationSpool
	contextReady   bool
	providerOrder  string
	guidance       *nativeguidance.Runtime
	callback       *nativeguidance.CallbackResource
	cleanupOnce    sync.Once
	cleanupErr     error
	mu             sync.Mutex
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
	r.refreshForkIdentity()
	observed := r.terminal.Observe()
	var ingress error
	if observed.Running {
		ingress = r.consumeObservations(ctx)
	}
	out := ports.Observation{ObservedAt: time.Now(), Context: ports.ContextUnknown, NativeConversation: nativeEvidence(r.nativeID)}
	switch {
	case observed.Running:
		out.Workload = ports.WorkloadRunning
		out.AgentActivity, out.AgentActivityObservedAt = r.activity.Observation()
		out.AttachmentActive = r.terminal.AttachmentActive()
		if r.contextReady {
			out.Context = ports.ContextReady
		}
	case observed.Exited:
		out.Workload = ports.WorkloadExited
		out.ExitCode = observed.ExitCode
		r.cleanup(ctx)
	case observed.Unknown:
		out.Workload = ports.WorkloadUnknown
	}
	e, err := r.providerEvidenceUnlocked()
	out.Evidence = e
	return out, errors.Join(ingress, err, r.cleanupErr)
}
func (r *Runtime) Interact(ctx context.Context, in ports.Interaction) (ports.InteractionResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.TrimSpace(in.Text) == "" {
		return ports.InteractionResult{Disposition: ports.EffectRefused}, nil
	}
	r.activity.Invalidate()
	if err := r.checkpointActivity(); err != nil {
		return ports.InteractionResult{Disposition: ports.EffectRefused}, err
	}
	if err := r.terminal.SendLiteral(ctx, in.Text); err != nil {
		e, _ := r.providerEvidenceUnlocked()
		return ports.InteractionResult{Disposition: ports.EffectUnknown, Evidence: e}, err
	}
	e, err := r.providerEvidenceUnlocked()
	return ports.InteractionResult{Disposition: ports.EffectAccepted, Evidence: e}, err
}
func (r *Runtime) Attach(ctx context.Context, request ports.AttachmentRequest) (ports.AttachmentResult, error) {
	if request.Kind != ports.AttachmentTerminal {
		return ports.AttachmentResult{Disposition: ports.EffectUnsupported}, nil
	}
	a, err := r.terminal.Attach(ctx)
	if err != nil {
		return ports.AttachmentResult{Disposition: ports.EffectRefused}, err
	}
	e, err := r.providerEvidence()
	if err != nil {
		_ = a.Close()
		return ports.AttachmentResult{}, err
	}
	return ports.AttachmentResult{Disposition: ports.EffectAccepted, Attachment: terminalAttachment{a}, Evidence: e}, nil
}
func (r *Runtime) ChangeContext(context.Context, ports.ContextChange) (ports.ContextChangeResult, error) {
	return ports.ContextChangeResult{Disposition: ports.EffectUnsupported}, nil
}
func (r *Runtime) Stop(ctx context.Context, request ports.StopRequest) (ports.StopResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ack, exited, err := r.terminal.Stop(ctx, request.Force)
	if exited {
		r.cleanup(ctx)
		err = errors.Join(err, r.cleanupErr)
	}
	e, eerr := r.providerEvidenceUnlocked()
	err = errors.Join(err, eerr)
	d := ports.EffectAccepted
	if err != nil {
		d = ports.EffectUnknown
	}
	return ports.StopResult{Disposition: d, Acknowledged: ack, Exited: exited, Evidence: e}, err
}
func (r *Runtime) providerEvidence() (model.ProviderEvidence, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.providerEvidenceUnlocked()
}
func (r *Runtime) refreshForkIdentity() {
	if r.forkReceipt != "" && r.nativeID == "" {
		if id, err := readForkReceipt(r.forkReceipt); err == nil {
			r.nativeID = id
		}
	}
}
func (r *Runtime) providerEvidenceUnlocked() (model.ProviderEvidence, error) {
	r.refreshForkIdentity()
	id := r.terminal.Identity()
	var callbackEvidence *nativeguidance.CallbackEvidence
	if r.callback != nil {
		value := r.callback.Evidence()
		callbackEvidence = &value
	}
	return encodeEvidence(evidence{ForkReceipt: r.forkReceipt, HostSandbox: r.artifact, HostSandboxPolicyHash: r.policyHash, ExecutionID: string(r.executionID), NativeID: r.nativeID, Intent: r.intent, StateRoot: r.stateRoot, ContextReady: r.contextReady, ProviderOrder: r.providerOrder, Terminal: &id, Access: r.access, ObservationSpool: r.spool.Directory(), Callback: callbackEvidence})
}
func (r *Runtime) cleanup(ctx context.Context) {
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
	HookEventName string `json:"hook_event_name"`
	Source        string `json:"source"`
	AgentID       string `json:"agent_id"`
}

func (r *Runtime) consumeObservations(ctx context.Context) error {
	if r.spool == nil {
		return nil
	}
	if !r.activityLoaded {
		if err := r.activity.Restore(r.spool.Directory(), r.nativeID); err != nil {
			return err
		}
		r.activityLoaded = true
	}
	events, err := r.spool.ReadPending()
	if err != nil {
		return err
	}
	for _, sp := range events {
		var event sessionStartEvent
		if json.Unmarshal(sp.Payload, &event) != nil || event.AgentID != "" {
			_ = r.acknowledgeObservation(sp.Order)
			continue
		}
		if _, parseErr := uuid.Parse(event.SessionID); parseErr != nil {
			_ = r.acknowledgeObservation(sp.Order)
			continue
		}
		if event.HookEventName != "SessionStart" {
			if event.SessionID == r.nativeID {
				if state, ok := nativeactivity.HookState(event.HookEventName); ok {
					r.activity.Record(state, sp.RecordedAt)
				}
			}
			if err := r.acknowledgeObservation(sp.Order); err != nil {
				return err
			}
			continue
		}
		if r.observations == nil {
			if err := r.acknowledgeObservation(sp.Order); err != nil {
				return err
			}
			continue
		}
		r.activity.Record(ports.AgentActivityUnknown, sp.RecordedAt)
		var priorBinding *model.NativeBinding
		unexpectedPrimary := false
		if r.nativeID == "" && r.intent == ports.StartFresh {
			r.nativeID = event.SessionID
		} else if event.SessionID != r.nativeID {
			priorBinding = nativeBinding(r.nativeID)
			r.nativeID = event.SessionID
			r.contextReady = false
			unexpectedPrimary = true
		}
		disposition := ports.PrimaryContextUnresolved
		if !unexpectedPrimary && !r.contextReady && ((r.intent == ports.StartFresh && event.Source == "startup") || (r.intent == ports.StartContinue && event.Source == "resume") || (r.intent == ports.StartFork && (event.Source == "fork" || event.Source == "resume"))) {
			disposition = ports.PrimaryContextInitial
		}
		e := ports.PrimaryContextEvidence{ExecutionID: r.executionID, Attempt: r.attempt, Provider: Name, PrimaryCorrelation: r.primaryCorrelation(), Disposition: disposition, PriorBinding: priorBinding, NextBinding: nativeBinding(r.nativeID), PriorProviderOrder: r.providerOrder, ProviderOrder: sp.Order, ObservedAt: time.Now().UTC()}
		if err := r.observations.ObservePrimaryContext(ctx, e); err != nil {
			return err
		}
		r.providerOrder = sp.Order
		if err := r.acknowledgeObservation(sp.Order); err != nil {
			return err
		}
		if disposition == ports.PrimaryContextInitial {
			r.contextReady = true
		}
	}
	return nil
}
func (r *Runtime) primaryCorrelation() string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d", r.spool.Directory(), r.executionID, r.attempt)))
	return fmt.Sprintf("terminal:%x", sum[:16])
}

const observationCommand = `set -eu
umask 077
tmp=$(mktemp "$TCLAUDE_OBSERVATION_SPOOL/.event-XXXXXX")
trap 'rm -f "$tmp"' EXIT
cat >"$tmp"
name=${tmp##*/}; name=${name#.event-}
mv "$tmp" "$TCLAUDE_OBSERVATION_SPOOL/event-$name"`

const codexCallbackDispatchCommand = `set -eu
if [ -z "${TCLAUDE_NATIVE_CALLBACK_SCRIPT:-}" ]; then exit 0; fi
exec "$TCLAUDE_NATIVE_CALLBACK_SCRIPT"`

type terminalAttachment struct{ host.TerminalAttachment }

var _ ports.ResizableAttachment = terminalAttachment{}

func (terminalAttachment) Kind() ports.AttachmentKind { return ports.AttachmentTerminal }
func (a terminalAttachment) Resize(ctx context.Context, size ports.TerminalSize) error {
	return a.TerminalAttachment.Resize(ctx, size.Columns, size.Rows)
}
func nativeBinding(id string) *model.NativeBinding {
	if id == "" {
		return nil
	}
	return &model.NativeBinding{Namespace: NativeNamespace, Reference: id}
}
func nativeEvidence(id string) *model.NativeConversationEvidence {
	if id == "" {
		return nil
	}
	return &model.NativeConversationEvidence{Namespace: NativeNamespace, Reference: id, ObservedAt: time.Now()}
}
func encodeEvidence(v evidence) (model.ProviderEvidence, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return model.ProviderEvidence{}, err
	}
	return model.NewProviderEvidence(Name, evidenceVersion, raw)
}
func decodeEvidence(e model.ProviderEvidence) (evidence, error) {
	if e.Provider != Name || e.Version != evidenceVersion {
		return evidence{}, fmt.Errorf("unsupported Codex evidence %q version %d", e.Provider, e.Version)
	}
	var v evidence
	if err := json.Unmarshal(e.Payload, &v); err != nil {
		return evidence{}, fmt.Errorf("decode Codex evidence: %w", err)
	}
	return v, nil
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
func codexSandbox(mode model.SandboxMode) string {
	switch mode {
	case model.SandboxReadOnly:
		return "read-only"
	case model.SandboxWorkspaceWrite:
		return "workspace-write"
	default:
		return "danger-full-access"
	}
}
func pathWithin(root, path string) bool {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

var _ ports.Provider = (*Provider)(nil)
var _ ports.HistoryProvider = (*Provider)(nil)
var _ ports.ActionCredentialProvider = (*Provider)(nil)
var _ ports.PreparedAttempt = (*prepared)(nil)
var _ ports.Runtime = (*Runtime)(nil)

func supportedLaunchPolicy() ports.PolicyRequirements {
	return ports.PolicyRequirements{SupportedApproval: []model.ApprovalMode{model.ApprovalSupervised, model.ApprovalAutomatic}, SupportedSandbox: []model.SandboxMode{model.SandboxReadOnly, model.SandboxWorkspaceWrite, model.SandboxUnconfined}}
}

func (r *Runtime) checkpointActivity() error {
	if r.spool == nil {
		return nil
	}
	return r.activity.Save(r.spool.Directory(), r.nativeID)
}

func (r *Runtime) acknowledgeObservation(order string) error {
	if err := r.checkpointActivity(); err != nil {
		return err
	}
	return r.spool.Acknowledge(order)
}
