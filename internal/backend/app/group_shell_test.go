package app_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

type groupShellHost struct {
	journeyShellHost
	prepared []ports.ShellPreparationRequest
}

func (h *groupShellHost) PrepareShell(ctx context.Context, req ports.ShellPreparationRequest) (ports.PreparedShell, error) {
	h.prepared = append(h.prepared, req)
	return h.journeyShellHost.PrepareShell(ctx, req)
}

func TestGroupShellPinsEnvironmentAndRetriesAfterDefaultsChangeAndReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "backend.db")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	workspaceHost := &journeyWorkspaceHost{}
	shell := &groupShellHost{}
	service := journeyService(store, newJourneyProvider(), workspaceHost).WithShellHost(shell)
	operator := model.OperatorPrincipal()
	group, err := service.CreateGroup(ctx, app.CreateGroupRequest{Context: operator, ID: "shell-group", Name: "Shell team"})
	require.NoError(t, err)
	config, err := service.SetGroupConfiguration(ctx, app.SetGroupConfigurationRequest{Principal: operator, GroupID: group.Group.ID, Environment: model.Environment{"SHARED": "group", "GROUP_ONLY": "literal $HOME\nline"}})
	require.NoError(t, err)
	workspace, err := service.CreateCheckout(ctx, app.CreateCheckoutRequest{Context: request(operator, "workspace"), ID: "workspace", Intent: model.WorkspaceIntent{Repository: "repo", IntendedPath: filepath.Join(t.TempDir(), "checkout"), BaseRevision: "main", Branch: "shell"}})
	require.NoError(t, err)
	req := app.StartShellRequest{Context: request(operator, "shell"), WorkspaceID: workspace.Workspace.ID, ExpectedRevision: workspace.Workspace.Revision, Sandbox: model.SandboxUnconfined, Group: &model.ShellGroupSelection{GroupID: group.Group.ID, Revision: group.Group.Revision, ConfigurationRevision: config.Revision}, Environment: model.Environment{"SHARED": "override"}}
	started, err := service.StartShell(ctx, req)
	require.NoError(t, err)
	require.Len(t, shell.prepared, 1)
	require.Equal(t, model.Environment{"SHARED": "override", "GROUP_ONLY": "literal $HOME\nline"}, shell.prepared[0].Environment)
	require.Equal(t, req.Group, started.Execution.Spec.ShellGroup)
	_, err = service.SetGroupConfiguration(ctx, app.SetGroupConfigurationRequest{Principal: operator, GroupID: group.Group.ID, ExpectedRevision: config.Revision, Environment: model.Environment{"CHANGED": "new"}})
	require.NoError(t, err)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	defer store.Close()
	// No host is configured: exact durable replay must not need fresh preparation.
	service = journeyService(store, newJourneyProvider(), workspaceHost)
	repeated, err := service.StartShell(ctx, req)
	require.NoError(t, err)
	require.True(t, repeated.Repeated)
	require.Equal(t, started.Execution.ID, repeated.Execution.ID)
	require.Equal(t, req.Group, repeated.Execution.Spec.ShellGroup)
	require.Equal(t, started.Execution.Spec.Environment, repeated.Execution.Spec.Environment)
	req.Environment = model.Environment{"SHARED": "changed intent"}
	_, err = service.StartShell(ctx, req)
	require.ErrorIs(t, err, app.ErrConflict)
	require.Len(t, shell.prepared, 1)
}

