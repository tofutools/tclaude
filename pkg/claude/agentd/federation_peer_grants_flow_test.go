package agentd_test

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
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

func TestFederation_AutoSpawnSlowLaunchFailure(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	f.HaveGroup("team")
	t.Cleanup(agentd.SetOpenCodeAsyncSpawnResponseGraceForTest(20 * time.Millisecond))
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	blockedOpenCodeRuntime(t, release, errors.New("runtime startup failed"))
	rec := fedHuman(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermGroupsMembersSpawn, "scope": "group=team", "spawn_policy": map[string]any{"harness": "opencode", "max_live": 1}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	env := p.envelope(proto.KindSpawnReq, proto.Endpoint{}, proto.SpawnRequestPayload{Group: "team", Name: "slow-failure", Brief: "review changes"})
	p.send(env)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, p, env.ID).Status)
	// The inbound envelope is acknowledged while the launch still runs; this
	// acknowledgement must not be confused with a completed spawn approval.
	time.Sleep(100 * time.Millisecond)
	for _, e := range p.envelopes(proto.KindSpawnRes) {
		require.NotEqual(t, env.ID, e.InReplyTo, "no successful result before enrollment")
	}
	once.Do(func() { close(release) })
	fedEventually(t, "slow launch failure notice", func() bool {
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
	require.Empty(t, f.ListGroupMembers("team"))
}

func TestFederation_AutoSpawnSlowLaunchOccupiesCap(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	f.HaveGroup("team")
	t.Cleanup(agentd.SetOpenCodeAsyncSpawnResponseGraceForTest(20 * time.Millisecond))
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	blockedOpenCodeRuntime(t, release, nil)
	rec := fedHuman(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermGroupsMembersSpawn, "scope": "group=team", "spawn_policy": map[string]any{"harness": "opencode", "max_live": 1}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	for _, name := range []string{"slow-one", "slow-two"} {
		env := p.envelope(proto.KindSpawnReq, proto.Endpoint{}, proto.SpawnRequestPayload{Group: "team", Name: name, Brief: "review changes"})
		p.send(env)
		require.Equal(t, proto.AckAccepted, fedAckFor(t, p, env.ID).Status)
	}
	time.Sleep(100 * time.Millisecond)
	once.Do(func() { close(release) })
	fedEventually(t, "one slow worker and cap failure", func() bool {
		rows, _ := db.ListFederationSpawnRequests(100)
		approved, pending := 0, 0
		for _, row := range rows {
			if row.Status == db.FedSpawnApproved {
				approved++
			}
			if row.Status == db.FedSpawnPending {
				pending++
			}
		}
		return approved == 1 && pending == 1 && len(f.ListGroupMembers("team")) == 1
	})
}

func TestFederation_UnconfirmedLaunchRequiresAbandon(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	f.HaveGroup("team")
	t.Cleanup(agentd.SetOpenCodeAsyncSpawnResponseGraceForTest(20 * time.Millisecond))
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	blockedOpenCodeRuntime(t, release, errors.New("abandoned startup failed"))
	rec := fedHuman(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermGroupsMembersSpawn, "scope": "group=team", "spawn_policy": map[string]any{"harness": "opencode", "max_live": 1}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	env := p.envelope(proto.KindSpawnReq, proto.Endpoint{}, proto.SpawnRequestPayload{Group: "team", Name: "unconfirmed", Brief: "review changes"})
	p.send(env)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, p, env.ID).Status)
	var req *db.FederationSpawnRequest
	fedEventually(t, "durable launching identity", func() bool {
		rows, _ := db.ListFederationSpawnRequests(100)
		if len(rows) != 1 {
			return false
		}
		req = rows[0]
		return req.Status == db.FedSpawnLaunching && req.ResultAgent != "" && req.LaunchLabel != ""
	})
	d, err := db.Open()
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE federation_spawn_requests SET launch_started_at=? WHERE id=?`, time.Now().Add(-time.Minute).UnixNano(), req.ID)
	require.NoError(t, err)
	// Startup and periodic paths invoke this same durable reconciler.
	agentd.ReconcileFederationSpawnsForTest()
	persisted, err := db.GetFederationSpawnRequest(req.ID)
	require.NoError(t, err)
	require.Equal(t, db.FedSpawnLaunching, persisted.Status)
	notice := false
	for _, m := range fedInbox(t, f) {
		if m.Subject == fmt.Sprintf("remote spawn request #%d still launching", req.ID) {
			notice = true
		}
	}
	require.True(t, notice)
	approve := fmt.Sprintf("/v1/federation/spawn-requests/%d/approve", req.ID)
	rec = fedHuman(t, f, http.MethodPost, approve, map[string]any{})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	abandon := fmt.Sprintf("/v1/federation/spawn-requests/%d/abandon", req.ID)
	rec = fedHuman(t, f, http.MethodPost, abandon, map[string]any{})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	rec = fedHuman(t, f, http.MethodPost, abandon, map[string]any{"acknowledge_late_worker": true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "late worker")
	persisted, err = db.GetFederationSpawnRequest(req.ID)
	require.NoError(t, err)
	require.Equal(t, db.FedSpawnPending, persisted.Status)
	workers, err := db.ListFederationAutoWorkers(p.id.ID())
	require.NoError(t, err)
	require.Empty(t, workers)
	once.Do(func() { close(release) })
	agentd.WaitForBackgroundForTest()
}
