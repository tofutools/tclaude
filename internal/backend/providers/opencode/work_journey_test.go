//go:build linux || darwin

package opencode

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	backendsqlite "github.com/tofutools/tclaude/internal/backend/sqlite"
)

type openCodeWorkSources map[string]ports.HistoryDiscoveryScope

func (s openCodeWorkSources) HistorySource(harness, name string) (ports.HistoryDiscoveryScope, bool) {
	scope, ok := s[harness+":"+name]
	return scope, ok
}

func TestOpenCodeExactForkRunsDurableWorkJourney(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "tclaude-opencode-work-")
	require.NoError(t, err)
	root, err = filepath.EvalSymlinks(root)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
	repository := filepath.Join(root, "repository")
	require.NoError(t, os.Mkdir(repository, 0o700))
	openCodeWorkGit(t, repository, "init", "-b", "main")
	openCodeWorkGit(t, repository, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "base")

	providerRoot := filepath.Join(root, "provider")
	require.NoError(t, os.Mkdir(providerRoot, 0o700))
	sourceRoot := filepath.Join(providerRoot, "execution-source")
	require.NoError(t, os.Mkdir(sourceRoot, 0o700))
	require.NoError(t, writeHistoryManifest(sourceRoot, historyManifest{NativeID: "ses_source", CWD: repository}))
	checkoutPath := filepath.Join(root, "checkout")
	sourceExportPath := filepath.Join(root, "source-export.json")
	writeOpenCodeExport(t, sourceExportPath, "ses_source", repository, "selected source answer")
	importedExportPath := filepath.Join(root, "imported-export.json")
	writeOpenCodeExport(t, importedExportPath, "ses_source", checkoutPath, "selected source answer")
	importPath := filepath.Join(root, "imported")
	forkPointPath := filepath.Join(root, "fork-point")
	promptPath := filepath.Join(root, "prompt")
	promptCountPath := filepath.Join(root, "prompt-count")
	launchCountPath := filepath.Join(root, "launch-count")
	executable := filepath.Join(root, "opencode-native-double")
	script := `#!/bin/sh
if [ "$1" = export ]; then
  if [ "$XDG_DATA_HOME" = "$OPENCODE_SOURCE_DATA_HOME" ]; then
    cat "$OPENCODE_SOURCE_EXPORT"
  else
    cat "$OPENCODE_IMPORTED_EXPORT"
  fi
  exit
fi
if [ "$1" = import ]; then printf 'import\n' >> "$OPENCODE_TEST_IMPORT"; exit; fi
if [ "$1" = serve ]; then printf 'launch\n' >> "$OPENCODE_TEST_LAUNCH_COUNT"; fi
exec "$OPENCODE_TEST_BINARY" -test.run=TestOpenCodeServerHelper -- "$@"
`
	require.NoError(t, os.WriteFile(executable, []byte(script), 0o700))
	provider, err := New(Config{Executable: executable, PrivateRoot: providerRoot, AgentSocket: filepath.Join(root, "agent.sock"), Environment: []string{
		"OPENCODE_TEST_BINARY=" + os.Args[0], "OPENCODE_SOURCE_EXPORT=" + sourceExportPath,
		"OPENCODE_IMPORTED_EXPORT=" + importedExportPath, "OPENCODE_SOURCE_DATA_HOME=" + filepath.Join(sourceRoot, "data"),
		"OPENCODE_TEST_IMPORT=" + importPath, "OPENCODE_TEST_FORK_POINT=" + forkPointPath,
		"OPENCODE_TEST_PROMPT=" + promptPath, "OPENCODE_TEST_PROMPT_COUNT=" + promptCountPath,
		"OPENCODE_TEST_LAUNCH_COUNT=" + launchCountPath,
	}})
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
			WithHistorySources(openCodeWorkSources{Name + ":owned": {}})
		return store, service
	}
	store, service := open()
	operator := model.OperatorPrincipal()
	refreshed, err := service.RefreshHistory(context.Background(), app.RefreshHistoryRequest{Principal: operator, Harness: Name, SourceName: "owned"})
	require.NoError(t, err)
	require.Len(t, refreshed.Entries, 1)
	head := model.HistorySelection{ConversationID: refreshed.Entries[0].ConversationID, ExpectedConversationRevision: refreshed.Entries[0].Revision}
	read, err := service.ReadHistory(context.Background(), app.ReadHistoryRequest{Principal: operator, Selection: head})
	require.NoError(t, err)
	require.Len(t, read.Turns, 2)
	require.Len(t, read.Points, 3)
	var point model.HistoryPoint
	for _, candidate := range read.Points {
		if candidate.Kind == model.HistoryPointBeforeMessage && (point.ID == "" || candidate.OccurredAt.After(point.OccurredAt)) {
			point = candidate
		}
	}
	require.NotEmpty(t, point.ID)
	require.Equal(t, model.HistoryPointBeforeMessage, point.Kind)
	selection := model.HistorySelection{ConversationID: read.Entry.ConversationID, ExpectedConversationRevision: read.Entry.Revision, PointID: point.ID, ExpectedPointRevision: point.Revision}

	workspace, err := service.CreateCheckout(context.Background(), app.CreateCheckoutRequest{
		Context: openCodeWorkRequest(operator, "create"), ID: "workspace_opencode",
		Intent: model.WorkspaceIntent{Repository: repository, IntendedPath: checkoutPath, BaseRevision: "main", Branch: "opencode-worker", RetainOnFinish: true},
	})
	require.NoError(t, err)
	desired := model.DesiredConfiguration{Harness: Name, Model: "provider/model", WorkingDirectory: checkoutPath, Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}
	agent, err := service.CreateAgent(context.Background(), app.CreateAgentRequest{Context: operator, ID: "worker_opencode", Name: "OpenCode worker", Desired: desired})
	require.NoError(t, err)
	start := app.StartWorkRequest{Context: openCodeWorkRequest(operator, "start"), ID: "work_opencode", Spec: model.WorkRunSpec{
		SourceMode: model.WorkSourceFork, History: selection,
		WorkspaceID: workspace.Workspace.ID, WorkspaceRevision: workspace.Workspace.Revision,
		WorkerAgentID: agent.Agent.ID, WorkerAgentRevision: agent.Agent.Revision, WorkerDesired: desired,
		Brief: "Implement the bounded forked change", Outcome: model.WorkOutcomePolicy{Mode: model.WorkOutcomeHumanDecision},
	}}
	run, err := service.StartWork(context.Background(), start)
	require.NoError(t, err)
	require.NotEmpty(t, run.Run.HistoryUseID)
	_, err = service.ReconcilePendingWork(context.Background())
	require.NoError(t, err)
	run, err = service.InspectWork(context.Background(), app.InspectWorkRequest{Principal: operator, WorkRunID: run.Run.ID})
	require.NoError(t, err)
	require.Equal(t, model.WorkRunWaiting, run.Run.State, "%+v", run.Run.Attempts)
	pointData, err := os.ReadFile(forkPointPath)
	require.NoError(t, err)
	require.Equal(t, "msg_two", string(pointData))
	require.FileExists(t, importPath, "the supported fork uses the native export/import boundary")
	promptData, err := os.ReadFile(promptPath)
	require.NoError(t, err)
	require.Contains(t, string(promptData), "Implement the bounded forked change")

	require.NoError(t, store.Close())
	store, service = open()
	defer store.Close()
	_, err = service.Recover(context.Background(), app.RecoverRequest{Principal: operator})
	require.NoError(t, err)
	_, err = service.ReconcilePendingWork(context.Background())
	require.NoError(t, err)
	launchData, err := os.ReadFile(launchCountPath)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(launchData), "launch"), "restart must recover rather than launch another server")
	promptCount, err := os.ReadFile(promptCountPath)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(promptCount), "prompt"), "restart must not redeliver the work request")

	run, err = service.InspectWork(context.Background(), app.InspectWorkRequest{Principal: operator, WorkRunID: run.Run.ID})
	require.NoError(t, err)
	reported, err := service.RecordWorkEvidence(context.Background(), app.RecordWorkEvidenceRequest{
		Context: openCodeWorkRequest(operator, "evidence"), WorkRunID: run.Run.ID, ExpectedRunRevision: run.Run.Revision,
		Step: model.WorkStepAwaitEvidence, Attempt: 1, Kind: model.WorkEvidenceWorkerReport, ArtifactRevision: "worker-commit", Detail: "bounded fork result ready",
	})
	require.NoError(t, err)
	decided, err := service.DecideWork(context.Background(), app.DecideWorkRequest{
		Context: openCodeWorkRequest(operator, "decision"), WorkRunID: run.Run.ID, ExpectedRunRevision: reported.Run.Revision,
		Step: model.WorkStepAwaitEvidence, Attempt: 1, Decision: model.WorkDecisionAccept, Reason: "human accepted bounded fork result",
	})
	require.NoError(t, err)
	require.Equal(t, model.WorkRunSucceeded, decided.Run.State)
	require.Equal(t, model.PrincipalOperator, decided.Decision.Decider.Kind)
	stopped, err := service.Stop(context.Background(), app.StopRequest{RequestContext: openCodeWorkRequest(operator, "stop"), ExecutionID: run.Run.WorkerExecutionID, Force: true})
	require.NoError(t, err)
	require.Equal(t, model.ExecutionExited, stopped.Execution.State)
	require.DirExists(t, checkoutPath, "stopping accepted work retains its owned checkout")
}

func openCodeWorkRequest(principal model.Principal, id model.RequestID) app.RequestContext {
	return app.RequestContext{Principal: principal, RequestID: id}
}

func openCodeWorkGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, args...)...)
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
}
