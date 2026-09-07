package transport

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestPublicConfigurationTransferRejectsUnsupportedInputAndCommitsBatch(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	h := testHandler(t, app.New(store, providers.NewRegistry()))
	bundle := `{"format":"tclaude-configuration-profiles","version":1,"profiles":[{"key":"source","name":"Worker","archived":true,"desired":{"Harness":"claude","Model":"fixture","WorkingDirectory":"/tmp","Approval":"supervised","Sandbox":"workspace_write"}}]}`
	require.Equal(t, 401, request(h, "POST", "/v2/configuration-transfer/inspect", bundle, "").Code)
	require.Equal(t, 200, request(h, "POST", "/v2/configuration-transfer/inspect", bundle, testCredential).Code)
	for _, test := range []struct {
		body   string
		status int
	}{
		{strings.Replace(bundle, `"version":1`, `"version":5`, 1), 422},
		{strings.Replace(bundle, `"archived":true`, `"archived":true,"environment":{"SECRET":"value"}`, 1), 400},
		{strings.Replace(bundle, "Worker", string([]byte{0xff}), 1), 400},
		{strings.Repeat(" ", maxRequestBytes+1) + bundle, 400},
	} {
		require.Equal(t, test.status, request(h, "POST", "/v2/configuration-transfer/inspect", test.body, testCredential).Code)
	}

	body := `{"request_id":"import","bundle":` + bundle + `,"selections":[{"key":"source","id":"copy","revision_id":"one","name":"Copied"}]}`
	response := request(h, "POST", "/v2/configuration-transfer/import", body, testCredential)
	require.Equal(t, 200, response.Code, response.Body.String())
	response = request(h, "POST", "/v2/configuration-transfer/import", body, testCredential)
	require.Equal(t, 200, response.Code, response.Body.String())
	var imported app.ConfigurationImportResult
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &imported))
	require.True(t, imported.Repeated)
	response = request(h, "GET", "/v2/configuration-profiles", "", testCredential)
	var profiles []model.ConfigurationProfile
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &profiles))
	require.Len(t, profiles, 1)
	require.True(t, profiles[0].Archived)
}

// Public boundary regression: a bundle admitted by inspect at the documented size boundary
// must remain admissible when the import endpoint adds its selection envelope.
func TestConfigurationTransferAcceptsInspectedBundleAtLimit(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	h := testHandler(t, app.New(store, providers.NewRegistry()))

	bundle := app.ConfigurationBundle{Format: app.ConfigurationBundleFormat, Version: 1}
	for i := 0; i < 32; i++ {
		bundle.Profiles = append(bundle.Profiles, app.ConfigurationBundleEntry{
			Key:  fmt.Sprintf("entry_%d", i),
			Name: fmt.Sprintf("Entry %d", i),
			Desired: model.DesiredConfiguration{
				Harness: "claude", Model: "fixture", WorkingDirectory: "/tmp",
				Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite,
			},
			Startup: &model.ProfileStartup{},
		})
	}
	// Grow every valid startup context equally until the compact bundle is the
	// closest value below the public 1 MiB request limit.
	low, high := 0, 32768
	for low < high {
		mid := (low + high + 1) / 2
		for i := range bundle.Profiles {
			bundle.Profiles[i].Startup.Context = strings.Repeat("x", mid)
		}
		encoded, marshalErr := json.Marshal(bundle)
		require.NoError(t, marshalErr)
		if len(encoded) <= maxRequestBytes {
			low = mid
		} else {
			high = mid - 1
		}
	}
	for i := range bundle.Profiles {
		bundle.Profiles[i].Startup.Context = strings.Repeat("x", low)
	}
	encoded, err := json.Marshal(bundle)
	require.NoError(t, err)
	require.LessOrEqual(t, len(encoded), maxRequestBytes)
	require.Equal(t, 200, request(h, "POST", "/v2/configuration-transfer/inspect", string(encoded), testCredential).Code)

	selections := make([]app.ConfigurationImportSelection, 0, len(bundle.Profiles))
	for i, entry := range bundle.Profiles {
		selections = append(selections, app.ConfigurationImportSelection{
			Key: entry.Key, ID: model.ConfigurationProfileID(fmt.Sprintf("copy_%d", i)),
			RevisionID: model.ConfigurationProfileRevisionID(fmt.Sprintf("revision_%d", i)),
			Name:       entry.Name,
		})
	}
	body, err := json.Marshal(struct {
		RequestID  string                             `json:"request_id"`
		Bundle     app.ConfigurationBundle            `json:"bundle"`
		Selections []app.ConfigurationImportSelection `json:"selections"`
	}{"boundary_import", bundle, selections})
	require.NoError(t, err)
	require.Greater(t, len(body), maxRequestBytes)
	require.Equal(t, 200, request(h, "POST", "/v2/configuration-transfer/import", string(body), testCredential).Code)
	// Extra transport room does not enlarge the logical bundle or admit unbounded envelopes.
	for i := range bundle.Profiles {
		bundle.Profiles[i].Startup.Context = strings.Repeat("x", 32768)
	}
	tooLarge, err := json.Marshal(map[string]any{"request_id": "oversized", "bundle": bundle, "selections": selections})
	require.NoError(t, err)
	require.Equal(t, 422, request(h, "POST", "/v2/configuration-transfer/import", string(tooLarge), testCredential).Code)
	require.Equal(t, 400, request(h, "POST", "/v2/configuration-transfer/import", strings.Repeat(" ", 2*app.ConfigurationBundleMaxBytes)+string(body), testCredential).Code)

}
