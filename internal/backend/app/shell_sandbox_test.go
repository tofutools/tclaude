package app_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

type sandboxShellHost struct {
	journeyShellHost
	prepared []ports.ShellPreparationRequest
}
type sandboxPreparedShell struct{ journeyPreparedShell }

func (h *sandboxShellHost) PrepareShell(_ context.Context, req ports.ShellPreparationRequest) (ports.PreparedShell, error) {
	h.prepared = append(h.prepared, req)
	return &sandboxPreparedShell{journeyPreparedShell{request: req}}, nil
}
func (p *sandboxPreparedShell) Describe() ports.ShellPreparedDescription {
	result := p.journeyPreparedShell.Describe()
	result.HostSandboxPolicyHash = p.request.HostSandboxPolicy.ContentHash
	return result
}

func TestShellSandboxRetainsAdmissionAndCurrentAuthorityAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "backend.db")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	paths, err := host.NewSandboxPathInspector([]string{t.TempDir()})
	require.NoError(t, err)
	shell := &sandboxShellHost{}
	workspaceHost := &journeyWorkspaceHost{}
	service := journeyService(store, newJourneyProvider(), workspaceHost).WithShellHost(shell).WithSandboxPathInspector(paths)
	operator := model.OperatorPrincipal()
	caller, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: operator, ID: "caller", Name: "Caller", Desired: model.DesiredConfiguration{Harness: "journey", Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}})
	require.NoError(t, err)
	profile, err := service.SaveSandboxProfile(ctx, app.SaveSandboxProfileRequest{Context: request(operator, "profile"), ID: "policy", Name: "Policy", Policy: model.SandboxPolicy{FilesystemRoot: model.SandboxRootSeparate, Environment: model.Environment{"VALUE": "pinned"}}})
	require.NoError(t, err)
	selected, err := service.ResolveLaunchSandbox(ctx, operator, []model.SandboxScopeSelection{{Scope: model.SandboxScopeExplicit, Ref: profile.Revision.Ref}})
	require.NoError(t, err)
	workspace, err := service.CreateCheckout(ctx, app.CreateCheckoutRequest{Context: request(operator, "workspace"), ID: "workspace", Intent: model.WorkspaceIntent{Repository: "repo", IntendedPath: filepath.Join(t.TempDir(), "checkout"), BaseRevision: "main", Branch: "shell"}})
	require.NoError(t, err)
	grant, err := service.PutGrant(ctx, app.PutGrantRequest{Principal: operator, Grant: model.AuthorityGrant{ID: "grant", Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: caller.Agent.ID}, Action: model.ActionStartShell, Resource: model.ResourceSelector{Kind: model.ResourceWorkspace, WorkspaceID: workspace.Workspace.ID}, Bounds: model.ConfigurationBounds{HostSandboxPolicies: []string{selected.PolicyHash}}}})
	require.NoError(t, err)
	req := app.StartShellRequest{Context: request(model.AgentPrincipal(caller.Agent.ID), "shell"), WorkspaceID: workspace.Workspace.ID, ExpectedRevision: workspace.Workspace.Revision, Sandbox: model.SandboxUnconfined, HostSandbox: &selected}
	unconfined := req
	unconfined.HostSandbox = nil
	_, err = service.StartShell(ctx, unconfined)
	require.ErrorIs(t, err, app.ErrUnauthorized, "confined grant cannot be used without its selected policy")
	require.Empty(t, shell.prepared)
	started, err := service.StartShell(ctx, req)
	require.NoError(t, err)
	require.Equal(t, &selected, started.Execution.Spec.HostSandbox)
	require.Len(t, shell.prepared, 1)
	require.Equal(t, selected.PolicyHash, shell.prepared[0].HostSandboxPolicy.ContentHash)
	require.Equal(t, "pinned", shell.prepared[0].HostSandboxPolicy.Composition.Values.Environment["VALUE"])
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	defer store.Close()
	service = journeyService(store, newJourneyProvider(), workspaceHost)
	// No path inspector or shell preparation is available after this reopen.
	repeated, err := service.StartShell(ctx, req)
	require.NoError(t, err)
	require.True(t, repeated.Repeated)
	require.Equal(t, started.Execution.ID, repeated.Execution.ID)
	require.Equal(t, &selected, repeated.Execution.Spec.HostSandbox)
	require.NoError(t, service.DeleteGrant(ctx, app.DeleteGrantRequest{Principal: operator, GrantID: grant.Grant.ID, ExpectedRevision: grant.Grant.Revision}))
	_, err = service.StartShell(ctx, req)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	require.Len(t, shell.prepared, 1)
}
