//go:build linux || darwin

package app_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestTeamWorkspaceReadinessRhythmRebriefAndStandDownJourney(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	checkoutHost, err := host.NewCheckoutHost("git")
	require.NoError(t, err)
	provider := &preparedWorkProvider{}
	now := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	service := app.New(store, providers.NewRegistry(provider)).WithWorkspaceHost(checkoutHost).WithClock(func() time.Time { return now })
	operator := model.OperatorPrincipal()
	repository := lifecycleRepository(t)
	checkoutPath := filepath.Join(t.TempDir(), "team-checkout")

	rhythm, err := service.SaveAutomationRule(ctx, app.SaveAutomationRuleRequest{
		Context: app.RequestContext{Principal: operator, RequestID: "save_source_rhythm"}, ID: "source_rhythm", RevisionID: "source_rhythm_v1", Name: "daily review", Enabled: true,
		Owner:     model.AuthoritySubject{Kind: model.AuthorityOperator},
		Condition: model.AutomationCondition{Kind: model.AutomationSchedule, Schedule: &model.ScheduleCondition{Timezone: "UTC", Interval: time.Minute, Anchor: now.Add(time.Hour)}},
		Action:    model.AutomationAction{Kind: model.AutomationSendMessage, Message: &model.AutomationMessageAction{Body: "review progress", GroupID: "team_group"}},
		Policy:    model.OccurrencePolicy{MissedTicks: model.MissedTickSkip, OfflineDelivery: model.OfflineQueue, ExpiresAfter: time.Hour, Overlap: model.OverlapForbid, MaxActive: 1, Deadline: time.Minute, Retry: model.RetryPolicy{MaxAttempts: 1}},
	})
	require.NoError(t, err)

	desired := model.DesiredConfiguration{Harness: "prepared-work", Model: "test", WorkingDirectory: "/authored/placeholder", Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}
	teamV1 := model.TeamDefinition{
		WorkspacePolicy: model.WorkspacePolicyShared,
		Members: []model.TeamMemberSpec{
			{Key: "builder", Name: "builder", Desired: desired, Required: true, BriefingIDs: []string{"start", "ready"}},
			{Key: "reviewer", Name: "reviewer", Desired: desired, Required: true, BriefingIDs: []string{"start"}},
		},
		Waves: []model.TeamWave{
			{ID: "build", MemberKeys: []string{"builder"}, RequiredReady: true, RequiredBriefs: true},
			{ID: "review", MemberKeys: []string{"reviewer"}, DependsOn: []string{"build"}, RequiredReady: true, RequiredBriefs: true},
		},
		Briefings: []model.TeamBriefing{
			{ID: "start", Body: "start from the pinned mission", Timing: model.BriefingBeforeFirstWork, Required: true, MemberKeys: []string{"builder", "reviewer"}},
			{ID: "ready", Body: "coordinate after readiness", Timing: model.BriefingAfterReady, Required: true, MemberKeys: []string{"builder"}},
		},
		AdvisoryPhases: []string{"investigate", "recommend"},
		Automation:     []model.AutomationRuleRef{{RuleID: rhythm.Rule.ID, RevisionID: rhythm.Revision.ID, ContentHash: rhythm.Revision.ContentHash}},
	}
	definition, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: operator, RequestID: "save_team_v1"}, Draft: app.DefinitionDraft{ID: "review_team", RevisionID: "review_team_v1", Name: "review team", Kind: model.DefinitionTeam, SchemaVersion: 1, Source: "v1", Team: &teamV1}})
	require.NoError(t, err)
	refV1 := model.DefinitionRef{DefinitionID: definition.Definition.ID, RevisionID: definition.Revision.ID, ContentHash: definition.Revision.ContentHash, Kind: model.DefinitionTeam}
	request := app.DeployTeamRequest{Context: app.RequestContext{Principal: operator, RequestID: "deploy_team"}, DeploymentID: "review_deployment", Instantiation: model.TeamInstantiation{
		Definition: refV1, Mission: "ship safely", Target: model.TeamDeploymentTarget{Kind: model.TeamTargetNewGroup, GroupID: "team_group"},
		Workspaces: model.TeamWorkspaceSelection{Shared: &model.TeamWorkspaceInput{WorkspaceID: "team_workspace", CreateIntent: &model.WorkspaceIntent{Repository: repository, IntendedPath: checkoutPath, BaseRevision: "HEAD", Branch: "feature/team-lifecycle", RetainOnFinish: true}}},
	}}
	deployed, err := service.DeployTeam(ctx, request)
	require.NoError(t, err)
	replayed, err := service.DeployTeam(ctx, request)
	require.NoError(t, err)
	require.Equal(t, deployed.Deployment.Revision, replayed.Deployment.Revision)
	changed := request
	changed.Instantiation.Mission = "different mission"
	_, err = service.DeployTeam(ctx, changed)
	require.ErrorIs(t, err, app.ErrConflict)
	require.Equal(t, model.TeamTargetNewGroup, deployed.Deployment.TargetKind)
	require.Equal(t, deployed.Deployment.Workspaces["builder"].WorkspaceID, deployed.Deployment.Workspaces["reviewer"].WorkspaceID)
	require.True(t, deployed.Deployment.Workspaces["builder"].Owned)
	require.DirExists(t, checkoutPath)
	require.Len(t, deployed.Deployment.OwnedAutomationRuleIDs, 1)
	ownedRhythm, err := service.GetAutomationRule(ctx, app.GetAutomationRuleRequest{Principal: operator, ID: deployed.Deployment.OwnedAutomationRuleIDs[0]})
	require.NoError(t, err)
	require.False(t, ownedRhythm.Rule.Enabled)
	require.Equal(t, deployed.Deployment.ID, ownedRhythm.Rule.DeploymentID)

	for range 8 {
		_, err = service.ReconcilePendingWork(ctx)
		require.NoError(t, err)
	}
	ready, err := service.GetTeamDeployment(ctx, app.GetTeamDeploymentRequest{Principal: operator, DeploymentID: deployed.Deployment.ID})
	require.NoError(t, err)
	require.Equal(t, model.DeploymentReady, ready.Deployment.State)
	require.NotEmpty(t, ready.Deployment.BriefingOperationIDs["builder"])
	require.Len(t, provider.preparations, 2)
	canonicalCheckoutPath, err := filepath.EvalSymlinks(checkoutPath)
	require.NoError(t, err)
	for _, preparation := range provider.preparations {
		require.Equal(t, canonicalCheckoutPath, preparation.Spec.WorkingDirectory)
	}
	ownedRhythm, err = service.GetAutomationRule(ctx, app.GetAutomationRuleRequest{Principal: operator, ID: ready.Deployment.OwnedAutomationRuleIDs[0]})
	require.NoError(t, err)
	require.True(t, ownedRhythm.Rule.Enabled)
	editedRhythm, err := service.SaveAutomationRule(ctx, app.SaveAutomationRuleRequest{
		Context: app.RequestContext{Principal: operator, RequestID: "edit_owned_rhythm"}, ID: ownedRhythm.Rule.ID, RevisionID: "owned_rhythm_edit", ExpectedRevision: ownedRhythm.Rule.Revision, Name: "external edit", Enabled: true,
		Owner: ownedRhythm.Revision.Owner, Delegation: ownedRhythm.Revision.Delegation, Condition: ownedRhythm.Revision.Condition, Action: ownedRhythm.Revision.Action, Policy: ownedRhythm.Revision.Policy, Dependencies: ownedRhythm.Revision.Dependencies,
	})
	require.NoError(t, err)
	require.Equal(t, ownedRhythm.Rule.DeploymentID, editedRhythm.Rule.DeploymentID, "ordinary edits retain deployment lifecycle association")

	teamV2 := teamV1
	teamV2.Briefings = append([]model.TeamBriefing(nil), teamV1.Briefings...)
	teamV2.Briefings[1].Body = "coordinate using the new work pattern"
	definitionV2, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: operator, RequestID: "save_team_v2"}, ExpectedRevision: definition.Definition.Revision, Draft: app.DefinitionDraft{ID: definition.Definition.ID, RevisionID: "review_team_v2", Name: "review team", Kind: model.DefinitionTeam, SchemaVersion: 1, Source: "v2", Team: &teamV2}})
	require.NoError(t, err)
	refV2 := model.DefinitionRef{DefinitionID: definitionV2.Definition.ID, RevisionID: definitionV2.Revision.ID, ContentHash: definitionV2.Revision.ContentHash, Kind: model.DefinitionTeam}
	rebriefRequest := app.RebriefDeploymentRequest{Context: app.RequestContext{Principal: operator, RequestID: "rebrief_team"}, DeploymentID: ready.Deployment.ID, ExpectedRevision: ready.Deployment.Revision, Definition: refV2}
	rebriefed, err := service.RebriefDeployment(ctx, rebriefRequest)
	require.NoError(t, err)
	require.Equal(t, refV1, rebriefed.Deployment.Definition, "rebrief must not mutate pinned launch configuration")
	require.Equal(t, refV2, rebriefed.Deployment.Rebriefs[0].Definition)
	require.Equal(t, model.TeamRebriefCompleted, rebriefed.Deployment.Rebriefs[0].State)
	require.NotEmpty(t, rebriefed.Deployment.Rebriefs[0].RecipientOperations["builder"])
	repeated, err := service.RebriefDeployment(ctx, rebriefRequest)
	require.NoError(t, err)
	require.Equal(t, rebriefed.Deployment.Revision, repeated.Deployment.Revision)

	advanced, err := service.AdvanceAdvisoryPhase(ctx, app.AdvanceAdvisoryPhaseRequest{Context: app.RequestContext{Principal: operator, RequestID: "advance_phase"}, DeploymentID: repeated.Deployment.ID, ExpectedRevision: repeated.Deployment.Revision})
	require.NoError(t, err)
	require.Equal(t, uint32(1), advanced.Deployment.AdvisoryPhase)
	listed, err := service.ListTeamDeployments(ctx, app.ListTeamDeploymentsRequest{Principal: operator, GroupID: "team_group"})
	require.NoError(t, err)
	require.Len(t, listed, 1)

	stopped, err := service.StandDownDeployment(ctx, app.StandDownDeploymentRequest{Context: app.RequestContext{Principal: operator, RequestID: "stand_down"}, DeploymentID: advanced.Deployment.ID, ExpectedRevision: advanced.Deployment.Revision, Reason: "mission complete"})
	require.NoError(t, err)
	require.Equal(t, model.DeploymentStopped, stopped.Deployment.State)
	require.DirExists(t, checkoutPath, "stand-down retains the owned checkout")
	_, err = store.Group(ctx, stopped.Deployment.GroupID)
	require.NoError(t, err, "stand-down retains the collaboration group")
	for _, id := range stopped.Deployment.Members {
		agent, readErr := store.Agent(ctx, id)
		require.NoError(t, readErr)
		require.Equal(t, model.AgentRetired, agent.Lifecycle)
	}
	uses, err := store.ActiveWorkspaceUses(ctx, "team_workspace")
	require.NoError(t, err)
	require.Empty(t, uses, "execution workspace claims end when exact executions exit")
	ownedRhythm, err = service.GetAutomationRule(ctx, app.GetAutomationRuleRequest{Principal: operator, ID: stopped.Deployment.OwnedAutomationRuleIDs[0]})
	require.NoError(t, err)
	require.False(t, ownedRhythm.Rule.Enabled)
}

