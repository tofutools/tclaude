package agentd_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type queuedSpawnWire struct {
	ID    int64           `json:"id"`
	Group string          `json:"group"`
	Draft json.RawMessage `json:"draft"`
}

// A queued spawn round-trips its opaque draft through the dashboard API and
// rides the snapshot until it is deleted.
func TestQueuedSpawnsDashboardCRUD(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)
	_ = newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	draft := map[string]any{"group": "infra", "name": "db-migration-2", "initialMessage": "after db-1 lands", "roleRefs": []string{"dev"}}
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost, "/api/queued-spawns", map[string]any{"draft": draft}))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created queuedSpawnWire
	testharness.DecodeJSON(t, rec, &created)
	require.Equal(t, "infra", created.Group)
	require.JSONEq(t, `{"group":"infra","name":"db-migration-2","initialMessage":"after db-1 lands","roleRefs":["dev"]}`, string(created.Draft))
	path := "/api/queued-spawns/" + strconv.FormatInt(created.ID, 10)

	var snapshot struct {
		QueuedSpawns []queuedSpawnWire `json:"queued_spawns"`
	}
	testharness.DecodeJSON(t, testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil)), &snapshot)
	require.Len(t, snapshot.QueuedSpawns, 1)
	require.Equal(t, created.ID, snapshot.QueuedSpawns[0].ID)

	draft["group"] = "frontend"
	rec = testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPut, path, map[string]any{"draft": draft}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var updated queuedSpawnWire
	testharness.DecodeJSON(t, rec, &updated)
	require.Equal(t, created.ID, updated.ID)
	require.Equal(t, "frontend", updated.Group)

	require.Equal(t, http.StatusNoContent, testharness.Serve(mux, testharness.JSONRequest(t, http.MethodDelete, path, nil)).Code)
	require.Equal(t, http.StatusNotFound, testharness.Serve(mux, testharness.JSONRequest(t, http.MethodDelete, path, nil)).Code)
	require.Equal(t, http.StatusNotFound, testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPut, path, map[string]any{"draft": draft})).Code)
	var list []queuedSpawnWire
	testharness.DecodeJSON(t, testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/queued-spawns", nil)), &list)
	require.Empty(t, list)
}

func TestQueuedSpawnsValidationAndAuth(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)
	_ = newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()
	for _, body := range []any{
		map[string]any{},
		map[string]any{"draft": "not an object"},
		map[string]any{"draft": []string{"x"}},
		map[string]any{"draft": map[string]any{"name": "no group"}},
		map[string]any{"draft": map[string]any{"group": "  "}},
	} {
		rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost, "/api/queued-spawns", body))
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	}
	require.Equal(t, http.StatusBadRequest, testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/queued-spawns/abc", nil)).Code)

	raw := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(raw)
	body := map[string]any{"draft": map[string]any{"group": "infra"}}
	for _, path := range []string{"/api/queued-spawns", "/api/queued-spawns/1"} {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
			rec := testharness.Serve(raw, testharness.JSONRequest(t, method, path, body))
			require.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, rec.Code)
		}
	}
}
