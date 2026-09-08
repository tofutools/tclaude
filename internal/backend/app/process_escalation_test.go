package app_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func escalationGraph() model.WorkGraph {
	graph := stagedHumanGraph()
	graph.Nodes = append(graph.Nodes,
		model.WorkNode{ID: "escalation", Kind: model.WorkNodeDecision, Decision: &model.DecisionNode{Kind: model.DecisionWork, Audience: []model.DecisionAudience{{Subject: model.AuthoritySubject{Kind: model.AuthorityOperator}}}, PermittedAnswers: []string{"retry", "cancel"}, ExpiresAfter: time.Hour}},
		model.WorkNode{ID: "cancelled", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeCancelled}},
	)
	graph.Edges = append(graph.Edges, model.WorkEdge{From: "task", To: "escalation", Verdict: "fail"}, model.WorkEdge{From: "escalation", To: "task", Verdict: "retry"}, model.WorkEdge{From: "escalation", To: "cancelled", Verdict: "cancel"})
	return graph
}

func TestProcessEscalationRetainsAuthoredLoopAndRefusesPinnedStart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	service := app.New(store, providers.NewRegistry())
	graph := escalationGraph()
	draft := app.DefinitionDraft{ID: "escalation", RevisionID: "v1", Name: "Escalation", Source: "authored retry/cancel", Kind: model.DefinitionProcess, SchemaVersion: 1, Process: &model.ProcessDefinition{Graph: graph}}
	saved, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"}, Draft: draft})
	require.NoError(t, err)
	require.Equal(t, graph.Edges, saved.Revision.Process.Graph.Edges)
	require.Empty(t, saved.Revision.Process.Graph.EscalationRetries)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	defer store.Close()
	service = app.New(store, providers.NewRegistry())
	read, err := service.GetDefinition(ctx, app.GetDefinitionRequest{Principal: model.OperatorPrincipal(), DefinitionID: "escalation"})
	require.NoError(t, err)
	require.Equal(t, graph.Edges, read.Revision.Process.Graph.Edges)
	ref := model.DefinitionRef{DefinitionID: saved.Definition.ID, RevisionID: saved.Revision.ID, ContentHash: saved.Revision.ContentHash, Kind: model.DefinitionProcess}
	draft.RevisionID = "v2"
	draft.Process.Graph = stagedHumanGraph()
	_, err = service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "update"}, Draft: draft, ExpectedRevision: saved.Definition.Revision})
	require.NoError(t, err)
	_, err = service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "run", Start: model.WorkStart{Definition: &ref, Deadline: time.Now().Add(time.Hour)}})
	require.ErrorIs(t, err, app.ErrUnsupported)
	_, err = service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "run"})
	require.ErrorIs(t, err, app.ErrNotFound)
}

func TestProcessEscalationRejectsOtherCyclesAndForgedCompilation(t *testing.T) {
	_, service, _ := regressionService(t)
	original := escalationGraph()
	for name, mutate := range map[string]func(*model.WorkGraph){
		"wrong-retry":  func(g *model.WorkGraph) { g.Edges[2].To = "done" },
		"wrong-cancel": func(g *model.WorkGraph) { g.Edges[3].To = "done" },
		"other-incoming": func(g *model.WorkGraph) {
			g.Edges = append(g.Edges, model.WorkEdge{From: "task", To: "escalation", Verdict: "pass"})
		},
		"extra-choice": func(g *model.WorkGraph) {
			g.Nodes[2].Decision.PermittedAnswers = append(g.Nodes[2].Decision.PermittedAnswers, "waive")
		},
		"empty-audience": func(g *model.WorkGraph) { g.Nodes[2].Decision.Audience = []model.DecisionAudience{{}} },
		"group-only":     func(g *model.WorkGraph) { g.Nodes[2].Decision.Audience = []model.DecisionAudience{{GroupID: "group"}} },
		"missing-agent": func(g *model.WorkGraph) {
			g.Nodes[2].Decision.Audience = []model.DecisionAudience{{Subject: model.AuthoritySubject{Kind: model.AuthorityAgent}}}
		},
		"mixed-subject": func(g *model.WorkGraph) {
			g.Nodes[2].Decision.Audience = []model.DecisionAudience{{Subject: model.AuthoritySubject{Kind: model.AuthorityOperator, AgentID: "agent"}}}
		},
		"invalid-agent-id": func(g *model.WorkGraph) {
			g.Nodes[2].Decision.Audience = []model.DecisionAudience{{Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: "bad id"}}}
		},
		"invalid-execution-id": func(g *model.WorkGraph) {
			g.Nodes[2].Decision.Audience = []model.DecisionAudience{{Subject: model.AuthoritySubject{Kind: model.AuthorityExecution, ExecutionID: "bad id"}}}
		},
		"invalid-role-id": func(g *model.WorkGraph) { g.Nodes[2].Decision.Audience = []model.DecisionAudience{{RoleID: "bad id"}} },
		"invalid-role-group-id": func(g *model.WorkGraph) {
			g.Nodes[2].Decision.Audience = []model.DecisionAudience{{RoleID: "role", GroupID: "bad id"}}
		},
		"not-compound": func(g *model.WorkGraph) { g.Nodes[0].Stages = nil },
		"entry":        func(g *model.WorkGraph) { g.EntryNodeID = "escalation" },
		"other-cycle":  func(g *model.WorkGraph) { g.Edges = append(g.Edges, model.WorkEdge{From: "done", To: "task"}) },
		"forged":       func(g *model.WorkGraph) { g.EscalationRetries = []model.WorkEdge{g.Edges[2]} },
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(original)
			require.NoError(t, err)
			var graph model.WorkGraph
			require.NoError(t, json.Unmarshal(raw, &graph))
			mutate(&graph)
			_, err = service.ValidateDefinition(context.Background(), app.ValidateDefinitionRequest{Principal: model.OperatorPrincipal(), Draft: app.DefinitionDraft{ID: "test", RevisionID: "v1", Name: "Test", Source: "test", Kind: model.DefinitionProcess, SchemaVersion: 1, Process: &model.ProcessDefinition{Graph: graph}}})
			require.ErrorIs(t, err, app.ErrInvalid)
		})
	}
}
