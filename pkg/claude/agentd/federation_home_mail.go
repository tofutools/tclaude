package agentd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

const fedAckCustody = "custody"

type homeMailPayload struct {
	Op        string            `json:"op"`
	Home      string            `json:"home"`
	Agent     string            `json:"agent"`
	Nonce     string            `json:"nonce"`
	Sender    proto.Endpoint    `json:"sender"`
	Envelope  string            `json:"envelope"`
	InReplyTo string            `json:"in_reply_to,omitempty"`
	ExpiresAt time.Time         `json:"expires_at"`
	Mail      proto.MailPayload `json:"mail"`
}
type homeMailReceipt struct {
	Sender   string `json:"sender"`
	Envelope string `json:"envelope"`
	Agent    string `json:"agent"`
	Status   string `json:"status"`
	Code     string `json:"code,omitempty"`
	Reason   string `json:"reason,omitempty"`
}
type agentLocationPayload struct {
	Agent string `json:"agent"`
	Home  string `json:"home"`
	Host  string `json:"host"`
	Nonce string `json:"nonce"`
	Offer string `json:"offer"`
	Hops  int    `json:"hops"`
}

func homeMailID(parts ...string) string {
	raw, _ := json.Marshal(parts)
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:16])
}
func sendConfirmedAgentLocation(m db.FederationAgentMove) error {
	p, e := db.GetAgentFederationPresence(m.TargetAgent)
	if e != nil || p == nil || p.State != "here" || p.ArrivalOffer != m.ID || !p.Transfer.Mail {
		return e
	}
	_ = db.ClearFederationMailFence(m.TargetAgent)
	_ = db.WakeFederationMailCustody(m.TargetAgent)
	if p.HomeInstance == arrivalNode() {
		return nil
	}
	home, e := db.GetFederationPeer(p.HomeInstance)
	if e != nil {
		return e
	}
	if home == nil || !peerSupportsHomeMail(home.InstanceID) {
		return nil
	}
	id := homeMailID("location", m.TargetAgent, m.Peer, m.ID)
	if row, e := db.GetFederationOutbox(id); e != nil {
		return e
	} else if row != nil {
		return nil
	}
	_, e = queueFederatedEnvelope(fedOutgoing{envelopeID: id, peer: home, kind: proto.KindAgentLocation, toLabel: peerDisplay(home), subject: "agent location", preview: m.TargetAgent, ttl: fedMailTTL, payload: agentLocationPayload{Agent: m.TargetAgent, Home: p.HomeInstance, Host: arrivalNode(), Nonce: p.Transfer.Proofs[p.HomeInstance], Offer: m.ID, Hops: p.HopCount}})
	return e
}
func (rt *fedRuntime) acceptAgentLocation(peer *db.FederationPeer, env *proto.Envelope) {
	var in agentLocationPayload
	ack := proto.AckPayload{Status: proto.AckRefused, Code: fedCodeMalformed, Reason: "invalid confirmed agent location"}
	if env.From.Agent != "" || env.To.Agent != "" || env.DecodePayload(&in) != nil || in.Home != rt.id.ID() || in.Host != peer.InstanceID || !proto.ValidAgentRef(in.Agent) || len(in.Nonce) > 128 || in.Hops < 1 {
		return
	}
	if err := db.UpdateFederationAgentLocation(in.Agent, in.Home, in.Host, in.Nonce, in.Offer, in.Hops); err != nil {
		p, _ := db.GetAgentFederationPresence(in.Agent)
		if p != nil && p.State == "here" {
			ack.Code = "agent_moving"
			ack.Reason = "home has not confirmed departure"
		} else {
			ack.Code = "continuation_refused"
			ack.Reason = err.Error()
		}
	} else {
		ack = proto.AckPayload{Status: proto.AckAccepted}
		_ = db.WakeFederationMailCustody(in.Agent)
		recordFederationAudit("agent.location", peer.InstanceID, in.Agent, "", fmt.Sprintf("host=%s hop=%d", in.Host, in.Hops), 200)
	}
	rt.sendControl(peer.InstanceID, proto.KindAck, env.ID, ack)
}

