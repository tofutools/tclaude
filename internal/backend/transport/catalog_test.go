package transport

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestPublicConfigurationProfileSelection(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	h := testHandler(t, app.New(store, providers.NewRegistry()))
	body := `{"request_id":"profile_save","id":"worker","revision_id":"one","name":"Worker","desired":{"Harness":"claude","Model":"example","WorkingDirectory":"/tmp/work","Approval":"supervised","Sandbox":"unconfined"}}`
	require.Equal(t, 401, request(h, "POST", "/v2/configuration-profiles", body, "").Code)
	result := request(h, "POST", "/v2/configuration-profiles", body, testCredential)
	require.Equal(t, 200, result.Code, result.Body.String())
	var profile app.ConfigurationProfileResult
	require.NoError(t, json.Unmarshal(result.Body.Bytes(), &profile))
	payload, err := json.Marshal(map[string]any{"id": "agent", "name": "Worker", "configuration_profile": profile.Revision.Ref})
	require.NoError(t, err)
	created := request(h, "POST", "/v2/agents", string(payload), testCredential)
	require.Equal(t, 201, created.Code, created.Body.String())
	var agent model.Agent
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &agent))
	require.Equal(t, &profile.Revision.Ref, agent.ConfigurationProfile)
	require.Equal(t, "example", agent.Desired.Model)
	listed := request(h, "GET", "/v2/configuration-profiles", "", testCredential)
	require.Equal(t, 200, listed.Code)
	var entries []model.ConfigurationProfile
	require.NoError(t, json.Unmarshal(listed.Body.Bytes(), &entries))
	require.Len(t, entries, 1)
	wrong := request(h, "GET", "/v2/configuration-profiles/worker?revision_id=one&content_hash=wrong", "", testCredential)
	require.Equal(t, 409, wrong.Code)
}
