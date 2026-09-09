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

func TestGroupSpawnAdmitsMemberAndLaunchTogetherAndReplaysAfterReopen(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "started", true: "uncertain"}[unknown], func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "state.sqlite")
			store, err := sqlite.Open(path)
			require.NoError(t, err)
			defer func() { _ = store.Close() }()
			provider := &launchBriefProvider{supported: true, unknown: unknown}
			svc := app.New(store, providers.NewRegistry(provider))
			op := model.OperatorPrincipal()
			desired := model.DesiredConfiguration{Harness: provider.Name(), Model: "fixture", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
			_, err = svc.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: "team", Name: "Team"})
			require.NoError(t, err)
			saved, err := svc.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{Context: app.RequestContext{Principal: op, RequestID: "profile"}, ID: "worker", RevisionID: "one", Name: "Worker", Desired: desired})
			require.NoError(t, err)
			_, err = svc.SetGroupConfiguration(ctx, app.SetGroupConfigurationRequest{Principal: op, GroupID: "team", Profile: &saved.Revision.Ref})
			require.NoError(t, err)
			in := app.CreateGroupMemberRequest{Context: app.RequestContext{Principal: op, RequestID: "spawn"}, ID: "child", GroupID: "team", Name: "Child", ExpectedGroupRevision: 1, ExpectedDefaultRevision: 1, Launch: &app.GroupMemberLaunch{InitialMessage: "Inspect the checkout."}}
			result, err := svc.CreateGroupMember(ctx, in)
			if unknown {
				require.ErrorIs(t, err, app.ErrUncertain)
			} else {
				require.NoError(t, err)
			}
			require.NotNil(t, result.Operation)
			agent, err := store.Agent(ctx, "child")
			require.NoError(t, err)
			require.Equal(t, result.Operation.Execution.ID, agent.PrimaryExecutionID)
			require.Equal(t, 1, provider.releases)
			require.Equal(t, in.Launch.InitialMessage, provider.preparation.InitialInput.Body)
			require.NoError(t, store.Close())
			store, err = sqlite.Open(path)
			require.NoError(t, err)
			svc = app.New(store, providers.NewRegistry(provider))
			// A changed profile does not alter or duplicate an already admitted spawn.
			desired.Model = "updated"
			_, err = svc.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{Context: app.RequestContext{Principal: op, RequestID: "edit"}, ID: "worker", RevisionID: "two", ExpectedRevision: saved.Profile.Revision, Name: "Worker", Desired: desired})
			require.NoError(t, err)
			replay, err := svc.CreateGroupMember(ctx, in)
			require.NoError(t, err)
			require.True(t, replay.Repeated)
			require.Equal(t, result.Operation.Operation.ID, replay.Operation.Operation.ID)
			require.Equal(t, result.Operation.Operation.State, replay.Operation.Operation.State)
			require.Equal(t, 1, provider.releases)
			for _, launch := range []*app.GroupMemberLaunch{nil, {InitialMessage: "Different"}} {
				changed := in
				changed.Launch = launch
				_, err = svc.CreateGroupMember(ctx, changed)
				require.ErrorIs(t, err, app.ErrConflict)
			}
		})
	}
}

func TestGroupSpawnRollsBackMemberWhenLaunchAuthorityIsAbsent(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	provider := &launchBriefProvider{supported: true}
	svc := app.New(store, providers.NewRegistry(provider))
	op := model.OperatorPrincipal()
	desired := model.DesiredConfiguration{Harness: provider.Name(), Model: "fixture", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	_, err = svc.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: "caller", Name: "Caller", Desired: desired})
	require.NoError(t, err)
	_, err = svc.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: "team", Name: "Team", Members: []model.AgentID{"caller"}})
	require.NoError(t, err)
	saved, err := svc.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{Context: app.RequestContext{Principal: op, RequestID: "profile"}, ID: "worker", RevisionID: "one", Name: "Worker", Desired: desired})
	require.NoError(t, err)
	_, err = svc.SetGroupConfiguration(ctx, app.SetGroupConfigurationRequest{Principal: op, GroupID: "team", Profile: &saved.Revision.Ref})
	require.NoError(t, err)
	bounds := model.ConfigurationBounds{Harnesses: []string{provider.Name()}, Models: []string{"fixture"}, WorkingDirectoryRoots: []string{desired.WorkingDirectory}, ApprovalModes: []model.ApprovalMode{desired.Approval}, SandboxModes: []model.SandboxMode{desired.Sandbox}}
	_, err = svc.PutGrant(ctx, app.PutGrantRequest{Principal: op, Grant: model.AuthorityGrant{ID: "create", Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: "caller"}, Action: model.ActionCreateGroupMember, Resource: model.ResourceSelector{Kind: model.ResourceGroup, GroupID: "team"}, Bounds: bounds}})
	require.NoError(t, err)
	in := app.CreateGroupMemberRequest{Context: app.RequestContext{Principal: model.AgentPrincipal("caller"), RequestID: "spawn"}, ID: "child", GroupID: "team", Name: "Child", ExpectedGroupRevision: 1, ExpectedDefaultRevision: 1, Launch: &app.GroupMemberLaunch{}}
	_, err = svc.CreateGroupMember(ctx, in)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	_, err = store.Agent(ctx, "child")
	require.ErrorIs(t, err, app.ErrNotFound)
	group, err := store.Group(ctx, "team")
	require.NoError(t, err)
	require.Equal(t, model.Revision(1), group.Revision)
	require.NotContains(t, group.Members, model.AgentID("child"))
	require.Empty(t, provider.preparations)
	// The refused combined request did not consume the creation receipt either.
	in.Launch = nil
	created, err := svc.CreateGroupMember(ctx, in)
	require.NoError(t, err)
	require.False(t, created.Repeated)
}

