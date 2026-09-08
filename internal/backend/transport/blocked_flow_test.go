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

func TestPublicBlockedProgramResolutionSurvivesRestartAndExactRetry(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "state.sqlite")
	var store *sqlite.Store
	var service *app.Service
	var handler *Handler
	open := func() {
		var err error
		store, err = sqlite.Open(path)
		require.NoError(t, err)
		service = app.New(store, providers.NewRegistry()).WithProgramHost(host.ProgramProcessHost{PrivateRoot: filepath.Join(root, "programs")})
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
	now := time.Now()
	cwd := t.TempDir()
	require.NoError(t, store.RegisterWorkspace(ctx, model.Workspace{ID: "workspace", State: model.WorkspaceAvailable, Observation: model.WorkspaceObservation{ActualPath: cwd, ObservedAt: now}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
	var profile app.ProgramProfileResult
	call("POST", "/v2/program-profiles", map[string]any{"request_id": "profile", "id": "check", "revision_id": "check_v1", "name": "failing check", "executable": "/bin/sh", "argument_prefix": []string{"-c", "printf bounded-output; exit 1"}, "sandbox": model.SandboxUnconfined, "working_directory": ".", "timeout": time.Minute, "output_limit_bytes": 1024, "effect_authority": []model.ProgramEffectRequirement{{Action: model.ActionExecuteProgram, Resource: model.ResourceSelector{Kind: model.ResourceWorkspace}}}}, &profile)
	ref := model.ProgramProfileRef{ProfileID: profile.Profile.ID, RevisionID: profile.Revision.ID, ContentHash: profile.Revision.ContentHash}
	graph := model.WorkGraph{CompilerVersion: "1", EntryNodeID: "check", Nodes: []model.WorkNode{{ID: "check", Kind: model.WorkNodeTask, Performer: &model.Performer{Kind: model.PerformerProgram, Program: &model.ProgramPerformer{Profile: ref}}, Waivable: true, Retry: model.RetryPolicy{MaxAttempts: 1, Retryable: []string{model.RetryableProgramFailure}}}, {ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}}}, Edges: []model.WorkEdge{{From: "check", To: "done"}}, Outcome: model.WorkGraphOutcomePolicy{RequiredNodes: []model.WorkNodeID{"check"}}}
	call("POST", "/v2/processes", map[string]any{"request_id": "start", "id": "run", "start": model.WorkStart{InlineGraph: &graph, Scope: model.WorkScope{WorkspaceID: "workspace"}, AuthorizedProgramProfiles: []model.ProgramProfileRef{ref}, Deadline: now.Add(time.Hour)}}, nil)
	var result workResultView
	// Poll on the test goroutine: Eventually can return while a slow callback
	// still uses the store, racing deferred Close and losing the real failure.
	settlementDeadline := time.Now().Add(20 * time.Second)
	for {
		_, err := service.ReconcilePendingWork(ctx)
		require.NoError(t, err)
		call("GET", "/v2/work/run", nil, &result)
		if len(result.Decisions) == 1 {
			break
		}
		require.True(t, time.Now().Before(settlementDeadline), "blocked decision did not settle: %+v", result)
		time.Sleep(20 * time.Millisecond)
	}
	require.NoError(t, store.Close())
	open()
	call("GET", "/v2/work/run", nil, &result)
	require.Len(t, result.Decisions, 1)
	window := result.Decisions[0]
	require.Equal(t, model.DecisionBlocked, window.Kind)
	body := map[string]any{"request_id": "waive", "decision_id": window.ID, "attempt": window.Attempt, "expected_window_revision": window.Revision, "expected_run_revision": result.Run.Revision, "action": "waive", "reason": "retain failed check as explicit waiver"}
	call("POST", "/v2/processes/resolve-blocked", body, &result)
	require.Equal(t, model.WorkRunFailed, result.Run.State, "waiver cannot turn required failed proof into verified success")
	call("POST", "/v2/processes/resolve-blocked", body, &result)
	require.Equal(t, model.WorkRunFailed, result.Run.State)
	body["action"] = "retry"
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	response := request(handler, "POST", "/v2/processes/resolve-blocked", string(encoded), testCredential)
	require.Equal(t, http.StatusConflict, response.Code)
}
