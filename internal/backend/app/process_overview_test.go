package app_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestProcessOverviewAndParameterHelpRejectInvalidText(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "overview.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	service := app.New(store, providers.NewRegistry())
	for _, invalid := range []string{"invalid\x00text", string([]byte{0xff}), strings.Repeat("x", (64<<10)+1)} {
		for _, parameter := range []bool{false, true} {
			graph := stagedHumanGraph()
			draft := app.DefinitionDraft{ID: "overview", RevisionID: "overview_v1", Name: "Overview", Kind: model.DefinitionProcess, SchemaVersion: 1, Source: "kind: process", Process: &model.ProcessDefinition{Graph: graph}}
			if parameter {
				draft.Parameters = []model.ParameterDeclaration{{Name: "exact_key", Type: model.ParameterString, Doc: invalid}}
			} else {
				draft.Process.Graph.Doc = invalid
			}
			_, err = service.ValidateDefinition(context.Background(), app.ValidateDefinitionRequest{Principal: model.OperatorPrincipal(), Draft: draft})
			require.ErrorIs(t, err, app.ErrInvalid)
		}
	}
}