// queueAwayMail is reached only after the ordinary mail size/rate checks.
// It neither accepts another recipient nor turns a move receipt into a relay.
func (rt *fedRuntime) queueAwayMail(peer *db.FederationPeer, env *proto.Envelope, mp proto.MailPayload) bool {
	p, e := db.GetAgentFederationPresence(env.To.Agent)
	if e != nil || p == nil || p.State != "away" || !p.Transfer.Mail {
		return false
	}
	refuse := func(code, reason string) {
		rt.sendControl(peer.InstanceID, proto.KindAck, env.ID, proto.AckPayload{Status: proto.AckRefused, Code: code, Reason: reason})
	}
	if !stableMailIngressAuthorized(peer.InstanceID, env, p.AgentID) {
		refuse(fedCodeNotExported, "away recipient is not shared with this sender")
		return true
	}
	if len(mp.Attachments) > 0 && !stableMailAttachmentsAllowed(peer.InstanceID, p.AgentID) {
		refuse(fedCodeNotExported, "away recipient does not accept attachments from this sender")
		return true
	}
	payload := homeMailPayload{Op: "handoff", Home: p.HomeInstance, Agent: p.AgentID, Nonce: p.Transfer.Proofs[p.HomeInstance], Sender: env.From, Envelope: env.ID, InReplyTo: env.InReplyTo, ExpiresAt: env.ExpiresAt, Mail: mp}
	state := "queued"
	if p.HomeInstance != rt.id.ID() {
		state = "handoff"
		home, _ := db.GetFederationPeer(p.HomeInstance)
		if home == nil || !peerSupportsHomeMail(p.HomeInstance) {
			refuse("agent_moved", "agent is away; send to "+p.AgentID+"@"+p.HomeInstance)
			return true
		}
	}
	raw, _ := json.Marshal(payload)
	_, e = db.QueueFederationMailCustody(db.FederationMailCustody{SenderInstance: env.From.Instance, EnvelopeID: env.ID, AgentID: p.AgentID, IngressInstance: peer.InstanceID, IngressEnvelope: env.ID, Payload: string(raw), State: state, ExpiresAt: env.ExpiresAt}, regularAgentMessageQueueLimit)
	if e != nil {
		refuse(fedCodeQueueFull, e.Error())
		return true
	}
	row, e := db.GetFederationMailCustody(env.From.Instance, env.ID, p.AgentID)
	if e != nil || row == nil {
		refuse(fedCodeInternal, "could not read mail custody")
		return true
	}
	if row.State == "accepted" || row.State == "refused" {
		rt.sendCustodyReceipt(*row)
		return true
	}
	if state == "handoff" {
		home, _ := db.GetFederationPeer(p.HomeInstance)
		id := homeMailID("handoff", payload.Sender.Instance, payload.Envelope, payload.Agent)
		if old, e := db.GetFederationOutbox(id); e != nil {
			refuse(fedCodeInternal, e.Error())
			return true
		} else if old == nil {
			if _, e = queueFederatedEnvelope(fedOutgoing{envelopeID: id, peer: home, kind: proto.KindHomeMail, subject: "agent mail handoff", preview: p.AgentID, ttl: time.Until(env.ExpiresAt), payload: payload}); e != nil {
				refuse(fedCodeInternal, e.Error())
				return true
			}
		}
	}
	rt.sendControl(peer.InstanceID, proto.KindAck, env.ID, proto.AckPayload{Status: fedAckCustody})
	recordFederationAudit("federation.mail.custody", peer.InstanceID, p.AgentID, "", "queued at home "+p.HomeInstance, 202)
	rt.kickOutbox()
	return true
}
func stableMailIngressAuthorized(peer string, env *proto.Envelope, agent string) bool {
	conv, _ := db.CurrentConvForAgent(agent)
	if _, ok := federationInboundAuthorized(peer, env, conv); ok {
		return true
	}
	presence, _ := db.GetAgentFederationPresence(agent)
	moves, e := db.ListFederationAgentMoves()
	if e != nil {
		return false
	}
	for _, m := range moves {
		if m.Direction == "out" && m.SourceAgent == agent && m.Identity != nil && presence != nil && m.ID == presence.DepartureOffer && m.SourceConv == conv && (m.State == "moved" || m.State == "retiring") {
			for _, g := range m.SourceGroups {
				if fedPeerAllows(peer, g, PermMessageDirect) {
					return true
				}
			}
		}
	}
	return false
}
func stableMailAttachmentsAllowed(peer, agent string) bool {
	conv, _ := db.CurrentConvForAgent(agent)
	if fedAttachmentsAllowed(peer, conv) {
		return true
	}
	presence, _ := db.GetAgentFederationPresence(agent)
	moves, _ := db.ListFederationAgentMoves()
	for _, m := range moves {
		if m.Direction == "out" && m.SourceAgent == agent && m.Identity != nil && presence != nil && m.ID == presence.DepartureOffer && m.SourceConv == conv && (m.State == "moved" || m.State == "retiring") {
			for _, g := range m.SourceGroups {
				if fedPeerAllows(peer, g, PermMessageAttachments) {
					return true
				}
			}
		}
	}
	return false
}
func validHomeMail(in homeMailPayload) bool {
	return proto.ValidAgentRef(in.Agent) && in.Home != "" && in.Sender.Instance != "" && in.Envelope != "" && len(in.Envelope) <= 128 && len(in.Nonce) <= 128 && in.ExpiresAt.After(time.Now()) && in.ExpiresAt.Before(time.Now().Add(fedMaxInboundTTL)) && len(in.Mail.Body) > 0 && len(in.Mail.Body) <= proto.MaxMailBody && len(in.Mail.Subject) <= 512 && validateFedAttachments(in.Mail.Attachments) == nil
}
func (rt *fedRuntime) acceptHomeMail(peer *db.FederationPeer, env *proto.Envelope) {
	var in homeMailPayload
	refuse := func(code, reason string) {
		rt.sendControl(peer.InstanceID, proto.KindAck, env.ID, proto.AckPayload{Status: proto.AckRefused, Code: code, Reason: reason})
	}
	if env.From.Agent != "" || env.To.Agent != "" || env.DecodePayload(&in) != nil || !validHomeMail(in) {
		refuse(fedCodeMalformed, "invalid stable mail wrapper")
		return
	}
	p, e := db.GetAgentFederationPresence(in.Agent)
	if e != nil || p == nil || p.HomeInstance != in.Home || p.State == "terminal" {
		refuse("continuation_refused", "no live stable recipient")
		return
	}
	if !rt.allowInbound(peer.InstanceID) {
		refuse(fedCodeRateLimited, "stable mail rate limit")
		return
	}
	if in.Op == "handoff" && in.Home == rt.id.ID() {
		if in.Nonce == "" || p.Transfer.Proofs[in.Home] != in.Nonce {
			refuse("continuation_refused", "handoff is not from this agent's continuation")
			return
		}
		origin, _ := db.GetFederationPeer(in.Sender.Instance)
		original := &proto.Envelope{ID: in.Envelope, From: in.Sender, To: proto.Endpoint{Instance: in.Home, Agent: in.Agent}, InReplyTo: in.InReplyTo}
		if origin == nil || !stableMailIngressAuthorized(origin.InstanceID, original, in.Agent) || len(in.Mail.Attachments) > 0 && !stableMailAttachmentsAllowed(origin.InstanceID, in.Agent) {
			refuse(fedCodeNotExported, "original sender is no longer shared with the home recipient")
			return
		}
		raw, _ := json.Marshal(in)
		_, e = db.QueueFederationMailCustody(db.FederationMailCustody{SenderInstance: in.Sender.Instance, EnvelopeID: in.Envelope, AgentID: in.Agent, IngressInstance: peer.InstanceID, IngressEnvelope: env.ID, Payload: string(raw), ExpiresAt: in.ExpiresAt}, regularAgentMessageQueueLimit)
		if e != nil {
			refuse(fedCodeQueueFull, e.Error())
			return
		}
		row, _ := db.GetFederationMailCustody(in.Sender.Instance, in.Envelope, in.Agent)
		if row != nil && (row.State == "accepted" || row.State == "refused") {
			rt.sendCustodyReceipt(*row)
		} else {
			rt.sendControl(peer.InstanceID, proto.KindAck, env.ID, proto.AckPayload{Status: fedAckCustody})
			rt.kickOutbox()
		}
		return
	}
	if in.Op != "deliver" || peer.InstanceID != in.Home || p.State != "here" || in.Nonce == "" || in.Nonce != p.Transfer.Proofs[in.Home] {
		refuse("continuation_refused", "only home may deliver to its current visiting agent")
		return
	}
	// Ordinary receive checks remain authoritative against home; origin identity
	// is display/dedup provenance, never a substitute for that grant.
	original := *env
	original.Kind = proto.KindMail
	original.To.Agent = in.Agent
	original.InReplyTo = in.InReplyTo
	original.Payload, _ = json.Marshal(in.Mail)
	rt.acceptMailWithOrigin(peer, &original, &in)
}

