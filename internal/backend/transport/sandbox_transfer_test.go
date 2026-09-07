package transport

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sandboxpolicy"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestPublicSandboxTransferUsesOperatorBoundaryAndConflicts(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry())
	handler := testHandler(t, service)
	created := request(handler, "POST", "/v2/sandbox-profiles", `{"request_id":"create","id":"original","name":"Original","policy":{}}`, testCredential)
	require.Equal(t, 200, created.Code)
	var saved app.SandboxProfileResult
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &saved))
	data, err := json.Marshal(map[string]any{"ref": saved.Revision.Ref})
	require.NoError(t, err)
	exported := request(handler, "POST", "/v2/sandbox-profiles/export", string(data), testCredential)
	require.Equal(t, 200, exported.Code, exported.Body.String())
	var bundle sandboxpolicy.Bundle
	require.NoError(t, json.Unmarshal(exported.Body.Bytes(), &bundle))
	inspected := request(handler, "POST", "/v2/sandbox-profiles/import/inspect", exported.Body.String(), testCredential)
	require.Equal(t, 200, inspected.Code, inspected.Body.String())
	command := map[string]any{"request_id": "import", "bundle": bundle, "selections": []app.SandboxImportSelection{{Source: saved.Revision.Ref, ID: "copy", RevisionID: "copy_revision", Name: "Copy"}}}
	data, err = json.Marshal(command)
	require.NoError(t, err)
	response := request(handler, "POST", "/v2/sandbox-profiles/import", string(data), testCredential)
	require.Equal(t, 200, response.Code, response.Body.String())
	require.NotContains(t, response.Body.String(), "Generation")
	require.NotContains(t, response.Body.String(), "Authority")
	repeated := request(handler, "POST", "/v2/sandbox-profiles/import", string(data), testCredential)
	require.Equal(t, 200, repeated.Code)
	require.Contains(t, repeated.Body.String(), `"Repeated":true`)
	command["request_id"] = "different"
	data, err = json.Marshal(command)
	require.NoError(t, err)
	require.Equal(t, 409, request(handler, "POST", "/v2/sandbox-profiles/import", string(data), testCredential).Code)
	agent, err := NewHandler(service, fixedCaller{model.AgentPrincipal("untrusted")})
	require.NoError(t, err)
	for _, test := range []struct{ path, body string }{{"/v2/sandbox-profiles/export", `{"ref":{}}`}, {"/v2/sandbox-profiles/import/inspect", exported.Body.String()}, {"/v2/sandbox-profiles/import", string(data)}} {
		require.Equal(t, 401, request(handler, "POST", test.path, test.body, "").Code)
		require.Equal(t, 403, request(agent, "POST", test.path, test.body, "").Code)
	}
}
