// Package opencode implements the replacement backend's server-authoritative
// OpenCode provider. The owned server, rather than an optional attach client,
// is the workload and control boundary.
package opencode

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/creack/pty"
	"github.com/google/uuid"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

const (
	Name                    = "opencode"
	NativeNamespace         = "opencode"
	serverUsername          = "opencode"
	evidenceVersion  uint32 = 1
	attemptMarkerKey        = "TCLAUDE_RUNTIME_ATTEMPT"
)

type Config struct {
	Executable  string
	PrivateRoot string
	AgentSocket string
	Environment []string
	HTTPClient  *http.Client
}

type Provider struct {
	executable  string
	privateRoot string
	environment []string
	httpClient  *http.Client
	credentials host.ActionCredentialHost
	agentSocket string
}

func New(config Config) (*Provider, error) {
	executable := config.Executable
	if executable == "" {
		executable = "opencode"
	}
	resolved, err := exec.LookPath(executable)
	if err != nil {
		return nil, fmt.Errorf("resolve OpenCode executable: %w", err)
	}
	if !filepath.IsAbs(config.PrivateRoot) {
		return nil, fmt.Errorf("OpenCode private root must be absolute")
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	return &Provider{
		executable: resolved, privateRoot: filepath.Clean(config.PrivateRoot),
		environment: append([]string(nil), config.Environment...), httpClient: client,
		credentials: host.ActionCredentialHost{PrivateRoot: filepath.Join(config.PrivateRoot, "action-credentials")},
		agentSocket: config.AgentSocket,
	}, nil
}

func (*Provider) Name() string                                        { return Name }
func (p *Provider) ActionCredentials() ports.ActionCredentialDelivery { return p.credentials }

type evidence struct {
	ExecutionID         string                         `json:"execution_id"`
	NativeID            string                         `json:"native_id,omitempty"`
	Endpoint            string                         `json:"endpoint"`
	Password            string                         `json:"password"`
	StateRoot           string                         `json:"state_root"`
	Process             *host.ProcessIdentity          `json:"process,omitempty"`
	AttemptMark         string                         `json:"attempt_marker"`
	EphemeralState      bool                           `json:"ephemeral_state,omitempty"`
	Access              *ports.ActionCredentialReceipt `json:"access,omitempty"`
	ObservationSequence uint64                         `json:"observation_sequence,omitempty"`
}

type prepared struct {
	provider      *Provider
	request       ports.PreparationRequest
	listener      net.Listener
	endpoint      string
	password      string
	stateRoot     string
	attemptMark   string
	removeOnAbort bool
	description   ports.PreparedDescription
	access        *ports.ActionCredentialReceipt
	mu            sync.Mutex
	released      bool
	aborted       bool
}

type sessionRecord struct {
	ID         string           `json:"id"`
	Directory  string           `json:"directory"`
	ParentID   *string          `json:"parentID,omitempty"`
	Permission []permissionRule `json:"permission"`
}

func (p *Provider) Prepare(ctx context.Context, request ports.PreparationRequest) (ports.PreparedAttempt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.Spec.Harness != Name {
		return nil, fmt.Errorf("OpenCode provider cannot prepare harness %q", request.Spec.Harness)
	}
	if err := validateDirectory(request.Spec.WorkingDirectory); err != nil {
		return nil, err
	}
	if request.Spec.Approval != model.ApprovalSupervised && request.Spec.Approval != model.ApprovalAutomatic {
		return nil, fmt.Errorf("OpenCode provider does not support approval mode %q", request.Spec.Approval)
	}
	if request.Spec.Sandbox != model.SandboxUnconfined {
		return nil, fmt.Errorf("OpenCode requires explicitly selected %q policy; native tool rules do not enforce %q confinement",
			model.SandboxUnconfined, request.Spec.Sandbox)
	}
	var access *ports.ActionCredentialReceipt
	if request.ActionCredential != nil {
		if request.ActionCredential.ExecutionID != request.Spec.ExecutionID {
			return nil, fmt.Errorf("action credential does not match OpenCode execution")
		}
		if !filepath.IsAbs(p.agentSocket) {
			return nil, fmt.Errorf("OpenCode agent API socket must be absolute for credential delivery")
		}
		receipt, deliveryErr := p.credentials.PrepareActionCredential(ctx, *request.ActionCredential)
		if deliveryErr != nil {
			return nil, deliveryErr
		}
		access = &receipt
	}
	stateRoot, removeOnAbort, nativeID, err := p.prepareHistory(request)
	if err != nil {
		if access != nil {
			_ = p.credentials.RemoveActionCredential(context.Background(), *access)
		}
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		if access != nil {
			_ = p.credentials.RemoveActionCredential(context.Background(), *access)
		}
		if removeOnAbort {
			_ = os.RemoveAll(stateRoot)
		}
		return nil, fmt.Errorf("reserve OpenCode loopback endpoint: %w", err)
	}
	endpoint := "http://" + listener.Addr().String()
	password, err := randomPassword()
	if err != nil {
		_ = listener.Close()
		if removeOnAbort {
			_ = os.RemoveAll(stateRoot)
		}
		if access != nil {
			_ = p.credentials.RemoveActionCredential(context.Background(), *access)
		}
		return nil, err
	}
	attemptMark, err := randomPassword()
	if err != nil {
		_ = listener.Close()
		if removeOnAbort {
			_ = os.RemoveAll(stateRoot)
		}
		if access != nil {
			_ = p.credentials.RemoveActionCredential(context.Background(), *access)
		}
		return nil, err
	}
	initial, err := encodeEvidence(evidence{
		ExecutionID: string(request.Spec.ExecutionID), NativeID: nativeID,
		Endpoint: endpoint, Password: password, StateRoot: stateRoot, AttemptMark: attemptMark,
		EphemeralState: removeOnAbort, Access: access,
	})
	if err != nil {
		_ = listener.Close()
		if removeOnAbort {
			_ = os.RemoveAll(stateRoot)
		}
		if access != nil {
			_ = p.credentials.RemoveActionCredential(context.Background(), *access)
		}
		return nil, err
	}
	return &prepared{
		provider: p, request: request, listener: listener, endpoint: endpoint,
		password: password, stateRoot: stateRoot, attemptMark: attemptMark, removeOnAbort: removeOnAbort, access: access,
		description: ports.PreparedDescription{
			ExecutionID: request.Spec.ExecutionID,
			Attempt:     request.Spec.Attempt,
			Topology:    ports.TopologyIndependentServer,
			Requirements: ports.RuntimeRequirements{
				Executable: p.executable, WorkingDirectory: request.Spec.WorkingDirectory,
				PrivateStorage: true,
				Loopback:       &ports.LoopbackRequirement{Protocol: "http"},
				Policy: ports.PolicyRequirements{
					SupportedApproval: []model.ApprovalMode{model.ApprovalSupervised, model.ApprovalAutomatic},
					SupportedSandbox:  []model.SandboxMode{model.SandboxUnconfined},
				},
			},
			// SandboxEnforced means the explicit absence of confinement was
			// preserved, never that OpenCode's permission rules are an OS sandbox.
			EffectivePolicy: ports.EffectivePolicy{
				Approval: request.Spec.Approval, Sandbox: request.Spec.Sandbox,
				ApprovalEnforced: true, SandboxEnforced: true,
			},
			Resources: []ports.ResourceClaim{
				{Kind: ports.ResourceServer, Key: endpoint},
				{Kind: ports.ResourceProcess, Key: stateRoot},
			},
			Evidence: initial, AccessDelivery: access,
		},
	}, nil
}

func (p *Provider) prepareHistory(request ports.PreparationRequest) (stateRoot string, removeOnAbort bool, nativeID string, err error) {
	switch request.Intent {
	case ports.StartFresh:
		if err := os.MkdirAll(p.privateRoot, 0o700); err != nil {
			return "", false, "", err
		}
		if err := os.Chmod(p.privateRoot, 0o700); err != nil {
			return "", false, "", err
		}
		stateRoot = filepath.Join(p.privateRoot, "execution-"+uuid.NewString())
		if err := os.Mkdir(stateRoot, 0o700); err != nil {
			return "", false, "", err
		}
		return stateRoot, true, "", nil
	case ports.StartContinue:
		if request.Continuation == nil || request.Continuation.Namespace != NativeNamespace ||
			!strings.HasPrefix(request.Continuation.Reference, "ses_") {
			return "", false, "", fmt.Errorf("OpenCode continuation requires %s ses_ evidence", NativeNamespace)
		}
		prior, decodeErr := decodeEvidence(request.PriorEvidence)
		if decodeErr != nil {
			return "", false, "", fmt.Errorf("decode OpenCode continuation evidence: %w", decodeErr)
		}
		if prior.StateRoot == "" || !pathWithin(p.privateRoot, prior.StateRoot) {
			return "", false, "", fmt.Errorf("OpenCode continuation state root is outside provider storage")
		}
		info, statErr := os.Stat(prior.StateRoot)
		if statErr != nil || !info.IsDir() {
			return "", false, "", fmt.Errorf("OpenCode continuation state is unavailable")
		}
		return prior.StateRoot, false, request.Continuation.Reference, nil
	default:
		return "", false, "", fmt.Errorf("unsupported start intent %q", request.Intent)
	}
}

func (p *prepared) Describe() ports.PreparedDescription { return p.description }

func (p *prepared) Abort(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.released {
		return fmt.Errorf("OpenCode attempt was released")
	}
	if p.aborted {
		return nil
	}
	p.aborted = true
	err := p.listener.Close()
	if p.removeOnAbort {
		err = errors.Join(err, os.RemoveAll(p.stateRoot))
	}
	if p.access != nil {
		err = errors.Join(err, p.provider.credentials.RemoveActionCredential(context.Background(), *p.access))
	}
	return err
}

func (p *prepared) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ReleaseResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.aborted || p.released {
		return ports.ReleaseResult{}, fmt.Errorf("OpenCode prepared attempt is no longer releasable")
	}
	if permit == nil || permit.ExecutionID() != p.request.Spec.ExecutionID {
		return ports.ReleaseResult{}, fmt.Errorf("release permit does not match execution")
	}
	if err := permit.Consume(ctx); err != nil {
		return ports.ReleaseResult{}, fmt.Errorf("consume release permit: %w", err)
	}
	// OpenCode cannot inherit a listener. Close the reservation at the exact
	// pre-start boundary; any later failure is reconciled as a retained attempt.
	port := p.listener.Addr().(*net.TCPAddr).Port
	if err := p.listener.Close(); err != nil {
		return ports.ReleaseResult{}, fmt.Errorf("release endpoint reservation: %w", err)
	}
	p.released = true
	process, err := host.StartProcess(host.ProcessSpec{
		Executable: p.provider.executable,
		Args:       []string{"serve", "--hostname", "127.0.0.1", "--port", strconv.Itoa(port), "--pure"},
		Directory:  p.request.Spec.WorkingDirectory,
		Env: append(p.provider.runtimeEnvironment(p.stateRoot),
			"OPENCODE_SERVER_USERNAME="+serverUsername,
			"OPENCODE_SERVER_PASSWORD="+p.password,
			attemptMarkerKey+"="+p.attemptMark,
			"TCLAUDE_BACKEND_CREDENTIAL_FILE="+accessResource(p.access),
			"TCLAUDE_BACKEND_SOCKET="+agentSocket(p.access, p.provider.agentSocket)),
	})
	if err != nil {
		return ports.ReleaseResult{}, fmt.Errorf("start OpenCode server: %w", err)
	}
	runtime := &Runtime{
		provider: p.provider, executionID: p.request.Spec.ExecutionID,
		attempt: p.request.Spec.Attempt, observations: p.request.Observations,
		process: process, endpoint: p.endpoint, password: p.password,
		stateRoot: p.stateRoot, cwd: p.request.Spec.WorkingDirectory,
		nativeID: p.descriptionNativeID(), approval: p.request.Spec.Approval,
		sandbox: p.request.Spec.Sandbox, model: p.request.Spec.Model, attemptMark: p.attemptMark, access: p.access,
	}
	currentEvidence, evidenceErr := runtime.providerEvidence()
	if evidenceErr != nil {
		return ports.ReleaseResult{State: ports.ReleaseUncertain, Runtime: runtime}, evidenceErr
	}
	if err := runtime.awaitHealthy(ctx); err != nil {
		return ports.ReleaseResult{State: ports.ReleaseUncertain, Runtime: runtime, Evidence: currentEvidence}, err
	}
	if runtime.nativeID == "" {
		if err := runtime.createSession(ctx); err != nil {
			currentEvidence, _ = runtime.providerEvidence()
			return ports.ReleaseResult{State: ports.ReleaseUncertain, Runtime: runtime, Evidence: currentEvidence}, err
		}
	} else if err := runtime.verifySession(ctx); err != nil {
		return ports.ReleaseResult{State: ports.ReleaseUncertain, Runtime: runtime, Evidence: currentEvidence}, err
	}
	disposition := ports.PrimaryContextInitial
	var prior *model.NativeBinding
	if p.request.Intent == ports.StartContinue {
		disposition = ports.PrimaryContextContinuity
		prior = bindingFor(p.request.Continuation)
	}
	if err := runtime.publishContext(ctx, disposition, prior, nativeBinding(runtime.nativeID), ""); err != nil {
		return ports.ReleaseResult{State: ports.ReleaseUncertain, Runtime: runtime, Evidence: currentEvidence}, err
	}
	currentEvidence, evidenceErr = runtime.providerEvidence()
	if evidenceErr != nil {
		return ports.ReleaseResult{State: ports.ReleaseUncertain, Runtime: runtime}, evidenceErr
	}
	return ports.ReleaseResult{State: ports.ReleaseStarted, Runtime: runtime, Evidence: currentEvidence}, nil
}

