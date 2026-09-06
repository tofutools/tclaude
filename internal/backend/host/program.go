//go:build linux || darwin

package host

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

const (
	programEvidenceOwner         = "host.program"
	programEvidenceVersion       = uint32(1)
	programAttemptEnvironmentKey = "TCLAUDE_PROGRAM_ATTEMPT"
	maxProgramArguments          = 256
	maxProgramArgumentBytes      = 32 << 10
	maxProgramArgumentsBytes     = 256 << 10
	maxProgramEnvironment        = 128
	maxProgramEnvironmentBytes   = 256 << 10
	maxProgramInputBytes         = 1 << 20
	maxProgramOutputBytes        = 1 << 20
)

const (
	programInputFile           = "input.json"
	programStdoutFile          = "stdout"
	programStderrFile          = "stderr"
	programStdoutTruncatedFile = "stdout.truncated"
	programStderrTruncatedFile = "stderr.truncated"
)

// ProgramProcessHost owns non-terminal program processes and their bounded,
// private output resources. Policy, workspace selection and retries remain in
// the application layer.
type ProgramProcessHost struct {
	PrivateRoot string
}

type programEvidence struct {
	ExecutionID      model.ExecutionID              `json:"execution_id"`
	Attempt          model.AttemptGeneration        `json:"attempt"`
	ProfileID        model.ProgramProfileID         `json:"profile_id"`
	ProfileRevision  model.ProgramProfileRevisionID `json:"profile_revision"`
	ProfileHash      string                         `json:"profile_hash"`
	WorkspaceID      model.WorkspaceID              `json:"workspace_id"`
	WorkspaceUseID   model.WorkspaceUseID           `json:"workspace_use_id"`
	WorkingDirectory string                         `json:"working_directory"`
	ResourceRoot     string                         `json:"resource_root"`
	AttemptMarker    string                         `json:"attempt_marker"`
	Executable       string                         `json:"executable"`
	ArgumentsHash    string                         `json:"arguments_hash"`
	OutputLimit      int64                          `json:"output_limit"`
	Deadline         time.Time                      `json:"deadline"`
	Process          *ProcessIdentity               `json:"process,omitempty"`
}

type preparedProgram struct {
	host        ProgramProcessHost
	request     ports.ProgramPreparationRequest
	description ports.ProgramPreparedDescription
	evidence    programEvidence
	executable  string
	arguments   []string
	environment []string

	mu       sync.Mutex
	released bool
	aborted  bool
}

type programRuntime struct {
	host     ProgramProcessHost
	process  *Process
	evidence programEvidence
	envelope model.ProviderEvidence

	stdout *boundedTailFile
	stderr *boundedTailFile
}

func (h ProgramProcessHost) PrepareProgram(ctx context.Context, request ports.ProgramPreparationRequest) (ports.PreparedProgram, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	executable, arguments, environment, deadline, err := validateProgramPreparation(request)
	if err != nil {
		return nil, err
	}
	root, marker, err := h.reserveProgramResource()
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = os.RemoveAll(root) }
	if err := WriteProtectedFile(filepath.Join(root, programInputFile), request.Input); err != nil {
		cleanup()
		return nil, err
	}
	if err := WriteProtectedFile(filepath.Join(root, programStdoutFile), nil); err != nil {
		cleanup()
		return nil, err
	}
	if err := WriteProtectedFile(filepath.Join(root, programStderrFile), nil); err != nil {
		cleanup()
		return nil, err
	}
	value := programEvidence{
		ExecutionID: request.Execution.ID, Attempt: request.Execution.Attempt,
		ProfileID: request.Profile.ProfileID, ProfileRevision: request.Profile.ID, ProfileHash: request.Profile.ContentHash,
		WorkspaceID: request.Workspace.ID, WorkspaceUseID: request.WorkspaceUse.ID,
		WorkingDirectory: filepath.Clean(request.WorkingDirectory), ResourceRoot: root, AttemptMarker: marker,
		Executable: executable, ArgumentsHash: hashArguments(arguments), OutputLimit: request.Profile.OutputLimitBytes,
		Deadline: deadline,
	}
	envelope, err := encodeProgramEvidence(value)
	if err != nil {
		cleanup()
		return nil, err
	}
	description := ports.ProgramPreparedDescription{
		ExecutionID: request.Execution.ID, Attempt: request.Execution.Attempt,
		Requirements: ports.RuntimeRequirements{
			Executable: executable, WorkingDirectory: request.WorkingDirectory, PrivateStorage: true,
			Policy: ports.PolicyRequirements{SupportedSandbox: []model.SandboxMode{model.SandboxUnconfined}},
		},
		// Enforced means the explicitly requested absence of confinement is
		// preserved; it never presents a process as OS-confined.
		EffectivePolicy: ports.ProgramEffectivePolicy{Sandbox: request.Profile.Sandbox, Enforced: true},
		Resources: []ports.ResourceClaim{
			{Kind: ports.ResourceProcess, Key: "program:" + marker},
		},
		Evidence: envelope,
	}
	return &preparedProgram{
		host: h, request: request, description: description, evidence: value,
		executable: executable, arguments: arguments, environment: environment,
	}, nil
}

