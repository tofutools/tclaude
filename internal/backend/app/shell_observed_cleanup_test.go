package app_test

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	db "github.com/tofutools/tclaude/internal/backend/sqlite"
	"path/filepath"
	"testing"
	"time"
)

type delayedExitShellHost struct{ runtime *delayedExitShellRuntime }
type delayedExitShellRuntime struct {
	journeyHostRuntime
	state     ports.WorkloadObservedState
	onObserve func()
}
type delayedExitPreparedShell struct {
	journeyPreparedShell
	runtime *delayedExitShellRuntime
}

func (h *delayedExitShellHost) PrepareShell(_ context.Context, req ports.ShellPreparationRequest) (ports.PreparedShell, error) {
	h.runtime.id = req.ExecutionID
	return &delayedExitPreparedShell{journeyPreparedShell: journeyPreparedShell{request: req}, runtime: h.runtime}, nil
}
func (h *delayedExitShellHost) RecoverShell(_ context.Context, _ ports.ShellRecoveryRequest) (ports.ShellRecoveryResult, error) {
	return ports.ShellRecoveryResult{State: ports.RecoveryControlled, Runtime: h.runtime, Evidence: modelShellEvidence()}, nil
}
func (p *delayedExitPreparedShell) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ShellReleaseResult, error) {
	if err := permit.Consume(ctx); err != nil {
		return ports.ShellReleaseResult{}, err
	}
	return ports.ShellReleaseResult{State: ports.ReleaseStarted, Runtime: p.runtime, Evidence: modelShellEvidence()}, nil
}
func (r *delayedExitShellRuntime) ObserveHost(context.Context) (ports.HostObservation, error) {
	if r.onObserve != nil {
		r.onObserve()
	}
	return ports.HostObservation{Workload: r.state, ObservedAt: time.Now(), Evidence: modelShellEvidence()}, nil
}
func (r *delayedExitShellRuntime) StopHost(context.Context, ports.StopRequest) (ports.HostStopResult, error) {
	return ports.HostStopResult{Disposition: ports.EffectAccepted, Acknowledged: true, Exited: false, Evidence: modelShellEvidence()}, nil
}
func TestObservedDelayedShellExitReleasesCheckoutAndCannotResurrect(t *testing.T) {
	testObservedShellCleanup(t, false)
}
func TestObservedShellExitSettlesAfterRequestCancellation(t *testing.T) {
	testObservedShellCleanup(t, true)
}
func testObservedShellCleanup(t *testing.T, cancelAtObservation bool) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "db"))
	require.NoError(t, err)
	defer store.Close()
	host := &journeyWorkspaceHost{}
	shell := &delayedExitShellHost{runtime: &delayedExitShellRuntime{state: ports.WorkloadRunning}}
	service := app.New(store, providers.NewRegistry()).WithWorkspaceHost(host).WithShellHost(shell)
	op := model.OperatorPrincipal()
	workspace, err := service.CreateCheckout(ctx, app.CreateCheckoutRequest{Context: request(op, "create"), ID: "workspace", Intent: model.WorkspaceIntent{Repository: "repo", IntendedPath: filepath.Join(t.TempDir(), "checkout"), BaseRevision: "main", Branch: "work"}})
	require.NoError(t, err)
	launch, err := service.StartShell(ctx, app.StartShellRequest{Context: request(op, "launch"), WorkspaceID: workspace.Workspace.ID, ExpectedRevision: workspace.Workspace.Revision, Sandbox: model.SandboxWorkspaceWrite})
	require.NoError(t, err)
	_, err = service.Stop(ctx, app.StopRequest{RequestContext: request(op, "stop"), ExecutionID: launch.Execution.ID})
	require.NoError(t, err)
	remove := app.RemoveCheckoutRequest{Context: request(op, "remove"), WorkspaceID: workspace.Workspace.ID, ExpectedRevision: workspace.Workspace.Revision}
	_, err = service.RemoveCheckout(ctx, remove)
	require.ErrorIs(t, err, app.ErrConflict)
	require.Zero(t, host.removes)
	shell.runtime.state = ports.WorkloadExited
	observationCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if cancelAtObservation {
		shell.runtime.onObserve = cancel
	}
	observed, err := service.Observe(observationCtx, app.ObserveRequest{Principal: op, ExecutionID: launch.Execution.ID})
	require.NoError(t, err)
	require.Equal(t, model.ExecutionExited, observed.Execution.State)
	shell.runtime.onObserve = nil
	shell.runtime.state = ports.WorkloadRunning
	stale, err := service.Observe(ctx, app.ObserveRequest{Principal: op, ExecutionID: launch.Execution.ID})
	require.NoError(t, err)
	require.Equal(t, model.ExecutionExited, stale.Execution.State)
	result, err := service.RemoveCheckout(ctx, remove)
	require.NoError(t, err)
	require.Equal(t, model.WorkspaceRemoved, result.Workspace.State)
	require.Equal(t, 1, host.removes)
}
