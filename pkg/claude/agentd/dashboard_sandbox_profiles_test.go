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
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func serveDashboardSandboxProfiles(w http.ResponseWriter, r *http.Request) {
	mux := http.NewServeMux()
	registerDashboardSandboxProfileRoutes(mux)
	mux.ServeHTTP(w, r)
}

func TestDashboardSandboxProfilesCRUDAndAssignments(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)
	_, err := db.CreateAgentGroup("crew", "")
	require.NoError(t, err)
	cache := filepath.Join(os.Getenv("HOME"), "shared-cache")
	require.NoError(t, os.MkdirAll(cache, 0o755))
	canonicalCache, err := filepath.EvalSymlinks(cache)
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	serveDashboardSandboxProfiles(rec, dashboardRequest(http.MethodPost, "/api/sandbox-profiles",
		`{"name":"dev-cache","filesystem":[{"path":"`+cache+`","access":"write"}],"environment":[{"name":"GOCACHE","value":"`+cache+`"}]}`))
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	for _, tc := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPut, "/api/sandbox-profile-default", `{"name":"dev-cache"}`},
		{http.MethodPut, "/api/groups/crew/sandbox-profile", `{"name":"dev-cache"}`},
	} {
		rec = httptest.NewRecorder()
		serveDashboardSandboxProfiles(rec, dashboardRequest(tc.method, tc.path, tc.body))
		require.Equalf(t, http.StatusOK, rec.Code, "%s body=%s", tc.path, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	serveDashboardSandboxProfiles(rec, dashboardRequest(http.MethodGet, "/api/sandbox-profiles/dev-cache", ""))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var profile sandboxProfileJSON
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &profile))
	assert.Equal(t, "dev-cache", profile.Name)
	require.Len(t, profile.Filesystem, 1)
	assert.Equal(t, canonicalCache, profile.Filesystem[0].Path)

	rec = httptest.NewRecorder()
	serveDashboardSandboxProfiles(rec, dashboardRequest(http.MethodGet, "/api/sandbox-profiles/export?include_assignments=true", ""))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var exported sandboxProfileExportEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &exported))
	require.NotNil(t, exported.Assignments)
	assert.Equal(t, "dev-cache", exported.Assignments.Global)
	assert.Equal(t, "dev-cache", exported.Assignments.Groups["crew"])
}

func TestDashboardSandboxProfilesRequireDashboardAuth(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)
	rec := httptest.NewRecorder()
	serveDashboardSandboxProfiles(rec, httptest.NewRequest(http.MethodGet, "/api/sandbox-profiles", nil))
	assert.Equal(t, http.StatusForbidden, rec.Code)
}
