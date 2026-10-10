package agentd_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/client"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

func homeMailPeer(t *testing.T, fh *fedHarness, name string) *fedPeer {
	t.Helper()
	id, e := proto.NewIdentity()
	require.NoError(t, e)
	require.NoError(t, fh.store.Admit(id.ID()))
	p := &fedPeer{t: t, id: id, agentdID: fh.peer.agentdID}
	cl, e := client.New(client.Options{URL: fh.url, Identity: id, Name: name, MaxBackoff: 100 * time.Millisecond, OnDeliver: func(from string, s *proto.Sealed) {
		key, ok := p.cl.LookupKey(from)
		if !ok {
			return
		}
		env, e := proto.Open(s, ed25519.PublicKey(key), id, time.Now())
		if e != nil {
			return
		}
		p.mu.Lock()
		p.got = append(p.got, env)
		p.mu.Unlock()
	}})
	require.NoError(t, e)
	p.cl = cl
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { cl.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	fedEventually(t, "new mail peer", func() bool { _, ok := cl.LookupKey(p.agentdID); return ok })
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/peers/trust", map[string]any{"instance": id.ID(), "label": name})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.NoError(t, db.PutFederationCatalog(id.ID(), string(mustJSON(t, proto.CatalogPayload{HomeRoutedMail: true, StableAgentIdentity: true})), time.Now()))
	return p
}
func homeMailAck(p *fedPeer, id, status string) bool {
	for _, env := range p.envelopes(proto.KindAck) {
		var ack proto.AckPayload
		_ = env.DecodePayload(&ack)
		if env.InReplyTo == id && ack.Status == status {
			return true
		}
	}
	return false
}
func homeMailControl(p *fedPeer, kind string, payload any) *proto.Envelope {
	env := p.envelope(kind, proto.Endpoint{}, payload)
	env.From.Agent = ""
	env.From.Name = ""
	return env
}

func TestFederation_HomeMailRetargetsAfterHopAndReturnsLocally(t *testing.T) {
	fh := newFedHarness(t)
	require.NoError(t, db.PutFederationCatalog(fh.peer.id.ID(), string(mustJSON(t, proto.CatalogPayload{HomeRoutedMail: true, StableAgentIdentity: true})), time.Now()))
	f, sender := fh.f, fh.peer
	b := homeMailPeer(t, fh, "b")
	c := homeMailPeer(t, fh, "c")
	const conv = "home-mail-source"
	f.HaveGroup("team")
	f.HaveConvWithTitle(conv, "traveling-agent")
	f.HaveMember("team", conv)
	id, e := db.AgentIDForConv(conv)
	require.NoError(t, e)
	rec := fedGrantCaps(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"group": "team", "peer": "bob", "caps": []string{"mail"}})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	home := sender.agentdID
	identity := db.FederationIdentity{Agent: id, Home: home, Hops: 1, Mail: true, Proofs: map[string]string{home: "home-proof"}}
	require.NoError(t, db.DepartFederationIdentity(conv, home, b.id.ID(), "departure-b", identity))
	require.NoError(t, db.PutFederationCatalog(sender.id.ID(), string(mustJSON(t, proto.CatalogPayload{})), time.Now()))
	legacy := sender.envelope(proto.KindMail, proto.Endpoint{Agent: id}, proto.MailPayload{Body: "old sender"})
	sender.send(legacy)
	fedEventually(t, "legacy sender retains moved-address bounce", func() bool { return homeMailAck(sender, legacy.ID, proto.AckRefused) })
	custody, e := db.GetFederationMailCustody(sender.id.ID(), legacy.ID, id)
	require.NoError(t, e)
	require.Nil(t, custody)
	require.NoError(t, db.PutFederationCatalog(sender.id.ID(), string(mustJSON(t, proto.CatalogPayload{HomeRoutedMail: true, StableAgentIdentity: true})), time.Now()))
	mail := sender.envelope(proto.KindMail, proto.Endpoint{Agent: id}, proto.MailPayload{Subject: "while away", Body: "retarget me"})
	sender.send(mail)
	fedEventually(t, "durable home custody", func() bool { return homeMailAck(sender, mail.ID, "custody") })
	fedEventually(t, "one-hop home delivery to B", func() bool { return len(b.envelopes(proto.KindHomeMail)) > 0 })
	// B never acknowledges: C's confirmed hop must retarget the same custody.
	update := homeMailControl(c, proto.KindAgentLocation, map[string]any{"agent": id, "home": home, "host": c.id.ID(), "nonce": "home-proof", "offer": "arrival-c", "hops": 2})
	c.send(update)
	fedEventually(t, "C location accepted", func() bool { return homeMailAck(c, update.ID, proto.AckAccepted) })
	fedEventually(t, "home forwards directly to C", func() bool { return len(c.envelopes(proto.KindHomeMail)) > 0 })
	projected, e := db.ProjectedAgentFederationPresence(id)
	require.NoError(t, e)
	require.Equal(t, c.id.ID(), projected.CurrentInstance)
	delivery := c.envelopes(proto.KindHomeMail)[0]
	var payload struct {
		Op, Home, Agent, Nonce, Envelope string
		Sender                           proto.Endpoint
		Mail                             proto.MailPayload
	}
	require.NoError(t, delivery.DecodePayload(&payload))
	require.Equal(t, "deliver", payload.Op)
	require.Equal(t, sender.id.ID(), payload.Sender.Instance)
	require.Equal(t, mail.ID, payload.Envelope)
	require.Equal(t, "retarget me", payload.Mail.Body)
	ack := homeMailControl(c, proto.KindAck, proto.AckPayload{Status: proto.AckAccepted})
	ack.InReplyTo = delivery.ID
	c.send(ack)
	fedEventually(t, "original sender receives final receipt", func() bool { return homeMailAck(sender, mail.ID, proto.AckAccepted) })
	sender.send(mail)
	fedEventually(t, "custody remains settled", func() bool {
		m, _ := db.GetFederationMailCustody(sender.id.ID(), mail.ID, id)
		return m != nil && m.State == "accepted"
	})
	delivered, e := db.FederationMailDelivered(id, sender.id.ID(), mail.ID)
	require.NoError(t, e)
	require.True(t, delivered)
	// Home return imports markers before becoming runnable. Retrying the lost
	// original receipt cannot insert the already delivered envelope at home.
	identity.Hops = 3
	_, e = db.ReserveFederationIdentity(identity, home, c.id.ID(), "return")
	require.NoError(t, e)
	_, _, e = db.EnsureAgentForConvWithID("home-mail-return", id, "return")
	require.NoError(t, e)
	sender.send(mail)
	time.Sleep(100 * time.Millisecond)
	in, e := db.FederationInboundByEnvelope(sender.id.ID(), mail.ID)
	require.NoError(t, e)
	require.Nil(t, in)
	local := sender.envelope(proto.KindMail, proto.Endpoint{Agent: id}, proto.MailPayload{Body: "new mail after return"})
	sender.send(local)
	fedEventually(t, "new mail lands locally after return", func() bool { in, _ := db.FederationInboundByEnvelope(sender.id.ID(), local.ID); return in != nil })
	stale := homeMailControl(b, proto.KindAgentLocation, map[string]any{"agent": id, "home": home, "host": b.id.ID(), "nonce": "home-proof", "offer": "stale-b", "hops": 1})
	b.send(stale)
	fedEventually(t, "stale location is not accepted", func() bool {
		for _, env := range b.envelopes(proto.KindAck) {
			if env.InReplyTo == stale.ID {
				var ack proto.AckPayload
				_ = env.DecodePayload(&ack)
				return ack.Status == proto.AckRefused && ack.Code == "continuation_refused"
			}
		}
		return false
	})
}

