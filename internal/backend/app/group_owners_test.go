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

func TestGroupMultipleOwnersPersistAndRevokeIndependently(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	service := app.New(store, providers.NewRegistry())
	op := model.OperatorPrincipal()
	for _, id := range []model.AgentID{"a", "b", "c"} {
		_, err = service.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: id, Name: string(id), Desired: model.DesiredConfiguration{Harness: "codex", Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}})
		require.NoError(t, err)
	}
	_, err = service.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: "group", Name: "Group", Members: []model.AgentID{"a", "b", "c"}})
	require.NoError(t, err)
	req := app.SetGroupOwnerRequest{Principal: op, GroupID: "group", OwnerAgentIDs: []model.AgentID{"a", "b"}, ExpectedGroupRevision: 1}
	result, err := service.SetGroupOwner(ctx, req)
	require.NoError(t, err)
	require.Equal(t, req.OwnerAgentIDs, result.Group.OwnerAgentIDs)
	_, err = service.SetGroupOwner(ctx, req)
	require.ErrorIs(t, err, app.ErrConflict)
	for _, id := range []model.AgentID{"a", "b"} {
		decision, err := store.Authorize(ctx, model.AuthorityRequest{Principal: model.AgentPrincipal(id), Action: model.ActionManageMembership, Resource: model.ResourceSelector{Kind: model.ResourceGroup, GroupID: "group"}}, time.Now())
		require.NoError(t, err)
		require.True(t, decision.Allowed)
	}
	_, err = service.UpdateGroup(ctx, app.UpdateGroupRequest{Context: op, ID: "group", Name: "Group", Members: []model.AgentID{"a", "c"}, ExpectedRevision: result.Group.Revision})
	require.ErrorIs(t, err, app.ErrConflict)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	service = app.New(store, providers.NewRegistry())
	persisted, err := store.Group(ctx, "group")
	require.NoError(t, err)
	require.Equal(t, result.Group, persisted)
	req.OwnerAgentIDs = []model.AgentID{"b"}
	req.ExpectedGroupRevision = persisted.Revision
	result, err = service.SetGroupOwner(ctx, req)
	require.NoError(t, err)
	require.Equal(t, []model.AgentID{"b"}, result.Group.OwnerAgentIDs)
	for _, id := range []model.AgentID{"a", "b"} {
		decision, err := store.Authorize(ctx, model.AuthorityRequest{Principal: model.AgentPrincipal(id), Action: model.ActionManageMembership, Resource: model.ResourceSelector{Kind: model.ResourceGroup, GroupID: "group"}}, time.Now())
		require.NoError(t, err)
		require.Equal(t, id == "b", decision.Allowed)
	}
	req.OwnerAgentIDs = []model.AgentID{}
	req.ExpectedGroupRevision = result.Group.Revision
	result, err = service.SetGroupOwner(ctx, req)
	require.NoError(t, err)
	require.Empty(t, result.Group.OwnerAgentIDs)
	require.Empty(t, result.Group.OwnerAgentID)
}
