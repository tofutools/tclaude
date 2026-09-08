package app_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/processimport"
)

func TestImportedJoinPreservesReducerAndSettlesLosingBranches(t *testing.T) {
	for _, mode := range []string{"all", "any"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			_, service, now := regressionService(t)
			source := fmt.Sprintf(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: joined
start: fork
nodes:
  fork:
    type: parallel
    next: {a: left, b: right}
  left:
    type: task
    performer: {kind: human, ask: Left}
    next: merge
  right:
    type: task
    performer: {kind: human, ask: Right}
    next: merge
  merge:
    type: task
    name: Retained reducer
    join: %s
    performer: {kind: human, ask: Merge}
    next: done
  done:
    type: end
layout:
  nodes:
    merge: {x: 400, y: 300}
`, mode)
			bindings := map[string]processimport.Binding{}
			for _, id := range []string{"left", "right", "merge"} {
				bindings["/nodes/"+id+"/performer"] = processimport.Binding{Performer: model.Performer{Kind: model.PerformerHuman, Human: &model.HumanPerformer{Operator: true}}}
			}
			converted, err := service.ConvertProcessImport(ctx, app.ConvertProcessImportRequest{Principal: model.OperatorPrincipal(), Source: source, ID: "copy", Bindings: bindings})
			require.NoError(t, err)
			require.Equal(t, source, converted.Draft.Source)
			require.Equal(t, model.EditorPosition{X: 400, Y: 300}, converted.Draft.EditorLayout.Nodes["merge"])
			saved, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"}, Draft: converted.Draft})
			require.NoError(t, err)
			ref := model.DefinitionRef{DefinitionID: saved.Definition.ID, RevisionID: saved.Revision.ID, ContentHash: saved.Revision.ContentHash, Kind: model.DefinitionProcess}
			_, err = service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "run", Start: model.WorkStart{Definition: &ref, Deadline: now.Add(time.Hour)}})
			require.NoError(t, err)
			read := func() app.WorkRunResult {
				r, e := service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "run"})
				require.NoError(t, e)
				return r
			}
			answer := func(id model.WorkNodeID) {
				r := read()
				for _, w := range r.Decisions {
					if w.Attempt.NodeID == id && w.State == model.DecisionOpen {
						_, e := service.SubmitDecision(ctx, app.SubmitDecisionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: model.RequestID("answer_" + string(id))}, DecisionID: w.ID, ExpectedWindowRevision: w.Revision, ExpectedRunRevision: r.Run.Revision, Answer: "complete", Reason: "reviewed"})
						require.NoError(t, e)
						return
					}
				}
				t.Fatalf("no open window for %s", id)
			}
			hasMerge := func() bool {
				for _, a := range read().Run.NodeAttempts {
					if a.Ref.NodeID == "merge" {
						return true
					}
				}
				return false
			}
			answer("left")
			if mode == "all" {
				require.False(t, hasMerge())
				answer("right")
				require.True(t, hasMerge())
				answer("merge")
			} else {
				require.True(t, hasMerge())
				answer("merge")
				require.Equal(t, model.WorkControlDraining, read().Run.ControlState)
				answer("right")
			}
			result := read()
			require.Equal(t, model.WorkRunSucceeded, result.Run.State)
			count := 0
			for _, a := range result.Run.NodeAttempts {
				if a.Ref.NodeID == "merge" {
					count++
				}
			}
			require.Equal(t, 1, count, "late arrivals cannot replay the retained reducer")
		})
	}
}
