package agentd_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/client"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func addPlacementPeer(t *testing.T, fh *fedHarness, label string) *fedPeer {
	t.Helper()
	id, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, fh.store.Admit(id.ID()))
	p := &fedPeer{t: t, id: id, agentdID: fh.peer.agentdID}
	cl, err := client.New(client.Options{URL: fh.url, Identity: id, Name: label, MaxBackoff: 200 * time.Millisecond, OnDeliver: func(from string, s *proto.Sealed) {
		key, ok := p.cl.LookupKey(from)
		if !ok {
			return
		}
		env, err := proto.Open(s, ed25519.PublicKey(key), id, time.Now())
		if err != nil {
			return
		}
		p.mu.Lock()
		p.got = append(p.got, env)
		p.mu.Unlock()
	}})
	require.NoError(t, err)
	p.cl = cl
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { cl.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	fedEventually(t, "additional placement peer online", func() bool {
		for _, row := range fedStatus(t, fh.f).Peers {
			if row.InstanceID == id.ID() && row.Online {
				return true
			}
		}
		return false
	})
	fedEventually(t, "additional peer directory", func() bool { _, ok := cl.LookupKey(p.agentdID); return ok })
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/peers/trust", map[string]any{"instance": id.ID(), "label": label})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	return p
}
func placementNode(load float64) *proto.NodeMetadata {
	at := time.Now().UTC()
	loads := [3]float64{load, load, load}
	return &proto.NodeMetadata{Schema: 1, SpawnPlacementVersion: 1, OS: "darwin", Arch: "arm64", Labels: []string{"test-rig"}, Harnesses: []proto.NodeHarness{{Name: "codex"}}, MaxLiveAgents: 4, Resources: proto.NodeResources{ObservedAt: &at, Status: "current", CPU: proto.NodeCPU{LogicalCores: 4, LoadAverage: &loads}, RAM: &proto.NodeRAM{TotalBytes: 8 << 30, AvailableBytes: 4 << 30}, Agents: &proto.NodeAgents{}}}
}
func publishPlacementNode(t *testing.T, p *fedPeer, n *proto.NodeMetadata, groups ...string) {
	t.Helper()
	gs := []proto.CatalogGroup{}
	for _, name := range groups {
		gs = append(gs, proto.CatalogGroup{Name: name, Caps: []string{proto.CapSpawn}})
	}
	at := time.Now().UTC()
	p.send(p.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{Node: n, NodeAt: at, Groups: gs}))
	fedEventually(t, "placement node publication", func() bool {
		raw, _, err := db.GetFederationCatalog(p.id.ID())
		if err != nil {
			return false
		}
		var c proto.CatalogPayload
		_ = json.Unmarshal([]byte(raw), &c)
		return c.Node != nil && c.NodeAt.Equal(at)
	})
}
func replyPlacementRequests(t *testing.T, p *fedPeer, code string, before func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		seen := map[string]bool{}
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for _, e := range p.envelopes(proto.KindSpawnReq) {
					if seen[e.ID] {
						continue
					}
					seen[e.ID] = true
					if before != nil {
						before()
					}
					ack := proto.AckPayload{Status: proto.AckAccepted}
					if code != "" {
						ack.Status = proto.AckRefused
						ack.Code = code
						ack.Reason = "scripted terminal refusal"
					}
					response := p.envelope(proto.KindAck, proto.Endpoint{}, ack)
					response.InReplyTo = e.ID
					p.send(response)
				}
			}
		}
	}()
	t.Cleanup(func() { cancel(); <-done })
}

type placementResult struct {
	State     string `json:"state"`
	To        string `json:"to"`
	Placement struct {
		Selected   string `json:"selected"`
		Candidates []struct {
			Instance string `json:"instance"`
			Eligible bool   `json:"eligible"`
			Reason   string `json:"reason"`
			Attempt  string `json:"attempt"`
		} `json:"candidates"`
	} `json:"placement"`
}

