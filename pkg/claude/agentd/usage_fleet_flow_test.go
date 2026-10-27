package agentd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func peerJSON(t *testing.T, h http.Handler, method, path string, body any) (int, map[string]any) {
	t.Helper()
	rec := testharness.Serve(h, testharness.JSONRequest(t, method, path, body))
	out := map[string]any{}
	if rec.Body.Len() > 0 {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), rec.Body.String())
	}
	return rec.Code, out
}

// Scenario: a peer reads this node's usage only once it holds usage.read
// (the summary always says which usage grants it holds); it manages this
// node's usage rules only under usage-rules.manage.
func TestUsageFleet_PeerSummaryAndManageFollowGrants(t *testing.T) {
	fh := newFedHarness(t)
	f := fh.f
	t.Cleanup(agentd.SetHumanMessageNotifierForTest(func(_, _, _, _, _ string) {}))
	seedAgentCostSession(t, "fleet-session", "fleet-conv", 30)
	peer := fh.peer.id.ID()
	h := agentd.PeerViewHandler(peer)

	code, out := peerJSON(t, h, http.MethodGet, "/api/usage/node-summary", nil)
	require.Equal(t, http.StatusOK, code, "any trusted peer may ask: %v", out)
	assert.Empty(t, out["grants"])
	assert.Nil(t, out["totals"], "no usage.read: nothing shared")

	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: peer, Slug: agentd.PermUsageRead}))
	code, out = peerJSON(t, h, http.MethodGet, "/api/usage/node-summary", nil)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, []any{"usage.read"}, out["grants"])
	month := out["totals"].(map[string]any)["month"].(map[string]any)
	assert.InDelta(t, 30, month["total_usd"], 0.001)

	code, _ = peerJSON(t, h, http.MethodGet, "/api/usage/rules", nil)
	require.Equal(t, http.StatusForbidden, code, "the rules need usage-rules.manage")
	code, _ = peerJSON(t, h, http.MethodPost, "/api/usage/limits", map[string]any{"scope": "global", "rule": map[string]any{
		"name": "cap", "period": "month", "budget": 100, "steps": []any{map[string]any{"at": 100, "action": "block_spawn"}}}})
	require.Equal(t, http.StatusForbidden, code)

	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: peer, Slug: agentd.PermUsageRulesManage}))
	code, out = peerJSON(t, h, http.MethodPost, "/api/usage/limits", map[string]any{"scope": "global", "rule": map[string]any{
		"name": "cap", "period": "month", "budget": 100, "steps": []any{map[string]any{"at": 100, "action": "block_spawn"}}}})
	require.Equal(t, http.StatusOK, code, "%v", out)
	code, out = peerJSON(t, h, http.MethodGet, "/api/usage/rules", nil)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, out["limits"], 1)

	for _, path := range []string{"/api/usage/fleet"} {
		code, _ = peerJSON(t, h, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusForbidden, code, "%s stays with this node", path)
	}
	_ = f
}

