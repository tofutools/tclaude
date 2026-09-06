//go:build linux || darwin

package opencode

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

// The server executable is doubled; the actual provider, application-owned
// admission sink and SQLite must agree on the resumed execution's first binding.
func TestApplicationResumeBindsInitialObservationToNewExecution(t *testing.T) {
	root, err := os.MkdirTemp("", "opencode-app-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
	executable := filepath.Join(root, "native")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\nexec \"$OPENCODE_TEST_BINARY\" -test.run=TestOpenCodeServerHelper -- \"$@\"\n"), 0700))
	socket := filepath.Join(root, "api.sock")
	provider, err := New(Config{Executable: executable, PrivateRoot: root, AgentSocket: socket, Environment: []string{"OPENCODE_TEST_BINARY=" + os.Args[0]}})
	require.NoError(t, err)
	store, err := sqlite.Open(filepath.Join(root, "backend.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry(provider)).WithAgentAPIEndpoint(socket)
	ctx := context.Background()
	operator := model.OperatorPrincipal()
	agent, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: operator, ID: "worker", Name: "Worker", Desired: model.DesiredConfiguration{Harness: Name, WorkingDirectory: root, Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}})
	require.NoError(t, err)
	first, err := service.Launch(ctx, app.LaunchRequest{RequestContext: app.RequestContext{Principal: operator, RequestID: "launch-first"}, Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: agent.Agent.ID, ExpectedRevision: agent.Agent.Revision}}})
	require.NoError(t, err)
	require.NotNil(t, first.Execution)
	defer func() {
		_, _ = service.Stop(ctx, app.StopRequest{RequestContext: app.RequestContext{Principal: operator, RequestID: "cleanup-first"}, ExecutionID: first.Execution.ID, Force: true})
	}()
	require.Equal(t, model.ExecutionRunning, first.Execution.State)
	require.Equal(t, model.ContextReadinessReady, first.Execution.ContextReadiness)
	_, err = service.Stop(ctx, app.StopRequest{RequestContext: app.RequestContext{Principal: operator, RequestID: "stop-first"}, ExecutionID: first.Execution.ID, Force: true})
	require.NoError(t, err)
	snapshot, err := service.Snapshot(ctx, app.SnapshotRequest{Principal: operator})
	require.NoError(t, err)
	require.Len(t, snapshot.Agents, 1)
	require.Len(t, snapshot.Associations, 1)
	resumed, err := service.Resume(ctx, app.ResumeRequest{RequestContext: app.RequestContext{Principal: operator, RequestID: "resume-second"}, Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: agent.Agent.ID, ExpectedRevision: snapshot.Agents[0].Revision}}, ConversationID: first.Execution.ConversationID, ExpectedAssociationRevision: snapshot.Associations[0].Revision})
	if resumed.Execution != nil {
		defer func() {
			_, _ = service.Stop(ctx, app.StopRequest{RequestContext: app.RequestContext{Principal: operator, RequestID: "cleanup-second"}, ExecutionID: resumed.Execution.ID, Force: true})
		}()
	}
	require.NoError(t, err)
	require.NotNil(t, resumed.Execution)
	require.NotEqual(t, first.Execution.ID, resumed.Execution.ID)
	require.Equal(t, first.Execution.ConversationID, resumed.Execution.ConversationID)
	require.Equal(t, model.ExecutionRunning, resumed.Execution.State)
	require.Equal(t, model.ContextReadinessReady, resumed.Execution.ContextReadiness)
	require.Equal(t, first.Execution.NativeConversation.Reference, resumed.Execution.NativeConversation.Reference)
}