func TestFederation_PlacementRanksScopesAndPool(t *testing.T) {
	fh := newFedHarness(t)
	p := fh.peer
	q := addPlacementPeer(t, fh, "carol")
	publishPlacementNode(t, p, placementNode(8), "builders")
	publishPlacementNode(t, q, placementNode(1), "builders")
	replyPlacementRequests(t, p, "", nil)
	replyPlacementRequests(t, q, "", nil)
	send := func(caller string, body map[string]any) placementResult {
		req := testharness.JSONRequest(t, http.MethodPost, "/v1/federation/spawn-requests", body)
		if caller == "" {
			req = agentd.AsHumanPeer(req)
		} else {
			req = agentd.AsAgentPeer(req, caller)
		}
		rec := testharness.Serve(fh.f.Mux, req)
		require.Equal(t, 200, rec.Code, rec.Body.String())
		var out placementResult
		testharness.DecodeJSON(t, rec, &out)
		return out
	}
	req := map[string]any{"node": "auto", "brief": "review", "require": "os=darwin,harness=codex,label=test-rig"}
	out := send("", req)
	require.Equal(t, q.id.ID(), out.Placement.Selected)
	require.Len(t, out.Placement.Candidates, 2)
	var wire proto.SpawnRequestPayload
	require.NoError(t, q.envelopes(proto.KindSpawnReq)[0].DecodePayload(&wire))
	require.Equal(t, 1, wire.PlacementVersion)
	require.Equal(t, req["require"], wire.Require)
	const caller = "placement-agent"
	fh.f.HaveConvWithTitle(caller, "placer")
	require.NoError(t, db.GrantAgentPermissionWithScope(caller, agentd.PermNodeRead, `{"peer":["`+p.id.ID()+`"]}`, "test"))
	require.NoError(t, db.GrantAgentPermissionWithScope(caller, agentd.PermGroupsMembersSpawn, `{"peer":["`+p.id.ID()+`/builders"]}`, "test"))
	out = send(caller, req)
	require.Equal(t, p.id.ID(), out.Placement.Selected)
	require.Len(t, out.Placement.Candidates, 1, "explanation does not reveal metadata beyond caller node.read")
	pool, err := db.CreateFederationNodeGroup("rigs")
	require.NoError(t, err)
	require.NoError(t, db.AddFederationNodeGroupPeer(pool.ID, p.id.ID()))
	req["node"] = "group:rigs"
	out = send("", req)
	require.Equal(t, p.id.ID(), out.Placement.Selected)
	req["node"] = "auto"
	req["prefer"] = "most-free-ram"
	n := placementNode(8)
	n.Resources.RAM.AvailableBytes = 7 << 30
	publishPlacementNode(t, p, n, "builders")
	out = send("", req)
	require.Equal(t, p.id.ID(), out.Placement.Selected)
}

func TestFederation_PlacementBusyOnlyFallback(t *testing.T) {
	for _, code := range []string{"node_busy", "denied", ""} {
		t.Run(fmt.Sprintf("code-%s", code), func(t *testing.T) {
			fh := newFedHarness(t)
			p := fh.peer
			q := addPlacementPeer(t, fh, "carol")
			publishPlacementNode(t, p, placementNode(0), "builders")
			publishPlacementNode(t, q, placementNode(4), "builders")
			if code != "" {
				replyPlacementRequests(t, p, code, nil)
			}
			replyPlacementRequests(t, q, "", nil)
			rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/spawn-requests", map[string]any{"node": "auto", "brief": "work"})
			require.Equal(t, 200, rec.Code, rec.Body.String())
			var out placementResult
			testharness.DecodeJSON(t, rec, &out)
			if code == "node_busy" {
				require.Equal(t, q.id.ID(), out.Placement.Selected)
				require.Len(t, q.envelopes(proto.KindSpawnReq), 1)
			} else {
				require.Equal(t, p.id.ID(), out.Placement.Selected)
				require.Empty(t, q.envelopes(proto.KindSpawnReq), "uncertainty and other refusals stop placement")
			}
		})
	}
}

