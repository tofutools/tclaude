package agentd_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestPeerSnapshotDashboardSchemaAndSummary(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	f.HaveGroup("team")
	f.HaveGroup("also-visible")
	f.HaveGroup("hidden")
	f.HaveConvWithTitle("schema-agent", "shown")
	f.HaveMemberWithRole("team", "schema-agent", "builder")
	f.HaveMember("also-visible", "schema-agent")
	f.HaveMember("hidden", "schema-agent")
	f.HaveAliveSession("schema-agent", "schema-session", "schema-tmux", f.TestCwd("private-worktree"))
	f.SetSessionStatus("schema-agent", "awaiting_input")
	f.HaveConvWithTitle("hidden-agent", "secret-name")
	f.HaveMember("hidden", "hidden-agent")
	f.HaveAliveSession("hidden-agent", "hidden-session", "hidden-tmux", f.TestCwd("hidden-work"))
	f.SetSessionStatus("hidden-agent", "awaiting_input")
	f.HaveConvWithTitle("loose-agent", "secret-loose")
	f.HaveEnrolledAgent("loose-agent")
	f.HaveAliveSession("loose-agent", "loose-session", "loose-tmux", f.TestCwd("loose-work"))
	require.NoError(t, db.UpdateSessionModel("schema-session", "shared-model"))
	require.NoError(t, db.UpdateSessionEffort("schema-session", "high"))
	require.NoError(t, db.UpdateContextSnapshot("schema-session", 25, 100, 20, 1000))
	require.NoError(t, db.UpdateSessionCost("schema-session", 12345))
	aid, err := db.AgentIDForConv("schema-agent")
	require.NoError(t, err)
	_, err = db.SetAgentTaskRef(aid, "https://tracker.test/task/one?token=private-token#private", "shared-task")
	require.NoError(t, err)
	grant := func(slug, group string) {
		t.Helper()
		scope := ""
		if group != "" {
			scope = "group=" + group
		}
		rec := fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug, "scope": scope})
		require.Equal(t, 200, rec.Code, rec.Body.String())
	}
	grant(agentd.PermGroupsRosterRead, "team")
	grant(agentd.PermGroupsRosterRead, "also-visible")
	h := agentd.PeerViewHandler(p.id.ID())
	read := func(path string) map[string]any {
		t.Helper()
		rec := testharness.Serve(h, testharness.JSONRequest(t, "GET", path, nil))
		require.Equal(t, 200, rec.Code, rec.Body.String())
		var out map[string]any
		testharness.DecodeJSON(t, rec, &out)
		return out
	}
	plain := read("/api/snapshot")
	require.Equal(t, []any{}, plain["pending"])
	require.Equal(t, []any{}, plain["profiles"])
	require.Equal(t, []any{}, plain["harnesses"])
	require.Equal(t, []any{}, plain["templates"])
	require.Equal(t, []any{}, plain["messages"])
	require.Equal(t, []any{}, plain["ungrouped"])
	require.Empty(t, plain["auth_session"].(map[string]any)["minted_at"])
	require.False(t, plain["usage"].(map[string]any)["available"].(bool))
	require.Empty(t, plain["permissions"].(map[string]any)["defaults"])
	summary := read("/api/node-summary")
	require.Equal(t, float64(2), summary["shared_groups"])
	require.Equal(t, float64(1), summary["shared_agents"])
	require.Equal(t, float64(0), summary["online_agents"])
	require.Equal(t, float64(0), summary["waiting_for_input"])
	require.NotContains(t, summary, "resources")
	require.NotContains(t, summary, "health")
	grant(agentd.PermAgentsStatusRead, "team")
	rec := testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/snapshot?static_version=local-version", nil))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var snap dashSnapshot
	testharness.DecodeJSON(t, rec, &snap)
	require.Len(t, snap.Groups, 2)
	require.Len(t, snap.Agents, 1)
	require.Equal(t, "schema-agent", snap.Agents[0].ConvID)
	require.ElementsMatch(t, []string{"team", "also-visible"}, snap.Agents[0].Groups)
	var team dashGroup
	for _, g := range snap.Groups {
		if g.Name == "team" {
			team = g
		}
	}
	require.Len(t, team.Members, 1)
	require.Equal(t, "builder", team.Members[0].Role)
	require.True(t, team.Members[0].Online)
	require.Equal(t, "awaiting_input", team.Members[0].State.Status)
	require.Equal(t, "shared-model", team.Members[0].State.Model)
	require.Equal(t, float64(25), team.Members[0].State.ContextPct)
	for _, secret := range []string{"secret-name", "secret-loose", "private-worktree", "hidden-work", "private-token", "hidden-session", `"cost_usd"`, `"status_detail"`, `"static_unchanged":true`} {
		require.NotContains(t, rec.Body.String(), secret)
	}
	require.Contains(t, rec.Body.String(), `"task_ref_url":"https://tracker.test/task/one"`)
	// Required snapshot keys match the ordinary local dashboard schema. Fields
	// with omitempty may legitimately be absent after peer filtering.
	local := testharness.Serve(agentd.BuildDashboardHandlerForTest(), testharness.JSONRequest(t, "GET", "/api/snapshot", nil))
	require.Equal(t, 200, local.Code, local.Body.String())
	var localMap, peerMap map[string]any
	require.NoError(t, json.Unmarshal(local.Body.Bytes(), &localMap))
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &peerMap))
	for _, key := range agentd.RequiredDashboardSnapshotFieldsForTest() {
		require.Contains(t, localMap, key, "local required snapshot field missing")
		require.Contains(t, peerMap, key, "peer required snapshot field missing")
	}
	// A role/roster-only group cannot inherit the status granted on another group.
	for _, g := range peerMap["groups"].([]any) {
		row := g.(map[string]any)
		if row["name"] == "also-visible" {
			m := row["members"].([]any)[0].(map[string]any)
			require.Empty(t, m["state"])
			require.Equal(t, false, m["online"])
		}
	}
	first := testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/node-summary", nil))
	require.Equal(t, 200, first.Code)
	require.Equal(t, "private, no-cache", first.Header().Get("Cache-Control"))
	require.NotEmpty(t, first.Header().Get("ETag"))
	var counts map[string]any
	testharness.DecodeJSON(t, first, &counts)
	require.Equal(t, float64(1), counts["shared_agents"])
	require.Equal(t, float64(1), counts["online_agents"])
	require.Equal(t, float64(1), counts["waiting_for_input"])
	conditional := testharness.JSONRequest(t, "GET", "/api/node-summary", nil)
	conditional.Header.Set("If-None-Match", first.Header().Get("ETag"))
	cached := testharness.Serve(h, conditional)
	require.Equal(t, http.StatusNotModified, cached.Code, cached.Body.String())
	require.Empty(t, cached.Body.String())
	// Revocation is checked before conditional responses, even on a warm cache.
	teamDB, err := db.GetAgentGroupByName("team")
	require.NoError(t, err)
	_, err = db.DeleteFederationPeerGrant(p.id.ID(), agentd.PermAgentsStatusRead, db.FederationGroupScope(teamDB.ID))
	require.NoError(t, err)
	after := testharness.Serve(h, conditional)
	require.Equal(t, 200, after.Code, after.Body.String())
	require.NotEqual(t, first.Header().Get("ETag"), after.Header().Get("ETag"))
	testharness.DecodeJSON(t, after, &counts)
	require.Equal(t, float64(0), counts["waiting_for_input"])
	grant(agentd.PermNodeRead, "")
	summary = read("/api/node-summary")
	require.Contains(t, summary, "resources")
	require.Contains(t, summary, "health")
	require.Nil(t, summary["resources"].(map[string]any)["agents"])
	localSummary := testharness.Serve(agentd.BuildDashboardHandlerForTest(), testharness.JSONRequest(t, "GET", "/api/node-summary", nil))
	require.Equal(t, 200, localSummary.Code, localSummary.Body.String())
	var localCounts map[string]any
	testharness.DecodeJSON(t, localSummary, &localCounts)
	require.Equal(t, float64(3), localCounts["shared_agents"])
	require.NotContains(t, localCounts, "peer_view")
}