func (p *preparedProgram) Describe() ports.ProgramPreparedDescription { return p.description }

func (p *preparedProgram) Abort(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.released {
		return fmt.Errorf("program attempt was released")
	}
	if p.aborted {
		return nil
	}
	p.aborted = true
	return removeProgramResource(p.host.PrivateRoot, p.evidence.ResourceRoot)
}

func (p *preparedProgram) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ProgramReleaseResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.aborted || p.released {
		return ports.ProgramReleaseResult{}, fmt.Errorf("program attempt is no longer releasable")
	}
	if permit == nil || permit.ExecutionID() != p.request.Execution.ID {
		return ports.ProgramReleaseResult{}, fmt.Errorf("release permit does not match program execution")
	}
	input, err := ReadProtectedFile(filepath.Join(p.evidence.ResourceRoot, programInputFile), maxProgramInputBytes)
	if err != nil {
		_ = removeProgramResource(p.host.PrivateRoot, p.evidence.ResourceRoot)
		return ports.ProgramReleaseResult{}, err
	}
	stdout, err := openBoundedTailFile(p.evidence.ResourceRoot, programStdoutFile, programStdoutTruncatedFile, p.evidence.OutputLimit)
	if err != nil {
		_ = removeProgramResource(p.host.PrivateRoot, p.evidence.ResourceRoot)
		return ports.ProgramReleaseResult{}, err
	}
	stderr, err := openBoundedTailFile(p.evidence.ResourceRoot, programStderrFile, programStderrTruncatedFile, p.evidence.OutputLimit)
	if err != nil {
		_ = removeProgramResource(p.host.PrivateRoot, p.evidence.ResourceRoot)
		return ports.ProgramReleaseResult{}, err
	}
	if err := permit.Consume(ctx); err != nil {
		return ports.ProgramReleaseResult{}, fmt.Errorf("consume program release permit: %w", err)
	}
	p.released = true
	process, err := StartProcess(ProcessSpec{
		Executable: p.executable, Args: p.arguments, Directory: p.evidence.WorkingDirectory,
		Env:              append(append([]string(nil), p.environment...), programAttemptEnvironmentKey+"="+p.evidence.AttemptMarker),
		ExactEnvironment: true, Stdin: bytes.NewReader(input), Stdout: stdout, Stderr: stderr,
	})
	if err != nil {
		_ = removeProgramResource(p.host.PrivateRoot, p.evidence.ResourceRoot)
		return ports.ProgramReleaseResult{}, err
	}
	identity := process.Identity()
	p.evidence.Process = &identity
	envelope, encodeErr := encodeProgramEvidence(p.evidence)
	runtime := &programRuntime{host: p.host, process: process, evidence: p.evidence, envelope: envelope, stdout: stdout, stderr: stderr}
	runtime.enforceDeadline()
	if encodeErr != nil {
		return ports.ProgramReleaseResult{State: ports.ReleaseUncertain, Runtime: runtime}, encodeErr
	}
	return ports.ProgramReleaseResult{State: ports.ReleaseStarted, Runtime: runtime, Evidence: envelope}, nil
}

