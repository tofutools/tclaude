package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type profileBundle struct {
	Format        string        `json:"format"`
	FormatVersion int           `json:"format_version"`
	ExportedAt    string        `json:"exported_at"`
	Profiles      []wireProfile `json:"profiles"`
}

type profileInspectResult struct {
	Profiles []struct {
		Name        string `json:"name"`
		Exists      bool   `json:"exists"`
		Valid       bool   `json:"valid"`
		DefaultName string `json:"default_name"`
	} `json:"profiles"`
}

type profileImportResultWire struct {
	Imported []struct {
		Source  string `json:"source"`
		Name    string `json:"name"`
		Updated bool   `json:"updated"`
	} `json:"imported"`
	Skipped []string `json:"skipped"`
}

func TestSpawnProfilesExportImport_RoundTripSelected(t *testing.T) {
	f := newFlow(t)

	require.Equal(t, http.StatusCreated, profileReq(t, f, http.MethodPost, "/v1/spawn-profiles",
		map[string]any{"name": "alpha", "model": "sonnet", "role": "lead", "sync_worktree": true}).Code)
	require.Equal(t, http.StatusCreated, profileReq(t, f, http.MethodPost, "/v1/spawn-profiles",
		map[string]any{"name": "beta", "model": "haiku"}).Code)

	rec := profileReq(t, f, http.MethodGet, "/v1/spawn-profiles/export?name=alpha", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "export body=%s", rec.Body.String())
	var bundle profileBundle
	testharness.DecodeJSON(t, rec, &bundle)
	assert.Equal(t, "tclaude-spawn-profiles", bundle.Format)
	assert.Equal(t, 1, bundle.FormatVersion)
	assert.NotEmpty(t, bundle.ExportedAt)
	require.Len(t, bundle.Profiles, 1)
	assert.Equal(t, "alpha", bundle.Profiles[0].Name)
	assert.Equal(t, "sonnet", bundle.Profiles[0].Model)
	require.NotNil(t, bundle.Profiles[0].SyncWorktree)
	assert.True(t, *bundle.Profiles[0].SyncWorktree)

	require.Equal(t, http.StatusNoContent, profileReq(t, f, http.MethodDelete, "/v1/spawn-profiles/alpha", nil).Code)

	rec = profileReq(t, f, http.MethodPost, "/v1/spawn-profiles/import", map[string]any{
		"format":         bundle.Format,
		"format_version": bundle.FormatVersion,
		"profiles":       bundle.Profiles,
		"decisions": []map[string]any{
			{"name": "alpha", "include": true, "action": "create"},
		},
	})
	require.Equalf(t, http.StatusOK, rec.Code, "import body=%s", rec.Body.String())
	var imported profileImportResultWire
	testharness.DecodeJSON(t, rec, &imported)
	require.Len(t, imported.Imported, 1)
	assert.Equal(t, "alpha", imported.Imported[0].Name)
	assert.False(t, imported.Imported[0].Updated)

	rec = profileReq(t, f, http.MethodGet, "/v1/spawn-profiles/alpha", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var got wireProfile
	testharness.DecodeJSON(t, rec, &got)
	assert.Equal(t, "sonnet", got.Model)
	require.NotNil(t, got.SyncWorktree)
	assert.True(t, *got.SyncWorktree)
}

func TestSpawnProfilesImport_InspectRenameAndOverwriteConflicts(t *testing.T) {
	f := newFlow(t)

	require.Equal(t, http.StatusCreated, profileReq(t, f, http.MethodPost, "/v1/spawn-profiles",
		map[string]any{"name": "dup", "model": "sonnet"}).Code)
	bundle := map[string]any{
		"format":         "tclaude-spawn-profiles",
		"format_version": 1,
		"profiles": []map[string]any{
			{"name": "dup", "model": "opus"},
		},
	}

	rec := profileReq(t, f, http.MethodPost, "/v1/spawn-profiles/import/inspect", bundle)
	require.Equalf(t, http.StatusOK, rec.Code, "inspect body=%s", rec.Body.String())
	var insp profileInspectResult
	testharness.DecodeJSON(t, rec, &insp)
	require.Len(t, insp.Profiles, 1)
	assert.Equal(t, "dup", insp.Profiles[0].Name)
	assert.True(t, insp.Profiles[0].Exists)
	assert.True(t, insp.Profiles[0].Valid)
	assert.Equal(t, "dup-copy", insp.Profiles[0].DefaultName)

	rec = profileReq(t, f, http.MethodPost, "/v1/spawn-profiles/import", map[string]any{
		"format":         "tclaude-spawn-profiles",
		"format_version": 1,
		"profiles":       []map[string]any{{"name": "dup", "model": "opus"}},
		"decisions":      []map[string]any{{"name": "dup", "include": true, "action": "rename", "as": "dup-imported"}},
	})
	require.Equalf(t, http.StatusOK, rec.Code, "rename import body=%s", rec.Body.String())
	rec = profileReq(t, f, http.MethodGet, "/v1/spawn-profiles/dup-imported", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var got wireProfile
	testharness.DecodeJSON(t, rec, &got)
	assert.Equal(t, "opus", got.Model)

	rec = profileReq(t, f, http.MethodPost, "/v1/spawn-profiles/import", map[string]any{
		"format":         "tclaude-spawn-profiles",
		"format_version": 1,
		"profiles":       []map[string]any{{"name": "dup", "model": "opus"}},
		"decisions":      []map[string]any{{"name": "dup", "include": true, "action": "overwrite"}},
	})
	require.Equalf(t, http.StatusOK, rec.Code, "overwrite import body=%s", rec.Body.String())
	var res profileImportResultWire
	testharness.DecodeJSON(t, rec, &res)
	require.Len(t, res.Imported, 1)
	assert.True(t, res.Imported[0].Updated)

	rec = profileReq(t, f, http.MethodGet, "/v1/spawn-profiles/dup", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	got = wireProfile{}
	testharness.DecodeJSON(t, rec, &got)
	assert.Equal(t, "opus", got.Model, "overwrite replaces the existing profile")
}

func TestSpawnProfilesImport_PermissionGating(t *testing.T) {
	f := newFlow(t)

	require.Equal(t, http.StatusCreated, profileReq(t, f, http.MethodPost, "/v1/spawn-profiles",
		map[string]any{"name": "gated", "model": "haiku"}).Code)
	const peer = "profile-gate-aaaa-bbbb"
	f.HaveConvWithTitle(peer, "peer")

	rec := testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/spawn-profiles/export?name=gated", nil), peer))
	assert.Equalf(t, http.StatusOK, rec.Code, "export is read-only/open: %s", rec.Body.String())

	env := map[string]any{"format": "tclaude-spawn-profiles", "format_version": 1, "profiles": []map[string]any{{"name": "newbie"}}}
	rec = testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/spawn-profiles/import/inspect", env), peer))
	assert.Equalf(t, http.StatusForbidden, rec.Code, "inspect requires profiles.manage: %s", rec.Body.String())

	rec = testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/spawn-profiles/import", env), peer))
	assert.Equalf(t, http.StatusForbidden, rec.Code, "import requires profiles.manage: %s", rec.Body.String())
}
