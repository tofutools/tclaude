package transport

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

func TestPublicDefinitionRestoreRequiresExplicitArchivedValue(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry())
	handler := testHandler(t, service)
	require.NoError(t, handler.RegisterOrchestrationAPI(service))
	ctx := context.Background()
	saved, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{
		Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"},
		Draft:   app.DefinitionDraft{ID: "library", Name: "Reusable", Kind: model.DefinitionProcess, SchemaVersion: 1, Source: "authored", Process: &model.ProcessDefinition{Graph: model.WorkGraph{CompilerVersion: "1", EntryNodeID: "done", Nodes: []model.WorkNode{{ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}}}}}},
	})
	require.NoError(t, err)
	archive := request(handler, "POST", "/v2/definitions/library/archive", `{"request_id":"archive","expected_revision":1,"archived":true}`, testCredential)
	require.Equal(t, 200, archive.Code, archive.Body.String())
	for _, body := range []string{
		`{"request_id":"restore","expected_revision":2}`,
		`{"request_id":"restore","expected_revision":2,"archived":null}`,
	} {
		response := request(handler, "POST", "/v2/definitions/library/archive", body, testCredential)
		require.Equal(t, 422, response.Code, response.Body.String())
		rows, err := service.ListDefinitions(ctx, app.ListDefinitionsRequest{Principal: model.OperatorPrincipal(), IncludeTombstoned: true})
		require.NoError(t, err)
		require.Len(t, rows, 1)
		require.True(t, rows[0].Tombstoned)
		require.Equal(t, model.Revision(2), rows[0].Revision)
		require.Equal(t, saved.Revision.ID, rows[0].HeadRevisionID)
	}
	restore := request(handler, "POST", "/v2/definitions/library/archive", `{"request_id":"restore","expected_revision":2,"archived":false}`, testCredential)
	require.Equal(t, 200, restore.Code, restore.Body.String())
	rows, err := service.ListDefinitions(ctx, app.ListDefinitionsRequest{Principal: model.OperatorPrincipal()})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.False(t, rows[0].Tombstoned)
	require.Equal(t, model.Revision(3), rows[0].Revision)
}
