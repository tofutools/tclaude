//go:build linux || darwin

package claude

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
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	backendsqlite "github.com/tofutools/tclaude/internal/backend/sqlite"
)

type claudeWorkSources map[string]ports.HistoryDiscoveryScope

func (s claudeWorkSources) HistorySource(harness, name string) (ports.HistoryDiscoveryScope, bool) {
	scope, ok := s[harness+":"+name]
	return scope, ok
}

func TestClaudeFreshHandoffRunsDurableWorkJourney(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is unavailable")
	}
	root, err := os.MkdirTemp("/tmp", "tclaude-claude-work-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
	repository := filepath.Join(root, "repository")
	require.NoError(t, os.Mkdir(repository, 0o700))
	claudeWorkGit(t, repository, "init", "-b", "main")
	claudeWorkGit(t, repository, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "base")

	historyRoot := filepath.Join(root, "history")
	project := filepath.Join(historyRoot, "-workspace")
	require.NoError(t, os.MkdirAll(project, 0o700))
	nativeID := "43e874eb-4827-4b22-b1b8-376a5e5e553f"
	writeClaudeHistory(t, filepath.Join(project, nativeID+".jsonl"),
		`{"type":"user","uuid":"message-one","sessionId":"`+nativeID+`","cwd":"/workspace","timestamp":"2026-09-06T10:00:00Z","message":{"role":"user","content":"retain the compatibility boundary"}}`,
		`{"type":"assistant","uuid":"message-two","sessionId":"`+nativeID+`","cwd":"/workspace","timestamp":"2026-09-06T10:01:00Z","message":{"role":"assistant","content":"use the explicit adapter"}}`,
	)
	launches := filepath.Join(root, "launches")
	input := filepath.Join(root, "input")
	executable := filepath.Join(root, "claude-native-double")
	script := "#!/bin/sh\nprintf 'launch\\n' >> '" + launches + "'\nwhile IFS= read -r line; do printf '%s\\n' \"$line\" >> '" + input + "'; done\n"
	require.NoError(t, os.WriteFile(executable, []byte(script), 0o700))
	provider, err := New(Config{Executable: executable, PrivateRoot: filepath.Join(root, "provider"), AgentSocket: filepath.Join(root, "agent.sock")})
	require.NoError(t, err)
	checkout, err := host.NewCheckoutHost("git")
	require.NoError(t, err)
	database := filepath.Join(root, "backend.sqlite")
	open := func() (*backendsqlite.Store, *app.Service) {
		store, openErr := backendsqlite.Open(database)
		require.NoError(t, openErr)
		service := app.New(store, providers.NewRegistry(provider)).
			WithAgentAPIEndpoint(filepath.Join(root, "agent.sock")).
			WithWorkspaceHost(checkout).
			WithHistorySources(claudeWorkSources{Name + ":archive": {Source: historyRoot}})
		return store, service
	}
	store, service := open()
	operator := model.OperatorPrincipal()
	refreshed, err := service.RefreshHistory(context.Background(), app.RefreshHistoryRequest{Principal: operator, Harness: Name, SourceName: "archive"})
	require.NoError(t, err)
	require.Len(t, refreshed.Entries, 1)
	selection := model.HistorySelection{ConversationID: refreshed.Entries[0].ConversationID, ExpectedConversationRevision: refreshed.Entries[0].Revision}
	read, err := service.ReadHistory(context.Background(), app.ReadHistoryRequest{Principal: operator, Selection: selection})
	require.NoError(t, err)
	require.Equal(t, "retain the compatibility boundary", read.Turns[0].Parts[0].Text)

	checkoutPath := filepath.Join(root, "checkout")
	workspace, err := service.CreateCheckout(context.Background(), app.CreateCheckoutRequest{
		Context: claudeWorkRequest(operator, "create"), ID: "workspace_claude",
		Intent: model.WorkspaceIntent{Repository: repository, IntendedPath: checkoutPath, BaseRevision: "main", Branch: "claude-worker", RetainOnFinish: true},
	})
	require.NoError(t, err)
	desired := model.DesiredConfiguration{Harness: Name, Model: "fixture", WorkingDirectory: checkoutPath, Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	agent, err := service.CreateAgent(context.Background(), app.CreateAgentRequest{Context: operator, ID: "worker_claude", Name: "Claude worker", Desired: desired})
	require.NoError(t, err)
	handoff := "Selected history: " + read.Turns[0].Parts[0].Text
	start := app.StartWorkRequest{Context: claudeWorkRequest(operator, "start"), ID: "work_claude", Spec: model.WorkRunSpec{
		SourceMode: model.WorkSourceFreshHandoff, FreshHandoff: handoff,
		WorkspaceID: workspace.Workspace.ID, WorkspaceRevision: workspace.Workspace.Revision,
		WorkerAgentID: agent.Agent.ID, WorkerAgentRevision: agent.Agent.Revision, WorkerDesired: desired,
		Brief: "Implement the bounded compatibility change", Outcome: model.WorkOutcomePolicy{Mode: model.WorkOutcomeHumanDecision},
	}}
	run, err := service.StartWork(context.Background(), start)
	require.NoError(t, err)
	_, err = service.ReconcilePendingWork(context.Background())
	require.NoError(t, err)
	run, err = service.InspectWork(context.Background(), app.InspectWorkRequest{Principal: operator, WorkRunID: run.Run.ID})
	require.NoError(t, err)
	require.Equal(t, model.WorkRunWaiting, run.Run.State)
	require.Eventually(t, func() bool {
		value, readErr := os.ReadFile(input)
		return readErr == nil && strings.Contains(string(value), handoff) && strings.Contains(string(value), "Implement the bounded compatibility change")
	}, time.Second, 10*time.Millisecond)

	require.NoError(t, store.Close())
	store, service = open()
	defer store.Close()
	_, err = service.Recover(context.Background(), app.RecoverRequest{Principal: operator})
	require.NoError(t, err)
	_, err = service.ReconcilePendingWork(context.Background())
	require.NoError(t, err)
	launchData, err := os.ReadFile(launches)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(launchData), "launch"))
	inputData, err := os.ReadFile(input)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(inputData), handoff), "restart must not redeliver the work request")

	run, err = service.InspectWork(context.Background(), app.InspectWorkRequest{Principal: operator, WorkRunID: run.Run.ID})
	require.NoError(t, err)
	reported, err := service.RecordWorkEvidence(context.Background(), app.RecordWorkEvidenceRequest{
		Context: claudeWorkRequest(operator, "evidence"), WorkRunID: run.Run.ID, ExpectedRunRevision: run.Run.Revision,
		Step: model.WorkStepAwaitEvidence, Attempt: 1, Kind: model.WorkEvidenceWorkerReport, ArtifactRevision: "worker-commit", Detail: "bounded result ready",
	})
	require.NoError(t, err)
	decided, err := service.DecideWork(context.Background(), app.DecideWorkRequest{
		Context: claudeWorkRequest(operator, "decision"), WorkRunID: run.Run.ID, ExpectedRunRevision: reported.Run.Revision,
		Step: model.WorkStepAwaitEvidence, Attempt: 1, Decision: model.WorkDecisionAccept, Reason: "human accepted bounded result",
	})
	require.NoError(t, err)
	require.Equal(t, model.WorkRunSucceeded, decided.Run.State)
	require.Equal(t, model.PrincipalOperator, decided.Decision.Decider.Kind)
	stopped, err := service.Stop(context.Background(), app.StopRequest{RequestContext: claudeWorkRequest(operator, "stop"), ExecutionID: run.Run.WorkerExecutionID, Force: true})
	require.NoError(t, err)
	require.Equal(t, model.ExecutionExited, stopped.Execution.State)
	require.DirExists(t, checkoutPath, "stopping accepted work retains its owned checkout")
}

func claudeWorkRequest(principal model.Principal, id model.RequestID) app.RequestContext {
	return app.RequestContext{Principal: principal, RequestID: id}
}

func claudeWorkGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, args...)...)
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
}
