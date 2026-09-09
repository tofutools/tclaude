package app_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestRoleScopesRemainCurrentForDirectAssignments(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	svc := app.New(store, providers.NewRegistry())
	op := model.OperatorPrincipal()
	desired := model.DesiredConfiguration{Harness: "codex", Model: "fixture", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	for _, id := range []model.AgentID{"caller", "target", "other"} {
		_, err = svc.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: id, Name: string(id), Desired: desired})
		require.NoError(t, err)
	}
	role, err := svc.PutRole(ctx, app.PutRoleRequest{Principal: op, Role: model.Role{ID: "scoped_role", Name: "Scoped role", Actions: []model.Action{model.ActionReadStatus}, Scopes: model.ActionScopes{model.ActionReadStatus: {"target_agent": {"target", "target"}}}}})
	require.NoError(t, err)
	_, err = svc.PutRoleAssignment(ctx, app.PutRoleAssignmentRequest{Principal: op, Assignment: model.RoleAssignment{RoleID: role.Role.ID, Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: "caller"}, Resource: model.ResourceSelector{Kind: model.ResourceAll}}})
	require.NoError(t, err)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	svc = app.New(store, providers.NewRegistry())
	state, err := store.AuthorityState(ctx)
	require.NoError(t, err)
	var reopened model.Role
	for _, candidate := range state.Roles {
		if candidate.ID == role.Role.ID {
			reopened = candidate
		}
	}
	require.Equal(t, model.ActionScopes{model.ActionReadStatus: {"target_agent": {"target"}}}, reopened.Scopes)
	for _, id := range []model.AgentID{"target", "other"} {
		decision, err := store.Authorize(ctx, model.AuthorityRequest{Principal: model.AgentPrincipal("caller"), Action: model.ActionReadStatus, Resource: model.ResourceSelector{Kind: model.ResourceAgent, AgentID: id}}, time.Now())
		require.NoError(t, err)
		require.Equal(t, id == "target", decision.Allowed)
	}

	olderEditor := reopened
	olderEditor.Name = "Renamed with older client"
	olderEditor.Scopes = nil
	preserved, err := svc.PutRole(ctx, app.PutRoleRequest{Principal: op, Role: olderEditor, ExpectedRevision: olderEditor.Revision})
	require.NoError(t, err)
	require.Equal(t, reopened.Scopes, preserved.Role.Scopes)
	reopened = preserved.Role
	reopened.Scopes[model.ActionReadStatus] = model.PermissionScope{"target_agent": {"other"}}
	updated, err := svc.PutRole(ctx, app.PutRoleRequest{Principal: op, Role: reopened, ExpectedRevision: reopened.Revision})
	require.NoError(t, err)
	decision, err := store.Authorize(ctx, model.AuthorityRequest{Principal: model.AgentPrincipal("caller"), Action: model.ActionReadStatus, Resource: model.ResourceSelector{Kind: model.ResourceAgent, AgentID: "target"}}, time.Now())
	require.NoError(t, err)
	require.False(t, decision.Allowed)
	updated.Role.Scopes[model.ActionSendMessage] = model.PermissionScope{"target_agent": {"target"}}
	_, err = svc.PutRole(ctx, app.PutRoleRequest{Principal: op, Role: updated.Role, ExpectedRevision: updated.Role.Revision})
	require.ErrorIs(t, err, app.ErrInvalid)
	updated.Role.Scopes = model.ActionScopes{}
	cleared, err := svc.PutRole(ctx, app.PutRoleRequest{Principal: op, Role: updated.Role, ExpectedRevision: updated.Role.Revision})
	require.NoError(t, err)
	require.Empty(t, cleared.Role.Scopes)

}
