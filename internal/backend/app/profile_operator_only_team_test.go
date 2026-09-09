//go:build linux || darwin

package app_test

import (
	"context"
	"fmt"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
	"path/filepath"
	"testing"
	"time"
)

func TestOperatorOnlyGlobalProfileBlocksInlineTeamAutomation(t *testing.T) {
	for _, tier := range []string{"global", "group"} {
		for _, race := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s_restriction_during_admission_%t", tier, race), func(t *testing.T) { testOperatorOnlyInlineTeam(t, race, tier) })
		}
	}
}
func testOperatorOnlyInlineTeam(t *testing.T, race bool, tier string) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 6, 20, 0, 0, 0, time.UTC)
	provider := &preparedWorkProvider{}
	wrapped := &operatorOnlyTeamRaceStore{Store: store}
	service := app.New(wrapped, providers.NewRegistry(provider)).WithClock(func() time.Time { return now })
	directory := t.TempDir()
	require.NoError(t, store.RegisterWorkspace(ctx, model.Workspace{ID: "deployed_workspace", State: model.WorkspaceAvailable, Observation: model.WorkspaceObservation{ActualPath: directory, ObservedAt: now}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
	desired := model.DesiredConfiguration{Harness: "prepared-work", Model: "test", WorkingDirectory: directory, Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}

	owner := model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: "owner"}
	_, err = service.CreateAgent(ctx, app.CreateAgentRequest{Context: model.OperatorPrincipal(), ID: "owner", Name: "Owner", Desired: desired})
	require.NoError(t, err)
	restricted := !race
	profileReq := app.SaveConfigurationProfileRequest{Context: effect(model.OperatorPrincipal(), "restricted"), ID: "global", RevisionID: "one", Name: "Global", Options: &model.ConfigurationOptions{}, OperatorOnly: &restricted}
	profile, err := service.SaveConfigurationProfile(ctx, profileReq)
	require.NoError(t, err)
	if tier == "global" {
		_, err = service.SaveConfigurationDefaults(ctx, app.SaveConfigurationDefaultsRequest{Context: effect(model.OperatorPrincipal(), "select_global"), Global: &profile.Revision.Ref})
		require.NoError(t, err)
	} else {
		_, err = service.CreateGroup(ctx, app.CreateGroupRequest{Context: model.OperatorPrincipal(), ID: "deployed_group", Name: "Existing"})
		require.NoError(t, err)
		_, err = service.SetGroupConfiguration(ctx, app.SetGroupConfigurationRequest{Principal: model.OperatorPrincipal(), GroupID: "deployed_group", Profile: &profile.Revision.Ref})
		require.NoError(t, err)
	}
	team := model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{{Key: "builder", Name: "builder", Desired: desired, Required: true}}, Waves: []model.TeamWave{{ID: "build", MemberKeys: []string{"builder"}, RequiredReady: true}}}
	definition, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "team"}, Draft: app.DefinitionDraft{ID: "team", RevisionID: "team_v1", Name: "team", Kind: model.DefinitionTeam, SchemaVersion: 1, Source: "team", Team: &team}})
	require.NoError(t, err)
	teamRef := model.DefinitionRef{DefinitionID: definition.Definition.ID, RevisionID: definition.Revision.ID, ContentHash: definition.Revision.ContentHash, Kind: model.DefinitionTeam}
	delegation := model.AutomationDelegation{Actions: []model.Action{model.ActionRunAutomation, model.ActionStartWork, model.ActionLaunch, model.ActionInspectWorkspace}, Resources: []model.ResourceSelector{{Kind: model.ResourceAutomationRule, AutomationRuleID: "deploy_rule"}, {Kind: model.ResourceGroupPeers, GroupID: "deployed_group"}, {Kind: model.ResourceWorkspace, WorkspaceID: "deployed_workspace"}}, Bounds: model.ConfigurationBounds{Harnesses: []string{desired.Harness}, Models: []string{desired.Model}, WorkingDirectoryRoots: []string{directory}, ApprovalModes: []model.ApprovalMode{desired.Approval}, SandboxModes: []model.SandboxMode{desired.Sandbox}}, ExpiresAt: now.Add(time.Hour)}

	delegation.Actions = append(delegation.Actions, model.ActionManageMembership)
	delegation.Resources = append(delegation.Resources, model.ResourceSelector{Kind: model.ResourceGroup, GroupID: "deployed_group"})
	for i, pair := range []struct {
		action   model.Action
		resource model.ResourceSelector
	}{
		{model.ActionRunAutomation, delegation.Resources[0]}, {model.ActionStartWork, delegation.Resources[0]}, {model.ActionLaunch, delegation.Resources[1]}, {model.ActionInspectWorkspace, delegation.Resources[2]}, {model.ActionManageMembership, delegation.Resources[3]},
	} {
		_, err = service.PutGrant(ctx, app.PutGrantRequest{Principal: model.OperatorPrincipal(), Grant: model.AuthorityGrant{ID: model.GrantID(fmt.Sprintf("grant_%d", i)), Subject: owner, Action: pair.action, Resource: pair.resource, Bounds: delegation.Bounds}})
		require.NoError(t, err)
	}
	ruleRequest := app.SaveAutomationRuleRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "rule"}, ID: "deploy_rule", RevisionID: "deploy_rule_v1", Name: "deploy", Enabled: true, Owner: owner, Delegation: delegation, Condition: model.AutomationCondition{Kind: model.AutomationSchedule, Schedule: &model.ScheduleCondition{Timezone: "UTC", Interval: time.Minute, Anchor: now.Add(time.Minute)}}, Action: model.AutomationAction{Kind: model.AutomationDeployTeam, Team: &model.TeamInstantiation{Definition: teamRef, Mission: "ship", GroupID: "deployed_group", Workspaces: model.TeamWorkspaceSelection{Shared: &model.TeamWorkspaceInput{WorkspaceID: "deployed_workspace", ExpectedRevision: 1}}}}, Policy: regressionOccurrencePolicy(), Dependencies: []model.DefinitionRef{teamRef}}
	if tier == "group" {
		ruleRequest.Action.Team.GroupID = ""
		ruleRequest.Action.Team.Target = model.TeamDeploymentTarget{Kind: model.TeamTargetExistingGroup, GroupID: "deployed_group"}
	}
	rule, err := service.SaveAutomationRule(ctx, ruleRequest)
	require.NoError(t, err)

	if race {
		wrapped.before = func() {
			restricted = true
			profileReq.Context.RequestID = "restrict_race"
			profileReq.RevisionID = "race_two"
			profileReq.ExpectedRevision = 1
			_, saveErr := service.SaveConfigurationProfile(ctx, profileReq)
			require.NoError(t, saveErr)
		}
	}
	now = now.Add(time.Minute)
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	snapshot, err := store.Snapshot(ctx)
	require.NoError(t, err)
	require.Len(t, snapshot.Agents, 1, "restricted global profile must block inline team members before creation")
	if tier == "group" {
		require.Len(t, snapshot.Groups, 1)
		require.Empty(t, snapshot.Groups[0].Members)
	} else {
		require.Empty(t, snapshot.Groups)
	}
	require.Empty(t, provider.preparations)
	restricted = false
	profileReq.Context.RequestID = "permit"
	profileReq.RevisionID = "two"
	profileReq.ExpectedRevision++
	_, err = service.SaveConfigurationProfile(ctx, profileReq)
	require.NoError(t, err)
	now = now.Add(time.Minute)
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	snapshot, err = store.Snapshot(ctx)
	require.NoError(t, err)
	require.Len(t, snapshot.Agents, 2, "same valid delegation permits inline creation after restriction is cleared")
	_ = rule
}

type operatorOnlyTeamRaceStore struct {
	*sqlite.Store
	before func()
}

func (s *operatorOnlyTeamRaceStore) CreateTeamDeployment(ctx context.Context, deployment model.TeamDeployment, group model.Group, agents []model.Agent, assignments []model.RoleAssignment, principal model.Principal, requestID model.RequestID, requestDigest string, at time.Time) (model.TeamDeployment, bool, error) {
	if s.before != nil {
		before := s.before
		s.before = nil
		before()
	}
	return s.Store.CreateTeamDeployment(ctx, deployment, group, agents, assignments, principal, requestID, requestDigest, at)
}
