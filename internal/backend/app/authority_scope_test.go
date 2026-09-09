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

func TestNamedGrantScopeUsesCurrentGroupAndSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	svc := app.New(store, providers.NewRegistry())
	op := model.OperatorPrincipal()
	for _, id := range []model.AgentID{"owner", "manager"} {
		_, err = svc.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: id, Name: string(id), Desired: model.DesiredConfiguration{Harness: "codex", Model: "fixture", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}})
		require.NoError(t, err)
	}
	for _, id := range []model.GroupID{"team", "other"} {
		_, err = svc.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: id, Name: string(id), Members: []model.AgentID{"owner"}, OwnerAgentID: "owner"})
		require.NoError(t, err)
	}
	grant, err := svc.PutGrant(ctx, app.PutGrantRequest{Principal: op, Grant: model.AuthorityGrant{ID: "named", Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: "manager"}, Action: model.ActionManageMembership, Resource: model.ResourceSelector{Kind: model.ResourceAll}, Scope: model.PermissionScope{"group": {"team", "team"}}}})
	require.NoError(t, err)
	require.Equal(t, model.PermissionScope{"group": {"team"}}, grant.Grant.Scope)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	svc = app.New(store, providers.NewRegistry())
	state, err := svc.ListAuthority(ctx, app.ListAuthorityRequest{Principal: op})
	require.NoError(t, err)
	require.Equal(t, grant.Grant, state.Grants[0])
	req := app.UpdateGroupRequest{Context: model.AgentPrincipal("manager"), ID: "other", ExpectedRevision: 1, Name: "other", Members: []model.AgentID{"owner", "manager"}}
	_, err = svc.UpdateGroup(ctx, req)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	req.ID, req.Name = "team", "renamed"
	updated, err := svc.UpdateGroup(ctx, req)
	require.NoError(t, err)
	req.ExpectedRevision = updated.Group.Revision
	_, err = svc.UpdateGroup(ctx, req)
	require.ErrorIs(t, err, app.ErrUnauthorized, "scope follows the current group name, not the old name or membership")
	grant.Grant.Scope = model.PermissionScope{"group": {"renamed"}, "spawn_profile": {"reviewer"}}
	_, err = svc.PutGrant(ctx, app.PutGrantRequest{Principal: op, Grant: grant.Grant, ExpectedRevision: grant.Grant.Revision})
	require.NoError(t, err)
	_, err = svc.UpdateGroup(ctx, req)
	require.ErrorIs(t, err, app.ErrUnauthorized, "a dimension missing from the effect cannot be ignored")
}

func TestNamedGrantScopeKeepsTargetAndUnknownDimensionsRestrictive(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	defer store.Close()
	svc := app.New(store, providers.NewRegistry())
	op := model.OperatorPrincipal()
	for _, id := range []model.AgentID{"caller", "target", "other"} {
		_, err = svc.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: id, Name: string(id), Desired: model.DesiredConfiguration{Harness: "codex", Model: "fixture", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}})
		require.NoError(t, err)
	}
	in := app.PutGrantRequest{Principal: op, Grant: model.AuthorityGrant{ID: "targeted", Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: "caller"}, Action: model.ActionReadStatus, Resource: model.ResourceSelector{Kind: model.ResourceAll}, Scope: model.PermissionScope{"target_agent": {"target"}}}}
	saved, err := svc.PutGrant(ctx, in)
	require.NoError(t, err)
	for _, target := range []model.AgentID{"target", "other"} {
		_, err = svc.ReadStatus(ctx, app.ReadStatusRequest{Principal: model.AgentPrincipal("caller"), Target: model.ResourceSelector{Kind: model.ResourceAgent, AgentID: target}})
		if target == "target" {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, app.ErrUnauthorized)
		}
	}
	in.ExpectedRevision = saved.Grant.Revision
	in.Grant.Scope["future"] = []string{"x"}
	_, err = svc.PutGrant(ctx, in)
	require.ErrorIs(t, err, app.ErrInvalid)
	delete(in.Grant.Scope, "future")
	in.Grant.Scope["group"] = []string{"team"}
	_, err = svc.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: "team", Name: "team", Members: []model.AgentID{"target"}, OwnerAgentID: "target"})
	require.NoError(t, err)
	_, err = svc.PutGrant(ctx, in)
	require.NoError(t, err)
	_, err = svc.ReadStatus(ctx, app.ReadStatusRequest{Principal: model.AgentPrincipal("caller"), Target: model.ResourceSelector{Kind: model.ResourceAgent, AgentID: "target"}})
	require.ErrorIs(t, err, app.ErrUnauthorized, "membership does not invent a group context for an agent-wide action")
}
