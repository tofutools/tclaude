package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestAtomicGroupSpawnGrantCreatesOnlyItsNewMember(t *testing.T) {
	ctx := context.Background()
	store, _, in, _ := ownerLineageFixture(t)
	provider := &atomicSpawnProvider{&lineageCodexProvider{}}
	svc := app.New(store, providers.NewRegistry(provider))
	op := model.OperatorPrincipal()
	other, err := svc.UpdateGroup(ctx, app.UpdateGroupRequest{Context: op, ID: "other", ExpectedRevision: 1, Name: "Other"})
	require.NoError(t, err)
	grant, err := svc.PutGrant(ctx, app.PutGrantRequest{Principal: op, Grant: model.AuthorityGrant{ID: "spawn", Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: "owner"}, Action: model.ActionSpawnGroupMember, Resource: model.ResourceSelector{Kind: model.ResourceAll}, Scope: model.PermissionScope{"group": {"Other"}}}})
	require.NoError(t, err)
	profile, err := store.ConfigurationProfile(ctx, "worker", "")
	require.NoError(t, err)
	_, err = svc.SetGroupConfiguration(ctx, app.SetGroupConfigurationRequest{Principal: op, GroupID: "other", Profile: &profile.Revision.Ref})
	require.NoError(t, err)
	in.GroupID, in.ExpectedGroupRevision, in.ExpectedDefaultRevision = "other", other.Group.Revision, 1
	in.Launch = &app.GroupMemberLaunch{}
	standalone := in
	standalone.Launch = nil
	_, err = svc.CreateGroupMember(ctx, standalone)
	require.ErrorIs(t, err, app.ErrUnauthorized, "atomic spawn does not imply detached member creation")
	out, err := svc.CreateGroupMember(ctx, in)
	require.ErrorIs(t, err, app.ErrUncertain)
	require.NotNil(t, out.Operation)
	require.Equal(t, 1, provider.releases)
	require.Contains(t, out.Group.Members, model.AgentID("child"))
	require.NotContains(t, out.Group.Members, model.AgentID("owner"), "direct grant does not require ownership or existing membership")
	_, err = svc.Launch(ctx, app.LaunchRequest{RequestContext: app.RequestContext{Principal: in.Context.Principal, RequestID: "unrelated_launch"}, Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: "child", ExpectedRevision: out.Agent.Revision}}})
	require.ErrorIs(t, err, app.ErrUnauthorized, "atomic grant does not become launch authority on an existing agent")
	replay, err := svc.CreateGroupMember(ctx, in)
	require.NoError(t, err)
	require.True(t, replay.Repeated)
	require.Equal(t, out.Operation.Operation.ID, replay.Operation.Operation.ID)
	require.Equal(t, 1, provider.releases)
	require.NoError(t, svc.DeleteGrant(ctx, app.DeleteGrantRequest{Principal: op, GrantID: grant.Grant.ID, ExpectedRevision: grant.Grant.Revision}))
	_, err = svc.CreateGroupMember(ctx, in)
	require.ErrorIs(t, err, app.ErrUnauthorized)
}

