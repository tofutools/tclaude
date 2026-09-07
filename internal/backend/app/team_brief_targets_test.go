package app_test

import (
	"context"
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

func TestAuthoredTeamRecipientsReachPreparedInitialBrief(t *testing.T) {
	for _, recipients := range [][]string{{"reviewer"}, {"builder", "reviewer"}, {}} {
		t.Run(strings.Join(recipients, "-")+"targets", func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC()
			store, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
			require.NoError(t, err)
			defer store.Close()
			provider := &preparedWorkProvider{}
			service := app.New(store, providers.NewRegistry(provider)).WithClock(func() time.Time { return now })
			desired := model.DesiredConfiguration{Harness: "prepared-work", Model: "fixture", WorkingDirectory: t.TempDir(), Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}
			require.NoError(t, store.RegisterWorkspace(ctx, model.Workspace{ID: "workspace", State: model.WorkspaceAvailable, Observation: model.WorkspaceObservation{ActualPath: desired.WorkingDirectory, ObservedAt: now}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
			team := model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{{Key: "builder", Name: "Builder", Desired: desired, Required: true}, {Key: "reviewer", Name: "Reviewer", Desired: desired, Required: true}}, Waves: []model.TeamWave{{ID: "initial", MemberKeys: []string{"builder", "reviewer"}, RequiredReady: true, RequiredBriefs: true}}, Briefings: []model.TeamBriefing{{ID: "review", Body: "Review exact artifact", Timing: model.BriefingBeforeFirstWork, Required: true, MemberKeys: recipients}}}
			definition, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"}, Draft: app.DefinitionDraft{ID: "team", RevisionID: "v1", Name: "Team", Kind: model.DefinitionTeam, SchemaVersion: 1, Source: "browser authored equivalent", Team: &team}})
			require.NoError(t, err)
			deployment, err := service.DeployTeam(ctx, app.DeployTeamRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "deploy"}, DeploymentID: "deployment", Instantiation: model.TeamInstantiation{Definition: model.DefinitionRef{DefinitionID: definition.Definition.ID, RevisionID: definition.Revision.ID, ContentHash: definition.Revision.ContentHash, Kind: model.DefinitionTeam}, Mission: "Ship", GroupID: "group", Workspaces: model.TeamWorkspaceSelection{Shared: &model.TeamWorkspaceInput{WorkspaceID: "workspace", ExpectedRevision: 1}}}})
			require.NoError(t, err)
			for range 4 {
				_, err = service.ReconcilePendingWork(ctx)
				require.NoError(t, err)
			}
			require.Len(t, provider.preparations, 2)
			for _, preparation := range provider.preparations {
				expected := 0
				for _, key := range recipients {
					if deployment.Deployment.Members[key] == preparation.Spec.AgentID {
						expected = 1
					}
				}
				require.NotNil(t, preparation.InitialInput)
				require.Equal(t, expected, strings.Count(preparation.InitialInput.Body, "Review exact artifact"))
			}
		})
	}
}
