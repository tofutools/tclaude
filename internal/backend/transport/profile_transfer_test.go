package transport

import (
	"encoding/json"
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
	for _, body := range []string{strings.Replace(bundle, `"version":1`, `"version":5`, 1), strings.Replace(bundle, `"archived":true`, `"archived":true,"environment":{"SECRET":"value"}`, 1), strings.Replace(bundle, "Worker", string([]byte{0xff}), 1), strings.Repeat(" ", maxRequestBytes+1) + bundle} {
		require.Equal(t, 400, request(h, "POST", "/v2/configuration-transfer/inspect", body, testCredential).Code)
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
