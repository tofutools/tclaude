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

func TestTeamEnvironmentAuthoringRejectsInvalidBeforePersistence(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.db"))
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry())
	draft := app.DefinitionDraft{ID: "team", RevisionID: "one", Name: "Team", Kind: model.DefinitionTeam, SchemaVersion: 1, Source: "fixture", Team: &model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{{Key: "worker", Name: "Worker", Desired: model.DesiredConfiguration{Harness: "codex", Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}}}, Waves: []model.TeamWave{{ID: "initial", MemberKeys: []string{"worker"}}}}}
	for _, env := range []model.Environment{{"HOME": "/elsewhere"}, {"INVALID-NAME": "value"}, {"VALID": "bad\x00value"}} {
		draft.Team.Members[0].Desired.Environment = env
		_, err = service.ValidateDefinition(ctx, app.ValidateDefinitionRequest{Principal: model.OperatorPrincipal(), Draft: draft})
		require.ErrorIs(t, err, app.ErrInvalid)
		_, err = service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"}, Draft: draft})
		require.ErrorIs(t, err, app.ErrInvalid)
	}
	definitions, err := service.ListDefinitions(ctx, app.ListDefinitionsRequest{Principal: model.OperatorPrincipal()})
	require.NoError(t, err)
	require.Empty(t, definitions)
	draft.Team.Members[0].Desired.Environment = model.Environment{"TEAM_VALUE": "literal $(nothing)\nsecond line"}
	saved, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"}, Draft: draft})
	require.NoError(t, err)
	require.Equal(t, draft.Team.Members[0].Desired.Environment, saved.Revision.Team.Members[0].Desired.Environment)
}
