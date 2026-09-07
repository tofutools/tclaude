//go:build linux || darwin

package host

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func TestProgramHostRunsBoundedArgvWithExactEnvironmentAndDurableOutput(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TCLAUDE_PROGRAM_AMBIENT_SECRET", "must-not-leak")
	host := ProgramProcessHost{PrivateRoot: filepath.Join(root, "private")}
	request := programRequest(root, "output", 64)
	prepared, err := host.PrepareProgram(context.Background(), request)
	require.NoError(t, err)
	description := prepared.Describe()
	require.Equal(t, request.Execution.ID, description.ExecutionID)
	require.Equal(t, model.SandboxUnconfined, description.EffectivePolicy.Sandbox)
	require.True(t, description.EffectivePolicy.Enforced)
	require.Len(t, description.Resources, 1)

	permit := &programTestPermit{execution: request.Execution.ID, operation: "operation_program"}
	released, err := prepared.Release(context.Background(), permit)
	require.NoError(t, err)
	require.Equal(t, int32(1), permit.consumed.Load())
	require.Equal(t, ports.ReleaseStarted, released.State)

	var observation ports.ProgramObservation
	require.Eventually(t, func() bool {
		observation, err = released.Runtime.ObserveProgram(context.Background())
		return err == nil && observation.Workload == ports.WorkloadExited
	}, 2*time.Second, 10*time.Millisecond)
	require.NotNil(t, observation.ExitCode)
	require.Equal(t, 7, *observation.ExitCode)
	require.True(t, observation.Stdout.Truncated)
	require.True(t, observation.Stderr.Truncated)
	require.Len(t, observation.Stdout.Data, 64)
	require.Len(t, observation.Stderr.Data, 64)
	require.Equal(t, "ambient=\n"+strings.Repeat("o", 55), string(observation.Stdout.Data))
	require.Equal(t, strings.Repeat("e", 64), string(observation.Stderr.Data))
	require.NotContains(t, string(observation.Stdout.Data), "must-not-leak")

	recorded, err := decodeProgramEvidence(released.Evidence)
	require.NoError(t, err)
	require.DirExists(t, recorded.ResourceRoot, "output survives until its durable observation is acknowledged")
	require.NoError(t, released.Runtime.ReleaseProgramResources(context.Background(), released.Evidence))
	require.NoDirExists(t, recorded.ResourceRoot)
}

func TestProgramHostRecoversCrashWindowByExactAttemptMarker(t *testing.T) {
	root := t.TempDir()
	host := ProgramProcessHost{PrivateRoot: filepath.Join(root, "private")}
	request := programRequest(root, "wait", 128)
	prepared, err := host.PrepareProgram(context.Background(), request)
	require.NoError(t, err)
	preparedEvidence := prepared.Describe().Evidence
	released, err := prepared.Release(context.Background(), &programTestPermit{execution: request.Execution.ID})
	require.NoError(t, err)
	process := released.Runtime.(*programRuntime).process
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _ = released.Runtime.StopProgram(ctx, ports.StopRequest{Force: true})
	})

	recovered, err := host.RecoverProgram(context.Background(), programRecoveryRequest(request, preparedEvidence))
	require.NoError(t, err)
	require.Equal(t, ports.RecoveryControlled, recovered.State)
	require.Equal(t, process.Identity(), recovered.Runtime.(*programRuntime).process.Identity())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stopped, err := recovered.Runtime.StopProgram(ctx, ports.StopRequest{Force: true})
	require.NoError(t, err)
	require.True(t, stopped.Exited)
	require.NoError(t, recovered.Runtime.ReleaseProgramResources(context.Background(), recovered.Evidence))
}

func TestProgramOutputCaptureSurvivesHostProcessExit(t *testing.T) {
	root := t.TempDir()
	evidencePath := filepath.Join(root, "released-evidence.json")
	completionPath := filepath.Join(root, "workload-complete")
	owner := exec.Command(os.Args[0], "-test.run=TestProgramCrashOwnerHelper", "--", root, evidencePath, completionPath)
	owner.Env = MergeEnvironment(os.Environ(), []string{"TCLAUDE_PROGRAM_CRASH_OWNER_HELPER=1"})
	require.NoError(t, owner.Run())
	var evidence model.ProviderEvidence
	raw, err := os.ReadFile(evidencePath)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &evidence))
	request := programRequest(root, "delayed-output", 128)
	request.Arguments = append(request.Arguments, completionPath)
	host := ProgramProcessHost{PrivateRoot: filepath.Join(root, "private")}
	recovered, err := host.RecoverProgram(context.Background(), programRecoveryRequest(request, evidence))
	require.NoError(t, err)
	require.Equal(t, ports.RecoveryControlled, recovered.State)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _ = recovered.Runtime.StopProgram(ctx, ports.StopRequest{Force: true})
	})
	require.Eventually(t, func() bool {
		_, statErr := os.Stat(completionPath)
		return statErr == nil
	}, 3*time.Second, 10*time.Millisecond)
	var observation ports.ProgramObservation
	require.Eventually(t, func() bool {
		observation, err = recovered.Runtime.ObserveProgram(context.Background())
		return err == nil && observation.Workload == ports.WorkloadExited
	}, 3*time.Second, 10*time.Millisecond)
	require.Contains(t, string(observation.Stdout.Data), "after-restart")
	require.NoError(t, recovered.Runtime.ReleaseProgramResources(context.Background(), recovered.Evidence))
}

