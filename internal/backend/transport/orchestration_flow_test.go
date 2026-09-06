package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestPublicProcessDecisionSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	var store *sqlite.Store
	var service *app.Service
	var handler *Handler
	open := func() {
		var err error
		store, err = sqlite.Open(path)
		require.NoError(t, err)
		service = app.New(store, providers.NewRegistry())
		handler = testHandler(t, service)
		require.NoError(t, handler.RegisterJourneyAPI(service))
		require.NoError(t, handler.RegisterOrchestrationAPI(service))
	}
	open()
	defer func() { require.NoError(t, store.Close()) }()
	call := func(method, path string, body, out any) {
		t.Helper()
		data, err := json.Marshal(body)
		require.NoError(t, err)
		response := request(handler, method, path, string(data), testCredential)
		require.Less(t, response.Code, 300, "%s: %s", path, response.Body)
		if out != nil {
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), out))
		}
	}
	graph := model.WorkGraph{CompilerVersion: "1", EntryNodeID: "approve", Nodes: []model.WorkNode{
		{ID: "approve", Kind: model.WorkNodeDecision, Name: "Approve result", Decision: &model.DecisionNode{Kind: model.DecisionWork, Audience: []model.DecisionAudience{{Subject: model.AuthoritySubject{Kind: model.AuthorityOperator}}}, PermittedAnswers: []string{"approve"}, ExpiresAfter: time.Hour}},
		{ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}},
	}, Edges: []model.WorkEdge{{From: "approve", To: "done"}}}
	draft := app.DefinitionDraft{ID: "review_flow", Name: "Review flow", Kind: model.DefinitionProcess, SchemaVersion: 1, Source: "operator-authored fixture", Process: &model.ProcessDefinition{Graph: graph}}
	var definition app.DefinitionResult
	call("POST", "/v2/definitions", map[string]any{"request_id": "save_flow", "draft": draft, "expected_revision": 0}, &definition)
	var listed []model.Definition
	call("GET", "/v2/definitions", nil, &listed)
	require.Len(t, listed, 1)
	start := map[string]any{"request_id": "start_flow", "id": "run_flow", "start": model.WorkStart{Definition: &model.DefinitionRef{Kind: model.DefinitionProcess, DefinitionID: definition.Definition.ID, RevisionID: definition.Revision.ID, ContentHash: definition.Revision.ContentHash}, Deadline: time.Now().Add(time.Hour)}}
	call("POST", "/v2/processes", start, nil)
	_, err := service.ReconcilePendingWork(context.Background())
	require.NoError(t, err)
	var decisions []app.DecisionResult
	call("GET", "/v2/decisions", nil, &decisions)
	require.Len(t, decisions, 1)
	require.NoError(t, store.Close())
	open()
	call("GET", "/v2/decisions", nil, &decisions)
	require.Len(t, decisions, 1)
	window := decisions[0].Window
	decision := map[string]any{"request_id": "approve_flow", "decision_id": window.ID, "expected_window_revision": window.Revision, "answer": "approve", "reason": "checked"}
	call("POST", "/v2/decisions/submit", decision, nil)
	call("POST", "/v2/decisions/submit", decision, nil)
	_, err = service.ReconcilePendingWork(context.Background())
	require.NoError(t, err)
	result, err := service.InspectWork(context.Background(), app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "run_flow"})
	require.NoError(t, err)
	require.Equal(t, model.WorkRunSucceeded, result.Run.State)
	forbidden := request(handler, "GET", "/v2/definitions", "", "")
	require.Equal(t, http.StatusUnauthorized, forbidden.Code)
}

func TestPublicProgramGraphRunsRealHostAndReleasesWorkspace(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := sqlite.Open(filepath.Join(root, "state.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry()).WithProgramHost(host.ProgramProcessHost{PrivateRoot: filepath.Join(root, "programs")})
	handler := testHandler(t, service)
	require.NoError(t, handler.RegisterJourneyAPI(service))
	require.NoError(t, handler.RegisterOrchestrationAPI(service))
	call := func(path string, body, out any) {
		t.Helper()
		data, err := json.Marshal(body)
		require.NoError(t, err)
		response := request(handler, "POST", path, string(data), testCredential)
		require.Less(t, response.Code, 300, "%s: %s", path, response.Body)
		if out != nil {
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), out))
		}
	}
	now := time.Now()
	require.NoError(t, store.RegisterWorkspace(ctx, model.Workspace{ID: "program_workspace", State: model.WorkspaceAvailable, Observation: model.WorkspaceObservation{ActualPath: root, ObservedAt: now}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
	var profile app.ProgramProfileResult
	call("/v2/program-profiles", map[string]any{"request_id": "program_profile", "id": "fixture", "revision_id": "fixture_v1", "name": "fixture", "executable": "/bin/sh", "argument_prefix": []string{"-c", "printf public-program"}, "sandbox": "unconfined", "timeout": int64(time.Minute), "output_limit_bytes": 4096, "effect_authority": []model.ProgramEffectRequirement{{Action: model.ActionExecuteProgram, Resource: model.ResourceSelector{Kind: model.ResourceWorkspace}}}}, &profile)
	ref := model.ProgramProfileRef{ProfileID: profile.Profile.ID, RevisionID: profile.Revision.ID, ContentHash: profile.Revision.ContentHash}
	graph := model.WorkGraph{CompilerVersion: "1", EntryNodeID: "program", Nodes: []model.WorkNode{
		{ID: "program", Kind: model.WorkNodeTask, Performer: &model.Performer{Kind: model.PerformerProgram, Program: &model.ProgramPerformer{Profile: ref}}},
		{ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}},
	}, Edges: []model.WorkEdge{{From: "program", To: "done"}}, Outcome: model.WorkGraphOutcomePolicy{RequiredNodes: []model.WorkNodeID{"program"}}}
	call("/v2/processes", map[string]any{"request_id": "run_program", "id": "real_program", "start": model.WorkStart{InlineGraph: &graph, Scope: model.WorkScope{WorkspaceID: "program_workspace"}, AuthorizedProgramProfiles: []model.ProgramProfileRef{ref}, Deadline: time.Now().Add(time.Minute)}}, nil)
	require.Eventually(t, func() bool {
		_, err := service.ReconcilePendingWork(ctx)
		if err != nil {
			return false
		}
		result, err := service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "real_program"})
		return err == nil && result.Run.State == model.WorkRunSucceeded
	}, 10*time.Second, 20*time.Millisecond)
	uses, err := store.ActiveWorkspaceUses(ctx, "program_workspace")
	require.NoError(t, err)
	require.Empty(t, uses)
	response := request(handler, "GET", "/v2/work/real_program", "", testCredential)
	require.Equal(t, http.StatusOK, response.Code)
	require.Contains(t, response.Body.String(), "node_attempts")
	require.NotContains(t, response.Body.String(), "resource_root")
}
