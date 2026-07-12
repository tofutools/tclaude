package agentd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	serveDashboardSandboxProfiles(rec, dashboardRequest(http.MethodPost, "/api/sandbox-profiles?dry_run=1",
		`{"name":"dev-cache","filesystem":[{"path":"`+cache+string(filepath.Separator)+`","access":"write"}],"environment":[{"name":"GOCACHE","value":"`+cache+`"}]}`))
	require.Equal(t, http.StatusOK, rec.Code, "dry-run body=%s", rec.Body.String())
	var preview sandboxProfilePreviewJSON
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &preview))
	assert.Nil(t, preview.Before)
	assert.Equal(t, canonicalCache, preview.After.Filesystem[0].Path, "preview is daemon-normalized")

	rec = httptest.NewRecorder()
	serveDashboardSandboxProfiles(rec, dashboardRequest(http.MethodGet, "/api/sandbox-profiles/dev-cache", ""))
	assert.Equal(t, http.StatusNotFound, rec.Code, "dry-run must not persist the profile")

	rec = httptest.NewRecorder()
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
	serveDashboardSandboxProfiles(rec, dashboardRequest(http.MethodPatch, "/api/sandbox-profiles/dev-cache?dry_run=1",
		`{"name":"renamed-preview","filesystem":[{"path":"`+cache+`","access":"read"}]}`))
	require.Equal(t, http.StatusOK, rec.Code, "patch dry-run body=%s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &preview))
	require.NotNil(t, preview.Before)
	assert.Equal(t, "dev-cache", preview.Before.Name)
	assert.Equal(t, "renamed-preview", preview.After.Name)
	require.NotEmpty(t, preview.Revision)

	// A real edit during the human's confirmation window invalidates the
	// preview instead of letting the stale complete replacement win.
	rec = httptest.NewRecorder()
	serveDashboardSandboxProfiles(rec, dashboardRequest(http.MethodPatch, "/api/sandbox-profiles/dev-cache",
		`{"name":"dev-cache","filesystem":[{"path":"`+cache+`","access":"write"}],"environment":[{"name":"NEWER","value":"kept"}]}`))
	require.Equal(t, http.StatusOK, rec.Code, "intervening update body=%s", rec.Body.String())
	rec = httptest.NewRecorder()
	serveDashboardSandboxProfiles(rec, dashboardRequest(http.MethodPatch,
		"/api/sandbox-profiles/dev-cache?revision="+url.QueryEscape(preview.Revision),
		`{"name":"renamed-preview","filesystem":[{"path":"`+cache+`","access":"read"}]}`))
	assert.Equal(t, http.StatusConflict, rec.Code, "stale preview must not overwrite a newer edit")

	rec = httptest.NewRecorder()
	serveDashboardSandboxProfiles(rec, dashboardRequest(http.MethodGet, "/api/sandbox-profiles/renamed-preview", ""))
	assert.Equal(t, http.StatusNotFound, rec.Code, "patch dry-run must not rename the profile")
	rec = httptest.NewRecorder()
	serveDashboardSandboxProfiles(rec, dashboardRequest(http.MethodGet, "/api/sandbox-profiles/dev-cache", ""))
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &profile))
	require.Len(t, profile.Environment, 1)
	assert.Equal(t, "NEWER", profile.Environment[0].Name, "intervening update survives stale confirmation")

	rec = httptest.NewRecorder()
	serveDashboardSandboxProfiles(rec, dashboardRequest(http.MethodGet, "/api/sandbox-profiles/export?include_assignments=true", ""))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var exported sandboxProfileExportEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &exported))
	require.NotNil(t, exported.Assignments)
	assert.Equal(t, "dev-cache", exported.Assignments.Global)
	assert.Equal(t, "dev-cache", exported.Assignments.Groups["crew"])

	// The two-second dashboard snapshot carries names and assignments for the
	// quick selectors, but not profile payloads/environment values.
	rec = httptest.NewRecorder()
	handleDashboardSnapshot(rec, dashboardRequest(http.MethodGet, "/api/snapshot", ""))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var snapshot snapshotPayload
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snapshot))
	assert.Equal(t, []string{"dev-cache"}, snapshot.SandboxProfiles)
	assert.Equal(t, "dev-cache", snapshot.SandboxProfileDefault)
	require.Len(t, snapshot.Groups, 1)
	assert.Equal(t, "dev-cache", snapshot.Groups[0].SandboxProfile)
	assert.NotContains(t, rec.Body.String(), canonicalCache, "snapshot must not expose sandbox-profile payload values")
}

func TestDashboardSandboxProfilesRequireDashboardAuth(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)
	rec := httptest.NewRecorder()
	serveDashboardSandboxProfiles(rec, httptest.NewRequest(http.MethodGet, "/api/sandbox-profiles", nil))
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestDashboardSandboxProfileDraftPreviewAndAcknowledge(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)
	token := "dashboarddrafttoken"
	sandboxProfileDraftMu.Lock()
	sandboxProfileDrafts[token] = sandboxProfileDraftEntry{
		Draft: sandboxProfileDraftJSON{
			Profile: sandboxProfileJSON{Name: "new-name"},
		},
		CreatedAt: time.Now(),
	}
	sandboxProfileDraftMu.Unlock()
	t.Cleanup(func() {
		sandboxProfileDraftMu.Lock()
		delete(sandboxProfileDrafts, token)
		sandboxProfileDraftMu.Unlock()
	})

	rec := httptest.NewRecorder()
	serveDashboardSandboxProfiles(rec, dashboardRequest(http.MethodGet, "/api/sandbox-profile-drafts/"+token, ""))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.JSONEq(t, `{"profile":{"name":"new-name","filesystem":null,"environment":null}}`, rec.Body.String())

	rec = httptest.NewRecorder()
	serveDashboardSandboxProfiles(rec, dashboardRequest(http.MethodGet, "/api/sandbox-profile-drafts/"+token, ""))
	assert.Equal(t, http.StatusNotFound, rec.Code, "the first GET atomically consumes the draft")
}
