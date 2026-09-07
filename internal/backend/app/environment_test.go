package app_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

// Regression retained from the independent launch retry authority review.
func TestLaunchEnvironmentRequiresExactAuthorityAndRemainsPinned(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	p := &launchBriefProvider{supported: true}
	service := app.New(store, providers.NewRegistry(p))
	operator := model.OperatorPrincipal()
	caller, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: operator, ID: "caller", Name: "Caller", Desired: model.DesiredConfiguration{Harness: p.Name(), Model: "fixture", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}})
	require.NoError(t, err)
	target, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: operator, ID: "target", Name: "Target", Desired: model.DesiredConfiguration{Environment: model.Environment{"LITERAL": "spaces $HOME\nmore=value"}, Harness: p.Name(), Model: "fixture", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}})
	require.NoError(t, err)
	grant, err := service.PutGrant(ctx, app.PutGrantRequest{Principal: operator, Grant: model.AuthorityGrant{
		ID: "launch-target", Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: caller.Agent.ID},
		Action: model.ActionLaunch, Resource: model.ResourceSelector{Kind: model.ResourceAgent, AgentID: target.Agent.ID},
		Bounds: model.ConfigurationBounds{Harnesses: []string{p.Name()}, Models: []string{"fixture"}, WorkingDirectoryRoots: []string{target.Agent.Desired.WorkingDirectory}, ApprovalModes: []model.ApprovalMode{model.ApprovalSupervised}, SandboxModes: []model.SandboxMode{model.SandboxWorkspaceWrite}},
	}})
	require.NoError(t, err)
	req := app.LaunchRequest{
		RequestContext: app.RequestContext{Principal: model.AgentPrincipal(caller.Agent.ID), RequestID: "launch"},
		Target:         app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: target.Agent.ID, ExpectedRevision: target.Agent.Revision}},
		InitialMessage: "Inspect the change.",
	}
	_, err = service.Launch(ctx, req)
	require.ErrorIs(t, err, app.ErrUnauthorized, "old grants do not authorize arbitrary new environment")
	grant.Grant.Bounds.Environments = []model.Environment{target.Agent.Desired.Environment.Clone()}
	grant, err = service.PutGrant(ctx, app.PutGrantRequest{Principal: operator, Grant: grant.Grant, ExpectedRevision: grant.Grant.Revision})
	require.NoError(t, err)
	launched, err := service.Launch(ctx, req)
	require.NoError(t, err)
	require.Equal(t, target.Agent.Desired.Environment, launched.Execution.Spec.Environment)
	executionID := launched.Execution.ID
	targetRead, err := store.Agent(ctx, target.Agent.ID)
	require.NoError(t, err)
	targetRead.Desired.Environment["LITERAL"] = "new desired value"
	_, err = service.UpdateAgent(ctx, app.UpdateAgentRequest{Context: operator, ID: targetRead.ID, ExpectedRevision: targetRead.Revision, Name: targetRead.Name, Notifications: targetRead.Notifications, Desired: targetRead.Desired})
	require.NoError(t, err)
	pinned, err := store.Execution(ctx, executionID)
	require.NoError(t, err)
	require.Equal(t, "spaces $HOME\nmore=value", pinned.Spec.Environment["LITERAL"])
	_, err = service.Launch(ctx, req)
	require.NoError(t, err, "exact retry authorizes pinned values, not mutable desired")
	require.NoError(t, service.DeleteGrant(ctx, app.DeleteGrantRequest{Principal: operator, GrantID: grant.Grant.ID, ExpectedRevision: grant.Grant.Revision}))

	_, err = service.Launch(ctx, req)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	require.Len(t, p.preparations, 1)
}
