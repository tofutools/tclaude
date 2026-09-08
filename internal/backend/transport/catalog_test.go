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

func TestPublicConfigurationDefaultsPinExistingAgents(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	h := testHandler(t, app.New(store, providers.NewRegistry()))
	save := func(revision, modelName string, expected int) app.ConfigurationProfileResult {
		payload, marshalErr := json.Marshal(map[string]any{"request_id": "save_" + revision, "id": "worker", "revision_id": revision, "expected_revision": expected, "name": "Worker", "desired": model.DesiredConfiguration{Harness: "claude", Model: modelName, WorkingDirectory: "/tmp/work", Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}})
		require.NoError(t, marshalErr)
		response := request(h, "POST", "/v2/configuration-profiles", string(payload), testCredential)
		require.Equal(t, 200, response.Code, response.Body.String())
		var result app.ConfigurationProfileResult
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
		return result
	}
	first := save("one", "first", 0)
	set := func(id string, ref model.ConfigurationProfileRef, expected int) string {
		data, marshalErr := json.Marshal(map[string]any{"request_id": id, "expected_revision": expected, "global": ref, "harnesses": map[string]model.ConfigurationProfileRef{"claude": ref}})
		require.NoError(t, marshalErr)
		return string(data)
	}
	original := set("defaults_one", first.Revision.Ref, 0)
	response := request(h, "POST", "/v2/configuration-defaults", original, testCredential)
	require.Equal(t, 200, response.Code, response.Body.String())
	created := request(h, "POST", "/v2/agents", `{"id":"first_agent","name":"First","configuration_default":"global"}`, testCredential)
	require.Equal(t, 201, created.Code, created.Body.String())
	second := save("two", "second", 1)
	// Editing the selected profile alone updates both future default paths.
	for _, scope := range []string{"global", "claude"} {
		response := request(h, "POST", "/v2/agents", `{"id":"current_`+scope+`","name":"Current","configuration_default":"`+scope+`"}`, testCredential)
		require.Equal(t, 201, response.Code, response.Body.String())
		var agent model.Agent
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &agent))
		require.Equal(t, "second", agent.Desired.Model)
		require.Equal(t, &second.Revision.Ref, agent.ConfigurationProfile)
	}
	response = request(h, "POST", "/v2/configuration-defaults", set("defaults_two", second.Revision.Ref, 1), testCredential)
	require.Equal(t, 200, response.Code, response.Body.String())
	retry := request(h, "POST", "/v2/configuration-defaults", original, testCredential)
	require.Equal(t, 200, retry.Code, retry.Body.String())
	var retried model.ConfigurationDefaults
	require.NoError(t, json.Unmarshal(retry.Body.Bytes(), &retried))
	require.Equal(t, model.Revision(1), retried.Revision)
	stale := request(h, "POST", "/v2/configuration-defaults", set("stale", first.Revision.Ref, 1), testCredential)
	require.Equal(t, 409, stale.Code, stale.Body.String())
	created = request(h, "POST", "/v2/agents", `{"id":"second_agent","name":"Second","configuration_default":"claude"}`, testCredential)
	require.Equal(t, 201, created.Code, created.Body.String())
	var current model.Agent
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &current))
	require.Equal(t, "second", current.Desired.Model)
	old, err := store.Agent(t.Context(), "first_agent")
	require.NoError(t, err)
	require.Equal(t, "first", old.Desired.Model)
	require.Equal(t, &first.Revision.Ref, old.ConfigurationProfile)
}
