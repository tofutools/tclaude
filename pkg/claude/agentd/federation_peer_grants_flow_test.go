package agentd_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestFederation_PeerGrantScopes(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	f.HaveGroup("one")
	f.HaveGroup("two")
	grant := func(slug, scope string) {
		rec := fedHuman(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug, "scope": scope})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	// Trust alone and agent defaults confer no discovery authority.
	latest := func() proto.CatalogPayload {
		cats := p.envelopes(proto.KindCatalog)
		require.NotEmpty(t, cats)
		var cat proto.CatalogPayload
		require.NoError(t, cats[len(cats)-1].DecodePayload(&cat))
		return cat
	}
	fedEventually(t, "empty initial catalog", func() bool { return len(p.envelopes(proto.KindCatalog)) > 0 })
	require.Empty(t, latest().Groups)
	grant(agentd.PermGroupsRosterRead, "group=one")
	fedEventually(t, "scoped catalog", func() bool { cat := latest(); return len(cat.Groups) == 1 && cat.Groups[0].Name == "one" })
	grant(agentd.PermGroupsPresenceRead, "")
	fedEventually(t, "unscoped discovery", func() bool { return len(latest().Groups) == 2 })
	f.HaveGroup("future")
	// Changing any grant broadcasts all current groups, including new groups.
	grant(agentd.PermGroupsRosterRead, "group=one")
	fedEventually(t, "future group discovery", func() bool { return len(latest().Groups) == 3 })
	require.NoError(t, db.ArchiveAgentGroup("two"))
	grant(agentd.PermGroupsRosterRead, "group=one")
	fedEventually(t, "archived group hidden", func() bool { return len(latest().Groups) == 2 })
	rec := fedHuman(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": "permissions.grant", "scope": "group=one"})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermMessageDirect}), "some-agent"))
	require.Equal(t, http.StatusForbidden, rec.Code)
	rec = fedHuman(t, f, http.MethodPost, "/v1/federation/peers/untrust", map[string]any{"instance": "bob"})
	// Untrust endpoint takes the peer selector in its path.
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	grants, err := db.ListFederationPeerGrants(p.id.ID())
	require.NoError(t, err)
	require.Empty(t, grants)
}

func TestFederation_AutoSpawnGrantAndCap(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	f.HaveGroup("team")
	rec := fedHuman(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermGroupsMembersSpawn, "scope": "group=team", "spawn_policy": map[string]any{"max_live": 1}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	request := func(name string) *proto.Envelope {
		env := p.envelope(proto.KindSpawnReq, proto.Endpoint{}, proto.SpawnRequestPayload{Group: "team", Name: name, Brief: "review the change"})
		p.send(env)
		require.Equal(t, proto.AckAccepted, fedAckFor(t, p, env.ID).Status)
		return env
	}
	first := request("worker-one")
	var approved *db.FederationSpawnRequest
	fedEventually(t, "automatic approval", func() bool {
		rows, err := db.ListFederationSpawnRequests(100)
		if err != nil {
			return false
		}
		for _, row := range rows {
			if row.EnvelopeID == first.ID && row.Status == db.FedSpawnApproved {
				approved = row
				return true
			}
		}
		return false
	})
	require.NotEmpty(t, approved.ResultAgent)
	actor, err := db.GetAgent(approved.ResultAgent)
	require.NoError(t, err)
	f.AssertGroupMember("team", actor.CurrentConvID, "worker-one", 5*time.Second)
	workers, err := db.ListFederationAutoWorkers(p.id.ID())
	require.NoError(t, err)
	require.Equal(t, []string{approved.ResultAgent}, workers)
	second := request("worker-two")
	fedEventually(t, "cap notice", func() bool {
		for _, m := range fedInbox(t, f) {
			if m.Subject == fmt.Sprintf("remote spawn request #%d needs approval", approved.ID+1) {
				return true
			}
		}
		return false
	})
	rows, err := db.ListFederationSpawnRequests(100)
	require.NoError(t, err)
	for _, row := range rows {
		if row.EnvelopeID == second.ID {
			require.Equal(t, db.FedSpawnPending, row.Status)
		}
	}
	// A replay cannot launch an additional worker after the cap changes.
	p.send(first)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, p, first.ID).Status)
	workers, err = db.ListFederationAutoWorkers(p.id.ID())
	require.NoError(t, err)
	require.Len(t, workers, 1)
}

func TestFederation_AutoSpawnFailureStaysPending(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	f.HaveGroup("team")
	rec := fedHuman(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermGroupsMembersSpawn, "scope": "group=team", "spawn_policy": map[string]any{"profile": "does-not-exist", "max_live": 2}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	env := p.envelope(proto.KindSpawnReq, proto.Endpoint{}, proto.SpawnRequestPayload{Group: "team", Name: "helper", Brief: "run a review"})
	p.send(env)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, p, env.ID).Status)
	fedEventually(t, "failed automatic spawn notice", func() bool {
		for _, m := range fedInbox(t, f) {
			if m.Subject == "remote spawn request #1 needs approval" {
				return true
			}
		}
		return false
	})
	rows, err := db.ListFederationSpawnRequests(100)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, db.FedSpawnPending, rows[0].Status)
	workers, err := db.ListFederationAutoWorkers(p.id.ID())
	require.NoError(t, err)
	require.Empty(t, workers)
	// Receiver-owned profile settings are not sourced from the remote payload.
	rec = fedHuman(t, f, http.MethodPost, fmt.Sprintf("/v1/federation/spawn-requests/%d/approve", rows[0].ID), map[string]any{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestFederation_AutoSpawnRateLimit(t *testing.T) {
	oldMax := agentd.SpawnMaxPerWindow
	agentd.SpawnMaxPerWindow = 1
	t.Cleanup(func() { agentd.SpawnMaxPerWindow = oldMax })
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	f.HaveGroup("team")
	rec := fedHuman(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermGroupsMembersSpawn, "scope": "group=team", "spawn_policy": map[string]any{"max_live": 3}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	p.send(p.envelope(proto.KindSpawnReq, proto.Endpoint{}, proto.SpawnRequestPayload{Group: "team", Name: "first", Brief: "first review"}))
	fedEventually(t, "first auto spawn", func() bool {
		rows, _ := db.ListFederationSpawnRequests(100)
		return len(rows) == 1 && rows[0].Status == db.FedSpawnApproved
	})
	p.send(p.envelope(proto.KindSpawnReq, proto.Endpoint{}, proto.SpawnRequestPayload{Group: "team", Name: "second", Brief: "second review"}))
	fedEventually(t, "rate limited spawn notice", func() bool {
		for _, m := range fedInbox(t, f) {
			if m.Subject == "remote spawn request #2 needs approval" {
				return true
			}
		}
		return false
	})
	rows, err := db.ListFederationSpawnRequests(100)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	pending := 0
	for _, row := range rows {
		if row.Status == db.FedSpawnPending {
			pending++
		}
	}
	require.Equal(t, 1, pending)
	workers, err := db.ListFederationAutoWorkers(p.id.ID())
	require.NoError(t, err)
	require.Len(t, workers, 1)
}
