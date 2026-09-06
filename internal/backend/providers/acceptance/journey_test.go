//go:build linux || darwin

package acceptance_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/internal/backend/app"
	backendhost "github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/providers/codex"
	"github.com/tofutools/tclaude/internal/backend/providers/copilot"
	backendsqlite "github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestNativeProvidersRunPersistentCheckoutWorkOutcomeJourney(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is unavailable")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable")
	}

	cases := []struct {
		name        string
		harness     string
		sandbox     model.SandboxMode
		newProvider func(root, executable, socket string) (ports.Provider, error)
	}{
		{
			name:    "codex",
			harness: codex.Name,
			sandbox: model.SandboxWorkspaceWrite,
			newProvider: func(root, executable, socket string) (ports.Provider, error) {
				return codex.New(codex.Config{Executable: executable, PrivateRoot: root, AgentSocket: socket})
			},
		},
		{
			name:    "copilot",
			harness: copilot.Name,
			sandbox: model.SandboxUnconfined,
			newProvider: func(root, executable, socket string) (ports.Provider, error) {
				return copilot.New(copilot.Config{Executable: executable, PrivateRoot: root, AgentSocket: socket})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testNativeProviderJourney(t, tc.harness, tc.sandbox, tc.newProvider)
		})
	}
}

func testNativeProviderJourney(
	t *testing.T,
	harness string,
	sandbox model.SandboxMode,
	newProvider func(root, executable, socket string) (ports.Provider, error),
) {
	t.Helper()
	ctx := context.Background()
	root, err := os.MkdirTemp("/tmp", "tcl-provider-journey-")
	require.NoError(t, err)
	root, err = filepath.EvalSymlinks(root)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })

	repository := filepath.Join(root, "repository")
	require.NoError(t, os.Mkdir(repository, 0o700))
	runGit(t, repository, "init", "-b", "main")
	runGit(t, repository, "config", "user.name", "Provider Journey Test")
	runGit(t, repository, "config", "user.email", "provider-journey@example.invalid")
	require.NoError(t, os.WriteFile(filepath.Join(repository, "README.md"), []byte("initial\n"), 0o600))
	runGit(t, repository, "add", "README.md")
	runGit(t, repository, "commit", "-m", "initial")

	input := filepath.Join(root, "native-input")
	executable := filepath.Join(root, "native-double")
	script := "#!/bin/sh\nwhile IFS= read -r line; do printf '%s\\n' \"$line\" >> \"" + input + "\"; done\n"
	require.NoError(t, os.WriteFile(executable, []byte(script), 0o700))
	agentSocket := filepath.Join(root, "agent.sock")
	provider, err := newProvider(filepath.Join(root, "provider"), executable, agentSocket)
	require.NoError(t, err)
	checkoutHost, err := backendhost.NewCheckoutHost("git")
	require.NoError(t, err)

	dbPath := filepath.Join(root, "backend.db")
	store, err := backendsqlite.Open(dbPath)
	require.NoError(t, err)
	service := app.New(store, providers.NewRegistry(provider)).
		WithWorkspaceHost(checkoutHost).
		WithAgentAPIEndpoint(agentSocket)
	operator := model.OperatorPrincipal()
	checkoutPath := filepath.Join(root, "checkout")
	workspace, err := service.CreateCheckout(ctx, app.CreateCheckoutRequest{
		Context: request(operator, "create_workspace"),
		ID:      model.WorkspaceID("workspace_" + harness),
		Intent: model.WorkspaceIntent{
			Repository: repository, IntendedPath: checkoutPath,
			BaseRevision: "HEAD", Branch: "feature/" + harness,
		},
	})
	require.NoError(t, err)
	require.Equal(t, model.WorkspaceAvailable, workspace.Workspace.State)
	require.NotEmpty(t, workspace.Workspace.Observation.Revision)

	desired := model.DesiredConfiguration{
		Harness: harness, Model: "test-model", WorkingDirectory: checkoutPath,
		Approval: model.ApprovalSupervised, Sandbox: sandbox,
	}
	worker, err := service.CreateAgent(ctx, app.CreateAgentRequest{
		Context: operator, ID: model.AgentID("worker_" + harness), Name: harness + " worker", Desired: desired,
	})
	require.NoError(t, err)
	require.NotZero(t, worker.Agent.Revision)

	handoffMarker := "fresh-handoff-" + harness
	briefMarker := "bounded-brief-" + harness
	run, err := service.StartWork(ctx, app.StartWorkRequest{
		Context: request(operator, "start_work"),
		ID:      model.WorkRunID("work_" + harness),
		Spec: model.WorkRunSpec{
			SourceMode:   model.WorkSourceFreshHandoff,
			FreshHandoff: handoffMarker,
			WorkspaceID:  workspace.Workspace.ID, WorkspaceRevision: workspace.Workspace.Revision,
			WorkerAgentID: worker.Agent.ID, WorkerAgentRevision: worker.Agent.Revision,
			WorkerDesired: desired, Brief: briefMarker,
			Outcome: model.WorkOutcomePolicy{Mode: model.WorkOutcomeHumanDecision},
		},
	})
	require.NoError(t, err)
	require.Equal(t, workspace.Workspace.Revision, run.Run.Spec.WorkspaceRevision)
	require.Equal(t, worker.Agent.Revision, run.Run.Spec.WorkerAgentRevision)
	require.Equal(t, model.WorkSourceFreshHandoff, run.Run.Spec.SourceMode)

	reconciled, err := service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	require.Empty(t, reconciled.Pending)
	require.Empty(t, reconciled.Uncertain)
	run, err = service.InspectWork(ctx, app.InspectWorkRequest{Principal: operator, WorkRunID: run.Run.ID})
	require.NoError(t, err)
	require.Equal(t, model.WorkRunWaiting, run.Run.State)
	require.NotEmpty(t, run.Run.WorkerExecutionID)
	workerExecutionID := run.Run.WorkerExecutionID
	workerStopped := false
	t.Cleanup(func() {
		if !workerStopped {
			stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, _ = service.Stop(stopCtx, app.StopRequest{
				RequestContext: request(operator, "cleanup_worker"), ExecutionID: workerExecutionID, Force: true,
			})
		}
		_ = store.Close()
	})
	require.Eventually(t, func() bool {
		raw, readErr := os.ReadFile(input)
		return readErr == nil && strings.Count(string(raw), handoffMarker) == 1 &&
			strings.Count(string(raw), briefMarker) == 1
	}, time.Second, 10*time.Millisecond)

	// A new application instance recovers the provider-owned terminal from
	// durable SQLite evidence. The settled delivery operation prevents replay.
	require.NoError(t, store.Close())
	store, err = backendsqlite.Open(dbPath)
	require.NoError(t, err)
	service = app.New(store, providers.NewRegistry(provider)).
		WithWorkspaceHost(checkoutHost).
		WithAgentAPIEndpoint(agentSocket)
	recovered, err := service.Recover(ctx, app.RecoverRequest{Principal: operator})
	require.NoError(t, err)
	require.Equal(t, []model.ExecutionID{run.Run.WorkerExecutionID}, recovered.Controlled)
	reconciled, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	require.Empty(t, reconciled.Pending)
	raw, err := os.ReadFile(input)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(raw), handoffMarker), "recovery must not redeliver the handoff")
	require.Equal(t, 1, strings.Count(string(raw), briefMarker), "recovery must not redeliver the brief")

	run, err = service.InspectWork(ctx, app.InspectWorkRequest{Principal: operator, WorkRunID: run.Run.ID})
	require.NoError(t, err)
	recorded, err := service.RecordWorkEvidence(ctx, app.RecordWorkEvidenceRequest{
		Context: request(operator, "record_evidence"), WorkRunID: run.Run.ID,
		Step: model.WorkStepAwaitEvidence, Attempt: 1, Kind: model.WorkEvidenceWorkerReport,
		ArtifactRevision: workspace.Workspace.Observation.Revision, Detail: "native worker reported completion",
		ExpectedRunRevision: run.Run.Revision,
	})
	require.NoError(t, err)
	require.Len(t, recorded.Evidence, 1)
	require.Equal(t, operator, recorded.Evidence[0].Reporter)
	require.Equal(t, workspace.Workspace.Observation.Revision, recorded.Evidence[0].ArtifactRevision)
	decided, err := service.DecideWork(ctx, app.DecideWorkRequest{
		Context: request(operator, "decide_outcome"), WorkRunID: run.Run.ID,
		Step: model.WorkStepAwaitEvidence, Attempt: 1, Decision: model.WorkDecisionAccept,
		Reason: "operator accepted attributed worker evidence", ExpectedRunRevision: recorded.Run.Revision,
	})
	require.NoError(t, err)
	require.Equal(t, model.WorkRunSucceeded, decided.Run.State)
	require.NotNil(t, decided.Decision)
	require.Equal(t, operator, decided.Decision.Decider)

	_, err = service.RemoveCheckout(ctx, app.RemoveCheckoutRequest{
		Context: request(operator, "remove_live_workspace"), WorkspaceID: workspace.Workspace.ID,
		ExpectedRevision: workspace.Workspace.Revision,
	})
	require.ErrorIs(t, err, app.ErrConflict, "settlement must retain the checkout claim while the worker is alive")
	stopped, err := service.Stop(ctx, app.StopRequest{
		RequestContext: request(operator, "stop_worker"), ExecutionID: run.Run.WorkerExecutionID, Force: true,
	})
	require.NoError(t, err)
	require.Equal(t, model.ExecutionExited, stopped.Execution.State)
	workerStopped = true
	removed, err := service.RemoveCheckout(ctx, app.RemoveCheckoutRequest{
		Context: request(operator, "remove_stopped_workspace"), WorkspaceID: workspace.Workspace.ID,
		ExpectedRevision: workspace.Workspace.Revision,
	})
	require.NoError(t, err)
	require.Equal(t, model.WorkspaceRemoved, removed.Workspace.State)
	require.NoDirExists(t, checkoutPath)
}

func request(principal model.Principal, id model.RequestID) app.RequestContext {
	return app.RequestContext{Principal: principal, RequestID: id}
}

func runGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, args...)...)
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
}
