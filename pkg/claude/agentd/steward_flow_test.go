package agentd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func setSteward(t *testing.T, reads, writes string) {
	t.Helper()
	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Steward = &config.StewardConfig{Reads: reads, Writes: writes}
	require.NoError(t, config.Save(cfg))
}

// The Steward's group is fixed: a summon under any other name would land where
// nothing recognises it. The name compares after trimming.
func TestSteward_SummonRejectsAnotherName(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	enableCollab(t)
	stubScribeTerminal(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	summon := func(name string) int {
		return testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe", map[string]any{
			"name": name, "scope": map[string]any{"kind": "steward"}, "brief": "Help.",
		}))).Code
	}
	assert.Equal(t, http.StatusBadRequest, summon("jeeves"))
	assert.Equal(t, http.StatusBadRequest, summon(""))
	assert.Equal(t, http.StatusOK, summon(" steward "))
}

// The operator's own doors take the steward address too.
func TestSteward_ReachableFromTheDashboard(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	enableCollab(t)
	stubScribeTerminal(t)
	mux := dashMessageMux(t)
	operatorSend := func() int {
		return testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost, "/api/operator-message",
			map[string]any{"to": "steward", "body": "hello"})).Code
	}
	require.Equal(t, http.StatusNotFound, operatorSend(), "no live Steward")

	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe", map[string]any{
		"name": "steward", "scope": map[string]any{"kind": "steward"}, "brief": "Help.",
	})))
	require.Equalf(t, http.StatusOK, rec.Code, "summon body=%s", rec.Body.String())
	conv := decodeScribeResp(t, rec).ConvID

	require.Equal(t, http.StatusAccepted, operatorSend())
	rows, err := db.ListAgentMessagesForConv(conv, 10)
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	assert.Equal(t, "hello", rows[0].Body)

	const worker = "stwd-aaaa-bbbb-cccc-000000000002"
	f.HaveConvWithTitle(worker, "worker-bee")
	f.HaveGroup("team")
	f.HaveMember("team", worker)
	f.HaveAliveSession(worker, "stwd2", "tclaude-stwd2", f.TestCwd("work"))
	sent := postDashMessage(t, mux, map[string]any{"from": worker, "to": "steward", "body": "from a worker"})
	require.Less(t, sent.Code, 300, sent.Body.String())
	rows, err = db.ListAgentMessagesForConv(conv, 10)
	require.NoError(t, err)
	assert.Equal(t, "from a worker", rows[0].Body)
}

