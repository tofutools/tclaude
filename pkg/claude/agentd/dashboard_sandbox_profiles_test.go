package agentd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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
		`{"name":"dev-cache","filesystem":[{"path":"`+cache+`","access":"write"}],"environment":[{"name":"GOCACHE","value":"`+cache+`"}],"agent_directories":["GOLANGCI_LINT_CACHE"],"network_access":"internet"}`))
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
	assert.Equal(t, []string{"GOLANGCI_LINT_CACHE"}, profile.AgentDirectories)
	assert.Equal(t, "internet", string(profile.NetworkAccess))

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

func TestDashboardSandboxProfileMissingDirectoriesCanBeSavedAndCreatedExplicitly(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)
	missing := filepath.Join(t.TempDir(), "nested", "cache")
	body := `{"name":"future-cache","filesystem":[{"path":"` + missing + `","access":"write"}]}`
	directoryBody := `{"name":"","filesystem":[{"path":"` + missing + `","access":"write"}],"environment":[{"name":"HOME","value":"in-progress-invalid-edit"}]}`

	rec := httptest.NewRecorder()
	serveDashboardSandboxProfiles(rec, dashboardRequest(http.MethodPost, "/api/sandbox-profiles", body))
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), missing)
	_, err := os.Stat(missing)
	require.ErrorIs(t, err, os.ErrNotExist, "saving a profile must not create its directories")

	rec = httptest.NewRecorder()
	serveDashboardSandboxProfiles(rec, dashboardRequest(http.MethodPost, "/api/sandbox-profile-directories/inspect", directoryBody))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), missing)
	_, err = os.Stat(missing)
	require.ErrorIs(t, err, os.ErrNotExist, "inspection must stay side-effect free")

	rec = httptest.NewRecorder()
	serveDashboardSandboxProfiles(rec, dashboardRequest(http.MethodPost, "/api/sandbox-profile-directories/create", directoryBody))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	info, err := os.Stat(missing)
	require.NoError(t, err)
	assert.True(t, info.IsDir())

	rec = httptest.NewRecorder()
	serveDashboardSandboxProfiles(rec, dashboardRequest(http.MethodPost, "/api/sandbox-profile-directories/inspect", body))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.JSONEq(t, `{"missing":[],"creatable":[]}`, rec.Body.String())
}

func TestDashboardSandboxProfileDirectoryCreationSkipsDenyRules(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	readPath := filepath.Join(root, "read-cache")
	denyPath := filepath.Join(root, "deny-cache")
	body := `{"filesystem":[{"path":"` + readPath + `","access":"read"},{"path":"` + denyPath + `","access":"deny"}]}`

	rec := httptest.NewRecorder()
	serveDashboardSandboxProfiles(rec, dashboardRequest(http.MethodPost, "/api/sandbox-profile-directories/inspect", body))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.JSONEq(t, `{"missing":["`+denyPath+`","`+readPath+`"],"creatable":["`+readPath+`"]}`, rec.Body.String())

	rec = httptest.NewRecorder()
	serveDashboardSandboxProfiles(rec, dashboardRequest(http.MethodPost, "/api/sandbox-profile-directories/create", body))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.JSONEq(t, `{"created":["`+readPath+`"]}`, rec.Body.String())
	require.DirExists(t, readPath)
	_, err = os.Lstat(denyPath)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestDashboardSandboxProfileDirectoryCreationRejectsSymlinkSubstitution(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)
	base := t.TempDir()
	victim := t.TempDir()
	missing := filepath.Join(base, "swapped", "cache")
	body := `{"filesystem":[{"path":"` + missing + `","access":"write"}]}`

	previous := sandboxProfileBeforeMkdir
	sandboxProfileBeforeMkdir = func(string) {
		require.NoError(t, os.Symlink(victim, filepath.Join(base, "swapped")))
	}
	t.Cleanup(func() { sandboxProfileBeforeMkdir = previous })

	rec := httptest.NewRecorder()
	serveDashboardSandboxProfiles(rec, dashboardRequest(http.MethodPost, "/api/sandbox-profile-directories/create", body))
	require.Equal(t, http.StatusInternalServerError, rec.Code, "body=%s", rec.Body.String())
	_, err := os.Stat(filepath.Join(victim, "cache"))
	require.ErrorIs(t, err, os.ErrNotExist, "a substituted symlink must not redirect directory creation")
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

func TestDashboardSandboxProfileIncludesCRUDAndTransitiveExport(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)
	cache := filepath.Join(os.Getenv("HOME"), "include-cache")
	require.NoError(t, os.MkdirAll(cache, 0o755))

	rec := httptest.NewRecorder()
	serveDashboardSandboxProfiles(rec, dashboardRequest(http.MethodPost, "/api/sandbox-profiles",
		`{"name":"base","filesystem":[{"path":"`+cache+`","access":"read"}],"environment":[{"name":"LAYER","value":"base"}]}`))
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	rec = httptest.NewRecorder()
	serveDashboardSandboxProfiles(rec, dashboardRequest(http.MethodPost, "/api/sandbox-profiles",
		`{"name":"team","includes":["base"],"environment":[{"name":"LAYER","value":"team"}]}`))
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	// The include list survives the wire round-trip in authored order.
	rec = httptest.NewRecorder()
	serveDashboardSandboxProfiles(rec, dashboardRequest(http.MethodGet, "/api/sandbox-profiles/team", ""))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var team sandboxProfileJSON
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &team))
	assert.Equal(t, []string{"base"}, team.Includes)

	// Dangling and cyclic includes are rejected as invalid input, not IO errors.
	rec = httptest.NewRecorder()
	serveDashboardSandboxProfiles(rec, dashboardRequest(http.MethodPost, "/api/sandbox-profiles",
		`{"name":"broken","includes":["ghost"]}`))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())

	rec = httptest.NewRecorder()
	serveDashboardSandboxProfiles(rec, dashboardRequest(http.MethodPatch, "/api/sandbox-profiles/base",
		`{"name":"base","includes":["team"]}`))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())

	// Deleting a profile another one includes is refused with a clear conflict.
	rec = httptest.NewRecorder()
	serveDashboardSandboxProfiles(rec, dashboardRequest(http.MethodDelete, "/api/sandbox-profiles/base", ""))
	assert.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "team")

	// A named export follows includes so the bundle stays self-contained.
	rec = httptest.NewRecorder()
	serveDashboardSandboxProfiles(rec, dashboardRequest(http.MethodGet, "/api/sandbox-profiles/export?name=team", ""))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var exported sandboxProfileExportEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &exported))
	names := make([]string, 0, len(exported.Profiles))
	for _, p := range exported.Profiles {
		names = append(names, p.Name)
	}
	assert.ElementsMatch(t, []string{"team", "base"}, names)
}

