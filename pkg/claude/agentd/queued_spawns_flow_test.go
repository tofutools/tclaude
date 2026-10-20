package agentd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
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

// A queued spawn keeps its attachments past the staging batch: queueing copies
// each daemon-staged upload into the queue's own store, a launch re-stages the
// stored files as a fresh daemon-issued batch, and editing or deleting the
// queued spawn drops the files it no longer lists.
func TestQueuedSpawnAttachmentsPersistAndRestage(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)
	_ = newFlow(t)
	staging, store := testutil.CanonicalTempDir(t), testutil.CanonicalTempDir(t)
	t.Cleanup(agentd.SetQueuedSpawnAttachmentBasesForTest(staging, store))
	mux := agentd.BuildDashboardHandlerForTest()

	raw, contentType := terminalFileMultipart(t, "spec.md", "text/markdown", "# the spec")
	req := httptest.NewRequest(http.MethodPost, "/api/spawn-attachments", bytes.NewReader(raw))
	req.Header.Set("Content-Type", contentType)
	rec := testharness.Serve(mux, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var upload struct {
		Files []struct{ Name, Path string } `json:"files"`
	}
	testharness.DecodeJSON(t, rec, &upload)
	require.Len(t, upload.Files, 1)

	draft := map[string]any{"group": "infra", "name": "later", "attachments": []map[string]any{
		{"name": "spec.md", "size": 10, "staged": upload.Files[0].Path},
	}}
	rec = testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost, "/api/queued-spawns", map[string]any{"draft": draft}))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created queuedSpawnWire
	testharness.DecodeJSON(t, rec, &created)
	require.JSONEq(t, `{"group":"infra","name":"later","attachments":[{"name":"spec.md","size":10}]}`, string(created.Draft))
	path := "/api/queued-spawns/" + strconv.FormatInt(created.ID, 10)
	dir := filepath.Join(store, strconv.FormatInt(created.ID, 10))

	// The staging batch can expire; the queued copy survives it.
	require.NoError(t, os.RemoveAll(filepath.Dir(upload.Files[0].Path)))
	rec = testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost, path+"/stage-attachments", map[string]any{"names": []string{"spec.md"}}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var staged struct {
		Files []struct{ Name, Path string } `json:"files"`
	}
	testharness.DecodeJSON(t, rec, &staged)
	require.Len(t, staged.Files, 1)
	require.True(t, strings.HasPrefix(staged.Files[0].Path, staging+string(filepath.Separator)), staged.Files[0].Path)
	body, err := os.ReadFile(staged.Files[0].Path)
	require.NoError(t, err)
	require.Equal(t, "# the spec", string(body))

	for _, names := range [][]string{{"missing.md"}, {"../" + strconv.FormatInt(created.ID, 10) + "/spec.md"}, {}} {
		rec = testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost, path+"/stage-attachments", map[string]any{"names": names}))
		require.NotEqual(t, http.StatusOK, rec.Code, names)
	}

	// Only daemon-issued staging paths are copied into the queue.
	forged := filepath.Join(t.TempDir(), "secret.txt")
	require.NoError(t, os.WriteFile(forged, []byte("x"), 0o600))
	for _, entry := range []map[string]any{{"name": "secret.txt", "staged": forged}, {"name": "other.md"}} {
		bad := map[string]any{"group": "infra", "attachments": []map[string]any{entry}}
		rec = testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPut, path, map[string]any{"draft": bad}))
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	}
	_, err = os.Stat(filepath.Join(dir, "spec.md"))
	require.NoError(t, err, "a rejected edit leaves the stored files alone")

	delete(draft, "attachments")
	rec = testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPut, path, map[string]any{"draft": draft}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	_, err = os.Stat(dir)
	require.True(t, os.IsNotExist(err), "removing every attachment drops the store")

	require.Equal(t, http.StatusNoContent, testharness.Serve(mux, testharness.JSONRequest(t, http.MethodDelete, path, nil)).Code)
	rec = testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost, path+"/stage-attachments", map[string]any{"names": []string{"spec.md"}}))
	require.Equal(t, http.StatusNotFound, rec.Code)
}
