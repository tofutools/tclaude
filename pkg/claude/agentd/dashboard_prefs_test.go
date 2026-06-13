package agentd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Functional tests for /api/dashboard/prefs — the DB-backed replacement
// for the dashboard's localStorage view/config prefs (prefs.js). Same
// harness as the slop-volume tests: setupTestDB isolates the SQLite
// store, withDashboardAuth satisfies the cookie gate.

func serveDashboardPrefs(t *testing.T, method, body string) (*httptest.ResponseRecorder, map[string]json.RawMessage) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/dashboard/prefs", handleDashboardPrefsAPI)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, dashboardRequest(method, "/api/dashboard/prefs", body))
	out := map[string]json.RawMessage{}
	if w.Body.Len() > 0 {
		_ = json.Unmarshal(w.Body.Bytes(), &out)
	}
	return w, out
}

// getPrefsMap GETs the whole pref map as plain strings.
func getPrefsMap(t *testing.T) map[string]string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/dashboard/prefs", handleDashboardPrefsAPI)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, dashboardRequest(http.MethodGet, "/api/dashboard/prefs", ""))
	require.Equal(t, http.StatusOK, w.Code, "GET prefs; body=%s", w.Body.String())
	out := map[string]string{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	return out
}

func TestDashboardPrefs_GetEmptyIsObject(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	w, _ := serveDashboardPrefs(t, http.MethodGet, "")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	// Must be an object, never null — the client spreads it into a cache.
	assert.Equal(t, "{}", strings.TrimSpace(w.Body.String()))
}

func TestDashboardPrefs_SetGetOverwriteDelete(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	// Upsert two keys.
	w, _ := serveDashboardPrefs(t, http.MethodPost, `{"key":"tclaude.dash.sort","value":"{\"col\":\"name\"}"}`)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	w, _ = serveDashboardPrefs(t, http.MethodPost, `{"key":"tclaude.dash.group.x","value":"1"}`)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	prefs := getPrefsMap(t)
	assert.Equal(t, `{"col":"name"}`, prefs["tclaude.dash.sort"])
	assert.Equal(t, "1", prefs["tclaude.dash.group.x"])

	// Overwrite one.
	w, _ = serveDashboardPrefs(t, http.MethodPost, `{"key":"tclaude.dash.group.x","value":"0"}`)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, "0", getPrefsMap(t)["tclaude.dash.group.x"], "overwrite wins")

	// An explicit empty-string value is stored (distinct from delete).
	w, _ = serveDashboardPrefs(t, http.MethodPost, `{"key":"tclaude.dash.filter.groups","value":""}`)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	v, present := getPrefsMap(t)["tclaude.dash.filter.groups"]
	assert.True(t, present, "empty-string value is stored, not dropped")
	assert.Equal(t, "", v)

	// value:null deletes (mirrors removeItem).
	w, _ = serveDashboardPrefs(t, http.MethodPost, `{"key":"tclaude.dash.group.x","value":null}`)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	_, present = getPrefsMap(t)["tclaude.dash.group.x"]
	assert.False(t, present, "value:null deletes the key")

	// Deleting a missing key is a no-op 200.
	w, _ = serveDashboardPrefs(t, http.MethodPost, `{"key":"tclaude.dash.nope","value":null}`)
	require.Equal(t, http.StatusOK, w.Code, "deleting a missing key is a no-op")
}

func TestDashboardPrefs_RejectsBadInput(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	cases := map[string]string{
		"missing key": `{"value":"1"}`,
		"empty key":   `{"key":"","value":"1"}`,
		"not json":    `nope`,
	}
	for name, body := range cases {
		w, _ := serveDashboardPrefs(t, http.MethodPost, body)
		assert.Equal(t, http.StatusBadRequest, w.Code, "%s must 400; body=%s", name, w.Body.String())
	}

	// Oversized value is rejected and nothing is stored.
	big := strings.Repeat("x", maxPrefValueLen+1)
	w, _ := serveDashboardPrefs(t, http.MethodPost, `{"key":"tclaude.dash.k","value":"`+big+`"}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, "oversized value must 400")
	_, present := getPrefsMap(t)["tclaude.dash.k"]
	assert.False(t, present, "a rejected oversized value must not be stored")
}