func TestFederation_PlacementRejectsStaleMissingAndAmbiguous(t *testing.T) {
	cases := []struct {
		name   string
		change func(*proto.NodeMetadata)
		groups []string
		why    string
	}{
		{"stale", func(n *proto.NodeMetadata) { at := time.Now().Add(-2 * time.Minute); n.Resources.ObservedAt = &at }, []string{"builders"}, "stale"},
		{"missing-load", func(n *proto.NodeMetadata) { n.Resources.CPU.LoadAverage = nil }, []string{"builders"}, "CPU load unavailable"},
		{"old-daemon", func(n *proto.NodeMetadata) { n.SpawnPlacementVersion = 0 }, []string{"builders"}, "support"},
		{"ambiguous", func(n *proto.NodeMetadata) {}, []string{"builders", "testers"}, "specify --group"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fh := newFedHarness(t)
			n := placementNode(0)
			tc.change(n)
			publishPlacementNode(t, fh.peer, n, tc.groups...)
			rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/spawn-requests", map[string]any{"node": "auto", "brief": "work"})
			require.Equal(t, 409, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), tc.why)
			require.Empty(t, fh.peer.envelopes(proto.KindSpawnReq))
		})
	}
}

func TestFederation_NodeCapacityCountsPendingAndDuplicate(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	q := addPlacementPeer(t, fh, "carol")
	f.HaveGroup("team")
	g, err := db.GetAgentGroupByName("team")
	require.NoError(t, err)
	for _, peer := range []*fedPeer{p, q} {
		require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: peer.id.ID(), Slug: agentd.PermGroupsRosterRead, Scope: db.FederationGroupScope(g.ID)}))
	}
	_, err = config.Update(func(c *config.Config, e error) error {
		if e != nil {
			return e
		}
		c.Federation.MaxLiveAgents = 1
		return nil
	})
	require.NoError(t, err)
	a := p.envelope(proto.KindSpawnReq, proto.Endpoint{}, proto.SpawnRequestPayload{Group: "team", Brief: "first"})
	b := q.envelope(proto.KindSpawnReq, proto.Endpoint{}, proto.SpawnRequestPayload{Group: "team", Brief: "second"})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); p.send(a) }()
	go func() { defer wg.Done(); q.send(b) }()
	wg.Wait()
	ackA, ackB := fedAckFor(t, p, a.ID), fedAckFor(t, q, b.ID)
	require.NotEqual(t, ackA.Status, ackB.Status)
	rows, err := db.ListFederationSpawnRequests(100)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	winner, env := p, a
	if ackB.Status == proto.AckAccepted {
		winner, env = q, b
		require.Equal(t, "node_busy", ackA.Code)
	} else {
		require.Equal(t, "node_busy", ackB.Code)
	}
	before := len(winner.envelopes(proto.KindAck))
	winner.send(env)
	fedEventually(t, "duplicate capacity acknowledgement", func() bool { return len(winner.envelopes(proto.KindAck)) > before })
	acks := winner.envelopes(proto.KindAck)
	var duplicate proto.AckPayload
	require.NoError(t, acks[len(acks)-1].DecodePayload(&duplicate))
	require.Equal(t, proto.AckAccepted, duplicate.Status)
	rows, err = db.ListFederationSpawnRequests(100)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	localCwd := f.TestCwd("local")
	require.NoError(t, os.MkdirAll(localCwd, 0700))
	local := f.AsHuman().SpawnWith("team", map[string]any{"name": "local-blocked", "cwd": localCwd, "harness": "claude"})
	require.Equal(t, 409, local.Code, local.Raw)
	require.Contains(t, string(local.Raw), "node_busy", "local managed launches share remote reservations")
	// Releasing the winning reservation must not make the terminally busy
	// envelope admissible: the sender may already have chosen another machine.
	rec := fedHuman(t, f, http.MethodPost, fmt.Sprintf("/v1/federation/spawn-requests/%d/deny", rows[0].ID), map[string]any{"reason": "release capacity"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	loser, rejected := q, b
	if winner == q {
		loser, rejected = p, a
	}
	before = len(loser.envelopes(proto.KindAck))
	loser.send(rejected)
	fedEventually(t, "terminal busy replay", func() bool { return len(loser.envelopes(proto.KindAck)) > before })
	acks = loser.envelopes(proto.KindAck)
	require.NoError(t, acks[len(acks)-1].DecodePayload(&duplicate))
	require.Equal(t, proto.AckRefused, duplicate.Status)
	require.Equal(t, "node_busy", duplicate.Code)
	rows, err = db.ListFederationSpawnRequests(100)
	require.NoError(t, err)
	require.Len(t, rows, 1)

}

func TestFederation_PlacementReceiverChecksRequirements(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	f.HaveGroup("team")
	g, err := db.GetAgentGroupByName("team")
	require.NoError(t, err)
	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: p.id.ID(), Slug: agentd.PermGroupsRosterRead, Scope: db.FederationGroupScope(g.ID)}))
	wrong := "linux"
	if runtime.GOOS == "linux" {
		wrong = "darwin"
	}
	e := p.envelope(proto.KindSpawnReq, proto.Endpoint{}, proto.SpawnRequestPayload{Group: "team", Brief: "work", PlacementVersion: 1, Require: "os=" + wrong})
	p.send(e)
	ack := fedAckFor(t, p, e.ID)
	require.Equal(t, proto.AckRefused, ack.Status)
	require.Equal(t, "node_incompatible", ack.Code)
	rows, err := db.ListFederationSpawnRequests(100)
	require.NoError(t, err)
	require.Empty(t, rows)
	e = p.envelope(proto.KindSpawnReq, proto.Endpoint{}, proto.SpawnRequestPayload{Group: "team", Brief: "work", PlacementVersion: 1, Require: "os=" + runtime.GOOS})
	p.send(e)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, p, e.ID).Status)
	rows, err = db.ListFederationSpawnRequests(100)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "os="+runtime.GOOS, rows[0].Requirements)
}

