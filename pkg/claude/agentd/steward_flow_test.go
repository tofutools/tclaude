package agentd_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/configbundle"
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
	require.NoError(t, db.GrantAgentPermission(worker, agentd.PermMessageDirect, "test"))
	sent := postDashMessage(t, mux, map[string]any{"from": worker, "to": "steward", "body": "from a worker"})
	require.Less(t, sent.Code, 300, sent.Body.String())
	rows, err = db.ListAgentMessagesForConv(conv, 10)
	require.NoError(t, err)
	assert.Equal(t, "from a worker", rows[0].Body)
}

// A Steward's access comes from config, whatever the summon asks for.
func TestSteward_AccessFromConfigAndAddress(t *testing.T) {
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

	refused := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/messages", map[string]any{"to": "steward", "body": "done"}), worker))
	assert.Equal(t, http.StatusForbidden, refused.Code, "a worker cannot write to the Steward first")
	assert.Contains(t, refused.Body.String(), "tclaude agent reply", "and is told what it can do")
	assert.Equal(t, http.StatusForbidden, send(conv), "nor by its conversation")
	const teammate = "stwd-aaaa-bbbb-cccc-000000000003"
	f.HaveMember("team", teammate)
	f.HaveAliveSession(teammate, "stwt", "tclaude-stwt", f.TestCwd("team"))
	viaCc := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/messages", map[string]any{"to": teammate, "cc": []string{"steward"}, "body": "copy"}), worker)
	assert.Equal(t, http.StatusForbidden, testharness.Serve(f.Mux, viaCc).Code, "nor as a copy")

	// Naming the Steward as a child's reply target would hand the child a
	// briefing "from" it to answer.
	require.NoError(t, db.GrantAgentPermission(worker, agentd.PermGroupsMembersSpawn, "test"))
	forged := f.AsAgent(worker).SpawnWith("team", map[string]any{"name": "child", "initial_message": "Task", "reply_to": conv})
	assert.Equal(t, http.StatusForbidden, forged.Code, "spawn body=%s", forged.Raw)

	// The Steward wrote first, so the worker can answer, and the answer lands.
	asked := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/messages", map[string]any{"to": worker, "body": "how is it going?"}), conv))
	require.Lessf(t, asked.Code, 300, "steward send body=%s", asked.Body.String())
	inbox, err := db.ListAgentMessagesForConv(worker, 10)
	require.NoError(t, err)
	require.NotEmpty(t, inbox)
	answered := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, fmt.Sprintf("/v1/messages/%d/reply", inbox[0].ID), map[string]any{"body": "done"}), worker))
	require.Lessf(t, answered.Code, 300, "reply body=%s", answered.Body.String())

	require.NoError(t, db.GrantAgentPermission(worker, agentd.PermMessageDirect, "test"))
	assert.Less(t, send("steward"), 300, "with message.direct the Steward has an address")
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
	assert.Equal(t, 2, delivered, "the answer and the send")

	sessions, err := db.FindSessionsByConvID(conv)
	require.NoError(t, err)
	require.NotEmpty(t, sessions)
	f.MarkOffline(sessions[0].TmuxSession)

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

