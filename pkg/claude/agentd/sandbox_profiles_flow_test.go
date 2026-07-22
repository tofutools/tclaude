package agentd_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type wireSandboxProfile struct {
	Name             string   `json:"name"`
	AgentDirectories []string `json:"agent_directories"`
	NetworkAccess    string   `json:"network_access"`
	Filesystem       []struct {
		Path   string `json:"path"`
		Access string `json:"access"`
	} `json:"filesystem"`
	Environment []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"environment"`
}

func TestSandboxProfilesPayloadReadsAndMutationsRequireDedicatedPermission(t *testing.T) {
	f := newFlow(t)
	const peer = "sandbox-profile-gate-aaaa-bbbb"
	f.HaveConvWithTitle(peer, "peer")
	_, err := db.CreateAgentGroup("exists", "")
	require.NoError(t, err)
	for _, req := range []*http.Request{
		testharness.JSONRequest(t, http.MethodGet, "/v1/sandbox-profile-read-exclusions", nil),
		testharness.JSONRequest(t, http.MethodGet, "/v1/sandbox-profiles", nil),
		testharness.JSONRequest(t, http.MethodGet, "/v1/sandbox-profiles/anything", nil),
		testharness.JSONRequest(t, http.MethodGet, "/v1/sandbox-profiles/export", nil),
		testharness.JSONRequest(t, http.MethodPost, "/v1/sandbox-profiles/import/inspect", nil),
		testharness.JSONRequest(t, http.MethodPut, "/v1/groups/exists/sandbox-profile", map[string]any{"name": "x"}),
		testharness.JSONRequest(t, http.MethodPut, "/v1/groups/missing/sandbox-profile", map[string]any{"name": "x"}),
	} {
		rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(req, peer))
		assert.Equalf(t, http.StatusForbidden, rec.Code, "%s %s body=%s", req.Method, req.URL.Path, rec.Body.String())
	}
}

func TestSandboxProfileReadExclusionCatalog(t *testing.T) {
	f := newFlow(t)
	rec := profileReq(t, f, http.MethodGet, "/v1/sandbox-profile-read-exclusions", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var catalog struct {
		Version       int `json:"version"`
		Platform      string
		Categories    []map[string]any `json:"categories"`
		Informational []map[string]any `json:"informational"`
	}
	testharness.DecodeJSON(t, rec, &catalog)
	assert.Equal(t, 1, catalog.Version)
	assert.NotEmpty(t, catalog.Platform)
	require.Len(t, catalog.Categories, 7)
	assert.Equal(t, "secrets.ssh", catalog.Categories[0]["id"])
	assert.Equal(t, "home.directory", catalog.Categories[6]["id"])
	assert.NotEmpty(t, catalog.Categories[6]["paths"])
	assert.NotEmpty(t, catalog.Informational)
}

func TestSandboxProfileDraftPermissionCanOnlySubmitValidatedDraft(t *testing.T) {
	f := newFlow(t)
	const peer = "sandbox-drafter-aaaa-bbbb"
	f.HaveConvWithTitle(peer, "sandbox-scribe")
	require.NoError(t, db.GrantAgentPermission(peer, agentd.PermSandboxProfilesDraft, "test"))
	token := "abcdefghijklmnop"

	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/sandbox-profile-drafts/"+token, map[string]any{
			"profile": map[string]any{
				"name": "proposed", "filesystem": []any{},
				"environment": []map[string]any{{"name": "CACHE_DIR", "value": "/tmp/cache"}},
			},
		}), peer))
	require.Equalf(t, http.StatusAccepted, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "has not been saved")

	// Draft permission is not policy-management permission: registry reads and
	// all CRUD/assignment surfaces remain forbidden, and no profile was saved.
	for _, req := range []*http.Request{
		testharness.JSONRequest(t, http.MethodGet, "/v1/sandbox-profiles", nil),
		testharness.JSONRequest(t, http.MethodPost, "/v1/sandbox-profiles", map[string]any{"name": "proposed"}),
		testharness.JSONRequest(t, http.MethodPut, "/v1/sandbox-profile-default", map[string]any{"name": "proposed"}),
	} {
		denied := testharness.Serve(f.Mux, agentd.AsAgentPeer(req, peer))
		assert.Equalf(t, http.StatusForbidden, denied.Code, "%s %s body=%s", req.Method, req.URL.Path, denied.Body.String())
	}
	missing := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/sandbox-profiles/proposed", nil)))
	assert.Equal(t, http.StatusNotFound, missing.Code, "draft submission must not persist a profile")
}

func TestSandboxProfilesCRUDValidationAndAssignments(t *testing.T) {
	f := newFlow(t)
	home := os.Getenv("HOME")
	cache := filepath.Join(home, "shared-cache")
	require.NoError(t, os.MkdirAll(cache, 0o755))
	canonicalCache, err := filepath.EvalSymlinks(cache)
	require.NoError(t, err)
	_, err = db.CreateAgentGroup("crew", "")
	require.NoError(t, err)

	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name": "dev-caches",
		"filesystem": []map[string]any{
			{"path": cache, "access": "read"},
			{"path": cache + string(filepath.Separator), "access": "write"},
		},
		"environment": []map[string]any{
			{"name": "GOCACHE", "value": cache},
			{"name": "GOCACHE", "value": cache},
		},
		"agent_directories": []string{"GOLANGCI_LINT_CACHE"},
		"network_access":    "internet",
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "create body=%s", rec.Body.String())

	rec = profileReq(t, f, http.MethodGet, "/v1/sandbox-profiles/dev-caches", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var got wireSandboxProfile
	testharness.DecodeJSON(t, rec, &got)
	require.Len(t, got.Filesystem, 1)
	assert.Equal(t, canonicalCache, got.Filesystem[0].Path)
	assert.Equal(t, "write", got.Filesystem[0].Access)
	require.Len(t, got.Environment, 1)
	assert.Equal(t, "GOCACHE", got.Environment[0].Name)
	assert.Equal(t, []string{"GOLANGCI_LINT_CACHE"}, got.AgentDirectories)
	assert.Equal(t, "internet", got.NetworkAccess)

	for _, body := range []map[string]any{
		{"name": "export"},
		{"name": "bad-network", "network_access": "local-only"},
		{"name": "IMPORT"},
		{"name": "protected", "filesystem": []map[string]any{{"path": filepath.Join(home, ".tclaude", "data"), "access": "write"}}},
		{"name": "reserved", "environment": []map[string]any{{"name": "TCLAUDE_SESSION_ID", "value": "spoof"}}},
		{"name": "conflict", "environment": []map[string]any{{"name": "A", "value": "1"}, {"name": "A", "value": "2"}}},
		{"name": "agent-dir-conflict", "environment": []map[string]any{{"name": "GOCACHE", "value": cache}}, "agent_directories": []string{"GOCACHE"}},
	} {
		rec = profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", body)
		assert.Equalf(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	}

	require.Equal(t, http.StatusOK, profileReq(t, f, http.MethodPut,
		"/v1/sandbox-profile-default", map[string]any{"name": "dev-caches"}).Code)
	require.Equal(t, http.StatusOK, profileReq(t, f, http.MethodPut,
		"/v1/groups/crew/sandbox-profile", map[string]any{"name": "dev-caches"}).Code)

	rec = profileReq(t, f, http.MethodPatch, "/v1/sandbox-profiles/dev-caches", map[string]any{
		"name": "renamed", "filesystem": []map[string]any{{"path": cache, "access": "write"}},
	})
	require.Equalf(t, http.StatusOK, rec.Code, "rename body=%s", rec.Body.String())
	for _, path := range []string{"/v1/sandbox-profile-default", "/v1/groups/crew/sandbox-profile"} {
		rec = profileReq(t, f, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var ref struct {
			Name string `json:"name"`
		}
		testharness.DecodeJSON(t, rec, &ref)
		assert.Equal(t, "renamed", ref.Name)
	}

	require.Equal(t, http.StatusNoContent,
		profileReq(t, f, http.MethodDelete, "/v1/sandbox-profiles/renamed", nil).Code)
	for _, path := range []string{"/v1/sandbox-profile-default", "/v1/groups/crew/sandbox-profile"} {
		rec = profileReq(t, f, http.MethodGet, path, nil)
		var ref struct {
			Name string `json:"name"`
		}
		testharness.DecodeJSON(t, rec, &ref)
		assert.Empty(t, ref.Name, "delete atomically clears assignment at %s", path)
	}
}

func TestSandboxProfilesExportImportRoundTrip(t *testing.T) {
	f := newFlow(t)
	cache := filepath.Join(os.Getenv("HOME"), "cache")
	require.NoError(t, os.MkdirAll(cache, 0o755))
	canonicalCache, err := filepath.EvalSymlinks(cache)
	require.NoError(t, err)
	_, err = db.CreateAgentGroup("portable-group", "")
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name":           "portable",
		"filesystem":     []map[string]any{{"path": cache, "access": "write"}},
		"environment":    []map[string]any{{"name": "GOCACHE", "value": cache}},
		"network_access": "none",
	}).Code)
	require.Equal(t, http.StatusOK, profileReq(t, f, http.MethodPut,
		"/v1/sandbox-profile-default", map[string]any{"name": "portable"}).Code)
	require.Equal(t, http.StatusOK, profileReq(t, f, http.MethodPut,
		"/v1/groups/portable-group/sandbox-profile", map[string]any{"name": "portable"}).Code)

	rec := profileReq(t, f, http.MethodGet, "/v1/sandbox-profiles/export?name=portable&include_assignments=true", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "export body=%s", rec.Body.String())
	var bundle map[string]any
	testharness.DecodeJSON(t, rec, &bundle)
	assert.Equal(t, "tclaude-sandbox-profiles", bundle["format"])
	// v5 removes read_baseline/read_baseline_exclusions (TCL-623). Exporting
	// only the newest version keeps an older importer from silently dropping a
	// security-significant field as an unknown key; v1–v4 stay importable.
	assert.Equal(t, float64(5), bundle["format_version"])

	require.Equal(t, http.StatusNoContent,
		profileReq(t, f, http.MethodDelete, "/v1/sandbox-profiles/portable", nil).Code)
	bundle["apply_assignments"] = true
	rec = profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles/import", bundle)
	require.Equalf(t, http.StatusOK, rec.Code, "import body=%s", rec.Body.String())
	rec = profileReq(t, f, http.MethodGet, "/v1/sandbox-profiles/portable", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var got wireSandboxProfile
	testharness.DecodeJSON(t, rec, &got)
	require.Len(t, got.Filesystem, 1)
	assert.Equal(t, canonicalCache, got.Filesystem[0].Path)
	assert.Equal(t, "none", got.NetworkAccess)
	for _, path := range []string{"/v1/sandbox-profile-default", "/v1/groups/portable-group/sandbox-profile"} {
		rec = profileReq(t, f, http.MethodGet, path, nil)
		var ref struct {
			Name string `json:"name"`
		}
		testharness.DecodeJSON(t, rec, &ref)
		assert.Equal(t, "portable", ref.Name)
	}
}

func TestSandboxProfilesImportConflictRollsBackWholeBundle(t *testing.T) {
	f := newFlow(t)
	require.Equal(t, http.StatusCreated,
		profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{"name": "already-there"}).Code)
	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles/import", map[string]any{
		"format": "tclaude-sandbox-profiles", "format_version": 1,
		"profiles": []map[string]any{{"name": "would-be-partial"}, {"name": "already-there"}},
	})
	require.Equalf(t, http.StatusConflict, rec.Code, "import body=%s", rec.Body.String())
	rec = profileReq(t, f, http.MethodGet, "/v1/sandbox-profiles/would-be-partial", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, "conflict planning must happen before the first insert")
}

func TestSandboxProfilesImportPreviewWarnsAndImportRetainsMissingPaths(t *testing.T) {
	f := newFlow(t)
	canonicalHome, err := filepath.EvalSymlinks(os.Getenv("HOME"))
	require.NoError(t, err)
	missing := filepath.Join(canonicalHome, "portable-recipient-missing", "cache")
	require.NoError(t, os.RemoveAll(filepath.Dir(missing)))
	bundle := map[string]any{
		"format": "tclaude-sandbox-profiles", "format_version": 1,
		"profiles": []map[string]any{{
			"name":       "portable-missing",
			"filesystem": []map[string]any{{"path": missing, "access": "write"}},
		}},
	}

	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles/import/inspect", bundle)
	require.Equalf(t, http.StatusOK, rec.Code, "inspect body=%s", rec.Body.String())
	var preview struct {
		Profiles []wireSandboxProfile `json:"profiles"`
		Warnings []struct {
			Profile string `json:"profile"`
			Path    string `json:"path"`
			Message string `json:"message"`
		} `json:"warnings"`
	}
	testharness.DecodeJSON(t, rec, &preview)
	require.Len(t, preview.Profiles, 1)
	require.Len(t, preview.Warnings, 1)
	assert.Equal(t, "portable-missing", preview.Warnings[0].Profile)
	assert.Equal(t, missing, preview.Warnings[0].Path)
	assert.Contains(t, preview.Warnings[0].Message, "does not exist locally")

	rec = profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles/import", bundle)
	require.Equalf(t, http.StatusOK, rec.Code, "import body=%s", rec.Body.String())
	var result struct {
		Imported []string `json:"imported"`
		Warnings []string `json:"warnings"`
	}
	testharness.DecodeJSON(t, rec, &result)
	assert.Equal(t, []string{"portable-missing"}, result.Imported)
	require.Len(t, result.Warnings, 1)
	assert.Contains(t, result.Warnings[0], missing)

	rec = profileReq(t, f, http.MethodGet, "/v1/sandbox-profiles/portable-missing", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var stored wireSandboxProfile
	testharness.DecodeJSON(t, rec, &stored)
	require.Len(t, stored.Filesystem, 1)
	assert.Equal(t, missing, stored.Filesystem[0].Path)
}