// A Steward's access comes from config, whatever the summon asks for, and any
// agent can reach a live one.
func TestSteward_AccessFromConfigAndReachableByAnyAgent(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	enableCollab(t)
	stubScribeTerminal(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	summon := func() string {
		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe", map[string]any{
			"name": "steward", "slugs": []string{agentd.PermNodeExec}, "scope": map[string]any{"kind": "steward"}, "brief": "Help.",
		})))
		require.Equalf(t, http.StatusOK, rec.Code, "summon body=%s", rec.Body.String())
		return decodeScribeResp(t, rec).ConvID
	}
	has := func(conv, slug string) bool {
		effect, ok, err := db.AgentPermissionOverride(conv, slug)
		require.NoError(t, err)
		return ok && effect == "grant"
	}
	// Workers outside the Steward's group: nobody can reach it yet.
	const worker = "stwd-aaaa-bbbb-cccc-000000000001"
	f.HaveGroup("team")
	f.HaveMember("team", worker)
	f.HaveAliveSession(worker, "stwd", "tclaude-stwd", f.TestCwd("work"))
	send := func(to string) int {
		r := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/messages", map[string]any{"to": to, "body": "done"}), worker)
		return testharness.Serve(f.Mux, r).Code
	}
	require.Equal(t, http.StatusNotFound, send("steward"), "no live Steward")

	conv := summon()
	assert.True(t, has(conv, agentd.PermUsageRead), "operator-level reads by default")
	assert.True(t, has(conv, agentd.PermMessageDirect), "messaging by default")
	assert.False(t, has(conv, agentd.PermAgentSpawn), "no spawning by default")
	assert.False(t, has(conv, agentd.PermNodeExec), "a summon cannot ask for more")
	spaced := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe", map[string]any{
		"name": "steward", "slugs": []string{agentd.PermNodeExec}, "scope": map[string]any{"kind": " steward"}, "brief": "Help.",
	})))
	require.Equalf(t, http.StatusOK, spaced.Code, "summon body=%s", spaced.Body.String())
	assert.False(t, has(decodeScribeResp(t, spaced).ConvID, agentd.PermNodeExec), "nor by spelling the scope differently")

	assert.Less(t, send("steward"), 300, "the Steward has an address")
	assert.Less(t, send(conv), 300, "and takes mail from any agent")
	copied := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/messages", map[string]any{"to": conv, "cc": []string{"steward"}, "body": "copy"}), worker)
	assert.NotEqual(t, http.StatusNotFound, testharness.Serve(f.Mux, copied).Code, "the address works in cc too")
	rows, err := db.ListAgentMessagesForConv(conv, 10)
	require.NoError(t, err)
	delivered := 0
	for _, row := range rows {
		if row.Body == "done" {
			delivered++
		}
	}
	assert.Equal(t, 2, delivered, "the summon's brief is mail too")

	// Not an open door: only the Steward, only with Collab on.
	const other = "stwd-aaaa-bbbb-cccc-000000000002"
	f.HaveGroup("elsewhere")
	f.HaveMember("elsewhere", other)
	f.HaveAliveSession(other, "stwo", "tclaude-stwo", f.TestCwd("other"))
	assert.Equal(t, http.StatusForbidden, send(other))
	sessions, err := db.FindSessionsByConvID(conv)
	require.NoError(t, err)
	require.NotEmpty(t, sessions)
	f.MarkOffline(sessions[0].TmuxSession)
	assert.Equal(t, http.StatusForbidden, send(conv), "only a live Steward")

	setSteward(t, config.StewardReadsFleet, config.StewardWritesNone)
	conv = summon()
	assert.True(t, has(conv, agentd.PermFleetRead))
	assert.False(t, has(conv, agentd.PermUsageRead), "fleet-only reads")
	assert.False(t, has(conv, agentd.PermMessageDirect), "no writes")

	setSteward(t, "", config.StewardWritesWorkers)
	conv = summon()
	assert.True(t, has(conv, agentd.PermAgentSpawn))

	// The operator's own prompt reaches a new Steward's brief.
	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Steward = &config.StewardConfig{Writes: config.StewardWritesNone, StartupPrompt: "Reply in Swedish."}
	require.NoError(t, config.Save(cfg))
	rows, err = db.ListAgentMessagesForConv(summon(), 10)
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	assert.Contains(t, rows[0].Body, "Standing instructions from the operator:\nReply in Swedish.")

	// A named Steward answers to that name as well, and keeps its address.
	cfg.Steward = &config.StewardConfig{Name: "Jeeves"}
	require.NoError(t, config.Save(cfg))
	named := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe", map[string]any{
		"name": "steward", "scope": map[string]any{"kind": "steward"}, "brief": "Help.",
	})))
	require.Equalf(t, http.StatusOK, named.Code, "summon body=%s", named.Body.String())
	resp := decodeScribeResp(t, named)
	assert.True(t, strings.HasPrefix(strings.ToLower(resp.Name), "jeeves-"), resp.Name)
	rows, err = db.ListAgentMessagesForConv(resp.ConvID, 10)
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	assert.Contains(t, rows[0].Body, "The operator calls you Jeeves.")
	assert.Less(t, send("jeeves"), 300)
	assert.Less(t, send("steward"), 300)
}

// The companion's band reads what a Steward holds beyond Config from the snapshot.
func TestSteward_SnapshotListsElevatedAccess(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	enableCollab(t)
	stubScribeTerminal(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe", map[string]any{
		"name": "steward", "scope": map[string]any{"kind": "steward"}, "brief": "Help.",
	})))
	require.Equalf(t, http.StatusOK, rec.Code, "summon body=%s", rec.Body.String())
	conv := decodeScribeResp(t, rec).ConvID
	agentID, err := db.AgentIDForConv(conv)
	require.NoError(t, err)

	elevated := func() map[string]struct {
		Standing  []string `json:"standing"`
		Inherited []string `json:"inherited"`
	} {
		rec := testharness.Serve(dashMessageMux(t), testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil))
		require.Equal(t, http.StatusOK, rec.Code)
		var snap struct {
			Elevated map[string]struct {
				Standing  []string `json:"standing"`
				Inherited []string `json:"inherited"`
			} `json:"steward_elevated"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snap))
		return snap.Elevated
	}
	assert.Empty(t, elevated(), "a new Steward holds what Config gives it")

	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermAgentSpawn, "test"))
	assert.Equal(t, []string{agentd.PermAgentSpawn}, elevated()[agentID].Standing)
}

// Config picks the Steward's launch profile; unset, the group carries none, so
// the global default applies.
func TestSteward_ProfileFromConfig(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	enableCollab(t)
	stubScribeTerminal(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	summon := func() {
		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe", map[string]any{
			"name": "steward", "scope": map[string]any{"kind": "steward"}, "brief": "Help.",
		})))
		require.Equalf(t, http.StatusOK, rec.Code, "summon body=%s", rec.Body.String())
	}
	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Scribe = &config.ScribeConfig{Profile: "scribes"}
	require.NoError(t, config.Save(cfg))
	summon()
	g, err := db.GetAgentGroupByName("steward")
	require.NoError(t, err)
	assert.Empty(t, g.DefaultProfile, "the scribe profile is not the Steward's")
}