// Scenario: a peer's fleet limit blocks spawns here only in the groups its
// usage-rules.enforce grant covers, records each fired step once with a
// message to this node's operator, and lifts the block when released (or
// when its lease runs out).
func TestUsageFleet_EnforcementFollowsGrantScope(t *testing.T) {
	fh := newFedHarness(t)
	f := fh.f
	var messages []string
	var mu sync.Mutex
	t.Cleanup(agentd.SetHumanMessageNotifierForTest(func(_, _, _, subject, _ string) {
		mu.Lock()
		messages = append(messages, subject)
		mu.Unlock()
	}))
	t.Cleanup(agentd.SetUsageRulesLiveSessionsForTest(func() (map[string]struct{}, error) { return map[string]struct{}{}, nil }))
	t.Cleanup(agentd.SetUsageFleetExchangeForTest(func(string, string, string, []byte) (int, []byte, error) { return 0, nil, nil }))
	alpha := f.HaveGroup("alpha")
	f.HaveGroup("beta")
	peer := fh.peer.id.ID()
	h := agentd.PeerViewHandler(peer)
	enforce := map[string]any{"rule_key": "fleet:1", "rule_name": "team cap", "fleet": "all nodes", "period_key": "2026-10|b=100",
		"metric": "total_usd", "period": "month", "value": 120, "budget": 100, "pct": 120,
		"events": []any{map[string]any{"key": "block_spawn@100", "action": "block_spawn"}}, "block": true, "lease_seconds": 180}

	code, _ := peerJSON(t, h, http.MethodPost, "/api/usage/fleet-enforce", enforce)
	require.Equal(t, http.StatusForbidden, code, "no usage-rules.enforce grant")

	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: peer, Slug: agentd.PermUsageRulesEnforce, Scope: db.FederationGroupScope(alpha.ID)}))
	code, out := peerJSON(t, h, http.MethodPost, "/api/usage/fleet-enforce", enforce)
	require.Equal(t, http.StatusOK, code, "%v", out)
	assert.Equal(t, true, out["block"])

	resp := f.AsHuman().SpawnWith("alpha", map[string]any{"alias": "worker"})
	require.Equal(t, http.StatusConflict, resp.Code, "body=%s", resp.Raw)
	assert.Contains(t, string(resp.Raw), "fleet usage limit")
	assert.Contains(t, string(resp.Raw), "team cap")
	resp = f.AsHuman().SpawnWith("beta", map[string]any{"alias": "other"})
	require.Equal(t, http.StatusOK, resp.Code, "beta is outside the grant; body=%s", resp.Raw)

	// A repeated delivery of the same step is recorded once.
	code, _ = peerJSON(t, h, http.MethodPost, "/api/usage/fleet-enforce", enforce)
	require.Equal(t, http.StatusOK, code)
	mu.Lock()
	blocked := 0
	for _, m := range messages {
		if strings.Contains(m, "blocks new spawns") {
			blocked++
		}
	}
	mu.Unlock()
	assert.Equal(t, 1, blocked, "messages: %v", messages)

	overview := usageCall(t, f, "", http.MethodGet, "/v1/usage/fleet", nil)
	require.Equal(t, http.StatusOK, overview.Code, overview.Body.String())
	var fleet struct {
		Controls []struct {
			RuleName string `json:"rule_name"`
			Block    bool   `json:"block"`
		} `json:"controls"`
	}
	require.NoError(t, json.Unmarshal(overview.Body.Bytes(), &fleet))
	require.Len(t, fleet.Controls, 1, "the operator sees the peer's block in force")
	assert.Equal(t, "team cap", fleet.Controls[0].RuleName)

	code, _ = peerJSON(t, h, http.MethodPost, "/api/usage/fleet-enforce", map[string]any{"rule_key": "fleet:1", "lease_seconds": 0})
	require.Equal(t, http.StatusOK, code)
	resp = f.AsHuman().SpawnWith("alpha", map[string]any{"alias": "worker"})
	require.Equal(t, http.StatusOK, resp.Code, "a release lifts the block; body=%s", resp.Raw)
}

