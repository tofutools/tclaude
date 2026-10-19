package agentd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
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
	require.NoError(t, applyTeleportModelCredentials(b, "proxy:allowed@gateway"))
	b.Manifest.Agent.Harness = "copilot"
	require.NoError(t, applyTeleportModelCredentials(b, "proxy:allowed@gateway"))
	b.Manifest.Agent.Harness = "gemini"
	require.ErrorContains(t, applyTeleportModelCredentials(b, "proxy:allowed@gateway"), "GenerateContent")
	b.Manifest.Agent.Harness = "opencode"
	require.ErrorContains(t, applyTeleportModelCredentials(b, "proxy:allowed@gateway"), "persistent account")
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
		seed, err := seedProfileFromConv("snapshot-conv")
		require.NoError(t, err)
		require.Equal(t, want, seed.ModelProxy)
		merged, _ := mergeSnapshotInlineProfile(&db.SpawnProfile{ModelProxy: "previous@peer"}, &db.SpawnProfile{ModelProxy: trace.ModelProxy}, true)
		require.NotNil(t, merged)
		require.Equal(t, want, merged.ModelProxy)
	}
}

func TestModelProxyMalformedProviderErrorRetainsRetryHeaders(t *testing.T) {
	for _, body := range []string{"", "not JSON", strings.Repeat("x", 33<<10), `{"type":"error","error":{"type":"api_error","message":"provider-secret"}}`} {
		resp := &http.Response{StatusCode: 503, Header: http.Header{"Retry-After": {"17"}, "X-Should-Retry": {"false"}}, Body: io.NopCloser(strings.NewReader(body))}
		rec := httptest.NewRecorder()
		writeModelProviderError(rec, resp, "provider-secret")
		require.Equal(t, 503, rec.Code)
		require.Equal(t, "17", rec.Header().Get("Retry-After"))
		require.Equal(t, "false", rec.Header().Get("X-Should-Retry"))
		require.NotContains(t, rec.Body.String(), "provider-secret")
		require.Contains(t, rec.Body.String(), "provider refused the request")
	}
}

func TestModelOpenAIUsageRequiresValidTerminalCounts(t *testing.T) {
	for _, data := range []string{
		`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":3,"input_tokens_details":{"cached_tokens":7}}}}`,
		`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10}}}`,
		`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":-1,"output_tokens":3}}}`,
		`{"type":"response.incomplete","response":{"usage":{"input_tokens":10,"output_tokens":3}}}`,
	} {
		u := &db.ModelProxyUsage{}
		o := modelUsageObserver{usage: u, maxInput: 50, maxOutput: 20, openai: true}
		o.event([]byte("data: " + data + "\n\n"))
		complete := o.stopped && o.sawInput && o.sawOutput && !o.invalid
		require.Equal(t, strings.Contains(data, "cached_tokens"), complete, data)
		if complete {
			require.EqualValues(t, 13, u.InputTokens+u.OutputTokens+u.CacheReadTokens)
			o.event([]byte("data: " + data + "\n\n"))
			require.True(t, o.invalid, "duplicate terminal accounting must not settle")
		}
	}
}

func TestModelPricingCosts(t *testing.T) {
	claude, ok := lookupModelPricing(&config.ModelProxyPolicy{}, "claude-sonnet-5-5")
	require.True(t, ok)
	// $2 in, $10 out, $0.10 read, $2.50 5m write, $4 1h write per MTok.
	u := db.ModelProxyUsage{InputTokens: 1000, OutputTokens: 1000, CacheReadTokens: 10000, CacheWriteTokens: 3000, CacheWrite1hTokens: 1000}
	require.EqualValues(t, 2000+10000+1000+5000+4000, claude.costMicroUSD(u))
	require.EqualValues(t, 100*4+10*10, claude.worstCaseMicroUSD(100, 10), "the dearest input rate bounds every prompt token")

	haiku, ok := lookupModelPricing(nil, "claude-haiku-5-5")
	require.True(t, ok)
	require.EqualValues(t, 1000*0.10, haiku.costMicroUSD(db.ModelProxyUsage{InputTokens: 1000}))
	require.EqualValues(t, 50000*0.50+60000*0.05, haiku.costMicroUSD(db.ModelProxyUsage{InputTokens: 50000, CacheReadTokens: 60000}), "over 100k prompt tokens prices the whole request at the long tier")

	gpt, ok := lookupModelPricing(&config.ModelProxyPolicy{Dialect: "openai"}, "gpt-6-sol")
	require.True(t, ok)
	require.EqualValues(t, 1000*2+1000*0.2+100*10, gpt.costMicroUSD(db.ModelProxyUsage{InputTokens: 1000, CacheReadTokens: 1000, OutputTokens: 100}))

	_, ok = lookupModelPricing(&config.ModelProxyPolicy{Dialect: "openai"}, "claude-sonnet-5-5")
	require.False(t, ok, "an OpenAI gateway does not borrow Claude prices")
	own, ok := lookupModelPricing(&config.ModelProxyPolicy{Prices: map[string]config.ModelProxyPrice{"local-*": {Input: 1, Output: 2}, "local-big": {Input: 3, Output: 4}}}, "local-big")
	require.True(t, ok)
	require.Equal(t, 3.0, own.short.input, "an exact price beats a pattern")
	own, ok = lookupModelPricing(&config.ModelProxyPolicy{Prices: map[string]config.ModelProxyPrice{"local-*": {Input: 1, Output: 2}}}, "local-small")
	require.True(t, ok)
	require.Equal(t, 1.0, own.short.cacheRead, "an unset cache rate falls back to input")
	require.Equal(t, "$1.31", formatUSD(1_300_001))
}

func TestModelUsageParseIgnoresNonCountFields(t *testing.T) {
	u := db.ModelProxyUsage{}
	o := modelUsageObserver{usage: &u}
	o.parse([]byte(`{"input_tokens":5,"cache_creation_input_tokens":7,"cache_creation":{"ephemeral_5m_input_tokens":3,"ephemeral_1h_input_tokens":4},"service_tier":"standard","server_tool_use":{"web_search_requests":0},"output_tokens":2}`))
	require.True(t, o.sawInput && o.sawOutput)
	require.EqualValues(t, 5, u.InputTokens)
	require.EqualValues(t, 7, u.CacheWriteTokens)
	require.EqualValues(t, 4, u.CacheWrite1hTokens)

	v := db.ModelProxyUsage{}
	o = modelUsageObserver{usage: &v, openai: true}
	o.parse([]byte(`{"input_tokens":100,"input_tokens_details":{"cached_tokens":80},"output_tokens":9,"output_tokens_details":{"reasoning_tokens":4}}`))
	require.EqualValues(t, 20, v.InputTokens, "OpenAI cached input is split out of input_tokens")
	require.EqualValues(t, 80, v.CacheReadTokens)
	require.EqualValues(t, 9, v.OutputTokens)
}
