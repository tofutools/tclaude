package agentd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestModelProxyBindingRequiresWorkerScopeAndRevokes(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: "peer", PubKey: bytes.Repeat([]byte{1}, 32), Label: "gateway"}))
	groupID, err := db.CreateAgentGroup("workers", "")
	require.NoError(t, err)
	group, err := db.GetAgentGroupByID(groupID)
	require.NoError(t, err)
	require.NoError(t, db.ReplaceAgentGroupPermissionGrants(groupID, []db.PermissionGrant{{Slug: PermModelsProxy, Scope: `{"peer":["peer"],"http_proxy":["allowed"]}`, ScopeSpecified: true}}, "test"))
	row := &db.SessionRow{ID: "launch", Status: "idle", ExitLaunchGeneration: "generation"}
	require.NoError(t, db.SaveSession(row))
	rememberHTTPProxyLaunchGroup(row.ID, group)
	t.Cleanup(func() { httpProxyLaunchGroups.Delete(row.ID) })
	bearer := strings.Repeat("a", 64)
	hash := sha256.Sum256([]byte(bearer))
	bind := func(name string) int {
		body, _ := json.Marshal(map[string]string{"reference": name + "@gateway", "bearer_hash": hex.EncodeToString(hash[:])})
		r := httptest.NewRequest(http.MethodPost, "/v1/models/bind", bytes.NewReader(body))
		r = r.WithContext(context.WithValue(r.Context(), httpProxyLaunchRowKey{}, row))
		w := httptest.NewRecorder()
		handleModelProxyBind(w, r)
		return w.Code
	}
	require.Equal(t, 403, bind("other"))
	require.Equal(t, 200, bind("allowed"))
	launch, err := db.VerifyModelProxyLaunch(row.ID, bearer)
	require.NoError(t, err)
	require.Equal(t, "allowed@peer", launch.Reference)
	require.NoError(t, db.ReplaceAgentGroupPermissions(groupID, nil, "test"))
	r := httptest.NewRequest("GET", "/v1/models/request/v1/models", nil)
	r.Header.Set("Authorization", "Bearer "+bearer)
	r = r.WithContext(context.WithValue(r.Context(), httpProxyLaunchRowKey{}, row))
	_, _, _, _, err = modelBoundCaller(r)
	require.Error(t, err)
}
func TestTeleportCredentialsReplaceProfileGateway(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: "peer", PubKey: bytes.Repeat([]byte{1}, 32), Label: "gateway"}))
	b := &agentbundle.Bundle{}
	b.Manifest.Agent.Harness = "claude"
	b.Manifest.Agent.Profile = json.RawMessage(`{"model_proxy":"source@elsewhere","model":"sonnet"}`)
	require.NoError(t, applyTeleportModelCredentials(b, "proxy:allowed@gateway"))
	var p spawnProfileJSON
	require.NoError(t, json.Unmarshal(b.Manifest.Agent.Profile, &p))
	require.Equal(t, "allowed@peer", p.ModelProxy)
	require.NoError(t, applyTeleportModelCredentials(b, "local"))
	require.NoError(t, json.Unmarshal(b.Manifest.Agent.Profile, &p))
	require.Equal(t, "off", p.ModelProxy)
	b.Manifest.Agent.Harness = "codex"
	require.ErrorContains(t, applyTeleportModelCredentials(b, "proxy:allowed@gateway"), "Claude")
}
func TestModelUsageBoundsBeforeForwardingEvents(t *testing.T) {
	for _, raw := range []string{`data: {"type":"message_delta","usage":{"output_tokens":21}}` + "\n\n", `data: {"type":"content_block_delta","delta":{"text":"provider-secret-test"}}` + "\n\n"} {
		observer := &modelUsageObserver{usage: &db.ModelProxyUsage{}, maxInput: 50, maxOutput: 20, credential: "provider-secret-test"}
		w := httptest.NewRecorder()
		require.Error(t, relayModelEvents(w, strings.NewReader(raw), 4096, 4096, observer))
		require.Empty(t, w.Body.String())
	}
}

func TestModelGatewaySharedDefaultResolutionAndExplicitOff(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: "peer", PubKey: bytes.Repeat([]byte{1}, 32), Label: "gateway"}))
	profile := &db.SpawnProfile{Name: "gateway-default", Harness: "claude", ModelProxy: "allowed@gateway"}
	_, err := db.CreateSpawnProfile(profile)
	require.NoError(t, err)
	groupID, err := db.CreateAgentGroup("workers", "")
	require.NoError(t, err)
	_, err = db.SetAgentGroupDefaultProfile("workers", profile.Name)
	require.NoError(t, err)
	group, err := db.GetAgentGroupByID(groupID)
	require.NoError(t, err)
	for _, tc := range []struct{ harness, ref, want string }{{"claude", "", "allowed@peer"}, {"claude", "off", "off"}, {"codex", "", ""}} {
		p := spawnParams{Harness: tc.harness, ModelProxy: tc.ref}
		require.Nil(t, applyDefaultProfile(group, &p))
		require.Equal(t, tc.want, p.ModelProxy)
	}
	launch, fail := resolveTemplateAgentLaunch(group, db.GroupTemplateAgent{ProfileInline: &db.SpawnProfile{Harness: "claude", ModelProxy: "allowed@gateway"}}, nil, "/tmp", "")
	require.Nil(t, fail)
	require.Equal(t, "allowed@peer", launch.ModelProxy)
	_, fail = runNonInteractiveSpawn(context.Background(), spawnParams{ModelProxy: "allowed@peer"}, 1)
	require.NotNil(t, fail)
	require.Equal(t, "unsupported_model_proxy", fail.Kind)
}

func TestModelGatewaySnapshotTraceKeepsRecordedCredentialMode(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, db.SaveSession(&db.SessionRow{ID: "snapshot", ConvID: "snapshot-conv", Harness: "claude", Status: "idle"}))
	for _, ref := range []string{"allowed@peer", ""} {
		require.NoError(t, db.RecordSessionModelProxy("snapshot", ref))
		trace := traceMemberLaunch("snapshot-conv")
		require.True(t, trace.Traced)
		want := ref
		if want == "" {
			want = "off"
		}
		require.Equal(t, want, trace.ModelProxy)
		merged, _ := mergeSnapshotInlineProfile(&db.SpawnProfile{ModelProxy: "previous@peer"}, &db.SpawnProfile{ModelProxy: trace.ModelProxy}, true)
		require.NotNil(t, merged)
		require.Equal(t, want, merged.ModelProxy)
	}
}