type groupSpawnRaceStore struct {
	*sqlite.Store
	before func()
}

func (s *groupSpawnRaceStore) AdmitLaunch(ctx context.Context, in app.LaunchAdmission) (app.AdmissionResult, error) {
	if s.before != nil {
		before := s.before
		s.before = nil
		before()
	}
	return s.Store.AdmitLaunch(ctx, in)
}

func TestGroupSpawnCurrentProfileFenceAndBoundedOwner(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	wrapped := &groupSpawnRaceStore{Store: store}
	provider := &launchBriefProvider{supported: true}
	svc := app.New(wrapped, providers.NewRegistry(provider))
	op := model.OperatorPrincipal()
	desired := model.DesiredConfiguration{Harness: provider.Name(), Model: "fixture", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	_, err = svc.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: "owner", Name: "Owner", Desired: desired})
	require.NoError(t, err)
	profile, err := svc.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{Context: app.RequestContext{Principal: op, RequestID: "profile"}, ID: "worker", RevisionID: "one", Name: "Worker", Desired: desired})
	require.NoError(t, err)
	for _, id := range []model.GroupID{"team", "other"} {
		_, err = svc.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: id, Name: string(id), Members: []model.AgentID{"owner"}})
		require.NoError(t, err)
		_, err = svc.SetGroupConfiguration(ctx, app.SetGroupConfigurationRequest{Principal: op, GroupID: id, Profile: &profile.Revision.Ref})
		require.NoError(t, err)
	}
	bounds := model.ConfigurationBounds{Harnesses: []string{provider.Name()}, Models: []string{"fixture"}, WorkingDirectoryRoots: []string{desired.WorkingDirectory}, ApprovalModes: []model.ApprovalMode{desired.Approval}, SandboxModes: []model.SandboxMode{desired.Sandbox}}
	owned, err := svc.SetGroupOwner(ctx, app.SetGroupOwnerRequest{Principal: op, GroupID: "team", OwnerAgentIDs: []model.AgentID{"owner"}, ExpectedGroupRevision: 1, Bounds: bounds})
	require.NoError(t, err)
	in := app.CreateGroupMemberRequest{Context: app.RequestContext{Principal: model.AgentPrincipal("owner"), RequestID: "spawn"}, ID: "child", GroupID: "team", Name: "Child", ExpectedGroupRevision: owned.Group.Revision, ExpectedDefaultRevision: 1, Launch: &app.GroupMemberLaunch{InitialMessage: "First work"}}
	wrapped.before = func() {
		_, err := svc.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{Context: app.RequestContext{Principal: op, RequestID: "edit"}, ID: "worker", RevisionID: "two", ExpectedRevision: profile.Profile.Revision, Name: "Renamed", Desired: desired})
		require.NoError(t, err)
	}
	_, err = svc.CreateGroupMember(ctx, in)
	require.ErrorIs(t, err, app.ErrConflict)
	_, err = store.Agent(ctx, "child")
	require.ErrorIs(t, err, app.ErrNotFound)
	require.Empty(t, provider.preparations)
	result, err := svc.CreateGroupMember(ctx, in)
	require.NoError(t, err)
	require.NotNil(t, result.Operation)
	require.Equal(t, 1, provider.releases)
	require.Equal(t, model.ConfigurationProfileRevisionID("two"), result.Agent.ConfigurationProfile.RevisionID)
	outside := in
	outside.GroupID = "other"
	outside.Context.RequestID = "outside"
	outside.ID = "outside"
	outside.ExpectedGroupRevision = 1
	_, err = svc.CreateGroupMember(ctx, outside)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	_, err = store.Agent(ctx, "outside")
	require.ErrorIs(t, err, app.ErrNotFound)
}
