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

func TestTeamAdvisoryProcessPreservesGuidanceAndDeliversBeforeWork(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	provider := &preparedWorkProvider{}
	service := app.New(store, providers.NewRegistry(provider))
	now := time.Now().UTC()
	cwd := t.TempDir()
	require.NoError(t, store.RegisterWorkspace(ctx, model.Workspace{ID: "workspace", State: model.WorkspaceAvailable, Observation: model.WorkspaceObservation{ActualPath: cwd, ObservedAt: now}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
	phases := []model.TeamPhase{{Name: "Investigate", Roles: []string{"all"}, Criteria: "Record evidence <literally>."}, {Name: "Review", Roles: []string{"reviewer"}, Criteria: "Report findings, then hand off."}}
	team := model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{{Key: "worker", Name: "Worker", Labels: model.AgentLabels{Role: " Reviewer ", Description: "Member guidance"}, Desired: model.DesiredConfiguration{Harness: "prepared-work", Model: "test", WorkingDirectory: cwd, Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}, Required: true}}, Waves: []model.TeamWave{{ID: "initial", MemberKeys: []string{"worker"}, RequiredReady: true}}, AdvisoryProcess: phases}
	draft := app.DefinitionDraft{ID: "team", RevisionID: "team_v1", Name: "Team", Kind: model.DefinitionTeam, SchemaVersion: 1, Source: "test", Team: &team}
	team.Members[0].Labels.Groups = map[model.GroupID]model.AgentDisplayLabels{"unrelated": {Role: "wrong scope"}}
	_, err = service.ValidateDefinition(ctx, app.ValidateDefinitionRequest{Principal: model.OperatorPrincipal(), Draft: draft})
	require.ErrorIs(t, err, app.ErrInvalid)
	team.Members[0].Labels.Groups = nil
	saved, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save_team"}, Draft: draft})
	require.NoError(t, err)
	require.Equal(t, phases, saved.Revision.Team.AdvisoryProcess)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	service = app.New(store, providers.NewRegistry(provider))
	deployed, err := service.DeployTeam(ctx, app.DeployTeamRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "deploy"}, DeploymentID: "deployment", Instantiation: model.TeamInstantiation{Definition: model.DefinitionRef{DefinitionID: saved.Definition.ID, RevisionID: saved.Revision.ID, ContentHash: saved.Revision.ContentHash, Kind: model.DefinitionTeam}, Mission: "Investigate the issue", GroupID: "group", Workspaces: model.TeamWorkspaceSelection{Shared: &model.TeamWorkspaceInput{WorkspaceID: "workspace", ExpectedRevision: 1}}}})
	require.NoError(t, err)
	member, err := store.Agent(ctx, deployed.Deployment.Members["worker"])
	require.NoError(t, err)
	require.Equal(t, team.Members[0].Labels.InGroup(""), member.Labels.InGroup("group"))
	require.Empty(t, member.Labels.InGroup("other"))
	for range 6 {
		_, err = service.ReconcilePendingWork(ctx)
		require.NoError(t, err)
	}
	require.Len(t, provider.preparations, 1)
	body := provider.preparations[0].InitialInput.Body
	require.Contains(t, body, "1. Investigate — active roles: all")
	require.Contains(t, body, "Record evidence <literally>.")
	require.Contains(t, body, "2. Review — active roles: reviewer")
	require.Contains(t, body, "phases do not change permissions or gate work")
	_, err = service.CreateAgent(ctx, app.CreateAgentRequest{Context: model.OperatorPrincipal(), ID: "observer", Name: "Observer", Desired: team.Members[0].Desired})
	require.NoError(t, err)
	group, err := store.Group(ctx, deployed.Deployment.GroupID)
	require.NoError(t, err)
	_, err = service.UpdateGroup(ctx, app.UpdateGroupRequest{Context: model.OperatorPrincipal(), ID: group.ID, ExpectedRevision: group.Revision, Name: group.Name, Members: append(group.Members, "observer")})
	require.NoError(t, err)
	current, err := service.GetTeamDeployment(ctx, app.GetTeamDeploymentRequest{Principal: model.OperatorPrincipal(), DeploymentID: deployed.Deployment.ID})
	require.NoError(t, err)
	require.Equal(t, uint32(0), current.Deployment.AdvisoryPhase)
	advanced, err := service.AdvanceAdvisoryPhase(ctx, app.AdvanceAdvisoryPhaseRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "advance"}, DeploymentID: deployed.Deployment.ID, ExpectedRevision: current.Deployment.Revision})
	require.NoError(t, err)
	require.Equal(t, uint32(1), advanced.Deployment.AdvisoryPhase)
	require.Equal(t, 1, advanced.PhaseNotifications)
	observerInbox, err := service.ReadInbox(ctx, app.ReadInboxRequest{Principal: model.AgentPrincipal("observer")})
	require.NoError(t, err)
	require.Empty(t, observerInbox.Messages, "an unrelated role must not receive the phase notice")
	inbox, err := service.ReadInbox(ctx, app.ReadInboxRequest{Principal: model.AgentPrincipal(deployed.Deployment.Members["worker"])})
	require.NoError(t, err)
	require.Len(t, inbox.Messages, 1)
	require.Contains(t, inbox.Messages[0].Body, "Report findings, then hand off.")
	require.Equal(t, phases, advanced.Phases)
	require.Len(t, advanced.Deployment.PhaseHistory, 1)
	require.Equal(t, "Investigate", advanced.Deployment.PhaseHistory[0].From)
	require.Equal(t, "Review", advanced.Deployment.PhaseHistory[0].To)
	backRequest := app.AdvanceAdvisoryPhaseRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "back"}, DeploymentID: deployed.Deployment.ID, ExpectedRevision: advanced.Deployment.Revision, Phase: "Investigate"}
	back, err := service.AdvanceAdvisoryPhase(ctx, backRequest)
	require.NoError(t, err)
	require.Equal(t, uint32(0), back.Deployment.AdvisoryPhase)
	require.Len(t, back.Deployment.PhaseHistory, 2)
	require.Equal(t, 2, back.PhaseNotifications, "all includes the current group members")
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	service = app.New(store, providers.NewRegistry(provider))
	replay, err := service.AdvanceAdvisoryPhase(ctx, backRequest)
	require.NoError(t, err)
	require.Equal(t, back.Deployment.PhaseHistory, replay.Deployment.PhaseHistory)
	inbox, err = service.ReadInbox(ctx, app.ReadInboxRequest{Principal: model.AgentPrincipal(deployed.Deployment.Members["worker"])})
	require.NoError(t, err)
	require.Len(t, inbox.Messages, 2, "exact phase replay must not repeat entry notices")
	changed := backRequest
	changed.Phase = "Review"
	_, err = service.AdvanceAdvisoryPhase(ctx, changed)
	require.ErrorIs(t, err, app.ErrConflict)
}
