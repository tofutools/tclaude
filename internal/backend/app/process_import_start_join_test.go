package app_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestImportedInteriorStartReducerRetainsIdentityAndRuns(t *testing.T) {
	for _, mode := range []string{"all", "any"} {
		t.Run(mode, func(t *testing.T) {
			source := fmt.Sprintf(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: nested-start-reducer
start: outer
nodes:
  outer:
    type: parallel
    next: {left: inner, right: outer_join}
  inner:
    type: parallel
    next: {a: inner_join, b: inner_join}
  inner_join:
    type: start
    name: Inner control
    join: %s
    next: {pass: outer_join}
  outer_join:
    type: end
    join: all
layout:
  nodes:
    inner_join: {x: 240, y: 160}
`, mode)
			ctx := context.Background()
			_, service, now := regressionService(t)
			c, err := service.ConvertProcessImport(ctx, app.ConvertProcessImportRequest{Principal: model.OperatorPrincipal(), Source: source, ID: "nested_copy"})
			require.NoError(t, err)
			require.Equal(t, source, c.Draft.Source)
			require.Equal(t, model.EditorPosition{X: 240, Y: 160}, c.Draft.EditorLayout.Nodes["inner_join"])
			found := false
			for _, n := range c.Draft.Process.Graph.Nodes {
				if n.ID == "inner_join" {
					found = true
					require.Equal(t, "Inner control", n.Name)
					require.Equal(t, model.WorkNodeJoin, n.Kind)
					require.Equal(t, model.JoinMode(mode), n.Join.Mode)
				}
			}
			require.True(t, found)
			saved, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"}, Draft: c.Draft})
			require.NoError(t, err)
			ref := model.DefinitionRef{DefinitionID: saved.Definition.ID, RevisionID: saved.Revision.ID, ContentHash: saved.Revision.ContentHash, Kind: model.DefinitionProcess}
			run, err := service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "nested", Start: model.WorkStart{Definition: &ref, Deadline: now.Add(time.Hour)}})
			require.NoError(t, err)
			require.Equal(t, model.WorkRunSucceeded, run.Run.State)
			count := 0
			for _, a := range run.Run.NodeAttempts {
				if a.Ref.NodeID == "inner_join" {
					count++
				}
			}
			require.Equal(t, 1, count)
		})
	}
}