func TestFederation_PlacementRechecksPoolIdentityAfterBusy(t *testing.T) {
	fh := newFedHarness(t)
	p := fh.peer
	q := addPlacementPeer(t, fh, "carol")
	publishPlacementNode(t, p, placementNode(0), "builders")
	publishPlacementNode(t, q, placementNode(4), "builders")
	pool, err := db.CreateFederationNodeGroup("rigs")
	require.NoError(t, err)
	require.NoError(t, db.AddFederationNodeGroupPeer(pool.ID, p.id.ID()))
	require.NoError(t, db.AddFederationNodeGroupPeer(pool.ID, q.id.ID()))
	replyPlacementRequests(t, p, "node_busy", func() {
		require.NoError(t, db.DeleteFederationNodeGroup(pool.ID))
		replacement, err := db.CreateFederationNodeGroup("rigs")
		require.NoError(t, err)
		require.NoError(t, db.AddFederationNodeGroupPeer(replacement.ID, q.id.ID()))
	})
	replyPlacementRequests(t, q, "", nil)
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/spawn-requests", map[string]any{"node": "group:rigs", "brief": "work"})
	require.Equal(t, 409, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "no longer in selected pool")
	require.Empty(t, q.envelopes(proto.KindSpawnReq))
}

func TestFederation_PlacementApprovalUsesReceiverHarness(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	fakeBin := f.TestCwd("fake-bin")
	require.NoError(t, os.MkdirAll(fakeBin, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(fakeBin, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0700))
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	f.HaveGroup("team")
	g, err := db.GetAgentGroupByName("team")
	require.NoError(t, err)
	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: p.id.ID(), Slug: agentd.PermGroupsRosterRead, Scope: db.FederationGroupScope(g.ID)}))
	_, err = config.Update(func(c *config.Config, e error) error {
		if e != nil {
			return e
		}
		c.Federation.MaxLiveAgents = 1
		return nil
	})
	require.NoError(t, err)
	e := p.envelope(proto.KindSpawnReq, proto.Endpoint{}, proto.SpawnRequestPayload{Group: "team", Brief: "work", PlacementVersion: 1, Require: "harness=claude"})
	p.send(e)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, p, e.ID).Status)
	rows, err := db.ListFederationSpawnRequests(100)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	path := fmt.Sprintf("/v1/federation/spawn-requests/%d/approve", rows[0].ID)
	rec := fedHuman(t, f, http.MethodPost, path, map[string]any{"harness": "codex", "cwd": fakeBin})
	require.Equal(t, 409, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "node_incompatible")
	row, err := db.GetFederationSpawnRequest(rows[0].ID)
	require.NoError(t, err)
	require.Equal(t, db.FedSpawnPending, row.Status)
	rec = fedHuman(t, f, http.MethodPost, path, map[string]any{"harness": "claude", "cwd": fakeBin})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	fedEventually(t, "compatible receiver harness starts", func() bool {
		row, _ := db.GetFederationSpawnRequest(rows[0].ID)
		return row != nil && row.Status == db.FedSpawnApproved
	})
}

