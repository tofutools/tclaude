package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/groupexport"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestFederation_AgentStatusExportTransitionsAndSecrecy(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	const alice = "status-alice"
	f.HaveGroup("team")
	f.HaveGroup("private")
	f.HaveConvWithTitle(alice, "alice")
	f.HaveMember("team", alice)
	f.HaveAliveSession(alice, "status-alice-session", "tclaude-status-alice", f.TestCwd("secret-work"))
	f.SetSessionStatus(alice, "working")
	f.HaveConvWithTitle("hidden-agent", "hidden")
	f.HaveMember("private", "hidden-agent")
	aid, err := db.AgentIDForConv(alice)
	require.NoError(t, err)
	require.NoError(t, db.UpdateSessionModel("status-alice-session", "model-one"))
	require.NoError(t, db.UpdateSessionEffort("status-alice-session", "high"))
	require.NoError(t, db.UpdateContextSnapshot("status-alice-session", 25, 100, 20, 1000))
	_, err = db.SetAgentTaskRef(aid, "https://tracker.test/issues/1?token=do-not-share#private", "work item")
	require.NoError(t, err)
	grant := func(slug string) {
		r := fedHuman(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug, "scope": "group=team"})
		require.Equal(t, 200, r.Code, r.Body.String())
	}
	latest := func() proto.CatalogPayload {
		var c proto.CatalogPayload
		es := p.envelopes(proto.KindCatalog)
		if len(es) > 0 {
			require.NoError(t, es[len(es)-1].DecodePayload(&c))
		}
		return c
	}
	grant(agentd.PermGroupsRosterRead)
	fedEventually(t, "roster without status", func() bool { return len(latest().Groups) == 1 })
	require.Empty(t, latest().Groups[0].AgentStatuses)
	grant(agentd.PermAgentsStatusRead)
	fedEventually(t, "authorized status catalog", func() bool { return len(latest().Groups) == 1 && len(latest().Groups[0].AgentStatuses) == 1 })
	c := latest()
	row := c.Groups[0].AgentStatuses[0]
	require.Equal(t, aid, row.Agent)
	require.Equal(t, "model-one", row.Model)
	require.Equal(t, "high", row.Effort)
	require.NotNil(t, row.Context)
	require.Equal(t, "https://tracker.test/issues/1", row.TaskURL)
	raw, err := json.Marshal(c)
	require.NoError(t, err)
	for _, secret := range []string{config.DataDir(), "secret-work", "do-not-share", "hidden-agent", "status-alice-session", `"cwd"`, `"status_detail"`, `"effective"`, `"cost_usd"`} {
		require.NotContains(t, string(raw), secret)
	}
	f.SetSessionStatus(alice, "awaiting_input")
	fedEventually(t, "status transition through existing observer", func() bool {
		for _, e := range p.envelopes(proto.KindAgentStatusUpdate) {
			var u proto.AgentStatusUpdatePayload
			require.NoError(t, e.DecodePayload(&u))
			for _, g := range u.Groups {
				if g.Name == "team" && len(g.Statuses) == 1 && g.Statuses[0].WaitingReason == "question" {
					return true
				}
			}
		}
		return false
	})
	r := fedHuman(t, f, http.MethodDelete, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermAgentsStatusRead, "scope": "group=team"})
	require.Equal(t, 200, r.Code)
	fedEventually(t, "status revoked independently", func() bool {
		return len(latest().Groups) == 1 && !latest().Groups[0].HasCap(proto.CapAgentStatus) && len(latest().Groups[0].AgentStatuses) == 0
	})
	require.NotEmpty(t, latest().Groups[0].Members)
	setFedTrustLevel(t, fh, "unrestricted")
	fedEventually(t, "implicit unrestricted status", func() bool {
		for _, g := range latest().Groups {
			if g.Name == "team" && len(g.AgentStatuses) == 1 {
				return true
			}
		}
		return false
	})
}
func TestFederation_AgentStatusReadScopesOrderingAndStaleness(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	const caller = "status-reader"
	f.HaveConvWithTitle(caller, "reader")
	type view struct {
		Agent       string             `json:"agent"`
		RemoteGroup string             `json:"remote_group"`
		State       *proto.AgentStatus `json:"state"`
		StatusStale bool               `json:"status_stale"`
		IdleSeconds *int64             `json:"idle_seconds"`
	}
	list := func(human bool) []view {
		req := testharness.JSONRequest(t, http.MethodGet, "/v1/federation/reachable", nil)
		if human {
			req = agentd.AsHumanPeer(req)
		} else {
			req = agentd.AsAgentPeer(req, caller)
		}
		r := testharness.Serve(f.Mux, req)
		require.Equal(t, 200, r.Code, r.Body.String())
		var rows []view
		testharness.DecodeJSON(t, r, &rows)
		return rows
	}
	at := time.Now().UTC()
	activity := at.Add(-time.Minute)
	status := proto.AgentStatus{Agent: "agt_remote", Name: "remote", Status: "idle", Online: true, Harness: "codex", Model: "model", LastActivity: &activity, TaskURL: "https://tracker.test/issue?secret=x"}
	p.send(p.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{Groups: []proto.CatalogGroup{{Name: "team", Caps: []string{proto.CapAgentStatus}, AgentStatuses: []proto.AgentStatus{status}, AgentStatusesAt: at, AgentStatusesUpdatedAt: at}, {Name: "private", Caps: []string{proto.CapAgentStatus}, AgentStatuses: []proto.AgentStatus{{Agent: "agt_hidden", Name: "hidden"}}, AgentStatusesAt: at}}}))
	fedEventually(t, "remote status catalog stored", func() bool { return len(list(true)) == 2 })
	require.Empty(t, list(false))
	require.NoError(t, db.GrantAgentPermissionWithScope(caller, agentd.PermSessionsRead, `{"peer":["`+p.id.ID()+`/team"]}`, "test"))
	require.Empty(t, list(false), "sessions read does not grant status")
	require.NoError(t, db.GrantAgentPermissionWithScope(caller, agentd.PermAgentsStatusRead, `{"peer":["`+p.id.ID()+`/team"]}`, "test"))
	rows := list(false)
	require.Len(t, rows, 1)
	require.Equal(t, "team", rows[0].RemoteGroup)
	require.False(t, rows[0].StatusStale)
	require.NotNil(t, rows[0].IdleSeconds)
	require.GreaterOrEqual(t, *rows[0].IdleSeconds, int64(60))
	require.Equal(t, "https://tracker.test/issue", rows[0].State.TaskURL)
	_, catalogAt, err := db.GetFederationCatalog(p.id.ID())
	require.NoError(t, err)
	status.Model = "new-model"
	pub := at.Add(100 * time.Millisecond)
	p.send(p.envelope(proto.KindAgentStatusUpdate, proto.Endpoint{}, proto.AgentStatusUpdatePayload{Groups: []proto.AgentStatusGroupUpdate{{Name: "team", Statuses: []proto.AgentStatus{status}, At: at, PublishedAt: pub}}}))
	fedEventually(t, "same-observation newer publication accepted", func() bool { return list(false)[0].State.Model == "new-model" })
	_, after, err := db.GetFederationCatalog(p.id.ID())
	require.NoError(t, err)
	require.Equal(t, catalogAt, after, "status push does not refresh roster/presence")
	status.Model = "old-model"
	p.send(p.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{Groups: []proto.CatalogGroup{{Name: "team", Caps: []string{proto.CapAgentStatus}, AgentStatuses: []proto.AgentStatus{status}, AgentStatusesAt: at, AgentStatusesUpdatedAt: at}}}))
	fedEventually(t, "delayed full catalog processed", func() bool { return len(list(true)) == 1 })
	require.Equal(t, "new-model", list(false)[0].State.Model)
	fh.hub.Close()
	fedEventually(t, "status stale on disconnect", func() bool { return list(false)[0].StatusStale })
	require.Equal(t, int64(60), *list(false)[0].IdleSeconds, "stale idle age freezes at observation")
}

