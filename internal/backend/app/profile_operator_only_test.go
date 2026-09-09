package app_test

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
	"testing"
	"time"
)

func TestProfileOperatorOnlySavePreservesOmissionAndExactIntent(t *testing.T) {
	ctx := context.Background()
	_, service, _ := regressionService(t)
	op := model.OperatorPrincipal()
	restricted := true
	req := app.SaveConfigurationProfileRequest{Context: effect(op, "restricted"), ID: "restricted", RevisionID: "one", Name: "Restricted", Options: &model.ConfigurationOptions{}, OperatorOnly: &restricted}
	first, err := service.SaveConfigurationProfile(ctx, req)
	require.NoError(t, err)
	require.True(t, first.Profile.OperatorOnly)
	replay, err := service.SaveConfigurationProfile(ctx, req)
	require.NoError(t, err)
	require.Equal(t, first, replay)
	restricted = false
	_, err = service.SaveConfigurationProfile(ctx, req)
	require.ErrorIs(t, err, app.ErrConflict)
	req.Context.RequestID = "edit"
	req.RevisionID = "two"
	req.ExpectedRevision = 1
	req.OperatorOnly = nil
	kept, err := service.SaveConfigurationProfile(ctx, req)
	require.NoError(t, err)
	require.True(t, kept.Profile.OperatorOnly)
	req.Context.RequestID = "clear"
	req.RevisionID = "three"
	req.ExpectedRevision = 2
	req.OperatorOnly = &restricted
	cleared, err := service.SaveConfigurationProfile(ctx, req)
	require.NoError(t, err)
	require.False(t, cleared.Profile.OperatorOnly)
}

