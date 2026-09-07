package browser

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/migration"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserImportedSandboxCanBeInspectedAndCopiedWithoutActivation(t *testing.T) {
	ctx, page, operator := processEditorBrowserWithSetup(t, func(state string) {
		root := t.TempDir()
		source := filepath.Join(root, "snapshot.sqlite")
		db, err := sql.Open("sqlite", source)
		require.NoError(t, err)
		schema, err := os.ReadFile(filepath.Join("..", "..", "backend", "migration", "source", "v228", "testdata", "schema.sql"))
		require.NoError(t, err)
		_, err = db.Exec(string(schema))
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO schema_version(version) VALUES(228); INSERT INTO sandbox_profiles(name,filesystem_json,environment_json,created_at,updated_at,network_access,pre_launch_json) VALUES('Retained policy','[]','[{"name":"RETAINED","value":"literal $(never-run)"}]',1700000000,1700000000,'none','[{"name":"setup","script":"exit 91"}]');`)
		require.NoError(t, err)
		require.NoError(t, db.Close())
		data, err := os.ReadFile(source)
		require.NoError(t, err)
		hash := sha256.Sum256(data)
		manifest, err := json.Marshal(migration.Manifest{FormatVersion: 1, Database: migration.ManifestFile{Path: "snapshot.sqlite", Size: int64(len(data)), SHA256: hex.EncodeToString(hash[:])}})
		require.NoError(t, err)
		manifestPath := filepath.Join(root, "manifest.json")
		require.NoError(t, os.WriteFile(manifestPath, manifest, 0600))
		_, err = migration.ImportSnapshot(context.Background(), migration.Bundle{Root: root, ManifestPath: manifestPath}, migration.ImportOptions{DestinationPath: filepath.Join(state, "backend.sqlite")})
		require.NoError(t, err)
	})
	page.MustElement("main:not([inert])")
	page.MustElement("[data-tab=configurations]").MustClick()
	page.MustElementR("#configurations summary", "^Sandbox profiles$").MustClick()
	require.False(t, page.MustHasR("#sandbox-profiles article", "Retained policy"))
	page.MustElement("#sandbox-profiles [aria-label='Sandbox profile status']").MustSelect("archived")
	card := page.MustElementR("#sandbox-profiles article", "Retained policy")
	card.MustElementR("button", "^Inspect sandbox profile$").MustClick()
	page.MustElementR(".sandbox-editor summary", "^Environment and generated directories$").MustClick()
	require.Equal(t, "literal $(never-run)", page.MustElement(".sandbox-editor [aria-label='Literal environment value 1']").MustProperty("value").String())
	page.MustElementR(".sandbox-editor button", "^Close$").MustClick()
	page.MustWait(`()=>!document.querySelector('.sandbox-editor')`)
	card.MustElementR("button", "^Copy sandbox profile$").MustClick()
	page.MustElement(".sandbox-editor [aria-label='Sandbox profile name']").MustSelectAllText().MustInput("Reviewed copy")
	page.MustElementR(".sandbox-editor button", "^Save sandbox profile$").MustClick()
	page.MustWait(`()=>!document.querySelector('.sandbox-editor')`)
	var profiles []model.SandboxProfile
	require.NoError(t, operator.Call(ctx, "GET", "/v2/sandbox-profiles?include_archived=true", nil, &profiles))
	require.Len(t, profiles, 2)
	for _, profile := range profiles {
		if profile.Name == "Retained policy" {
			require.True(t, profile.Archived)
		} else {
			require.Equal(t, "Reviewed copy", profile.Name)
			require.False(t, profile.Archived)
		}
		var read app.SandboxProfileResult
		require.NoError(t, operator.Call(ctx, "GET", "/v2/sandbox-profiles/"+string(profile.ID), nil, &read))
		require.Equal(t, "literal $(never-run)", read.Revision.Policy.Environment["RETAINED"])
		require.Equal(t, "closed", read.Revision.Policy.UnixSockets.Mode)
	}
	var snapshot struct {
		Executions []any `json:"executions"`
		WorkRuns   []any `json:"work_runs"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Empty(t, snapshot.Executions)
	require.Empty(t, snapshot.WorkRuns)
}
