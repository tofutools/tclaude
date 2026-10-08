package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

const moveSourceConv = "019fe740-43a4-7023-b8ae-1ee64459f2a1"

func fedMoveSource(t *testing.T, fh *fedHarness) string {
	t.Helper()
	f := fh.f
	f.HaveGroup("source")
	f.HaveAliveSession(moveSourceConv, "moving-source", "moving-source-pane", testutil.CanonicalTempDir(t))
	f.HaveMember("source", moveSourceConv)
	aid, err := db.AgentIDForConv(moveSourceConv)
	require.NoError(t, err)
	require.NotEmpty(t, aid)
	fh.peer.send(fh.peer.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{AgentMoves: true, Groups: []proto.CatalogGroup{}}))
	fedEventually(t, "move capable catalog", func() bool {
		raw, _, _ := db.GetFederationCatalog(fh.peer.id.ID())
		var c proto.CatalogPayload
		return json.Unmarshal([]byte(raw), &c) == nil && c.AgentMoves
	})
	return aid
}
func fedStartMove(t *testing.T, fh *fedHarness) bundletransfer.Descriptor {
	t.Helper()
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/move-agent", map[string]any{"agent": moveSourceConv, "peer": "bob", "group": "receiver"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var v struct {
		Offer struct {
			D bundletransfer.Descriptor `json:"offer"`
		} `json:"offer"`
	}
	testharness.DecodeJSON(t, rec, &v)
	require.NotNil(t, v.Offer.D.Move)
	return v.Offer.D
}
func fedMoveConfirm(t *testing.T, fh *fedHarness, d bundletransfer.Descriptor, hash string) *proto.Envelope {
	t.Helper()
	env := fh.peer.envelope(proto.KindAgentMoveConfirm, proto.Endpoint{}, bundletransfer.MoveConfirmation{ObservedAt: time.Now().UTC(), Offer: d.ID, SHA256: hash, SourceAgent: d.Move.SourceAgent, SourceConv: d.Move.SourceConv, TargetAgent: "agt_bobremote0000000000000000", TargetConv: "019fe740-43a4-7023-b8ae-1ee64459f2a2"})
	env.From.Agent = ""
	fh.peer.send(env)
	return env
}
func TestFederation_MoveRequiresRunningConfirmationAndBouncesOldMail(t *testing.T) {
	fh := newFedHarness(t)
	aid := fedMoveSource(t, fh)
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": "message.direct", "scope": "group=source"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	d := fedStartMove(t, fh)
	generic := fh.peer.envelope(proto.KindBundleResult, proto.Endpoint{}, bundletransfer.Result{Offer: d.ID, State: "applied"})
	generic.From.Agent = ""
	fh.peer.send(generic)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, generic.ID).Status)
	m, err := db.GetFederationAgentMove("out", fh.peer.id.ID(), d.ID)
	require.NoError(t, err)
	require.Equal(t, "awaiting_confirmation", m.State)
	a, err := db.GetAgent(aid)
	require.NoError(t, err)
	require.True(t, a.Active())
	fedMoveConfirm(t, fh, d, "wrong-digest")
	good := fedMoveConfirm(t, fh, d, d.SHA256)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, good.ID).Status)
	fedEventually(t, "source moved after running confirmation", func() bool {
		m, _ := db.GetFederationAgentMove("out", fh.peer.id.ID(), d.ID)
		return m != nil && m.State == "moved"
	})
	a, err = db.GetAgent(aid)
	require.NoError(t, err)
	require.False(t, a.Active())
	require.Equal(t, moveSourceConv, a.CurrentConvID)
	conv, err := db.GetConvIndex(moveSourceConv)
	require.NoError(t, err)
	require.NotNil(t, conv, "source conversation remains")
	mail := fh.peer.envelope(proto.KindMail, proto.Endpoint{Agent: aid}, proto.MailPayload{Body: "old address"})
	fh.peer.send(mail)
	ack := fedAckFor(t, fh.peer, mail.ID)
	require.Equal(t, proto.AckRefused, ack.Status)
	require.Equal(t, "agent_moved", ack.Code)
	require.NotContains(t, ack.Reason, "agt_bobremote")
	replay := fedMoveConfirm(t, fh, d, d.SHA256)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, replay.ID).Status)
	rec = fedHuman(t, fh.f, http.MethodGet, "/v1/federation/moves/"+d.ID, nil)
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), "moved_to")
}
func TestFederation_MoveAbandonMakesLateConfirmationInert(t *testing.T) {
	fh := newFedHarness(t)
	aid := fedMoveSource(t, fh)
	d := fedStartMove(t, fh)
	for i := 0; i < 2; i++ {
		rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/moves/"+d.ID+"/abandon", nil)
		require.Equal(t, 200, rec.Code, rec.Body.String())
	}
	env := fedMoveConfirm(t, fh, d, d.SHA256)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, env.ID).Status)
	m, err := db.GetFederationAgentMove("out", fh.peer.id.ID(), d.ID)
	require.NoError(t, err)
	require.Equal(t, "abandoned", m.State)
	a, err := db.GetAgent(aid)
	require.NoError(t, err)
	require.True(t, a.Active())
}
func TestFederation_MoveGenerationChangeBlocksRetirement(t *testing.T) {
	fh := newFedHarness(t)
	aid := fedMoveSource(t, fh)
	d := fedStartMove(t, fh)
	const next = "019fe740-43a4-7023-b8ae-1ee64459f2a3"
	fh.f.HaveAliveSession(next, "successor", "successor-pane", testutil.CanonicalTempDir(t))
	_, err := db.RotateAgentConv(moveSourceConv, next, "reincarnate")
	require.NoError(t, err)
	fedMoveConfirm(t, fh, d, d.SHA256)
	fedEventually(t, "rotated source blocked", func() bool {
		m, _ := db.GetFederationAgentMove("out", fh.peer.id.ID(), d.ID)
		return m != nil && m.State == "blocked"
	})
	a, err := db.GetAgent(aid)
	require.NoError(t, err)
	require.True(t, a.Active())
	require.Equal(t, next, a.CurrentConvID)
}
func TestFederation_MoveReceiverConfirmsReservedRunningIdentity(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("receiver")
	fedReceiveAgents(t, fh, "receiver")
	b := fedAgentBundle(t)
	b.SetHistory("claude-jsonl", moveSourceConv, []byte(`{"type":"user","sessionId":"`+moveSourceConv+`","cwd":"/source","message":{"content":"move history"}}`+"\n"))
	raw, err := b.Encode()
	require.NoError(t, err)
	d := bundletransfer.New(bundletransfer.Agent, raw, "Move worker", time.Now().Add(time.Hour))
	d.Group = "receiver"
	d.Move = &bundletransfer.MoveIntent{SourceAgent: "agt_bobremote0000000000000000", SourceConv: moveSourceConv}
	env := fh.peer.envelope(proto.KindBundleOffer, proto.Endpoint{}, d)
	env.ID = d.ID
	fh.peer.send(env)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, d.ID).Status)
	path := "/v1/federation/bundle-offers/" + d.ID + "/import"
	rec := fedHuman(t, fh.f, http.MethodPost, path, map[string]any{"cwd": testutil.CanonicalTempDir(t), "skip_history": true, "apply": true})
	require.Equal(t, 400, rec.Code)
	rec = fedHuman(t, fh.f, http.MethodPost, path, map[string]any{"cwd": testutil.CanonicalTempDir(t)})
	require.Equal(t, 200, rec.Code)
	require.Empty(t, fh.peer.envelopes(proto.KindAgentMoveConfirm))
	rec = fedHuman(t, fh.f, http.MethodPost, path, map[string]any{"cwd": testutil.CanonicalTempDir(t), "apply": true})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	fedEventually(t, "move running confirmation", func() bool { return len(fh.peer.envelopes(proto.KindAgentMoveConfirm)) > 0 })
	var c bundletransfer.MoveConfirmation
	require.NoError(t, fh.peer.envelopes(proto.KindAgentMoveConfirm)[0].DecodePayload(&c))
	require.Equal(t, d.SHA256, c.SHA256)
	require.NotEqual(t, d.Move.SourceAgent, c.TargetAgent)
	require.NotEqual(t, moveSourceConv, c.TargetConv)
	m, err := db.GetFederationAgentMove("in", fh.peer.id.ID(), d.ID)
	require.NoError(t, err)
	require.Equal(t, "running", m.State)
	require.NotNil(t, m.MovedFrom)
	require.Equal(t, fh.peer.id.ID(), m.MovedFrom.Instance)
	a, err := db.GetAgent(c.TargetAgent)
	require.NoError(t, err)
	require.True(t, a.Active())
	require.Equal(t, c.TargetConv, a.CurrentConvID)
}
func TestFederation_MoveAgentAuthorityRechecked(t *testing.T) {
	fh := newFedHarness(t)
	aid := fedMoveSource(t, fh)
	in := map[string]any{"agent": "self", "peer": "bob", "group": "receiver"}
	request := func() *httptest.ResponseRecorder {
		return testharness.Serve(fh.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/federation/move-agent", in), moveSourceConv))
	}
	rec := request()
	require.Equal(t, 403, rec.Code)
	require.NoError(t, db.GrantAgentPermissionWithScope(moveSourceConv, agentd.PermAgentMove, `{"peer":["`+fh.peer.id.ID()+`"]}`, "test"))
	rec = request()
	require.Equal(t, 403, rec.Code, "move alone does not retire authority")
	require.NoError(t, db.GrantAgentPermission(moveSourceConv, agentd.PermAgentRetire, "test"))
	rec = request()
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var v struct {
		Offer struct {
			D bundletransfer.Descriptor `json:"offer"`
		} `json:"offer"`
	}
	testharness.DecodeJSON(t, rec, &v)
	require.NoError(t, db.SetAgentPermissionOverride(moveSourceConv, agentd.PermAgentRetire, db.PermEffectDeny, "test"))
	fedMoveConfirm(t, fh, v.Offer.D, v.Offer.D.SHA256)
	fedEventually(t, "revoked retire authority blocks move", func() bool {
		m, _ := db.GetFederationAgentMove("out", fh.peer.id.ID(), v.Offer.D.ID)
		return m != nil && m.State == "blocked"
	})
	a, err := db.GetAgent(aid)
	require.NoError(t, err)
	require.True(t, a.Active())
}

func TestFederation_MoveRequiresCapabilityAndExclusiveIntent(t *testing.T) {
	fh := newFedHarness(t)
	fedMoveSource(t, fh)
	raw, _, err := db.GetFederationCatalog(fh.peer.id.ID())
	require.NoError(t, err)
	var cat proto.CatalogPayload
	require.NoError(t, json.Unmarshal([]byte(raw), &cat))
	cat.AgentMoves = false
	require.NoError(t, db.PutFederationCatalog(fh.peer.id.ID(), string(mustJSON(t, cat)), time.Now()))
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/move-agent", map[string]any{"agent": moveSourceConv, "peer": "bob", "group": "receiver"})
	require.Equal(t, 409, rec.Code)
	require.Contains(t, rec.Body.String(), "unsupported_peer")
	cat.AgentMoves = true
	require.NoError(t, db.PutFederationCatalog(fh.peer.id.ID(), string(mustJSON(t, cat)), time.Now()))
	d := fedStartMove(t, fh)
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/move-agent", map[string]any{"agent": moveSourceConv, "peer": "bob", "group": "receiver"})
	require.Equal(t, 409, rec.Code)
	m, err := db.GetFederationAgentMove("out", fh.peer.id.ID(), d.ID)
	require.NoError(t, err)
	m.ExpiresAt = time.Now().Add(-time.Minute)
	won, err := db.TransitionFederationAgentMove(*m, m.State)
	require.NoError(t, err)
	require.True(t, won)
	fedEventually(t, "unconfirmed move expires", func() bool {
		m, _ := db.GetFederationAgentMove("out", fh.peer.id.ID(), d.ID)
		return m != nil && m.State == "expired"
	})
	a, err := db.GetAgent(d.Move.SourceAgent)
	require.NoError(t, err)
	require.True(t, a.Active())
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/moves/"+d.ID+"/abandon", nil)
	require.Equal(t, 200, rec.Code)
	_ = fedStartMove(t, fh)
}
func TestFederation_MovePermissionRecheckAllowsUnchangedAgentAuthority(t *testing.T) {
	fh := newFedHarness(t)
	aid := fedMoveSource(t, fh)
	require.NoError(t, db.GrantAgentPermissionWithScope(moveSourceConv, agentd.PermAgentMove, `{"peer":["`+fh.peer.id.ID()+`"]}`, "test"))
	require.NoError(t, db.GrantAgentPermission(moveSourceConv, agentd.PermAgentRetire, "test"))
	rec := testharness.Serve(fh.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/federation/move-agent", map[string]any{"agent": "self", "peer": "bob", "group": "receiver"}), moveSourceConv))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var v struct {
		Offer struct {
			D bundletransfer.Descriptor `json:"offer"`
		} `json:"offer"`
	}
	testharness.DecodeJSON(t, rec, &v)
	fedMoveConfirm(t, fh, v.Offer.D, v.Offer.D.SHA256)
	fedEventually(t, "authorized agent move retires source", func() bool {
		m, _ := db.GetFederationAgentMove("out", fh.peer.id.ID(), v.Offer.D.ID)
		return m != nil && m.State == "moved"
	})
	a, err := db.GetAgent(aid)
	require.NoError(t, err)
	require.False(t, a.Active())
}

func TestFederation_MoveRestartFinishesCommittedRetirement(t *testing.T) {
	fh := newFedHarness(t)
	aid := fedMoveSource(t, fh)
	d := fedStartMove(t, fh)
	agentd.ResetFederationForTest()
	// Model a daemon crash between the normal retirement commit and teardown.
	m, err := db.GetFederationAgentMove("out", fh.peer.id.ID(), d.ID)
	require.NoError(t, err)
	m.State = "retiring"
	m.TargetAgent = "agt_bobremote0000000000000000"
	m.TargetConv = "019fe740-43a4-7023-b8ae-1ee64459f2a2"
	m.ConfirmedAt = time.Now()
	m.MovedTo = &db.FederationMoveLink{Instance: m.Peer, Agent: m.TargetAgent, Offer: m.ID}
	won, err := db.TransitionFederationAgentMove(*m, "awaiting_confirmation")
	require.NoError(t, err)
	require.True(t, won)
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/agent/"+aid+"/retire?shutdown=0", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/config", map[string]any{"enabled": true, "hub_url": fh.url, "name": "alice-box"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	fedEventually(t, "restart completes move teardown", func() bool {
		m, _ := db.GetFederationAgentMove("out", fh.peer.id.ID(), d.ID)
		return m != nil && m.State == "moved"
	})
	a, err := db.GetAgent(aid)
	require.NoError(t, err)
	require.False(t, a.Active())
	require.Equal(t, moveSourceConv, a.CurrentConvID)
}
