package agentd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

var fedMoveMu sync.Mutex

func sameMoveIntent(a, b *bundletransfer.MoveIntent) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
func peerSupportsAgentMoves(peer string) bool {
	raw, _, err := db.GetFederationCatalog(peer)
	if err != nil {
		return false
	}
	var cat proto.CatalogPayload
	return json.Unmarshal([]byte(raw), &cat) == nil && cat.AgentMoves
}
func reserveIncomingAgentMove(o *db.FederationBundleOffer, raw []byte, agentID string) error {
	b, err := agentbundle.Decode(raw)
	if err != nil {
		return err
	}
	if b.Manifest.History == nil || b.Manifest.History.SourceConvID != o.Descriptor.Move.SourceConv {
		return errors.New("move requires the offered source generation's native history")
	}
	var profile struct {
		CodexAppServer bool `json:"codex_app_server"`
	}
	if err = json.Unmarshal(b.Manifest.Agent.Profile, &profile); err != nil {
		return err
	}
	m := db.FederationAgentMove{Direction: "in", Peer: o.Peer, ID: o.Descriptor.ID, State: "awaiting_running", SourceAgent: o.Descriptor.Move.SourceAgent, SourceConv: o.Descriptor.Move.SourceConv, TargetAgent: agentID, SHA256: o.Descriptor.SHA256, Group: o.Descriptor.Group, ExpiresAt: o.Descriptor.ExpiresAt, CodexAppServer: profile.CodexAppServer}
	m.MovedFrom = &db.FederationMoveLink{Instance: o.Peer, Agent: m.SourceAgent, Offer: m.ID}
	return db.InsertFederationAgentMove(m)
}