// The operator's default Steward permissions: saved through the Config tab's
// endpoint, which refuses a slug the registry does not know, and applied by the
// daemon at summon. They count as usual access, and a running Steward keeps
// what it was summoned with.
func TestSteward_DefaultPermissions(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	enableCollab(t)
	stubScribeTerminal(t)
	mux := dashMessageMux(t)

	save := func(path string, slugs ...string) *httptest.ResponseRecorder {
		cfg, err := config.Load()
		require.NoError(t, err)
		cfg.Steward = &config.StewardConfig{DefaultPermissions: slugs}
		return testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost, path, map[string]any{"config": cfg}))
	}
	saved := func() []string {
		cfg, err := config.Load()
		require.NoError(t, err)
		return cfg.StewardDefaultPermissions()
	}
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
	standing := func(conv string) []string {
		agentID, err := db.AgentIDForConv(conv)
		require.NoError(t, err)
		rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil))
		require.Equal(t, http.StatusOK, rec.Code)
		var snap struct {
			Elevated map[string]struct {
				Standing []string `json:"standing"`
			} `json:"steward_elevated"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snap))
		return snap.Elevated[agentID].Standing
	}

	for _, path := range []string{"/api/config?dry_run=1", "/api/config"} {
		rec := save(path, agentd.PermAgentSpawn, "no.such.slug")
		require.Equalf(t, http.StatusBadRequest, rec.Code, "%s body=%s", path, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "no.such.slug")
	}
	assert.Empty(t, saved(), "a refused save writes nothing")

	// message.direct already comes from the default Writes level.
	rec := save("/api/config", agentd.PermAgentSpawn, agentd.PermMessageDirect)
	require.Equalf(t, http.StatusOK, rec.Code, "save body=%s", rec.Body.String())

	conv := summon()
	assert.True(t, has(conv, agentd.PermAgentSpawn), "the default permission is granted at birth")
	assert.True(t, has(conv, agentd.PermMessageDirect))
	assert.False(t, has(conv, agentd.PermNodeExec), "the summon still cannot ask for more")
	assert.Empty(t, standing(conv), "a default permission is usual access, not an elevation")
	rows, err := db.ListAgentMessagesForConv(conv, 10)
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	assert.Contains(t, rows[0].Body, "The operator also granted you these permissions: "+agentd.PermAgentSpawn+". Everything else needs approval.")

	require.Equal(t, http.StatusOK, save("/api/config", agentd.PermAgentTask).Code)
	assert.False(t, has(conv, agentd.PermAgentTask), "a running Steward keeps what it was summoned with")
	assert.Equal(t, []string{agentd.PermAgentSpawn}, standing(conv), "what config no longer gives shows as beyond it")

	// Standing grant authority is refused on save, and ignored from a hand edit.
	assert.NotEqual(t, http.StatusOK, save("/api/config", agentd.PermPermissionsGrant).Code)

	// A hand edit can leave an unknown slug. It grants nothing and must not
	// stop the Steward from starting.
	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Steward = &config.StewardConfig{DefaultPermissions: []string{"no.such.slug", agentd.PermPermissionsGrant, agentd.PermAgentTask}}
	require.NoError(t, config.Save(cfg))
	next := summon()
	assert.True(t, has(next, agentd.PermAgentTask))
	assert.False(t, has(next, "no.such.slug"))
	assert.False(t, has(next, agentd.PermPermissionsGrant))
	rows, err = db.ListAgentMessagesForConv(next, 10)
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	assert.NotContains(t, rows[0].Body, "no.such.slug")
}

// Only the operator's dashboard writes steward.default_permissions: the agent
// socket has no /api/config, and a config bundle, which an agent can import
// with config.import, cannot carry the steward block.
func TestSteward_DefaultPermissionsNotReachableByAgents(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	enableCollab(t)
	const worker = "stwd-aaaa-bbbb-cccc-000000000003"
	f.HaveGroup("team")
	f.HaveMember("team", worker)
	f.HaveAliveSession(worker, "stwd3", "tclaude-stwd3", f.TestCwd("work"))

	steward := json.RawMessage(`{"default_permissions":["` + agentd.PermNodeExec + `"]}`)
	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/api/config", map[string]any{
		"config": map[string]any{"steward": steward},
	}), worker))
	assert.Equal(t, http.StatusNotFound, rec.Code, "the agent socket has no config write")

	bundle := configbundle.Bundle{
		Format: configbundle.Format, FormatVersion: 1,
		Sections: map[string][]configbundle.Item{"config": {{Name: "steward", Value: steward}}},
	}
	rec = testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/config-bundle/import", map[string]any{
		"bundle": bundle, "apply": true,
	})))
	assert.GreaterOrEqual(t, rec.Code, http.StatusBadRequest, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "not portable")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Empty(t, cfg.StewardDefaultPermissions())
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

// A Steward lists what the operator lists, from a group of its own, and seeing
// an agent gives it no way to write to it.
func TestSteward_ListsEveryAgentAndGroup(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	enableCollab(t)
	stubScribeTerminal(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	setSteward(t, config.StewardReadsFleet, config.StewardWritesNone)

	const worker = "stwd-aaaa-bbbb-cccc-000000000003"
	f.HaveConvWithTitle(worker, "worker-bee")
	f.HaveGroup("team")
	f.HaveMember("team", worker)
	f.HaveAliveSession(worker, "stwd3", "tclaude-stwd3", f.TestCwd("work"))
	_, err := db.SetAgentGroupDefaultCwd("team", f.TestCwd("work"))
	require.NoError(t, err)
	const outsider = "stwd-aaaa-bbbb-cccc-000000000004"
	f.HaveGroup("other")
	f.HaveMember("other", outsider)

	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe", map[string]any{
		"name": "steward", "scope": map[string]any{"kind": "steward"}, "brief": "Help.",
	})))
	require.Equalf(t, http.StatusOK, rec.Code, "summon body=%s", rec.Body.String())
	steward := decodeScribeResp(t, rec).ConvID

	peers := func(caller, query string) (int, []string) {
		rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/peers"+query, nil), caller))
		var rows []struct {
			ConvID string `json:"conv_id"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &rows)
		convs := make([]string, 0, len(rows))
		for _, row := range rows {
			convs = append(convs, row.ConvID)
		}
		return rec.Code, convs
	}
	_, convs := peers(steward, "")
	assert.Contains(t, convs, worker, "the Steward shares no group with it")
	code, convs := peers(steward, "?group=team")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, []string{worker}, convs)

	_, convs = peers(outsider, "")
	assert.NotContains(t, convs, worker, "an agent without fleet.read stays scoped")
	code, _ = peers(outsider, "?group=team")
	assert.Equal(t, http.StatusNotFound, code)

	defaultCwd := func(caller string) string {
		rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/groups", nil), caller))
		require.Equal(t, http.StatusOK, rec.Code)
		var groups []struct {
			Name       string `json:"name"`
			DefaultCwd string `json:"default_cwd"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &groups))
		for _, g := range groups {
			if g.Name == "team" {
				return g.DefaultCwd
			}
		}
		t.Fatal("group team is not listed")
		return ""
	}
	assert.Equal(t, f.TestCwd("work"), defaultCwd(steward))
	assert.Empty(t, defaultCwd(outsider))

	sent := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/messages", map[string]any{"to": worker, "body": "hi"}), steward))
	assert.Equal(t, http.StatusForbidden, sent.Code, "with writes off, seeing an agent does not reach it")

	// fleet.read is a Collab surface: with Collab off the grant lifts nothing.
	cfg, err := config.Load()
	require.NoError(t, err)
	require.NotNil(t, cfg.Features)
	cfg.Features.Collab = false
	require.NoError(t, config.Save(cfg))
	_, convs = peers(steward, "")
	assert.NotContains(t, convs, worker)
}

// stewardCarryFlow is a Steward's lifecycle as the dashboard drives it.
type stewardCarryFlow struct {
	t    *testing.T
	f    *testharness.Flow
	dash http.Handler
}

func newStewardCarryFlow(t *testing.T) *stewardCarryFlow {
	f, _ := processAuthoringFlow(t)
	enableCollab(t)
	stubScribeTerminal(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	return &stewardCarryFlow{t: t, f: f, dash: dashMessageMux(t)}
}

func (c *stewardCarryFlow) summon() string {
	c.t.Helper()
	rec := testharness.Serve(c.f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(c.t, http.MethodPost, "/v1/scribe", map[string]any{
		"name": "steward", "scope": map[string]any{"kind": "steward"}, "brief": "Help.",
	})))
	require.Equalf(c.t, http.StatusOK, rec.Code, "summon body=%s", rec.Body.String())
	return decodeScribeResp(c.t, rec).ConvID
}

func (c *stewardCarryFlow) post(path string, body any) *httptest.ResponseRecorder {
	c.t.Helper()
	return testharness.Serve(c.dash, testharness.JSONRequest(c.t, http.MethodPost, path, body))
}

func (c *stewardCarryFlow) shutDown(conv string) {
	c.t.Helper()
	rec := c.post("/api/agents/"+conv+"/retire", nil)
	require.Equalf(c.t, http.StatusOK, rec.Code, "retire body=%s", rec.Body.String())
}

func (c *stewardCarryFlow) agentID(conv string) string {
	c.t.Helper()
	id, err := db.AgentIDForConv(conv)
	require.NoError(c.t, err)
	return id
}

func (c *stewardCarryFlow) sudoSlugs(conv string) map[string]*db.SudoGrant {
	c.t.Helper()
	rows, err := db.ListActiveSudoGrants(conv)
	require.NoError(c.t, err)
	out := map[string]*db.SudoGrant{}
	for _, row := range rows {
		out[row.Slug] = row
	}
	return out
}

func stewardOverride(t *testing.T, conv, slug string) string {
	t.Helper()
	effect, ok, err := db.AgentPermissionOverride(conv, slug)
	require.NoError(t, err)
	if !ok {
		return ""
	}
	return effect
}

// The operator's choice per grant: instance-only ends with the Steward, stored
// reaches the next one, denies and scopes included.
func TestSteward_CarriedPermissionsReachOnlyTheChosenGrants(t *testing.T) {
	c := newStewardCarryFlow(t)
	first := c.summon()
	rec := c.post("/api/permissions", map[string]any{
		"conv": first,
		"overrides": map[string]string{
			agentd.PermNodeExec: "grant", agentd.PermAgentStop: "grant", agentd.PermAgentDelete: "deny",
			agentd.PermGroupsMembersSpawn: "grant",
		},
		"scopes": map[string]any{agentd.PermGroupsMembersSpawn: map[string]any{"group": []string{"dev"}}},
		"carry":  map[string]bool{agentd.PermAgentStop: true, agentd.PermAgentDelete: true, agentd.PermGroupsMembersSpawn: true},
	})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	c.shutDown(first)

	second := c.summon()
	require.NotEqual(t, first, second, "a shut-down Steward is not reused")
	assert.Equal(t, "", stewardOverride(t, second, agentd.PermNodeExec), "this Steward only ends with it")
	assert.Equal(t, "grant", stewardOverride(t, second, agentd.PermAgentStop))
	assert.Equal(t, "deny", stewardOverride(t, second, agentd.PermAgentDelete))
	rows, err := db.ListAgentPermissionOverrideRowsForConv(second)
	require.NoError(t, err)
	var scope string
	for _, row := range rows {
		if row.Slug == agentd.PermGroupsMembersSpawn {
			scope = row.ScopeJSON
		}
	}
	assert.Contains(t, scope, `"dev"`, "the scope comes along")

	// Revoking from a later instance holds for the ones after it.
	rec = c.post("/api/permissions", map[string]any{"conv": second, "overrides": map[string]string{agentd.PermAgentStop: "default"}})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	c.shutDown(second)
	third := c.summon()
	assert.Equal(t, "", stewardOverride(t, third, agentd.PermAgentStop))
	assert.Equal(t, "deny", stewardOverride(t, third, agentd.PermAgentDelete), "the others stay")

	// Taking the choice back keeps the grant here and stops storing it.
	rec = c.post("/api/permissions", map[string]any{
		"conv": third, "overrides": map[string]string{agentd.PermAgentDelete: "deny"}, "carry": map[string]bool{agentd.PermAgentDelete: false},
	})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, "deny", stewardOverride(t, third, agentd.PermAgentDelete))
	c.shutDown(third)
	assert.Equal(t, "", stewardOverride(t, c.summon(), agentd.PermAgentDelete))
}

func TestSteward_CarriedSudoKeepsItsExpiry(t *testing.T) {
	c := newStewardCarryFlow(t)
	first := c.summon()
	rec := c.post("/api/sudo", map[string]any{
		"agent_id": c.agentID(first), "slugs": []string{agentd.PermNodeExec, agentd.PermAgentStop}, "duration": "45s",
		"carry": []string{agentd.PermNodeExec},
	})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	original := c.sudoSlugs(first)[agentd.PermNodeExec].ExpiresAt
	c.shutDown(first)

	second := c.summon()
	held := c.sudoSlugs(second)
	require.Contains(t, held, agentd.PermNodeExec)
	assert.NotContains(t, held, agentd.PermAgentStop, "this Steward only ends with it")
	assert.True(t, original.Equal(held[agentd.PermNodeExec].ExpiresAt), "a new Steward never restarts the clock")

	// Extending reaches the stored grant, and running out ends it for good.
	require.Greater(t, mustExtend(t, held[agentd.PermNodeExec].ID, time.Now().Add(150*time.Millisecond)), int64(0))
	c.shutDown(second)
	time.Sleep(200 * time.Millisecond)
	assert.Empty(t, c.sudoSlugs(c.summon()), "an expired grant is not handed on")
}

func mustExtend(t *testing.T, id int64, to time.Time) int64 {
	t.Helper()
	n, err := db.ExtendSudoGrant(id, to)
	require.NoError(t, err)
	return n
}

func TestSteward_RevokingCarriedSudoHoldsForLaterStewards(t *testing.T) {
	c := newStewardCarryFlow(t)
	first := c.summon()
	rec := c.post("/api/sudo", map[string]any{
		"agent_id": c.agentID(first), "slugs": []string{agentd.PermNodeExec}, "duration": "30m", "carry": []string{agentd.PermNodeExec},
	})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	c.shutDown(first)
	second := c.summon()
	grant := c.sudoSlugs(second)[agentd.PermNodeExec]
	require.NotNil(t, grant)
	revoke := testharness.Serve(c.dash, testharness.JSONRequest(t, http.MethodDelete, fmt.Sprintf("/api/sudo/%d", grant.ID), nil))
	require.Equalf(t, http.StatusOK, revoke.Code, "body=%s", revoke.Body.String())
	c.shutDown(second)
	assert.Empty(t, c.sudoSlugs(c.summon()))
}

// Storing is for real Stewards, and what Config gives stays in Config.
func TestSteward_CarryIsRefusedOutsideItsRules(t *testing.T) {
	c := newStewardCarryFlow(t)
	steward := c.summon()
	const named = "stwd-aaaa-bbbb-cccc-000000000002"
	c.f.HaveGroup("team")
	c.f.HaveMember("team", named)
	c.f.HaveAliveSession(named, "steward", "tclaude-named", c.f.TestCwd("work"))
	namedAgent := c.agentID(named)

	for name, tc := range map[string]struct {
		path string
		body map[string]any
		want int
	}{
		"an agent named steward": {"/api/permissions", map[string]any{"conv": named, "overrides": map[string]string{agentd.PermAgentStop: "grant"}, "carry": map[string]bool{agentd.PermAgentStop: true}}, http.StatusForbidden},
		"its sudo":               {"/api/sudo", map[string]any{"agent_id": namedAgent, "slugs": []string{agentd.PermAgentStop}, "carry": []string{agentd.PermAgentStop}}, http.StatusForbidden},
		"a Config slug":          {"/api/permissions", map[string]any{"conv": steward, "overrides": map[string]string{agentd.PermFleetRead: "grant"}, "carry": map[string]bool{agentd.PermFleetRead: true}}, http.StatusForbidden},
		"grant authority":        {"/api/permissions", map[string]any{"conv": steward, "overrides": map[string]string{agentd.PermPermissionsGrant: "grant"}, "carry": map[string]bool{agentd.PermPermissionsGrant: true}}, http.StatusForbidden},
		"sudo grant authority":   {"/api/sudo", map[string]any{"agent_id": c.agentID(steward), "slugs": []string{agentd.PermFleetRead}, "duration": "5m", "carry": []string{agentd.PermFleetRead}}, http.StatusForbidden},
	} {
		assert.Equal(t, tc.want, c.post(tc.path, tc.body).Code, name)
	}
	c.shutDown(steward)
	assert.Equal(t, "", stewardOverride(t, c.summon(), agentd.PermAgentStop))
}

// Revoke-all is the last resort, so it reaches a stored grant with no Steward
// running, when no live row links to it.
func TestSteward_RevokeAllReachesStoredSudoBetweenStewards(t *testing.T) {
	c := newStewardCarryFlow(t)
	first := c.summon()
	rec := c.post("/api/sudo", map[string]any{"agent_id": c.agentID(first), "slugs": []string{agentd.PermNodeExec},
		"duration": "45s", "carry": []string{agentd.PermNodeExec}})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	c.shutDown(first)

	n, err := db.RevokeAllActiveSudoGrants()
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "the count covers the stored grant")
	assert.Empty(t, c.sudoSlugs(c.summon()))
}

// A CLI revoke or deny of a stored grant holds for later Stewards, and a later
// save of an unrelated row is no choice about a slug the instance does not hold.
func TestSteward_StoredGrantFollowsCLIAndSurvivesUnrelatedSaves(t *testing.T) {
	c := newStewardCarryFlow(t)
	first := c.summon()
	rec := c.post("/api/permissions", map[string]any{"conv": first,
		"overrides": map[string]string{agentd.PermAgentStop: "grant", agentd.PermNodeExec: "grant"},
		"carry":     map[string]bool{agentd.PermAgentStop: true, agentd.PermNodeExec: true}})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	// The row is gone from this instance by other means, so a save posts Default.
	require.NoError(t, func() error { _, err := db.RevokeAgentPermission(first, agentd.PermNodeExec); return err }())
	rec = c.post("/api/permissions", map[string]any{"conv": first,
		"overrides": map[string]string{agentd.PermNodeExec: "default", agentd.PermAgentStop: "grant"}})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	stored, err := db.ListStewardCarriedPermissions()
	require.NoError(t, err)
	assert.Contains(t, stored, agentd.PermNodeExec)

	revoked := testharness.Serve(c.f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/permissions/revoke",
		map[string]any{"target": first, "slug": agentd.PermAgentStop})))
	require.Equalf(t, http.StatusOK, revoked.Code, "body=%s", revoked.Body.String())
	c.shutDown(first)
	assert.Equal(t, "", stewardOverride(t, c.summon(), agentd.PermAgentStop))
}

// Stored sudo stands on the operator's approval: an agent's summon must not
// pass it on, and a Steward scope with an id is not the real Steward.
func TestSteward_StoredSudoReachesOnlyTheOperatorsSummon(t *testing.T) {
	c := newStewardCarryFlow(t)
	first := c.summon()
	rec := c.post("/api/sudo", map[string]any{"agent_id": c.agentID(first), "slugs": []string{agentd.PermNodeExec},
		"duration": "45s", "carry": []string{agentd.PermNodeExec}})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	c.shutDown(first)

	crafted := testharness.Serve(c.f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe", map[string]any{
		"name": "steward", "scope": map[string]any{"kind": "steward", "id": "x"}, "brief": "Help.",
	})))
	assert.Equalf(t, http.StatusBadRequest, crafted.Code, "body=%s", crafted.Body.String())

	const caller = "stwd-aaaa-bbbb-cccc-000000000009"
	c.f.HaveGroup("leads")
	haveSpawnCapableMember(t, c.f, "leads", caller)
	require.NoError(t, db.SetAgentPermissionOverride(caller, agentd.PermGroupsMembersSpawn, db.PermEffectGrant, "test"))
	require.NoError(t, db.SetAgentPermissionOverride(caller, agentd.PermPermissionsGrant, db.PermEffectGrant, "test"))
	byAgent := agentReq(t, c.f, caller, http.MethodPost, "/v1/scribe", map[string]any{
		"name": "steward", "scope": map[string]any{"kind": "steward"}, "brief": "Help.",
	})
	require.Equalf(t, http.StatusOK, byAgent.Code, "body=%s", byAgent.Body.String())
	assert.Empty(t, c.sudoSlugs(decodeScribeResp(t, byAgent).ConvID))
}

// The name reaches a Steward in its brief, and the companion does not summon
// one that is live, so a save has to tell it.
func TestSteward_RunningStewardHearsANewName(t *testing.T) {
	c := newStewardCarryFlow(t)
	conv := c.summon()
	saveName := func(name string) {
		cfg, err := config.Load()
		require.NoError(t, err)
		if cfg.Steward == nil {
			cfg.Steward = &config.StewardConfig{}
		}
		cfg.Steward.Name = name
		rec := c.post("/api/config", map[string]any{"config": cfg})
		require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	}
	nameMail := func() []string {
		msgs, err := db.ListAgentMessagesForConv(conv, 50)
		require.NoError(t, err)
		var bodies []string
		for _, m := range msgs {
			if m.Subject == "Your name changed" {
				bodies = append(bodies, m.Body)
			}
		}
		return bodies
	}

	saveName("Ada")
	require.Len(t, nameMail(), 1)
	assert.Contains(t, nameMail()[0], "calls you Ada")

	saveName("Ada")
	assert.Len(t, nameMail(), 1, "a save that keeps the name says nothing")

	saveName("")
	assert.Len(t, nameMail(), 2, "and clearing it is a change too")
}

func stewardOverrideHeld(t *testing.T, conv, slug string) (string, bool) {
	t.Helper()
	overrides, err := db.ListAgentPermissionOverrideRowsForConv(conv)
	require.NoError(t, err)
	for _, row := range overrides {
		if row.Slug == slug {
			return row.Effect, true
		}
	}
	return "", false
}

// Scenario: a release adds a slug to the base set. A running Steward gets it and
// hears about it, unless it denies it, and what the operator takes off later,
// by any route, stays off.
func TestSteward_ReconcileAddsANewSlugAndKeepsRemovalsAndDenies(t *testing.T) {
	c := newStewardCarryFlow(t)
	conv := c.summon()
	accessMail := func() []*db.AgentMessage {
		messages, err := db.ListUndeliveredAgentMessagesFor(conv)
		require.NoError(t, err)
		var out []*db.AgentMessage
		for _, message := range messages {
			if message.Subject == "Your access changed" {
				out = append(out, message)
			}
		}
		return out
	}
	require.NoError(t, db.SetAgentPermissionOverride(conv, agentd.PermPromptsManage, db.PermEffectDeny, "test"))
	t.Cleanup(agentd.AddStewardBaseSlugForTest(agentd.PermRolesManage))
	t.Cleanup(agentd.AddStewardBaseSlugForTest(agentd.PermPromptsManage))
	_, err := db.RevokeAgentPermission(conv, agentd.PermHumanNotify)
	require.NoError(t, err)

	agentd.ReconcileStewardAccessForTest()

	effect, held := stewardOverrideHeld(t, conv, agentd.PermRolesManage)
	require.True(t, held, "a slug that is simply new arrives")
	assert.Equal(t, db.PermEffectGrant, effect)
	_, held = stewardOverrideHeld(t, conv, agentd.PermHumanNotify)
	assert.False(t, held, "offered once and taken off, so it stays off")
	effect, _ = stewardOverrideHeld(t, conv, agentd.PermPromptsManage)
	assert.Equal(t, db.PermEffectDeny, effect, "a deny stays")

	mail := accessMail()
	require.Len(t, mail, 1)
	assert.Contains(t, mail[0].Body, agentd.PermRolesManage)
	assert.NotContains(t, mail[0].Body, agentd.PermPromptsManage)
	assert.Contains(t, mail[0].Body, "guide capture -o /tmp/view.png")
	assert.Contains(t, mail[0].Body, "tclaude agent knowledge search")

	// Taken off after it arrived, and nothing further to say.
	_, err = db.RevokeAgentPermission(conv, agentd.PermRolesManage)
	require.NoError(t, err)
	agentd.ReconcileStewardAccessForTest()
	_, held = stewardOverrideHeld(t, conv, agentd.PermRolesManage)
	assert.False(t, held)
	assert.Len(t, accessMail(), 1)
}

// Scenario: a Steward from before the record, like the one that could not take a
// screenshot. It gets capture, and any other gap counts as deliberate.
func TestSteward_ReconcileOfAStewardFromBeforeTheRecord(t *testing.T) {
	c := newStewardCarryFlow(t)
	conv := c.summon()
	d, err := db.Open()
	require.NoError(t, err)
	_, err = d.Exec(`DELETE FROM steward_offered_permissions WHERE slug <> ?`, db.StewardRecordOpened)
	require.NoError(t, err)
	for _, slug := range []string{agentd.PermDashboardCapture, agentd.PermDashboardMark, agentd.PermHumanNotify} {
		_, err = db.RevokeAgentPermission(conv, slug)
		require.NoError(t, err)
	}

	agentd.ReconcileStewardAccessForTest()

	effect, _ := stewardOverrideHeld(t, conv, agentd.PermDashboardCapture)
	assert.Equal(t, db.PermEffectGrant, effect)
	effect, _ = stewardOverrideHeld(t, conv, agentd.PermDashboardMark)
	assert.Equal(t, db.PermEffectGrant, effect)
	_, held := stewardOverrideHeld(t, conv, agentd.PermHumanNotify)
	assert.False(t, held)
}

// Scenario: an agent reaches the Steward's group and description some other way
// than a summon. Those are not access, so reconcile gives it nothing.
func TestSteward_ReconcileLeavesAnAgentThatWasNeverSummoned(t *testing.T) {
	c := newStewardCarryFlow(t)
	conv := c.summon()
	d, err := db.Open()
	require.NoError(t, err)
	_, err = d.Exec(`DELETE FROM steward_offered_permissions`)
	require.NoError(t, err)
	_, err = db.RevokeAgentPermission(conv, agentd.PermDashboardCapture)
	require.NoError(t, err)

	agentd.ReconcileStewardAccessForTest()

	_, held := stewardOverrideHeld(t, conv, agentd.PermDashboardCapture)
	assert.False(t, held)
}
