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

func TestTeamMembersRetainSelectedWorkspaceAcrossReopenAndRetry(t *testing.T) {
	for _, policy := range []model.WorkspacePolicy{model.WorkspacePolicyShared, model.WorkspacePolicyPerMember} {
		t.Run(string(policy), func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "state.db")
			store, err := sqlite.Open(path)
			require.NoError(t, err)
			defer func() { require.NoError(t, store.Close()) }()
			service := app.New(store, providers.NewRegistry(&preparedWorkProvider{}))
			desired := model.DesiredConfiguration{Harness: "prepared-work", Model: "test", Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}
			team := model.TeamDefinition{WorkspacePolicy: policy, Members: []model.TeamMemberSpec{{Key: "a", Name: "A", Desired: desired}, {Key: "b", Name: "B", Desired: desired}}, Waves: []model.TeamWave{{ID: "initial", MemberKeys: []string{"a", "b"}}}}
			team.Members[1].Desired.WorkingDirectory = "/former-template-directory"
			saved, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"}, Draft: app.DefinitionDraft{ID: "team", RevisionID: "team_v1", Name: "Portable team", Kind: model.DefinitionTeam, SchemaVersion: 1, Source: "portable team", Team: &team}})
			require.NoError(t, err)
			selection := model.TeamWorkspaceSelection{Members: map[string]model.TeamWorkspaceInput{}}
			paths := map[string]string{}
			for _, key := range []string{"a", "b"} {
				dir := t.TempDir()
				now := time.Now().UTC()
				id := model.WorkspaceID(key)
				require.NoError(t, store.RegisterWorkspace(ctx, model.Workspace{ID: id, State: model.WorkspaceAvailable, Observation: model.WorkspaceObservation{ActualPath: dir, ObservedAt: now}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
				selection.Members[key] = model.TeamWorkspaceInput{WorkspaceID: id, ExpectedRevision: 1}
				paths[key] = dir
			}
			if policy == model.WorkspacePolicyShared {
				shared := selection.Members["a"]
				selection = model.TeamWorkspaceSelection{Shared: &shared}
				paths["b"] = paths["a"]
			}
			req := app.DeployTeamRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "deploy"}, DeploymentID: "deployment", Instantiation: model.TeamInstantiation{Definition: model.DefinitionRef{DefinitionID: saved.Definition.ID, RevisionID: saved.Revision.ID, ContentHash: saved.Revision.ContentHash, Kind: model.DefinitionTeam}, GroupID: "group", Workspaces: selection}}
			result, err := service.DeployTeam(ctx, req)
			require.NoError(t, err)
			require.NoError(t, store.Close())
			store, err = sqlite.Open(path)
			require.NoError(t, err)
			service = app.New(store, providers.NewRegistry(&preparedWorkProvider{}))
			replay, err := service.DeployTeam(ctx, req)
			require.NoError(t, err)
			require.Equal(t, result.Deployment.ID, replay.Deployment.ID)
			for key, id := range replay.Deployment.Members {
				agent, err := store.Agent(ctx, id)
				require.NoError(t, err)
				require.Equal(t, paths[key], agent.Desired.WorkingDirectory)
			}
			original, err := store.DefinitionRevision(ctx, saved.Revision.ID)
			require.NoError(t, err)
			require.Empty(t, original.Team.Members[0].Desired.WorkingDirectory)
			require.Equal(t, "/former-template-directory", original.Team.Members[1].Desired.WorkingDirectory)
		})
	}
}
