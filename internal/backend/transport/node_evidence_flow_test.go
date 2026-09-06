package transport

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
	"path/filepath"
	"testing"
	"time"
)

func TestPublicNodeEvidenceRetainsExactProof(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := sqlite.Open(filepath.Join(root, "state.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry()).WithProgramHost(host.ProgramProcessHost{PrivateRoot: filepath.Join(root, "programs")})
	h := testHandler(t, service)
	require.NoError(t, h.RegisterJourneyAPI(service))
	require.NoError(t, h.RegisterOrchestrationAPI(service))
	call := func(method, path string, body, out any) {
		t.Helper()
		data, err := json.Marshal(body)
		require.NoError(t, err)
		response := request(h, method, path, string(data), testCredential)
		require.Equal(t, 200, response.Code, response.Body.String())
		if out != nil {
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), out))
		}
	}
	now := time.Now()
	require.NoError(t, store.RegisterWorkspace(ctx, model.Workspace{ID: "workspace", State: model.WorkspaceAvailable, Observation: model.WorkspaceObservation{ActualPath: root, ObservedAt: now}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
	var profile app.ProgramProfileResult
	call("POST", "/v2/program-profiles", map[string]any{"request_id": "profile", "id": "profile", "name": "profile", "executable": "/bin/sleep", "argument_prefix": []string{"1"}, "sandbox": "unconfined", "timeout": int64(time.Minute), "output_limit_bytes": 4096, "effect_authority": []model.ProgramEffectRequirement{{Action: model.ActionExecuteProgram, Resource: model.ResourceSelector{Kind: model.ResourceWorkspace}}}}, &profile)
	ref := model.ProgramProfileRef{ProfileID: profile.Profile.ID, RevisionID: profile.Revision.ID, ContentHash: profile.Revision.ContentHash}
	graph := model.WorkGraph{CompilerVersion: "1", EntryNodeID: "program", Nodes: []model.WorkNode{{ID: "program", Kind: model.WorkNodeTask, Performer: &model.Performer{Kind: model.PerformerProgram, Program: &model.ProgramPerformer{Profile: ref}}}, {ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}}}, Edges: []model.WorkEdge{{From: "program", To: "done"}}}
	call("POST", "/v2/processes", map[string]any{"request_id": "run", "id": "run", "start": model.WorkStart{InlineGraph: &graph, Scope: model.WorkScope{WorkspaceID: "workspace"}, AuthorizedProgramProfiles: []model.ProgramProfileRef{ref}, Deadline: now.Add(time.Minute)}}, nil)
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	run, err := service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "run"})
	require.NoError(t, err)
	var response map[string]any
	call("POST", "/v2/processes/evidence", map[string]any{"request_id": "evidence", "attempt": run.Run.NodeAttempts[0].Ref, "expected_run_revision": run.Run.Revision, "kind": "verification", "artifact_revision": "artifact-123", "passed": true, "detail": "audited proof"}, &response)
	run, err = service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "run"})
	require.NoError(t, err)
	require.Len(t, run.NodeEvidence, 1)
	call("GET", "/v2/work/run", nil, &response)
	data, err := json.Marshal(response)
	require.NoError(t, err)
	require.NotContains(t, string(data), "Generation")
	require.NotContains(t, string(data), "Delegation")
	require.Contains(t, string(data), string(run.NodeEvidence[0].ID))
	require.Contains(t, string(data), "artifact-123", "public response loses evidence artifact, ID, and reporter")
}