func (rt *fedRuntime) flushHomeMail(now time.Time) {
	rows, e := db.DueFederationMailCustody(now, 50)
	if e != nil {
		return
	}
	for _, m := range rows {
		var in homeMailPayload
		if json.Unmarshal([]byte(m.Payload), &in) != nil {
			m.State = "refused"
			m.NextAttemptAt = now
			_ = db.UpdateFederationMailCustody(m)
			continue
		}
		p, _ := db.GetAgentFederationPresence(m.AgentID)
		origin, _ := db.GetFederationPeer(m.SenderInstance)
		original := &proto.Envelope{ID: in.Envelope, From: in.Sender, To: proto.Endpoint{Instance: rt.id.ID(), Agent: in.Agent}, InReplyTo: in.InReplyTo}
		if !m.ExpiresAt.After(now) || p == nil || p.HomeInstance != rt.id.ID() || p.State == "terminal" || origin == nil || !stableMailIngressAuthorized(m.SenderInstance, original, m.AgentID) || len(in.Mail.Attachments) > 0 && !stableMailAttachmentsAllowed(m.SenderInstance, m.AgentID) {
			m.State = "refused"
			m.NextAttemptAt = now
			_ = db.UpdateFederationMailCustody(m)
			rt.sendCustodyReceipt(m)
			continue
		}
		if delivered, _ := db.FederationMailDelivered(m.AgentID, m.SenderInstance, m.EnvelopeID); delivered {
			m.State = "accepted"
			m.NextAttemptAt = now
			_ = db.UpdateFederationMailCustody(m)
			rt.sendCustodyReceipt(m)
			continue
		}
		if p.State == "here" {
			original.ExpiresAt = in.ExpiresAt
			original.Kind = proto.KindMail
			original.Payload, _ = json.Marshal(in.Mail)
			rt.acceptMail(origin, original)
			if delivered, _ := db.FederationMailDelivered(m.AgentID, m.SenderInstance, m.EnvelopeID); delivered {
				m.State = "accepted"
				rt.sendCustodyReceipt(m)
			}
			m.NextAttemptAt = now.Add(fedAckWait)
			_ = db.UpdateFederationMailCustody(m)
			continue
		}
		destination, e := db.FederationMailDestination(m.AgentID, rt.id.ID())
		if e != nil || destination == "" {
			continue
		}
		host, _ := db.GetFederationPeer(destination)
		if host == nil || !peerSupportsHomeMail(destination) {
			m.State = "refused"
			m.NextAttemptAt = now
			_ = db.UpdateFederationMailCustody(m)
			rt.sendCustodyReceipt(m)
			continue
		}
		in.Op = "deliver"
		in.Nonce = p.Transfer.Proofs[p.HomeInstance]
		attempt := homeMailID("delivery", m.SenderInstance, m.EnvelopeID, m.AgentID, destination, p.ContinuationNonceHash)
		if old, e := db.GetFederationOutbox(attempt); e != nil {
			continue
		} else if old != nil && old.State == db.FedOutboxRefused {
			m.State = "refused"
			m.NextAttemptAt = now
			_ = db.UpdateFederationMailCustody(m)
			rt.sendCustodyReceipt(m)
			continue
		} else if old == nil {
			if _, e = queueFederatedEnvelope(fedOutgoing{envelopeID: attempt, peer: host, kind: proto.KindHomeMail, subject: "home-routed agent mail", preview: m.AgentID, ttl: time.Until(in.ExpiresAt), payload: in}); e != nil {
				continue
			}
		}
		m.Destination, m.AttemptID, m.NextAttemptAt = destination, attempt, now.Add(fedAckWait)
		_ = db.UpdateFederationMailCustody(m)
	}
}
func (rt *fedRuntime) sendCustodyReceipt(m db.FederationMailCustody) {
	status := proto.AckRefused
	if m.State == "accepted" {
		status = proto.AckAccepted
	}
	if m.IngressInstance == m.SenderInstance {
		rt.sendControl(m.IngressInstance, proto.KindAck, m.IngressEnvelope, proto.AckPayload{Status: status, Code: "home_mail_result", Reason: m.State})
		return
	}
	peer, _ := db.GetFederationPeer(m.IngressInstance)
	if peer == nil {
		return
	}
	id := homeMailID("receipt", m.SenderInstance, m.EnvelopeID, m.AgentID, m.State)
	if old, _ := db.GetFederationOutbox(id); old != nil {
		return
	}
	_, _ = queueFederatedEnvelope(fedOutgoing{envelopeID: id, peer: peer, kind: proto.KindHomeMailReceipt, subject: "home mail result", preview: m.AgentID, ttl: time.Until(m.ExpiresAt), payload: homeMailReceipt{Sender: m.SenderInstance, Envelope: m.EnvelopeID, Agent: m.AgentID, Status: status, Reason: m.State}})
}
func (rt *fedRuntime) acceptHomeMailReceipt(peer *db.FederationPeer, env *proto.Envelope) {
	var r homeMailReceipt
	if env.From.Agent != "" || env.To.Agent != "" || env.DecodePayload(&r) != nil || (r.Status != proto.AckAccepted && r.Status != proto.AckRefused) {
		return
	}
	m, e := db.GetFederationMailCustody(r.Sender, r.Envelope, r.Agent)
	if e != nil || m == nil || m.State != "handoff" {
		return
	}
	p, _ := db.GetAgentFederationPresence(r.Agent)
	if p == nil || p.HomeInstance != peer.InstanceID {
		return
	}
	if r.Status == proto.AckAccepted {
		m.State = "accepted"
	} else {
		m.State = "refused"
	}
	m.NextAttemptAt = time.Now()
	_ = db.UpdateFederationMailCustody(*m)
	rt.sendCustodyReceipt(*m)
	rt.sendControl(peer.InstanceID, proto.KindAck, env.ID, proto.AckPayload{Status: proto.AckAccepted})
}

