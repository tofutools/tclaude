//go:build linux || darwin

package app_test

import (
	"context"
	"github.com/tofutools/tclaude/internal/backend/host"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestTeamMissionBriefingsPinSinglePassAcrossRestartAndRebrief(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	provider := &preparedWorkProvider{}
	now := time.Now().UTC()
	service := app.New(store, providers.NewRegistry(provider)).WithClock(func() time.Time { return now })
	desired := model.DesiredConfiguration{Harness: "prepared-work", Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}
	require.NoError(t, store.RegisterWorkspace(ctx, model.Workspace{ID: "workspace", State: model.WorkspaceAvailable, Observation: model.WorkspaceObservation{ActualPath: desired.WorkingDirectory, ObservedAt: now}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
	team := model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{{Key: "worker", Name: "Worker", Desired: desired, Required: true}}, Waves: []model.TeamWave{{ID: "wave", MemberKeys: []string{"worker"}, RequiredReady: true, RequiredBriefs: true}}, Briefings: []model.TeamBriefing{
		{ID: "initial", Syntax: "mission-v1", Body: "Do {{task}}; mission {{mission}}", Timing: model.BriefingBeforeFirstWork, MemberKeys: []string{"worker"}},
		{ID: "later", Syntax: "mission-v1", Body: "Follow {{mission}}", Timing: model.BriefingAfterReady, MemberKeys: []string{"worker"}},
		{ID: "literal", Body: "Literal {{mission}}", Timing: model.BriefingAfterReady, MemberKeys: []string{"worker"}},
	}}
	draft := app.DefinitionDraft{ID: "team", RevisionID: "v1", Name: "Team", Kind: model.DefinitionTeam, SchemaVersion: 1, Source: "test", Team: &team}
	saved, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"}, Draft: draft})
	require.NoError(t, err)
	ref := model.DefinitionRef{DefinitionID: saved.Definition.ID, RevisionID: saved.Revision.ID, ContentHash: saved.Revision.ContentHash, Kind: model.DefinitionTeam}
	mission := "ship {{mission}} $(literal)"
	request := app.DeployTeamRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "deploy"}, DeploymentID: "deployment", Instantiation: model.TeamInstantiation{Definition: ref, Mission: mission, GroupID: "group", Workspaces: model.TeamWorkspaceSelection{Shared: &model.TeamWorkspaceInput{WorkspaceID: "workspace", ExpectedRevision: 1}}}}
	checkoutHost, err := host.NewCheckoutHost("git")
	require.NoError(t, err)
	service.WithWorkspaceHost(checkoutHost)
	refused := request
	refused.Context.RequestID = "refused"
	refused.DeploymentID = "refused"
	refused.Instantiation.Mission = strings.Repeat("x", 32768)
	refused.Instantiation.GroupID = "refused_group"
	refusedPath := filepath.Join(t.TempDir(), "refused_checkout")
	refused.Instantiation.Workspaces = model.TeamWorkspaceSelection{Shared: &model.TeamWorkspaceInput{WorkspaceID: "refused_workspace", CreateIntent: &model.WorkspaceIntent{Repository: lifecycleRepository(t), IntendedPath: refusedPath, BaseRevision: "HEAD", Branch: "refused"}}}
	_, err = service.DeployTeam(ctx, refused)
	require.ErrorIs(t, err, app.ErrInvalid)
	require.NoDirExists(t, refusedPath)
	_, err = store.Workspace(ctx, "refused_workspace")
	require.ErrorIs(t, err, app.ErrNotFound)
	deployed, err := service.DeployTeam(ctx, request)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	defer store.Close()
	service = app.New(store, providers.NewRegistry(provider)).WithClock(func() time.Time { return now })
	_, err = service.DeployTeam(ctx, request)
	require.NoError(t, err)
	for range 6 {
		_, err = service.ReconcilePendingWork(ctx)
		require.NoError(t, err)
	}
	require.Len(t, provider.preparations, 1)
	require.Equal(t, mission+"\n\nDo "+mission+"; mission "+mission, provider.preparations[0].InitialInput.Body)
	agent := deployed.Deployment.Members["worker"]
	inbox, err := service.ReadInbox(ctx, app.ReadInboxRequest{Principal: model.AgentPrincipal(agent)})
	require.NoError(t, err)
	bodies := []string{}
	for _, message := range inbox.Messages {
		bodies = append(bodies, message.Body)
	}
	require.Contains(t, bodies, "Follow "+mission)
	require.Contains(t, bodies, "Literal {{mission}}")
	ready, err := service.GetTeamDeployment(ctx, app.GetTeamDeploymentRequest{Principal: model.OperatorPrincipal(), DeploymentID: "deployment"})
	require.NoError(t, err)
	team.Briefings = []model.TeamBriefing{{ID: "new", Syntax: "mission-v1", Body: "Revisit {{task}}", Timing: model.BriefingAfterReady, MemberKeys: []string{"worker"}}}
	draft.RevisionID = "v2"
	saved, err = service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save_v2"}, Draft: draft, ExpectedRevision: 1})
	require.NoError(t, err)
	rebrief := app.RebriefDeploymentRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "rebrief"}, DeploymentID: "deployment", ExpectedRevision: ready.Deployment.Revision, Definition: model.DefinitionRef{DefinitionID: saved.Definition.ID, RevisionID: saved.Revision.ID, ContentHash: saved.Revision.ContentHash, Kind: model.DefinitionTeam}}
	crashing := app.New(&rebriefAdmissionLossStore{Store: store}, providers.NewRegistry(provider)).WithClock(func() time.Time { return now })
	_, err = crashing.RebriefDeployment(ctx, rebrief)
	require.ErrorIs(t, err, context.Canceled)
	service = app.New(store, providers.NewRegistry(provider)).WithClock(func() time.Time { return now })
	for range 4 {
		_, err = service.ReconcilePendingWork(ctx)
		require.NoError(t, err)
	}
	_, err = service.RebriefDeployment(ctx, rebrief)
	require.NoError(t, err)
	_, err = service.RebriefDeployment(ctx, rebrief)
	require.NoError(t, err)
	inbox, err = service.ReadInbox(ctx, app.ReadInboxRequest{Principal: model.AgentPrincipal(agent)})
	require.NoError(t, err)
	count := 0
	for _, message := range inbox.Messages {
		if message.Body == "Revisit "+mission {
			count++
		}
	}
	require.Equal(t, 1, count)
	require.Equal(t, "Revisit {{task}}", saved.Revision.Team.Briefings[0].Body)
	require.Equal(t, "mission-v1", saved.Revision.Team.Briefings[0].Syntax)
}

