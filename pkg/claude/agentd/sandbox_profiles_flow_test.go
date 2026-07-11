package agentd_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type wireSandboxProfile struct {
	Name       string `json:"name"`
	Filesystem []struct {
		Path   string `json:"path"`
		Access string `json:"access"`
	} `json:"filesystem"`
	Environment []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"environment"`
}

func TestSandboxProfilesCRUDValidationAndAssignments(t *testing.T) {
	f := newFlow(t)
	home := os.Getenv("HOME")
	cache := filepath.Join(home, "shared-cache")
	require.NoError(t, os.MkdirAll(cache, 0o755))
	_, err := db.CreateAgentGroup("crew", "")
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
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "create body=%s", rec.Body.String())

	rec = profileReq(t, f, http.MethodGet, "/v1/sandbox-profiles/dev-caches", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var got wireSandboxProfile
	testharness.DecodeJSON(t, rec, &got)
	require.Len(t, got.Filesystem, 1)
	assert.Equal(t, cache, got.Filesystem[0].Path)
	assert.Equal(t, "write", got.Filesystem[0].Access)
	require.Len(t, got.Environment, 1)
	assert.Equal(t, "GOCACHE", got.Environment[0].Name)

	for _, body := range []map[string]any{
		{"name": "protected", "filesystem": []map[string]any{{"path": filepath.Join(home, ".tclaude", "data"), "access": "write"}}},
		{"name": "reserved", "environment": []map[string]any{{"name": "TCLAUDE_SESSION_ID", "value": "spoof"}}},
		{"name": "conflict", "environment": []map[string]any{{"name": "A", "value": "1"}, {"name": "A", "value": "2"}}},
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
	_, err := db.CreateAgentGroup("portable-group", "")
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name":        "portable",
		"filesystem":  []map[string]any{{"path": cache, "access": "write"}},
		"environment": []map[string]any{{"name": "GOCACHE", "value": cache}},
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
	assert.Equal(t, cache, got.Filesystem[0].Path)
	for _, path := range []string{"/v1/sandbox-profile-default", "/v1/groups/portable-group/sandbox-profile"} {
		rec = profileReq(t, f, http.MethodGet, path, nil)
		var ref struct {
			Name string `json:"name"`
		}
		testharness.DecodeJSON(t, rec, &ref)
		assert.Equal(t, "portable", ref.Name)
	}
}
