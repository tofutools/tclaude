package app_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestAuthorityDenialOverridesOwnerAndDirectGrantUntilRemoved(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	service := app.New(store, providers.NewRegistry())
	op := model.OperatorPrincipal()
	for _, id := range []model.AgentID{"owner", "worker"} {
		_, err = service.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: id, Name: string(id), Desired: model.DesiredConfiguration{Harness: "codex", Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}})
		require.NoError(t, err)
	}
	group, err := service.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: "team", Name: "Team", Members: []model.AgentID{"owner", "worker"}, OwnerAgentID: "owner"})
	require.NoError(t, err)
	subject := model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: "owner"}
	resource := model.ResourceSelector{Kind: model.ResourceGroup, GroupID: "team"}
	_, err = service.PutGrant(ctx, app.PutGrantRequest{Principal: op, Grant: model.AuthorityGrant{ID: "direct", Subject: subject, Action: model.ActionManageMembership, Resource: resource}})
	require.NoError(t, err)
	request := app.AuthorityExplanationRequest{Principal: model.AgentPrincipal("owner"), Action: model.ActionManageMembership, Resource: resource}
	before, err := service.ExplainAuthority(ctx, request)
	require.NoError(t, err)
	require.True(t, before.Decision.Allowed)
	deny := app.PutDenialRequest{Principal: op, Denial: model.AuthorityDenial{ID: "deny", Subject: subject, Action: model.ActionManageMembership}}
	saved, err := service.PutDenial(ctx, deny)
	require.NoError(t, err)
	deny.Principal = model.AgentPrincipal("owner")
	_, err = service.PutDenial(ctx, deny)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	_, err = service.UpdateGroup(ctx, app.UpdateGroupRequest{Context: model.AgentPrincipal("owner"), ID: "team", Name: "Changed", Members: group.Group.Members, ExpectedRevision: group.Group.Revision})
	require.ErrorIs(t, err, app.ErrUnauthorized)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	defer store.Close()
	service = app.New(store, providers.NewRegistry())
	denied, err := service.ExplainAuthority(ctx, request)
	require.NoError(t, err)
	require.False(t, denied.Decision.Allowed)
	require.Equal(t, model.AuthorityDenied, denied.Decision.SourceKind)
	require.Equal(t, "deny", denied.Decision.SourceID)
	unchanged, err := store.Group(ctx, "team")
	require.NoError(t, err)
	require.Equal(t, group.Group.Revision, unchanged.Revision)
	operatorRequest := request
	operatorRequest.Principal = op
	allowed, err := service.ExplainAuthority(ctx, operatorRequest)
	require.NoError(t, err)
	require.True(t, allowed.Decision.Allowed)
	err = service.DeleteDenial(ctx, app.DeleteDenialRequest{Principal: op, DenialID: saved.Denial.ID, ExpectedRevision: 99})
	require.ErrorIs(t, err, app.ErrConflict)
	require.NoError(t, service.DeleteDenial(ctx, app.DeleteDenialRequest{Principal: op, DenialID: saved.Denial.ID, ExpectedRevision: saved.Denial.Revision}))
	after, err := service.ExplainAuthority(ctx, request)
	require.NoError(t, err)
	require.True(t, after.Decision.Allowed)
	require.NoError(t, service.DeleteGrant(ctx, app.DeleteGrantRequest{Principal: op, GrantID: "direct", ExpectedRevision: 1}))
	owner, err := service.ExplainAuthority(ctx, request)
	require.NoError(t, err)
	require.True(t, owner.Decision.Allowed)
	require.Equal(t, model.AuthorityRole, owner.Decision.SourceKind)
}

func TestAuthorityDenialSuppressesDefaultAndTransactionalConfiguration(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	defer store.Close()
	service := testAccessService(store, newAccessProvider())
	op := model.OperatorPrincipal()
	target := createAgent(t, ctx, service, op, "worker")
	caller := model.AgentPrincipal(target.ID)
	subject := model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: target.ID}
	self := model.ResourceSelector{Kind: model.ResourceSelf}
	before, err := service.ExplainAuthority(ctx, app.AuthorityExplanationRequest{Principal: caller, Action: model.ActionReadStatus, Resource: self})
	require.NoError(t, err)
	require.True(t, before.Decision.Allowed)
	for _, action := range []model.Action{model.ActionReadStatus, model.ActionUpdateConfiguration} {
		_, err = service.PutDenial(ctx, app.PutDenialRequest{Principal: op, Denial: model.AuthorityDenial{ID: model.DenialID("deny_" + strings.ReplaceAll(string(action), ".", "_")), Subject: subject, Action: action}})
		require.NoError(t, err)
	}
	after, err := service.ExplainAuthority(ctx, app.AuthorityExplanationRequest{Principal: caller, Action: model.ActionReadStatus, Resource: self})
	require.NoError(t, err)
	require.False(t, after.Decision.Allowed)
	// A stale/precomputed application allow must not bypass transaction-time denial.
	wrapped := testAccessService(&permissiveAuthorizeStore{Store: store}, newAccessProvider())
	_, err = wrapped.UpdateAgent(ctx, app.UpdateAgentRequest{Context: caller, ID: target.ID, ExpectedRevision: target.Revision, Name: "Forbidden", Desired: target.Desired})
	require.ErrorIs(t, err, app.ErrUnauthorized)
	persisted, err := store.Agent(ctx, target.ID)
	require.NoError(t, err)
	require.Equal(t, target.Name, persisted.Name)
	require.Equal(t, target.Revision, persisted.Revision)
	for _, denial := range []model.AuthorityDenial{
		{ID: "bad", Subject: subject, Action: "unknown"},
		{ID: "bad", Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: "bad id"}, Action: model.ActionReadStatus},
		{ID: "bad", Subject: model.AuthoritySubject{Kind: model.AuthorityOperator}, Action: model.ActionReadStatus},
	} {
		_, err = service.PutDenial(ctx, app.PutDenialRequest{Principal: op, Denial: denial})
		require.ErrorIs(t, err, app.ErrInvalid)
	}
}