func (h ProgramProcessHost) RecoverProgram(ctx context.Context, request ports.ProgramRecoveryRequest) (ports.ProgramRecoveryResult, error) {
	if err := ctx.Err(); err != nil {
		return ports.ProgramRecoveryResult{}, err
	}
	value, err := decodeProgramEvidence(request.Evidence)
	if err != nil {
		return ports.ProgramRecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence}, err
	}
	if err := validateProgramRecovery(h.PrivateRoot, request, value); err != nil {
		return ports.ProgramRecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence}, err
	}
	stdout, err := openBoundedTailFile(value.ResourceRoot, programStdoutFile, programStdoutTruncatedFile, value.OutputLimit)
	if err != nil {
		return ports.ProgramRecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence}, err
	}
	stderr, err := openBoundedTailFile(value.ResourceRoot, programStderrFile, programStderrTruncatedFile, value.OutputLimit)
	if err != nil {
		return ports.ProgramRecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence}, err
	}
	var process *Process
	if value.Process != nil {
		process, err = RecoverProcess(*value.Process)
	} else {
		process, err = RecoverProcessByEnvironment(programAttemptEnvironmentKey, value.AttemptMarker)
		if err == nil {
			identity := process.Identity()
			value.Process = &identity
		}
	}
	if errors.Is(err, ErrProcessIdentityNotLive) {
		runtime := &programRuntime{host: h, evidence: value, envelope: request.Evidence, stdout: stdout, stderr: stderr}
		observation, observeErr := runtime.ObserveProgram(ctx)
		return ports.ProgramRecoveryResult{State: ports.RecoveryExited, Runtime: runtime, Observation: observation, Evidence: request.Evidence}, observeErr
	}
	if err != nil {
		return ports.ProgramRecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence}, err
	}
	envelope, err := encodeProgramEvidence(value)
	if err != nil {
		return ports.ProgramRecoveryResult{State: ports.RecoveryUnknown, Evidence: request.Evidence}, err
	}
	runtime := &programRuntime{host: h, process: process, evidence: value, envelope: envelope, stdout: stdout, stderr: stderr}
	runtime.enforceDeadline()
	observation, observeErr := runtime.ObserveProgram(ctx)
	state := ports.RecoveryControlled
	switch observation.Workload {
	case ports.WorkloadExited:
		state = ports.RecoveryExited
	case ports.WorkloadUnknown:
		state = ports.RecoveryUnknown
	}
	return ports.ProgramRecoveryResult{State: state, Runtime: runtime, Observation: observation, Evidence: envelope}, observeErr
}

func (r *programRuntime) ExecutionID() model.ExecutionID { return r.evidence.ExecutionID }

func (r *programRuntime) ObserveProgram(context.Context) (ports.ProgramObservation, error) {
	stdout, stdoutTruncated, stdoutErr := r.stdout.snapshot()
	stderr, stderrTruncated, stderrErr := r.stderr.snapshot()
	result := ports.ProgramObservation{
		ObservedAt: time.Now().UTC(), Workload: ports.WorkloadExited, Evidence: r.envelope,
		Stdout: ports.ProgramOutput{Data: stdout, MediaType: "application/octet-stream", Truncated: stdoutTruncated},
		Stderr: ports.ProgramOutput{Data: stderr, MediaType: "application/octet-stream", Truncated: stderrTruncated},
	}
	if r.process != nil {
		observed := r.process.Observe()
		switch {
		case observed.Running:
			result.Workload = ports.WorkloadRunning
		case observed.Exited:
			result.Workload, result.ExitCode = ports.WorkloadExited, observed.ExitCode
		case observed.Unknown:
			result.Workload = ports.WorkloadUnknown
		}
	}
	return result, errors.Join(stdoutErr, stderrErr)
}

