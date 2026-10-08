package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/configbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestConfigBundleRoundTripAndConflict(t *testing.T) {
	f := newFlow(t)
	role := &db.Role{Name: "portable-role", Brief: "Review the change."}
	_, err := db.CreateRole(role)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, profileReq(t, f, http.MethodPost, "/v1/spawn-profiles", map[string]any{"name": "portable-profile", "role_ref": role.Name}).Code)
	rec := profileReq(t, f, http.MethodGet, "/v1/config-bundle/export?only=roles/portable-role&only=profiles/portable-profile", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var b configbundle.Bundle
	testharness.DecodeJSON(t, rec, &b)
	require.Equal(t, configbundle.Format, b.Format)
	require.NotEmpty(t, b.CreatedAt)
	require.NotEmpty(t, b.TclaudeVersion)
	require.Len(t, b.Sections["profiles"], 1)
	_, err = db.DeleteSpawnProfile("portable-profile")
	require.NoError(t, err)
	_, err = db.DeleteRole(role.Name)
	require.NoError(t, err)
	rec = profileReq(t, f, http.MethodPost, "/v1/config-bundle/import", map[string]any{"bundle": b})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"security_changes":2`)
	missing, err := db.GetRole(role.Name)
	require.NoError(t, err)
	assert.Nil(t, missing, "preview must not write")
	rec = profileReq(t, f, http.MethodPost, "/v1/config-bundle/import", map[string]any{"bundle": b, "apply": true})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	got, err := db.GetSpawnProfile("portable-profile")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, role.Name, got.RoleRef)
	// Re-importing the same values is harmless without --replace.
	rec = profileReq(t, f, http.MethodPost, "/v1/config-bundle/import", map[string]any{"bundle": b, "apply": true})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	got.Model = "haiku"
	require.NoError(t, db.UpdateSpawnProfile(got))
	rec = profileReq(t, f, http.MethodPost, "/v1/config-bundle/import", map[string]any{"bundle": b, "apply": true})
	require.Equal(t, 409, rec.Code, rec.Body.String())
	got, err = db.GetSpawnProfile("portable-profile")
	require.NoError(t, err)
	assert.Equal(t, "haiku", got.Model)
	rec = profileReq(t, f, http.MethodPost, "/v1/config-bundle/import", map[string]any{"bundle": b, "apply": true, "replace": true})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	got, err = db.GetSpawnProfile("portable-profile")
	require.NoError(t, err)
	assert.Empty(t, got.Model)
}

func TestConfigBundleFlagsAndSelectiveImport(t *testing.T) {
	f := newFlow(t)
	_, err := db.CreateRole(&db.Role{Name: "credential-role", Brief: "Use api_key=supersecret123456789 to connect"})
	require.NoError(t, err)
	rec := profileReq(t, f, http.MethodGet, "/v1/config-bundle/export?only=roles/credential-role", nil)
	require.Equal(t, 422, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "supersecret")
	assert.Contains(t, rec.Body.String(), "credential-role")
	assert.Contains(t, rec.Body.String(), "brief")
	rec = profileReq(t, f, http.MethodGet, "/v1/config-bundle/export?only=roles/credential-role&allow_flagged=true", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "supersecret")
	var b configbundle.Bundle
	testharness.DecodeJSON(t, rec, &b)
	// Skip does not modify even an explicitly replaceable role.
	rec = profileReq(t, f, http.MethodPost, "/v1/config-bundle/import", map[string]any{"bundle": b, "skip": []string{"roles"}, "apply": true, "replace": true})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"applied":[]`)
	b.FormatVersion = 99
	rec = profileReq(t, f, http.MethodPost, "/v1/config-bundle/import", map[string]any{"bundle": b})
	require.Equal(t, 400, rec.Code)
	assert.Contains(t, rec.Body.String(), "format_version 99")
}

func TestConfigBundlePlaceholdersAndSandboxIncludes(t *testing.T) {
	f := newFlow(t)
	for _, v := range []map[string]any{
		{"name": "z-base", "filesystem": []map[string]string{{"path": "/opt/bundle-example", "access": "read"}}},
		{"name": "a-child", "includes": []string{"z-base"}},
	} {
		rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", v)
		require.Equal(t, 201, rec.Code, rec.Body.String())
	}
	rec := profileReq(t, f, http.MethodGet, "/v1/config-bundle/export?only=sandbox-profiles/z-base&only=sandbox-profiles/a-child", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "/opt/bundle-example")
	var b configbundle.Bundle
	testharness.DecodeJSON(t, rec, &b)
	require.NotEmpty(t, b.Placeholders)
	rec = profileReq(t, f, http.MethodPost, "/v1/config-bundle/import", map[string]any{"bundle": b})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "unresolved")
	rec = profileReq(t, f, http.MethodPost, "/v1/config-bundle/import", map[string]any{"bundle": b, "apply": true, "replace": true})
	require.Equal(t, 409, rec.Code, rec.Body.String())
	// Binding structured paths preserves valid JSON, including quote characters.
	values := map[string]string{}
	for _, p := range b.Placeholders {
		values[p.Name] = "/opt/rebound"
	}
	rec = profileReq(t, f, http.MethodPost, "/v1/config-bundle/import", map[string]any{"bundle": b, "values": values, "apply": true, "replace": true})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	got, err := db.GetSandboxProfile("z-base")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "/opt/rebound", got.Filesystem[0].Path)
	// External paths never appear in the exported bytes, even in nested arrays.
	raw, err := json.Marshal(b)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "/opt/bundle-example")
}