func TestProgramOutputCaptureCountsDelayedShortWritesAsBytes(t *testing.T) {
	root := t.TempDir()
	host := ProgramProcessHost{PrivateRoot: filepath.Join(root, "private")}
	request := programRequest(root, "delayed-short-output", 4096)
	prepared, err := host.PrepareProgram(context.Background(), request)
	require.NoError(t, err)
	released, err := prepared.Release(context.Background(), &programTestPermit{execution: request.Execution.ID})
	require.NoError(t, err)
	var observation ports.ProgramObservation
	require.Eventually(t, func() bool {
		observation, err = released.Runtime.ObserveProgram(context.Background())
		return err == nil && observation.Workload == ports.WorkloadExited
	}, 2*time.Second, 10*time.Millisecond)
	require.Equal(t, "ab", string(observation.Stdout.Data))
	require.False(t, observation.Stdout.Truncated)
	require.NoError(t, released.Runtime.ReleaseProgramResources(context.Background(), released.Evidence))
}

func TestProgramHostRecoveryRejectsMismatchedWorkspaceUse(t *testing.T) {
	root := t.TempDir()
	host := ProgramProcessHost{PrivateRoot: filepath.Join(root, "private")}
	request := programRequest(root, "wait", 128)
	prepared, err := host.PrepareProgram(context.Background(), request)
	require.NoError(t, err)
	t.Cleanup(func() { _ = prepared.Abort(context.Background()) })
	recovery := programRecoveryRequest(request, prepared.Describe().Evidence)
	recovery.WorkspaceUse.ID = "different_use"
	result, err := host.RecoverProgram(context.Background(), recovery)
	require.ErrorContains(t, err, "does not match")
	require.Equal(t, ports.RecoveryUnknown, result.State)
}

func TestProgramHostAbortIsIdempotentAndReleasePermitIsOneShot(t *testing.T) {
	root := t.TempDir()
	host := ProgramProcessHost{PrivateRoot: filepath.Join(root, "private")}
	request := programRequest(root, "wait", 128)
	prepared, err := host.PrepareProgram(context.Background(), request)
	require.NoError(t, err)
	recorded, err := decodeProgramEvidence(prepared.Describe().Evidence)
	require.NoError(t, err)
	require.NoError(t, prepared.Abort(context.Background()))
	require.NoError(t, prepared.Abort(context.Background()))
	require.NoDirExists(t, recorded.ResourceRoot)
	_, err = prepared.Release(context.Background(), &programTestPermit{execution: request.Execution.ID})
	require.ErrorContains(t, err, "no longer releasable")
}

func TestProgramHostRefusesUnboundedOrConfinedRequests(t *testing.T) {
	root := t.TempDir()
	host := ProgramProcessHost{PrivateRoot: filepath.Join(root, "private")}
	request := programRequest(root, "output", 0)
	_, err := host.PrepareProgram(context.Background(), request)
	require.ErrorContains(t, err, "output limit")
	request.Profile.OutputLimitBytes = 64
	request.Profile.Sandbox = model.SandboxWorkspaceWrite
	_, err = host.PrepareProgram(context.Background(), request)
	require.ErrorContains(t, err, "does not enforce")
	request.Profile.Sandbox = model.SandboxUnconfined
	request.Input = []byte("not-json")
	_, err = host.PrepareProgram(context.Background(), request)
	require.ErrorContains(t, err, "valid JSON")
}