func (r *programRuntime) StopProgram(ctx context.Context, request ports.StopRequest) (ports.StopResult, error) {
	if r.process == nil {
		return ports.StopResult{Disposition: ports.EffectAccepted, Exited: true, Evidence: r.envelope}, nil
	}
	acknowledged, exited, err := r.process.Stop(ctx, request.Force)
	disposition := ports.EffectAccepted
	if err != nil {
		disposition = ports.EffectUnknown
	}
	return ports.StopResult{Disposition: disposition, Acknowledged: acknowledged, Exited: exited, Evidence: r.envelope}, err
}

func (r *programRuntime) ReleaseProgramResources(_ context.Context, evidence model.ProviderEvidence) error {
	if evidence.Provider != r.envelope.Provider || evidence.Version != r.envelope.Version || !bytes.Equal(evidence.Payload, r.envelope.Payload) {
		return fmt.Errorf("program cleanup evidence does not match runtime")
	}
	if r.process != nil && r.process.Observe().Running {
		return fmt.Errorf("cannot release resources for a running program")
	}
	return removeProgramResource(r.host.PrivateRoot, r.evidence.ResourceRoot)
}

func (r *programRuntime) enforceDeadline() {
	if r.process == nil || r.evidence.Deadline.IsZero() {
		return
	}
	deadline := r.evidence.Deadline
	process := r.process
	go func() {
		delay := time.Until(deadline)
		if delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			<-timer.C
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _, _ = process.Stop(ctx, true)
	}()
}

func validateProgramPreparation(request ports.ProgramPreparationRequest) (string, []string, []string, time.Time, error) {
	if err := request.Execution.ID.Validate(); err != nil {
		return "", nil, nil, time.Time{}, err
	}
	if request.Execution.Workload != model.ExecutionWorkloadProgram || request.Execution.Attempt == 0 || request.Execution.ConversationID != "" {
		return "", nil, nil, time.Time{}, fmt.Errorf("program execution identity is invalid")
	}
	if err := request.Profile.ID.Validate(); err != nil {
		return "", nil, nil, time.Time{}, err
	}
	if err := request.Profile.ProfileID.Validate(); err != nil {
		return "", nil, nil, time.Time{}, err
	}
	if strings.TrimSpace(request.Profile.ContentHash) == "" {
		return "", nil, nil, time.Time{}, fmt.Errorf("program profile content hash is required")
	}
	if request.Profile.Sandbox != model.SandboxUnconfined {
		return "", nil, nil, time.Time{}, fmt.Errorf("program host does not enforce sandbox mode %q", request.Profile.Sandbox)
	}
	if request.Profile.OutputLimitBytes <= 0 || request.Profile.OutputLimitBytes > maxProgramOutputBytes {
		return "", nil, nil, time.Time{}, fmt.Errorf("program output limit must be between 1 and %d bytes", maxProgramOutputBytes)
	}
	if len(request.Input) > maxProgramInputBytes {
		return "", nil, nil, time.Time{}, fmt.Errorf("program input exceeds %d bytes", maxProgramInputBytes)
	}
	if len(request.Input) > 0 && !json.Valid(request.Input) {
		return "", nil, nil, time.Time{}, fmt.Errorf("program input must be valid JSON")
	}
	if request.Profile.Timeout < 0 {
		return "", nil, nil, time.Time{}, fmt.Errorf("program timeout cannot be negative")
	}
	if err := validateProgramWorkspace(request.Execution, request.Workspace, request.WorkspaceUse, request.WorkingDirectory); err != nil {
		return "", nil, nil, time.Time{}, err
	}
	executable, err := exec.LookPath(request.Profile.Executable)
	if err != nil {
		return "", nil, nil, time.Time{}, fmt.Errorf("resolve program executable: %w", err)
	}
	arguments := append(append([]string(nil), request.Profile.ArgumentPrefix...), request.Arguments...)
	if err := validateProgramArguments(arguments); err != nil {
		return "", nil, nil, time.Time{}, err
	}
	environment, err := validateProgramEnvironment(request.Profile.Environment)
	if err != nil {
		return "", nil, nil, time.Time{}, err
	}
	deadline := request.Deadline
	if request.Profile.Timeout > 0 {
		profileDeadline := time.Now().UTC().Add(request.Profile.Timeout)
		if deadline.IsZero() || profileDeadline.Before(deadline) {
			deadline = profileDeadline
		}
	}
	if !deadline.IsZero() && !deadline.After(time.Now()) {
		return "", nil, nil, time.Time{}, fmt.Errorf("program deadline has expired")
	}
	return executable, arguments, environment, deadline, nil
}