func TestFederation_AgentStatusExplicitNameAndCoarseExit(t *testing.T) {
	fh := newFedHarness(t)
	f := fh.f
	const conv = "status-unnamed"
	f.HaveGroup("team")
	f.HaveConvWithPrompt(conv, "private prompt content")
	f.HaveMember("team", conv)
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{ConvID: conv, Summary: "private summary content", FirstPrompt: "private prompt content"}))
	f.HaveAliveSession(conv, "status-unnamed-session", "tclaude-unnamed", f.TestCwd("unnamed"))
	f.MarkOffline("tclaude-unnamed")
	f.SetSessionStatus(conv, "exited")
	require.NoError(t, db.SetSessionExitReason("status-unnamed-session", "resource_limit_oom"))
	aid, err := db.AgentIDForConv(conv)
	require.NoError(t, err)
	g, err := db.GetAgentGroupByName("team")
	require.NoError(t, err)
	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: fh.peer.id.ID(), Slug: agentd.PermAgentsStatusRead, Scope: db.FederationGroupScope(g.ID)}))
	agentd.ResetStatusSnapshotForTest()
	c, err := agentd.FederationCatalogForStatusTest(fh.peer.id.ID())
	require.NoError(t, err)
	require.Len(t, c.Groups, 1)
	require.Len(t, c.Groups[0].AgentStatuses, 1)
	row := c.Groups[0].AgentStatuses[0]
	require.Equal(t, aid, row.Name)
	require.Equal(t, "crashed", row.ExitReason)
	raw, err := json.Marshal(c)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "private prompt")
	require.NotContains(t, string(raw), "private summary")
}

func TestFederation_AgentStatusWarmCacheIncludesTransactionalImport(t *testing.T) {
	fh := newFedHarness(t)
	f := fh.f
	f.HaveGroup("existing")
	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: fh.peer.id.ID(), Slug: agentd.PermAgentsStatusRead}))
	before, err := agentd.FederationCatalogForStatusTest(fh.peer.id.ID())
	require.NoError(t, err)
	require.Len(t, before.Groups, 1)
	const conv = "imported-status-member"
	_, err = db.ImportGroup(db.GroupImportPlan{
		Export:     &groupexport.Export{FormatVersion: groupexport.FormatVersion, SourceGroup: "imported", Group: groupexport.Group{}, Members: []groupexport.Member{{ConvID: conv, Role: "builder"}}},
		TargetName: "imported", TargetCwd: f.TestCwd("imported"), ConvRemap: map[string]string{conv: conv},
	})
	require.NoError(t, err)
	after, err := agentd.FederationCatalogForStatusTest(fh.peer.id.ID())
	require.NoError(t, err)
	for _, g := range after.Groups {
		if g.Name == "imported" {
			require.Len(t, g.AgentStatuses, 1)
			require.Equal(t, "builder", g.AgentStatuses[0].Role)
			return
		}
	}
	t.Fatal("committed imported group missing from refreshed status catalog")
}
