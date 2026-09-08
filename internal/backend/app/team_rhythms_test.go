//go:build linux || darwin

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

func TestTeamRhythmsMaterializeOnceAndRemainEditable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	provider := &preparedWorkProvider{}
	service := app.New(store, providers.NewRegistry(provider))
	now := time.Now().UTC()
	service = service.WithClock(func() time.Time { return now })
	cwd := t.TempDir()
	require.NoError(t, store.RegisterWorkspace(ctx, model.Workspace{ID: "workspace", State: model.WorkspaceAvailable, Observation: model.WorkspaceObservation{ActualPath: cwd, ObservedAt: now}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
	phases := []model.TeamPhase{{Name: "Investigate", Roles: []string{"all"}, Criteria: "Record evidence <literally>."}, {Name: "Review", Roles: []string{"reviewer"}, Criteria: "Report findings, then hand off."}}
	team := model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{{Key: "worker", Name: "Worker", Desired: model.DesiredConfiguration{Harness: "prepared-work", Model: "test", WorkingDirectory: cwd, Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}, Required: true}}, Waves: []model.TeamWave{{ID: "initial", MemberKeys: []string{"worker"}, RequiredReady: true}}, AdvisoryProcess: phases, Rhythms: []model.TeamRhythm{{Name: "Status", Interval: "10m", Timezone: "UTC", Subject: "Progress", Body: "Report progress."}}}
	draft := app.DefinitionDraft{ID: "team", RevisionID: "team_v1", Name: "Team", Kind: model.DefinitionTeam, SchemaVersion: 1, Source: "test", Team: &team}
	saved, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save_team"}, Draft: draft})
	require.NoError(t, err)
	require.Equal(t, phases, saved.Revision.Team.AdvisoryProcess)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	service = app.New(store, providers.NewRegistry(provider)).WithClock(func() time.Time { return now })
	deployed, err := service.DeployTeam(ctx, app.DeployTeamRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "deploy"}, DeploymentID: "deployment", Instantiation: model.TeamInstantiation{Definition: model.DefinitionRef{DefinitionID: saved.Definition.ID, RevisionID: saved.Revision.ID, ContentHash: saved.Revision.ContentHash, Kind: model.DefinitionTeam}, Mission: "Investigate the issue", GroupID: "group", Workspaces: model.TeamWorkspaceSelection{Shared: &model.TeamWorkspaceInput{WorkspaceID: "workspace", ExpectedRevision: 1}}}})
	require.NoError(t, err)
	for range 6 {
		_, err = service.ReconcilePendingWork(ctx)
		require.NoError(t, err)
	}

	require.Len(t, deployed.Deployment.OwnedAutomationRuleIDs, 1)
	ruleID := deployed.Deployment.OwnedAutomationRuleIDs[0]
	rule, err := service.GetAutomationRule(ctx, app.GetAutomationRuleRequest{Principal: model.OperatorPrincipal(), ID: ruleID})
	require.NoError(t, err)
	require.Equal(t, deployed.Deployment.GroupID, rule.Revision.Action.Message.GroupID)
	require.Equal(t, "Progress", rule.Revision.Action.Message.Subject)
	require.Equal(t, 10*time.Minute, rule.Revision.Condition.Schedule.Interval)
	require.True(t, rule.Rule.Enabled)
	_, err = store.PutRole(ctx, model.Role{ID: "reviewers", Name: "Reviewers", Revision: 1, CreatedAt: now, UpdatedAt: now}, 0)
	require.NoError(t, err)
	_, err = store.PutRoleAssignment(ctx, model.RoleAssignment{RoleID: "reviewers", Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: deployed.Deployment.Members["worker"]}, Resource: model.ResourceSelector{Kind: model.ResourceGroupPeers, GroupID: deployed.Deployment.GroupID}, Revision: 1, CreatedAt: now, UpdatedAt: now}, 0)
	require.NoError(t, err)
	_, err = service.CreateAgent(ctx, app.CreateAgentRequest{Context: model.OperatorPrincipal(), ID: "observer", Name: "Observer", Desired: team.Members[0].Desired})
	require.NoError(t, err)
	group, err := store.Group(ctx, deployed.Deployment.GroupID)
	require.NoError(t, err)
	_, err = service.UpdateGroup(ctx, app.UpdateGroupRequest{Context: model.OperatorPrincipal(), ID: group.ID, ExpectedRevision: group.Revision, Name: group.Name, Members: append(group.Members, "observer")})
	require.NoError(t, err)
	edited, err := service.SaveAutomationRule(ctx, app.SaveAutomationRuleRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "edit_rhythm"}, ID: ruleID, RevisionID: "edited_rhythm", ExpectedRevision: rule.Rule.Revision, Name: "Updated status", Enabled: true, Owner: rule.Revision.Owner, Delegation: rule.Revision.Delegation, Condition: rule.Revision.Condition, Action: model.AutomationAction{Kind: model.AutomationSendMessage, Message: &model.AutomationMessageAction{GroupID: deployed.Deployment.GroupID, RoleID: "reviewers", Subject: "New subject", Body: "Updated body"}}, Policy: rule.Revision.Policy})
	require.NoError(t, err)
	require.Equal(t, deployed.Deployment.ID, edited.Rule.DeploymentID)
	now = now.Add(10 * time.Minute)
	for range 3 {
		_, err = service.ReconcilePendingWork(ctx)
		require.NoError(t, err)
	}
	inbox, err := service.ReadInbox(ctx, app.ReadInboxRequest{Principal: model.AgentPrincipal(deployed.Deployment.Members["worker"])})
	require.NoError(t, err)
	occurrences, occErr := service.ListOccurrences(ctx, app.ListOccurrencesRequest{Principal: model.OperatorPrincipal(), RuleID: ruleID})
	require.NoError(t, occErr)
	require.Len(t, inbox.Messages, 1, "occurrences: %+v", occurrences)
	require.Equal(t, "New subject", inbox.Messages[0].Subject)
	require.Equal(t, "Updated body", inbox.Messages[0].Body)
	observerInbox, err := service.ReadInbox(ctx, app.ReadInboxRequest{Principal: model.AgentPrincipal("observer")})
	require.NoError(t, err)
	require.Empty(t, observerInbox.Messages, "current role filtering excludes unrelated group members")
	archived, err := service.SetAutomationArchived(ctx, app.SetAutomationArchivedRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "archive_rhythm"}, ID: ruleID, ExpectedRevision: edited.Rule.Revision, Archived: true})
	require.NoError(t, err)
	for range 3 {
		_, err = service.ReconcilePendingWork(ctx)
		require.NoError(t, err)
	}
	current, err := service.GetAutomationRule(ctx, app.GetAutomationRuleRequest{Principal: model.OperatorPrincipal(), ID: ruleID})
	require.NoError(t, err)
	require.True(t, current.Rule.Tombstoned)
	require.False(t, current.Rule.Enabled)
	require.Equal(t, archived.Revision, current.Rule.Revision)
	rules, err := service.ListAutomationRules(ctx, app.ListAutomationRulesRequest{Principal: model.OperatorPrincipal(), IncludeTombstoned: true})
	require.NoError(t, err)
	require.Len(t, rules, 1)
}