func TestReinforcementStandDownDoesNotRetireSharedMembers(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 7, 11, 0, 0, 0, time.UTC)
	provider := &preparedWorkProvider{}
	service := app.New(store, providers.NewRegistry(provider)).WithClock(func() time.Time { return now })
	operator := model.OperatorPrincipal()
	shared := model.Agent{ID: "shared_member", Name: "shared", Lifecycle: model.AgentActive, Desired: model.DesiredConfiguration{Harness: "prepared-work", Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}, Revision: 1, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, store.CreateAgent(ctx, shared))
	_, err = service.CreateGroup(ctx, app.CreateGroupRequest{Context: operator, ID: "existing_group", Name: "existing", Members: []model.AgentID{shared.ID}})
	require.NoError(t, err)
	workspacePath := t.TempDir()
	require.NoError(t, store.RegisterWorkspace(ctx, model.Workspace{ID: "existing_workspace", Intent: model.WorkspaceIntent{IntendedPath: workspacePath, Provenance: model.WorkspaceRegistered, Ownership: model.WorkspaceExternal}, State: model.WorkspaceAvailable, Observation: model.WorkspaceObservation{ActualPath: workspacePath, ObservedAt: now}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
	team := model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{{Key: "reinforcement", Name: "reinforcement", Desired: shared.Desired, Required: true}}, Waves: []model.TeamWave{{ID: "join", MemberKeys: []string{"reinforcement"}, RequiredReady: true}}}
	definition, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: operator, RequestID: "save_reinforcement"}, Draft: app.DefinitionDraft{ID: "reinforcement_team", RevisionID: "reinforcement_v1", Name: "reinforcement", Kind: model.DefinitionTeam, SchemaVersion: 1, Source: "reinforce", Team: &team}})
	require.NoError(t, err)
	deployed, err := service.DeployTeam(ctx, app.DeployTeamRequest{Context: app.RequestContext{Principal: operator, RequestID: "deploy_reinforcement"}, DeploymentID: "reinforcement_deployment", Instantiation: model.TeamInstantiation{Definition: model.DefinitionRef{DefinitionID: definition.Definition.ID, RevisionID: definition.Revision.ID, ContentHash: definition.Revision.ContentHash, Kind: model.DefinitionTeam}, Target: model.TeamDeploymentTarget{Kind: model.TeamTargetExistingGroup, GroupID: "existing_group"}, Workspaces: model.TeamWorkspaceSelection{Shared: &model.TeamWorkspaceInput{WorkspaceID: "existing_workspace", ExpectedRevision: 1}}}})
	require.NoError(t, err)
	for range 4 {
		_, err = service.ReconcilePendingWork(ctx)
		require.NoError(t, err)
	}
	current, err := service.GetTeamDeployment(ctx, app.GetTeamDeploymentRequest{Principal: operator, DeploymentID: deployed.Deployment.ID})
	require.NoError(t, err)
	manager := model.Agent{ID: "lifecycle_manager", Name: "manager", Lifecycle: model.AgentActive, Desired: shared.Desired, Revision: 1, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, store.CreateAgent(ctx, manager))
	managedMember, err := store.Agent(ctx, current.Deployment.Members["reinforcement"])
	require.NoError(t, err)
	putGrant := func(id model.GrantID, action model.Action, resource model.ResourceSelector) model.AuthorityGrant {
		grant, grantErr := store.PutGrant(ctx, model.AuthorityGrant{ID: id, Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: manager.ID}, Action: action, Resource: resource, Revision: 1, CreatedAt: now, UpdatedAt: now}, 0)
		require.NoError(t, grantErr)
		return grant
	}
	putGrant("manage_reinforcement", model.ActionManageMembership, model.ResourceSelector{Kind: model.ResourceGroup, GroupID: current.Deployment.GroupID})
	putGrant("stop_reinforcement", model.ActionStop, model.ResourceSelector{Kind: model.ResourceExecution, ExecutionID: managedMember.PrimaryExecutionID})
	revokedRetirement := putGrant("retire_reinforcement_revoked", model.ActionRetireAgent, model.ResourceSelector{Kind: model.ResourceAgent, AgentID: managedMember.ID})
	require.NoError(t, store.DeleteGrant(ctx, revokedRetirement.ID, revokedRetirement.Revision))
	standDownRequest := app.StandDownDeploymentRequest{Context: app.RequestContext{Principal: model.AgentPrincipal(manager.ID), RequestID: "stop_reinforcement"}, DeploymentID: current.Deployment.ID, ExpectedRevision: current.Deployment.Revision, Reason: "reinforcement done"}
	_, err = service.StandDownDeployment(ctx, standDownRequest)
	require.ErrorIs(t, err, app.ErrUnauthorized, "revoked exact retirement authority blocks cleanup after the admitted stand-down")
	standingDown, err := service.GetTeamDeployment(ctx, app.GetTeamDeploymentRequest{Principal: operator, DeploymentID: current.Deployment.ID})
	require.NoError(t, err)
	require.Equal(t, model.DeploymentStandingDown, standingDown.Deployment.State)
	// Recovery must retain the manager's revoked authority, not the creator's.
	service = app.New(store, providers.NewRegistry(provider)).WithClock(func() time.Time { return now })
	_, err = service.ReconcilePendingWork(ctx)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	stillActive, err := store.Agent(ctx, managedMember.ID)
	require.NoError(t, err)
	require.Equal(t, model.AgentActive, stillActive.Lifecycle)
	putGrant("retire_reinforcement", model.ActionRetireAgent, model.ResourceSelector{Kind: model.ResourceAgent, AgentID: managedMember.ID})
	stopped, err := service.StandDownDeployment(ctx, standDownRequest)
	require.NoError(t, err)
	require.Equal(t, model.DeploymentStopped, stopped.Deployment.State)
	sharedAfter, err := store.Agent(ctx, shared.ID)
	require.NoError(t, err)
	require.Equal(t, model.AgentActive, sharedAfter.Lifecycle)
	group, err := store.Group(ctx, "existing_group")
	require.NoError(t, err)
	require.Contains(t, group.Members, shared.ID)
}

func lifecycleRepository(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	lifecycleGit(t, dir, "init")
	lifecycleGit(t, dir, "config", "user.email", "tests@example.invalid")
	lifecycleGit(t, dir, "config", "user.name", "Lifecycle Test")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("team lifecycle\n"), 0o600))
	lifecycleGit(t, dir, "add", "README.md")
	lifecycleGit(t, dir, "commit", "-m", "initial")
	return dir
}

func lifecycleGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
}