// Scenario: a fleet limit over every trusted peer sums the members that
// share usage with this node, plus this node; past its block step it blocks
// spawns here and sends the block to the member that granted
// usage-rules.enforce, and releases it once the limit is disabled.
func TestUsageFleet_HolderSumsMembersAndEnforces(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	t.Cleanup(agentd.SetHumanMessageNotifierForTest(func(_, _, _, _, _ string) {}))
	t.Cleanup(agentd.SetUsageRulesLiveSessionsForTest(func() (map[string]struct{}, error) { return map[string]struct{}{}, nil }))
	f.HaveGroup("alpha")
	seedAgentCostSession(t, "self-session", "self-conv", 30)

	ann, err := proto.NewIdentity()
	require.NoError(t, err)
	ben, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: ann.ID(), PubKey: ann.Pub, Label: "ann"}))
	require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: ben.ID(), PubKey: ben.Pub, Label: "ben"}))

	month := time.Now().Format("2006-01")
	annSpend := 40.0
	var mu sync.Mutex
	var sent []map[string]any
	summary := func(grants []string, spend float64) []byte {
		out := map[string]any{"grants": grants}
		if spend >= 0 {
			out["totals"] = map[string]any{"month": map[string]any{"total_usd": spend}}
			out["periods"] = map[string]any{"month": month}
		}
		b, _ := json.Marshal(out)
		return b
	}
	t.Cleanup(agentd.SetUsageFleetExchangeForTest(func(peer, method, uri string, body []byte) (int, []byte, error) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case method == http.MethodGet && peer == ann.ID():
			return 200, summary([]string{"usage.read", "usage-rules.enforce"}, annSpend), nil
		case method == http.MethodGet && peer == ben.ID():
			return 200, summary([]string{}, -1), nil
		case method == http.MethodPost && uri == "/api/usage/fleet-enforce":
			var req map[string]any
			require.NoError(t, json.Unmarshal(body, &req))
			req["peer"] = peer
			sent = append(sent, req)
			return 200, []byte(`{"agents":1,"block":` + map[bool]string{true: "true", false: "false"}[req["block"] == true] + `}`), nil
		}
		return 404, []byte(`{}`), nil
	}))

	rec := usageCall(t, f, "", http.MethodPost, "/v1/usage/fleet-limits", map[string]any{"target": "all", "rule": map[string]any{
		"name": "fleet cap", "period": "month", "budget": 100, "steps": []any{
			map[string]any{"at": 50, "action": "notify", "notify": []string{"operator"}},
			map[string]any{"at": 100, "action": "block_spawn"}}}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var limit db.UsageFleetLimit
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &limit))
	assert.True(t, limit.IncludeSelf, "a fleet limit counts this node by default")

	rec = usageCall(t, f, "", http.MethodPost, "/v1/usage/fleet-limits", map[string]any{"target": "pool:nope", "rule": map[string]any{
		"name": "x", "period": "month", "budget": 1, "steps": []any{map[string]any{"at": 100, "action": "block_spawn"}}}})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	rec = usageCall(t, f, "", http.MethodPost, "/v1/usage/fleet-limits", map[string]any{"target": "all", "rule": map[string]any{
		"name": "x", "period": "lifetime", "budget": 1, "steps": []any{map[string]any{"at": 100, "action": "block_spawn"}}}})
	require.Equal(t, http.StatusBadRequest, rec.Code, "no lifetime period for a fleet: %s", rec.Body.String())

	agentd.PollUsageFleetForTest(time.Now())
	status := fleetStatus(t, f)
	assert.InDelta(t, 70, status.Value, 0.001, "this node 30 + ann 40; ben shares nothing")
	assert.Equal(t, 2, status.Counted)
	states := map[string]string{}
	for _, m := range status.Members {
		states[m.Label] = m.State + "/" + m.Enforce
	}
	assert.Equal(t, map[string]string{"this node": "self/self", "ann": "ok/granted", "ben": "not_shared/not_granted"}, states)
	resp := f.AsHuman().SpawnWith("alpha", map[string]any{"alias": "w1"})
	require.Equal(t, http.StatusOK, resp.Code, "under the block step; body=%s", resp.Raw)
	mu.Lock()
	assert.Empty(t, sent, "nothing to enforce yet")
	annSpend = 80
	mu.Unlock()

	agentd.PollUsageFleetForTest(time.Now().Add(time.Minute))
	status = fleetStatus(t, f)
	assert.InDelta(t, 110, status.Value, 0.001)
	resp = f.AsHuman().SpawnWith("alpha", map[string]any{"alias": "w2"})
	require.Equal(t, http.StatusConflict, resp.Code, "the fleet limit counts this node; body=%s", resp.Raw)
	assert.Contains(t, string(resp.Raw), "fleet cap")
	mu.Lock()
	require.Len(t, sent, 1, "only ann granted enforcement")
	assert.Equal(t, ann.ID(), sent[0]["peer"])
	assert.Equal(t, true, sent[0]["block"])
	require.Len(t, sent[0]["events"], 1)
	mu.Unlock()

	rec = usageCall(t, f, "", http.MethodPut, "/v1/usage/fleet-limits/"+jsonID(limit.ID), map[string]any{"enabled": false})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	agentd.PollUsageFleetForTest(time.Now().Add(2 * time.Minute))
	resp = f.AsHuman().SpawnWith("alpha", map[string]any{"alias": "w3"})
	require.Equal(t, http.StatusOK, resp.Code, "a disabled fleet limit never blocks; body=%s", resp.Raw)
	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, len(sent), 2)
	last := sent[len(sent)-1]
	assert.Equal(t, float64(0), last["lease_seconds"], "the block is released at once")
}

type fleetStatusView struct {
	Value   float64 `json:"value"`
	Counted int     `json:"counted"`
	Members []struct {
		Label   string `json:"label"`
		State   string `json:"state"`
		Enforce string `json:"enforce"`
	} `json:"members"`
}

func fleetStatus(t *testing.T, f *testharness.Flow) fleetStatusView {
	t.Helper()
	rec := usageCall(t, f, "", http.MethodGet, "/v1/usage/fleet", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		Statuses []fleetStatusView `json:"statuses"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out.Statuses, 1, rec.Body.String())
	return out.Statuses[0]
}

func jsonID(id int64) string {
	b, _ := json.Marshal(id)
	return string(b)
}