func TestAtomicGroupSpawnGrantRetainsLineageAndDenialBoundaries(t *testing.T) {
	for _, mode := range []string{"sandbox", "approval", "stopped", "spawn-deny", "create-deny", "launch-deny"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			store, _, in, parent := ownerLineageFixture(t)
			provider := &atomicSpawnProvider{&lineageCodexProvider{}}
			svc := app.New(store, providers.NewRegistry(provider))
			op := model.OperatorPrincipal()
			profile, err := store.ConfigurationProfile(ctx, "worker", "")
			require.NoError(t, err)
			_, err = svc.SetGroupConfiguration(ctx, app.SetGroupConfigurationRequest{Principal: op, GroupID: "other", Profile: &profile.Revision.Ref})
			require.NoError(t, err)
			_, err = svc.PutGrant(ctx, app.PutGrantRequest{Principal: op, Grant: model.AuthorityGrant{ID: "spawn", Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: "owner"}, Action: model.ActionSpawnGroupMember, Resource: model.ResourceSelector{Kind: model.ResourceAll}}})
			require.NoError(t, err)
			in.GroupID, in.ExpectedGroupRevision, in.ExpectedDefaultRevision = "other", 1, 1
			in.Launch = &app.GroupMemberLaunch{}
			switch mode {
			case "sandbox":
				sandbox := model.SandboxUnconfined
				in.ConfigurationOverrides = &model.ConfigurationOptions{Sandbox: &sandbox}
			case "approval":
				approval := model.ApprovalOnRequest
				review := true
				in.ConfigurationOverrides = &model.ConfigurationOptions{Approval: &approval, AutoReview: &review}
			case "stopped":
				_, err = store.RecordRecovery(ctx, parent.ID, model.ExecutionExited, nil, parent.Evidence, time.Now())
				require.NoError(t, err)
			default:
				action := model.ActionSpawnGroupMember
				if mode == "create-deny" {
					action = model.ActionCreateGroupMember
				}
				if mode == "launch-deny" {
					action = model.ActionLaunch
				}
				_, err = svc.PutDenial(ctx, app.PutDenialRequest{Principal: op, Denial: model.AuthorityDenial{ID: "deny", Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: "owner"}, Action: action}})
				require.NoError(t, err)
			}
			_, err = svc.CreateGroupMember(ctx, in)
			require.ErrorIs(t, err, app.ErrUnauthorized)
			_, err = store.Agent(ctx, "child")
			require.ErrorIs(t, err, app.ErrNotFound)
			require.Zero(t, provider.releases)
		})
	}
}

type atomicSpawnProvider struct{ *lineageCodexProvider }

func (*atomicSpawnProvider) Capabilities() ports.ProviderCapabilities {
	return ports.ProviderCapabilities{HostSandbox: true, LaunchPolicy: &ports.PolicyRequirements{DefaultApproval: model.ApprovalNever, DefaultSandbox: model.SandboxWorkspaceWrite, SupportedApproval: []model.ApprovalMode{model.ApprovalNever, model.ApprovalOnRequest}, SupportedSandbox: []model.SandboxMode{model.SandboxWorkspaceWrite, model.SandboxUnconfined}}}
}

type atomicSpawnAdmissionRace struct {
	*sqlite.Store
	before func()
}

func (s *atomicSpawnAdmissionRace) AdmitLaunch(ctx context.Context, in app.LaunchAdmission) (app.AdmissionResult, error) {
	if in.GroupMember != nil {
		s.before()
	}
	return s.Store.AdmitLaunch(ctx, in)
}

func TestAtomicGroupSpawnRechecksRevocationAndParentAtAdmission(t *testing.T) {
	for _, changed := range []string{"grant", "parent"} {
		t.Run(changed, func(t *testing.T) {
			ctx := context.Background()
			store, svc, in, parent := ownerLineageFixture(t)
			op := model.OperatorPrincipal()
			profile, err := store.ConfigurationProfile(ctx, "worker", "")
			require.NoError(t, err)
			_, err = svc.SetGroupConfiguration(ctx, app.SetGroupConfigurationRequest{Principal: op, GroupID: "other", Profile: &profile.Revision.Ref})
			require.NoError(t, err)
			grant, err := svc.PutGrant(ctx, app.PutGrantRequest{Principal: op, Grant: model.AuthorityGrant{ID: "spawn", Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: "owner"}, Action: model.ActionSpawnGroupMember, Resource: model.ResourceSelector{Kind: model.ResourceAll}}})
			require.NoError(t, err)
			provider := &atomicSpawnProvider{&lineageCodexProvider{}}
			race := &atomicSpawnAdmissionRace{Store: store, before: func() {
				if changed == "grant" {
					require.NoError(t, store.DeleteGrant(ctx, grant.Grant.ID, grant.Grant.Revision))
				} else {
					_, err := store.RecordRecovery(ctx, parent.ID, model.ExecutionExited, nil, parent.Evidence, time.Now())
					require.NoError(t, err)
				}
			}}
			svc = app.New(race, providers.NewRegistry(provider))
			in.GroupID, in.ExpectedGroupRevision, in.ExpectedDefaultRevision = "other", 1, 1
			in.Launch = &app.GroupMemberLaunch{}
			_, err = svc.CreateGroupMember(ctx, in)
			require.ErrorIs(t, err, app.ErrUnauthorized)
			_, err = store.Agent(ctx, "child")
			require.ErrorIs(t, err, app.ErrNotFound)
			require.Zero(t, provider.releases)
		})
	}
}
