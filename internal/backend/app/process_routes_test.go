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

func TestImportedRouteNamesDoNotAcquireControlEffects(t *testing.T) {
	for _, kind := range []string{"task", "decision"} {
		for _, label := range []string{"custom result", "cancel", "waive", "reject"} {
			t.Run(kind+"/"+label, func(t *testing.T) {
				ctx := context.Background()
				_, s, now := regressionService(t)
				source := fmt.Sprintf(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: routes
start: begin
nodes:
  begin: {type: start, next: {custom-start: work}}
  work:
    type: %s
    performer: {kind: human, ask: Continue}
    next: {%q: done}
  done: {type: end}
layout:
  edges:
    work:
      %q: {pinned: false}
`, kind, label, label)
				c, err := s.ConvertProcessImport(ctx, app.ConvertProcessImportRequest{Principal: model.OperatorPrincipal(), ID: "copy", Source: source, Bindings: map[string]processimport.Binding{"/nodes/work/performer": {Performer: model.Performer{Kind: model.PerformerHuman, Human: &model.HumanPerformer{Operator: true}}, DecisionTimeout: "1h"}}})
				require.NoError(t, err)
				require.Equal(t, "custom-start", c.Draft.Process.Graph.Edges[0].Verdict)
				require.Equal(t, label, c.Draft.Process.Graph.Edges[1].Verdict)
				native := c.Draft
				native.Process = &model.ProcessDefinition{}
				*native.Process = *c.Draft.Process
				native.Process.Graph.Nodes = append([]model.WorkNode(nil), c.Draft.Process.Graph.Nodes...)
				native.Process.Graph.Nodes[0].RoutingMode = ""
				_, nativeErr := s.ValidateDefinition(ctx, app.ValidateDefinitionRequest{Principal: model.OperatorPrincipal(), Draft: native})
				require.ErrorIs(t, nativeErr, app.ErrInvalid, "native starts retain the unlabelled route contract")
				require.Equal(t, label, c.Draft.EditorLayout.EdgeLabels[0].Edge.Verdict)
				saved, err := s.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"}, Draft: c.Draft})
				require.NoError(t, err)
				ref := model.DefinitionRef{DefinitionID: saved.Definition.ID, RevisionID: saved.Revision.ID, ContentHash: saved.Revision.ContentHash, Kind: model.DefinitionProcess}
				run, err := s.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "run", Start: model.WorkStart{Definition: &ref, Deadline: now.Add(time.Hour)}})
				require.NoError(t, err)
				require.Len(t, run.Decisions, 1)
				answer := label
				if kind == "task" {
					answer = "complete"
				}
				w := run.Decisions[0]
				_, err = s.SubmitDecision(ctx, app.SubmitDecisionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "answer"}, DecisionID: w.ID, ExpectedWindowRevision: w.Revision, ExpectedRunRevision: run.Run.Revision, Answer: answer, Reason: "chosen"})
				require.NoError(t, err)
				final, err := s.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "run"})
				require.NoError(t, err)
				require.Equal(t, model.WorkRunSucceeded, final.Run.State)
				if kind == "task" && label == "reject" {
					rejected, e := s.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start_rejection"}, ID: "rejection", Start: model.WorkStart{Definition: &ref, Deadline: now.Add(time.Hour)}})
					require.NoError(t, e)
					window := rejected.Decisions[0]
					_, e = s.SubmitDecision(ctx, app.SubmitDecisionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "reject"}, DecisionID: window.ID, ExpectedWindowRevision: window.Revision, ExpectedRunRevision: rejected.Run.Revision, Answer: "reject", Reason: "rejected work"})
					require.NoError(t, e)
					after, e := s.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "rejection"})
					require.NoError(t, e)
					require.NotEqual(t, model.WorkRunSucceeded, after.Run.State)
					for _, attempt := range after.Run.NodeAttempts {
						require.NotEqual(t, model.WorkNodeID("done"), attempt.Ref.NodeID, "a rejection cannot follow an ordinary route merely labelled reject")
					}
				}

			})
		}
	}
}

func TestImportedMultipleOrdinaryRoutesPersistButRefusePinnedAdmission(t *testing.T) {
	ctx := context.Background()
	_, s, now := regressionService(t)
	source := `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: routes
start: work
nodes:
  work:
    type: task
    performer: {kind: human, ask: Continue}
    next: {left: done, right: other}
  done: {type: end}
  other: {type: end}
`
	c, err := s.ConvertProcessImport(ctx, app.ConvertProcessImportRequest{Principal: model.OperatorPrincipal(), ID: "copy", Source: source, Bindings: map[string]processimport.Binding{"/nodes/work/performer": {Performer: model.Performer{Kind: model.PerformerHuman, Human: &model.HumanPerformer{Operator: true}}}}})
	require.NoError(t, err)
	saved, err := s.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"}, Draft: c.Draft})
	require.NoError(t, err)
	ref := model.DefinitionRef{DefinitionID: saved.Definition.ID, RevisionID: saved.Revision.ID, ContentHash: saved.Revision.ContentHash, Kind: model.DefinitionProcess}
	_, err = s.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "refused", Start: model.WorkStart{Definition: &ref, Deadline: now.Add(time.Hour)}})
	require.ErrorIs(t, err, app.ErrUnsupported)
	_, err = s.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "refused"})
	require.ErrorIs(t, err, app.ErrNotFound)
	// A later definition head cannot silently change the retained pin's contract.
	c.Draft.Process.Graph.Edges = c.Draft.Process.Graph.Edges[:1]
	for i, n := range c.Draft.Process.Graph.Nodes {
		if n.ID == "other" {
			c.Draft.Process.Graph.Nodes = append(c.Draft.Process.Graph.Nodes[:i], c.Draft.Process.Graph.Nodes[i+1:]...)
			break
		}
	}
	c.Draft.RevisionID = "new_revision"
	_, err = s.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "revise"}, ExpectedRevision: saved.Definition.Revision, Draft: c.Draft})
	require.NoError(t, err)
	_, err = s.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start_again"}, ID: "refused_again", Start: model.WorkStart{Definition: &ref, Deadline: now.Add(time.Hour)}})
	require.ErrorIs(t, err, app.ErrUnsupported)
}
