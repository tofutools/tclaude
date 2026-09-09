package app_test

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
	"path/filepath"
	"testing"
)

func TestAutoReviewPersistsOnAgentAndAdmittedExecution(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "backend.sqlite")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	provider := &fastModeProvider{newFakeProvider()}
	service := app.New(store, providers.NewRegistry(provider))
	operator := model.OperatorPrincipal()
	desired := model.DesiredConfiguration{Harness: "codex", Model: "first", AutoReview: true, WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}
	req := app.SaveConfigurationProfileRequest{Context: effect(operator, "profile_first"), ID: "profile", RevisionID: "first", Name: "Worker", Desired: desired}
	profile, err := service.SaveConfigurationProfile(ctx, req)
	require.NoError(t, err)
	mixed := app.CreateAgentRequest{Context: operator, ID: "worker", Name: "Worker", Desired: desired, ConfigurationProfile: &profile.Revision.Ref}
	_, err = service.CreateAgent(ctx, mixed)
	require.ErrorIs(t, err, app.ErrInvalid)
	mixed.Desired = model.DesiredConfiguration{}
	worker, err := service.CreateAgent(ctx, mixed)
	require.NoError(t, err)
	require.Equal(t, desired, worker.Agent.Desired)
	launched, err := service.Launch(ctx, app.LaunchRequest{RequestContext: effect(operator, "launch_selected"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: worker.Agent.ID, ExpectedRevision: worker.Agent.Revision}}})
	require.NoError(t, err)
	require.Equal(t, &profile.Revision.Ref, launched.Execution.Spec.ConfigurationProfile)
	require.Equal(t, true, provider.lastPreparation.Spec.AutoReview)
	req.Context.RequestID = "profile_second"
	req.RevisionID = "second"
	req.ExpectedRevision = 1
	req.Desired.Model = "second"
	req.Desired.AutoReview = false
	newer, err := service.SaveConfigurationProfile(ctx, req)
	require.NoError(t, err)
	current, err := store.Agent(ctx, worker.Agent.ID)
	require.NoError(t, err)
	updated, err := service.UpdateAgent(ctx, app.UpdateAgentRequest{Context: operator, ID: current.ID, ExpectedRevision: current.Revision, Name: current.Name, ConfigurationProfile: &newer.Revision.Ref})
	require.NoError(t, err)
	require.Equal(t, "second", updated.Agent.Desired.Model)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	durableAgent, err := store.Agent(ctx, worker.Agent.ID)
	require.NoError(t, err)
	require.Equal(t, &newer.Revision.Ref, durableAgent.ConfigurationProfile)
	require.Equal(t, false, durableAgent.Desired.AutoReview)
	execution, err := store.Execution(ctx, launched.Execution.ID)
	require.NoError(t, err)
	require.Equal(t, "first", execution.Spec.Model)
	require.Equal(t, true, execution.Spec.AutoReview)
	require.Equal(t, &profile.Revision.Ref, execution.Spec.ConfigurationProfile)
}

func TestAutoReviewRequiresExplicitAuthorityOnLaunchAndReplay(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	p := &fastModeProvider{newFakeProvider()}
	service := app.New(store, providers.NewRegistry(p))
	op := model.OperatorPrincipal()
	desired := model.DesiredConfiguration{Harness: "codex", Model: "fixture", WorkingDirectory: t.TempDir(), Approval: model.ApprovalOnRequest, Sandbox: model.SandboxWorkspaceWrite}
	caller, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: "caller", Name: "Caller", Desired: desired})
	require.NoError(t, err)
	desired.AutoReview = true
	target, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: "target", Name: "Target", Desired: desired})
	require.NoError(t, err)
	grant, err := service.PutGrant(ctx, app.PutGrantRequest{Principal: op, Grant: model.AuthorityGrant{ID: "launch", Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: caller.Agent.ID}, Action: model.ActionLaunch, Resource: model.ResourceSelector{Kind: model.ResourceAgent, AgentID: target.Agent.ID}, Bounds: model.ConfigurationBounds{Harnesses: []string{"codex"}, Models: []string{"fixture"}, WorkingDirectoryRoots: []string{desired.WorkingDirectory}, ApprovalModes: []model.ApprovalMode{desired.Approval}, SandboxModes: []model.SandboxMode{desired.Sandbox}}}})
	require.NoError(t, err)
	request := app.LaunchRequest{RequestContext: effect(model.AgentPrincipal(caller.Agent.ID), "launch"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: target.Agent.ID, ExpectedRevision: target.Agent.Revision}}}
	_, err = service.Launch(ctx, request)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	grant.Grant.Bounds.AutoReview = true
	grant, err = service.PutGrant(ctx, app.PutGrantRequest{Principal: op, Grant: grant.Grant, ExpectedRevision: grant.Grant.Revision})
	require.NoError(t, err)
	launched, err := service.Launch(ctx, request)
	require.NoError(t, err)
	require.True(t, launched.Execution.Spec.AutoReview)
	grant.Grant.Bounds.AutoReview = false
	_, err = service.PutGrant(ctx, app.PutGrantRequest{Principal: op, Grant: grant.Grant, ExpectedRevision: grant.Grant.Revision})
	require.NoError(t, err)
	_, err = service.Launch(ctx, request)
	require.ErrorIs(t, err, app.ErrUnauthorized)
}
