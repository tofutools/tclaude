package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func fedTeleportSource(t *testing.T, fh *fedHarness) string {
	t.Helper()
	aid := fedMoveSource(t, fh)
	cat := proto.CatalogPayload{AgentMoves: true, AgentTeleports: 1, Groups: []proto.CatalogGroup{}}
	require.NoError(t, db.PutFederationCatalog(fh.peer.id.ID(), string(mustJSON(t, cat)), time.Now()))
	return aid
}
func fedSelfTeleport(t *testing.T, fh *fedHarness, input map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return testharness.Serve(fh.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/teleport", input), moveSourceConv))
}
func TestFederation_TeleportSelfGrantAndConfirmedRetirement(t *testing.T) {
	fh := newFedHarness(t)
	aid := fedTeleportSource(t, fh)
	input := map[string]any{"peer": "bob", "group": "receiver", "note": "Continue the focused tests.", "credentials": "local"}
	rec := fedSelfTeleport(t, fh, input)
	require.Equal(t, 403, rec.Code, rec.Body.String())
	require.NoError(t, db.GrantAgentPermissionWithScope(moveSourceConv, agentd.PermSelfTeleport, `{"peer":["`+fh.peer.id.ID()+`"]}`, "test"))
	// Teleport has its own authority; ordinary cross-agent lifecycle grants do not apply.
	require.NoError(t, db.SetAgentPermissionOverride(moveSourceConv, agentd.PermAgentRetire, db.PermEffectDeny, "test"))
	require.NoError(t, db.SetAgentPermissionOverride(moveSourceConv, agentd.PermAgentMove, db.PermEffectDeny, "test"))
	rec = fedSelfTeleport(t, fh, input)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var result struct {
		Offer struct {
			D bundletransfer.Descriptor `json:"offer"`
		} `json:"offer"`
	}
	testharness.DecodeJSON(t, rec, &result)
	d := result.Offer.D
	require.NotNil(t, d.Teleport)
	require.Equal(t, aid, d.Teleport.SourceAgent)
	require.Equal(t, "local", d.Teleport.Credentials)
	a, err := db.GetAgent(aid)
	require.NoError(t, err)
	require.True(t, a.Active())
	// An import receipt alone never retires the predecessor.
	receipt := fh.peer.envelope(proto.KindBundleResult, proto.Endpoint{}, bundletransfer.Result{Offer: d.ID, State: "applied"})
	receipt.From.Agent = ""
	fh.peer.send(receipt)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, receipt.ID).Status)
	a, err = db.GetAgent(aid)
	require.NoError(t, err)
	require.True(t, a.Active())
	fedMoveConfirm(t, fh, d, d.SHA256)
	fedEventually(t, "teleport source retired", func() bool {
		m, _ := db.GetFederationAgentMove("out", fh.peer.id.ID(), d.ID)
		return m != nil && m.State == "moved"
	})
	// The addressed peer can discover the new teleport address.
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": "message.direct", "scope": "group=source"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	mail := fh.peer.envelope(proto.KindMail, proto.Endpoint{Agent: aid}, proto.MailPayload{Body: "old address"})
	fh.peer.send(mail)
	ack := fedAckFor(t, fh.peer, mail.ID)
	require.Equal(t, "agent_moved", ack.Code)
	require.Contains(t, ack.Reason, "agt_bobremote0000000000000000@"+fh.peer.id.ID())
}
func TestFederation_TeleportRevocationKeepsSourceAlive(t *testing.T) {
	fh := newFedHarness(t)
	aid := fedTeleportSource(t, fh)
	require.NoError(t, db.GrantAgentPermissionWithScope(moveSourceConv, agentd.PermSelfTeleport, `{"peer":["`+fh.peer.id.ID()+`"]}`, "test"))
	rec := fedSelfTeleport(t, fh, map[string]any{"peer": "bob", "group": "receiver"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var result struct {
		Offer struct {
			D bundletransfer.Descriptor `json:"offer"`
		} `json:"offer"`
	}
	testharness.DecodeJSON(t, rec, &result)
	require.NoError(t, db.SetAgentPermissionOverride(moveSourceConv, agentd.PermSelfTeleport, db.PermEffectDeny, "test"))
	fedMoveConfirm(t, fh, result.Offer.D, result.Offer.D.SHA256)
	fedEventually(t, "revoked teleport blocked", func() bool {
		m, _ := db.GetFederationAgentMove("out", fh.peer.id.ID(), result.Offer.D.ID)
		return m != nil && m.State == "blocked"
	})
	a, err := db.GetAgent(aid)
	require.NoError(t, err)
	require.True(t, a.Active())
}
func fedIncomingTeleport(t *testing.T, fh *fedHarness, credentials string, alter func(*bundletransfer.TeleportIntent)) bundletransfer.Descriptor {
	t.Helper()
	b := fedAgentBundle(t)
	transcript := append(mustJSON(t, map[string]any{"type": "user", "sessionId": moveSourceConv, "cwd": "/source", "message": map[string]string{"content": "Continue the test task."}}), '\n')
	b.SetHistory("claude-jsonl", moveSourceConv, transcript)
	raw, err := b.Encode()
	require.NoError(t, err)
	d := bundletransfer.New(bundletransfer.Agent, raw, "Teleport continuation", time.Now().Add(time.Hour))
	d.Group = "receiver"
	d.Teleport = &bundletransfer.TeleportIntent{Version: 1, Chain: proto.NewEnvelopeID(), OriginInstance: fh.peer.id.ID(), OriginAgent: "agt_bobremote0000000000000000", SourceAgent: "agt_bobremote0000000000000000", SourceConv: moveSourceConv, Clone: true, Credentials: credentials, Note: "Finish the task", Hops: []bundletransfer.TeleportHop{{Offer: d.ID, FromInstance: fh.peer.id.ID(), FromAgent: "agt_bobremote0000000000000000", ToInstance: fh.peer.agentdID, ToGroup: "receiver", At: time.Now().UTC()}}}
	if alter != nil {
		alter(d.Teleport)
	}
	if !d.Teleport.Clone {
		d.Move = &bundletransfer.MoveIntent{SourceAgent: d.Teleport.SourceAgent, SourceConv: d.Teleport.SourceConv}
	}
	env := fh.peer.envelope(proto.KindBundleOffer, proto.Endpoint{}, d)
	env.ID = d.ID
	fh.peer.send(env)
	return d
}
func TestFederation_TeleportPendingImportPreservesPredecessor(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("receiver")
	fedReceiveAgents(t, fh, "receiver")
	d := fedIncomingTeleport(t, fh, "local", nil)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, d.ID).Status)
	row, err := db.GetFederationTeleport("in", fh.peer.id.ID(), d.ID)
	require.NoError(t, err)
	require.Equal(t, "pending", row.State)
	path := "/v1/federation/bundle-offers/" + d.ID + "/import"
	rec := fedHuman(t, fh.f, http.MethodPost, path, map[string]any{"cwd": testutil.CanonicalTempDir(t), "apply": true})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"credentials":"local"`)
	row, err = db.GetFederationTeleport("in", fh.peer.id.ID(), d.ID)
	require.NoError(t, err)
	require.Equal(t, "landed", row.State)
	require.NotEmpty(t, row.TargetAgent)
	a, err := db.GetAgent(row.TargetAgent)
	require.NoError(t, err)
	require.NotNil(t, a)
	who := testharness.Serve(fh.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/whoami", nil), a.CurrentConvID))
	require.Equal(t, 200, who.Code, who.Body.String())
	require.Contains(t, who.Body.String(), `"predecessor"`)
	require.Contains(t, who.Body.String(), d.Teleport.SourceAgent)
	var body map[string]any
	require.NoError(t, json.Unmarshal(who.Body.Bytes(), &body))
	rec = fedHuman(t, fh.f, http.MethodPost, path, map[string]any{"cwd": testutil.CanonicalTempDir(t), "apply": true})
	require.Equal(t, 409, rec.Code)
}
func TestFederation_TeleportProxyAndForgedDestinationRefused(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("receiver")
	fedReceiveAgents(t, fh, "receiver")
	d := fedIncomingTeleport(t, fh, "proxy:main@bob", nil)
	ack := fedAckFor(t, fh.peer, d.ID)
	require.Equal(t, proto.AckRefused, ack.Status)
	require.Contains(t, ack.Reason, "not allowed")
	d = fedIncomingTeleport(t, fh, "local", func(in *bundletransfer.TeleportIntent) { in.Hops[0].ToInstance = fh.peer.id.ID() })
	require.Equal(t, proto.AckRefused, fedAckFor(t, fh.peer, d.ID).Status)
}

type fedTeleportBirthSpawner struct {
	inner agentd.Spawner
	check func(clcommon.SpawnArgs)
}

func (s *fedTeleportBirthSpawner) SpawnNew(a clcommon.SpawnArgs) error {
	s.check(a)
	return s.inner.SpawnNew(a)
}
func (s *fedTeleportBirthSpawner) SpawnResume(a clcommon.SpawnArgs) error {
	s.check(a)
	return s.inner.SpawnResume(a)
}

func TestFederation_TeleportAutomaticLandingUsesReceiverPolicy(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("receiver")
	cwd := testutil.CanonicalTempDir(t)
	profile := &db.SpawnProfile{Name: "landing", Harness: "claude", Model: "sonnet", Approval: "default"}
	_, err := db.CreateSpawnProfile(profile)
	require.NoError(t, err)
	p := fedNodeProfile(t, fh, "landing-node", db.FederationNodeProfileSpec{
		PeerGrants:        []db.FederationPeerGrant{{Slug: agentd.PermAgentsTeleportReceive, Scope: "group=receiver"}},
		WorkerPermissions: map[string]db.PermissionOverride{"self.rename": {Effect: "deny"}},
		TeleportLanding:   &db.FederationTeleportLanding{Group: "receiver", Cwd: cwd, SpawnProfile: "landing", MaxLive: 1},
	})
	fedApplyNodeProfile(t, fh, p)
	previous := agentd.Spawn
	births := 0
	agentd.Spawn = &fedTeleportBirthSpawner{inner: previous, check: func(args clcommon.SpawnArgs) {
		births++
		require.Equal(t, "sonnet", args.Model)
		require.Equal(t, cwd, args.Cwd)
		conv := args.SessionID
		if conv == "" {
			conv = args.ConvID
		}
		permissions, err := db.ListAgentPermissionOverridesForConv(conv)
		require.NoError(t, err)
		require.Equal(t, "deny", permissions["self.rename"], "receiver defaults must exist before execution")
	}}
	t.Cleanup(func() { agentd.Spawn = previous })
	d := fedIncomingTeleport(t, fh, "", func(in *bundletransfer.TeleportIntent) { in.Clone = false })
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, d.ID).Status)
	var row *db.FederationTeleport
	fedEventually(t, "automatic teleport landing", func() bool {
		row, _ = db.GetFederationTeleport("in", fh.peer.id.ID(), d.ID)
		return row != nil && row.State != "auto_pending" && row.State != "admitting"
	})
	require.Equal(t, "landed", row.State)
	require.Equal(t, 1, births)
	require.Equal(t, "local", row.Credentials)
	fedEventually(t, "automatic teleport running confirmation", func() bool { return len(fh.peer.envelopes(proto.KindAgentMoveConfirm)) > 0 })
	a, err := db.GetAgent(row.TargetAgent)
	require.NoError(t, err)
	require.NotNil(t, a)
	grants, err := db.ListAgentPermissionOverridesForConv(a.CurrentConvID)
	require.NoError(t, err)
	require.Equal(t, "deny", grants["self.rename"])
	require.NotEqual(t, "grant", grants["config.import"], "source permissions never become receiver grants")
	defaults, err := db.GetFederationWorkerDefaults(a.AgentID)
	require.NoError(t, err)
	require.NotNil(t, defaults)
	require.Equal(t, "landing-node", defaults.ProfileName)
	// A second automatic landing stays pending while the first worker occupies the cap.
	d2 := fedIncomingTeleport(t, fh, "local", nil)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, d2.ID).Status)
	fedEventually(t, "landing cap refusal", func() bool {
		r, _ := db.GetFederationTeleport("in", fh.peer.id.ID(), d2.ID)
		return r != nil && r.State == "pending"
	})
	r, err := db.GetFederationTeleport("in", fh.peer.id.ID(), d2.ID)
	require.NoError(t, err)
	require.Empty(t, r.TargetAgent)
}

func TestFederation_TeleportCloneQuotaAndFreeze(t *testing.T) {
	fh := newFedHarness(t)
	aid := fedTeleportSource(t, fh)
	require.NoError(t, db.GrantAgentPermissionWithScope(moveSourceConv, agentd.PermSelfTeleport, `{"peer":["`+fh.peer.id.ID()+`"]}`, "test"))
	for i := 0; i < 4; i++ {
		rec := fedSelfTeleport(t, fh, map[string]any{"peer": "bob", "group": "receiver", "clone": true})
		require.Equal(t, 200, rec.Code, rec.Body.String())
		var result struct {
			Offer struct {
				D bundletransfer.Descriptor `json:"offer"`
			} `json:"offer"`
		}
		testharness.DecodeJSON(t, rec, &result)
		require.Nil(t, result.Offer.D.Move)
		require.True(t, result.Offer.D.Teleport.Clone)
	}
	rec := fedSelfTeleport(t, fh, map[string]any{"peer": "bob", "group": "receiver", "clone": true})
	require.Equal(t, 429, rec.Code, rec.Body.String())
	a, err := db.GetAgent(aid)
	require.NoError(t, err)
	require.True(t, a.Active())
	rec = fedHuman(t, fh.f, http.MethodPut, "/v1/federation/teleport", map[string]any{"disabled": true})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	rec = fedSelfTeleport(t, fh, map[string]any{"peer": "bob", "group": "receiver"})
	require.Equal(t, 409, rec.Code)
	require.Contains(t, rec.Body.String(), "frozen")
}
func TestFederation_TeleportReceiverFreezeAndLoopRefusal(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("receiver")
	fedReceiveAgents(t, fh, "receiver")
	d := fedIncomingTeleport(t, fh, "local", nil)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, d.ID).Status)
	rec := fedHuman(t, fh.f, http.MethodPut, "/v1/federation/teleport", map[string]any{"disabled": true})
	require.Equal(t, 200, rec.Code)
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/bundle-offers/"+d.ID+"/import", map[string]any{"cwd": testutil.CanonicalTempDir(t), "apply": true})
	require.Equal(t, 409, rec.Code, rec.Body.String())
	row, err := db.GetFederationTeleport("in", fh.peer.id.ID(), d.ID)
	require.NoError(t, err)
	require.Empty(t, row.TargetAgent)
	rec = fedHuman(t, fh.f, http.MethodPut, "/v1/federation/teleport", map[string]any{"disabled": false})
	require.Equal(t, 200, rec.Code)
	d = fedIncomingTeleport(t, fh, "local", func(in *bundletransfer.TeleportIntent) {
		last := in.Hops[0]
		first := bundletransfer.TeleportHop{Offer: proto.NewEnvelopeID(), FromInstance: fh.peer.agentdID, FromAgent: in.OriginAgent, ToInstance: fh.peer.id.ID(), ToGroup: "old-group", At: time.Now().Add(-time.Minute)}
		in.OriginInstance = fh.peer.agentdID
		in.Hops = []bundletransfer.TeleportHop{first, last}
	})
	ack := fedAckFor(t, fh.peer, d.ID)
	require.Equal(t, proto.AckRefused, ack.Status)
	require.Contains(t, ack.Reason, "ping-pong")
}

func TestFederation_TeleportCredentialPrecedenceNeverFallsBack(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("receiver")
	profile := &db.SpawnProfile{Name: "credential-landing", Harness: "claude"}
	_, err := db.CreateSpawnProfile(profile)
	require.NoError(t, err)
	p := fedNodeProfile(t, fh, "credential-node", db.FederationNodeProfileSpec{PeerGrants: []db.FederationPeerGrant{{Slug: agentd.PermAgentsReceive, Scope: "group=receiver"}}, TeleportLanding: &db.FederationTeleportLanding{Group: "receiver", Cwd: testutil.CanonicalTempDir(t), SpawnProfile: profile.Name, MaxLive: 1, CredentialsDefault: "proxy:main@home", CredentialsAllowed: []string{"local", "proxy:main@home"}}})
	fedApplyNodeProfile(t, fh, p)
	d := fedIncomingTeleport(t, fh, "", nil)
	ack := fedAckFor(t, fh.peer, d.ID)
	require.Equal(t, proto.AckRefused, ack.Status)
	require.NotEmpty(t, ack.Reason)
	d = fedIncomingTeleport(t, fh, "local", nil)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, d.ID).Status)
	row, err := db.GetFederationTeleport("in", fh.peer.id.ID(), d.ID)
	require.NoError(t, err)
	require.Equal(t, "local", row.Credentials)
	p.Definition.TeleportLanding.CredentialsAllowed = []string{"proxy:main@home"}
	rec := fedHuman(t, fh.f, http.MethodPut, "/v1/federation/profiles/credential-node", p)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	testharness.DecodeJSON(t, rec, p)
	fedApplyNodeProfile(t, fh, p)
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/bundle-offers/"+d.ID+"/import", map[string]any{"cwd": testutil.CanonicalTempDir(t), "apply": true})
	require.Equal(t, 409, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "not allowed")
	row, err = db.GetFederationTeleport("in", fh.peer.id.ID(), d.ID)
	require.NoError(t, err)
	require.Empty(t, row.TargetAgent)
}
func TestFederation_TeleportPlacementScopesAndStaleMetadata(t *testing.T) {
	fh := newFedHarness(t)
	aid := fedTeleportSource(t, fh)
	require.NoError(t, db.GrantAgentPermissionWithScope(moveSourceConv, agentd.PermSelfTeleport, `{"peer":["`+fh.peer.id.ID()+`"]}`, "test"))
	at := time.Now().UTC()
	n := placementNode(1)
	cat := proto.CatalogPayload{AgentMoves: true, AgentTeleports: 1, Node: n, NodeAt: at, Groups: []proto.CatalogGroup{{Name: "receiver", Caps: []string{proto.CapTeleportReceive}}}}
	fh.peer.send(fh.peer.envelope(proto.KindCatalog, proto.Endpoint{}, cat))
	fedEventually(t, "teleport placement metadata", func() bool {
		raw, _, _ := db.GetFederationCatalog(fh.peer.id.ID())
		var c proto.CatalogPayload
		return json.Unmarshal([]byte(raw), &c) == nil && c.Node != nil && c.AgentTeleports == 1
	})
	rec := fedSelfTeleport(t, fh, map[string]any{"node": "auto", "group": "receiver", "clone": true})
	require.Equal(t, 409, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"candidates":[]`)
	require.NoError(t, db.GrantAgentPermissionWithScope(moveSourceConv, agentd.PermNodeRead, `{"peer":["`+fh.peer.id.ID()+`"]}`, "test"))
	rec = fedSelfTeleport(t, fh, map[string]any{"node": "auto", "group": "receiver", "clone": true, "require": "os=darwin"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"selected":"`+fh.peer.id.ID()+`"`)
	old := time.Now().Add(-5 * time.Minute)
	cat.NodeAt = time.Now().UTC()
	cat.Node.Resources.ObservedAt = &old
	fh.peer.send(fh.peer.envelope(proto.KindCatalog, proto.Endpoint{}, cat))
	fedEventually(t, "stale teleport placement metadata", func() bool {
		raw, _, _ := db.GetFederationCatalog(fh.peer.id.ID())
		var c proto.CatalogPayload
		return json.Unmarshal([]byte(raw), &c) == nil && c.Node != nil && c.Node.Resources.ObservedAt.Equal(old)
	})
	rec = fedSelfTeleport(t, fh, map[string]any{"node": "auto", "group": "receiver", "clone": true})
	require.Equal(t, 409, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "stale")
	a, err := db.GetAgent(aid)
	require.NoError(t, err)
	require.True(t, a.Active())
}

func TestFederation_TeleportGitRefUsesAllowlistedIsolatedCheckout(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "automatic"}[automatic], func(t *testing.T) {
			fh := newFedHarness(t)
			fh.f.HaveGroup("team")
			fh.f.HaveGroup("receiver")
			clone := fedJobRepo(t, fh)
			repo, err := db.GetFederationRepo("project")
			require.NoError(t, err)
			rec := fedHuman(t, fh.f, http.MethodPut, "/v1/federation/repos/project", map[string]any{"name": "project", "url": (&url.URL{Scheme: "file", Path: clone}).String(), "clone": clone, "groups": []string{"receiver"}, "revision": repo.Revision})
			require.Equal(t, 200, rec.Code, rec.Body.String())
			profile := &db.SpawnProfile{Name: "git-landing", Harness: "claude", Approval: "default"}
			_, err = db.CreateSpawnProfile(profile)
			require.NoError(t, err)
			slug := agentd.PermAgentsReceive
			if automatic {
				slug = agentd.PermAgentsTeleportReceive
			}
			p := fedNodeProfile(t, fh, "git-node", db.FederationNodeProfileSpec{PeerGrants: []db.FederationPeerGrant{{Slug: slug, Scope: "group=receiver"}}, TeleportLanding: &db.FederationTeleportLanding{Group: "receiver", Cwd: clone, Repo: "project", SpawnProfile: profile.Name, MaxLive: 2}})
			fedApplyNodeProfile(t, fh, p)
			d := fedIncomingTeleport(t, fh, "local", func(in *bundletransfer.TeleportIntent) { in.GitRef = "main" })
			require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, d.ID).Status)
			if !automatic {
				path := "/v1/federation/bundle-offers/" + d.ID + "/import"
				rec = fedHuman(t, fh.f, http.MethodPost, path, nil)
				require.Equal(t, 200, rec.Code, rec.Body.String())
				require.Contains(t, rec.Body.String(), `"git_ref":"main"`)
				row, err := db.GetFederationTeleport("in", fh.peer.id.ID(), d.ID)
				require.NoError(t, err)
				require.Nil(t, row.Checkout, "preview never prepares a checkout")
				fh.f.HaveAliveSession("019fe740-43a4-7023-b8ae-1ee64459f2a8", "occupied", "occupied-pane", clone)
				fh.f.HaveMember("receiver", "019fe740-43a4-7023-b8ae-1ee64459f2a8")
				_, err = db.SetAgentGroupMaxMembers("receiver", 1)
				require.NoError(t, err)
				rec = fedHuman(t, fh.f, http.MethodPost, path, map[string]any{"apply": true})
				require.Equal(t, 409, rec.Code, rec.Body.String())
				row, err = db.GetFederationTeleport("in", fh.peer.id.ID(), d.ID)
				require.NoError(t, err)
				require.Equal(t, "pending", row.State)
				require.Nil(t, row.Checkout, "proven unlaunched checkout must be cleared for retry")
				_, err = db.SetAgentGroupMaxMembers("receiver", 2)
				require.NoError(t, err)
				rec = fedHuman(t, fh.f, http.MethodPost, path, map[string]any{"apply": true})
				require.Equal(t, 200, rec.Code, rec.Body.String())
			}
			var row *db.FederationTeleport
			fedEventually(t, "git teleport landed", func() bool {
				row, _ = db.GetFederationTeleport("in", fh.peer.id.ID(), d.ID)
				return row != nil && row.State != "auto_pending" && row.State != "admitting"
			})
			require.Equal(t, "landed", row.State)
			require.NotNil(t, row.Checkout)
			require.NotEqual(t, clone, row.Checkout.Path)
			data, err := os.ReadFile(filepath.Join(row.Checkout.Path, "hello"))
			require.NoError(t, err)
			require.Equal(t, "from git\n", string(data))
			require.NotEmpty(t, row.Checkout.Commit)
			actor, err := db.GetAgent(row.TargetAgent)
			require.NoError(t, err)
			require.NotNil(t, actor)
			sessions, err := db.FindSessionsByConvID(actor.CurrentConvID)
			require.NoError(t, err)
			require.NotEmpty(t, sessions)
			require.Equal(t, row.Checkout.Path, sessions[0].Cwd)
			// The operator clone is never checked out or modified by landing.
			data, err = os.ReadFile(filepath.Join(clone, "hello"))
			require.NoError(t, err)
			require.Equal(t, "from git\n", string(data))
		})
	}
}

func TestFederation_TeleportFinalAuthorityRefusalReleasesUndispatchedReservation(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("receiver")
	cwd := testutil.CanonicalTempDir(t)
	profile := &db.SpawnProfile{Name: "dispatch-boundary", Harness: "claude", Approval: "default"}
	_, err := db.CreateSpawnProfile(profile)
	require.NoError(t, err)
	p := fedNodeProfile(t, fh, "dispatch-node", db.FederationNodeProfileSpec{PeerGrants: []db.FederationPeerGrant{{Slug: agentd.PermAgentsTeleportReceive, Scope: "group=receiver"}}, TeleportLanding: &db.FederationTeleportLanding{Group: "receiver", Cwd: cwd, SpawnProfile: profile.Name, MaxLive: 1}})
	fedApplyNodeProfile(t, fh, p)
	database, err := db.Open()
	require.NoError(t, err)
	// Change local policy exactly when the importer records its pre-dispatch
	// marker. This exercises the last authority gate without invoking Spawn.
	_, err = database.Exec(`CREATE TRIGGER disable_teleport_at_dispatch AFTER UPDATE OF import_label ON federation_bundle_offers WHEN NEW.import_label <> '' BEGIN UPDATE spawn_profiles SET disabled=1 WHERE name='dispatch-boundary'; END`)
	require.NoError(t, err)
	d := fedIncomingTeleport(t, fh, "local", nil)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, d.ID).Status)
	var row *db.FederationTeleport
	fedEventually(t, "final authority refusal settled", func() bool {
		row, _ = db.GetFederationTeleport("in", fh.peer.id.ID(), d.ID)
		return row != nil && row.State == "pending"
	})
	require.Empty(t, row.TargetAgent)
	offer, err := db.GetFederationBundleOffer("in", fh.peer.id.ID(), d.ID)
	require.NoError(t, err)
	require.Empty(t, offer.ImportAgent)
	require.Empty(t, offer.ImportLabel)
	require.Empty(t, fh.peer.envelopes(proto.KindAgentMoveConfirm))
	_, err = database.Exec(`DROP TRIGGER disable_teleport_at_dispatch`)
	require.NoError(t, err)
	_, err = database.Exec(`UPDATE spawn_profiles SET disabled=0 WHERE name='dispatch-boundary'`)
	require.NoError(t, err)
	// The operator may now retry the same safely unlaunched offer.
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/bundle-offers/"+d.ID+"/import", map[string]any{"apply": true, "cwd": cwd})
	require.Equal(t, 200, rec.Code, rec.Body.String())
}
