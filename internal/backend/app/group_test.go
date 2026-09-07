package app_test

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	backendsqlite "github.com/tofutools/tclaude/internal/backend/sqlite"
	"path/filepath"
	"testing"
)

func TestGroupUpdateRechecksExactAuthorityAndPreservesOwner(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := backendsqlite.Open(path)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	service := app.New(store, providers.NewRegistry())
	operator := model.OperatorPrincipal()
	for _, id := range []model.AgentID{"owner", "worker", "manager"} {
		_, err = service.CreateAgent(ctx, app.CreateAgentRequest{Context: operator, ID: id, Name: string(id), Desired: model.DesiredConfiguration{Harness: "codex", Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}})
		require.NoError(t, err)
	}
	_, err = service.CreateGroup(ctx, app.CreateGroupRequest{Context: operator, ID: "team", Name: "Team", Members: []model.AgentID{"owner", "worker"}, OwnerAgentID: "owner"})
	require.NoError(t, err)
	req := app.UpdateGroupRequest{Context: model.AgentPrincipal("manager"), ID: "team", ExpectedRevision: 1, Name: "Renamed", Members: []model.AgentID{"worker", "owner"}}
	_, err = service.UpdateGroup(ctx, req)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	grant, err := service.PutGrant(ctx, app.PutGrantRequest{Principal: operator, Grant: model.AuthorityGrant{ID: "manage", Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: "manager"}, Action: model.ActionManageMembership, Resource: model.ResourceSelector{Kind: model.ResourceGroup, GroupID: "team"}}})
	require.NoError(t, err)
	result, err := service.UpdateGroup(ctx, req)
	require.NoError(t, err)
	require.Equal(t, req.Members, result.Group.Members)
	require.Equal(t, model.AgentID("owner"), result.Group.OwnerAgentID)
	_, err = service.UpdateGroup(ctx, req)
	require.ErrorIs(t, err, app.ErrConflict)
	req.ExpectedRevision = result.Group.Revision
	req.Members = []model.AgentID{"worker"}
	_, err = service.UpdateGroup(ctx, req)
	require.ErrorIs(t, err, app.ErrConflict)
	require.NoError(t, store.DeleteGrant(ctx, grant.Grant.ID, grant.Grant.Revision))
	req.Members = []model.AgentID{"owner"}
	_, err = service.UpdateGroup(ctx, req)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	require.NoError(t, store.Close())
	store, err = backendsqlite.Open(path)
	require.NoError(t, err)
	persisted, err := store.Group(ctx, "team")
	require.NoError(t, err)
	require.Equal(t, result.Group, persisted)
}