func TestPeerSnapshotPollsShareGatherWithoutStalingLocalReads(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	f.HaveGroup("team")
	f.HaveConvWithTitle("poll-cache-agent", "worker")
	f.HaveMember("team", "poll-cache-agent")
	f.HaveAliveSession("poll-cache-agent", "poll-cache-session", "poll-cache-pane", f.TestCwd("work"))
	rec := fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermAgentsStatusRead, "scope": "group=team"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	agentd.ResetStatusSnapshotForTest()
	var gathers atomic.Int64
	t.Cleanup(agentd.SetPeerStatusGatherHookForTest(func() { gathers.Add(1) }))
	h := agentd.PeerViewHandler(p.id.ID())
	for i := 0; i < 10; i++ {
		seen, err := db.MarkFederationEnvelopeSeen(p.id.ID(), fmt.Sprintf("poll-%d", i), time.Now().Add(time.Hour))
		require.NoError(t, err)
		require.True(t, seen)
		rec = testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/snapshot", nil))
		require.Equal(t, 200, rec.Code, rec.Body.String())
		var snapshot dashSnapshot
		testharness.DecodeJSON(t, rec, &snapshot)
		require.Len(t, snapshot.Agents, 1)
	}
	require.Equal(t, int64(1), gathers.Load(), "ten peer polls share one gather despite persisted replay markers")
	seen, err := db.MarkFederationEnvelopeSeen(p.id.ID(), "poll-0", time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.False(t, seen, "replay remains refused")
	require.NoError(t, db.UpdateSessionModel("poll-cache-session", "changed-model"))
	f.SetSessionStatus("poll-cache-agent", "awaiting_input")
	dashboard := agentd.BuildDashboardHandlerForTest()
	summary := testharness.Serve(dashboard, testharness.JSONRequest(t, "GET", "/api/node-summary", nil))
	require.Equal(t, 200, summary.Code, summary.Body.String())
	var counts map[string]any
	testharness.DecodeJSON(t, summary, &counts)
	require.Equal(t, float64(1), counts["waiting_for_input"], "local node summary remains immediately fresh")
	local := fetchSnapshotOnly(t, dashboard)
	require.Equal(t, "changed-model", findDashMember(local, "team", "poll-cache-agent").State.Model)
	require.Equal(t, int64(1), gathers.Load(), "local reads do not refresh the peer cache")
	_, err = config.Update(func(c *config.Config, err error) error {
		if err != nil {
			return err
		}
		c.StatusSnapshot = &config.StatusSnapshotConfig{Disabled: true}
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, db.UpdateSessionModel("poll-cache-session", "uncached-model"))
	rec = testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/snapshot", nil))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "uncached-model", "global cache-off also disables peer coalescing")
	_, err = config.Update(func(c *config.Config, err error) error { c.StatusSnapshot = nil; return err })
	require.NoError(t, err)
	rec = fedHuman(t, f, "DELETE", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermAgentsStatusRead, "scope": "group=team"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	rec = testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/snapshot", nil))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var snapshot dashSnapshot
	testharness.DecodeJSON(t, rec, &snapshot)
	require.Empty(t, snapshot.Groups, "revocation is immediate even with warm private status data")
	require.Empty(t, snapshot.Agents)
}