func TestTeamMissionIndependentAfterReadyMessagesKeepIndividualLimits(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	require.NoError(t, err)
	defer store.Close()
	now := time.Now().UTC()
	provider := &preparedWorkProvider{}
	service := app.New(store, providers.NewRegistry(provider)).WithClock(func() time.Time { return now })
	cwd := t.TempDir()
	require.NoError(t, store.RegisterWorkspace(ctx, model.Workspace{ID: "workspace", State: model.WorkspaceAvailable, Observation: model.WorkspaceObservation{ActualPath: cwd, ObservedAt: now}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
	desired := model.DesiredConfiguration{Harness: "prepared-work", Model: "test", WorkingDirectory: cwd, Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}
	body := strings.Repeat("x", 600<<10) + " {{mission}}"
	team := model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{{Key: "a", Name: "A", Desired: desired}, {Key: "b", Name: "B", Desired: desired}}, Waves: []model.TeamWave{{ID: "initial", MemberKeys: []string{"a", "b"}}}, Briefings: []model.TeamBriefing{{ID: "a", Syntax: "mission-v1", Body: body, Timing: model.BriefingAfterReady, MemberKeys: []string{"a"}}, {ID: "b", Syntax: "mission-v1", Body: body, Timing: model.BriefingAfterReady, MemberKeys: []string{"b"}}}}
	saved, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"}, Draft: app.DefinitionDraft{ID: "large", RevisionID: "v1", Name: "large", Kind: model.DefinitionTeam, SchemaVersion: 1, Source: "test", Team: &team}})
	require.NoError(t, err)
	deployed, err := service.DeployTeam(ctx, app.DeployTeamRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "deploy"}, DeploymentID: "deployment", Instantiation: model.TeamInstantiation{Definition: model.DefinitionRef{DefinitionID: saved.Definition.ID, RevisionID: saved.Revision.ID, ContentHash: saved.Revision.ContentHash, Kind: model.DefinitionTeam}, Mission: "Ship", GroupID: "group", Workspaces: model.TeamWorkspaceSelection{Shared: &model.TeamWorkspaceInput{WorkspaceID: "workspace", ExpectedRevision: 1}}}})
	require.NoError(t, err)
	for range 6 {
		_, err = service.ReconcilePendingWork(ctx)
		require.NoError(t, err)
	}
	for _, id := range deployed.Deployment.Members {
		inbox, readErr := service.ReadInbox(ctx, app.ReadInboxRequest{Principal: model.AgentPrincipal(id)})
		require.NoError(t, readErr)
		require.Len(t, inbox.Messages, 1)
		require.Equal(t, strings.Repeat("x", 600<<10)+" Ship", inbox.Messages[0].Body)
	}
}