func TestFederation_HomeMailFinalHostGrantDedupAndDepartureFence(t *testing.T) {
	fh := newFedHarness(t)
	require.NoError(t, db.PutFederationCatalog(fh.peer.id.ID(), string(mustJSON(t, proto.CatalogPayload{HomeRoutedMail: true, StableAgentIdentity: true})), time.Now()))
	f, home := fh.f, fh.peer
	identity := db.FederationIdentity{Agent: db.NewAgentID(), Home: home.id.ID(), Hops: 1, Mail: true, Proofs: map[string]string{home.id.ID(): "proof"}}
	_, e := db.ReserveFederationIdentity(identity, home.agentdID, home.id.ID(), "arrival")
	require.NoError(t, e)
	f.HaveGroup("visit")
	f.HaveConvWithTitle("visitor-mail", "visitor")
	_, _, e = db.EnsureAgentForConvWithID("visitor-mail", identity.Agent, "visit")
	require.NoError(t, e)
	f.HaveMember("visit", "visitor-mail")
	original := "original-mail"
	payload := map[string]any{"op": "deliver", "home": home.id.ID(), "agent": identity.Agent, "nonce": "proof", "sender": proto.Endpoint{Instance: home.id.ID(), Agent: "agt_external0000000000000000", Name: "original sender"}, "envelope": original, "expires_at": time.Now().Add(time.Hour), "mail": proto.MailPayload{Subject: "forwarded", Body: "one delivery"}}
	env := homeMailControl(home, proto.KindHomeMail, payload)
	home.send(env)
	fedEventually(t, "final host refuses missing recipient grant", func() bool { return homeMailAck(home, env.ID, proto.AckRefused) })
	rec := fedGrantCaps(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"group": "visit", "peer": "bob", "caps": []string{"mail"}})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	env = homeMailControl(home, proto.KindHomeMail, payload)
	home.send(env)
	fedEventually(t, "final host accepts home-routed mail", func() bool { return homeMailAck(home, env.ID, proto.AckAccepted) })
	in, e := db.FederationInboundByEnvelope(home.id.ID(), original)
	require.NoError(t, e)
	require.NotNil(t, in)
	duplicate := homeMailControl(home, proto.KindHomeMail, payload)
	home.send(duplicate)
	fedEventually(t, "duplicate wrapper acknowledged", func() bool { return homeMailAck(home, duplicate.ID, proto.AckAccepted) })
	d, e := db.Open()
	require.NoError(t, e)
	var count int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM federation_inbound WHERE from_instance=? AND envelope_id=?`, home.id.ID(), original).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, db.FenceFederationMail(identity.Agent, "departure", time.Now().Add(time.Hour)))
	payload["envelope"] = "during-departure"
	fenced := homeMailControl(home, proto.KindHomeMail, payload)
	home.send(fenced)
	fedEventually(t, "fenced mailbox asks home to retry", func() bool {
		for _, a := range home.envelopes(proto.KindAck) {
			if a.InReplyTo == fenced.ID {
				var ack proto.AckPayload
				_ = a.DecodePayload(&ack)
				return ack.Code == "agent_moving"
			}
		}
		return false
	})
	in, e = db.FederationInboundByEnvelope(home.id.ID(), "during-departure")
	require.NoError(t, e)
	require.Nil(t, in)
	require.NoError(t, db.ReleaseFederationMailFence(identity.Agent, "departure"))
	retry := homeMailControl(home, proto.KindHomeMail, payload)
	home.send(retry)
	fedEventually(t, "unfenced retry delivers", func() bool { return homeMailAck(home, retry.ID, proto.AckAccepted) })
	payload["agent"] = db.NewAgentID()
	generic := homeMailControl(home, proto.KindHomeMail, payload)
	home.send(generic)
	fedEventually(t, "generic relay refused", func() bool { return homeMailAck(home, generic.ID, proto.AckRefused) })
	// A confirmed departure before home's location update is retryable, not a bounce.
	identity.Hops = 2
	identity.Proofs[home.agentdID] = "visitor-proof"
	require.NoError(t, db.DepartFederationIdentity("visitor-mail", home.agentdID, "next-host", "next-offer", identity))
	payload["agent"] = identity.Agent
	payload["envelope"] = "after-departure"
	departed := homeMailControl(home, proto.KindHomeMail, payload)
	home.send(departed)
	fedEventually(t, "old host requests location retry", func() bool {
		for _, a := range home.envelopes(proto.KindAck) {
			var ack proto.AckPayload
			_ = a.DecodePayload(&ack)
			if a.InReplyTo == departed.ID && ack.Code == "agent_moving" {
				return true
			}
		}
		return false
	})
	// The imported ledger has no bodies or credential material.
	rows, e := db.ListFederationMailDeliveries(identity.Agent)
	require.NoError(t, e)
	raw, e := json.Marshal(rows)
	require.NoError(t, e)
	require.NotContains(t, string(raw), "one delivery")
}

func TestFederation_HomeMailRevokesRemovedHomeMembershipAndSendsExpiredReceipt(t *testing.T) {
	fh := newFedHarness(t)
	require.NoError(t, db.PutFederationCatalog(fh.peer.id.ID(), string(mustJSON(t, proto.CatalogPayload{HomeRoutedMail: true, StableAgentIdentity: true})), time.Now()))
	f, sender := fh.f, fh.peer
	old := homeMailPeer(t, fh, "old-host")
	f.HaveGroup("home-team")
	f.HaveConvWithTitle("revoked-home-mail", "traveler")
	f.HaveMember("home-team", "revoked-home-mail")
	id, e := db.AgentIDForConv("revoked-home-mail")
	require.NoError(t, e)
	group, e := db.GetAgentGroupByName("home-team")
	require.NoError(t, e)
	grant := fedGrantCaps(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"group": "home-team", "peer": "bob", "caps": []string{"mail"}})
	require.Equal(t, 200, grant.Code, grant.Body.String())
	identity := db.FederationIdentity{Agent: id, Home: sender.agentdID, Hops: 1, Mail: true, Proofs: map[string]string{sender.agentdID: "proof"}}
	require.NoError(t, db.DepartFederationIdentity("revoked-home-mail", sender.agentdID, old.id.ID(), "left-home", identity))
	require.NoError(t, db.InsertFederationAgentMove(db.FederationAgentMove{Direction: "out", Peer: old.id.ID(), ID: "left-home", State: "moved", SourceAgent: id, SourceConv: "revoked-home-mail", SourceGroups: []int64{group.ID}, Identity: &identity, ExpiresAt: time.Now().Add(time.Hour)}))
	require.NoError(t, db.RemoveAgentGroupMember(group.ID, "revoked-home-mail"))
	mail := sender.envelope(proto.KindMail, proto.Endpoint{Agent: id}, proto.MailPayload{Body: "must not forward"})
	sender.send(mail)
	fedEventually(t, "removed home membership refuses ingress", func() bool { return homeMailAck(sender, mail.ID, proto.AckRefused) })
	custody, e := db.GetFederationMailCustody(sender.id.ID(), mail.ID, id)
	require.NoError(t, e)
	require.Nil(t, custody)
	// Expired mail still needs a fresh transport TTL for its final refusal.
	payload := map[string]any{"op": "handoff", "home": sender.agentdID, "agent": id, "nonce": "proof", "sender": proto.Endpoint{Instance: sender.id.ID()}, "envelope": "expired-mail", "expires_at": time.Now().Add(-time.Second), "mail": proto.MailPayload{Body: "expired"}}
	raw, e := json.Marshal(payload)
	require.NoError(t, e)
	_, e = db.QueueFederationMailCustody(db.FederationMailCustody{SenderInstance: sender.id.ID(), EnvelopeID: "expired-mail", AgentID: id, IngressInstance: old.id.ID(), IngressEnvelope: "old-handoff", Payload: string(raw), ExpiresAt: time.Now().Add(-time.Second)}, 100)
	require.NoError(t, e)
	fedEventually(t, "expiry receipt reaches old host", func() bool { return len(old.envelopes(proto.KindHomeMailReceipt)) > 0 })
	receipt := old.envelopes(proto.KindHomeMailReceipt)[0]
	require.True(t, receipt.ExpiresAt.After(time.Now()))
	var result struct{ Status string }
	require.NoError(t, receipt.DecodePayload(&result))
	require.Equal(t, proto.AckRefused, result.Status)
}

func TestFederation_HomeMailOldHostHandsOffAndAcknowledgesDuplicateReceipts(t *testing.T) {
	fh := newFedHarness(t)
	require.NoError(t, db.PutFederationCatalog(fh.peer.id.ID(), string(mustJSON(t, proto.CatalogPayload{HomeRoutedMail: true, StableAgentIdentity: true})), time.Now()))
	f, sender := fh.f, fh.peer
	home := homeMailPeer(t, fh, "home")
	f.HaveGroup("visited")
	identity := db.FederationIdentity{Agent: db.NewAgentID(), Home: home.id.ID(), Hops: 1, Mail: true, Proofs: map[string]string{home.id.ID(): "home-proof", sender.agentdID: "old-proof"}}
	_, e := db.ReserveFederationIdentity(identity, sender.agentdID, home.id.ID(), "first-arrival")
	require.NoError(t, e)
	f.HaveConvWithTitle("old-host-agent", "visitor")
	_, _, e = db.EnsureAgentForConvWithID("old-host-agent", identity.Agent, "test")
	require.NoError(t, e)
	f.HaveMember("visited", "old-host-agent")
	group, e := db.GetAgentGroupByName("visited")
	require.NoError(t, e)
	grant := fedGrantCaps(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"group": "visited", "peer": "bob", "caps": []string{"mail"}})
	require.Equal(t, 200, grant.Code, grant.Body.String())
	identity.Hops = 2
	require.NoError(t, db.DepartFederationIdentity("old-host-agent", sender.agentdID, "next-host", "departed", identity))
	require.NoError(t, db.InsertFederationAgentMove(db.FederationAgentMove{Direction: "out", Peer: "next-host", ID: "departed", State: "moved", SourceAgent: identity.Agent, SourceConv: "old-host-agent", SourceGroups: []int64{group.ID}, Identity: &identity, ExpiresAt: time.Now().Add(time.Hour)}))
	_, e = db.RemoveAllAgentGroupMembershipsForConv("old-host-agent")
	require.NoError(t, e)
	mail := sender.envelope(proto.KindMail, proto.Endpoint{Agent: identity.Agent}, proto.MailPayload{Body: "find me at home"})
	sender.send(mail)
	fedEventually(t, "old host accepts bounded custody", func() bool { return homeMailAck(sender, mail.ID, "custody") })
	fedEventually(t, "old host hands only to home", func() bool { return len(home.envelopes(proto.KindHomeMail)) > 0 })
	handoff := home.envelopes(proto.KindHomeMail)[0]
	var payload struct {
		Op, Home, Envelope string
		Sender             proto.Endpoint
	}
	require.NoError(t, handoff.DecodePayload(&payload))
	require.Equal(t, "handoff", payload.Op)
	require.Equal(t, home.id.ID(), payload.Home)
	require.Equal(t, sender.id.ID(), payload.Sender.Instance)
	require.Equal(t, mail.ID, payload.Envelope)
	receipt := homeMailControl(home, proto.KindHomeMailReceipt, map[string]any{"sender": sender.id.ID(), "envelope": mail.ID, "agent": identity.Agent, "status": proto.AckAccepted})
	home.send(receipt)
	fedEventually(t, "final outcome reaches original sender", func() bool { return homeMailAck(sender, mail.ID, proto.AckAccepted) })
	duplicate := homeMailControl(home, proto.KindHomeMailReceipt, map[string]any{"sender": sender.id.ID(), "envelope": mail.ID, "agent": identity.Agent, "status": proto.AckAccepted})
	home.send(duplicate)
	fedEventually(t, "duplicate receipt stops retrying", func() bool { return homeMailAck(home, duplicate.ID, proto.AckAccepted) })
	require.Empty(t, home.envelopes(proto.KindAgentLocation))
}

func TestFederation_HomeMailOfflineCustodySurvivesUntilLiveRevocation(t *testing.T) {
	fh := newFedHarness(t)
	require.NoError(t, db.PutFederationCatalog(fh.peer.id.ID(), string(mustJSON(t, proto.CatalogPayload{HomeRoutedMail: true, StableAgentIdentity: true})), time.Now()))
	f, sender := fh.f, fh.peer
	offline, e := proto.NewIdentity()
	require.NoError(t, e)
	// A previously paired host can be offline without appearing in discovery.
	require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: offline.ID(), PubKey: offline.Pub, Label: "offline-host"}))
	require.NoError(t, db.PutFederationCatalog(offline.ID(), string(mustJSON(t, proto.CatalogPayload{HomeRoutedMail: true, StableAgentIdentity: true})), time.Now()))
	f.HaveGroup("offline-team")
	f.HaveConvWithTitle("offline-traveler", "away")
	f.HaveMember("offline-team", "offline-traveler")
	id, e := db.AgentIDForConv("offline-traveler")
	require.NoError(t, e)
	group, e := db.GetAgentGroupByName("offline-team")
	require.NoError(t, e)
	rec := fedGrantCaps(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"group": "offline-team", "peer": "bob", "caps": []string{"mail"}})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	identity := db.FederationIdentity{Agent: id, Home: sender.agentdID, Hops: 1, Mail: true, Proofs: map[string]string{sender.agentdID: "proof"}}
	require.NoError(t, db.DepartFederationIdentity("offline-traveler", sender.agentdID, offline.ID(), "offline-departure", identity))
	mail := sender.envelope(proto.KindMail, proto.Endpoint{Agent: id}, proto.MailPayload{Body: "wait for receiver"})
	sender.send(mail)
	fedEventually(t, "offline delivery remains in home custody", func() bool {
		m, _ := db.GetFederationMailCustody(sender.id.ID(), mail.ID, id)
		return m != nil && m.State == "queued" && m.Destination == offline.ID()
	})
	m, e := db.GetFederationMailCustody(sender.id.ID(), mail.ID, id)
	require.NoError(t, e)
	require.NotEmpty(t, m.AttemptID)
	out, e := db.GetFederationOutbox(m.AttemptID)
	require.NoError(t, e)
	require.NotEqual(t, db.FedOutboxRefused, out.State)
	require.NoError(t, db.RemoveAgentGroupMember(group.ID, "offline-traveler"))
	require.NoError(t, db.WakeFederationMailCustody(id))
	fedEventually(t, "live revocation settles offline custody", func() bool { return homeMailAck(sender, mail.ID, proto.AckRefused) })
	m, e = db.GetFederationMailCustody(sender.id.ID(), mail.ID, id)
	require.NoError(t, e)
	require.Equal(t, "refused", m.State)
}

func TestFederation_HomeMailLocationBeforeSourceConfirmationRetriesOnlyLiveOffer(t *testing.T) {
	fh := newFedHarness(t)
	f, host := fh.f, fh.peer
	f.HaveConvWithTitle("pending-location", "source")
	id, e := db.AgentIDForConv("pending-location")
	require.NoError(t, e)
	identity := db.FederationIdentity{Agent: id, Home: host.agentdID, Hops: 1, Mail: true, Proofs: map[string]string{host.agentdID: "pending-proof"}}
	require.NoError(t, db.InsertFederationAgentMove(db.FederationAgentMove{Direction: "out", Peer: host.id.ID(), ID: "pending-offer", State: "awaiting_confirmation", SourceAgent: id, SourceConv: "pending-location", Identity: &identity, ExpiresAt: time.Now().Add(time.Hour)}))
	update := homeMailControl(host, proto.KindAgentLocation, map[string]any{"agent": id, "home": host.agentdID, "host": host.id.ID(), "nonce": "pending-proof", "offer": "pending-offer", "hops": 1})
	host.send(update)
	fedEventually(t, "arrival retries while source is confirming", func() bool {
		for _, env := range host.envelopes(proto.KindAck) {
			var ack proto.AckPayload
			_ = env.DecodePayload(&ack)
			if env.InReplyTo == update.ID {
				return ack.Code == "agent_moving"
			}
		}
		return false
	})
	require.NoError(t, db.DepartFederationIdentity("pending-location", host.agentdID, host.id.ID(), "pending-offer", identity))
	host.send(update)
	fedEventually(t, "pending location accepts after confirmation", func() bool { return homeMailAck(host, update.ID, proto.AckAccepted) })
}