func accessResource(access *ports.ActionCredentialReceipt) string {
	if access == nil {
		return ""
	}
	return access.Resource
}

func agentSocket(access *ports.ActionCredentialReceipt, socket string) string {
	if access == nil {
		return ""
	}
	return socket
}

func (p *prepared) descriptionNativeID() string {
	recorded, err := decodeEvidence(p.description.Evidence)
	if err != nil {
		return ""
	}
	return recorded.NativeID
}

func (p *Provider) runtimeEnvironment(stateRoot string) []string {
	return append(append([]string(nil), p.environment...),
		"XDG_DATA_HOME="+filepath.Join(stateRoot, "data"),
		"XDG_CONFIG_HOME="+filepath.Join(stateRoot, "config"),
		"XDG_CACHE_HOME="+filepath.Join(stateRoot, "cache"),
		"XDG_STATE_HOME="+filepath.Join(stateRoot, "state"))
}

func (p *Provider) Recover(ctx context.Context, request ports.RecoveryRequest) (ports.RecoveryResult, error) {
	recorded, err := decodeEvidence(request.Evidence)
	if err != nil {
		return ports.RecoveryResult{}, err
	}
	if recorded.ExecutionID != string(request.ExecutionID) || recorded.AttemptMark == "" ||
		recorded.Password == "" || !pathWithin(p.privateRoot, recorded.StateRoot) {
		return ports.RecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence}, nil
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
	var process *host.Process
	if recorded.Process != nil {
		process, err = host.RecoverProcess(*recorded.Process)
	} else {
		process, err = host.RecoverProcessByEnvironment(attemptMarkerKey, recorded.AttemptMark)
	}
	if errors.Is(err, host.ErrProcessIdentityNotLive) {
		if recorded.EphemeralState {
			_ = os.RemoveAll(recorded.StateRoot)
		}
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
	runtime := &Runtime{
		provider: p, executionID: request.ExecutionID, process: process,
		attempt: request.Attempt, observations: request.Observations,
		endpoint: recorded.Endpoint, password: recorded.Password, stateRoot: recorded.StateRoot,
		cwd: request.Spec.WorkingDirectory, nativeID: recorded.NativeID,
		approval: request.Spec.Approval, sandbox: request.Spec.Sandbox, model: request.Spec.Model,
		attemptMark: recorded.AttemptMark, access: recorded.Access,
		observationSequence: recorded.ObservationSequence,
	}
	var reconcileErr error
	if runtime.nativeID == "" {
		reconcileErr = runtime.reconcileFreshSession(ctx)
	} else {
		reconcileErr = runtime.verifySession(ctx)
	}
	if reconcileErr != nil {
		observation, _ := runtime.Observe(ctx)
		return ports.RecoveryResult{State: ports.RecoveryUnknown, Runtime: runtime,
			Observation: observation, Evidence: request.Evidence}, reconcileErr
	}
	if err := runtime.publishContext(ctx, ports.PrimaryContextContinuity, nativeBinding(runtime.nativeID), nativeBinding(runtime.nativeID), "recovery"); err != nil {
		observation, _ := runtime.Observe(ctx)
		return ports.RecoveryResult{State: ports.RecoveryUnknown, Runtime: runtime, Observation: observation,
			Evidence: request.Evidence, Attempt: request.Attempt}, err
	}
	observation, observeErr := runtime.Observe(ctx)
	if observeErr != nil || observation.Workload == ports.WorkloadUnknown {
		return ports.RecoveryResult{State: ports.RecoveryUnknown, Runtime: runtime, Observation: observation, Evidence: request.Evidence}, observeErr
	}
	currentEvidence, evidenceErr := runtime.providerEvidence()
	if evidenceErr != nil {
		return ports.RecoveryResult{State: ports.RecoveryUnknown, Runtime: runtime, Observation: observation,
			Evidence: request.Evidence}, evidenceErr
	}
	return ports.RecoveryResult{State: ports.RecoveryControlled, Runtime: runtime, Observation: observation, Evidence: currentEvidence,
		Attempt: request.Attempt, AccessProof: accessProof}, nil
}

type Runtime struct {
	provider            *Provider
	executionID         model.ExecutionID
	attempt             model.AttemptGeneration
	observations        ports.PrimaryObservationSink
	process             *host.Process
	endpoint            string
	password            string
	stateRoot           string
	cwd                 string
	approval            model.ApprovalMode
	sandbox             model.SandboxMode
	model               string
	attemptMark         string
	access              *ports.ActionCredentialReceipt
	contextReady        bool
	observationSequence uint64

	mu               sync.Mutex
	nativeID         string
	attachmentActive atomic.Bool
}

func (r *Runtime) ExecutionID() model.ExecutionID { return r.executionID }

func (r *Runtime) Observe(ctx context.Context) (ports.Observation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := ports.Observation{ObservedAt: time.Now(), Context: ports.ContextUnknown,
		AttachmentActive: r.attachmentActive.Load(), NativeConversation: nativeEvidence(r.nativeID)}
	process := r.process.Observe()
	switch {
	case process.Exited:
		result.Workload, result.ExitCode = ports.WorkloadExited, process.ExitCode
	case process.Unknown:
		result.Workload = ports.WorkloadUnknown
	case process.Running:
		result.Workload = ports.WorkloadRunning
		if err := r.health(ctx); err == nil && r.nativeID != "" && r.contextReady {
			result.Context = ports.ContextReady
		} else {
			result.Context = ports.ContextPending
		}
	}
	evidence, err := r.providerEvidenceLocked()
	if err != nil {
		return ports.Observation{}, err
	}
	result.Evidence = evidence
	return result, nil
}

func (r *Runtime) Interact(ctx context.Context, interaction ports.Interaction) (ports.InteractionResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.TrimSpace(interaction.Text) == "" || r.nativeID == "" {
		return ports.InteractionResult{Disposition: ports.EffectRefused}, nil
	}
	body := map[string]any{"parts": []map[string]string{{"type": "text", "text": interaction.Text}}}
	if providerID, modelID, ok := strings.Cut(r.model, "/"); ok && providerID != "" && modelID != "" {
		body["model"] = map[string]string{"providerID": providerID, "modelID": modelID}
	}
	response, err := r.do(ctx, http.MethodPost, "/session/"+url.PathEscape(r.nativeID)+
		"/prompt_async?directory="+url.QueryEscape(r.cwd), body)
	evidence, _ := r.providerEvidenceLocked()
	if err != nil {
		return ports.InteractionResult{Disposition: ports.EffectUnknown, Evidence: evidence}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return ports.InteractionResult{Disposition: ports.EffectRefused, Evidence: evidence},
			fmt.Errorf("OpenCode prompt returned HTTP %d", response.StatusCode)
	}
	return ports.InteractionResult{Disposition: ports.EffectAccepted, Evidence: evidence}, nil
}

func (r *Runtime) Attach(ctx context.Context, request ports.AttachmentRequest) (ports.AttachmentResult, error) {
	if request.Kind != ports.AttachmentTerminal {
		return ports.AttachmentResult{Disposition: ports.EffectUnsupported}, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.health(ctx); err != nil || r.nativeID == "" {
		return ports.AttachmentResult{Disposition: ports.EffectRefused}, err
	}
	cmd := exec.CommandContext(ctx, r.provider.executable, "attach", r.endpoint,
		"--dir", r.cwd, "--session", r.nativeID)
	cmd.Env = host.MergeEnvironment(os.Environ(), append(r.provider.runtimeEnvironment(r.stateRoot),
		"OPENCODE_SERVER_USERNAME="+serverUsername,
		"OPENCODE_SERVER_PASSWORD="+r.password))
	file, err := pty.Start(cmd)
	if err != nil {
		return ports.AttachmentResult{Disposition: ports.EffectRefused}, err
	}
	r.attachmentActive.Store(true)
	attachment := &terminalAttachment{file: file, cmd: cmd, active: &r.attachmentActive}
	evidence, evidenceErr := r.providerEvidenceLocked()
	if evidenceErr != nil {
		_ = attachment.Close()
		return ports.AttachmentResult{}, evidenceErr
	}
	return ports.AttachmentResult{Disposition: ports.EffectAccepted, Attachment: attachment, Evidence: evidence}, nil
}

func (r *Runtime) ChangeContext(ctx context.Context, change ports.ContextChange) (ports.ContextChangeResult, error) {
	if change.Intent != ports.ContextClear && change.Intent != ports.ContextReset {
		return ports.ContextChangeResult{Disposition: ports.EffectUnsupported}, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	prior := nativeBinding(r.nativeID)
	if err := r.createSession(ctx); err != nil {
		evidence, _ := r.providerEvidenceLocked()
		return ports.ContextChangeResult{Disposition: ports.EffectUnknown, Evidence: evidence}, err
	}
	transition := fmt.Sprintf("%s:%d", change.ExpectedConversation, change.ExpectedAssociationRevision)
	if err := r.publishContextLocked(ctx, ports.PrimaryContextReset, prior, nativeBinding(r.nativeID), transition); err != nil {
		evidence, _ := r.providerEvidenceLocked()
		return ports.ContextChangeResult{Disposition: ports.EffectUnknown, Evidence: evidence}, err
	}
	evidence, err := r.providerEvidenceLocked()
	return ports.ContextChangeResult{Disposition: ports.EffectAccepted,
		NativeConversation: nativeEvidence(r.nativeID), Evidence: evidence}, err
}

func (r *Runtime) Stop(ctx context.Context, request ports.StopRequest) (ports.StopResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	acknowledged, exited, err := r.process.Stop(ctx, request.Force)
	if exited && r.access != nil {
		err = errors.Join(err, r.provider.credentials.RemoveActionCredential(ctx, *r.access))
	}
	evidence, evidenceErr := r.providerEvidenceLocked()
	if evidenceErr != nil && err == nil {
		err = evidenceErr
	}
	disposition := ports.EffectAccepted
	if err != nil {
		disposition = ports.EffectUnknown
	}
	return ports.StopResult{Disposition: disposition, Acknowledged: acknowledged, Exited: exited, Evidence: evidence}, err
}

func (r *Runtime) awaitHealthy(ctx context.Context) error {
	deadlineCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		if observation := r.process.Observe(); observation.Exited {
			return fmt.Errorf("OpenCode server exited before readiness")
		}
		if err := r.health(deadlineCtx); err == nil {
			return nil
		}
		select {
		case <-deadlineCtx.Done():
			return fmt.Errorf("OpenCode server readiness is uncertain: %w", deadlineCtx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (r *Runtime) health(ctx context.Context) error {
	if !r.process.Observe().Running {
		return fmt.Errorf("OpenCode process identity is not running")
	}
	response, err := r.do(ctx, http.MethodGet, "/global/health", nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("OpenCode health returned HTTP %d", response.StatusCode)
	}
	var body struct {
		Healthy bool `json:"healthy"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&body); err != nil || !body.Healthy {
		return fmt.Errorf("OpenCode health response is invalid")
	}
	return nil
}

func (r *Runtime) createSession(ctx context.Context) error {
	expected := permissionRules(r.approval, r.sandbox)
	body := map[string]any{"permission": expected}
	response, err := r.do(ctx, http.MethodPost, "/session?directory="+url.QueryEscape(r.cwd), body)
	if err != nil {
		return fmt.Errorf("create OpenCode session: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("create OpenCode session returned HTTP %d", response.StatusCode)
	}
	var created sessionRecord
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&created); err != nil {
		return fmt.Errorf("decode OpenCode session: %w", err)
	}
	if !strings.HasPrefix(created.ID, "ses_") || created.ParentID != nil ||
		filepath.Clean(created.Directory) != filepath.Clean(r.cwd) {
		return fmt.Errorf("OpenCode returned invalid native session %q", created.ID)
	}
	if !permissionHasSuffix(created.Permission, expected) {
		return fmt.Errorf("OpenCode session did not retain the requested permission policy")
	}
	r.nativeID = created.ID
	return nil
}

// reconcileFreshSession resolves the crash window between starting the private
// server and persisting its newly-created native session id. A fresh attempt
// owns an isolated state root, so zero sessions means creation never completed
// and one top-level session is the exact result to retain. Nested sessions are
// not primary evidence; multiple top-level sessions are ambiguous.
func (r *Runtime) reconcileFreshSession(ctx context.Context) error {
	if err := r.health(ctx); err != nil {
		return err
	}
	response, err := r.do(ctx, http.MethodGet, "/session?directory="+url.QueryEscape(r.cwd), nil)
	if err != nil {
		return fmt.Errorf("list private OpenCode sessions: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("list private OpenCode sessions returned HTTP %d", response.StatusCode)
	}
	var sessions []sessionRecord
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&sessions); err != nil {
		return fmt.Errorf("decode private OpenCode sessions: %w", err)
	}
	primary := make([]sessionRecord, 0, len(sessions))
	for _, session := range sessions {
		if session.ParentID == nil && filepath.Clean(session.Directory) == filepath.Clean(r.cwd) {
			primary = append(primary, session)
		}
	}
	switch len(primary) {
	case 0:
		return r.createSession(ctx)
	case 1:
		if !strings.HasPrefix(primary[0].ID, "ses_") {
			return fmt.Errorf("OpenCode returned invalid private session %q", primary[0].ID)
		}
		r.nativeID = primary[0].ID
		return r.verifySession(ctx)
	default:
		return fmt.Errorf("private OpenCode attempt has %d primary sessions; native identity is ambiguous", len(primary))
	}
}

func (r *Runtime) verifySession(ctx context.Context) error {
	expected := permissionRules(r.approval, r.sandbox)
	response, err := r.do(ctx, http.MethodGet, "/session/"+url.PathEscape(r.nativeID)+
		"?directory="+url.QueryEscape(r.cwd), nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("OpenCode continuation returned HTTP %d", response.StatusCode)
	}
	var current sessionRecord
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&current); err != nil {
		return fmt.Errorf("decode OpenCode continuation: %w", err)
	}
	if current.ID != r.nativeID || current.ParentID != nil || filepath.Clean(current.Directory) != filepath.Clean(r.cwd) {
		return fmt.Errorf("OpenCode session is not the exact primary context")
	}
	if permissionHasSuffix(current.Permission, expected) {
		return nil
	}
	updated, err := r.do(ctx, http.MethodPatch, "/session/"+url.PathEscape(r.nativeID)+
		"?directory="+url.QueryEscape(r.cwd), map[string]any{"permission": expected})
	if err != nil {
		return fmt.Errorf("apply OpenCode continuation permission: %w", err)
	}
	defer updated.Body.Close()
	if updated.StatusCode != http.StatusOK {
		return fmt.Errorf("apply OpenCode continuation permission returned HTTP %d", updated.StatusCode)
	}
	var result sessionRecord
	if err := json.NewDecoder(io.LimitReader(updated.Body, 1<<20)).Decode(&result); err != nil {
		return fmt.Errorf("decode applied OpenCode continuation permission: %w", err)
	}
	if result.ID != r.nativeID || result.ParentID != nil || filepath.Clean(result.Directory) != filepath.Clean(r.cwd) ||
		!permissionHasSuffix(result.Permission, expected) {
		return fmt.Errorf("OpenCode continuation did not retain the requested permission policy")
	}
	return nil
}

func (r *Runtime) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	if !r.process.Observe().Running {
		return nil, fmt.Errorf("OpenCode process identity is not running")
	}
	parsed, err := url.Parse(r.endpoint)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" {
		return nil, fmt.Errorf("OpenCode endpoint is not exact loopback HTTP")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("OpenCode endpoint port is invalid")
	}
	owned, err := r.process.OwnsLoopbackPort(port)
	if err != nil {
		return nil, fmt.Errorf("prove OpenCode endpoint ownership: %w", err)
	}
	if !owned {
		return nil, fmt.Errorf("OpenCode process does not own its recorded endpoint")
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, r.endpoint+path, reader)
	if err != nil {
		return nil, err
	}
	request.SetBasicAuth(serverUsername, r.password)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return r.provider.httpClient.Do(request)
}

func (r *Runtime) providerEvidence() (model.ProviderEvidence, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.providerEvidenceLocked()
}

func (r *Runtime) providerEvidenceLocked() (model.ProviderEvidence, error) {
	identity := r.process.Identity()
	return encodeEvidence(evidence{
		ExecutionID: string(r.executionID), NativeID: r.nativeID, Endpoint: r.endpoint,
		Password: r.password, StateRoot: r.stateRoot, Process: &identity, AttemptMark: r.attemptMark,
		Access: r.access, ObservationSequence: r.observationSequence,
	})
}

func (r *Runtime) publishContext(ctx context.Context, disposition ports.PrimaryContextDisposition, prior, next *model.NativeBinding, transition string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.publishContextLocked(ctx, disposition, prior, next, transition)
}

func (r *Runtime) publishContextLocked(ctx context.Context, disposition ports.PrimaryContextDisposition, prior, next *model.NativeBinding, transition string) error {
	if r.observations == nil {
		return nil
	}
	r.observationSequence++
	identity := r.process.Identity()
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s\x00%s", r.executionID, r.attempt, r.attemptMark, identity.StartToken)))
	evidence := ports.PrimaryContextEvidence{
		ExecutionID: r.executionID, Attempt: r.attempt, Provider: Name,
		PrimaryCorrelation: fmt.Sprintf("server:%x", digest[:16]), Disposition: disposition,
		PriorBinding: prior, NextBinding: next, TransitionCorrelation: transition,
		ProviderOrder: fmt.Sprintf("%020d:%s", r.observationSequence, r.nativeID), ObservedAt: time.Now().UTC(),
	}
	if err := r.observations.ObservePrimaryContext(ctx, evidence); err != nil {
		return err
	}
	r.contextReady = disposition != ports.PrimaryContextUnresolved
	return nil
}

type permissionRule struct {
	Permission string `json:"permission"`
	Pattern    string `json:"pattern"`
	Action     string `json:"action"`
}

func permissionRules(approval model.ApprovalMode, sandbox model.SandboxMode) []permissionRule {
	action := "ask"
	if approval == model.ApprovalAutomatic {
		action = "allow"
	}
	rules := []permissionRule{{Permission: "*", Pattern: "*", Action: "deny"},
		{Permission: "read", Pattern: "*", Action: "allow"}}
	if sandbox == model.SandboxUnconfined {
		for _, permission := range []string{"edit", "external_directory", "bash", "glob", "grep", "lsp", "task", "skill", "webfetch", "websearch"} {
			rules = append(rules, permissionRule{Permission: permission, Pattern: "*", Action: action})
		}
	}
	return rules
}

func permissionHasSuffix(current, expected []permissionRule) bool {
	if len(expected) == 0 || len(current) < len(expected) {
		return false
	}
	offset := len(current) - len(expected)
	for index := range expected {
		if current[offset+index] != expected[index] {
			return false
		}
	}
	return true
}

type terminalAttachment struct {
	file   *os.File
	cmd    *exec.Cmd
	active *atomic.Bool
	once   sync.Once
}

func (*terminalAttachment) Kind() ports.AttachmentKind        { return ports.AttachmentTerminal }
func (a *terminalAttachment) Read(value []byte) (int, error)  { return a.file.Read(value) }
func (a *terminalAttachment) Write(value []byte) (int, error) { return a.file.Write(value) }
func (a *terminalAttachment) Close() error {
	var err error
	a.once.Do(func() {
		err = a.file.Close()
		if a.cmd.Process != nil {
			_ = a.cmd.Process.Kill()
		}
		_ = a.cmd.Wait()
		a.active.Store(false)
	})
	return err
}

func randomPassword() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
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
		return evidence{}, fmt.Errorf("unsupported OpenCode evidence %q version %d", envelope.Provider, envelope.Version)
	}
	var value evidence
	if err := json.Unmarshal(envelope.Payload, &value); err != nil {
		return evidence{}, fmt.Errorf("decode OpenCode evidence: %w", err)
	}
	return value, nil
}

func nativeEvidence(id string) *model.NativeConversationEvidence {
	if id == "" {
		return nil
	}
	return &model.NativeConversationEvidence{Namespace: NativeNamespace, Reference: id, ObservedAt: time.Now()}
}

func nativeBinding(id string) *model.NativeBinding {
	if id == "" {
		return nil
	}
	return &model.NativeBinding{Namespace: NativeNamespace, Reference: id}
}

func bindingFor(evidence *model.NativeConversationEvidence) *model.NativeBinding {
	if evidence == nil {
		return nil
	}
	return &model.NativeBinding{Namespace: evidence.Namespace, Reference: evidence.Reference}
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

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && relative != ".." && relative != "." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

var _ ports.Provider = (*Provider)(nil)
var _ ports.ActionCredentialProvider = (*Provider)(nil)
var _ ports.PreparedAttempt = (*prepared)(nil)
var _ ports.Runtime = (*Runtime)(nil)