func TestShellEnvironmentAuthoritySurvivesReleaseAndRevocationBlocksRetry(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.db"))
	require.NoError(t, err)
	defer store.Close()
	shell := &groupShellHost{}
	service := journeyService(store, newJourneyProvider(), &journeyWorkspaceHost{}).WithShellHost(shell)
	operator := model.OperatorPrincipal()
	caller, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: operator, ID: "caller", Name: "Caller", Desired: model.DesiredConfiguration{Harness: "journey", Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}})
	require.NoError(t, err)
	workspace, err := service.CreateCheckout(ctx, app.CreateCheckoutRequest{Context: request(operator, "workspace"), ID: "workspace", Intent: model.WorkspaceIntent{Repository: "repo", IntendedPath: filepath.Join(t.TempDir(), "checkout"), BaseRevision: "main", Branch: "shell"}})
	require.NoError(t, err)
	grant, err := service.PutGrant(ctx, app.PutGrantRequest{Principal: operator, Grant: model.AuthorityGrant{ID: "shell-grant", Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: caller.Agent.ID}, Action: model.ActionStartShell, Resource: model.ResourceSelector{Kind: model.ResourceWorkspace, WorkspaceID: workspace.Workspace.ID}}})
	require.NoError(t, err)
	req := app.StartShellRequest{Context: request(model.AgentPrincipal(caller.Agent.ID), "shell"), WorkspaceID: workspace.Workspace.ID, ExpectedRevision: workspace.Workspace.Revision, Sandbox: model.SandboxUnconfined, Environment: model.Environment{"APP_VALUE": "literal"}}
	_, err = service.StartShell(ctx, req)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	require.Empty(t, shell.prepared)
	grant.Grant.Bounds.Environments = []model.Environment{req.Environment.Clone()}
	grant, err = service.PutGrant(ctx, app.PutGrantRequest{Principal: operator, Grant: grant.Grant, ExpectedRevision: grant.Grant.Revision})
	require.NoError(t, err)
	emptyReq := req
	emptyReq.Context.RequestID = "empty-shell"
	emptyReq.Environment = nil
	_, err = service.StartShell(ctx, emptyReq)
	require.ErrorIs(t, err, app.ErrUnauthorized, "exact nonempty bounds do not authorize an empty shell")
	started, err := service.StartShell(ctx, req)
	require.NoError(t, err)
	require.Equal(t, model.ExecutionRunning, started.Execution.State)
	require.Len(t, shell.prepared, 1)
	require.NoError(t, service.DeleteGrant(ctx, app.DeleteGrantRequest{Principal: operator, GrantID: grant.Grant.ID, ExpectedRevision: grant.Grant.Revision}))
	_, err = service.StartShell(ctx, req)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	require.Len(t, shell.prepared, 1)
}

type shellAdmissionRaceStore struct {
	*sqlite.Store
	before func()
}

func (s *shellAdmissionRaceStore) AdmitShell(ctx context.Context, in app.ShellAdmission) (app.AdmissionResult, error) {
	if s.before != nil {
		run := s.before
		s.before = nil
		run()
	}
	return s.Store.AdmitShell(ctx, in)
}

func TestGroupShellRechecksDefaultsAtTransactionalAdmission(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.db"))
	require.NoError(t, err)
	defer store.Close()
	wrapped := &shellAdmissionRaceStore{Store: store}
	shell := &groupShellHost{}
	service := journeyService(wrapped, newJourneyProvider(), &journeyWorkspaceHost{}).WithShellHost(shell)
	operator := model.OperatorPrincipal()
	group, err := service.CreateGroup(ctx, app.CreateGroupRequest{Context: operator, ID: "group", Name: "Group"})
	require.NoError(t, err)
	config, err := service.SetGroupConfiguration(ctx, app.SetGroupConfigurationRequest{Principal: operator, GroupID: group.Group.ID, Environment: model.Environment{"APP_VALUE": "before"}})
	require.NoError(t, err)
	workspace, err := service.CreateCheckout(ctx, app.CreateCheckoutRequest{Context: request(operator, "workspace"), ID: "workspace", Intent: model.WorkspaceIntent{IntendedPath: filepath.Join(t.TempDir(), "checkout")}})
	require.NoError(t, err)
	wrapped.before = func() {
		_, err := service.SetGroupConfiguration(ctx, app.SetGroupConfigurationRequest{Principal: operator, GroupID: group.Group.ID, ExpectedRevision: config.Revision, Environment: model.Environment{"APP_VALUE": "after"}})
		require.NoError(t, err)
	}
	_, err = service.StartShell(ctx, app.StartShellRequest{Context: request(operator, "shell"), WorkspaceID: workspace.Workspace.ID, ExpectedRevision: workspace.Workspace.Revision, Sandbox: model.SandboxUnconfined, Group: &model.ShellGroupSelection{GroupID: group.Group.ID, Revision: group.Group.Revision, ConfigurationRevision: config.Revision}})
	require.ErrorIs(t, err, app.ErrConflict)
	require.Empty(t, shell.prepared)
	snapshot, err := service.Snapshot(ctx, app.SnapshotRequest{Principal: operator})
	require.NoError(t, err)
	require.Empty(t, snapshot.Executions)
}