// A durable confirmation names exactly the reserved launch. An applied receipt
// alone is deliberately insufficient: normal spawn may still be pending.
func confirmIncomingAgentMove(m db.FederationAgentMove) {
	if !m.ExpiresAt.After(time.Now()) {
		m.State = "expired"
		_, _ = db.TransitionFederationAgentMove(m, "awaiting_running")
		return
	}
	o, err := db.GetFederationBundleOffer("in", m.Peer, m.ID)
	if err != nil || o == nil || o.State != "applied" || o.ImportAgent != m.TargetAgent || o.ImportLabel == "" || !fedBundleOfferAdmitted(o, bundletransfer.Agent) {
		return
	}
	a, err := db.GetAgent(m.TargetAgent)
	if err != nil || a == nil || !a.Active() || a.CurrentConvID == "" {
		return
	}
	s, err := db.LoadSession(o.ImportLabel)
	if err != nil || s == nil || s.ConvID != a.CurrentConvID || s.TmuxSession == "" || !session.IsTmuxSessionAlive(s.TmuxSession) {
		return
	}
	if m.CodexAppServer {
		runtime, e := db.GetCodexAppServerRuntimeByLaunchID(o.ImportLabel)
		if e != nil || runtime == nil || runtime.ConvID != a.CurrentConvID || runtime.State != db.CodexAppServerReady {
			return
		}
	}
	p, err := db.GetFederationPeer(m.Peer)
	if err != nil || p == nil {
		return
	}
	confirm := bundletransfer.MoveConfirmation{ObservedAt: time.Now().UTC(), Offer: m.ID, SHA256: m.SHA256, SourceAgent: m.SourceAgent, SourceConv: m.SourceConv, TargetAgent: m.TargetAgent, TargetConv: a.CurrentConvID}
	hash := sha256.Sum256([]byte("agent-move-confirm/" + m.Peer + "/" + m.ID))
	id := hex.EncodeToString(hash[:16])
	if row, e := db.GetFederationOutbox(id); e != nil {
		return
	} else if row == nil {
		if _, e = queueFederatedEnvelope(fedOutgoing{envelopeID: id, peer: p, kind: proto.KindAgentMoveConfirm, toLabel: peerDisplay(p), inReplyTo: m.ID, subject: "agent move running", preview: m.ID, ttl: time.Until(m.ExpiresAt), payload: confirm}); e != nil {
			return
		}
	}
	m.TargetConv = a.CurrentConvID
	m.State = "running"
	_, _ = db.TransitionFederationAgentMove(m, "awaiting_running")
}
func (rt *fedRuntime) acceptAgentMoveConfirmation(p *db.FederationPeer, env *proto.Envelope) {
	var c bundletransfer.MoveConfirmation
	if env.From.Agent != "" || env.To.Agent != "" || env.DecodePayload(&c) != nil || !proto.ValidAgentRef(c.TargetAgent) || len(c.TargetConv) != 36 {
		return
	}
	m, err := db.GetFederationAgentMove("out", p.InstanceID, c.Offer)
	if err != nil || m == nil || m.SHA256 != c.SHA256 || m.SourceAgent != c.SourceAgent || m.SourceConv != c.SourceConv {
		return
	}
	if m.State == "awaiting_confirmation" && m.ExpiresAt.After(time.Now()) {
		if c.ObservedAt.IsZero() || time.Since(c.ObservedAt) > 5*time.Minute || c.ObservedAt.After(time.Now().Add(5*time.Second)) {
			return
		}
		m.ConfirmedAt = c.ObservedAt
		m.State = "confirmed"
		m.TargetAgent = c.TargetAgent
		m.TargetConv = c.TargetConv
		m.MovedTo = &db.FederationMoveLink{Instance: m.Peer, Agent: c.TargetAgent, Offer: m.ID}
		if _, err = db.TransitionFederationAgentMove(*m, "awaiting_confirmation"); err != nil {
			return
		}
	}
	// Replays and late confirmations after abandon are acknowledged but inert.
	rt.sendControl(p.InstanceID, proto.KindAck, env.ID, proto.AckPayload{Status: proto.AckAccepted})
}
func moveAuthority(m db.FederationAgentMove) error {
	if m.ConfirmedAt.IsZero() || time.Since(m.ConfirmedAt) > 5*time.Minute {
		return errors.New("running confirmation is stale; abandon and request another move")
	}
	p, err := db.GetFederationPeer(m.Peer)
	if err != nil || p == nil {
		return errors.New("destination peer is no longer trusted")
	}
	a, err := db.GetAgent(m.SourceAgent)
	if err != nil || a == nil || a.CurrentConvID != m.SourceConv {
		return errors.New("source generation changed; abandon this move and offer the current generation")
	}
	if m.Human {
		return nil
	}
	caller, err := db.GetAgent(m.Initiator)
	if err != nil || caller == nil || !caller.Active() || caller.CurrentConvID == "" {
		return errors.New("initiating agent is no longer active")
	}
	// Reuse the ordinary gates with no ask-human continuation or cached grant.
	r := httptest.NewRequest(http.MethodPost, "/v1/federation/move-agent", nil)
	r = r.WithContext(context.WithValue(r.Context(), peerKey{}, &peer{PID: -1, HasClaudeAncestor: true, ConvID: caller.CurrentConvID}))
	rec := httptest.NewRecorder()
	if _, ok := requirePermission(rec, r, PermAgentMove, ActionContext{RemotePeer: m.Peer, RemoteGroup: m.Group}); !ok {
		return errors.New("initiator no longer has agent.move authority")
	}
	if _, ok := requireCrossAgentPermission(rec, r, PermAgentRetire, m.SourceConv); !ok {
		return errors.New("initiator no longer has source retire authority")
	}
	return nil
}
func retireConfirmedAgentMove(m db.FederationAgentMove) {
	if m.State == "confirmed" {
		if err := moveAuthority(m); err != nil {
			m.State = "blocked"
			m.LastError = err.Error()
			_, _ = db.TransitionFederationAgentMove(m, "confirmed")
			return
		}
		m.State = "retiring"
		won, err := db.TransitionFederationAgentMove(m, "confirmed")
		if err != nil || !won {
			return
		}
	}
	a, err := db.GetAgent(m.SourceAgent)
	if err != nil || a == nil {
		return
	}
	if a.CurrentConvID != m.SourceConv {
		m.LastError = "source generation changed before retirement"
		m.State = "blocked"
		_, _ = db.TransitionFederationAgentMove(m, "retiring")
		return
	}
	// A restart after the retire commit resumes teardown, rather than demoting
	// twice or losing the moved-address tombstone.
	if a.Active() {
		_, _, err = retireAgentConvGuarded(m.SourceConv, "system:federation-move", "moved to "+m.Peer+"/"+m.TargetAgent, false, func() error { return moveAuthority(m) })
		if err != nil {
			m.LastError = err.Error()
			m.State = "blocked"
			_, _ = db.TransitionFederationAgentMove(m, "retiring")
			return
		}
	}
	// Keep source history and worktree. Normal retirement removes membership,
	// grants and owned runtime directories, and shuts down the source pane.
	td := finishRetiredConv(m.SourceConv, true, false, agentWorktreeView{}, "")
	if td.Stop.Action == "error" {
		m.LastError = td.Stop.Detail
		_, _ = db.TransitionFederationAgentMove(m, "retiring")
		return
	}
	m.State = "moved"
	m.LastError = ""
	_, _ = db.TransitionFederationAgentMove(m, "retiring")
}
func reconcileFederationMoves() {
	if !fedMoveMu.TryLock() {
		return
	}
	defer fedMoveMu.Unlock()
	moves, err := db.ListFederationAgentMoves()
	if err != nil {
		return
	}
	for _, m := range moves {
		if m.Direction == "in" && m.State == "awaiting_running" {
			confirmIncomingAgentMove(m)
			continue
		}
		if m.Direction != "out" {
			continue
		}
		switch m.State {
		case "awaiting_confirmation":
			offer, _ := db.GetFederationBundleOffer("out", m.Peer, m.ID)
			state := ""
			if !m.ExpiresAt.After(time.Now()) {
				state = "expired"
			} else if offer != nil && offer.State == "declined" {
				state = "declined"
			}
			if state != "" {
				m.State = state
				_, _ = db.TransitionFederationAgentMove(m, "awaiting_confirmation")
			}
		case "confirmed", "retiring":
			retireConfirmedAgentMove(m)
		}
	}
}
func handleFederationMoves(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "inspect agent moves") {
		return
	}
	moves, err := db.ListFederationAgentMoves()
	if err != nil {
		writeError(w, 500, "moves", err.Error())
		return
	}
	id := r.PathValue("id")
	if id != "" {
		for _, m := range moves {
			if m.ID == id {
				writeJSON(w, 200, m)
				return
			}
		}
		writeError(w, 404, "move", "no such move")
		return
	}
	writeJSON(w, 200, map[string]any{"moves": moves})
}
func handleFederationMoveAbandon(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "abandon an agent move") {
		return
	}
	moves, err := db.ListFederationAgentMoves()
	if err != nil {
		writeError(w, 500, "moves", err.Error())
		return
	}
	for _, m := range moves {
		if m.Direction != "out" || m.ID != r.PathValue("id") {
			continue
		}
		if m.State == "abandoned" {
			writeJSON(w, 200, m)
			return
		}
		if m.State != "awaiting_confirmation" && m.State != "confirmed" && m.State != "blocked" && m.State != "expired" && m.State != "declined" {
			writeError(w, 409, "move", "retirement has already started; inspect move status")
			return
		}
		from := m.State
		m.State = "abandoned"
		m.LastError = "destination clone, if created, remains independent"
		won, e := db.TransitionFederationAgentMove(m, from)
		if e != nil || !won {
			writeError(w, 409, "move", "move state changed; inspect and retry")
			return
		}
		writeJSON(w, 200, m)
		return
	}
	writeError(w, 404, "move", "no such outgoing move")
}

// Bounce without forwarding or disclosing the destination. The old group's
// current mail grant must still authorize the sender, and group IDs are pinned.
func movedAgentMailRefusal(peerID, agentID string) bool {
	moves, err := db.ListFederationAgentMoves()
	if err != nil {
		return false
	}
	for _, m := range moves {
		if m.Direction != "out" || m.SourceAgent != agentID || (m.State != "moved" && m.State != "retiring") {
			continue
		}
		a, e := db.GetAgent(agentID)
		if e != nil || a == nil || a.Active() || a.CurrentConvID != m.SourceConv {
			continue
		}
		for _, g := range m.SourceGroups {
			if fedPeerAllows(peerID, g, PermMessageDirect) {
				return true
			}
		}
	}
	return false
}
