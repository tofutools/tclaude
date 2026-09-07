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

func TestPublicSandboxProfilesPreserveAuthoredPolicyAndRejectClaimedAuthority(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry())
	handler := testHandler(t, service)
	body := `{"request_id":"create","id":"sandbox_profile","name":"Profile","policy":{"Filesystem":[{"HostPath":"/missing/host","Access":"read"}],"PreLaunch":[{"Name":"setup","Script":"printf '%s' $VALUE"}]}}`
	require.Equal(t, 401, request(handler, "POST", "/v2/sandbox-profiles", body, "").Code)
	response := request(handler, "POST", "/v2/sandbox-profiles", body, testCredential)
	require.Equal(t, 200, response.Code, response.Body.String())
	duplicate := request(handler, "POST", "/v2/sandbox-profiles", strings.Replace(body, `"request_id":"create"`, `"request_id":"another-create"`, 1), testCredential)
	require.Equal(t, 409, duplicate.Code, duplicate.Body.String())
	require.NotContains(t, response.Body.String(), "Generation")
	require.NotContains(t, response.Body.String(), "Authority")
	var saved app.SandboxProfileResult
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &saved))
	require.Equal(t, "printf '%s' $VALUE", saved.Revision.Policy.PreLaunch[0].Script)
	ref, err := json.Marshal(saved.Revision.Ref)
	require.NoError(t, err)
	inspected := request(handler, "POST", "/v2/sandbox-profiles/inspect", fmt.Sprintf(`{"ref":%s}`, ref), testCredential)
	require.Equal(t, 200, inspected.Code, inspected.Body.String())
	require.Contains(t, inspected.Body.String(), "/missing/host")
	for _, invalid := range []string{
		strings.Replace(body, `"policy":`, `"principal":{"Kind":"operator"},"policy":`, 1),
		strings.Replace(body, "Profile", string([]byte{255}), 1),
		strings.Replace(body, `"HostPath":"/missing/host"`, `"HostPath":"/missing/host","unexpected":true`, 1),
	} {
		require.Equal(t, 400, request(handler, "POST", "/v2/sandbox-profiles", invalid, testCredential).Code)
	}
	require.Equal(t, 422, request(handler, "POST", "/v2/sandbox-profiles/inspect", `{"ref":{"ProfileID":"sandbox_profile","RevisionID":"sandbox_revision","ContentHash":"invalid"}}`, testCredential).Code)
	agentHandler, err := NewHandler(service, fixedCaller{model.AgentPrincipal("agent")})
	require.NoError(t, err)
	require.Equal(t, 403, request(agentHandler, "POST", "/v2/sandbox-profiles", body, "").Code)
	require.Equal(t, 403, request(agentHandler, "GET", "/v2/sandbox-profiles", "", "").Code)
	require.Equal(t, 401, request(handler, "GET", "/v2/sandbox-network-packs", "", "").Code)
	require.Equal(t, 403, request(agentHandler, "GET", "/v2/sandbox-network-packs", "", "").Code)
	packs := request(handler, "GET", "/v2/sandbox-network-packs", "", testCredential)
	require.Equal(t, 200, packs.Code)
	require.Contains(t, packs.Body.String(), "api.anthropic.com")
	require.Contains(t, packs.Body.String(), "ContentHash")

	for _, network := range []string{
		`{"Baseline":"deny","Packs":["net-typo"]}`,
		`{"Baseline":"deny","Packs":["net-anthropic","net-anthropic"]}`,
		`{"Baseline":"allow","DenyPacks":["net-local","net-local"]}`,
		`{"Baseline":"deny","Packs":["net-local"],"DenyPacks":["net-local"]}`,
	} {
		invalidPack := fmt.Sprintf(`{"request_id":"invalid-pack","id":"invalid_pack","name":"Invalid","policy":{"Network":%s}}`, network)
		require.Equal(t, 422, request(handler, "POST", "/v2/sandbox-profiles", invalidPack, testCredential).Code)
	}
	require.Equal(t, 404, request(handler, "GET", "/v2/sandbox-profiles/invalid_pack", "", testCredential).Code)
	require.Equal(t, 422, request(handler, "GET", "/v2/sandbox-profiles?include_archived=perhaps", "", testCredential).Code)
	archive := request(handler, "POST", "/v2/sandbox-profiles/sandbox_profile/archive", `{"request_id":"archive","expected_revision":1,"archived":true}`, testCredential)
	require.Equal(t, 200, archive.Code, archive.Body.String())
	require.JSONEq(t, `[]`, request(handler, "GET", "/v2/sandbox-profiles", "", testCredential).Body.String())
	require.Equal(t, 200, request(handler, "GET", "/v2/sandbox-profiles/sandbox_profile", "", testCredential).Code)
}
