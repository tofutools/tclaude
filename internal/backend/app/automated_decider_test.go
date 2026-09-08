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

func TestAutomatedDeciderRetainsPinAndRefusesExecution(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "automated_decider.sqlite")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	service := app.New(store, providers.NewRegistry())
	graph := model.WorkGraph{CompilerVersion: "v1", EntryNodeID: "choose", Nodes: []model.WorkNode{
		{ID: "choose", Kind: model.WorkNodeDecision, Name: "Choose", Decision: &model.DecisionNode{Kind: model.DecisionWork, Audience: []model.DecisionAudience{{Subject: model.AuthoritySubject{Kind: model.AuthorityOperator}}}, PermittedAnswers: []string{"approve"}, ExpiresAfter: time.Hour}},
		{ID: "end", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}},
	}, Edges: []model.WorkEdge{{From: "choose", To: "end", Verdict: "approve"}}}
	policy := &model.Performer{Kind: model.PerformerAgent, Agent: &model.AgentPerformer{MemberKey: "decider", Brief: "Choose honestly", ContextPolicy: "fresh"}}
	graph.Nodes[0].Decision.Decider = policy
	draft := app.DefinitionDraft{ID: "automated_decider", RevisionID: "automated_decider_v1", Name: "Approval retry", Kind: model.DefinitionProcess, SchemaVersion: 1, Source: "kind: process", Process: &model.ProcessDefinition{Graph: graph}}
	saved, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"}, Draft: draft})
	require.NoError(t, err)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	service = app.New(store, providers.NewRegistry())
	read, err := service.GetDefinition(ctx, app.GetDefinitionRequest{Principal: model.OperatorPrincipal(), DefinitionID: "automated_decider"})
	require.NoError(t, err)
	require.Equal(t, policy, read.Revision.Process.Graph.Nodes[0].Decision.Decider)
	ref := model.DefinitionRef{DefinitionID: saved.Definition.ID, RevisionID: saved.Revision.ID, ContentHash: saved.Revision.ContentHash, Kind: model.DefinitionProcess}
	start := app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "run"}, ID: "run", Start: model.WorkStart{Definition: &ref, Deadline: time.Now().Add(time.Hour)}}
	_, err = service.StartProcess(ctx, start)
	require.ErrorIs(t, err, app.ErrUnsupported)
	_, err = service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "run"})
	require.ErrorIs(t, err, app.ErrNotFound)
	// Clearing the authored policy is an explicit new revision, not an implicit execution fallback.
	draft.Process.Graph.Nodes[0].Decision.Decider = nil
	draft.RevisionID = "automated_decider_v2"
	second, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "clear"}, Draft: draft, ExpectedRevision: 1})
	require.NoError(t, err)
	_, err = service.StartProcess(ctx, start)
	require.ErrorIs(t, err, app.ErrUnsupported, "original pin retains its automated decider declaration")
	ref.RevisionID = second.Revision.ID
	ref.ContentHash = second.Revision.ContentHash
	_, err = service.StartProcess(ctx, start)
	require.NoError(t, err)
}

func TestAutomatedDeciderValidationAndManualApprovalBoundary(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "validate.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry())
	for _, kind := range []model.PerformerKind{model.PerformerAgent, model.PerformerProgram} {
		t.Run(string(kind), func(t *testing.T) {
			performer := &model.Performer{Kind: kind}
			if kind == model.PerformerAgent {
				performer.Agent = &model.AgentPerformer{MemberKey: "worker", Brief: "{{ params.missing }}", ContextPolicy: "fresh"}
			} else {
				performer.Program = &model.ProgramPerformer{Profile: model.ProgramProfileRef{ProfileID: "profile", RevisionID: "revision", ContentHash: "hash"}, Arguments: []string{"{{ params.missing }}"}}
			}
			g := model.WorkGraph{CompilerVersion: "v1", EntryNodeID: "choose", Nodes: []model.WorkNode{{ID: "choose", Name: "Choose", Kind: model.WorkNodeDecision, Decision: &model.DecisionNode{Kind: model.DecisionWork, Decider: performer, PermittedAnswers: []string{"approve"}, ExpiresAfter: time.Hour}}, {ID: "end", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}}}, Edges: []model.WorkEdge{{From: "choose", To: "end", Verdict: "approve"}}}
			draft := app.DefinitionDraft{ID: "def", RevisionID: "rev", Name: "Decision", Kind: model.DefinitionProcess, SchemaVersion: 1, Source: "kind: process", Process: &model.ProcessDefinition{Graph: g}}
			_, err := service.ValidateDefinition(ctx, app.ValidateDefinitionRequest{Principal: model.OperatorPrincipal(), Draft: draft})
			require.NoError(t, err)
			draft.Process.ParameterSyntax = "mustache-v1"
			_, err = service.ValidateDefinition(ctx, app.ValidateDefinitionRequest{Principal: model.OperatorPrincipal(), Draft: draft})
			require.ErrorIs(t, err, app.ErrInvalid)
			draft.Process.ParameterSyntax = ""
			g.Nodes[0].Performer = performer
			_, err = service.ValidateDefinition(ctx, app.ValidateDefinitionRequest{Principal: model.OperatorPrincipal(), Draft: draft})
			require.ErrorIs(t, err, app.ErrInvalid)
			g.Nodes[0].Performer = nil
			staged := stagedHumanGraph()
			staged.Nodes[0].Stages.PlanApproval.Decider = performer
			draft.Process.Graph = staged
			_, err = service.ValidateDefinition(ctx, app.ValidateDefinitionRequest{Principal: model.OperatorPrincipal(), Draft: draft})
			require.ErrorIs(t, err, app.ErrInvalid)
		})
	}
}
