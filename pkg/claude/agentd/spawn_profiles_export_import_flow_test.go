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
		Name        string   `json:"name"`
		Aliases     []string `json:"aliases"`
		Exists      bool     `json:"exists"`
		Valid       bool     `json:"valid"`
		DefaultName string   `json:"default_name"`
		Error       string   `json:"error"`
	} `json:"profiles"`
}

type profileImportResultWire struct {
	Imported []struct {
		Source  string `json:"source"`
		Name    string `json:"name"`
		Updated bool   `json:"updated"`
	} `json:"imported"`
	Skipped  []string `json:"skipped"`
	Warnings []string `json:"warnings"`
}

func TestSpawnProfilesExportImport_RoundTripSelected(t *testing.T) {
	f := newFlow(t)

	require.Equal(t, http.StatusCreated, profileReq(t, f, http.MethodPost, "/v1/spawn-profiles",
		map[string]any{
			"name": "alpha", "aliases": []string{"codex-reviewer"},
			"model": "sonnet", "role": "lead", "sync_worktree": true,
		}).Code)
	require.Equal(t, http.StatusCreated, profileReq(t, f, http.MethodPost, "/v1/spawn-profiles",
		map[string]any{"name": "beta", "model": "haiku"}).Code)

	rec := profileReq(t, f, http.MethodGet, "/v1/spawn-profiles/export?name=alpha&name=codex-reviewer", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "export body=%s", rec.Body.String())
	var bundle profileBundle
	testharness.DecodeJSON(t, rec, &bundle)
	assert.Equal(t, "tclaude-spawn-profiles", bundle.Format)
	assert.Equal(t, 2, bundle.FormatVersion)
	assert.NotEmpty(t, bundle.ExportedAt)
	require.Len(t, bundle.Profiles, 1)
	assert.Equal(t, "alpha", bundle.Profiles[0].Name)
	assert.Equal(t, []string{"codex-reviewer"}, bundle.Profiles[0].Aliases)
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
	assert.Equal(t, []string{"codex-reviewer"}, got.Aliases)
	require.NotNil(t, got.SyncWorktree)
	assert.True(t, *got.SyncWorktree)

	rec = profileReq(t, f, http.MethodGet, "/v1/spawn-profiles/codex-reviewer", nil)
	require.Equal(t, http.StatusOK, rec.Code, "the imported alias resolves")
}

func TestSpawnProfilesImport_RejectsExistingAliasCollision(t *testing.T) {
	f := newFlow(t)
	require.Equal(t, http.StatusCreated, profileReq(t, f, http.MethodPost, "/v1/spawn-profiles",
		map[string]any{"name": "review-profile", "aliases": []string{"codex-reviewer"}}).Code)

	bundle := map[string]any{
		"format":         "tclaude-spawn-profiles",
		"format_version": 2,
		"profiles": []map[string]any{
			{"name": "codex-reviewer", "model": "opus"},
		},
	}
	rec := profileReq(t, f, http.MethodPost, "/v1/spawn-profiles/import/inspect", bundle)
	require.Equalf(t, http.StatusOK, rec.Code, "inspect body=%s", rec.Body.String())
	var insp profileInspectResult
	testharness.DecodeJSON(t, rec, &insp)
	require.Len(t, insp.Profiles, 1)
	assert.False(t, insp.Profiles[0].Exists, "the alias is not presented as an overwriteable primary name")
	assert.False(t, insp.Profiles[0].Valid)
	assert.Contains(t, insp.Profiles[0].Error, `already owned by "review-profile"`)

	rec = profileReq(t, f, http.MethodPost, "/v1/spawn-profiles/import", map[string]any{
		"format":         "tclaude-spawn-profiles",
		"format_version": 2,
		"profiles":       []map[string]any{{"name": "codex-reviewer", "model": "opus"}},
		"decisions":      []map[string]any{{"name": "codex-reviewer", "include": true, "action": "create"}},
	})
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestSpawnProfilesImport_InspectRenameAndOverwriteConflicts(t *testing.T) {
	f := newFlow(t)

	require.Equal(t, http.StatusCreated, profileReq(t, f, http.MethodPost, "/v1/spawn-profiles",
		map[string]any{"name": "dup", "aliases": []string{"reviewer"}, "model": "sonnet"}).Code)
	bundle := map[string]any{
		"format":         "tclaude-spawn-profiles",
		"format_version": 2,
		"profiles": []map[string]any{
			{"name": "dup", "aliases": []string{"reviewer"}, "model": "opus"},
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
	assert.Equal(t, []string{"reviewer"}, insp.Profiles[0].Aliases)
	assert.Equal(t, "dup-copy", insp.Profiles[0].DefaultName)

	rec = profileReq(t, f, http.MethodPost, "/v1/spawn-profiles/import", map[string]any{
		"format":         "tclaude-spawn-profiles",
		"format_version": 2,
		"profiles":       []map[string]any{{"name": "dup", "aliases": []string{"reviewer"}, "model": "opus"}},
		"decisions":      []map[string]any{{"name": "dup", "include": true, "action": "rename", "as": "dup-imported"}},
	})
	require.Equalf(t, http.StatusOK, rec.Code, "rename import body=%s", rec.Body.String())
	var renamed profileImportResultWire
	testharness.DecodeJSON(t, rec, &renamed)
	require.Len(t, renamed.Warnings, 1)
	assert.Contains(t, renamed.Warnings[0], "aliases were omitted")
	rec = profileReq(t, f, http.MethodGet, "/v1/spawn-profiles/dup-imported", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var got wireProfile
	testharness.DecodeJSON(t, rec, &got)
	assert.Equal(t, "opus", got.Model)
	assert.Empty(t, got.Aliases, "renamed copies cannot duplicate aliases owned by the source")

	rec = profileReq(t, f, http.MethodPost, "/v1/spawn-profiles/import", map[string]any{
		"format":         "tclaude-spawn-profiles",
		"format_version": 2,
		"profiles":       []map[string]any{{"name": "dup", "aliases": []string{"reviewer"}, "model": "opus"}},
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
	assert.Equal(t, []string{"reviewer"}, got.Aliases, "overwrite preserves imported aliases on their original owner")
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
