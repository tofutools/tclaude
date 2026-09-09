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

func TestGroupMemberCreationRequiresScopedCurrentConfigurationAuthority(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry())
	op := model.OperatorPrincipal()
	desired := model.DesiredConfiguration{Harness: "codex", Model: "worker", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	_, err = service.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: "caller", Name: "Caller", Desired: desired})
	require.NoError(t, err)
	caller := model.AgentPrincipal("caller")
	profile, err := service.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{Context: app.RequestContext{Principal: op, RequestID: "profile"}, ID: "worker", RevisionID: "worker_1", Name: "Worker", Desired: desired})
	require.NoError(t, err)
	for _, id := range []model.GroupID{"team", "other"} {
		_, err = service.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: id, Name: string(id)})
		require.NoError(t, err)
		_, err = service.SetGroupConfiguration(ctx, app.SetGroupConfigurationRequest{Principal: op, GroupID: id, Profile: &profile.Revision.Ref})
		require.NoError(t, err)
	}
	request := app.CreateGroupMemberRequest{Context: app.RequestContext{Principal: caller, RequestID: "create"}, GroupID: "team", ID: "child", Name: "Child", ExpectedGroupRevision: 1, ExpectedDefaultRevision: 1}
	_, err = service.CreateGroupMember(ctx, request)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	_, err = service.GetGroupConfiguration(ctx, caller, "team")
	require.ErrorIs(t, err, app.ErrUnauthorized)
	grant, err := service.PutGrant(ctx, app.PutGrantRequest{Principal: op, Grant: model.AuthorityGrant{ID: "create_member", Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: "caller"}, Action: model.ActionCreateGroupMember, Resource: model.ResourceSelector{Kind: model.ResourceGroup, GroupID: "team"}, Bounds: model.ConfigurationBounds{Harnesses: []string{"codex"}, Models: []string{"worker"}, WorkingDirectoryRoots: []string{desired.WorkingDirectory}, ApprovalModes: []model.ApprovalMode{desired.Approval}, SandboxModes: []model.SandboxMode{desired.Sandbox}}}})
	require.NoError(t, err)
	_, err = service.GetGroupConfiguration(ctx, caller, "team")
	require.NoError(t, err)
	outside := request
	outside.GroupID = "other"
	_, err = service.CreateGroupMember(ctx, outside)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	outside = request
	outside.Environment = model.Environment{"EXTRA": "not permitted"}
	_, err = service.CreateGroupMember(ctx, outside)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	_, err = store.Agent(ctx, "child")
	require.ErrorIs(t, err, app.ErrNotFound)
	created, err := service.CreateGroupMember(ctx, request)
	require.NoError(t, err)
	require.Contains(t, created.Group.Members, model.AgentID("child"))
	require.Empty(t, created.Agent.PrimaryExecutionID)
	expected := desired
	expected.HostSandbox = model.SandboxInGroup(nil, "team")
	require.True(t, created.Agent.Desired.Equal(expected))
	// A later profile edit governs new effects but not the original receipt.
	wider := desired
	wider.Model = "outside"
	_, err = service.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{Context: app.RequestContext{Principal: op, RequestID: "edit_profile"}, ID: "worker", RevisionID: "worker_2", Name: "Worker", Desired: wider, ExpectedRevision: profile.Profile.Revision})
	require.NoError(t, err)
	fresh := request
	fresh.Context.RequestID = "fresh"
	fresh.ID = "outside_child"
	fresh.ExpectedGroupRevision = created.Group.Revision
	_, err = service.CreateGroupMember(ctx, fresh)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	_, err = store.Agent(ctx, "outside_child")
	require.ErrorIs(t, err, app.ErrNotFound)
	repeated, err := service.CreateGroupMember(ctx, request)
	require.NoError(t, err)
	require.True(t, repeated.Repeated)
	require.Equal(t, created.Agent, repeated.Agent)
	changed := request
	changed.Name = "Changed"
	_, err = service.CreateGroupMember(ctx, changed)
	require.ErrorIs(t, err, app.ErrConflict)
	require.NoError(t, service.DeleteGrant(ctx, app.DeleteGrantRequest{Principal: op, GrantID: grant.Grant.ID, ExpectedRevision: grant.Grant.Revision}))
	_, err = service.CreateGroupMember(ctx, request)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	retained, err := store.Agent(ctx, "child")
	require.NoError(t, err)
	require.Equal(t, created.Agent, retained)
}

func TestGroupOwnerCreatesMemberWithinSharedLimitsWithoutDirectGrant(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry())
	op := model.OperatorPrincipal()
	desired := model.DesiredConfiguration{Harness: "codex", Model: "worker", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	_, err = service.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: "owner", Name: "Owner", Desired: desired})
	require.NoError(t, err)
	profile, err := service.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{Context: app.RequestContext{Principal: op, RequestID: "profile"}, ID: "worker", RevisionID: "one", Name: "Worker", Desired: desired})
	require.NoError(t, err)
	for _, id := range []model.GroupID{"team", "other"} {
		_, err = service.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: id, Name: string(id), Members: []model.AgentID{"owner"}})
		require.NoError(t, err)
		_, err = service.SetGroupConfiguration(ctx, app.SetGroupConfigurationRequest{Principal: op, GroupID: id, Profile: &profile.Revision.Ref})
		require.NoError(t, err)
	}
	owned, err := service.SetGroupOwner(ctx, app.SetGroupOwnerRequest{Principal: op, GroupID: "team", OwnerAgentIDs: []model.AgentID{"owner"}, ExpectedGroupRevision: 1, Bounds: model.ConfigurationBounds{Harnesses: []string{"codex"}, Models: []string{"worker"}, WorkingDirectoryRoots: []string{desired.WorkingDirectory}, ApprovalModes: []model.ApprovalMode{desired.Approval}, SandboxModes: []model.SandboxMode{desired.Sandbox}}})
	require.NoError(t, err)
	caller := model.AgentPrincipal("owner")
	_, err = service.GetGroupConfiguration(ctx, caller, "team")
	require.NoError(t, err)
	_, err = service.GetGroupConfiguration(ctx, caller, "other")
	require.ErrorIs(t, err, app.ErrUnauthorized)
	request := app.CreateGroupMemberRequest{Context: app.RequestContext{Principal: caller, RequestID: "create"}, GroupID: "team", ID: "child", Name: "Child", ExpectedGroupRevision: owned.Group.Revision, ExpectedDefaultRevision: 1}
	out, err := service.CreateGroupMember(ctx, request)
	require.NoError(t, err)
	require.Contains(t, out.Group.Members, model.AgentID("child"))
	request.Context.RequestID = "outside_group"
	request.GroupID = "other"
	request.ExpectedGroupRevision = 1
	request.ID = "outside"
	_, err = service.CreateGroupMember(ctx, request)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	request.Context.RequestID = "outside_environment"
	request.GroupID = "team"
	request.ExpectedGroupRevision = out.Group.Revision
	request.Environment = model.Environment{"EXTRA": "outside bounds"}
	_, err = service.CreateGroupMember(ctx, request)
	require.ErrorIs(t, err, app.ErrUnauthorized)
}