func TestFederation_PlacementRejectsIncompatibleAutoPolicy(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	fakeBin := f.TestCwd("fake-bin")
	require.NoError(t, os.MkdirAll(fakeBin, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(fakeBin, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0700))
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	f.HaveGroup("team")
	g, err := db.GetAgentGroupByName("team")
	require.NoError(t, err)
	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: p.id.ID(), Slug: agentd.PermGroupsMembersSpawn, Scope: db.FederationGroupScope(g.ID), SpawnPolicy: db.FederationSpawnPolicy{Harness: "codex", MaxLive: 1}}))
	e := p.envelope(proto.KindSpawnReq, proto.Endpoint{}, proto.SpawnRequestPayload{Group: "team", Brief: "work", PlacementVersion: 1, Require: "harness=claude"})
	p.send(e)
	ack := fedAckFor(t, p, e.ID)
	require.Equal(t, proto.AckRefused, ack.Status)
	require.Equal(t, "node_incompatible", ack.Code)
	rows, err := db.ListFederationSpawnRequests(100)
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestFederation_PlacementRechecksWithdrawnAuthority(t *testing.T) {
	fh := newFedHarness(t)
	p := fh.peer
	q := addPlacementPeer(t, fh, "carol")
	publishPlacementNode(t, p, placementNode(0), "builders")
	publishPlacementNode(t, q, placementNode(4), "builders")
	const caller = "placement-revoked"
	fh.f.HaveConvWithTitle(caller, "placer")
	require.NoError(t, db.GrantAgentPermissionWithScope(caller, agentd.PermNodeRead, `{"peer":["`+p.id.ID()+`","`+q.id.ID()+`"]}`, "test"))
	require.NoError(t, db.GrantAgentPermissionWithScope(caller, agentd.PermGroupsMembersSpawn, `{"peer":["`+p.id.ID()+`/builders","`+q.id.ID()+`/builders"]}`, "test"))
	replyPlacementRequests(t, p, "node_busy", func() {
		require.NoError(t, db.SetAgentPermissionOverride(caller, agentd.PermNodeRead, db.PermEffectDeny, "test"))
	})
	replyPlacementRequests(t, q, "", nil)
	req := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/federation/spawn-requests", map[string]any{"node": "auto", "brief": "work"}), caller)
	rec := testharness.Serve(fh.f.Mux, req)
	require.Equal(t, 409, rec.Code, rec.Body.String())
	var out placementResult
	testharness.DecodeJSON(t, rec, &out)
	require.Empty(t, out.Placement.Candidates, "withdrawn metadata must not appear in the final explanation")
	require.Empty(t, q.envelopes(proto.KindSpawnReq), "withdrawn authority prevents another send")
}

func TestFederation_NodeCapacityKeepsUnconfirmedLocalLaunch(t *testing.T) {
	fh := newFedHarness(t)
	f := fh.f
	f.HaveGroup("team")
	t.Cleanup(agentd.SetAsyncSpawnInlineGraceForTest(50 * time.Millisecond))
	f.World.SkipSpawnRow = true
	_, err := config.Update(func(c *config.Config, e error) error {
		if e != nil {
			return e
		}
		c.Federation.MaxLiveAgents = 1
		return nil
	})
	require.NoError(t, err)
	cwd := f.TestCwd("local")
	require.NoError(t, os.MkdirAll(cwd, 0700))
	first := f.AsHuman().SpawnWith("team", map[string]any{"name": "slow", "cwd": cwd, "harness": "claude"})
	require.Equal(t, http.StatusGatewayTimeout, first.Code, string(first.Raw))
	require.Contains(t, string(first.Raw), "spawn_unconfirmed")
	pending, err := db.ListPendingSpawns()
	require.NoError(t, err)
	require.Len(t, pending, 1, "uncertain local process retains a durable capacity reservation")
	// executeSpawn has returned and released its volatile admission reservation.
	second := f.AsHuman().SpawnWith("team", map[string]any{"name": "second", "cwd": cwd, "harness": "claude"})
	require.Equal(t, http.StatusConflict, second.Code, string(second.Raw))
	require.Contains(t, string(second.Raw), "node_busy")
}