func validateProgramWorkspace(execution model.Execution, workspace model.Workspace, use model.WorkspaceUse, directory string) error {
	if err := workspace.ID.Validate(); err != nil {
		return err
	}
	if workspace.State != model.WorkspaceAvailable || !filepath.IsAbs(workspace.Observation.ActualPath) {
		return fmt.Errorf("program workspace is unavailable")
	}
	if err := use.ID.Validate(); err != nil {
		return err
	}
	if use.WorkspaceID != workspace.ID || use.ExecutionID != execution.ID || use.ReleasedAt != nil {
		return fmt.Errorf("program workspace use does not match execution")
	}
	if !filepath.IsAbs(directory) {
		return fmt.Errorf("program working directory must be absolute")
	}
	root, err := filepath.EvalSymlinks(workspace.Observation.ActualPath)
	if err != nil {
		return fmt.Errorf("resolve program workspace: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return fmt.Errorf("resolve program working directory: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() || (filepath.Clean(root) != filepath.Clean(resolved) && !pathWithin(root, resolved)) {
		return fmt.Errorf("program working directory is outside the supplied workspace")
	}
	return nil
}

func validateProgramArguments(arguments []string) error {
	if len(arguments) > maxProgramArguments {
		return fmt.Errorf("program argv exceeds %d arguments", maxProgramArguments)
	}
	total := 0
	for _, argument := range arguments {
		if strings.ContainsRune(argument, 0) || len(argument) > maxProgramArgumentBytes {
			return fmt.Errorf("program argument is invalid or exceeds %d bytes", maxProgramArgumentBytes)
		}
		total += len(argument)
	}
	if total > maxProgramArgumentsBytes {
		return fmt.Errorf("program argv exceeds %d bytes", maxProgramArgumentsBytes)
	}
	return nil
}

func validateProgramEnvironment(values map[string]string) ([]string, error) {
	if len(values) > maxProgramEnvironment {
		return nil, fmt.Errorf("program environment exceeds %d entries", maxProgramEnvironment)
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	total := 0
	for _, key := range keys {
		value := values[key]
		if key == "" || key == programAttemptEnvironmentKey || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("program environment contains invalid key %q", key)
		}
		total += len(key) + len(value) + 1
		if total > maxProgramEnvironmentBytes {
			return nil, fmt.Errorf("program environment exceeds %d bytes", maxProgramEnvironmentBytes)
		}
		result = append(result, key+"="+value)
	}
	return result, nil
}

func validateProgramRecovery(privateRoot string, request ports.ProgramRecoveryRequest, value programEvidence) error {
	if request.Execution.Workload != model.ExecutionWorkloadProgram || request.Execution.Attempt == 0 || request.Execution.ConversationID != "" ||
		request.Profile.Sandbox != model.SandboxUnconfined || request.Profile.OutputLimitBytes != value.OutputLimit {
		return fmt.Errorf("program recovery request is invalid")
	}
	if value.ExecutionID != request.Execution.ID || value.Attempt != request.Execution.Attempt ||
		value.ProfileID != request.Profile.ProfileID || value.ProfileRevision != request.Profile.ID ||
		value.ProfileHash != request.Profile.ContentHash || value.WorkspaceID != request.Workspace.ID ||
		value.WorkspaceUseID != request.WorkspaceUse.ID || filepath.Clean(value.WorkingDirectory) != filepath.Clean(request.WorkingDirectory) {
		return fmt.Errorf("program recovery evidence does not match exact execution resources")
	}
	if err := validateProgramWorkspace(request.Execution, request.Workspace, request.WorkspaceUse, request.WorkingDirectory); err != nil {
		return err
	}
	if value.OutputLimit <= 0 || value.OutputLimit > maxProgramOutputBytes || value.AttemptMarker == "" ||
		!validProgramResource(privateRoot, value.ResourceRoot) {
		return fmt.Errorf("program recovery evidence contains invalid private resources")
	}
	return nil
}

func (h ProgramProcessHost) reserveProgramResource() (string, string, error) {
	root := filepath.Clean(h.PrivateRoot)
	if root == "." || !filepath.IsAbs(root) {
		return "", "", fmt.Errorf("program private root must be absolute")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", "", err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return "", "", err
	}
	for range 8 {
		markerBytes := make([]byte, 24)
		if _, err := rand.Read(markerBytes); err != nil {
			return "", "", err
		}
		marker := hex.EncodeToString(markerBytes)
		directory := filepath.Join(root, "attempt-"+marker)
		if err := os.Mkdir(directory, 0o700); err == nil {
			return directory, marker, nil
		} else if !errors.Is(err, os.ErrExist) {
			return "", "", err
		}
	}
	return "", "", fmt.Errorf("reserve unique program resource")
}

func removeProgramResource(privateRoot, resourceRoot string) error {
	if !validProgramResource(privateRoot, resourceRoot) {
		return fmt.Errorf("program resource is outside private storage")
	}
	err := os.RemoveAll(resourceRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func validProgramResource(privateRoot, resourceRoot string) bool {
	root := filepath.Clean(privateRoot)
	resource := filepath.Clean(resourceRoot)
	return root != "." && filepath.IsAbs(root) && filepath.IsAbs(resource) && filepath.Dir(resource) == root &&
		strings.HasPrefix(filepath.Base(resource), "attempt-")
}

func hashArguments(arguments []string) string {
	hash := sha256.New()
	for _, argument := range arguments {
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(argument))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func encodeProgramEvidence(value programEvidence) (model.ProviderEvidence, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return model.ProviderEvidence{}, err
	}
	return model.NewProviderEvidence(programEvidenceOwner, programEvidenceVersion, payload)
}

func decodeProgramEvidence(envelope model.ProviderEvidence) (programEvidence, error) {
	if envelope.Provider != programEvidenceOwner || envelope.Version != programEvidenceVersion || len(envelope.Payload) == 0 {
		return programEvidence{}, fmt.Errorf("unsupported program evidence")
	}
	var value programEvidence
	if err := json.Unmarshal(envelope.Payload, &value); err != nil {
		return programEvidence{}, fmt.Errorf("decode program evidence: %w", err)
	}
	return value, nil
}

type boundedTailFile struct {
	path       string
	markerPath string
	limit      int64

	mu        sync.Mutex
	data      []byte
	truncated bool
}

func openBoundedTailFile(root, name, marker string, limit int64) (*boundedTailFile, error) {
	path := filepath.Join(root, name)
	data, err := ReadProtectedFile(path, limit)
	if err != nil {
		return nil, err
	}
	_, markerErr := os.Lstat(filepath.Join(root, marker))
	if markerErr != nil && !errors.Is(markerErr, os.ErrNotExist) {
		return nil, markerErr
	}
	return &boundedTailFile{path: path, markerPath: filepath.Join(root, marker), limit: limit, data: data, truncated: markerErr == nil}, nil
}

func (w *boundedTailFile) Write(value []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	count := len(value)
	if int64(len(value)) >= w.limit {
		w.data = append(w.data[:0], value[len(value)-int(w.limit):]...)
		w.truncated = true
	} else {
		overflow := int64(len(w.data)+len(value)) - w.limit
		if overflow > 0 {
			w.data = append(w.data[:0], w.data[int(overflow):]...)
			w.truncated = true
		}
		w.data = append(w.data, value...)
	}
	if err := WriteProtectedFile(w.path, w.data); err != nil {
		return 0, err
	}
	if w.truncated {
		if err := WriteProtectedFile(w.markerPath, []byte("truncated\n")); err != nil {
			return 0, err
		}
	}
	return count, nil
}

func (w *boundedTailFile) snapshot() ([]byte, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	data, err := ReadProtectedFile(w.path, w.limit)
	return data, w.truncated, err
}

var _ ports.ProgramHost = ProgramProcessHost{}
var _ ports.PreparedProgram = (*preparedProgram)(nil)
var _ ports.ProgramRuntime = (*programRuntime)(nil)