// A final-host ack settles custody only for that exact attempt and destination.
func (rt *fedRuntime) handleHomeMailAck(row *db.FederationOutboxRow, ack proto.AckPayload) {
	if row.Kind != proto.KindHomeMail || ack.Status == fedAckCustody || fedRetryableCode(ack.Code) {
		return
	}
	// Match by the persisted attempt, never a peer-supplied recipient key.
	d, e := db.Open()
	if e != nil {
		return
	}
	rows, e := d.Query(`SELECT sender_instance,envelope_id,agent_id FROM federation_mail_custody WHERE attempt_id=? AND destination=? AND state='queued'`, row.EnvelopeID, row.ToInstance)
	if e != nil {
		return
	}
	type key struct{ s, i, a string }
	var keys []key
	for rows.Next() {
		var k key
		if rows.Scan(&k.s, &k.i, &k.a) == nil {
			keys = append(keys, k)
		}
	}
	_ = rows.Close()
	for _, k := range keys {
		m, _ := db.GetFederationMailCustody(k.s, k.i, k.a)
		if m == nil {
			continue
		}
		if ack.Status == proto.AckAccepted {
			m.State = "accepted"
			_ = db.ImportFederationMailDeliveries(k.a, []db.FederationMailDelivery{{Sender: k.s, Envelope: k.i, ExpiresAt: m.ExpiresAt}})
		} else {
			m.State = "refused"
		}
		m.NextAttemptAt = time.Now()
		_ = db.UpdateFederationMailCustody(*m)
		rt.sendCustodyReceipt(*m)
		recordFederationAudit("federation.mail.forward.result", row.ToInstance, k.a, "", m.State, 200)
	}
}
