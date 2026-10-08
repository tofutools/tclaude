package agentd_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestFederation_SessionsExportAndTransitions(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	const alice = "fed-sessions-alice"
	f.HaveGroup("team")
	f.HaveGroup("private")
	for _, c := range []string{alice, "fed-sessions-hidden", "fed-sessions-offline"} {
		f.HaveConvWithTitle(c, c)
	}
	f.HaveMember("team", alice)
	f.HaveMember("team", "fed-sessions-offline")
	f.HaveMember("private", "fed-sessions-hidden")
	f.HaveAliveSession(alice, "sessions-alice", "tclaude-sessions-alice", f.TestCwd("work"))
	f.HaveAliveSession("fed-sessions-hidden", "sessions-hidden", "tclaude-sessions-hidden", f.TestCwd("private"))
	f.SetSessionStatus(alice, "working")
	aid, err := db.AgentIDForConv(alice)
	require.NoError(t, err)
	grant := func(slug string) {
		rec := fedHuman(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug, "scope": "group=team"})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	grant(agentd.PermGroupsRosterRead)
	latest := func() proto.CatalogPayload {
		var cat proto.CatalogPayload
		cats := p.envelopes(proto.KindCatalog)
		if len(cats) > 0 {
			require.NoError(t, cats[len(cats)-1].DecodePayload(&cat))
		}
		return cat
	}
	fedEventually(t, "roster without session data", func() bool { cat := latest(); return len(cat.Groups) == 1 })
	require.Empty(t, latest().Groups[0].Sessions)
	grant(agentd.PermSessionsRead)
	fedEventually(t, "session catalog", func() bool { cat := latest(); return len(cat.Groups) == 1 && len(cat.Groups[0].Sessions) == 1 })
	s := latest().Groups[0].Sessions[0]
	require.Equal(t, aid, s.Agent)
	require.Equal(t, "sessions-alice", s.Session)
	require.Equal(t, "working", s.State)
	require.Empty(t, s.WaitingReason)
	// The transition push uses the real timer: it arrives without a catalog request.
	f.SetSessionStatus(alice, "awaiting_permission")
	var waiting proto.CatalogSession
	fedEventually(t, "prompt transition push", func() bool {
		for _, e := range p.envelopes(proto.KindSessionsUpdate) {
			var u proto.SessionsUpdatePayload
			require.NoError(t, e.DecodePayload(&u))
			for _, g := range u.Groups {
				if g.Name == "team" && len(g.Sessions) == 1 && g.Sessions[0].State == "awaiting_permission" {
					waiting = g.Sessions[0]
					return true
				}
			}
		}
		return false
	})
	require.Equal(t, "permission", waiting.WaitingReason)
	require.NotNil(t, waiting.WaitingObservedSince)
	require.WithinDuration(t, time.Now(), *waiting.WaitingObservedSince, 10*time.Second)
	// Unrestricted trust includes the new slug and all live groups automatically.
	setFedTrustLevel(t, fh, "unrestricted")
	fedEventually(t, "unrestricted session catalogs", func() bool {
		cat := latest()
		if len(cat.Groups) != 2 {
			return false
		}
		for _, g := range cat.Groups {
			if !g.HasCap(proto.CapSessions) || len(g.Sessions) != 1 {
				return false
			}
		}
		return true
	})
	rec := fedHuman(t, f, http.MethodDelete, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermSessionsRead, "scope": "group=team"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	setFedTrustLevel(t, fh, "restricted")
	fedEventually(t, "revoked session data", func() bool {
		cat := latest()
		return len(cat.Groups) == 1 && !cat.Groups[0].HasCap(proto.CapSessions) && len(cat.Groups[0].Sessions) == 0
	})
}

func TestFederation_SessionsReadScopeUpdatesAndStaleness(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	const caller = "fed-sessions-reader"
	f.HaveConvWithTitle(caller, "reader")
	at := time.Now().UTC()
	since := at.Add(-time.Minute)
	row := proto.CatalogSession{Agent: "agt_remote00001", Session: "remote-runtime", Name: "remote", Harness: "codex", State: "working"}
	p.send(p.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{Groups: []proto.CatalogGroup{
		{Name: "builders", Caps: []string{proto.CapSessions}, Sessions: []proto.CatalogSession{row}, SessionsAt: at},
		{Name: "secret", Caps: []string{proto.CapSessions}, Sessions: []proto.CatalogSession{{Agent: "agt_hidden00001", Session: "hidden-runtime", Name: "hidden", State: "idle"}}, SessionsAt: at},
	}}))
	type view struct {
		proto.CatalogSession
		Address string   `json:"address"`
		Stale   bool     `json:"stale"`
		Groups  []string `json:"groups"`
	}
	list := func(human bool) []view {
		req := testharness.JSONRequest(t, http.MethodGet, "/v1/federation/sessions?peer=bob", nil)
		if human {
			req = agentd.AsHumanPeer(req)
		} else {
			req = agentd.AsAgentPeer(req, caller)
		}
		rec := testharness.Serve(f.Mux, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var rows []view
		testharness.DecodeJSON(t, rec, &rows)
		return rows
	}
	fedEventually(t, "remote sessions stored", func() bool { return len(list(true)) == 2 })
	require.Empty(t, list(false))
	require.NoError(t, db.GrantAgentPermission(caller, agentd.PermSessionsRead, "test"))
	require.Empty(t, list(false), "unscoped grants never authorize a restricted peer")
	require.NoError(t, db.GrantAgentPermissionWithScope(caller, agentd.PermSessionsRead, `{"peer":["`+p.id.ID()+`/builders"]}`, "test"))
	rows := list(false)
	require.Len(t, rows, 1)
	require.Equal(t, "agt_remote00001@bob", rows[0].Address)
	require.False(t, rows[0].Stale)
	row.State = "awaiting_input"
	row.WaitingReason = "question"
	row.WaitingObservedSince = &since
	update := func(at time.Time, s proto.CatalogSession) {
		p.send(p.envelope(proto.KindSessionsUpdate, proto.Endpoint{}, proto.SessionsUpdatePayload{Groups: []proto.SessionGroupUpdate{{Name: "builders", Sessions: []proto.CatalogSession{s}, At: at}}}))
	}
	raw, catalogAt, err := db.GetFederationCatalog(p.id.ID())
	require.NoError(t, err)
	require.NotEmpty(t, raw)
	update(at.Add(time.Second), row)
	fedEventually(t, "remote waiting update", func() bool { return list(false)[0].WaitingReason == "question" })
	_, after, err := db.GetFederationCatalog(p.id.ID())
	require.NoError(t, err)
	require.Equal(t, catalogAt, after, "state update does not refresh unrelated presence")
	row.State = "working"
	row.WaitingReason = ""
	row.WaitingObservedSince = nil
	update(at, row) // Delayed pushes cannot roll back a newer state.
	// A following catalog serializes processing of the preceding update and must
	// itself preserve the newer session snapshot.
	p.send(p.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{Groups: []proto.CatalogGroup{{Name: "builders", Caps: []string{proto.CapSessions}, Sessions: []proto.CatalogSession{row}, SessionsAt: at}}}))
	fedEventually(t, "delayed catalog processed", func() bool { return len(list(true)) == 1 })
	require.Equal(t, "question", list(false)[0].WaitingReason)
	fh.hub.Close()
	fedEventually(t, "session stale on disconnect", func() bool { return list(false)[0].Stale })
}