func TestOperatorOnlyProfileBlocksAgentMemberCreationAtSelectedAndGlobalTiers(t *testing.T) {
	for _, tier := range []string{"selected", "global"} {
		t.Run(tier, func(t *testing.T) {
			ctx := context.Background()
			store, service, now := regressionService(t)
			op := model.OperatorPrincipal()
			desired := model.DesiredConfiguration{Harness: "codex", Model: "worker", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
			_, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: "caller", Name: "Caller", Desired: desired})
			require.NoError(t, err)
			restricted := true
			req := app.SaveConfigurationProfileRequest{Context: effect(op, "profile"), ID: "selected", RevisionID: "one", Name: "Selected", Desired: desired}
			if tier == "selected" {
				req.OperatorOnly = &restricted
			}
			selected, err := service.SaveConfigurationProfile(ctx, req)
			require.NoError(t, err)
			restrictedReq := req
			if tier == "global" {
				restrictedReq.ID = "global"
				restrictedReq.Name = "Global"
				restrictedReq.Context.RequestID = "global"
				restrictedReq.OperatorOnly = &restricted
				global, err := service.SaveConfigurationProfile(ctx, restrictedReq)
				require.NoError(t, err)
				_, err = service.SaveConfigurationDefaults(ctx, app.SaveConfigurationDefaultsRequest{Context: effect(op, "defaults"), Global: &global.Revision.Ref})
				require.NoError(t, err)
			}
			_, err = service.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: "team", Name: "Team"})
			require.NoError(t, err)
			_, err = service.SetGroupConfiguration(ctx, app.SetGroupConfigurationRequest{Principal: op, GroupID: "team", Profile: &selected.Revision.Ref})
			require.NoError(t, err)
			_, err = service.PutGrant(ctx, app.PutGrantRequest{Principal: op, Grant: model.AuthorityGrant{ID: "create", Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: "caller"}, Action: model.ActionCreateGroupMember, Resource: model.ResourceSelector{Kind: model.ResourceGroup, GroupID: "team"}, Bounds: model.ConfigurationBounds{Harnesses: []string{"codex"}, Models: []string{"worker"}, WorkingDirectoryRoots: []string{desired.WorkingDirectory}, ApprovalModes: []model.ApprovalMode{desired.Approval}, SandboxModes: []model.SandboxMode{desired.Sandbox}}}})
			require.NoError(t, err)
			create := app.CreateGroupMemberRequest{Context: effect(model.AgentPrincipal("caller"), "create_agent"), GroupID: "team", ID: "child", Name: "Child", ExpectedGroupRevision: 1, ExpectedDefaultRevision: 1}
			_, err = service.CreateGroupMember(ctx, create)
			require.ErrorIs(t, err, app.ErrUnauthorized)
			_, err = store.Agent(ctx, "child")
			require.ErrorIs(t, err, app.ErrNotFound)
			human := create
			human.Context = effect(op, "human")
			human.ID = "human_child"
			out, err := service.CreateGroupMember(ctx, human)
			require.NoError(t, err)
			restricted = false
			restrictedReq.OperatorOnly = &restricted
			restrictedReq.Context.RequestID = "permit"
			restrictedReq.RevisionID = "two"
			restrictedReq.ExpectedRevision = 1
			_, err = service.SaveConfigurationProfile(ctx, restrictedReq)
			require.NoError(t, err)
			create.ExpectedGroupRevision = out.Group.Revision
			if tier == "global" {
				wrapped := &profileCreationRaceStore{Store: store, before: func() {
					restricted = true
					restrictedReq.Context.RequestID = "race_restrict"
					restrictedReq.RevisionID = "race_three"
					restrictedReq.ExpectedRevision = 2
					_, saveErr := service.SaveConfigurationProfile(ctx, restrictedReq)
					require.NoError(t, saveErr)
				}}
				racing := app.New(wrapped, providers.NewRegistry()).WithClock(func() time.Time { return now })
				_, raceErr := racing.CreateGroupMember(ctx, create)
				require.ErrorIs(t, raceErr, app.ErrUnauthorized)
				_, raceErr = store.Agent(ctx, "child")
				require.ErrorIs(t, raceErr, app.ErrNotFound)
				restricted = false
				restrictedReq.Context.RequestID = "race_permit"
				restrictedReq.RevisionID = "race_four"
				restrictedReq.ExpectedRevision = 3
				_, err = service.SaveConfigurationProfile(ctx, restrictedReq)
				require.NoError(t, err)
			}
			created, err := service.CreateGroupMember(ctx, create)
			require.NoError(t, err)
			restricted = true
			restrictedReq.Context.RequestID = "restrict_again"
			restrictedReq.RevisionID = "three"
			restrictedReq.ExpectedRevision++
			_, err = service.SaveConfigurationProfile(ctx, restrictedReq)
			require.NoError(t, err)
			replay, err := service.CreateGroupMember(ctx, create)
			require.NoError(t, err)
			require.True(t, replay.Repeated)
			require.Equal(t, created.Agent, replay.Agent)
			require.Equal(t, created.Group, replay.Group)
		})
	}
}

func TestOperatorOnlyProfileRetainsAutomationOrigin(t *testing.T) {
	profile := model.ConfigurationProfile{Name: "Restricted", OperatorOnly: true}
	require.NoError(t, app.ConfigurationProfileCreationAllowed(profile, model.OperatorPrincipal()))
	require.NoError(t, app.ConfigurationProfileCreationAllowed(profile, model.Principal{Kind: model.PrincipalAutomation, Authority: model.AuthoritySubject{Kind: model.AuthorityOperator}}))
	for _, principal := range []model.Principal{model.AgentPrincipal("caller"), {Kind: model.PrincipalAutomation, Authority: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: "caller"}}, {Kind: model.PrincipalAutomation}} {
		require.ErrorIs(t, app.ConfigurationProfileCreationAllowed(profile, principal), app.ErrUnauthorized)
	}
}

type profileCreationRaceStore struct {
	*sqlite.Store
	before func()
}

func (s *profileCreationRaceStore) AdmitGroupMember(ctx context.Context, in app.GroupMemberAdmission) (app.GroupMemberResult, error) {
	s.before()
	return s.Store.AdmitGroupMember(ctx, in)
}
