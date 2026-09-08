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

func TestApprovalRetryAuthoringRetainsPolicyAndRefusesExecution(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "approval_retry.sqlite")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	service := app.New(store, providers.NewRegistry())
	graph := stagedHumanGraph()
	policy := &model.ApprovalRetryPolicy{MaxAttempts: 3, Backoff: " 30s ", OnFail: "feedback-same-session"}
	graph.Nodes[0].Stages.Plan.ApprovalRetry = policy
	draft := app.DefinitionDraft{ID: "approval_retry", RevisionID: "approval_retry_v1", Name: "Approval retry", Kind: model.DefinitionProcess, SchemaVersion: 1, Source: "kind: process", Process: &model.ProcessDefinition{Graph: graph}}
	saved, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"}, Draft: draft})
	require.NoError(t, err)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	service = app.New(store, providers.NewRegistry())
	read, err := service.GetDefinition(ctx, app.GetDefinitionRequest{Principal: model.OperatorPrincipal(), DefinitionID: "approval_retry"})
	require.NoError(t, err)
	require.Equal(t, policy, read.Revision.Process.Graph.Nodes[0].Stages.Plan.ApprovalRetry)
	ref := model.DefinitionRef{DefinitionID: saved.Definition.ID, RevisionID: saved.Revision.ID, ContentHash: saved.Revision.ContentHash, Kind: model.DefinitionProcess}
	start := app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "run"}, ID: "run", Start: model.WorkStart{Definition: &ref, Deadline: time.Now().Add(time.Hour)}}
	_, err = service.StartProcess(ctx, start)
	require.ErrorIs(t, err, app.ErrUnsupported)
	_, err = service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "run"})
	require.ErrorIs(t, err, app.ErrNotFound)
	// Clearing the authored policy is an explicit new revision, not an implicit execution fallback.
	draft.Process.Graph.Nodes[0].Stages.Plan.ApprovalRetry = nil
	draft.RevisionID = "approval_retry_v2"
	second, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "clear"}, Draft: draft, ExpectedRevision: 1})
	require.NoError(t, err)
	_, err = service.StartProcess(ctx, start)
	require.ErrorIs(t, err, app.ErrUnsupported, "original pin retains its approval retry declaration")
	ref.RevisionID = second.Revision.ID
	ref.ContentHash = second.Revision.ContentHash
	_, err = service.StartProcess(ctx, start)
	require.NoError(t, err)
}

func TestApprovalRetryRejectsInvalidOrOrphanPolicies(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "validation.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	service := app.New(store, providers.NewRegistry())
	for _, test := range []struct {
		name  string
		alter func(*model.WorkGraph)
	}{
		{"zero", func(g *model.WorkGraph) { g.Nodes[0].Stages.Plan.ApprovalRetry.MaxAttempts = 0 }},
		{"negative delay", func(g *model.WorkGraph) { g.Nodes[0].Stages.Plan.ApprovalRetry.Backoff = "-1s" }},
		{"zero delay", func(g *model.WorkGraph) { g.Nodes[0].Stages.Plan.ApprovalRetry.Backoff = "0s" }},
		{"invalid mode", func(g *model.WorkGraph) { g.Nodes[0].Stages.Plan.ApprovalRetry.OnFail = "retry" }},
		{"without approval", func(g *model.WorkGraph) { g.Nodes[0].Stages.PlanApproval = nil }},
		{"on check", func(g *model.WorkGraph) {
			g.Nodes[0].Stages.Checks[0].ApprovalRetry = g.Nodes[0].Stages.Plan.ApprovalRetry
			g.Nodes[0].Stages.Plan.ApprovalRetry = nil
		}},
		{"on review", func(g *model.WorkGraph) {
			g.Nodes[0].Stages.Review.ApprovalRetry = g.Nodes[0].Stages.Plan.ApprovalRetry
			g.Nodes[0].Stages.Plan.ApprovalRetry = nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			g := stagedHumanGraph()
			g.Nodes[0].Stages.Plan.ApprovalRetry = &model.ApprovalRetryPolicy{MaxAttempts: 2}
			test.alter(&g)
			_, err := service.ValidateDefinition(ctx, app.ValidateDefinitionRequest{Principal: model.OperatorPrincipal(), Draft: app.DefinitionDraft{ID: "policy", RevisionID: "policy_v1", Name: "Policy", Kind: model.DefinitionProcess, SchemaVersion: 1, Process: &model.ProcessDefinition{Graph: g}}})
			require.ErrorIs(t, err, app.ErrInvalid)
		})
	}
}
