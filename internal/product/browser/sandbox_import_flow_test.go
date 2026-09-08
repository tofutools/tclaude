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

func TestBrowserImportedSandboxIsAvailableEditableAndDoesNotLaunch(t *testing.T) {
	ctx, page, operator := processEditorBrowserWithSetup(t, func(state string) {
		root := t.TempDir()
		source := filepath.Join(root, "snapshot.sqlite")
		db, err := sql.Open("sqlite", source)
		require.NoError(t, err)
		schema, err := os.ReadFile(filepath.Join("..", "..", "backend", "migration", "source", "v228", "testdata", "schema.sql"))
		require.NoError(t, err)
		_, err = db.Exec(string(schema))
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO schema_version(version) VALUES(228); INSERT INTO sandbox_profiles(name,filesystem_json,environment_json,created_at,updated_at,network_access,pre_launch_json) VALUES('Retained policy','[]','[{"name":"RETAINED","value":"literal $(never-run)"}]',1700000000,1700000000,'none','[{"name":"setup","script":"exit 91"}]'); INSERT INTO sandbox_profile_global_assignment(id,profile_id,profile_name) SELECT 1,id,name FROM sandbox_profiles; INSERT INTO agents(agent_id,current_conv_id,created_at,pending_name,initial_spawn_config,effective_sandbox_config) VALUES('agt_imported','imported-conv',1700000000,'Imported worker','{"harness":"claude","model":"fixture","cwd":"/tmp"}','{"version":7,"profiles_omitted":true,"applied":[]}'); INSERT INTO agent_conversations(conv_id,agent_id,linked_at) VALUES('imported-conv','agt_imported',1700000000);`)
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
	var defaults model.SandboxDefaults
	require.NoError(t, operator.Call(ctx, "GET", "/v2/sandbox-defaults", nil, &defaults))
	require.NotEmpty(t, defaults.Global)
	page.MustElementR("#sandbox-profiles button", "^Global sandbox profile$").MustClick()
	page.MustElement("#editor").MustWaitVisible()
	require.Equal(t, string(defaults.Global), page.MustElement("#editor [name=sandbox_default]").MustProperty("value").String())
	page.MustElementR("#editor button", "^Cancel$").MustClick()
	card := page.MustElementR("#sandbox-profiles article", "Retained policy")
	require.True(t, card.MustHasR("button", "^Edit sandbox profile$"))

	card.MustElementR("button", "^Inspect sandbox profile$").MustClick()
	page.MustElementR(".sandbox-editor summary", "^Environment and generated directories$").MustClick()
	require.Equal(t, "literal $(never-run)", page.MustElement(".sandbox-editor [aria-label='Literal environment value 1']").MustProperty("value").String())
	page.MustElementR(".sandbox-editor button", "^Close$").MustClick()
	page.MustWait(`()=>!document.querySelector('.sandbox-editor')`)
	card.MustElementR("button", "^Edit sandbox profile$").MustClick()
	page.MustElement(".sandbox-editor [aria-label='Sandbox profile name']").MustSelectAllText().MustInput("Renamed policy")
	page.MustElementR(".sandbox-editor button", "^Save sandbox profile$").MustClick()
	page.MustWait(`()=>!document.querySelector('.sandbox-editor')`)
	var edited app.SandboxProfileResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/sandbox-profiles/"+string(defaults.Global), nil, &edited))
	require.Equal(t, "Renamed policy", edited.Profile.Name)
	card = page.MustElementR("#sandbox-profiles article", "Renamed policy")
	card.MustElementR("button", "^Copy sandbox profile$").MustClick()
	page.MustElement(".sandbox-editor [aria-label='Sandbox profile name']").MustSelectAllText().MustInput("Reviewed copy")
	page.MustElementR(".sandbox-editor button", "^Save sandbox profile$").MustClick()
	page.MustWait(`()=>!document.querySelector('.sandbox-editor')`)
	var profiles []model.SandboxProfile
	require.NoError(t, operator.Call(ctx, "GET", "/v2/sandbox-profiles?include_archived=true", nil, &profiles))
	require.Len(t, profiles, 2)
	for _, profile := range profiles {
		if profile.Name == "Renamed policy" {
			require.False(t, profile.Archived)
			require.True(t, profile.Imported)
		} else {
			require.Equal(t, "Reviewed copy", profile.Name)
			require.False(t, profile.Archived)
			require.False(t, profile.Imported)
		}
		var read app.SandboxProfileResult
		require.NoError(t, operator.Call(ctx, "GET", "/v2/sandbox-profiles/"+string(profile.ID), nil, &read))
		require.Equal(t, "literal $(never-run)", read.Revision.Policy.Environment["RETAINED"])
		require.Equal(t, "closed", read.Revision.Policy.UnixSockets.Mode)
	}
	var snapshot struct {
		Agents     []model.Agent `json:"agents"`
		Executions []any         `json:"executions"`
		WorkRuns   []any         `json:"work_runs"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Empty(t, snapshot.Executions)
	require.Empty(t, snapshot.WorkRuns)
	require.Len(t, snapshot.Agents, 1)
	require.True(t, snapshot.Agents[0].Desired.HostSandbox.OmitProfiles)
	page.MustElement("[data-tab=groups]").MustClick()
	page.MustElementR("#roster button", "^Configure$").MustClick()
	page.MustElement("#editor").MustWaitVisible()
	require.Equal(t, "Keep profiles omitted", page.MustElement("#editor [name=host_sandbox] option:checked").MustText())
	page.MustElementR("#editor button", "^Cancel$").MustClick()
}