func TestDashboardSandboxProfileImportInspectValidatesIncludeGraph(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)
	envelope := func(profiles string) string {
		return `{"format":"tclaude-sandbox-profiles","format_version":1,"profiles":[` + profiles + `]}`
	}

	// A self-contained bundle with cross-references previews clean.
	rec := httptest.NewRecorder()
	serveDashboardSandboxProfiles(rec, dashboardRequest(http.MethodPost, "/api/sandbox-profiles/import/inspect",
		envelope(`{"name":"team","includes":["base"]},{"name":"base"}`)))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	// The preview gates Import, so graph problems that fail under EVERY
	// conflict policy must fail inspection too: a dangling include, a
	// two-profile cycle among new profiles, and a duplicated bundle name.
	for name, profiles := range map[string]string{
		"dangling":  `{"name":"orphan","includes":["nowhere"]}`,
		"cycle":     `{"name":"a","includes":["b"]},{"name":"b","includes":["a"]}`,
		"duplicate": `{"name":"twin"},{"name":"twin"}`,
	} {
		rec = httptest.NewRecorder()
		serveDashboardSandboxProfiles(rec, dashboardRequest(http.MethodPost, "/api/sandbox-profiles/import/inspect", envelope(profiles)))
		assert.Equalf(t, http.StatusBadRequest, rec.Code, "%s: body=%s", name, rec.Body.String())
	}

	// Inspection never writes.
	rec = httptest.NewRecorder()
	serveDashboardSandboxProfiles(rec, dashboardRequest(http.MethodGet, "/api/sandbox-profiles", ""))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "[]", strings.TrimSpace(rec.Body.String()))
}

// A bundle invalid under "overwrite" but importable with "skip" must preview
// as 200 with a per-policy error, and the subsequent skip import must succeed
// — the inspect-vs-import regression from the cold review.
func TestDashboardSandboxProfileImportInspectReportsPerPolicyErrors(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)
	rec := httptest.NewRecorder()
	serveDashboardSandboxProfiles(rec, dashboardRequest(http.MethodPost, "/api/sandbox-profiles", `{"name":"A"}`))
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	envelope := `{"format":"tclaude-sandbox-profiles","format_version":1,"profiles":[` +
		`{"name":"A","includes":["B"]},{"name":"B","includes":["A"]}]}`
	rec = httptest.NewRecorder()
	serveDashboardSandboxProfiles(rec, dashboardRequest(http.MethodPost, "/api/sandbox-profiles/import/inspect", envelope))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var inspected struct {
		IncludeErrors map[string]string `json:"include_errors"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &inspected))
	assert.Contains(t, inspected.IncludeErrors["overwrite"], "cycle")
	assert.NotContains(t, inspected.IncludeErrors, "skip")

	rec = httptest.NewRecorder()
	serveDashboardSandboxProfiles(rec, dashboardRequest(http.MethodPost, "/api/sandbox-profiles/import",
		`{"format":"tclaude-sandbox-profiles","format_version":1,"on_conflict":"skip","profiles":[`+
			`{"name":"A","includes":["B"]},{"name":"B","includes":["A"]}]}`))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"skipped":["A"]`)
}
