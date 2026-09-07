package app_test

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	db "github.com/tofutools/tclaude/internal/backend/sqlite"
	"path/filepath"
	"testing"
)

func TestGroupHierarchyPersistsExactRetriesWithoutInheritingAuthority(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	store, err := db.Open(path)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	svc := app.New(store, providers.NewRegistry())
	op := model.OperatorPrincipal()
	for _, id := range []model.GroupID{"parent", "child", "leaf"} {
		_, err = svc.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: id, Name: "Duplicate"})
		require.NoError(t, err)
	}
	req := app.SetGroupParentRequest{Context: app.RequestContext{Principal: op, RequestID: "nest"}, ID: "child", ParentGroupID: "parent", ExpectedRevision: 1}
	result, err := svc.SetGroupParent(ctx, req)
	require.NoError(t, err)
	require.Equal(t, model.GroupID("parent"), result.ParentGroupID)
	leafReq := app.SetGroupParentRequest{Context: app.RequestContext{Principal: op, RequestID: "leaf"}, ID: "leaf", ParentGroupID: "child", ExpectedRevision: 1}
	_, err = svc.SetGroupParent(ctx, leafReq)
	require.NoError(t, err)
	_, err = svc.SetGroupParent(ctx, app.SetGroupParentRequest{Context: app.RequestContext{Principal: op, RequestID: "cycle"}, ID: "parent", ParentGroupID: "leaf", ExpectedRevision: 1})
	require.ErrorIs(t, err, app.ErrInvalid)
	changed := req
	changed.ParentGroupID = "leaf"
	_, err = svc.SetGroupParent(ctx, changed)
	require.ErrorIs(t, err, app.ErrConflict)
	changed.Context.RequestID = "stale"
	changed.ParentGroupID = ""
	_, err = svc.SetGroupParent(ctx, changed)
	require.ErrorIs(t, err, app.ErrConflict)
	updated, err := svc.UpdateGroup(ctx, app.UpdateGroupRequest{Context: op, ID: "child", Name: "Renamed", ExpectedRevision: result.Revision})
	require.NoError(t, err)
	require.Equal(t, result.ParentGroupID, updated.Group.ParentGroupID)
	require.NoError(t, store.Close())
	store, err = db.Open(path)
	require.NoError(t, err)
	svc = app.New(store, providers.NewRegistry())
	replay, err := svc.SetGroupParent(ctx, req)
	require.NoError(t, err)
	require.Equal(t, result, replay)
	current, err := store.Group(ctx, "child")
	require.NoError(t, err)
	require.Equal(t, "Renamed", current.Name)
	require.Equal(t, model.GroupID("parent"), current.ParentGroupID)
	_, err = svc.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: "manager", Name: "manager", Desired: model.DesiredConfiguration{Harness: "codex", Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}})
	require.NoError(t, err)
	_, err = svc.PutGrant(ctx, app.PutGrantRequest{Principal: op, Grant: model.AuthorityGrant{ID: "parent-grant", Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: "manager"}, Action: model.ActionManageMembership, Resource: model.ResourceSelector{Kind: model.ResourceGroup, GroupID: "parent"}}})
	require.NoError(t, err)
	_, err = svc.UpdateGroup(ctx, app.UpdateGroupRequest{Context: model.AgentPrincipal("manager"), ID: "child", Name: "Unauthorized", ExpectedRevision: current.Revision})
	require.ErrorIs(t, err, app.ErrUnauthorized)
	denied := req
	denied.Context.Principal = model.AgentPrincipal("manager")
	_, err = svc.SetGroupParent(ctx, denied)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	changed.Context.RequestID = "unnest"
	changed.ExpectedRevision = current.Revision
	unnested, err := svc.SetGroupParent(ctx, changed)
	require.NoError(t, err)
	require.Empty(t, unnested.ParentGroupID)
	leaf, err := store.Group(ctx, "leaf")
	require.NoError(t, err)
	require.Equal(t, model.GroupID("child"), leaf.ParentGroupID)
	state, err := svc.Snapshot(ctx, app.SnapshotRequest{Principal: op})
	require.NoError(t, err)
	require.Empty(t, state.Executions)
	for _, g := range state.Groups {
		require.Empty(t, g.Members)
	}
}