func programRequest(root, mode string, outputLimit int64) ports.ProgramPreparationRequest {
	execution := model.Execution{ID: "execution_program", Workload: model.ExecutionWorkloadProgram, State: model.ExecutionReserved, Attempt: 1}
	workspace := model.Workspace{ID: "workspace_program", State: model.WorkspaceAvailable,
		Observation: model.WorkspaceObservation{ActualPath: root}}
	use := model.WorkspaceUse{ID: "workspace_use_program", WorkspaceID: workspace.ID, ExecutionID: execution.ID, WorkRunID: "work_run_program"}
	return ports.ProgramPreparationRequest{
		Execution: execution,
		Profile: model.ProgramProfileRevision{
			ID: "program_revision", ProfileID: "program_profile", ContentHash: "profile-content-hash",
			Executable: os.Args[0], ArgumentPrefix: []string{"-test.run=TestProgramHostHelper", "--"},
			Environment: map[string]string{"TCLAUDE_PROGRAM_HOST_HELPER": "1"},
			Sandbox:     model.SandboxUnconfined, Timeout: 5 * time.Second, OutputLimitBytes: outputLimit,
		},
		Arguments: []string{mode}, Input: []byte(`{"bounded":true}`), Workspace: workspace, WorkspaceUse: use,
		WorkingDirectory: root, Deadline: time.Now().Add(10 * time.Second),
	}
}

func programRecoveryRequest(request ports.ProgramPreparationRequest, evidence model.ProviderEvidence) ports.ProgramRecoveryRequest {
	return ports.ProgramRecoveryRequest{Execution: request.Execution, Profile: request.Profile, Workspace: request.Workspace,
		WorkspaceUse: request.WorkspaceUse, WorkingDirectory: request.WorkingDirectory, Evidence: evidence}
}

type programTestPermit struct {
	execution model.ExecutionID
	operation model.OperationID
	consumed  atomic.Int32
}

func (p *programTestPermit) ExecutionID() model.ExecutionID { return p.execution }
func (p *programTestPermit) OperationID() model.OperationID { return p.operation }
func (p *programTestPermit) Consume(context.Context) error {
	if !p.consumed.CompareAndSwap(0, 1) {
		return fmt.Errorf("permit already consumed")
	}
	return nil
}

func TestProgramHostHelper(t *testing.T) {
	if os.Getenv("TCLAUDE_PROGRAM_HOST_HELPER") != "1" {
		return
	}
	args := processHelperArgsAfterDoubleDash(os.Args)
	switch args[0] {
	case "output":
		input, err := io.ReadAll(os.Stdin)
		if err != nil || string(input) != `{"bounded":true}` {
			os.Exit(8)
		}
		_, _ = fmt.Fprint(os.Stdout, "ambient="+os.Getenv("TCLAUDE_PROGRAM_AMBIENT_SECRET")+"\n"+strings.Repeat("o", 128))
		_, _ = fmt.Fprint(os.Stderr, strings.Repeat("e", 128))
		os.Exit(7)
	case "wait":
		waitForTestProcessStop()
	case "delayed-output":
		time.Sleep(500 * time.Millisecond)
		_, _ = fmt.Fprint(os.Stdout, "after-restart")
		if err := os.WriteFile(args[1], []byte("complete"), 0o600); err != nil {
			os.Exit(10)
		}
		os.Exit(0)
	case "delayed-short-output":
		_, _ = fmt.Fprint(os.Stdout, "a")
		time.Sleep(200 * time.Millisecond)
		_, _ = fmt.Fprint(os.Stdout, "b")
		os.Exit(0)
	}
	os.Exit(9)
}

func TestProgramCrashOwnerHelper(t *testing.T) {
	if os.Getenv("TCLAUDE_PROGRAM_CRASH_OWNER_HELPER") != "1" {
		return
	}
	args := processHelperArgsAfterDoubleDash(os.Args)
	request := programRequest(args[0], "delayed-output", 128)
	request.Arguments = append(request.Arguments, args[2])
	host := ProgramProcessHost{PrivateRoot: filepath.Join(args[0], "private")}
	prepared, err := host.PrepareProgram(context.Background(), request)
	if err != nil {
		os.Exit(20)
	}
	released, err := prepared.Release(context.Background(), &programTestPermit{execution: request.Execution.ID})
	if err != nil {
		os.Exit(21)
	}
	raw, err := json.Marshal(released.Evidence)
	if err != nil || os.WriteFile(args[1], raw, 0o600) != nil {
		os.Exit(22)
	}
	os.Exit(0)
}

func TestProgramOutputCompletionDuringExitObservation(t *testing.T) {
	output := newBoundedOutputFile(t.TempDir(), "stdout", "truncated", "complete", 128)
	pending, err := observeOutputPending(func() ProcessObservation {
		// The spooler completes and exits after the first filesystem read but
		// before the process observation returns.
		require.NoError(t, os.WriteFile(output.completePath, nil, 0600))
		return ProcessObservation{Exited: true}
	}, output)
	require.NoError(t, err)
	require.False(t, pending)
	require.NoError(t, os.Remove(output.completePath))
	_, err = observeOutputPending(func() ProcessObservation { return ProcessObservation{Exited: true} }, output)
	require.ErrorContains(t, err, "without durable completion")
}
