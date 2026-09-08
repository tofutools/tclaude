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

func TestProcessWorkerConfigurationRetainsPinAndRefusesCreation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "worker_config.sqlite")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	service := app.New(store, providers.NewRegistry())
	graph := stagedHumanGraph()
	policy := &model.DesiredConfiguration{Harness: "codex", Model: "model", Effort: "high", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite, Environment: model.Environment{"LITERAL": "$HOME"}}
	graph.Nodes[0].Performer = &model.Performer{Kind: model.PerformerAgent, Agent: &model.AgentPerformer{CreateDesired: policy, Brief: "Work", ContextPolicy: model.AgentContextFresh}}
	draft := app.DefinitionDraft{ID: "worker_config", RevisionID: "worker_config_v1", Name: "Approval retry", Kind: model.DefinitionProcess, SchemaVersion: 1, Source: "kind: process", Process: &model.ProcessDefinition{Graph: graph}}
	saved, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"}, Draft: draft})
	require.NoError(t, err)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	service = app.New(store, providers.NewRegistry())
	read, err := service.GetDefinition(ctx, app.GetDefinitionRequest{Principal: model.OperatorPrincipal(), DefinitionID: "worker_config"})
	require.NoError(t, err)
	require.Equal(t, policy, read.Revision.Process.Graph.Nodes[0].Performer.Agent.CreateDesired)
	ref := model.DefinitionRef{DefinitionID: saved.Definition.ID, RevisionID: saved.Revision.ID, ContentHash: saved.Revision.ContentHash, Kind: model.DefinitionProcess}
	start := app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "run"}, ID: "run", Start: model.WorkStart{Definition: &ref, Deadline: time.Now().Add(time.Hour)}}
	_, err = service.StartProcess(ctx, start)
	require.ErrorIs(t, err, app.ErrUnsupported)
	_, err = service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "run"})
	require.ErrorIs(t, err, app.ErrNotFound)
	// Clearing the authored policy is an explicit new revision, not an implicit execution fallback.
	draft.Process.Graph.Nodes[0].Performer = stagedHumanGraph().Nodes[0].Performer
	draft.RevisionID = "worker_config_v2"
	second, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "clear"}, Draft: draft, ExpectedRevision: 1})
	require.NoError(t, err)
	_, err = service.StartProcess(ctx, start)
	require.ErrorIs(t, err, app.ErrUnsupported, "original pin retains its approval retry declaration")
	ref.RevisionID = second.Revision.ID
	ref.ContentHash = second.Revision.ContentHash
	_, err = service.StartProcess(ctx, start)
	require.NoError(t, err)
}

func TestProcessWorkerConfigurationRejectsIncompleteOrAmbiguousIntent(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "validation.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry())
	for _, mutate := range []func(*model.AgentPerformer){
		func(a *model.AgentPerformer) { a.CreateDesired.Harness = "" },
		func(a *model.AgentPerformer) { a.CreateDesired.Effort = "--bad" },
		func(a *model.AgentPerformer) { a.AgentID = "worker" },
		func(a *model.AgentPerformer) { a.MemberKey = "worker" },
		func(a *model.AgentPerformer) { a.CreateDesired.Environment = model.Environment{"HOME": "override"} },
	} {
		graph := stagedHumanGraph()
		agent := &model.AgentPerformer{CreateDesired: &model.DesiredConfiguration{Harness: "codex", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}, Brief: "Work"}
		graph.Nodes[0].Performer = &model.Performer{Kind: model.PerformerAgent, Agent: agent}
		draft := app.DefinitionDraft{ID: "def", RevisionID: "rev", Name: "Configuration", Kind: model.DefinitionProcess, SchemaVersion: 1, Source: "kind: process", Process: &model.ProcessDefinition{Graph: graph}}
		_, err := service.ValidateDefinition(ctx, app.ValidateDefinitionRequest{Principal: model.OperatorPrincipal(), Draft: draft})
		require.NoError(t, err)
		mutate(agent)
		_, err = service.ValidateDefinition(ctx, app.ValidateDefinitionRequest{Principal: model.OperatorPrincipal(), Draft: draft})
		require.ErrorIs(t, err, app.ErrInvalid)
	}
}
