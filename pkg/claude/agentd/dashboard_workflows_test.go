package agentd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// workflowsTestMux registers the read-only workflows routes WITHOUT the cookie
// auth wrapper (auth is covered by dashboard_auth_flow_test.go) so these tests
// exercise the handler logic and the {runId} path-value dispatch directly.
func workflowsTestMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/workflows", handleDashboardWorkflowsList)
	mux.HandleFunc("GET /api/workflows/{runId}", handleDashboardWorkflowDetail)
	return mux
}

// seedWorkflowFixtures copies the ccworkflows package's vetted fixtures into the
// temp HOME the endpoint reads through (runs under ~/.claude/projects, saved
// templates under ~/.claude/workflows/saved).
func seedWorkflowFixtures(t *testing.T) {
	t.Helper()
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	require.NoError(t, os.CopyFS(filepath.Join(home, ".claude", "projects"),
		os.DirFS("../ccworkflows/testdata/projects")))
	require.NoError(t, os.CopyFS(filepath.Join(home, ".claude", "workflows", "saved"),
		os.DirFS("../ccworkflows/testdata/saved")))
}

func getWorkflows(t *testing.T, path string) (*httptest.ResponseRecorder, []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	workflowsTestMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec, rec.Body.Bytes()
}

func TestDashboardWorkflowsList(t *testing.T) {
	setupTestDB(t)
	seedWorkflowFixtures(t)

	rec, body := getWorkflows(t, "/api/workflows")
	require.Equal(t, http.StatusOK, rec.Code, string(body))

	var got workflowsListView
	require.NoError(t, json.Unmarshal(body, &got))

	assert.Len(t, got.Saved, 3, "three saved templates")
	assert.Len(t, got.Runs, 4, "four runs across the fixture session")

	byID := map[string]workflowRunView{}
	for _, r := range got.Runs {
		byID[r.RunID] = r
		// The conv join always carries the session id, even with no conv_index
		// row (graceful degradation).
		assert.NotEmpty(t, r.SessionID, "run %s should carry its sessionId", r.RunID)
		assert.NotEmpty(t, r.ProjectDir, "run %s should carry its projectDir", r.RunID)
	}

	completed := byID["wf_213c457c-3ac"]
	assert.Equal(t, "completed", completed.Status)
	assert.Equal(t, "ccwf-fixture-probe", completed.WorkflowName)
	assert.True(t, completed.HasCompletedJSON)
	assert.Equal(t, 3, completed.AgentCount)

	live := byID["wf_11ab22cd-e01"]
	assert.Equal(t, "running", live.Status)
	assert.False(t, live.HasCompletedJSON)

	assert.Equal(t, "failed", byID["wf_fa11ed00-f01"].Status)
}

func TestDashboardWorkflowDetail_Tree(t *testing.T) {
	setupTestDB(t)
	seedWorkflowFixtures(t)

	rec, body := getWorkflows(t, "/api/workflows/wf_213c457c-3ac")
	require.Equal(t, http.StatusOK, rec.Code, string(body))

	var got workflowDetailView
	require.NoError(t, json.Unmarshal(body, &got))
	require.NotNil(t, got.RunState)

	assert.Equal(t, "wf_213c457c-3ac", got.RunID)
	assert.Equal(t, "completed", string(got.Status))
	assert.Equal(t, "completed-json", got.Source)
	require.Len(t, got.Phases, 2)
	assert.Equal(t, "Scout", got.Phases[0].Title)
	assert.Equal(t, "Fan", got.Phases[1].Title)
	require.Len(t, got.Agents, 3)
	// The script is embedded for the script view.
	assert.Contains(t, got.Script, "export const meta")
	// Join carries the session id.
	assert.Equal(t, got.Join.SessionID, "11111111-1111-1111-1111-111111111111")
	// The mermaid projection is computed server-side from the same RunState.
	assert.Contains(t, got.Mermaid, "flowchart TD")
	assert.Contains(t, got.Mermaid, `P1["Phase 1: Scout"]`)
	assert.Contains(t, got.Mermaid, "P1 --> P2")
}

func TestDashboardWorkflowDetail_LiveBestEffort(t *testing.T) {
	setupTestDB(t)
	seedWorkflowFixtures(t)

	rec, body := getWorkflows(t, "/api/workflows/wf_11ab22cd-e01")
	require.Equal(t, http.StatusOK, rec.Code, string(body))

	var got workflowDetailView
	require.NoError(t, json.Unmarshal(body, &got))
	assert.Equal(t, "running", string(got.Status))
	assert.Equal(t, "journal", got.Source)
	require.Len(t, got.Agents, 2)
	assert.Equal(t, "done", string(got.Agents[0].State))
	assert.Equal(t, "running", string(got.Agents[1].State))
	assert.Equal(t, "build:a", got.Agents[0].Label)
}

func TestDashboardWorkflowDetail_NotFound(t *testing.T) {
	setupTestDB(t)
	seedWorkflowFixtures(t)

	rec, body := getWorkflows(t, "/api/workflows/wf_does-not-exist")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	var got map[string]string
	require.NoError(t, json.Unmarshal(body, &got))
	assert.Contains(t, got["error"], "not found")
}

func TestDashboardWorkflowDetail_CorruptRunIs500(t *testing.T) {
	setupTestDB(t)
	seedWorkflowFixtures(t)
	// Corrupt a real run's completed JSON: it is located by enumeration but
	// fails to parse, so the detail endpoint must 500 (found-but-load-failed),
	// not 404 (not found).
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	bad := filepath.Join(home, ".claude", "projects",
		"-Users-johkjo-fixture-proj", "11111111-1111-1111-1111-111111111111",
		"workflows", "wf_213c457c-3ac.json")
	require.NoError(t, os.WriteFile(bad, []byte("{ not valid json"), 0o644))

	rec, _ := getWorkflows(t, "/api/workflows/wf_213c457c-3ac")
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestDashboardWorkflowsList_EmptyMachine(t *testing.T) {
	setupTestDB(t) // temp HOME, no fixtures seeded
	rec, body := getWorkflows(t, "/api/workflows")
	require.Equal(t, http.StatusOK, rec.Code, string(body))
	var got workflowsListView
	require.NoError(t, json.Unmarshal(body, &got))
	// Empty machine → empty (non-null) arrays, stable shape.
	assert.NotNil(t, got.Saved)
	assert.Empty(t, got.Saved)
	assert.Empty(t, got.Runs)
}
