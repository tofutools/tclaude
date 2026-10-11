package agentd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// Local sends have already passed their ordinary direct/group/operator gates.
// Keep their outbox copy and custody atomic; never wake the departed pane.
func queueLocalHomeMail(m *db.AgentMessage, attachments []db.AgentMessageAttachment, policy ...int64) (int64, int, bool, error) {
	if m.PinGen {
		return 0, 0, false, nil
	}
	id, e := db.AgentIDForConv(m.ToConv)
	if e != nil {
		return 0, 0, false, e
	}
	p, e := db.GetAgentFederationPresence(id)
	if e != nil {
		return 0, 0, false, e
	}
	if p == nil || p.State != "away" || p.HomeInstance != arrivalNode() || !p.Transfer.Mail {
		return 0, 0, false, nil
	}
	if len(m.Body) > proto.MaxMailBody || len(m.Subject) > fedMaxSubject {
		return 0, 0, true, fmt.Errorf("away agent mail is limited to %d body bytes and %d subject bytes", proto.MaxMailBody, fedMaxSubject)
	}
	var atts []proto.AttachmentPayload
	for _, a := range attachments {
		f, e := os.Open(a.StoragePath)
		if e != nil {
			return 0, 0, true, e
		}
		raw, e := io.ReadAll(io.LimitReader(f, proto.MaxAttachmentBytes+1))
		_ = f.Close()
		if e != nil {
			return 0, 0, true, e
		}
		atts = append(atts, proto.AttachmentPayload{Name: a.Filename, Data: raw})
	}
	if e = validateFedAttachments(atts); e != nil {
		return 0, 0, true, e
	}
	sender, _ := db.AgentIDForConv(m.FromConv)
	name := agent.TitleFor(m.FromConv)
	if m.OperatorAuthored {
		name = "human operator"
	}
	expiry := time.Now().Add(fedMailTTL)
	limit, cron := regularAgentMessageQueueLimit, int64(0)
	if len(policy) > 0 {
		limit = 0
		cron = policy[0]
	}
	mid, pending, e := db.InsertLocalHomeMail(m, attachments, limit, func(mid int64) (db.FederationMailCustody, error) {
		envelope := "local:" + strconv.FormatInt(mid, 10)
		in := homeMailPayload{Op: "deliver", Home: p.HomeInstance, Agent: id, Nonce: p.Transfer.Proofs[p.HomeInstance], Sender: proto.Endpoint{Instance: p.HomeInstance, Agent: sender, Name: name}, Envelope: envelope, ExpiresAt: expiry, Mail: proto.MailPayload{Subject: m.Subject, Body: m.Body, Attachments: atts}, LocalMessage: mid, Operator: m.OperatorAuthored}
		raw, e := json.Marshal(in)
		return db.FederationMailCustody{SenderInstance: p.HomeInstance, EnvelopeID: envelope, AgentID: id, IngressInstance: p.HomeInstance, IngressEnvelope: envelope, Payload: string(raw), ExpiresAt: expiry}, e
	}, cron)
	if e == nil {
		wakeHomeMailOutbox()
		recordFederationAudit("federation.mail.custody", p.HomeInstance, id, "", "local message queued at home", 202)
	}
	return mid, pending, true, e
}
func wakeHomeMailOutbox() {
	fedMu.Lock()
	rt := fedCurrent
	fedMu.Unlock()
	if rt != nil {
		rt.kickOutbox()
	}
}
func localHomeMailAuthorized(in homeMailPayload, home string) bool {
	if in.LocalMessage <= 0 || in.Sender.Instance != home || in.Envelope != "local:"+strconv.FormatInt(in.LocalMessage, 10) {
		return false
	}
	m, e := db.GetAgentMessage(in.LocalMessage)
	if e != nil || m == nil || m.ToAgent != in.Agent {
		return false
	}
	if in.Operator {
		return db.IsOperatorAgentMessage(m.ID)
	}
	if m.FromAgent == "" {
		// Senderless, non-operator rows are daemon-generated lifecycle/process notes.
		return m.FromConv == ""
	}
	from, _ := db.CurrentConvForAgent(m.FromAgent)
	to, _ := db.CurrentConvForAgent(in.Agent)
	active, _ := db.GetAgent(m.FromAgent)
	if active == nil || !active.RetiredAt.IsZero() {
		return false
	}
	via, _, e := db.CanSenderReachTarget(from, to)
	return e == nil && via != nil || holdsPermission(from, PermMessageDirect)
}
func localHomeMailStatus(id int64) (destination, state string) {
	m, e := db.LocalHomeMailCustody(id)
	if e != nil || m == nil {
		return "", ""
	}
	destination = m.Destination
	if destination == "" {
		destination, _ = db.FederationMailDestination(m.AgentID, arrivalNode())
	}
	if p, _ := db.GetFederationPeer(destination); p != nil {
		destination = peerDisplay(p)
	}
	return destination, m.State
}
